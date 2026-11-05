package tgproxy

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitForPolicy(t *testing.T, current *atomic.Pointer[AccessConfig], want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c := current.Load().Authorize("123456", netip.MustParseAddr("192.0.2.10"))
		if c != nil && c.Name == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("policy %q was not applied", want)
}

func TestReloadRetainsLastValidAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(path+".new", []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".new", path); err != nil {
			t.Fatal(err)
		}
	}
	write(accessFixture)
	initial, err := ParseAccessConfig([]byte(accessFixture))
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[AccessConfig]
	current.Store(initial)
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		WatchAccessFile(ctx, path, 5*time.Millisecond, &current, slog.New(slog.NewTextHandler(&logs, nil)))
	}()
	t.Cleanup(func() { cancel(); <-done })
	write("clients: [")
	time.Sleep(60 * time.Millisecond)
	if current.Load() != initial {
		t.Fatal("invalid update replaced the valid policy")
	}
	write(strings.ReplaceAll(accessFixture, "service-a", "service-b"))
	waitForPolicy(t, &current, "service-b")
	replacement := filepath.Join(filepath.Dir(path), "replacement.yaml")
	if err := os.WriteFile(replacement, []byte(strings.ReplaceAll(accessFixture, "service-a", "service-c")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	waitForPolicy(t, &current, "service-c")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	c := current.Load().Authorize("123456", netip.MustParseAddr("192.0.2.10"))
	if c == nil || c.Name != "service-c" {
		t.Fatal("file removal replaced valid policy")
	}
	write(accessFixture)
	waitForPolicy(t, &current, "service-a")
	cancel()
	<-done
	if got := strings.Count(logs.String(), "config rejected"); got != 1 {
		t.Errorf("invalid candidate logged %d times: %s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "config read failed"); got != 1 {
		t.Errorf("read failure logged %d times: %s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "config applied"); got != 3 {
		t.Errorf("applied logged %d times: %s", got, logs.String())
	}
}

func TestReloadConcurrentReadersSeeCompletePolicies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial, err := ParseAccessConfig([]byte(accessFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(accessFixture), 0600); err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[AccessConfig]
	current.Store(initial)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Go(func() { WatchAccessFile(ctx, path, time.Millisecond, &current, slog.New(slog.DiscardHandler)) })
	workers.Go(func() {
		for ctx.Err() == nil {
			c := current.Load().Authorize("123456", netip.MustParseAddr("192.0.2.10"))
			if c == nil || (c.Name != "service-a" && c.Name != "service-b") {
				t.Error("reader observed an incomplete policy")
				return
			}
		}
	})
	t.Cleanup(func() { cancel(); workers.Wait() })
	for _, name := range []string{"service-b", "service-a", "service-b"} {
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(accessFixture, "service-a", name)), 0600); err != nil {
			t.Fatal(err)
		}
		waitForPolicy(t, &current, name)
	}
}

func TestReloadInPlaceSameSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(accessFixture), 0600); err != nil {
		t.Fatal(err)
	}
	current := testAccess(t, accessFixture)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		WatchAccessFile(ctx, path, time.Millisecond, current, slog.New(slog.DiscardHandler))
	}()
	t.Cleanup(func() { cancel(); <-done })
	file, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	raw := strings.ReplaceAll(accessFixture, "service-a", "service-b")
	if _, err := file.WriteAt([]byte(raw), 0); err != nil {
		t.Fatal(err)
	}
	waitForPolicy(t, current, "service-b")
}
