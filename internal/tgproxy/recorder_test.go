package tgproxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRecorderCapturesExplicitStatus(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := &recorder{ResponseWriter: rr}

	rec.WriteHeader(http.StatusForbidden)

	if rec.status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.status)
	}
	if rr.Code != http.StatusForbidden {
		t.Errorf("статус не дошёл до нижнего writer'а: %d", rr.Code)
	}
}

func TestRecorderInfersImplicitOK(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := &recorder{ResponseWriter: rr}

	// net/http подразумевает 200 при первой записи без WriteHeader — recorder
	// обязан вывести тот же статус, иначе успешные ответы попадут в лог как 0.
	if _, err := rec.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if rec.statusOrOK() != http.StatusOK {
		t.Errorf("statusOrOK() = %d, want 200", rec.statusOrOK())
	}
	// Проверяется поле, а не statusOrOK(): у того есть собственный откат 0 → 200,
	// который замаскировал бы отсутствие присваивания в Write.
	if rec.status != http.StatusOK {
		t.Errorf("rec.status = %d, want 200: Write обязан вывести неявный статус сам", rec.status)
	}
	if rec.written != 5 {
		t.Errorf("written = %d, want 5", rec.written)
	}
}

func TestRecorderStatusOrOKWithoutAnyWrite(t *testing.T) {
	rec := &recorder{ResponseWriter: httptest.NewRecorder()}

	// Хендлер, не написавший ничего, всё равно приводит к 200 с пустым телом.
	if rec.statusOrOK() != http.StatusOK {
		t.Errorf("statusOrOK() = %d, want 200", rec.statusOrOK())
	}
	if rec.written != 0 {
		t.Errorf("written = %d, want 0", rec.written)
	}
}

func TestRecorderKeepsFirstStatus(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := &recorder{ResponseWriter: rr}

	rec.WriteHeader(http.StatusBadGateway)
	rec.WriteHeader(http.StatusOK)

	if rec.status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502: записывается первый статус", rec.status)
	}
	if rr.Code != http.StatusBadGateway {
		t.Errorf("нижний writer получил %d, want 502: повторный WriteHeader не должен пробрасываться, "+
			"иначе клиент увидит не тот статус, что попал в лог", rr.Code)
	}
}

// flushCounter считает вызовы Flush на нижнем writer'е.
type flushCounter struct {
	http.ResponseWriter
	flushes int
}

func (f *flushCounter) Flush() { f.flushes++ }

func TestRecorderForwardsFlush(t *testing.T) {
	fc := &flushCounter{ResponseWriter: httptest.NewRecorder()}
	rec := &recorder{ResponseWriter: fc}

	rec.Flush()

	if fc.flushes != 1 {
		t.Fatalf("flushes = %d, want 1: без проброса Flush стриминг long poll и "+
			"скачивание файлов начнут отдаваться одним куском в конце", fc.flushes)
	}
}

// shortWriter принимает только часть данных — так ведёт себя нижний writer при
// обрыве соединения. httptest.ResponseRecorder так не умеет, поэтому короткую
// запись без него не воспроизвести.
type shortWriter struct {
	http.ResponseWriter
	limit int
}

func (s *shortWriter) Write(b []byte) (int, error) {
	if len(b) > s.limit {
		b = b[:s.limit]
	}
	n, err := s.ResponseWriter.Write(b)
	return n, err
}

// TestFinalStatusSurvivesInformational1xx — регрессия: апстрим, отправивший
// 100 Continue перед окончательным статусом, не должен подменить статус,
// который увидит клиент. Триггер достижим без нашего участия: libcurl и
// cURL в PHP сами добавляют Expect: 100-continue для крупных и multipart
// POST — то есть для sendDocument/sendVideo.
//
// Обязательно через живой сервер, а не httptest.ResponseRecorder: у него
// собственный guard wroteHeader, не отличающий 1xx от финального статуса, —
// на нём дефект не воспроизводится. У настоящего net/http WriteHeader(1xx) не
// взводит wroteHeader, поэтому последующий WriteHeader с окончательным кодом
// легален и ожидаем; ReverseProxy ретранслирует 1xx через Got1xxResponse.
func TestFinalStatusSurvivesInformational1xx(t *testing.T) {
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusContinue)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":502}`)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("разбор адреса апстрима: %v", err)
	}
	const secret = "0123456789abcdef0123456789abcdef"
	handler, err := NewHandler(Config{
		Upstream: upstreamURL,
		Clients:  testClientsFor(t, secret),
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodGet, proxy.URL+"/bot123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/getMe", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set(HeaderProxyKey, secret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("запрос к прокси: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("клиент увидел статус %d, want %d: guard в recorder.WriteHeader запомнил "+
			"информационный 100 Continue как окончательный статус и отбросил настоящий 502 — "+
			"429/502 от Telegram доехали бы до потребителя как 200, ломая его retry/backoff",
			resp.StatusCode, http.StatusBadGateway)
	}
	if !strings.Contains(logBuf.String(), `"status":502`) {
		t.Errorf("строка лога не содержит status:502 (получено: %s): все три канала "+
			"наблюдаемости сообщили бы status=100, а фильтр LOG_EXCLUDE_METHODS "+
			"заглушил бы такой запрос, потому что 100 < 400", logBuf.String())
	}
}

func TestRecorderCountsOnlyAcceptedBytes(t *testing.T) {
	sw := &shortWriter{ResponseWriter: httptest.NewRecorder(), limit: 3}
	rec := &recorder{ResponseWriter: sw}

	n, err := rec.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 3 {
		t.Errorf("Write вернул %d, want 3", n)
	}
	// Считать нужно принятые байты, а не запрошенные: иначе размер ответа в логе
	// будет завышен на всём, что не дошло до клиента.
	if rec.written != 3 {
		t.Errorf("written = %d, want 3", rec.written)
	}
}
