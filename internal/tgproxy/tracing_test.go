package tgproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// traceFixture поднимает middleware с синхронным экспортёром спанов.
func traceFixture(t *testing.T, next http.Handler) (http.Handler, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() {
		// Не t.Context(): он гарантированно уже отменён к моменту вызова любого
		// Cleanup (так документировано), а TracerProvider.Shutdown при отменённом
		// контексте отказывает немедленно — тест ловил бы "context canceled" на
		// каждом прогоне.
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	h, _ := observeFixtureWithTracer(tp.Tracer("test"), next)
	return h, exp
}

func TestSpanNamedByMethod(t *testing.T) {
	// client выставляется вручную, как это делает core в proxy.go
	// после успешного Lookup: без этого имя спана определялось бы
	// methodLabel()="" и свелось бы к синтетическому "unknown".
	h, exp := traceFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if info := infoFrom(r.Context()); info != nil {
			info.client = testClientName
		}
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil))

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("спанов %d, want 1", len(spans))
	}
	if spans[0].Name != "getme" {
		t.Errorf("имя спана = %q, want getme", spans[0].Name)
	}
}

func TestSpanNamedForUnparsablePath(t *testing.T) {
	h, exp := traceFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot1:se!cret/getMe", nil))

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("спанов %d, want 1", len(spans))
	}
	if spans[0].Name != "unauthorized" {
		t.Errorf("имя спана = %q, want unauthorized", spans[0].Name)
	}
	// Синтетическое имя живёт только в имени спана: атрибут метода обязан
	// остаться пустым, потому что метода в этом случае действительно нет.
	for _, attr := range spans[0].Attributes {
		if attr.Key == "tg.method" && attr.Value.AsString() != "" {
			t.Errorf("tg.method = %q, ожидалось пустое значение", attr.Value.AsString())
		}
	}
}

func TestSpanHasNoPathAttribute(t *testing.T) {
	h, exp := traceFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil))

	for _, attr := range exp.GetSpans()[0].Attributes {
		switch string(attr.Key) {
		case "url.path", "url.full", "http.target", "http.url":
			t.Errorf("атрибут %s содержит путь запроса, а путь несёт токен бота", attr.Key)
		}
	}
}

func TestSpanName(t *testing.T) {
	tests := []struct {
		name   string
		method string
		status int
		want   string
	}{
		{name: "метод известен", method: "getme", status: 200, want: "getme"},
		{name: "401 без метода", method: "", status: 401, want: "unauthorized"},
		{name: "400 без метода", method: "", status: 400, want: "badrequest"},
		{name: "иной статус без метода", method: "", status: 200, want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spanName(tt.method, tt.status); got != tt.want {
				t.Errorf("spanName(%q, %d) = %q, want %q", tt.method, tt.status, got, tt.want)
			}
		})
	}
}

// TestSpanExtractsIncomingTraceparent проверяет, что Observe извлекает
// входящий traceparent, а не всегда открывает новый корневой спан: без
// Extract трейсы не сшивались бы с вызывающими сервисами.
func TestSpanExtractsIncomingTraceparent(t *testing.T) {
	// Глобальный пропагатор по умолчанию — no-op; в проде его выставляет
	// telemetry.Setup. Тест воспроизводит это здесь, чтобы не зависеть от
	// того, был ли этот пакет уже инициализирован, и возвращает прежнее
	// значение, чтобы не задеть остальные тесты пакета глобальной мутацией.
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	h, exp := traceFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if info := infoFrom(r.Context()); info != nil {
			info.client = testClientName
		}
	}))

	// Формат W3C: version-traceid-spanid-flags.
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("спанов %d, want 1", len(spans))
	}
	if got := spans[0].Parent.TraceID().String(); got != traceID {
		t.Errorf("Parent.TraceID() = %q, want %q: входящий traceparent обязан сшивать трейс с вызывающим сервисом", got, traceID)
	}
}
