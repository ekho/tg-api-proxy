// Package telemetry поднимает экспортёры OpenTelemetry.
//
// Пакет намеренно не знает ничего про Bot API, пути запросов и токены: он
// держит подключение к коллектору, и отсутствие у него доступа к пути — это
// гарантия того, что токен бота не может утечь через телеметрию. Всё, что
// связано с разбором путей, живёт в internal/tgproxy.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ShutdownTimeout — предел на сброс буферов экспортёров при остановке.
//
// Складывается с 60 с graceful shutdown сервера и обязан оставаться внутри
// stop_grace_period в compose.yaml.
const ShutdownTimeout = 5 * time.Second

// DurationBuckets — границы гистограммы длительности запроса, в секундах.
//
// Дефолтные бакеты SDK заканчиваются на 10 с, а основной трафик сервиса — long
// poll длиной до 45 с: при них весь getUpdates схлопнулся бы в корзину +Inf, и
// зависший на 44 секундах запрос стал бы неотличим от нормального.
var DurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 45, 60}

// ShutdownFunc сбрасывает буферы экспортёров.
type ShutdownFunc func(context.Context) error

// noop — заглушка для случаев, когда телеметрия не поднялась.
func noop(context.Context) error { return nil }

// Setup поднимает провайдеры трейсов и метрик и регистрирует их глобально.
//
// Адрес коллектора, имя сервиса и протокол берутся из стандартных переменных
// OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME и OTEL_EXPORTER_OTLP_PROTOCOL.
//
// Fail-open: любая ошибка означает предупреждение и no-op провайдеры, а не
// остановку сервиса. Остальная конфигурация проекта fail-closed, потому что
// публичный релей без allowlist — дыра в безопасности; недоступный коллектор
// дырой не является, и падение Alloy не должно ронять прокси Telegram.
func Setup(ctx context.Context, logger *slog.Logger) ShutdownFunc {
	// Ошибки экспорта уводим в slog: дефолтный обработчик пишет в stderr свой
	// текст, и в JSON-потоке появились бы непарсящиеся строки — ровно на самых
	// интересных событиях.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("telemetry export error", "err", err)
	}))

	// otel.SetErrorHandler покрывает только ошибки экспорта. Внутренние
	// сообщения SDK (например, диагностика при разборе переменных окружения)
	// идут через отдельный крючок otel.SetLogger, за которым по умолчанию стоит
	// stdr и пишет обычным текстом в stderr — в JSON-потоке это те же
	// непарсящиеся строки, от которых уже защищает SetErrorHandler, только по
	// другому каналу. Ставим рядом с SetErrorHandler, до всех проверок и
	// раннего выхода, чтобы хук работал и при OTEL_SDK_DISABLED.
	otel.SetLogger(logr.FromSlogHandler(logger.Handler()))

	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		logger.Info("telemetry disabled via OTEL_SDK_DISABLED")
		return noop
	}

	// otlptracehttp.New/otlpmetrichttp.New разбирают OTEL_EXPORTER_OTLP_ENDPOINT
	// сами, но по спецификации OTel ошибку парсинга не возвращают: молча логируют
	// её через otel.SetLogger и откатываются на дефолтный адрес localhost. Без
	// проверки здесь сломанный адрес выглядел бы как успешная настройка и
	// проявился бы только при остановке — попыткой отправить данные в дефолтный
	// адрес вместо быстрого fail-open на старте.
	//
	// Одного url.Parse недостаточно: он почти ничего не отсеивает — строку вида
	// "не url вообще" разбирает как относительный путь и не возвращает ошибку,
	// ловит только битое percent-кодирование. Поэтому дополнительно требуем
	// непустые схему и хост.
	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil {
			logger.Warn("telemetry disabled: OTEL_EXPORTER_OTLP_ENDPOINT not recognized", "err", err)
			return noop
		}
		if u.Scheme == "" || u.Host == "" {
			logger.Warn(fmt.Sprintf("telemetry disabled: OTEL_EXPORTER_OTLP_ENDPOINT: need address with scheme and host, got %q", endpoint))
			return noop
		}
	}

	// Контекст трассировки извлекается из входящих запросов, чтобы трейсы
	// сшивались с вызывающими сервисами.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		logger.Warn("telemetry disabled: failed to create trace exporter", "err", err)
		return noop
	}

	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		logger.Warn("telemetry disabled: failed to create metric exporter", "err", err)
		// Экспортёр трейсов уже создан — закрываем, чтобы не оставить висящих
		// соединений.
		if shutdownErr := traceExp.Shutdown(ctx); shutdownErr != nil {
			logger.Warn("trace exporter shutdown", "err", shutdownErr)
		}
		return noop
	}

	// Ресурс не задаётся явно: по умолчанию SDK берёт resource.Default(), а он
	// читает OTEL_SERVICE_NAME и OTEL_RESOURCE_ATTRIBUTES из окружения.
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp))
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		// PLATFORM.md требует явных бакетов: SDK с 1.33 по умолчанию отдаёт
		// ExponentialHistogram, который платформенный пайплайн может не принять.
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{
				Kind: sdkmetric.InstrumentKindHistogram,
				// Имя продублировано строкой, а не константой из
				// internal/tgproxy.metrics: internal/telemetry — независимый
				// пакет экспорта и не должен знать о internal/tgproxy. Без
				// имени селектор матчил бы любую будущую гистограмму и
				// навязал бы ей бакеты, рассчитанные под long poll до 60 с.
				Name: "tgproxy.request.duration",
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: DurationBuckets,
				},
			},
		)),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)

	return func(ctx context.Context) error {
		return errors.Join(
			tracerProvider.Shutdown(ctx),
			meterProvider.Shutdown(ctx),
		)
	}
}
