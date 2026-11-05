package tgproxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestAnonymousRequestsCollapseCardinality — регрессия: newObsCtx разбирал
// путь до всех проверок, а botLabel отдавал сырой bot_id всякий раз, когда
// pathParsed && !botDenied. На 401 проверка allowlist вообще не выполняется,
// поэтому botDenied оставался false, и лейбл метрики полностью управлялся
// анонимным клиентом. То же с именем метода: methodNameRe ограничивает его
// форму (~26^32 вариантов), а не количество.
//
// SDK метрик держит запись на каждый набор атрибутов всё время жизни
// процесса при лимите контейнера 256Mi — без аутентификации это удалённое
// исчерпание памяти, доступное без единого валидного секрета, плюс
// засорение платформенного Prometheus и списка трейсов именами спанов.
func TestAnonymousRequestsCollapseCardinality(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	// Апстрим не должен получить ни одного запроса: все 25 обязаны отсеяться
	// на аутентификации.
	upstreamURL, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("разбор адреса апстрима: %v", err)
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	handler, err := NewHandler(Config{
		Upstream:       upstreamURL,
		Clients:        testClientsFor(t, secret),
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
		MeterProvider:  mp,
		TracerProvider: tp,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	const n = 25
	for i := range n {
		// bot_id разный на каждой итерации (форма как в проде: cifры после
		// "9"), метод — тоже разный и валидной формы (только буквы, как
		// того требует methodNameRe: цифры в имя метода класть нельзя).
		// Секрет один и тот же: его значение не участвует в лейблах и не
		// проходит проверку X-Proxy-Key, которую мы сознательно не ставим.
		path := fmt.Sprintf("/bot9%06d:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/method%c", i, 'a'+i)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("запрос %d: код = %d, want %d (без X-Proxy-Key прокси обязан отвергать все запросы)",
				i, rec.Code, http.StatusUnauthorized)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	seriesCount := 0
	botValues := map[string]struct{}{}
	methodValues := map[string]struct{}{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "tgproxy.requests" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("Data имеет тип %T, want metricdata.Sum[int64]", m.Data)
			}
			seriesCount += len(sum.DataPoints)
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("bot")); ok {
					botValues[v.AsString()] = struct{}{}
				}
				if v, ok := dp.Attributes.Value(attribute.Key("method")); ok {
					methodValues[v.AsString()] = struct{}{}
				}
			}
		}
	}

	spanNames := map[string]struct{}{}
	for _, span := range exporter.GetSpans() {
		spanNames[span.Name] = struct{}{}
	}

	const maxAllowed = 3
	if seriesCount > maxAllowed {
		t.Errorf("серий tgproxy.requests = %d, want <= %d: аноним на публичном роуте без единого "+
			"валидного секрета плодит серии метрики, которые SDK держит в памяти процесса "+
			"(лимит контейнера 256Mi) всё время его жизни — это удалённое исчерпание памяти",
			seriesCount, maxAllowed)
	}
	if len(botValues) > maxAllowed {
		t.Errorf("различных значений bot = %d (%v), want <= %d: атрибут bot обязан быть ограничен "+
			"по кардинальности независимо от аутентификации", len(botValues), keys(botValues), maxAllowed)
	}
	if len(methodValues) > maxAllowed {
		t.Errorf("различных значений method = %d (%v), want <= %d: methodNameRe ограничивает форму "+
			"имени метода (~26^32 вариантов), а не количество — без проверки аутентификации это "+
			"не защита от кардинальности", len(methodValues), keys(methodValues), maxAllowed)
	}
	if len(spanNames) > maxAllowed {
		t.Errorf("различных имён спанов = %d (%v), want <= %d: анонимные запросы обязаны "+
			"схлопываться в одно имя спана, иначе список трейсов засоряется анонимным вводом",
			len(spanNames), keys(spanNames), maxAllowed)
	}
}
