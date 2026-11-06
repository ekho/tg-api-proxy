package tgproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testBotID      = "123456"
	testToken      = "123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
	testClientName = "service-a"
	testProxyKey   = "test-proxy-key-0123456789"
)

const defaultAccessFixture = `clients:
 - {name: service-a, bot_ids: ['123456'], allowed_ips: ['192.0.2.1', '127.0.0.1', '::1']}
`

// recordedRequest хранит то, что реально доехало до апстрима.
type recordedRequest struct {
	mu       sync.Mutex
	method   string
	path     string
	rawQuery string
	host     string
	headers  http.Header
	body     string
	seen     bool
}

func (r *recordedRequest) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body, _ := io.ReadAll(req.Body)
	r.method = req.Method
	r.path = req.URL.Path
	r.rawQuery = req.URL.RawQuery
	r.host = req.Host
	r.headers = req.Header.Clone()
	r.body = string(body)
	r.seen = true
}

func newTestHandler(t *testing.T, upstreamURL string, longPollMax time.Duration) http.Handler {
	t.Helper()

	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("разбор адреса апстрима: %v", err)
	}
	h, err := NewHandler(Config{
		Upstream:    u,
		Access:      testAccess(t, defaultAccessFixture),
		LongPollMax: longPollMax,
		// Ожидаемые ошибки апстрима не должны засорять вывод тестов.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("создание обработчика: %v", err)
	}
	return h
}

func TestHealthzSkipsAuth(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", time.Second)

	// Liveness доступен без клиентских IP-правил.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestOptionalProxyKey(t *testing.T) {
	const keyHeader = "X-Proxy-Key"
	const policy = `clients:
 - {name: keyed, bot_ids: ['123456'], allowed_ips: ['192.0.2.1'], proxy_key: '` + testProxyKey + `'}
 - {name: key-only, bot_ids: ['234567'], proxy_key: '` + testProxyKey + `-other'}
 - {name: unkeyed, bot_ids: ['345678'], allowed_ips: ['192.0.2.1']}
`
	access := testAccess(t, policy)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values(keyHeader)) != 0 {
			t.Error("proxy key reached upstream")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	var logs bytes.Buffer
	h, err := NewHandler(Config{Upstream: u, Access: access, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, bot, peer string
		keys            []string
		want            int
	}{
		{"correct key and IP", "123456", "192.0.2.1:1234", []string{testProxyKey}, 200},
		{"missing key", "123456", "192.0.2.1:1234", nil, 401},
		{"wrong key", "123456", "192.0.2.1:1234", []string{testProxyKey + "-other"}, 401},
		{"duplicate header", "123456", "192.0.2.1:1234", []string{testProxyKey, testProxyKey}, 401},
		{"wrong IP", "123456", "198.51.100.1:1234", []string{testProxyKey}, 403},
		{"unknown bot", "999999", "192.0.2.1:1234", []string{testProxyKey}, 403},
		{"key only", "234567", "198.51.100.1:1234", []string{testProxyKey + "-other"}, 200},
		{"unkeyed client", "345678", "192.0.2.1:1234", nil, 200},
		{"unkeyed client with header", "345678", "192.0.2.1:1234", []string{testProxyKey}, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/bot"+tt.bot+strings.TrimPrefix(testToken, testBotID)+"/getMe", nil)
			req.RemoteAddr = tt.peer
			for _, key := range tt.keys {
				req.Header.Add(keyHeader, key)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status=%d want=%d", rec.Code, tt.want)
			}
		})
	}
	if strings.Contains(logs.String(), testProxyKey) {
		t.Error("proxy key appeared in request logs")
	}
}

func TestRejectsDisallowedIP(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", time.Second)
	req := httptest.NewRequest(http.MethodPost, "/bot"+testToken+"/sendMessage", nil)
	req.RemoteAddr = "203.0.113.99:1234"
	req.Header.Set("X-Proxy-Key", "obsolete-key")
	req.Header.Set("X-Forwarded-For", "192.0.2.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", rec.Code)
	}
}

func TestRejectsBotNotOwnedByClient(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", time.Second)

	req := httptest.NewRequest(http.MethodPost, "/bot999999:AAHdqTcvCH1vGWJxfSeofSAs/sendMessage", nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("код = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestRejectsNonBotAPIPath(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", time.Second)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("код = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestForwardsRequestToUpstream(t *testing.T) {
	var got recordedRequest

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, `{"ok":true,"result":42}`)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.URL, time.Second)

	path := "/bot" + testToken + "/sendMessage"
	req := httptest.NewRequest(http.MethodPost, path+"?chat_id=1", strings.NewReader(`{"text":"hi"}`))
	req.Host = "tg-api-proxy.example.com"

	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !got.seen {
		t.Fatal("апстрим не получил запрос")
	}

	t.Run("путь и query сохраняются", func(t *testing.T) {
		if got.path != path {
			t.Errorf("путь = %q, want %q", got.path, path)
		}
		if got.rawQuery != "chat_id=1" {
			t.Errorf("query = %q, want %q", got.rawQuery, "chat_id=1")
		}
	})

	t.Run("метод и тело сохраняются", func(t *testing.T) {
		if got.method != http.MethodPost {
			t.Errorf("метод = %q, want POST", got.method)
		}
		if got.body != `{"text":"hi"}` {
			t.Errorf("тело = %q, want %q", got.body, `{"text":"hi"}`)
		}
	})

	t.Run("Host переписан на апстрим", func(t *testing.T) {
		want := strings.TrimPrefix(upstream.URL, "http://")
		if got.host != want {
			t.Errorf("Host = %q, want %q (иначе Telegram получит хост прокси)", got.host, want)
		}
	})

	t.Run("статус и заголовки ответа возвращаются как есть", func(t *testing.T) {
		if rec.Code != http.StatusTeapot {
			t.Errorf("код = %d, want %d", rec.Code, http.StatusTeapot)
		}
		if v := rec.Header().Get("Retry-After"); v != "7" {
			t.Errorf("Retry-After = %q, want %q", v, "7")
		}
		if body := rec.Body.String(); body != `{"ok":true,"result":42}` {
			t.Errorf("тело ответа = %q", body)
		}
	})
}

func TestLongPollDeadlineReturnsEmptyResult(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Апстрим «висит» дольше дедлайна прокси.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	h := newTestHandler(t, upstream.URL, 150*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/bot"+testToken+"/getUpdates?timeout=50", nil)

	rec := httptest.NewRecorder()

	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("дедлайн не сработал, запрос занял %v", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, want %d", rec.Code, http.StatusOK)
	}
	if v := rec.Header().Get(HeaderClampedTimeout); v == "" {
		t.Errorf("нет заголовка %s — вмешательство прокси должно быть видимым", HeaderClampedTimeout)
	}

	var body struct {
		OK     bool              `json:"ok"`
		Result []json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не разбирается как JSON: %v (%q)", err, rec.Body.String())
	}
	if !body.OK {
		t.Error(`ok = false, want true — клиент должен увидеть обычный пустой опрос`)
	}
	if len(body.Result) != 0 {
		t.Errorf("result непустой: %v", body.Result)
	}
}

func TestNonLongPollIsNotDeadlined(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":"slow"}`)
	}))
	defer upstream.Close()

	// Дедлайн long poll короче времени ответа, но к sendMessage он не применяется.
	h := newTestHandler(t, upstream.URL, 50*time.Millisecond)

	req := httptest.NewRequest(http.MethodPost, "/bot"+testToken+"/sendMessage", nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("код = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "slow") {
		t.Errorf("тело = %q, ожидался ответ апстрима", rec.Body.String())
	}
}

// TestClientIsolation — центральный тест фичи: ключ одного клиента не открывает
// ботов другого.
//
// Проверяются обе стороны, а не одна: тест, утверждающий только «свой бот
// проходит», остался бы зелёным, если Allows начнёт возвращать true всегда.
func TestClientIsolation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ok":true}`) }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	policy := `clients:
 - {name: service-a, bot_ids: ['111111'], allowed_ips: ['192.0.2.1']}
 - {name: service-b, bot_ids: ['222222'], allowed_ips: ['198.51.100.1']}
 - {name: shared, bot_ids: ['111111','222222'], allowed_ips: ['203.0.113.1']}
`
	h, err := NewHandler(Config{Upstream: u, Access: testAccess(t, policy), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		ip, bot string
		want    int
	}{
		{"192.0.2.1", "111111", 200}, {"192.0.2.1", "222222", 403},
		{"198.51.100.1", "222222", 200}, {"198.51.100.1", "111111", 403},
		{"203.0.113.1", "111111", 200}, {"203.0.113.1", "222222", 200},
		{"203.0.113.99", "111111", 403},
	} {
		req := httptest.NewRequest("GET", "/bot"+tt.bot+":SYNTHETIC/getMe", nil)
		req.RemoteAddr = tt.ip + ":1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Fatalf("%s/%s: %d want %d", tt.ip, tt.bot, rec.Code, tt.want)
		}
	}
}

// TestUnparseablePathStillBadRequest — валидный ключ не превращает мусорный
// путь в 403: код 400 остаётся за неразобранным путём.
func TestUnparseablePathStillBadRequest(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", time.Second)

	req := httptest.NewRequest(http.MethodGet, "/nonsense/getMe", nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("статус = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
