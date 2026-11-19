package main

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"protecht/internal/core"
)

func TestParseFlags(t *testing.T) {
	opt, err := parseFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opt.server != "http://127.0.0.1:8080" {
		t.Fatalf("server = %q", opt.server)
	}
	if opt.interval != 3*time.Second {
		t.Fatalf("interval = %s", opt.interval)
	}
	if opt.config != "agent.json" {
		t.Fatalf("config = %q", opt.config)
	}
	if err := core.ValidateDeviceName(opt.name); err != nil {
		t.Fatal(err)
	}

	opt, err = parseFlags([]string{
		"-server", "http://127.0.0.1:9",
		"-name", "desk",
		"-interval", "1s",
		"-config", `C:\agent.json`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if opt.server != "http://127.0.0.1:9" || opt.name != "desk" || opt.interval != time.Second || opt.config != `C:\agent.json` {
		t.Fatalf("parsed %+v", opt)
	}
	if _, err := parseFlags([]string{"-interval", "nope"}); err == nil {
		t.Fatal("expected interval parse error")
	}
	if _, err := parseFlags([]string{"extra"}); err == nil {
		t.Fatal("expected unexpected argument error")
	}
}

func TestSanitizeDeviceName(t *testing.T) {
	if got := sanitizeDeviceName("  desk  "); got != "desk" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeDeviceName("   "); got != "win-agent" {
		t.Fatalf("blank name = %q", got)
	}
	if got := sanitizeDeviceName(strings.Repeat("x", core.MaxDeviceNameLen+1)); got != "win-agent" {
		t.Fatalf("long name = %q", got)
	}
}

func TestValidateFlags(t *testing.T) {
	ok := options{server: "http://127.0.0.1:8080", name: "desk", interval: time.Second, config: "agent.json"}
	if err := validateFlags(ok); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.interval = 0
	if err := validateFlags(bad); err == nil {
		t.Fatal("expected interval error")
	}
	bad = ok
	bad.name = " "
	if err := validateFlags(bad); err == nil {
		t.Fatal("expected name error")
	}
	bad = ok
	bad.config = "  "
	if err := validateFlags(bad); err == nil {
		t.Fatal("expected config error")
	}
}

func TestNormalizeBase(t *testing.T) {
	got, err := normalizeBase(" http://127.0.0.1:8080/ ")
	if err != nil || got != "http://127.0.0.1:8080" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := normalizeBase("127.0.0.1:8080"); err == nil {
		t.Fatal("expected scheme error")
	}
	if _, err := normalizeBase("http://user:pass@127.0.0.1:8080"); err == nil {
		t.Fatal("expected userinfo to be rejected")
	}
}

func TestRunRejectsNonWindowsBeforeNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agent runs on windows")
	}
	err := run(options{
		server:   "http://127.0.0.1:9",
		name:     "desk",
		interval: time.Second,
		config:   "agent.json",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "windows") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoopStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int32
	err := loop(ctx, time.Hour, time.Now, func(time.Time) (Observation, error) {
		n.Add(1)
		cancel()
		return Observation{Title: "a", CapturedAt: time.Now()}, nil
	}, func(Observation, string, string) (string, error) {
		return core.NewUUID(), nil
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != 1 {
		t.Fatalf("captures = %d, want 1", n.Load())
	}
}

func TestLoopContinuesAfterCaptureError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var captures atomic.Int32
	var publishes atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- loop(ctx, 10*time.Millisecond, time.Now, func(time.Time) (Observation, error) {
			n := captures.Add(1)
			if n == 1 {
				return Observation{}, errors.New("no foreground window")
			}
			if n >= 3 {
				cancel()
			}
			return Observation{Title: "ok", CapturedAt: time.Now()}, nil
		}, func(Observation, string, string) (string, error) {
			publishes.Add(1)
			return core.NewUUID(), nil
		}, io.Discard)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("loop did not stop")
	}
	if publishes.Load() < 1 {
		t.Fatal("expected a publish after the capture error")
	}
	if captures.Load() < 3 {
		t.Fatalf("captures = %d, want at least 3", captures.Load())
	}
}
