package tgproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

// rawLogLines разбирает JSON-лог в набор записей — все до единой.
func rawLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("строка лога не JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// logLines возвращает только строки access-лога.
//
// Разовая диагностика края (edgeDiagMsg) отфильтрована: она пишется в тот же
// буфер, но access-логом не является, и без фильтра каждый тест, читающий
// lines[0], получал бы её вместо своего запроса. Тем, кто проверяет саму
// диагностику, нужен rawLogLines.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range rawLogLines(t, buf) {
		if rec["msg"] == edgeDiagMsg {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// observeFixture поднимает middleware поверх заданного хендлера.
func observeFixture(excluded []string, next http.Handler) (http.Handler, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))
	m, err := newMetrics(noopmetric.NewMeterProvider())
	if err != nil {
		panic(err)
	}
	return Observe(logger, NewLogFilter(excluded), m, nooptrace.NewTracerProvider().Tracer("test"), next), buf
}

// observeFixtureWithTracer — вариант observeFixture с заданным трейсером.
func observeFixtureWithTracer(tracer trace.Tracer, next http.Handler) (http.Handler, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))
	m, err := newMetrics(noopmetric.NewMeterProvider())
	if err != nil {
		panic(err)
	}
	return Observe(logger, NewLogFilter(nil), m, tracer, next), buf
}

func TestObserveLogsSuccess(t *testing.T) {
	h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	req.Header.Set("User-Agent", "python-telegram-bot/21.0")
	h.ServeHTTP(httptest.NewRecorder(), req)

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("строк лога %d, want 1", len(lines))
	}
	rec := lines[0]

	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if rec["method"] != "getme" {
		t.Errorf("method = %v, want getme", rec["method"])
	}
	if rec["status"] != float64(200) {
		t.Errorf("status = %v, want 200", rec["status"])
	}
	if rec["bytes"] != float64(11) {
		t.Errorf("bytes = %v, want 11", rec["bytes"])
	}
	if rec["bot_id"] != "123456" {
		t.Errorf("bot_id = %v, want 123456", rec["bot_id"])
	}
	if rec["client_ip"] != "192.0.2.1" {
		t.Errorf("client_ip = %v, want TCP peer without trusted IP middleware", rec["client_ip"])
	}
	if rec["ua"] != "python-telegram-bot/21.0" {
		t.Errorf("ua = %v", rec["ua"])
	}
	if _, ok := rec["dur_ms"]; !ok {
		t.Error("dur_ms отсутствует")
	}
	if path, _ := rec["path"].(string); !strings.Contains(path, "REDACTED") {
		t.Errorf("path = %q, ожидалась замаскированная форма", path)
	}
}

func TestObserveExcludedMethodSilentOnSuccess(t *testing.T) {
	h, buf := observeFixture([]string{"getUpdates"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getUpdates", nil))

	if got := logLines(t, buf); len(got) != 0 {
		t.Errorf("ожидалась тишина в access-логе, получено: %v", got)
	}
}

func TestObserveExcludedMethodLoggedOnError(t *testing.T) {
	h, buf := observeFixture([]string{"getUpdates"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getUpdates", nil))

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("строк лога %d, want 1: ошибки исключённого метода логируются всегда", len(lines))
	}
	if lines[0]["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", lines[0]["level"])
	}
}

func TestObserveUnauthorizedHasNoMethodOrPath(t *testing.T) {
	h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	// Путь с секретом недопустимой формы: маскирование Redact на нём неполно,
	// поэтому path в лог попадать не должен вовсе.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot1:se!cret/getMe", nil))

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("строк лога %d, want 1", len(lines))
	}
	rec := lines[0]
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
	if _, ok := rec["path"]; ok {
		t.Errorf("path не должен логироваться для неразобранного пути: %v", rec["path"])
	}
	if _, ok := rec["bot_id"]; ok {
		t.Errorf("bot_id не должен логироваться для неразобранного пути: %v", rec["bot_id"])
	}
	if _, ok := rec["method"]; ok {
		t.Errorf("method не должен логироваться при 401: %v", rec["method"])
	}
}

func TestObserveRecoversPanicBeforeHeaders(t *testing.T) {
	h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("что-то сломалось")
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("строк лога %d, want 1", len(lines))
	}
	if lines[0]["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", lines[0]["level"])
	}
	if lines[0]["panic"] != "что-то сломалось" {
		t.Errorf("panic = %v", lines[0]["panic"])
	}
}

func TestObserveAbortsPanicAfterHeaders(t *testing.T) {
	h, buf := observeFixture([]string{"getUpdates"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("частичное тело"))
		panic("паника посреди стриминга")
	}))

	defer func() {
		p := recover()
		if p != http.ErrAbortHandler {
			t.Errorf("recover() = %v, want http.ErrAbortHandler: подменить статус нельзя, "+
				"а тихо отдать обрезанное тело как валидный ответ хуже, чем оборвать соединение", p)
		}
		// Паника логируется даже для исключённого метода со статусом 200:
		// фильтр не должен прятать аварию.
		lines := logLines(t, buf)
		if len(lines) != 1 {
			t.Fatalf("строк лога %d, want 1", len(lines))
		}
		if lines[0]["level"] != "ERROR" {
			t.Errorf("level = %v, want ERROR", lines[0]["level"])
		}
		// Статус обязан остаться 200: заголовки ушли до паники, подменить его
		// невозможно. Без этой проверки мутация `panicked &&` → `panicked ||`
		// в obsCtx.status прошла бы незамеченной.
		if lines[0]["status"] != float64(200) {
			t.Errorf("status = %v, want 200: после отправки заголовков статус не подменяется", lines[0]["status"])
		}
	}()

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getUpdates", nil))
}

func TestObserveLogsAbortHandlerAsWarnWithoutPanicField(t *testing.T) {
	h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("тело до обрыва"))
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		p := recover()
		if p != http.ErrAbortHandler {
			t.Errorf("recover() = %v, want http.ErrAbortHandler: сигнал ReverseProxy обязан долетать до net/http", p)
		}
		lines := logLines(t, buf)
		if len(lines) != 1 {
			t.Fatalf("строк лога %d, want 1", len(lines))
		}
		rec := lines[0]
		if rec["level"] != "WARN" {
			t.Errorf("level = %v, want WARN: это штатный сигнал ReverseProxy, а не авария нашего кода", rec["level"])
		}
		if _, ok := rec["panic"]; ok {
			t.Errorf("поле panic не должно появляться для http.ErrAbortHandler: %v", rec["panic"])
		}
		if rec["aborted"] != true {
			t.Errorf("aborted = %v, want true", rec["aborted"])
		}
	}()

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil))
}

func TestObserveTruncatesUserAgent(t *testing.T) {
	h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil)
	req.Header.Set("User-Agent", strings.Repeat("a", 500))
	h.ServeHTTP(httptest.NewRecorder(), req)

	ua, _ := logLines(t, buf)[0]["ua"].(string)
	if len(ua) != maxUALen {
		t.Errorf("len(ua) = %d, want %d: длину UA задаёт клиент", len(ua), maxUALen)
	}
}

func TestObserveFallsBackToRemoteAddr(t *testing.T) {
	h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil)
	req.RemoteAddr = "192.0.2.44:54321"
	h.ServeHTTP(httptest.NewRecorder(), req)

	if ip := logLines(t, buf)[0]["client_ip"]; ip != "192.0.2.44" {
		t.Errorf("client_ip = %v, want 192.0.2.44 без порта", ip)
	}
}

func TestLevelFor(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		panicVal any
		want     slog.Level
	}{
		{name: "обычный успех", status: 200, want: slog.LevelInfo},
		{name: "перенаправление отказом не является", status: 302, want: slog.LevelInfo},
		{name: "399 — на единицу ниже границы WARN", status: 399, want: slog.LevelInfo},
		{name: "400 — ровно граница WARN", status: 400, want: slog.LevelWarn},
		{name: "499 — на единицу ниже границы ERROR", status: 499, want: slog.LevelWarn},
		{name: "500 — ровно граница ERROR", status: 500, want: slog.LevelError},
		{name: "недоступный апстрим", status: 502, want: slog.LevelError},
		{name: "паника при успешном статусе всё равно ERROR", status: 200, panicVal: "сломалось", want: slog.LevelError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := levelFor(tt.status, tt.panicVal); got != tt.want {
				t.Errorf("levelFor(%d, %v) = %v, want %v", tt.status, tt.panicVal, got, tt.want)
			}
		})
	}
}

func TestObserveLogsReqInfoFields(t *testing.T) {
	h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := infoFrom(r.Context())
		if info == nil {
			t.Fatal("reqInfo не доехал через контекст: ErrorHandler у ReverseProxy получает исходящий клон запроса, и обвязка держится на унаследованном контексте")
		}
		info.clamped = true
		info.upstreamErr = errors.New("dial tcp: connection refused")
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getUpdates", nil))

	rec := logLines(t, buf)[0]
	if rec["clamped"] != true {
		t.Errorf("clamped = %v, want true: иначе клампнутый опрос неотличим от честного пустого ответа Telegram", rec["clamped"])
	}
	if rec["upstream_err"] != "dial tcp: connection refused" {
		t.Errorf("upstream_err = %v: без него в строке остался бы статус 502 без объяснения причины", rec["upstream_err"])
	}
}

// TestClientLabel — значение лейбла до и после аутентификации.
func TestClientLabel(t *testing.T) {
	tests := []struct {
		name   string
		client string
		want   string
	}{
		{name: "аноним", client: "", want: clientUnknown},
		{name: "клиент известен", client: "service-a", want: "service-a"},
		{name: "имя с дефисом", client: "vacancy-bot", want: "vacancy-bot"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &obsCtx{info: &reqInfo{client: tt.client}}
			if got := o.clientLabel(); got != tt.want {
				t.Errorf("clientLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLogCarriesClientName — имя клиента попадает в строку лога при успехе и
// отсутствует до аутентификации.
//
// Это то, чем в эксплуатации отвечают на вопрос «кто ещё ходит со старым общим
// ключом»: без имени в наблюдаемости момент, когда запись legacy можно удалить,
// определить нечем.
func TestLogCarriesClientName(t *testing.T) {
	const path = "/bot123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/getMe"

	t.Run("аутентифицированный запрос", func(t *testing.T) {
		h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if info := infoFrom(r.Context()); info != nil {
				info.client = "vacancy-bot"
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))

		lines := logLines(t, buf)
		if len(lines) != 1 {
			t.Fatalf("строк лога = %d, want 1", len(lines))
		}
		if got := lines[0]["client"]; got != "vacancy-bot" {
			t.Errorf("client = %v, want %q", got, "vacancy-bot")
		}
	})

	t.Run("до аутентификации поля нет", func(t *testing.T) {
		h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))

		lines := logLines(t, buf)
		if len(lines) != 1 {
			t.Fatalf("строк лога = %d, want 1", len(lines))
		}
		if _, present := lines[0]["client"]; present {
			t.Errorf("поле client не должно появляться до аутентификации: %v", lines[0])
		}
	})
}

// --- Цепочка X-Forwarded-For ------------------------------------------------

func TestForwardedChainLoggedWhenChainHasHops(t *testing.T) {
	h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 100.125.24.19")
	h.ServeHTTP(httptest.NewRecorder(), req)

	rec := logLines(t, buf)[0]
	if got := rec["forwarded_chain"]; got != "203.0.113.7, 100.125.24.19" {
		t.Errorf("forwarded_chain = %v, want всю цепочку целиком", got)
	}
	if got := rec["client_ip"]; got != "192.0.2.1" {
		t.Errorf("client_ip = %v, want TCP peer without trusted IP middleware", got)
	}
}

func TestForwardedChainOmittedForSingleHop(t *testing.T) {
	h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil)
	req.Header.Set("X-Forwarded-For", "100.125.24.19")
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Один хоп полностью повторяет client_ip. Дублировать его в каждой строке
	// значит платить объёмом лога за нулевую информацию.
	if _, present := logLines(t, buf)[0]["forwarded_chain"]; present {
		t.Error("forwarded_chain не должен появляться, когда в цепочке один адрес")
	}
}

// --- Разовая диагностика края ------------------------------------------------

func TestEdgeDiagnosticLoggedOncePerProcess(t *testing.T) {
	h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for range 3 {
		req := httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil)
		req.Header.Set("X-Real-Ip", "100.125.24.19")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	var diag []map[string]any
	for _, rec := range rawLogLines(t, buf) {
		if rec["msg"] == edgeDiagMsg {
			diag = append(diag, rec)
		}
	}
	if len(diag) != 1 {
		t.Fatalf("строк диагностики = %d, want ровно 1 на процесс", len(diag))
	}
	if got := diag[0]["x_real_ip"]; got != "100.125.24.19" {
		t.Errorf("x_real_ip = %v, want значение заголовка", got)
	}
	if got, _ := diag[0]["remote_addr"].(string); got == "" {
		t.Error("remote_addr должен попасть в диагностику: это адрес пода ingress")
	}
}

func TestEdgeDiagnosticNeverLogsSecretHeaderValues(t *testing.T) {
	const secret = "s3cret-proxy-key-value"

	h, buf := observeFixture(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil)

	req.Header.Set("Authorization", "Bearer "+secret)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(buf.String(), secret) {
		t.Fatal("значение секретного заголовка утекло в лог диагностики")
	}
	// Имя заголовка — не секрет, и именно оно показывает, что край шлёт нам
	// заголовок, о котором мы не знали.
	var diag map[string]any
	for _, rec := range rawLogLines(t, buf) {
		if rec["msg"] == edgeDiagMsg {
			diag = rec
		}
	}
	if diag == nil {
		t.Fatal("диагностика не записана")
	}
	if names, _ := diag["headers"].(string); !strings.Contains(names, "Authorization") {
		t.Errorf("headers = %q, want перечисление имён включая %s", names, "Authorization")
	}
}
