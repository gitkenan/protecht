package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"protecht/internal/api"
	"protecht/internal/core"
	"protecht/internal/store"
)

func TestPublishFollowsContract(t *testing.T) {
	var evidencePosts atomic.Int32
	var eventPosts atomic.Int32
	var gotAuth string
	var gotKind, gotSHA, gotCaptured, gotFile string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/evidence":
			evidencePosts.Add(1)
			gotAuth = r.Header.Get("Authorization")
			media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || !strings.HasPrefix(media, "multipart/") {
				t.Errorf("content-type %q", r.Header.Get("Content-Type"))
			}
			form, err := multipart.NewReader(r.Body, params["boundary"]).ReadForm(1 << 20)
			if err != nil {
				t.Errorf("multipart: %v", err)
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			gotKind = form.Value["kind"][0]
			gotSHA = form.Value["sha256"][0]
			gotCaptured = form.Value["captured_at"][0]
			f, err := form.File["file"][0].Open()
			if err != nil {
				t.Errorf("file: %v", err)
			}
			b, _ := io.ReadAll(f)
			f.Close()
			gotFile = string(b)
			if core.SHA256Hex(b) != gotSHA {
				t.Errorf("sha256 %s does not match file", gotSHA)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(core.Evidence{
				ID:     "11111111-1111-4111-8111-111111111111",
				Kind:   gotKind,
				SHA256: gotSHA,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/events":
			eventPosts.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("ab", 32) {
				t.Errorf("event auth %q", r.Header.Get("Authorization"))
			}
			var ev core.Event
			if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
				t.Errorf("event body: %v", err)
			}
			if ev.Type != eventType {
				t.Errorf("type = %q", ev.Type)
			}
			if len(ev.EvidenceIDs) != 1 || ev.EvidenceIDs[0] != "11111111-1111-4111-8111-111111111111" {
				t.Errorf("evidence_ids = %v", ev.EvidenceIDs)
			}
			if ev.ID != "22222222-2222-4222-8222-222222222222" {
				t.Errorf("event id = %q", ev.ID)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(ev)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL, strings.Repeat("ab", 32))
	obs := Observation{
		Title:      "Notes",
		Process:    "notepad.exe",
		Executable: `C:\Windows\System32\notepad.exe`,
		CapturedAt: time.Date(2026, 10, 17, 12, 0, 3, 500*int(time.Millisecond), time.UTC),
	}
	eventID := "22222222-2222-4222-8222-222222222222"
	evID, err := c.publish(context.Background(), obs, eventID, "")
	if err != nil {
		t.Fatal(err)
	}
	if evID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("evidence id = %s", evID)
	}
	if gotAuth != "Bearer "+c.token {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotKind != evidenceKind {
		t.Fatalf("kind = %q", gotKind)
	}
	wantJSON, err := obs.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if gotFile != string(wantJSON) {
		t.Fatalf("file = %s, want %s", gotFile, wantJSON)
	}
	if gotSHA != core.SHA256Hex(wantJSON) {
		t.Fatalf("sha = %s", gotSHA)
	}
	if gotCaptured != "2026-10-17T12:00:03Z" {
		t.Fatalf("captured_at = %q", gotCaptured)
	}

	// A retry that already has an evidence id must not upload again.
	if _, err := c.publish(context.Background(), obs, eventID, evID); err != nil {
		t.Fatal(err)
	}
	if evidencePosts.Load() != 1 {
		t.Fatalf("evidence uploads = %d, want 1", evidencePosts.Load())
	}
	if eventPosts.Load() != 2 {
		t.Fatalf("event submits = %d, want 2", eventPosts.Load())
	}
}

func TestPublishKeepsEvidenceIDWhenEventFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/evidence" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(core.Evidence{
				ID:     "11111111-1111-4111-8111-111111111111",
				SHA256: core.SHA256Hex(mustReadFile(t, r)),
			})
			return
		}
		http.Error(w, `{"error":"nope"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL, testToken())
	obs := Observation{Title: "A", CapturedAt: time.Now().UTC()}
	evID, err := c.publish(context.Background(), obs, core.NewUUID(), "")
	if err == nil {
		t.Fatal("expected event error")
	}
	if evID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("evidence id = %q, want the uploaded id alongside the error", evID)
	}
}

func mustReadFile(t *testing.T, r *http.Request) []byte {
	t.Helper()
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(media, "multipart/") {
		t.Fatalf("content-type %q: %v", r.Header.Get("Content-Type"), err)
	}
	form, err := multipart.NewReader(r.Body, params["boundary"]).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	f, err := form.File["file"][0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAgentFlowAgainstServer(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(api.NewServer(st))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	c := newClient(srv.URL, "")
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	cfg, reused, err := ensureDevice(cfgPath, "desk", func(name string) (*core.Device, error) {
		return c.register(ctx, name)
	})
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("expected a fresh registration")
	}
	c.token = cfg.Token

	again, reused, err := ensureDevice(cfgPath, "other", func(string) (*core.Device, error) {
		t.Fatal("second run registered again")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reused || again.Token != cfg.Token || again.ID != cfg.ID {
		t.Fatalf("config was not reused: %+v", again)
	}

	var sess session
	var printed bytes.Buffer
	t0 := time.Now().UTC().Add(-time.Second)
	first := Observation{
		Title:      "README.md - protecht",
		Process:    "Code.exe",
		Executable: `C:\Program Files\Code.exe`,
		CapturedAt: t0,
	}
	sess.observe(first, &printed, func(obs Observation, eventID, evidenceID string) (string, error) {
		return c.publish(ctx, obs, eventID, evidenceID)
	})
	// Same window, later poll: no second event.
	same := first
	same.CapturedAt = time.Now().UTC()
	sess.observe(same, &printed, func(obs Observation, eventID, evidenceID string) (string, error) {
		return c.publish(ctx, obs, eventID, evidenceID)
	})
	sess.observe(Observation{
		Title:      "Inbox - Mail",
		Process:    "olk.exe",
		Executable: `C:\Program Files\olk.exe`,
		CapturedAt: time.Now().UTC(),
	}, &printed, func(obs Observation, eventID, evidenceID string) (string, error) {
		return c.publish(ctx, obs, eventID, evidenceID)
	})

	resp, err := http.Get(srv.URL + "/api/v1/events?device_id=" + cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("timeline HTTP %d: %s", resp.StatusCode, b)
	}
	var timeline struct {
		Events []struct {
			ID         string `json:"id"`
			DeviceName string `json:"device_name"`
			Type       string `json:"type"`
			Evidence   []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"evidence"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&timeline); err != nil {
		t.Fatal(err)
	}
	if len(timeline.Events) != 2 {
		t.Fatalf("events = %d, want 2 (unchanged window must not submit)", len(timeline.Events))
	}
	for _, ev := range timeline.Events {
		if ev.Type != "application.foreground" || ev.DeviceName != "desk" {
			t.Fatalf("event = %+v", ev)
		}
		if len(ev.Evidence) != 1 || ev.Evidence[0].Kind != evidenceKind {
			t.Fatalf("evidence = %+v", ev.Evidence)
		}
	}

	evResp, err := http.Get(srv.URL + "/api/v1/evidence/" + timeline.Events[0].Evidence[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer evResp.Body.Close()
	body, err := io.ReadAll(evResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if evResp.StatusCode != http.StatusOK {
		t.Fatalf("evidence HTTP %d: %s", evResp.StatusCode, body)
	}
	if evResp.Header.Get("X-SHA256") != core.SHA256Hex(body) {
		t.Fatalf("X-SHA256 = %s, content hashes to %s", evResp.Header.Get("X-SHA256"), core.SHA256Hex(body))
	}
	if !bytes.Contains(body, []byte(`"title":`)) || !bytes.Contains(body, []byte(`"captured_at":`)) {
		t.Fatalf("evidence is not an observation document: %s", body)
	}
	if strings.Count(printed.String(), "\n") != 2 {
		t.Fatalf("printed %q, want two observations", printed.String())
	}
}
