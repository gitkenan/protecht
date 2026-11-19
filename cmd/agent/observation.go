package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Event and evidence labels are client conventions. The server treats both
// as length-capped free-form strings; see the README API contract.
const (
	eventType    = "application.foreground"
	evidenceKind = "application.foreground"
)

// Observation is one look at the foreground window. CapturedAt is the
// device clock at capture, stored and transmitted in UTC.
type Observation struct {
	Title      string
	Process    string
	Executable string
	CapturedAt time.Time
}

// windowID is the part of an observation that means "the same window".
// The timestamp is not part of it: a poll of an unchanged window is not
// a new observation.
type windowID struct {
	title, process, executable string
}

func (o Observation) window() windowID {
	return windowID{title: o.Title, process: o.Process, executable: o.Executable}
}

// capturedAt is the timestamp the protocol will store: UTC, second
// resolution, matching the RFC 3339 values the server accepts.
func (o Observation) capturedAt() time.Time {
	return o.CapturedAt.UTC().Truncate(time.Second)
}

// JSON encodes the observation as the evidence document. The encoding is
// deterministic: struct field order, no HTML escaping (titles are data, not
// markup), and a second-resolution UTC timestamp. The SHA-256 sent with the
// upload is the hash of these exact bytes.
func (o Observation) JSON() ([]byte, error) {
	if o.CapturedAt.IsZero() {
		return nil, errors.New("observation captured_at is required")
	}
	payload := struct {
		Title      string `json:"title"`
		Process    string `json:"process"`
		Executable string `json:"executable"`
		CapturedAt string `json:"captured_at"`
	}{
		Title:      o.Title,
		Process:    o.Process,
		Executable: o.Executable,
		CapturedAt: o.capturedAt().Format(time.RFC3339),
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&payload); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// formatObservation is the stdout line printed when the foreground window
// changes. Empty process or path is shown as "-", meaning unavailable.
func formatObservation(o Observation) string {
	proc := o.Process
	if proc == "" {
		proc = "-"
	}
	exe := o.Executable
	if exe == "" {
		exe = "-"
	}
	return fmt.Sprintf("%s  %s  %q  %s", o.capturedAt().Format(time.RFC3339), proc, o.Title, exe)
}

// processName is the file name of a Windows image path. Separators are
// handled explicitly so the helper is correct even when compiled on a
// non-Windows GOOS (tests, and filepath.Base follows the host OS).
func processName(exe string) string {
	exe = strings.TrimSpace(exe)
	if exe == "" {
		return ""
	}
	if i := strings.LastIndexAny(exe, `/\`); i >= 0 {
		return exe[i+1:]
	}
	return exe
}
