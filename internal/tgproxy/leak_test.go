package tgproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// secretPart — секретная часть токена бота.
const secretPart = "AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

// headerSecretPart checks that arbitrary sensitive headers are not logged.
const headerSecretPart = "LEAKCANARY0123456789abcdefabcdef"

// TestNoTokenLeaksIntoObservability — центральный тест дизайна.
//
// Он утверждает ОТСУТСТВИЕ: секрет не появляется ни в логах, ни в спанах, ни в
// метриках. Обычные тесты проверяют, что нужное присутствует, и потому не
// поймали бы регрессию, которую невозможно заметить глазами при ревью:
// достаточно кому-то добавить otelhttp.NewHandler «ради стандартных атрибутов»
// или дописать slog.Any("req", r) — и токены поедут в Tempo, а все остальные
// тесты останутся зелёными.
//
// Проверяются все три канала одним тестом, потому что добавляют их по одному, а
// забывают про остальные два.
func TestNoTokenLeaksIntoObservability(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("разбор адреса апстрима: %v", err)
	}

	logBuf := &bytes.Buffer{}
	spanExp := tracetest.NewInMemoryExporter()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExp))
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	const secret = headerSecretPart

	handler, err := NewHandler(Config{
		Upstream: upstreamURL,
		Access:   testAccess(t, defaultAccessFixture),
		// none: логируем всё, чтобы у токена был максимальный шанс просочиться.
		LogFilter:      NewLogFilter(nil),
		Logger:         slog.New(slog.NewJSONHandler(logBuf, nil)),
		TracerProvider: tracerProvider,
		MeterProvider:  meterProvider,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	// Набор запросов, покрывающий все ветви: успех, отказ авторизации, бот
	// чужого клиента, невалидный путь, скачивание файла.
	requests := []struct {
		name string
		path string
		ip   string
	}{
		{name: "успех", path: "/bot123456:" + secretPart + "/getMe", ip: "192.0.2.1"},
		{name: "отказ авторизации", path: "/bot123456:" + secretPart + "/getMe", ip: "203.0.113.99"},
		{name: "бот чужого клиента", path: "/bot999999:" + secretPart + "/getMe", ip: "192.0.2.1"},
		{name: "невалидный путь", path: "/nonsense/" + secretPart, ip: "192.0.2.1"},
		{name: "скачивание файла", path: "/file/bot123456:" + secretPart + "/photos/f.jpg", ip: "192.0.2.1"},
	}

	for _, req := range requests {
		t.Run(req.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, req.path, nil)
			r.RemoteAddr = req.ip + ":1234"
			r.Header.Set("Authorization", secret)
			handler.ServeHTTP(httptest.NewRecorder(), r)
		})
	}

	if err := tracerProvider.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	// Оба секрета проверяются одним проходом: канал добавляют по одному, а
	// забывают про остальные — и то же самое верно для второго секрета.
	//
	// Слайс, а не map: порядок обхода детерминирован, и вывод упавшего теста не
	// меняется от запуска к запуску.
	secrets := []struct {
		what  string
		value string
	}{
		{what: "токен бота", value: secretPart},
		{what: "секретный заголовок", value: headerSecretPart},
	}

	// Канал 1: логи.
	for _, s := range secrets {
		if strings.Contains(logBuf.String(), s.value) {
			t.Errorf("%s утёк в логи:\n%s", s.what, logBuf.String())
		}
	}

	// Канал 2: спаны — имена и все атрибуты.
	for _, span := range spanExp.GetSpans() {
		for _, s := range secrets {
			if strings.Contains(span.Name, s.value) {
				t.Errorf("%s утёк в имя спана: %q", s.what, span.Name)
			}
			for _, attr := range span.Attributes {
				if strings.Contains(attr.Value.String(), s.value) {
					t.Errorf("%s утёк в атрибут спана %s = %q", s.what, attr.Key, attr.Value.String())
				}
			}
			for _, event := range span.Events {
				for _, attr := range event.Attributes {
					if strings.Contains(attr.Value.String(), s.value) {
						t.Errorf("%s утёк в событие спана %s: %s = %q", s.what, event.Name, attr.Key, attr.Value.String())
					}
				}
			}
		}
	}

	// Канал 3: метрики — сериализуем весь снимок и ищем секрет в нём.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	dump, err := json.Marshal(rm)
	if err != nil {
		t.Fatalf("сериализация метрик: %v", err)
	}
	for _, s := range secrets {
		if bytes.Contains(dump, []byte(s.value)) {
			t.Errorf("%s утёк в метрики:\n%s", s.what, dump)
		}
	}

	// Контроль осмысленности: имя клиента ОБЯЗАНО присутствовать в логе. Без
	// этого тест прошёл бы и в случае, когда наблюдаемость про клиента не знает
	// вовсе, — то есть охранял бы отсутствие того, чего и так нет.
	if !strings.Contains(logBuf.String(), testClientName) {
		t.Errorf("имя клиента отсутствует в логах — тест не отличает «секрет не утёк» от «клиента не видно»:\n%s", logBuf.String())
	}

	// Контроль осмысленности: если ни один канал ничего не записал, тест
	// проходил бы вхолостую.
	if logBuf.Len() == 0 {
		t.Error("логи пусты — тест не проверил ничего")
	}
	if len(spanExp.GetSpans()) == 0 {
		t.Error("спанов нет — тест не проверил ничего")
	}
	if len(rm.ScopeMetrics) == 0 {
		t.Error("метрик нет — тест не проверил ничего")
	}
}
