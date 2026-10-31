package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
)

// The JSON API serves the CLI while `laker serve` holds the local stores.

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error string `json:"error"`
}

func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	res, err := s.Index.Search(r.Context(), domain.Query{
		Text: q.Get("q"), Kind: q.Get("kind"), Limit: limit, Offset: offset, Highlight: q.Get("highlight"),
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiDoc(w http.ResponseWriter, r *http.Request) {
	doc, err := s.Index.Document(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{err.Error()})
		return
	}
	if doc == nil {
		writeJSON(w, http.StatusNotFound, apiError{"not indexed"})
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) apiTree(w http.ResponseWriter, r *http.Request) {
	var at time.Time
	if a := r.URL.Query().Get("at"); a != "" {
		t, err := time.Parse(time.RFC3339Nano, a)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{err.Error()})
			return
		}
		at = t
	}
	tree, err := core.Tree(r.Context(), s.Lake, s.State, r.PathValue("source"), at)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, tree)
}

// AdapterStatus is one row of GET /api/status.
type AdapterStatus struct {
	Name     string      `json:"name"`
	Type     string      `json:"type"`
	Enabled  bool        `json:"enabled"`
	Schedule string      `json:"schedule"`
	Running  bool        `json:"running"`
	Next     time.Time   `json:"next"`
	Last     *domain.Run `json:"last,omitempty"`
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	var out []AdapterStatus
	for _, a := range s.Adapters {
		st := AdapterStatus{Name: a.Name, Type: a.Type, Enabled: a.Enabled, Schedule: a.Schedule,
			Running: s.Runs.Running(a.Name), Next: s.Runs.Next(a.Name)}
		st.Last, _ = core.LastRun(s.State, a.Name)
		out = append(out, st)
	}
	for _, v := range s.adapterViews() {
		if v.Agent == "" {
			continue
		}
		st := AdapterStatus{Name: v.Name, Type: v.Type, Enabled: true, Schedule: "agent " + v.Agent}
		st.Last, _ = core.LastRun(s.State, v.Name)
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, out)
}

// apiRun starts a job, as the "Run now" button does: 202 started,
// 409 already running, 404 unknown.
func (s *Server) apiRun(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch err := s.Runs.Trigger(name); {
	case err == nil:
		s.Logger.Info().Str("job", name).Msg("INF402: run started from the API")
		writeJSON(w, http.StatusAccepted, map[string]string{"started": name})
	case errors.Is(err, core.ErrRunning):
		writeJSON(w, http.StatusConflict, apiError{err.Error()})
	default:
		writeJSON(w, http.StatusNotFound, apiError{err.Error()})
	}
}
