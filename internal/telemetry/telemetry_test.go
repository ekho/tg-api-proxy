package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestSetupDisabledBySDKEnv(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")

	buf := &bytes.Buffer{}
	shutdown := Setup(context.Background(), slog.New(slog.NewJSONHandler(buf, nil)))

	if shutdown == nil {
		t.Fatal("Setup обязан вернуть непустую функцию остановки")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown при отключённом SDK = %v, want nil", err)
	}
	if !strings.Contains(buf.String(), "OTEL_SDK_DISABLED") {
		t.Errorf("отключение должно быть видно в логе: %s", buf.String())
	}
}

func TestSetupFailsOpenOnBadEndpoint(t *testing.T) {
	// Проверка требует непустые схему и хост (см. комментарий в Setup): один
	// url.Parse почти ничего не отсеивает. Оба случая ниже НЕ вернули бы ошибку
	// от голого url.Parse, поэтому важно проверить их по отдельности — Setup
	// обязан вернуть рабочую заглушку, а не запаниковать и не заблокироваться.
	// Недоступный коллектор не является дырой в безопасности, и падение Alloy
	// не должно ронять прокси.
	tests := []struct {
		name  string
		value string
	}{
		{name: "без схемы", value: "//collector:4318"},
		{name: "без хоста", value: "http://"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tt.value)

			buf := &bytes.Buffer{}
			shutdown := Setup(context.Background(), slog.New(slog.NewJSONHandler(buf, nil)))

			if shutdown == nil {
				t.Fatal("Setup обязан вернуть непустую функцию остановки даже при ошибке")
			}
			ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
			defer cancel()
			if err := shutdown(ctx); err != nil {
				t.Errorf("shutdown заглушки = %v, want nil", err)
			}
		})
	}
}

// TestSetupRoutesSDKInternalLogsToSlog проверяет крючок otel.SetLogger — он
// покрывает внутренние сообщения SDK, а не ошибки экспорта (те покрывает
// otel.SetErrorHandler и проверяют другие тесты).
//
// В установленной версии go.opentelemetry.io/otel (v1.45.0) нет публичного
// геттера вида otel.Logger()/otel.GetLogger() — экспортирован только SetLogger
// (см. `go doc go.opentelemetry.io/otel` в отчёте), поэтому логгер, который
// Setup реально зарегистрировал глобально, нельзя забрать и дёрнуть напрямую.
// Вместо этого вызываем настоящий внутренний путь SDK, а не свой код:
// OTEL_EXPORTER_OTLP_HEADERS без "=" в паре не проходит разбор конфигурации
// экспортёра (internal/envconfig внутри otlptracehttp и otlpmetrichttp), и
// этот разбор сам сообщает об ошибке через global.Error — тот самый крючок
// otel.SetLogger. Так тест проверяет поведение маршрутизации, а не факт
// вызова какой-то нашей обёртки.
func TestSetupRoutesSDKInternalLogsToSlog(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "bad-header-without-equals")

	buf := &bytes.Buffer{}
	shutdown := Setup(context.Background(), slog.New(slog.NewJSONHandler(buf, nil)))
	if shutdown == nil {
		t.Fatal("Setup обязан вернуть непустую функцию остановки")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
		defer cancel()
		_ = shutdown(ctx)
	})

	// Именно так — без закрывающей кавычки — эту ошибку формулирует сам SDK
	// (errors.New("missing '=") в internal/envconfig), поэтому сверяемся с
	// точным текстом, а не с его пересказом.
	if !strings.Contains(buf.String(), "missing '=") {
		t.Fatalf("внутренний лог SDK не попал в переданный slog.Logger: %s", buf.String())
	}

	// Записи обязаны быть валидным JSON: иначе это были бы те самые
	// непарсящиеся строки stdr, от которых должен защищать SetLogger.
	dec := json.NewDecoder(strings.NewReader(buf.String()))
	var found bool
	for {
		var entry map[string]any
		if err := dec.Decode(&entry); err != nil {
			break
		}
		if entry["msg"] == "parse headers" {
			found = true
		}
	}
	if !found {
		t.Errorf("не нашли запись SDK (msg=%q) как валидный JSON: %s", "parse headers", buf.String())
	}
}

func TestDurationBucketsCoverLongPoll(t *testing.T) {
	// Дефолтные бакеты SDK заканчиваются на 10 с, а long poll длится до 45 с:
	// без хвоста за 45 весь getUpdates схлопнулся бы в +Inf, и зависший запрос
	// стал бы неотличим от нормального.
	var maxBucket float64
	for _, b := range DurationBuckets {
		if b > maxBucket {
			maxBucket = b
		}
	}
	if maxBucket < 45 {
		t.Errorf("максимальная граница %v, ожидается не меньше 45 секунд", maxBucket)
	}
	for i := 1; i < len(DurationBuckets); i++ {
		if DurationBuckets[i] <= DurationBuckets[i-1] {
			t.Fatalf("границы обязаны возрастать: %v", DurationBuckets)
		}
	}
}
