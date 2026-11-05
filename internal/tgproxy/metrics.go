package tgproxy

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// instrumentScope — имя scope'а инструментов. По соглашению OTEL это путь
// пакета, выпускающего телеметрию.
const instrumentScope = "github.com/ekho/tg-api-proxy/internal/tgproxy"

// metrics — инструменты, которыми middleware описывает каждый запрос.
//
// Метрики считают ВСЕ запросы всегда, независимо от LOG_EXCLUDE_METHODS: именно
// это делает тишину в логах безопасной. Логи вдобавок не переживают перезапуск
// пода, а метрики переживают.
type metrics struct {
	requests metric.Int64Counter
	duration metric.Float64Histogram
}

// newMetrics создаёт инструменты. Ошибка означает неверное имя или единицу —
// то есть дефект кода, а не окружения.
func newMetrics(mp metric.MeterProvider) (*metrics, error) {
	meter := mp.Meter(instrumentScope)

	requests, err := meter.Int64Counter(
		"tgproxy.requests",
		metric.WithDescription("Обращения к прокси Telegram Bot API"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, fmt.Errorf("counter tgproxy.requests: %w", err)
	}

	duration, err := meter.Float64Histogram(
		"tgproxy.request.duration",
		metric.WithDescription("Длительность обработки запроса"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("histogram tgproxy.request.duration: %w", err)
	}

	return &metrics{requests: requests, duration: duration}, nil
}

// record описывает один завершившийся запрос.
//
// Статус передаётся отдельным аргументом, а не берётся из obsCtx: на момент
// вызова он уже вычислен единожды в Observe с поправкой на панику, и второй
// расчёт мог бы разойтись с первым.
func (m *metrics) record(ctx context.Context, o *obsCtx, status int) {
	if m == nil {
		return
	}
	base := []attribute.KeyValue{
		attribute.String("method", o.methodLabel()),
		attribute.Int("status", status),
		attribute.String("bot", o.botLabel()),
		attribute.String("client", o.clientLabel()),
	}
	m.duration.Record(ctx, time.Since(o.start).Seconds(), metric.WithAttributes(base...))
	// clamped есть только у счётчика: в гистограмме он удвоил бы число серий,
	// не добавив к вопросу «сколько времени занял запрос» ничего.
	m.requests.Add(ctx, 1, metric.WithAttributes(
		append(base, attribute.Bool("clamped", o.info.clamped))...,
	))
}
