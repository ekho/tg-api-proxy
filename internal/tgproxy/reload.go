package tgproxy

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// WatchAccessFile publishes a candidate only after all access rules validate.
func WatchAccessFile(ctx context.Context, path string, interval time.Duration, current *atomic.Pointer[AccessConfig], logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	seen := current.Load().digest
	lastReadError := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			raw, err := os.ReadFile(path)
			if err != nil {
				if err.Error() != lastReadError {
					logger.Error("config read failed; keeping previous policy", "err", err)
					lastReadError = err.Error()
				}
				continue
			}
			digest := sha256.Sum256(raw)
			if digest == seen && lastReadError == "" {
				continue
			}
			seen = digest
			lastReadError = ""
			candidate, err := ParseAccessConfig(raw)
			if err != nil {
				logger.Error("config rejected; keeping previous policy", "err", err)
				continue
			}
			current.Store(candidate)
			logger.Info("config applied", "clients", len(candidate.clients))
		}
	}
}
