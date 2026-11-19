package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"protecht/internal/core"
)

// publisher uploads an observation and returns the evidence id. A non-nil
// error may still carry an evidence id when the upload succeeded and the
// event submit did not.
type publisher func(obs Observation, eventID, evidenceID string) (string, error)

// session remembers the last window successfully reported and one in-flight
// change. Retries keep the original event id, capture time, and evidence id
// so a lost response does not create a second event or a second artifact.
type session struct {
	lastSent *windowID
	pending  *inflight
	logf     func(format string, args ...any)
}

type inflight struct {
	window     windowID
	capturedAt time.Time
	eventID    string
	evidenceID string
}

func (s *session) errorf(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// observe prints and publishes obs when it is a new foreground window.
// An unchanged window is ignored. A failed publish is retried on the next
// call with the same identity, without printing again.
func (s *session) observe(obs Observation, w io.Writer, pub publisher) {
	id := obs.window()
	if s.lastSent != nil && *s.lastSent == id {
		return
	}
	if s.pending == nil || s.pending.window != id {
		if s.pending != nil && s.pending.evidenceID != "" {
			s.errorf("dropping unpublished observation %q (evidence %s) for a newer window", s.pending.window.title, s.pending.evidenceID)
		}
		if _, err := fmt.Fprintln(w, formatObservation(obs)); err != nil {
			s.errorf("print observation: %v", err)
		}
		s.pending = &inflight{
			window:     id,
			capturedAt: obs.capturedAt(),
			eventID:    core.NewUUID(),
		}
	}
	toSend := Observation{
		Title:      id.title,
		Process:    id.process,
		Executable: id.executable,
		CapturedAt: s.pending.capturedAt,
	}
	evID, err := pub(toSend, s.pending.eventID, s.pending.evidenceID)
	if evID != "" {
		s.pending.evidenceID = evID
	}
	if err != nil {
		s.errorf("publish observation: %v", err)
		return
	}
	sent := id
	s.lastSent = &sent
	s.pending = nil
}

// loop polls until ctx is cancelled. The first capture runs immediately.
// Capture errors are logged once per distinct message and do not stop the
// loop; the next tick tries again.
func loop(ctx context.Context, interval time.Duration, now func() time.Time, capture func(time.Time) (Observation, error), pub publisher, out io.Writer) error {
	if now == nil {
		now = time.Now
	}
	var sess session
	var lastErr string
	poll := func() {
		obs, err := capture(now())
		if err != nil {
			msg := err.Error()
			if msg != lastErr {
				log.Printf("capture: %v", err)
				lastErr = msg
			}
			return
		}
		lastErr = ""
		sess.observe(obs, out, pub)
	}
	poll()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			poll()
		}
	}
}
