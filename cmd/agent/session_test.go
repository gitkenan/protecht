package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"protecht/internal/core"
)

func obsAt(title string, at time.Time) Observation {
	return Observation{
		Title:      title,
		Process:    title + ".exe",
		Executable: `C:\` + title + ".exe",
		CapturedAt: at,
	}
}

func TestSessionReportsChangesOnly(t *testing.T) {
	var buf bytes.Buffer
	var calls []Observation
	var eventIDs []string
	var sess session
	pub := func(obs Observation, eventID, evidenceID string) (string, error) {
		if !core.IsUUID(eventID) {
			t.Fatalf("event id %q", eventID)
		}
		if evidenceID != "" {
			t.Fatalf("unexpected evidence id %q", evidenceID)
		}
		calls = append(calls, obs)
		eventIDs = append(eventIDs, eventID)
		return core.NewUUID(), nil
	}

	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	a := obsAt("A", t0)
	sess.observe(a, &buf, pub)
	later := a
	later.CapturedAt = t0.Add(3 * time.Second)
	sess.observe(later, &buf, pub)
	sess.observe(obsAt("B", t0.Add(6*time.Second)), &buf, pub)

	if len(calls) != 2 {
		t.Fatalf("publishes = %d, want 2", len(calls))
	}
	if eventIDs[0] == eventIDs[1] {
		t.Fatal("distinct windows shared an event id")
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"A"`) || !strings.Contains(lines[1], `"B"`) {
		t.Fatalf("printed %q", buf.String())
	}
}

func TestSessionRetriesWithoutReprintOrReupload(t *testing.T) {
	var buf bytes.Buffer
	var logs []string
	var sess session
	sess.logf = func(format string, args ...any) {
		logs = append(logs, format)
	}
	fail := true
	var gotEvidence []string
	var gotTimes []time.Time
	var eventIDs []string
	pub := func(obs Observation, eventID, evidenceID string) (string, error) {
		eventIDs = append(eventIDs, eventID)
		gotEvidence = append(gotEvidence, evidenceID)
		gotTimes = append(gotTimes, obs.CapturedAt)
		if fail {
			fail = false
			return core.NewUUID(), errors.New("submit event: HTTP 500")
		}
		return evidenceID, nil
	}

	t0 := time.Date(2026, 10, 17, 12, 0, 1, 400*int(time.Millisecond), time.UTC)
	first := obsAt("A", t0)
	sess.observe(first, &buf, pub)
	retry := first
	retry.CapturedAt = t0.Add(9 * time.Second)
	sess.observe(retry, &buf, pub)

	if len(eventIDs) != 2 || eventIDs[0] != eventIDs[1] {
		t.Fatalf("event ids = %v, want one stable id", eventIDs)
	}
	if gotEvidence[0] != "" {
		t.Fatalf("first attempt evidence id = %q, want empty", gotEvidence[0])
	}
	if !core.IsUUID(gotEvidence[1]) {
		t.Fatalf("retry did not receive the uploaded evidence id: %q", gotEvidence[1])
	}
	wantAt := t0.UTC().Truncate(time.Second)
	if !gotTimes[0].Equal(wantAt) || !gotTimes[1].Equal(wantAt) {
		t.Fatalf("capture times = %v, want %s both", gotTimes, wantAt)
	}
	if strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("printed more than once: %q", buf.String())
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %v, want one publish error", logs)
	}
}

func TestSessionNewWindowDropsFailedPending(t *testing.T) {
	var sess session
	var dropped bool
	sess.logf = func(format string, args ...any) {
		if strings.Contains(format, "dropping unpublished") {
			dropped = true
		}
	}
	evID := core.NewUUID()
	n := 0
	pub := func(obs Observation, eventID, evidenceID string) (string, error) {
		n++
		if n == 1 {
			return evID, errors.New("submit event: HTTP 500")
		}
		if evidenceID != "" {
			t.Fatalf("new window reused evidence %s", evidenceID)
		}
		if obs.Title != "B" {
			t.Fatalf("published %q", obs.Title)
		}
		return core.NewUUID(), nil
	}
	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	sess.observe(obsAt("A", t0), ioDiscard(), pub)
	sess.observe(obsAt("B", t0.Add(time.Second)), ioDiscard(), pub)
	if !dropped {
		t.Fatal("expected the unpublished observation to be dropped")
	}
}

func ioDiscard() *bytes.Buffer { return &bytes.Buffer{} }
