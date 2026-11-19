package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"protecht/internal/core"
)

// deviceConfig is the local record of a registration. The token is the
// bearer secret returned once by the server; only this file keeps it.
type deviceConfig struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

func loadConfig(path string) (*deviceConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var cfg deviceConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("config %s: trailing data", path)
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

func saveConfig(path string, cfg *deviceConfig) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return os.Chmod(path, 0o600)
}

// replaceFile renames tmp onto path. Windows refuses to rename over an
// existing file, so fall back to remove-then-rename there only.
func replaceFile(tmp, path string) error {
	err := os.Rename(tmp, path)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmp, path)
}

func validateConfig(cfg *deviceConfig) error {
	if cfg == nil {
		return errors.New("config is nil")
	}
	if !core.IsUUID(cfg.ID) {
		return errors.New("id must be a UUID")
	}
	if err := core.ValidateDeviceName(cfg.Name); err != nil {
		return fmt.Errorf("name: %w", err)
	}
	if !validToken(cfg.Token) {
		return errors.New("token must be 64 lowercase hex characters")
	}
	return nil
}

func validToken(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// ensureDevice loads a saved registration, or registers and persists one
// when the file does not exist. A present but unreadable file is an error:
// registering again would orphan the device whose token we failed to read.
func ensureDevice(path, name string, register func(string) (*core.Device, error)) (*deviceConfig, bool, error) {
	cfg, err := loadConfig(path)
	if err == nil {
		return cfg, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	dev, err := register(name)
	if err != nil {
		return nil, false, err
	}
	if dev == nil {
		return nil, false, errors.New("register: empty response")
	}
	saved := &deviceConfig{ID: dev.ID, Name: dev.Name, Token: dev.Token}
	if saved.Name == "" {
		saved.Name = name
	}
	if err := saveConfig(path, saved); err != nil {
		return nil, false, fmt.Errorf("registered device %s but failed to save its token: %w", dev.ID, err)
	}
	return saved, false, nil
}
