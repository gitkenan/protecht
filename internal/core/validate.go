package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Protocol limits and rules. These are part of the API contract (see README).
const (
	MaxDeviceNameLen = 100
	MaxKindLen       = 50
	MaxEventTypeLen  = 50
	MaxEvidenceRefs  = 20
	MaxEvidenceBytes = 10 << 20 // 10 MiB

	// FutureSkewTolerance is how far a device-claimed timestamp may be ahead
	// of the server clock before it is rejected. It absorbs normal clock
	// skew while bounding how much a device can "pre-date" the future.
	FutureSkewTolerance = 5 * time.Minute
)

// ValidateDeviceName validates a device registration name.
func ValidateDeviceName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name is required")
	}
	if len(name) > MaxDeviceNameLen {
		return fmt.Errorf("name exceeds %d characters", MaxDeviceNameLen)
	}
	return nil
}

// ValidateEvidenceMeta validates an evidence upload's metadata. The content
// hash comparison happens separately (SHA256Hex); this covers everything else.
// sha256Hex must already be lower-cased by the caller.
func ValidateEvidenceMeta(kind, sha256Hex string, size int64, capturedAt, now time.Time) error {
	if strings.TrimSpace(kind) == "" {
		return errors.New("kind is required")
	}
	if len(kind) > MaxKindLen {
		return fmt.Errorf("kind exceeds %d characters", MaxKindLen)
	}
	if !isSHA256Hex(sha256Hex) {
		return errors.New("sha256 must be 64 lowercase hex characters")
	}
	if size <= 0 {
		return errors.New("content must not be empty")
	}
	if size > MaxEvidenceBytes {
		return fmt.Errorf("content exceeds %d bytes", MaxEvidenceBytes)
	}
	if capturedAt.IsZero() {
		return errors.New("captured_at is required")
	}
	if err := CheckNotFuture(capturedAt, now); err != nil {
		return fmt.Errorf("captured_at: %w", err)
	}
	return nil
}

// ValidateEvent performs structural validation of a submitted event. It does
// not check evidence existence or ownership; that requires storage and is
// done when the event is saved.
func ValidateEvent(ev *Event, now time.Time) error {
	if !IsUUID(ev.ID) {
		return errors.New("id must be a client-generated UUID")
	}
	if strings.TrimSpace(ev.Type) == "" {
		return errors.New("type is required")
	}
	if len(ev.Type) > MaxEventTypeLen {
		return fmt.Errorf("type exceeds %d characters", MaxEventTypeLen)
	}
	if ev.OccurredAt.IsZero() {
		return errors.New("occurred_at is required")
	}
	if err := CheckNotFuture(ev.OccurredAt, now); err != nil {
		return fmt.Errorf("occurred_at: %w", err)
	}
	if len(ev.EvidenceIDs) > MaxEvidenceRefs {
		return fmt.Errorf("an event may reference at most %d evidence items", MaxEvidenceRefs)
	}
	seen := make(map[string]bool, len(ev.EvidenceIDs))
	for _, id := range ev.EvidenceIDs {
		if !IsUUID(id) {
			return fmt.Errorf("evidence reference %q is not a UUID", id)
		}
		if seen[id] {
			return fmt.Errorf("duplicate evidence reference %q", id)
		}
		seen[id] = true
	}
	return nil
}

// CheckNotFuture rejects timestamps more than FutureSkewTolerance ahead of now.
func CheckNotFuture(t, now time.Time) error {
	if t.After(now.Add(FutureSkewTolerance)) {
		return fmt.Errorf("timestamp %s is more than %s ahead of the server clock",
			t.UTC().Format(time.RFC3339), FutureSkewTolerance)
	}
	return nil
}

// NormalizeTime converts t to UTC. All persisted timestamps are UTC.
func NormalizeTime(t time.Time) time.Time { return t.UTC() }

// SHA256Hex returns the lowercase hex SHA-256 digest of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < 64; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
