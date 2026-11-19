package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"protecht/internal/core"
)

// client speaks the protecht HTTP contract. Device identity comes from the
// bearer token, never from a request body.
type client struct {
	base  string
	token string
	hc    *http.Client
}

func newClient(base, token string) *client {
	return &client{
		base:  base,
		token: token,
		hc:    &http.Client{Timeout: 15 * time.Second},
	}
}

func normalizeBase(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", fmt.Errorf("server URL must be an absolute http(s) URL")
	}
	return raw, nil
}

func (c *client) register(ctx context.Context, name string) (*core.Device, error) {
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/devices", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("register: HTTP %d: %s", resp.StatusCode, readErr(resp.Body))
	}
	var dev core.Device
	if err := json.NewDecoder(resp.Body).Decode(&dev); err != nil {
		return nil, fmt.Errorf("register: decode: %w", err)
	}
	if !core.IsUUID(dev.ID) || !validToken(dev.Token) {
		return nil, fmt.Errorf("register: server returned an incomplete device")
	}
	return &dev, nil
}

// publish uploads the observation as JSON evidence, then submits an
// application.foreground event referencing it. evidenceID, when set, is a
// previous upload of this same observation: the event is submitted without
// uploading again. On an event failure after a successful upload, the
// evidence id is returned with the error so the caller can retry the event
// without creating a second artifact.
func (c *client) publish(ctx context.Context, obs Observation, eventID, evidenceID string) (string, error) {
	if !core.IsUUID(eventID) {
		return evidenceID, fmt.Errorf("event id must be a UUID")
	}
	if evidenceID != "" && !core.IsUUID(evidenceID) {
		return "", fmt.Errorf("evidence id must be a UUID")
	}
	body, err := obs.JSON()
	if err != nil {
		return evidenceID, err
	}
	captured := obs.capturedAt()
	if evidenceID == "" {
		ev, err := c.uploadEvidence(ctx, evidenceKind, body, captured)
		if err != nil {
			return "", err
		}
		evidenceID = ev.ID
	}
	_, err = c.submitEvent(ctx, &core.Event{
		ID:          eventID,
		Type:        eventType,
		OccurredAt:  captured,
		EvidenceIDs: []string{evidenceID},
	})
	if err != nil {
		return evidenceID, err
	}
	return evidenceID, nil
}

func (c *client) uploadEvidence(ctx context.Context, kind string, content []byte, capturedAt time.Time) (*core.Evidence, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := []struct{ k, v string }{
		{"kind", kind},
		{"sha256", core.SHA256Hex(content)},
		{"captured_at", capturedAt.UTC().Format(time.RFC3339)},
	}
	for _, f := range fields {
		if err := mw.WriteField(f.k, f.v); err != nil {
			return nil, err
		}
	}
	fw, err := mw.CreateFormFile("file", "foreground.json")
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(content); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/evidence", &buf)
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
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		return nil, fmt.Errorf("upload evidence: decode: %w", err)
	}
	if !core.IsUUID(ev.ID) {
		return nil, fmt.Errorf("upload evidence: response id is not a UUID")
	}
	if ev.SHA256 != core.SHA256Hex(content) {
		return nil, fmt.Errorf("upload evidence: server hash %s does not match content", ev.SHA256)
	}
	return &ev, nil
}

func (c *client) submitEvent(ctx context.Context, ev *core.Event) (int, error) {
	body, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/events", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("submit event: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("submit event: HTTP %d: %s", resp.StatusCode, readErr(resp.Body))
	}
	var saved core.Event
	if err := json.NewDecoder(resp.Body).Decode(&saved); err != nil {
		return resp.StatusCode, fmt.Errorf("submit event: decode: %w", err)
	}
	if saved.ID != ev.ID {
		return resp.StatusCode, fmt.Errorf("submit event: response id %s, want %s", saved.ID, ev.ID)
	}
	return resp.StatusCode, nil
}

func readErr(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return string(bytes.TrimSpace(b))
}
