package core

import (
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func validEvent() *Event {
	return &Event{
		ID:          NewUUID(),
		Type:        "check_in",
		OccurredAt:  testNow.Add(-time.Minute),
		EvidenceIDs: []string{NewUUID(), NewUUID()},
	}
}

func TestValidateEvent(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Event)
		wantErr string
	}{
		{name: "valid", mutate: func(*Event) {}, wantErr: ""},
		{name: "valid without evidence", mutate: func(e *Event) { e.EvidenceIDs = nil }, wantErr: ""},
		{name: "empty id", mutate: func(e *Event) { e.ID = "" }, wantErr: "id"},
		{name: "id not a uuid", mutate: func(e *Event) { e.ID = "not-a-uuid" }, wantErr: "id"},
		{name: "empty type", mutate: func(e *Event) { e.Type = "  " }, wantErr: "type"},
		{name: "type too long", mutate: func(e *Event) { e.Type = strings.Repeat("x", MaxEventTypeLen+1) }, wantErr: "type"},
		{name: "missing occurred_at", mutate: func(e *Event) { e.OccurredAt = time.Time{} }, wantErr: "occurred_at"},
		{
			name:    "occurred_at beyond future tolerance",
			mutate:  func(e *Event) { e.OccurredAt = testNow.Add(FutureSkewTolerance + time.Second) },
			wantErr: "occurred_at",
		},
		{
			name:    "occurred_at within future tolerance",
			mutate:  func(e *Event) { e.OccurredAt = testNow.Add(FutureSkewTolerance - time.Second) },
			wantErr: "",
		},
		{
			name: "too many evidence refs",
			mutate: func(e *Event) {
				e.EvidenceIDs = make([]string, MaxEvidenceRefs+1)
				for i := range e.EvidenceIDs {
					e.EvidenceIDs[i] = NewUUID()
				}
			},
			wantErr: "at most",
		},
		{
			name:    "evidence ref not a uuid",
			mutate:  func(e *Event) { e.EvidenceIDs = []string{"nope"} },
			wantErr: "not a UUID",
		},
		{
			name: "duplicate evidence ref",
			mutate: func(e *Event) {
				id := NewUUID()
				e.EvidenceIDs = []string{id, id}
			},
			wantErr: "duplicate evidence reference",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := validEvent()
			tt.mutate(ev)
			err := ValidateEvent(ev, testNow)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateEvidenceMeta(t *testing.T) {
	validSHA := strings.Repeat("a", 64)
	tests := []struct {
		name       string
		kind, sha  string
		size       int64
		capturedAt time.Time
		wantErr    string
	}{
		{name: "valid", kind: "screenshot", sha: validSHA, size: 100, capturedAt: testNow.Add(-time.Minute), wantErr: ""},
		{name: "empty kind", kind: " ", sha: validSHA, size: 100, capturedAt: testNow, wantErr: "kind"},
		{name: "sha too short", kind: "photo", sha: "abcd", size: 100, capturedAt: testNow, wantErr: "sha256"},
		{name: "sha not hex", kind: "photo", sha: strings.Repeat("z", 64), size: 100, capturedAt: testNow, wantErr: "sha256"},
		{name: "sha uppercase rejected", kind: "photo", sha: strings.Repeat("A", 64), size: 100, capturedAt: testNow, wantErr: "sha256"},
		{name: "empty content", kind: "photo", sha: validSHA, size: 0, capturedAt: testNow, wantErr: "empty"},
		{name: "oversized content", kind: "photo", sha: validSHA, size: MaxEvidenceBytes + 1, capturedAt: testNow, wantErr: "exceeds"},
		{name: "missing captured_at", kind: "photo", sha: validSHA, size: 1, capturedAt: time.Time{}, wantErr: "captured_at"},
		{
			name: "captured_at beyond future tolerance",
			kind: "photo", sha: validSHA, size: 1,
			capturedAt: testNow.Add(FutureSkewTolerance + time.Second),
			wantErr:    "captured_at",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEvidenceMeta(tt.kind, tt.sha, tt.size, tt.capturedAt, testNow)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestTimestampHandling(t *testing.T) {
	t.Run("future boundary is inclusive", func(t *testing.T) {
		if err := CheckNotFuture(testNow.Add(FutureSkewTolerance), testNow); err != nil {
			t.Fatalf("timestamp exactly at tolerance should be accepted: %v", err)
		}
		if err := CheckNotFuture(testNow.Add(FutureSkewTolerance+time.Nanosecond), testNow); err == nil {
			t.Fatal("timestamp past tolerance should be rejected")
		}
	})

	t.Run("far past timestamps are accepted", func(t *testing.T) {
		if err := CheckNotFuture(testNow.Add(-365*24*time.Hour), testNow); err != nil {
			t.Fatalf("old timestamp should be accepted: %v", err)
		}
	})

	t.Run("normalization converts zones to UTC", func(t *testing.T) {
		plusFive := time.Date(2026, 10, 16, 17, 0, 0, 0, time.FixedZone("UTC+5", 5*3600))
		normalized := NormalizeTime(plusFive)
		if normalized.Location() != time.UTC {
			t.Fatalf("expected UTC location, got %v", normalized.Location())
		}
		want := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
		if !normalized.Equal(want) {
			t.Fatalf("expected %v, got %v", want, normalized)
		}
	})

	t.Run("equal instants in different zones normalize equal", func(t *testing.T) {
		a := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
		b := time.Date(2026, 10, 16, 7, 0, 0, 0, time.FixedZone("UTC-5", -5*3600))
		if !NormalizeTime(a).Equal(NormalizeTime(b)) {
			t.Fatal("same instant expressed in two zones must be equal after normalization")
		}
	})
}

func TestIsUUID(t *testing.T) {
	if !IsUUID(NewUUID()) {
		t.Fatal("generated UUID should validate")
	}
	for _, bad := range []string{"", "x", "550e8400-e29b-41d4-a716-44665544000", "550e8400-e29b-41d4-a716-4466554400zz", "550e8400-e29b-41d4-a716-44665544000g"} {
		if IsUUID(bad) {
			t.Fatalf("%q should not validate as UUID", bad)
		}
	}
}
