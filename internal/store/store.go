// Package store persists devices, evidence and events in SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"protecht/internal/core"
)

// AssociationError is returned when an event references evidence that cannot
// be attached to it (missing, or owned by a different device).
type AssociationError struct {
	EvidenceID string
	Reason     string
}

func (e *AssociationError) Error() string {
	return fmt.Sprintf("evidence %s: %s", e.EvidenceID, e.Reason)
}

// EvidenceRef is a lightweight pointer to an evidence item, used in timelines.
type EvidenceRef struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// EventRow is an event enriched for timeline display.
type EventRow struct {
	ID         string        `json:"id"`
	DeviceID   string        `json:"device_id"`
	DeviceName string        `json:"device_name"`
	Type       string        `json:"type"`
	OccurredAt time.Time     `json:"occurred_at"`
	ReceivedAt time.Time     `json:"received_at"`
	Evidence   []EvidenceRef `json:"evidence"`
}

const schema = `
CREATE TABLE IF NOT EXISTS devices (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS evidence (
    id           TEXT PRIMARY KEY,
    device_id    TEXT NOT NULL REFERENCES devices(id),
    kind         TEXT NOT NULL,
    sha256       TEXT NOT NULL,
    byte_size    INTEGER NOT NULL,
    content_type TEXT NOT NULL,
    content      BLOB NOT NULL,
    captured_at  TEXT NOT NULL,
    received_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
    device_id   TEXT NOT NULL REFERENCES devices(id),
    id          TEXT NOT NULL,
    type        TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    received_at TEXT NOT NULL,
    PRIMARY KEY (device_id, id)
);
CREATE TABLE IF NOT EXISTS event_evidence (
    event_device_id TEXT NOT NULL,
    event_id        TEXT NOT NULL,
    evidence_id     TEXT NOT NULL REFERENCES evidence(id),
    position        INTEGER NOT NULL,
    PRIMARY KEY (event_device_id, event_id, position),
    FOREIGN KEY (event_device_id, event_id) REFERENCES events(device_id, id)
);
`

// Store wraps the SQLite connection.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the database at path and migrates it.
// Use ":memory:" for an ephemeral store.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on", path))
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time. A single pooled connection
	// serializes access and keeps the prototype free of SQLITE_BUSY; all
	// store methods must finish one query before starting the next.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// CreateDevice registers a new device and returns it including its plaintext
// token. Only the token's SHA-256 hash is stored.
func (s *Store) CreateDevice(name string) (*core.Device, error) {
	d := &core.Device{
		ID:        core.NewUUID(),
		Name:      name,
		Token:     core.NewToken(),
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO devices (id, name, token_hash, created_at) VALUES (?,?,?,?)`,
		d.ID, d.Name, core.SHA256Hex([]byte(d.Token)), formatTime(d.CreatedAt),
	)
	if err != nil {
		return nil, fmt.Errorf("insert device: %w", err)
	}
	return d, nil
}

// DeviceByToken authenticates a plaintext bearer token and returns the owning
// device (without the token), or (nil, nil) if the token is unknown.
func (s *Store) DeviceByToken(token string) (*core.Device, error) {
	var d core.Device
	var created string
	err := s.db.QueryRow(
		`SELECT id, name, created_at FROM devices WHERE token_hash = ?`,
		core.SHA256Hex([]byte(token)),
	).Scan(&d.ID, &d.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup device: %w", err)
	}
	if d.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &d, nil
}

// CreateEvidence persists an evidence record and its content.
func (s *Store) CreateEvidence(ev *core.Evidence) error {
	_, err := s.db.Exec(
		`INSERT INTO evidence (id, device_id, kind, sha256, byte_size, content_type, content, captured_at, received_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		ev.ID, ev.DeviceID, ev.Kind, ev.SHA256, ev.ByteSize, ev.ContentType, ev.Content,
		formatTime(ev.CapturedAt), formatTime(ev.ReceivedAt),
	)
	if err != nil {
		return fmt.Errorf("insert evidence: %w", err)
	}
	return nil
}

// EvidenceByID returns the evidence (including content) with the given id,
// or (nil, nil) if it does not exist.
func (s *Store) EvidenceByID(id string) (*core.Evidence, error) {
	var ev core.Evidence
	var captured, received string
	err := s.db.QueryRow(
		`SELECT id, device_id, kind, sha256, byte_size, content_type, content, captured_at, received_at
		 FROM evidence WHERE id = ?`, id,
	).Scan(&ev.ID, &ev.DeviceID, &ev.Kind, &ev.SHA256, &ev.ByteSize, &ev.ContentType,
		&ev.Content, &captured, &received)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load evidence: %w", err)
	}
	if ev.CapturedAt, err = parseTime(captured); err != nil {
		return nil, err
	}
	if ev.ReceivedAt, err = parseTime(received); err != nil {
		return nil, err
	}
	return &ev, nil
}

// SaveEvent validates evidence ownership, then inserts the event. If an event
// with the same (device, id) already exists, the submission is treated as an
// idempotent replay: the originally stored event is returned with created=false
// and nothing is modified.
func (s *Store) SaveEvent(ev *core.Event) (stored *core.Event, created bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	for _, ref := range ev.EvidenceIDs {
		var owner string
		err := tx.QueryRow(`SELECT device_id FROM evidence WHERE id = ?`, ref).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, &AssociationError{EvidenceID: ref, Reason: "does not exist"}
		}
		if err != nil {
			return nil, false, fmt.Errorf("check evidence: %w", err)
		}
		if owner != ev.DeviceID {
			return nil, false, &AssociationError{EvidenceID: ref, Reason: "belongs to a different device"}
		}
	}

	res, err := tx.Exec(
		`INSERT INTO events (device_id, id, type, occurred_at, received_at)
		 VALUES (?,?,?,?,?) ON CONFLICT(device_id, id) DO NOTHING`,
		ev.DeviceID, ev.ID, ev.Type, formatTime(ev.OccurredAt), formatTime(ev.ReceivedAt),
	)
	if err != nil {
		return nil, false, fmt.Errorf("insert event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}

	if n == 1 {
		for i, ref := range ev.EvidenceIDs {
			if _, err := tx.Exec(
				`INSERT INTO event_evidence (event_device_id, event_id, evidence_id, position)
				 VALUES (?,?,?,?)`, ev.DeviceID, ev.ID, ref, i,
			); err != nil {
				return nil, false, fmt.Errorf("link evidence: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return ev, true, nil
	}

	// Duplicate: return the original record, untouched.
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	existing, err := s.GetEvent(ev.DeviceID, ev.ID)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

// GetEvent loads one event with its evidence references.
func (s *Store) GetEvent(deviceID, id string) (*core.Event, error) {
	var ev core.Event
	var occ, rec string
	err := s.db.QueryRow(
		`SELECT device_id, id, type, occurred_at, received_at
		 FROM events WHERE device_id = ? AND id = ?`, deviceID, id,
	).Scan(&ev.DeviceID, &ev.ID, &ev.Type, &occ, &rec)
	if err != nil {
		return nil, fmt.Errorf("load event: %w", err)
	}
	if ev.OccurredAt, err = parseTime(occ); err != nil {
		return nil, err
	}
	if ev.ReceivedAt, err = parseTime(rec); err != nil {
		return nil, err
	}
	refs, err := s.evidenceRefs(deviceID, id)
	if err != nil {
		return nil, err
	}
	ev.EvidenceIDs = refIDs(refs)
	return &ev, nil
}

// ListEvents returns the activity timeline ordered by occurred_at, then
// received_at, then id. An empty deviceID lists events of all devices.
func (s *Store) ListEvents(deviceID string) ([]EventRow, error) {
	rows, err := s.db.Query(
		`SELECT e.device_id, e.id, e.type, e.occurred_at, e.received_at, d.name
		 FROM events e JOIN devices d ON d.id = e.device_id
		 WHERE (? = '' OR e.device_id = ?)
		 ORDER BY e.occurred_at ASC, e.received_at ASC, e.id ASC`, deviceID, deviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	// Fully drain and close the result set before issuing further queries:
	// the pool has a single connection, so a nested query while rows are
	// open would deadlock.
	out := make([]EventRow, 0)
	for rows.Next() {
		var r EventRow
		var occ, rec string
		if err := rows.Scan(&r.DeviceID, &r.ID, &r.Type, &occ, &rec, &r.DeviceName); err != nil {
			rows.Close()
			return nil, err
		}
		if r.OccurredAt, err = parseTime(occ); err != nil {
			rows.Close()
			return nil, err
		}
		if r.ReceivedAt, err = parseTime(rec); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range out {
		refs, err := s.evidenceRefs(out[i].DeviceID, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Evidence = refs
	}
	return out, nil
}

func (s *Store) evidenceRefs(deviceID, eventID string) ([]EvidenceRef, error) {
	rows, err := s.db.Query(
		`SELECT ee.evidence_id, ev.kind
		 FROM event_evidence ee JOIN evidence ev ON ev.id = ee.evidence_id
		 WHERE ee.event_device_id = ? AND ee.event_id = ?
		 ORDER BY ee.position`, deviceID, eventID,
	)
	if err != nil {
		return nil, fmt.Errorf("list evidence refs: %w", err)
	}
	defer rows.Close()
	refs := []EvidenceRef{}
	for rows.Next() {
		var ref EvidenceRef
		if err := rows.Scan(&ref.ID, &ref.Kind); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func refIDs(refs []EvidenceRef) []string {
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	return ids
}

// Timestamps are stored as RFC 3339 UTC text, which sorts lexicographically.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("corrupt stored timestamp %q: %w", s, err)
	}
	return t, nil
}
