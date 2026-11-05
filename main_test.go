package main

import (
	"os"
	"testing"
	"time"

	"github.com/ekho/tg-api-proxy/internal/telemetry"
	"github.com/goccy/go-yaml"
)

func TestShutdownBudgetFitsComposeGracePeriod(t *testing.T) {
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Grace string `yaml:"stop_grace_period"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatal(err)
	}
	grace, err := time.ParseDuration(compose.Services["tg-api-proxy"].Grace)
	if err != nil {
		t.Fatal(err)
	}
	if budget := shutdownGrace + telemetry.ShutdownTimeout; budget >= grace {
		t.Fatalf("shutdown budget %v exceeds container grace %v", budget, grace)
	}
}
