package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"protecht/internal/core"
)

func testToken() string { return strings.Repeat("ab", 32) }

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dir", "agent.json")
	cfg := &deviceConfig{ID: core.NewUUID(), Name: "win-agent", Token: testToken()}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o, want 600", fi.Mode().Perm())
		}
	}
	got, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *cfg {
		t.Fatalf("loaded %+v, want %+v", got, cfg)
	}

	cfg.Name = "renamed"
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err = loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" {
		t.Fatalf("overwrite failed: %+v", got)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := loadConfig(filepath.Join(dir, "missing.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(bad); err == nil {
		t.Fatal("expected corrupt config error")
	}

	trailing := filepath.Join(dir, "trailing.json")
	body := `{"id":"` + core.NewUUID() + `","name":"n","token":"` + testToken() + `"} extra`
	if err := os.WriteFile(trailing, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(trailing); err == nil {
		t.Fatal("expected trailing data error")
	}

	weak := &deviceConfig{ID: "not-a-uuid", Name: "n", Token: testToken()}
	if err := saveConfig(filepath.Join(dir, "nope.json"), weak); err == nil {
		t.Fatal("expected invalid id to be rejected")
	}
}

func TestEnsureDeviceRegistersOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	calls := 0
	register := func(name string) (*core.Device, error) {
		calls++
		if name != "pc" {
			t.Fatalf("register name = %q", name)
		}
		return &core.Device{ID: core.NewUUID(), Name: name, Token: testToken()}, nil
	}
	cfg, reused, err := ensureDevice(path, "pc", register)
	if err != nil {
		t.Fatal(err)
	}
	if reused || calls != 1 || cfg.Name != "pc" || cfg.Token != testToken() {
		t.Fatalf("first register: reused=%v calls=%d cfg=%+v", reused, calls, cfg)
	}

	again, reused, err := ensureDevice(path, "other-name", func(string) (*core.Device, error) {
		t.Fatal("register called on second run")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reused || again.ID != cfg.ID || again.Token != cfg.Token || again.Name != "pc" {
		t.Fatalf("reuse failed: %+v reused=%v", again, reused)
	}
}

func TestEnsureDeviceDoesNotReplaceCorruptConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ensureDevice(path, "pc", func(string) (*core.Device, error) {
		t.Fatal("register called for corrupt config")
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected error")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{" {
		t.Fatalf("corrupt config was overwritten: %q", b)
	}
}

func TestEnsureDeviceRegisterFailureWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	_, _, err := ensureDevice(path, "pc", func(string) (*core.Device, error) {
		return nil, errors.New("offline")
	})
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("config file created despite register failure: %v", statErr)
	}
}
