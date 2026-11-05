package tgproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// HeaderClampedTimeout сообщает клиенту, что long poll оборвал прокси по
// собственному дедлайну, а не Telegram по истечении timeout.
const HeaderClampedTimeout = "X-Proxy-Clamped-Timeout"

// emptyUpdates — ответ, который Telegram отдаёт при истечении long poll без
// обновлений. Прокси синтезирует его при срабатывании дедлайна.
const emptyUpdates = `{"ok":true,"result":[]}`

// defaultLongPollMax — потолок getUpdates; reverse proxy должен ждать дольше.
const defaultLongPollMax = 45 * time.Second

// Config — зависимости обработчика.
type Config struct {
	// Upstream — база Bot API (https://api.telegram.org).
	Upstream *url.URL
	// Access is the atomically published, validated client and proxy policy.
	Access *atomic.Pointer[AccessConfig]
	// LongPollMax — дедлайн для getUpdates.
	LongPollMax time.Duration
	// LogFilter решает, какие запросы попадают в access-лог.
	// nil = логировать всё (см. LogFilter.ShouldLog).
	LogFilter *LogFilter
	// MeterProvider — источник инструментов метрик. nil = глобальный провайдер
	// (после telemetry.Setup) или no-op, если Setup не вызывался.
	MeterProvider metric.MeterProvider
	// TracerProvider — источник спанов. nil = глобальный провайдер (после
	// telemetry.Setup) или no-op, если Setup не вызывался.
	TracerProvider trace.TracerProvider
	// Logger — необязателен, по умолчанию slog.Default().
	Logger *slog.Logger
}

// NewHandler собирает HTTP-обработчик прокси.
func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.Upstream == nil {
		return nil, errors.New("Config.Upstream not set")
	}
	if cfg.Access == nil || cfg.Access.Load() == nil || cfg.Access.Load().withClientIP == nil {
		return nil, errors.New("Config.Access has no validated policy")
	}
	if cfg.LongPollMax <= 0 {
		cfg.LongPollMax = defaultLongPollMax
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		// Покрывает long poll с запасом; сам дедлайн задаётся контекстом запроса.
		ResponseHeaderTimeout: 65 * time.Second,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	meterProvider := cfg.MeterProvider
	if meterProvider == nil {
		meterProvider = otel.GetMeterProvider()
	}
	requestMetrics, err := newMetrics(meterProvider)
	if err != nil {
		return nil, err
	}

	tracerProvider := cfg.TracerProvider
	if tracerProvider == nil {
		tracerProvider = otel.GetTracerProvider()
	}
	tracer := tracerProvider.Tracer(instrumentScope)

	clampedSeconds := strconv.Itoa(int(math.Ceil(cfg.LongPollMax.Seconds())))

	rp := &httputil.ReverseProxy{
		Transport: &upstreamTransport{base: transport, tracer: tracer},
		// ErrorLog иначе остался бы стандартным log.Logger по умолчанию:
		// ReverseProxy печатал бы им обычным текстом в stderr, в обход
		// JSON-потока — например "unexpected EOF", когда апстрим обрывает
		// тело посреди копирования ответа.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = cfg.Upstream.Scheme
			pr.Out.URL.Host = cfg.Upstream.Host
			// Обязательно: Rewrite меняет URL.Host, но заголовок Host остаётся
			// от входящего запроса. Без этой строки Telegram получил бы
			// Host: tg-api-proxy.<домен> и ответил ошибкой, при этом TLS-соединение
			// выглядело бы исправным — SNI берётся из URL.Host.
			pr.Out.Host = cfg.Upstream.Host
			// SetXForwarded намеренно не вызывается: адреса наших потребителей
			// третьей стороне не нужны.
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Истёкший дедлайн long poll — не ошибка, а штатное завершение
			// опроса. Отдаём ровно то, что вернул бы сам Telegram.
			if IsLongPoll(r.URL.Path) && errors.Is(err, context.DeadlineExceeded) {
				if info := infoFrom(r.Context()); info != nil {
					// Клампнутый опрос отдаёт синтетический 200, то есть
					// «успех», и под фильтром по умолчанию молчит. Отличить его
					// от честного пустого ответа Telegram можно только по этому
					// флагу.
					info.clamped = true
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(HeaderClampedTimeout, clampedSeconds)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, emptyUpdates)
				return
			}
			// Причина отказа уходит в reqInfo: строку лога пишет middleware, и
			// без этого в ней остался бы статус 502 без объяснения.
			if info := infoFrom(r.Context()); info != nil {
				info.upstreamErr = err
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			writeUpstreamErrorBody(w)
		},
	}

	// One policy snapshot governs both proxy trust and client authorization.
	core := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policy := cfg.Access.Load()
		policy.withClientIP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := middleware.GetClientIPAddr(r.Context())
			if info := infoFrom(r.Context()); info != nil {
				info.clientIP = "unknown"
				if ip.IsValid() {
					info.clientIP = ip.String()
				}
			}
			botID, ok := ParseBotID(r.URL.Path)
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			client := policy.Authorize(botID, ip)
			if client == nil {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if info := infoFrom(r.Context()); info != nil {
				info.client = client.Name
			}
			if IsLongPoll(r.URL.Path) {
				ctx, cancel := context.WithTimeout(r.Context(), cfg.LongPollMax)
				defer cancel()
				r = r.WithContext(ctx)
			}
			rp.ServeHTTP(w, r)
		})).ServeHTTP(w, r)
	})

	observed := Observe(logger, cfg.LogFilter, requestMetrics, tracer, core)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Liveness не проксирует трафик и не требует клиентских IP-правил.
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		observed.ServeHTTP(w, r)
	}), nil
}
