package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"protecht/internal/core"
)

var testNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustDevice(t *testing.T, st *Store, name string) *core.Device {
	t.Helper()
	d, err := st.CreateDevice(name)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	return d
}

func mustEvidence(t *testing.T, st *Store, deviceID string) *core.Evidence {
	t.Helper()
	content := []byte("evidence-bytes-" + core.NewUUID())
	ev := &core.Evidence{
		ID:          core.NewUUID(),
		DeviceID:    deviceID,
		Kind:        "screenshot",
		SHA256:      core.SHA256Hex(content),
		ByteSize:    int64(len(content)),
		ContentType: "application/octet-stream",
		Content:     content,
		CapturedAt:  testNow.Add(-10 * time.Minute),
		ReceivedAt:  testNow.Add(-9 * time.Minute),
	}
	if err := st.CreateEvidence(ev); err != nil {
		t.Fatalf("create evidence: %v", err)
	}
	return ev
}

func eventFor(deviceID string, occurredAt time.Time, refs ...string) *core.Event {
	return &core.Event{
		ID:          core.NewUUID(),
		DeviceID:    deviceID,
		Type:        "check_in",
		OccurredAt:  occurredAt,
		ReceivedAt:  occurredAt.Add(30 * time.Second),
		EvidenceIDs: refs,
	}
}

func TestEventEvidenceAssociation(t *testing.T) {
	st := openTemp(t)
	alice := mustDevice(t, st, "alice")
	bob := mustDevice(t, st, "bob")
	aliceEv := mustEvidence(t, st, alice.ID)

	t.Run("valid association round-trips", func(t *testing.T) {
		ev := eventFor(alice.ID, testNow.Add(-time.Minute), aliceEv.ID)
		stored, created, err := st.SaveEvent(ev)
		if err != nil {
			t.Fatalf("save event: %v", err)
		}
		if !created {
			t.Fatal("expected created=true")
		}
		got, err := st.GetEvent(alice.ID, stored.ID)
		if err != nil {
			t.Fatalf("get event: %v", err)
		}
		if len(got.EvidenceIDs) != 1 || got.EvidenceIDs[0] != aliceEv.ID {
			t.Fatalf("evidence refs not preserved: %v", got.EvidenceIDs)
		}
	})

	t.Run("evidence of another device is rejected", func(t *testing.T) {
		ev := eventFor(bob.ID, testNow.Add(-time.Minute), aliceEv.ID)
		_, _, err := st.SaveEvent(ev)
		var ae *AssociationError
		if !errors.As(err, &ae) {
			t.Fatalf("expected AssociationError, got %v", err)
		}
		if !strings.Contains(ae.Reason, "different device") {
			t.Fatalf("unexpected reason: %v", ae.Reason)
		}
	})

	t.Run("nonexistent evidence is rejected", func(t *testing.T) {
		ev := eventFor(bob.ID, testNow.Add(-time.Minute), core.NewUUID())
		_, _, err := st.SaveEvent(ev)
		var ae *AssociationError
		if !errors.As(err, &ae) {
			t.Fatalf("expected AssociationError, got %v", err)
		}
		if !strings.Contains(ae.Reason, "does not exist") {
			t.Fatalf("unexpected reason: %v", ae.Reason)
		}
	})

	t.Run("rejected event leaves no rows", func(t *testing.T) {
		rows, err := st.ListEvents(bob.ID)
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("expected no events for bob, got %d", len(rows))
		}
	})
}

func TestDuplicateEventSubmission(t *testing.T) {
	st := openTemp(t)
	d := mustDevice(t, st, "alice")
	ev := mustEvidence(t, st, d.ID)

	original := eventFor(d.ID, testNow.Add(-time.Minute), ev.ID)
	stored, created, err := st.SaveEvent(original)
	if err != nil || !created {
		t.Fatalf("first save: created=%v err=%v", created, err)
	}

	// Replay with the same id but a *different* payload: the server must keep
	// the original record (idempotent, last-write-never).
	replay := eventFor(d.ID, testNow.Add(-time.Hour))
	replay.ID = original.ID
	replay.Type = "tampered_type"
	replay.EvidenceIDs = nil

	stored2, created2, err := st.SaveEvent(replay)
	if err != nil {
		t.Fatalf("replay save: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on duplicate")
	}
	if stored2.Type != stored.Type || !stored2.ReceivedAt.Equal(stored.ReceivedAt) {
		t.Fatalf("duplicate overwrote the original: %+v vs %+v", stored2, stored)
	}
	if len(stored2.EvidenceIDs) != 1 {
		t.Fatalf("expected original evidence refs, got %v", stored2.EvidenceIDs)
	}

	rows, err := st.ListEvents(d.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("duplicate submission created extra rows: %d", len(rows))
	}
}

func TestTimelineTimestampOrdering(t *testing.T) {
	st := openTemp(t)
	d := mustDevice(t, st, "alice")
	base := testNow.Add(-time.Hour)

	// Insert out of order; include a tie on occurred_at resolved by received_at.
	eLate := eventFor(d.ID, base.Add(30*time.Minute))
	eEarly := eventFor(d.ID, base.Add(10*time.Minute))
	eTieB := eventFor(d.ID, base.Add(20*time.Minute))
	eTieB.ReceivedAt = base.Add(21 * time.Minute)
	eTieA := eventFor(d.ID, base.Add(20*time.Minute))
	eTieA.ReceivedAt = base.Add(20 * time.Minute)

	for _, ev := range []*core.Event{eLate, eTieB, eEarly, eTieA} {
		if _, _, err := st.SaveEvent(ev); err != nil {
			t.Fatalf("save event: %v", err)
		}
	}

	rows, err := st.ListEvents(d.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 events, got %d", len(rows))
	}
	wantIDs := []string{eEarly.ID, eTieA.ID, eTieB.ID, eLate.ID}
	for i, want := range wantIDs {
		if rows[i].ID != want {
			t.Fatalf("position %d: expected event %s, got %s (ordering must be occurred_at, then received_at)",
				i, want, rows[i].ID)
		}
	}

	t.Run("timestamps persist in UTC", func(t *testing.T) {
		offset := time.FixedZone("UTC+5", 5*3600)
		ev := eventFor(d.ID, time.Date(2026, 10, 16, 17, 0, 0, 0, offset))
		if _, _, err := st.SaveEvent(ev); err != nil {
			t.Fatalf("save event: %v", err)
		}
		got, err := st.GetEvent(d.ID, ev.ID)
		if err != nil {
			t.Fatalf("get event: %v", err)
		}
		if got.OccurredAt.Location() != time.UTC {
			t.Fatalf("expected UTC, got %v", got.OccurredAt.Location())
		}
		if !got.OccurredAt.Equal(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)) {
			t.Fatalf("unexpected instant after round-trip: %v", got.OccurredAt)
		}
	})
}

func TestEvidenceRetrieval(t *testing.T) {
	st := openTemp(t)
	d := mustDevice(t, st, "alice")
	ev := mustEvidence(t, st, d.ID)

	got, err := st.EvidenceByID(ev.ID)
	if err != nil || got == nil {
		t.Fatalf("evidence by id: got=%v err=%v", got, err)
	}
	if got.SHA256 != ev.SHA256 || got.ContentType != ev.ContentType ||
		!bytes.Equal(got.Content, ev.Content) || !got.CapturedAt.Equal(ev.CapturedAt) {
		t.Fatalf("evidence round-trip mismatch: %+v vs %+v", got, ev)
	}

	missing, err := st.EvidenceByID(core.NewUUID())
	if err != nil || missing != nil {
		t.Fatalf("expected (nil, nil) for unknown id, got %v, %v", missing, err)
	}
}

func TestTokenHashedAtRest(t *testing.T) {
	st := openTemp(t)
	d := mustDevice(t, st, "alice")

	var stored string
	if err := st.db.QueryRow(`SELECT token_hash FROM devices WHERE id = ?`, d.ID).Scan(&stored); err != nil {
		t.Fatalf("query token_hash: %v", err)
	}
	if stored == d.Token {
		t.Fatal("plaintext token must not be stored")
	}
	if stored != core.SHA256Hex([]byte(d.Token)) {
		t.Fatal("expected SHA-256 hash of the token at rest")
	}

	// Authentication still works with the plaintext token, and fails otherwise.
	authed, err := st.DeviceByToken(d.Token)
	if err != nil || authed == nil || authed.ID != d.ID {
		t.Fatalf("token auth failed: %v, %v", authed, err)
	}
	if authed.Token != "" {
		t.Fatal("authenticated device must not echo the token back")
	}
	if nobody, err := st.DeviceByToken("wrong-token"); err != nil || nobody != nil {
		t.Fatalf("wrong token should not authenticate: %v, %v", nobody, err)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	d := mustDevice(t, st, "alice")
	ev := mustEvidence(t, st, d.ID)
	e := eventFor(d.ID, testNow.Add(-time.Minute), ev.ID)
	if _, _, err := st.SaveEvent(e); err != nil {
		t.Fatalf("save event: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	rows, err := st2.ListEvents("")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != e.ID || rows[0].DeviceName != "alice" {
		t.Fatalf("data did not survive reopen: %+v", rows)
	}
	if len(rows[0].Evidence) != 1 || rows[0].Evidence[0].ID != ev.ID || rows[0].Evidence[0].Kind != "screenshot" {
		t.Fatalf("evidence association did not survive reopen: %+v", rows[0].Evidence)
	}
}
