package tgproxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// TestStreamingNotBufferedThroughMiddleware проверяет, что ответ апстрима течёт
// к клиенту по частям, а не собирается целиком.
//
// Спека называет это требованием продукта: скачивание файлов и long poll должны
// отдаваться потоком. Держится оно на том, что recorder реализует http.Flusher —
// Go не наследует методы, которые тип не объявил явно, поэтому обёртка над
// ResponseWriter теряет этот интерфейс молча. Проверки на этапе компиляции и
// теста-заглушки для этого мало: обе подтверждают, что метод существует, но не
// что поток доходит до настоящего клиента через настоящий ReverseProxy.
//
// Тест поднимает живой сервер намеренно: на httptest.ResponseRecorder эффект
// невоспроизводим, он собирает ответ в буфер по своей природе.
func TestStreamingNotBufferedThroughMiddleware(t *testing.T) {
	released := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"ok":true,`)); err != nil {
			t.Errorf("первая часть: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		// Апстрим молчит до конца теста. Потолка здесь быть не должно: если бы
		// он был, сломанный стриминг всё равно доставил бы клиенту ответ целиком
		// по истечении потолка, и тест прошёл бы, ничего не проверив.
		<-released
		_, _ = w.Write([]byte(`"result":[]}`))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("разбор адреса апстрима: %v", err)
	}

	handler, err := NewHandler(Config{
		Upstream:    upstreamURL,
		Access:      testAccess(t, defaultAccessFixture),
		LongPollMax: 45 * time.Second,
		LogFilter:   NewLogFilter(nil),
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	front := httptest.NewServer(handler)
	defer front.Close()
	// Регистрируется ПОСЛЕ front.Close и потому выполняется РАНЬШЕ него: defer'ы
	// идут в обратном порядке. Иначе front.Close, который ждёт незавершённые
	// запросы, встал бы намертво против обработчика апстрима, всё ещё висящего
	// на этом канале, и провал теста превращался бы в зависание.
	defer close(released)

	// Метод намеренно не getUpdates: к нему применяется дедлайн long poll в 45 с,
	// и провал теста растянулся бы на эти 45 с вместо срабатывания таймаута ниже.
	req, err := http.NewRequest(http.MethodGet, front.URL+"/bot123456:"+e2eSecret+"/getMe", nil)
	if err != nil {
		t.Fatalf("сборка запроса: %v", err)
	}

	// Потолок ставится на клиенте, а не на чтении тела. При буферизации ответа
	// заголовки не уходят до его завершения, поэтому блокируется сам Do — до
	// чтения дело не доходит, и таймаут вокруг чтения не сработал бы никогда.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("ответ не начался за 5 с (%v): ответ буферизуется целиком, стриминг сломан — "+
			"скачивание файлов и long poll будут отдаваться одним куском в конце", err)
	}
	defer resp.Body.Close()

	chunk := []byte(`{"ok":true,`)
	buf := make([]byte, len(chunk))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("чтение первой части: %v", err)
	}
	if string(buf) != string(chunk) {
		t.Errorf("первая часть = %q, want %q", buf, chunk)
	}
}
