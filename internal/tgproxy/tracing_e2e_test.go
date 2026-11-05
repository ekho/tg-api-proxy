package tgproxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// e2eSecret — секретная часть токена. Ни в одном спане её быть не должно.
const e2eSecret = "AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

// TestSpansNestedAndFreeOfPath проверяет трассировку через целиком собранный
// хендлер, а не через Observe в отдельности.
//
// Три свойства, которые на уровне юнит-тестов не видны:
//
// Вложенность — единственный смысл дочернего спана. Если upstream окажется
// самостоятельным корнем, доля времени, которую съел Telegram, в трейсе
// пропадёт, а на сервисе, где основной метод висит в long poll до 45 секунд,
// это единственный способ отличить «Telegram молчит» от «мы тормозим».
//
// Отсутствие traceparent в запросе к Telegram — по той же причине, по которой
// туда не уходит X-Forwarded-For: телеметрия наших потребителей третьей стороне
// не нужна. Обёртка транспорта переносит контекст только внутри процесса.
//
// Отсутствие пути в атрибутах — потому что путь несёт токен бота. Именно из-за
// этого в проекте не используется otelhttp: он кладёт url.path сам.
func TestSpansNestedAndFreeOfPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tp := r.Header.Get("traceparent"); tp != "" {
			t.Errorf("traceparent доехал до Telegram: %q", tp)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("разбор адреса апстрима: %v", err)
	}
	const secret = "0123456789abcdef0123456789abcdef"

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	handler, err := NewHandler(Config{
		Upstream:       upstreamURL,
		Clients:        testClientsFor(t, secret),
		LongPollMax:    45 * time.Second,
		LogFilter:      NewLogFilter(nil),
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
		TracerProvider: provider,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/bot123456:"+e2eSecret+"/getMe", nil)
	req.Header.Set(HeaderProxyKey, secret)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	spans := exporter.GetSpans()
	var server, child *tracetest.SpanStub
	for i := range spans {
		switch spans[i].Name {
		case "getme":
			server = &spans[i]
		case "upstream":
			child = &spans[i]
		}
	}
	if server == nil {
		t.Fatalf("серверного спана getme нет; собраны: %v", collectedSpanNames(spans))
	}
	if child == nil {
		t.Fatalf("дочернего спана upstream нет; собраны: %v", collectedSpanNames(spans))
	}

	if child.Parent.SpanID() != server.SpanContext.SpanID() {
		t.Error("upstream не вложен в серверный спан: доля времени Telegram в трейсе не видна")
	}
	if child.SpanContext.TraceID() != server.SpanContext.TraceID() {
		t.Error("серверный и дочерний спаны оказались в разных трейсах")
	}

	for _, span := range spans {
		if strings.Contains(span.Name, e2eSecret) {
			t.Errorf("секрет в имени спана: %q", span.Name)
		}
		for _, attr := range span.Attributes {
			switch string(attr.Key) {
			case "url.path", "url.full", "http.target", "http.url":
				t.Errorf("спан %q несёт путь запроса в атрибуте %s", span.Name, attr.Key)
			}
			if strings.Contains(attr.Value.String(), e2eSecret) {
				t.Errorf("секрет в атрибуте %s спана %q", attr.Key, span.Name)
			}
		}
	}
}

// collectedSpanNames нужен только для внятного сообщения об ошибке, когда
// ожидаемого спана в наборе не оказалось.
func collectedSpanNames(spans tracetest.SpanStubs) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name)
	}
	return names
}
