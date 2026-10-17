// Package api exposes the protecht HTTP API and a minimal HTML timeline.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"protecht/internal/core"
	"protecht/internal/store"
)

// Server is the HTTP front end. The now clock is injectable for tests.
type Server struct {
	st  *store.Store
	mux *http.ServeMux
	tpl *template.Template
	now func() time.Time
}

// NewServer builds a Server on top of st.
func NewServer(st *store.Store) *Server {
	s := &Server{
		st:  st,
		mux: http.NewServeMux(),
		tpl: template.Must(template.New("timeline").Funcs(template.FuncMap{
			"short": func(id string) string {
				if len(id) > 8 {
					return id[:8]
				}
				return id
			},
		}).Parse(timelineHTML)),
		now: time.Now,
	}
	s.mux.HandleFunc("POST /api/v1/devices", s.handleCreateDevice)
	s.mux.HandleFunc("POST /api/v1/evidence", s.requireAuth(s.handleCreateEvidence))
	s.mux.HandleFunc("GET /api/v1/evidence/{id}", s.handleGetEvidence)
	s.mux.HandleFunc("POST /api/v1/events", s.requireAuth(s.handleCreateEvent))
	s.mux.HandleFunc("GET /api/v1/events", s.handleListEvents)
	s.mux.HandleFunc("GET /", s.handleTimelinePage)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleCreateDevice registers a device: POST /api/v1/devices {"name": "..."}.
func (s *Server) handleCreateDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := core.ValidateDeviceName(req.Name); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	d, err := s.st.CreateDevice(req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// handleCreateEvidence accepts a multipart upload: POST /api/v1/evidence with
// form fields kind, sha256, captured_at and a file part named "file".
func (s *Server) handleCreateEvidence(w http.ResponseWriter, r *http.Request, d *core.Device) {
	// MaxEvidenceBytes for content plus slack for multipart framing.
	r.Body = http.MaxBytesReader(w, r.Body, core.MaxEvidenceBytes+(1<<20))
	if err := r.ParseMultipartForm(core.MaxEvidenceBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form or payload too large")
		return
	}
	kind := r.FormValue("kind")
	claimed := strings.ToLower(r.FormValue("sha256"))
	captured, err := time.Parse(time.RFC3339, r.FormValue("captured_at"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "captured_at must be an RFC 3339 timestamp")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, `multipart file part "file" is required`)
		return
	}
	defer f.Close()
	content, err := io.ReadAll(f)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read evidence content")
		return
	}

	actual := core.SHA256Hex(content)
	if actual != claimed {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("sha256 mismatch: claimed %s, content hashes to %s", claimed, actual))
		return
	}
	now := s.now()
	if err := core.ValidateEvidenceMeta(kind, claimed, int64(len(content)), captured, now); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	ev := &core.Evidence{
		ID:          core.NewUUID(),
		DeviceID:    d.ID,
		Kind:        kind,
		SHA256:      actual,
		ByteSize:    int64(len(content)),
		ContentType: http.DetectContentType(content),
		Content:     content,
		CapturedAt:  core.NormalizeTime(captured),
		ReceivedAt:  now.UTC(),
	}
	if err := s.st.CreateEvidence(ev); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

// handleGetEvidence serves evidence content: GET /api/v1/evidence/{id}. The
// stored (sniffed) content type is returned so browsers can render images;
// the SHA-256 is echoed in a header so viewers can verify integrity.
func (s *Server) handleGetEvidence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !core.IsUUID(id) {
		writeError(w, http.StatusNotFound, "evidence not found")
		return
	}
	ev, err := s.st.EvidenceByID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if ev == nil {
		writeError(w, http.StatusNotFound, "evidence not found")
		return
	}
	w.Header().Set("Content-Type", ev.ContentType)
	w.Header().Set("X-SHA256", ev.SHA256)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(ev.Content)
}

// handleCreateEvent submits an event: POST /api/v1/events with a JSON body of
// {"id", "type", "occurred_at", "evidence_ids"}. The device is taken from the
// bearer token, never from the body.
func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request, d *core.Device) {
	var ev core.Event
	if !decodeJSON(w, r, &ev) {
		return
	}
	now := s.now()
	if err := core.ValidateEvent(&ev, now); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	ev.DeviceID = d.ID
	ev.OccurredAt = core.NormalizeTime(ev.OccurredAt)
	ev.ReceivedAt = now.UTC()

	stored, created, err := s.st.SaveEvent(&ev)
	if err != nil {
		var ae *store.AssociationError
		if errors.As(err, &ae) {
			writeError(w, http.StatusUnprocessableEntity, ae.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !created {
		// Idempotent replay of an already-submitted event.
		writeJSON(w, http.StatusOK, stored)
		return
	}
	writeJSON(w, http.StatusCreated, stored)
}

// handleListEvents returns the timeline JSON: GET /api/v1/events?device_id=...
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListEvents(r.URL.Query().Get("device_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": rows})
}

// handleTimelinePage renders the HTML timeline at GET /.
func (s *Server) handleTimelinePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	rows, err := s.st.ListEvents("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.Execute(w, rows); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// requireAuth authenticates the device bearer token for write endpoints.
func (s *Server) requireAuth(next func(http.ResponseWriter, *http.Request, *core.Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="protecht"`)
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		d, err := s.st.DeviceByToken(strings.TrimSpace(strings.TrimPrefix(h, prefix)))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if d == nil {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r, d)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

const timelineHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>protecht &mdash; activity timeline</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 56em; margin: 2rem auto; padding: 0 1rem; color: #222; }
  table { border-collapse: collapse; width: 100%; }
  th, td { border: 1px solid #ccc; padding: .4rem .6rem; text-align: left; font-size: .9rem; vertical-align: top; }
  th { background: #f0f0f0; }
  code { font-size: .78rem; color: #555; }
</style>
</head>
<body>
<h1>Activity timeline</h1>
{{if .}}
<table>
  <tr><th>Occurred (UTC)</th><th>Device</th><th>Event</th><th>Evidence</th><th>Received (UTC)</th></tr>
  {{range .}}
  <tr>
    <td>{{.OccurredAt.Format "2006-01-02 15:04:05"}}</td>
    <td>{{.DeviceName}}</td>
    <td>{{.Type}}<br><code>{{.ID}}</code></td>
    <td>{{if .Evidence}}{{range .Evidence}}<a href="/api/v1/evidence/{{.ID}}">{{.Kind}}</a> <code>{{short .ID}}</code><br>{{end}}{{else}}&mdash;{{end}}</td>
    <td>{{.ReceivedAt.Format "2006-01-02 15:04:05"}}</td>
  </tr>
  {{end}}
</table>
{{else}}
<p>No events yet.</p>
{{end}}
</body>
</html>
`
