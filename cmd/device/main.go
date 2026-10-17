// Command device is a simulated device client. It exercises the full
// protocol against a running server: register, upload evidence, submit an
// event referencing that evidence, resubmit it (demonstrating idempotent
// duplicate handling), and print the activity timeline.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"time"

	"protecht/internal/core"
	"protecht/internal/store"
)

type client struct {
	base  string
	token string
	hc    *http.Client
}

func main() {
	base := flag.String("server", "http://127.0.0.1:8080", "server base URL")
	name := flag.String("name", "sim-device", "device name to register as")
	flag.Parse()

	c := &client{base: *base, hc: &http.Client{Timeout: 10 * time.Second}}
	if err := run(c, *name); err != nil {
		log.Fatalf("device simulation failed: %v", err)
	}
}

func run(c *client, name string) error {
	// 1. Register the device.
	dev, err := c.register(name)
	if err != nil {
		return err
	}
	c.token = dev.Token
	fmt.Printf("1. registered device %q (id %s)\n", dev.Name, dev.ID)

	// 2. Upload two pieces of simulated evidence.
	shot := make([]byte, 128*1024)
	if _, err := rand.Read(shot); err != nil {
		return err
	}
	ev1, err := c.uploadEvidence("screenshot", shot, time.Now().Add(-2*time.Minute))
	if err != nil {
		return err
	}
	fmt.Printf("2. uploaded evidence %s (kind=%s, %d bytes, sha256 %.12s...)\n",
		ev1.ID, ev1.Kind, ev1.ByteSize, ev1.SHA256)

	note := []byte("arrived at the gym, badge swiped at entrance")
	ev2, err := c.uploadEvidence("note", note, time.Now().Add(-90*time.Second))
	if err != nil {
		return err
	}
	fmt.Printf("   uploaded evidence %s (kind=%s, %d bytes, sha256 %.12s...)\n",
		ev2.ID, ev2.Kind, ev2.ByteSize, ev2.SHA256)

	// 3. Submit an event referencing both evidence items.
	event := core.Event{
		ID:          core.NewUUID(),
		Type:        "check_in",
		OccurredAt:  time.Now().Add(-1 * time.Minute).UTC(),
		EvidenceIDs: []string{ev1.ID, ev2.ID},
	}
	status, saved, err := c.submitEvent(&event)
	if err != nil {
		return err
	}
	fmt.Printf("3. submitted event %s (type=%s) -> HTTP %d, received_at=%s\n",
		saved.ID, saved.Type, status, saved.ReceivedAt.Format(time.RFC3339))

	// 4. Resubmit the exact same event (e.g. a client retry): the server must
	//    replay the original record without creating a duplicate.
	status, replay, err := c.submitEvent(&event)
	if err != nil {
		return err
	}
	if status != http.StatusOK || replay.ID != saved.ID || !replay.ReceivedAt.Equal(saved.ReceivedAt) {
		return fmt.Errorf("duplicate submission was not idempotent: status=%d replay=%+v", status, replay)
	}
	fmt.Printf("4. resubmitted same event -> HTTP %d (idempotent replay, original record returned)\n", status)

	// 5. Fetch and print the activity timeline.
	rows, err := c.timeline()
	if err != nil {
		return err
	}
	fmt.Printf("5. activity timeline (%d event(s)):\n", len(rows))
	for _, r := range rows {
		fmt.Printf("   - %s  %-10s %-10s evidence=%d  (id %s)\n",
			r.OccurredAt.Format("2006-01-02 15:04:05"), r.DeviceName, r.Type,
			len(r.Evidence), r.ID)
	}

	// 6. Retrieve one evidence item back and verify its integrity.
	downloaded, err := c.downloadEvidence(ev1.ID)
	if err != nil {
		return err
	}
	if got := core.SHA256Hex(downloaded); got != ev1.SHA256 {
		return fmt.Errorf("downloaded evidence hash mismatch: got %s, want %s", got, ev1.SHA256)
	}
	fmt.Printf("6. retrieved evidence %s (%d bytes) and verified its sha256\n", ev1.ID, len(downloaded))
	return nil
}

func (c *client) register(name string) (*core.Device, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	resp, err := c.hc.Post(c.base+"/api/v1/devices", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("register: HTTP %d: %s", resp.StatusCode, readErr(resp.Body))
	}
	var d core.Device
	return &d, json.NewDecoder(resp.Body).Decode(&d)
}

func (c *client) uploadEvidence(kind string, content []byte, capturedAt time.Time) (*core.Evidence, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("kind", kind)
	_ = mw.WriteField("sha256", core.SHA256Hex(content))
	_ = mw.WriteField("captured_at", capturedAt.UTC().Format(time.RFC3339))
	fw, err := mw.CreateFormFile("file", kind+".bin")
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(content); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.base+"/api/v1/evidence", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload evidence: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("upload evidence: HTTP %d: %s", resp.StatusCode, readErr(resp.Body))
	}
	var ev core.Evidence
	return &ev, json.NewDecoder(resp.Body).Decode(&ev)
}

func (c *client) submitEvent(ev *core.Event) (int, *core.Event, error) {
	body, _ := json.Marshal(ev)
	req, err := http.NewRequest(http.MethodPost, c.base+"/api/v1/events", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("submit event: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil, fmt.Errorf("submit event: HTTP %d: %s", resp.StatusCode, readErr(resp.Body))
	}
	var saved core.Event
	return resp.StatusCode, &saved, json.NewDecoder(resp.Body).Decode(&saved)
}

func (c *client) downloadEvidence(id string) ([]byte, error) {
	resp, err := c.hc.Get(c.base + "/api/v1/evidence/" + id)
	if err != nil {
		return nil, fmt.Errorf("download evidence: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download evidence: HTTP %d: %s", resp.StatusCode, readErr(resp.Body))
	}
	return io.ReadAll(resp.Body)
}

func (c *client) timeline() ([]store.EventRow, error) {
	resp, err := c.hc.Get(c.base + "/api/v1/events")
	if err != nil {
		return nil, fmt.Errorf("timeline: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("timeline: HTTP %d: %s", resp.StatusCode, readErr(resp.Body))
	}
	var out struct {
		Events []store.EventRow `json:"events"`
	}
	return out.Events, json.NewDecoder(resp.Body).Decode(&out)
}

func readErr(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return string(bytes.TrimSpace(b))
}
