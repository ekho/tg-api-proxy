package main

import (
	"testing"
	"time"

	"github.com/ekho/tg-api-proxy/internal/telemetry"
)

// podGracePeriodFromManifest — копия terminationGracePeriodSeconds из
// k8s/deployment.yaml. Настоящее значение из Go-кода не прочитать, поэтому при
// правке манифеста эту константу нужно менять вручную. Всё остальное в тесте —
// реальные константы, а не их копии, поэтому правка таймаутов в коде теста не
// пройдёт мимо.
const podGracePeriodFromManifest = 75 * time.Second

func TestShutdownBudgetFitsGracePeriod(t *testing.T) {
	budget := shutdownGrace + telemetry.ShutdownTimeout
	if budget >= podGracePeriodFromManifest {
		t.Errorf("бюджет остановки %v не влезает в terminationGracePeriodSeconds %v: "+
			"Kubernetes пришлёт SIGKILL посреди сброса телеметрии",
			budget, podGracePeriodFromManifest)
	}
}
