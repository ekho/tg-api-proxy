package tgproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testAccess(t *testing.T, raw string) *atomic.Pointer[AccessConfig] {
	t.Helper()
	p, err := ParseAccessConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	current := new(atomic.Pointer[AccessConfig])
	current.Store(p)
	return current
}

const requestPolicy = `trusted_proxies: ['10.0.0.2', '10.0.0.3', '2001:db8:ffff::/48']
clients:
 - {name: service-a, bot_ids: ['123456'], allowed_ips: ['192.0.2.10', '2001:db8::10']}
`

func TestIPAccessTrustBoundary(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstreamCalls.Add(1); io.WriteString(w, `{"ok":true}`) }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	var logs bytes.Buffer
	h, err := NewHandler(Config{Upstream: u, Access: testAccess(t, requestPolicy), Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, peer string
		xff        []string
		want       int
	}{
		{"direct allowed", "192.0.2.10:1234", nil, 200},
		{"untrusted spoof", "203.0.113.99:1234", []string{"192.0.2.10"}, 403},
		{"trusted proxy", "10.0.0.2:1234", []string{"192.0.2.10"}, 200},
		{"stop at untrusted hop", "10.0.0.2:1234", []string{"192.0.2.10, 203.0.113.99, 10.0.0.3"}, 403},
		{"trusted chain", "10.0.0.2:1234", []string{"192.0.2.10, 10.0.0.3"}, 200},
		{"missing XFF", "10.0.0.2:1234", nil, 403},
		{"invalid traversed hop", "10.0.0.2:1234", []string{"192.0.2.10, broken"}, 403},
		{"irrelevant left hop", "10.0.0.2:1234", []string{"broken, 192.0.2.10"}, 200},
		{"all hops trusted", "10.0.0.2:1234", []string{"10.0.0.3"}, 403},
		{"repeated spoof", "10.0.0.2:1234", []string{"192.0.2.10", "203.0.113.99"}, 403},
		{"repeated trusted", "10.0.0.2:1234", []string{"192.0.2.10", "10.0.0.3"}, 200},
		{"mapped peer and client", "[::ffff:10.0.0.2]:1234", []string{"::ffff:192.0.2.10"}, 200},
		{"IPv6 proxy and client", "[2001:db8:ffff::1]:1234", []string{"2001:db8::10"}, 200},
		{"invalid peer", "invalid", []string{"192.0.2.10"}, 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/bot"+testToken+"/getMe", nil)
			req.RemoteAddr = tt.peer
			for _, v := range tt.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			req.Header.Set("X-Real-IP", "192.0.2.10")
			req.Header.Set("X-Proxy-Key", "obsolete-key-cannot-grant-access")
			before := upstreamCalls.Load()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status=%d want=%d", rec.Code, tt.want)
			}
			if tt.want == 403 && upstreamCalls.Load() != before {
				t.Fatal("denied request reached upstream")
			}
		})
	}
	// The log uses the same resolved address that grants access.
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["client"] == "service-a" && row["client_ip"] != "192.0.2.10" && row["client_ip"] != "2001:db8::10" {
			t.Fatalf("wrong authorized client IP: %v", row)
		}
	}
}

func TestHandlerAppliesReloadedTrustAndClientsTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(requestPolicy), 0600); err != nil {
		t.Fatal(err)
	}
	current := testAccess(t, requestPolicy)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ok":true}`) }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	h, err := NewHandler(Config{Upstream: u, Access: current, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		WatchAccessFile(ctx, path, time.Millisecond, current, slog.New(slog.DiscardHandler))
	}()
	t.Cleanup(func() { cancel(); <-done })
	check := func(peer, ip string, want int) {
		t.Helper()
		req := httptest.NewRequest("GET", "/bot"+testToken+"/getMe", nil)
		req.RemoteAddr = peer
		req.Header.Set("X-Forwarded-For", ip)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s/%s: %d want %d", peer, ip, rec.Code, want)
		}
	}
	check("10.0.0.2:1234", "192.0.2.10", 200)
	replacement := strings.ReplaceAll(requestPolicy, "10.0.0.2", "10.0.0.4")
	replacement = strings.ReplaceAll(replacement, "192.0.2.10", "192.0.2.20")
	replacement = strings.ReplaceAll(replacement, "service-a", "service-b")
	if err := os.WriteFile(path, []byte(replacement), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for current.Load().Authorize("123456", netip.MustParseAddr("192.0.2.20")) == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	check("10.0.0.2:1234", "192.0.2.10", 403)
	check("10.0.0.2:1234", "192.0.2.20", 403)
	check("10.0.0.4:1234", "192.0.2.20", 200)
}

func TestAdmittedRequestFinishesAfterRevocation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	current := testAccess(t, defaultAccessFixture)
	u, _ := url.Parse(upstream.URL)
	h, err := NewHandler(Config{Upstream: u, Access: current, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(first, httptest.NewRequest("GET", "/bot"+testToken+"/getMe", nil))
	}()
	<-started
	current.Store(testAccess(t, strings.ReplaceAll(defaultAccessFixture, "123456", "999999")).Load())
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("GET", "/bot"+testToken+"/getMe", nil))
	close(release)
	<-done
	if first.Code != 200 || second.Code != 403 {
		t.Fatalf("in-flight=%d new=%d", first.Code, second.Code)
	}
}

func TestAmbiguousClientDeniedByHandler(t *testing.T) {
	p := requestPolicy + " - {name: service-b, bot_ids: ['123456'], allowed_ips: ['192.0.2.10']}\n"
	u, _ := url.Parse("http://127.0.0.1:1")
	h, err := NewHandler(Config{Upstream: u, Access: testAccess(t, p), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/bot"+testToken+"/getMe", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("ambiguous client status=%d", rec.Code)
	}
}
