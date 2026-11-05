package tgproxy

import (
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
	testSecret     = "3f8a1c9e5b7d2f4a6c8e0b1d3f5a7c9e"
	testBotID      = "123456"
	testToken      = "123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
	testClientName = "codo"
)

// testClientsFor собирает список из одного клиента testClientName с заданным
// ключом и единственным ботом testBotID.
//
// Живёт здесь, а не в каждом тесте: шесть файлов поднимают NewHandler, и до
// перехода на Clients каждый повторял одни и те же восемь строк сборки
// зависимостей аутентификации.
func testClientsFor(t *testing.T, key string) *Clients {
	t.Helper()
	c, err := ParseClients(testClientName + fieldSep + key + fieldSep + testBotID)
	if err != nil {
		t.Fatalf("ParseClients: %v", err)
	}
	return c
}

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
		Clients:     testClientsFor(t, testSecret),
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

	// Без заголовка аутентификации — kubelet ходит на пробы без заголовков.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRejectsBadAuth(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", time.Second)

	tests := []struct {
		name   string
		key    string
		setKey bool
	}{
		{name: "заголовок отсутствует", setKey: false},
		{name: "пустой заголовок", key: "", setKey: true},
		{name: "неверный секрет", key: "wrong-secret-value-000000000000", setKey: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/bot"+testToken+"/sendMessage", nil)
			if tt.setKey {
				req.Header.Set(HeaderProxyKey, tt.key)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("код = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestRejectsBotNotOwnedByClient(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", time.Second)

	req := httptest.NewRequest(http.MethodPost, "/bot999999:AAHdqTcvCH1vGWJxfSeofSAs/sendMessage", nil)
	req.Header.Set(HeaderProxyKey, testSecret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("код = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestRejectsNonBotAPIPath(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", time.Second)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set(HeaderProxyKey, testSecret)
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
	req.Header.Set(HeaderProxyKey, testSecret)
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

	t.Run("общий секрет не утекает в Telegram", func(t *testing.T) {
		if v := got.headers.Get(HeaderProxyKey); v != "" {
			t.Errorf("%s дошёл до апстрима со значением %q — секрет утёк", HeaderProxyKey, v)
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
	req.Header.Set(HeaderProxyKey, testSecret)
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
	req.Header.Set(HeaderProxyKey, testSecret)
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
	const (
		codoBot    = "111111"
		vacancyBot = "222222"
		codoKeyE2E = "1111111111111111aaaaaaaaaaaaaaaa"
		vacKeyE2E  = "2222222222222222bbbbbbbbbbbbbbbb"
		legKeyE2E  = "3333333333333333cccccccccccccccc"
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("разбор адреса апстрима: %v", err)
	}

	// legacy владеет обоими ботами — так выглядит переходный период.
	clients, err := ParseClients(
		"codo:" + codoKeyE2E + ":" + codoBot + ";" +
			"vacancy-bot:" + vacKeyE2E + ":" + vacancyBot + ";" +
			"legacy:" + legKeyE2E + ":" + codoBot + "," + vacancyBot)
	if err != nil {
		t.Fatalf("ParseClients: %v", err)
	}

	h, err := NewHandler(Config{
		Upstream: u,
		Clients:  clients,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("создание обработчика: %v", err)
	}

	tests := []struct {
		name  string
		key   string
		botID string
		want  int
	}{
		{name: "свой бот", key: codoKeyE2E, botID: codoBot, want: http.StatusOK},
		{name: "бот другого клиента", key: codoKeyE2E, botID: vacancyBot, want: http.StatusForbidden},
		{name: "свой бот у второго клиента", key: vacKeyE2E, botID: vacancyBot, want: http.StatusOK},
		{name: "бот первого клиента у второго", key: vacKeyE2E, botID: codoBot, want: http.StatusForbidden},
		{name: "legacy и первый бот", key: legKeyE2E, botID: codoBot, want: http.StatusOK},
		{name: "legacy и второй бот", key: legKeyE2E, botID: vacancyBot, want: http.StatusOK},
		{name: "неизвестный ключ", key: "0000000000000000ffffffffffffffff", botID: codoBot, want: http.StatusUnauthorized},
		{name: "пустой ключ", key: "", botID: codoBot, want: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/bot"+tt.botID+":AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/getMe", nil)
			req.Header.Set(HeaderProxyKey, tt.key)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("статус = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

// TestUnparseablePathStillBadRequest — валидный ключ не превращает мусорный
// путь в 403: код 400 остаётся за неразобранным путём.
func TestUnparseablePathStillBadRequest(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", time.Second)

	req := httptest.NewRequest(http.MethodGet, "/nonsense/getMe", nil)
	req.Header.Set(HeaderProxyKey, testSecret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("статус = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
