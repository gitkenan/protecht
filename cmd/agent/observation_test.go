package main

import (
	"strings"
	"testing"
	"time"

	"protecht/internal/core"
)

func TestObservationJSON(t *testing.T) {
	obs := Observation{
		Title:      "a & b <c>",
		Process:    "notepad.exe",
		Executable: `C:\Windows\System32\notepad.exe`,
		CapturedAt: time.Date(2026, 10, 17, 15, 4, 5, 999, time.FixedZone("L", 3600)),
	}
	got, err := obs.JSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"title":"a & b <c>","process":"notepad.exe","executable":"C:\\Windows\\System32\\notepad.exe","captured_at":"2026-10-17T14:04:05Z"}`
	if string(got) != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
	if core.SHA256Hex(got) != core.SHA256Hex([]byte(want)) {
		t.Fatal("hash of encoded observation does not match the document bytes")
	}

	again, err := obs.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(got) {
		t.Fatal("JSON encoding is not stable")
	}
}

func TestObservationJSONUnicodeNotEscaped(t *testing.T) {
	obs := Observation{
		Title:      "café",
		CapturedAt: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC),
	}
	got, err := obs.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "café") {
		t.Fatalf("expected literal unicode title, got %s", got)
	}
}

func TestObservationJSONRejectsZeroTime(t *testing.T) {
	if _, err := (Observation{Title: "x"}).JSON(); err == nil {
		t.Fatal("expected error for zero captured_at")
	}
}

func TestWindowIdentityIgnoresTimestamp(t *testing.T) {
	a := Observation{Title: "A", Process: "a.exe", Executable: `C:\a.exe`, CapturedAt: time.Unix(1, 0)}
	b := a
	b.CapturedAt = time.Unix(2, 0)
	if a.window() != b.window() {
		t.Fatal("timestamp should not change window identity")
	}
	b.Title = "B"
	if a.window() == b.window() {
		t.Fatal("title change should change window identity")
	}
}

func TestFormatObservation(t *testing.T) {
	obs := Observation{
		Title:      `say "hi"`,
		Process:    "Code.exe",
		Executable: `C:\Code.exe`,
		CapturedAt: time.Date(2026, 10, 17, 12, 0, 3, 0, time.UTC),
	}
	got := formatObservation(obs)
	want := `2026-10-17T12:00:03Z  Code.exe  "say \"hi\""  C:\Code.exe`
	if got != want {
		t.Fatalf("format = %q, want %q", got, want)
	}

	empty := formatObservation(Observation{Title: "desktop", CapturedAt: obs.CapturedAt})
	if !strings.Contains(empty, "  -  ") || !strings.HasSuffix(empty, "  -") {
		t.Fatalf("missing path/process should render as -, got %q", empty)
	}
}

func TestProcessName(t *testing.T) {
	tests := []struct {
		exe  string
		want string
	}{
		{`C:\Windows\System32\notepad.exe`, "notepad.exe"},
		{`C:/Windows/System32/notepad.exe`, "notepad.exe"},
		{"notepad.exe", "notepad.exe"},
		{"", ""},
		{"  ", ""},
		{`C:\`, ""},
	}
	for _, tt := range tests {
		if got := processName(tt.exe); got != tt.want {
			t.Errorf("processName(%q) = %q, want %q", tt.exe, got, tt.want)
		}
	}
}

func TestProtocolLabelsFitContract(t *testing.T) {
	if strings.TrimSpace(eventType) == "" || len(eventType) > core.MaxEventTypeLen {
		t.Fatalf("event type %q exceeds the protocol limit", eventType)
	}
	if strings.TrimSpace(evidenceKind) == "" || len(evidenceKind) > core.MaxKindLen {
		t.Fatalf("evidence kind %q exceeds the protocol limit", evidenceKind)
	}
	if eventType != "application.foreground" {
		t.Fatalf("event type = %q", eventType)
	}
}
