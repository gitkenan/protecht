package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"protecht/internal/core"
	"protecht/internal/store"
)

var testNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// testEnv is a running server with a fixed clock backed by a temp database.
type testEnv struct {
	t   *testing.T
	srv *httptest.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(st)
	s.now = func() time.Time { return testNow }
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &testEnv{t: t, srv: srv}
}

func (e *testEnv) register(t *testing.T, name string) *core.Device {
	t.Helper()
	resp, err := http.Post(e.srv.URL+"/api/v1/devices", "application/json",
		strings.NewReader(fmt.Sprintf(`{"name":%q}`, name)))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	var d core.Device
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatalf("register: decode: %v", err)
	}
	if d.Token == "" {
		t.Fatal("register: no token returned")
	}
	return &d
}

func (e *testEnv) uploadEvidence(t *testing.T, token, kind string, content []byte, shaOverride string) (*core.Evidence, int) {
	t.Helper()
	sha := core.SHA256Hex(content)
	if shaOverride != "" {
		sha = shaOverride
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("kind", kind)
	_ = mw.WriteField("sha256", sha)
	_ = mw.WriteField("captured_at", testNow.Add(-2*time.Minute).Format(time.RFC3339))
	fw, _ := mw.CreateFormFile("file", kind+".bin")
	_, _ = fw.Write(content)
	_ = mw.Close()

	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/evidence", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload evidence: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, resp.StatusCode
	}
	var ev core.Evidence
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		t.Fatalf("upload evidence: decode: %v", err)
	}
	return &ev, resp.StatusCode
}

func (e *testEnv) submitEvent(t *testing.T, token string, ev *core.Event) (*core.Event, int) {
	t.Helper()
	body, _ := json.Marshal(ev)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("submit event: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	var saved core.Event
	if err := json.NewDecoder(resp.Body).Decode(&saved); err != nil {
		t.Fatalf("submit event: decode: %v", err)
	}
	return &saved, resp.StatusCode
}

func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func TestEndToEndFlow(t *testing.T) {
	e := newTestEnv(t)
	device := e.register(t, "sim-device")
	content := []byte("simulated screenshot bytes")

	t.Run("evidence upload requires a valid hash", func(t *testing.T) {
		_, status := e.uploadEvidence(t, device.Token, "screenshot", content, strings.Repeat("0", 64))
		if status != http.StatusBadRequest {
			t.Fatalf("expected 400 on hash mismatch, got %d", status)
		}
	})

	ev1, status := e.uploadEvidence(t, device.Token, "screenshot", content, "")
	if status != http.StatusCreated {
		t.Fatalf("upload evidence: status %d", status)
	}
	if ev1.SHA256 != core.SHA256Hex(content) {
		t.Fatalf("server stored unexpected hash: %s", ev1.SHA256)
	}

	event := &core.Event{
		ID:          core.NewUUID(),
		Type:        "check_in",
		OccurredAt:  testNow.Add(-time.Minute),
		EvidenceIDs: []string{ev1.ID},
	}

	saved, status := e.submitEvent(t, device.Token, event)
	if status != http.StatusCreated {
		t.Fatalf("submit event: status %d", status)
	}
	if saved.DeviceID != device.ID || saved.ReceivedAt.IsZero() {
		t.Fatalf("server fields not populated: %+v", saved)
	}

	t.Run("duplicate submission replays idempotently", func(t *testing.T) {
		replay, status := e.submitEvent(t, device.Token, event)
		if status != http.StatusOK {
			t.Fatalf("expected 200 on duplicate, got %d", status)
		}
		if replay.ID != saved.ID || !replay.ReceivedAt.Equal(saved.ReceivedAt) {
			t.Fatalf("duplicate did not return the original record: %+v", replay)
		}
	})

	t.Run("timeline JSON contains the event with its evidence", func(t *testing.T) {
		resp, err := http.Get(e.srv.URL + "/api/v1/events")
		if err != nil {
			t.Fatalf("timeline: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Events []store.EventRow `json:"events"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("timeline: decode: %v", err)
		}
		if len(out.Events) != 1 {
			t.Fatalf("expected exactly 1 event after duplicate submission, got %d", len(out.Events))
		}
		got := out.Events[0]
		if got.ID != event.ID || got.DeviceName != "sim-device" {
			t.Fatalf("unexpected timeline row: %+v", got)
		}
		if len(got.Evidence) != 1 || got.Evidence[0].ID != ev1.ID || got.Evidence[0].Kind != "screenshot" {
			t.Fatalf("evidence association missing from timeline: %+v", got.Evidence)
		}
	})

	t.Run("evidence content is retrievable and verified", func(t *testing.T) {
		resp, err := http.Get(e.srv.URL + "/api/v1/evidence/" + ev1.ID)
		if err != nil {
			t.Fatalf("get evidence: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get evidence: status %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct == "" {
			t.Fatal("get evidence: no Content-Type header")
		}
		if got := resp.Header.Get("X-SHA256"); got != ev1.SHA256 {
			t.Fatalf("get evidence: X-SHA256 %q, want %q", got, ev1.SHA256)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("get evidence: read: %v", err)
		}
		if !bytes.Equal(body, content) {
			t.Fatal("get evidence: content bytes differ from upload")
		}
		if core.SHA256Hex(body) != ev1.SHA256 {
			t.Fatal("get evidence: downloaded content fails hash verification")
		}
	})

	t.Run("unknown evidence id is a 404", func(t *testing.T) {
		resp, err := http.Get(e.srv.URL + "/api/v1/evidence/" + core.NewUUID())
		if err != nil {
			t.Fatalf("get evidence: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("HTML timeline renders the event", func(t *testing.T) {
		resp, err := http.Get(e.srv.URL + "/")
		if err != nil {
			t.Fatalf("html timeline: %v", err)
		}
		defer resp.Body.Close()
		page := bodyString(t, resp)
		if !strings.Contains(page, "check_in") || !strings.Contains(page, "sim-device") || !strings.Contains(page, event.ID) {
			t.Fatalf("HTML timeline does not show the event:\n%s", page)
		}
		if !strings.Contains(page, `/api/v1/evidence/`+ev1.ID) {
			t.Fatalf("HTML timeline does not link to the evidence:\n%s", page)
		}
	})
}

func TestEventValidationOverHTTP(t *testing.T) {
	e := newTestEnv(t)
	device := e.register(t, "alice")
	other := e.register(t, "bob")
	otherEv, _ := e.uploadEvidence(t, other.Token, "photo", []byte("bob's photo"), "")

	base := &core.Event{ID: core.NewUUID(), Type: "check_in", OccurredAt: testNow.Add(-time.Minute)}

	tests := []struct {
		name   string
		mutate func(*core.Event)
		want   int
	}{
		{name: "missing id", mutate: func(ev *core.Event) { ev.ID = "" }, want: http.StatusUnprocessableEntity},
		{name: "missing type", mutate: func(ev *core.Event) { ev.Type = "" }, want: http.StatusUnprocessableEntity},
		{
			name:   "occurred_at too far in the future",
			mutate: func(ev *core.Event) { ev.OccurredAt = testNow.Add(core.FutureSkewTolerance + time.Minute) },
			want:   http.StatusUnprocessableEntity,
		},
		{
			name:   "evidence of another device",
			mutate: func(ev *core.Event) { ev.EvidenceIDs = []string{otherEv.ID} },
			want:   http.StatusUnprocessableEntity,
		},
		{
			name:   "nonexistent evidence",
			mutate: func(ev *core.Event) { ev.EvidenceIDs = []string{core.NewUUID()} },
			want:   http.StatusUnprocessableEntity,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := *base
			ev.ID = core.NewUUID()
			tt.mutate(&ev)
			_, status := e.submitEvent(t, device.Token, &ev)
			if status != tt.want {
				t.Fatalf("expected %d, got %d", tt.want, status)
			}
		})
	}
}

func TestAuth(t *testing.T) {
	e := newTestEnv(t)
	device := e.register(t, "alice")
	event := &core.Event{ID: core.NewUUID(), Type: "check_in", OccurredAt: testNow.Add(-time.Minute)}

	if _, status := e.submitEvent(t, "", event); status != http.StatusUnauthorized {
		t.Fatalf("missing token: expected 401, got %d", status)
	}
	if _, status := e.submitEvent(t, "deadbeef", event); status != http.StatusUnauthorized {
		t.Fatalf("bad token: expected 401, got %d", status)
	}
	if _, status := e.uploadEvidence(t, "", "photo", []byte("x"), ""); status != http.StatusUnauthorized {
		t.Fatalf("evidence without token: expected 401, got %d", status)
	}
	// A token never lets a device claim another device's identity.
	saved, status := e.submitEvent(t, device.Token, event)
	if status != http.StatusCreated || saved.DeviceID != device.ID {
		t.Fatalf("device identity must come from the token: status=%d saved=%+v", status, saved)
	}
}
