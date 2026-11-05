// Command tg-api-proxy — прозрачный реверс-прокси Telegram Bot API.
//
// Перенаправляет запросы к api.telegram.org напрямую или через HTTPS_PROXY.
// Токены ботов не хранит:
// клиент передаёт токен в пути запроса, прокси передаёт его дальше как есть.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ekho/tg-api-proxy/internal/telemetry"
	"github.com/ekho/tg-api-proxy/internal/tgproxy"
)

// shutdownGrace — сколько ждём завершения текущих запросов при остановке.
// Больше дедлайна long poll (45 с), иначе выкат обрывал бы активные опросы.
// stop_grace_period в compose.yaml покрывает и этот бюджет, и flush телеметрии.
const shutdownGrace = 60 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	settings, err := tgproxy.LoadSettings(os.Getenv)
	if err != nil {
		// Fail-closed: публичный сервис не должен подниматься наполовину
		// настроенным.
		logger.Error("config rejected", "err", err)
		os.Exit(1)
	}
	raw, err := os.ReadFile(settings.ConfigFile)
	if err != nil {
		logger.Error("initial config read failed", "err", err)
		os.Exit(1)
	}
	initial, err := tgproxy.ParseAccessConfig(raw)
	if err != nil {
		logger.Error("initial config rejected", "err", err)
		os.Exit(1)
	}
	var access atomic.Pointer[tgproxy.AccessConfig]
	access.Store(initial)
	settings.Handler.Access = &access
	shutdownTelemetry := telemetry.Setup(context.Background(), logger)
	settings.Handler.Logger = logger

	handler, err := tgproxy.NewHandler(settings.Handler)
	if err != nil {
		logger.Error("failed to build handler", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              settings.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout намеренно не задан. Он отсчитывается от начала чтения
		// запроса, а не от начала записи ответа, поэтому любое конечное
		// значение обрывало бы long poll и скачивание больших файлов.
		// Защита от медленных клиентов — на ReadHeaderTimeout и таймаутах
		// транспорта (см. tgproxy.NewHandler).
		IdleTimeout: 120 * time.Second,
		// ErrorLog иначе остался бы стандартным log.Logger по умолчанию: тот
		// пишет обычным текстом прямо в stderr, в обход JSON-потока, которым
		// пишет всё остальное приложение.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		tgproxy.WatchAccessFile(ctx, settings.ConfigFile, time.Second, &access, logger)
	}()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		<-watchDone
		logger.Info("shutdown signal received, draining in-flight requests")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown failed", "err", err)
		}

		// Сброс телеметрии — строго ПОСЛЕ srv.Shutdown. В момент сигнала висит
		// до 45 с активных long poll, чьи спаны ещё не закрыты: ранний flush
		// отправил бы пустоту и потерял бы ровно те запросы, которые прервал
		// остановку. Бюджет: 60 с + 5 с = 65 с, меньше stop_grace_period в Compose.
		flushCtx, flushCancel := context.WithTimeout(context.Background(), telemetry.ShutdownTimeout)
		defer flushCancel()
		if err := shutdownTelemetry(flushCtx); err != nil {
			logger.Warn("telemetry flush failed", "err", err)
		}
	}()

	logger.Info("proxy started",
		"addr", settings.Addr,
		"upstream", settings.Handler.Upstream.String(),
		"long_poll_max", settings.Handler.LongPollMax.String(),
	)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped with error", "err", err)
		os.Exit(1)
	}

	<-shutdownDone
	logger.Info("stopped")
}
