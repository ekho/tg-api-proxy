package tgproxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
)

// A child process isolates ProxyFromEnvironment's process-wide environment cache.
func TestConfiguredOutgoingProxy(t *testing.T) {
	if os.Getenv("TG_PROXY_TEST_CHILD") == "1" {
		u, _ := url.Parse("https://telegram.invalid")
		h, err := NewHandler(Config{Upstream: u, Access: testAccess(t, defaultAccessFixture), Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/bot"+testToken+"/getMe", nil))
		if rec.Code != 502 {
			t.Fatalf("status=%d want 502 from test proxy", rec.Code)
		}
		return
	}
	var connects atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect && r.Host == "telegram.invalid:443" {
			connects.Add(1)
		}
		http.Error(w, "test proxy rejects CONNECT", http.StatusBadGateway)
	}))
	defer proxy.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestConfiguredOutgoingProxy$")
	cmd.Env = append(os.Environ(), "TG_PROXY_TEST_CHILD=1", "HTTPS_PROXY="+proxy.URL, "HTTP_PROXY=", "NO_PROXY=", "ALL_PROXY=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v: %s", err, out)
	}
	if connects.Load() != 1 {
		t.Fatalf("outgoing HTTPS proxy received %d CONNECT calls", connects.Load())
	}
}
