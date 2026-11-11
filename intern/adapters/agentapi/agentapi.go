// Package agentapi is the side of laker serve that agents send their data
// to (/agent/v1/*). Every request is signed with the agent's key; only
// approved agents may write, only into their own sources, and every blob is
// hashed again before it enters the lake.
package agentapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/agentkey"
)

// MaxCommit caps the body of a commit: its parquet files and documents.
// Agents cut their commits into much smaller pieces (agentclient.MaxPart);
// the cap is this high for the agents that do not yet.
const MaxCommit = 1 << 30

// commitSlots is how many POST bodies are held in memory at once: the others
// wait, so several agents committing together do not add up.
const commitSlots = 2

// Hello is what an agent sends to present itself.
type Hello struct {
	PublicKey string `json:"public_key"`
	Name      string `json:"name"`
	Hostname  string `json:"hostname"`
	Version   string `json:"version"`
}

// HelloReply tells the agent who it is for the server and if it may write.
type HelloReply struct {
	ID     string             `json:"id"`
	Status domain.AgentStatus `json:"status"`
}

type Deps struct {
	Ingest *core.LocalIngest
	State  out.StateStore
	TmpDir string
	Logger zerolog.Logger
	Now    func() time.Time
}

type API struct {
	Deps
	mu    sync.Mutex    // serializes the registry writes (hello, claim, touch)
	slots chan struct{} // see commitSlots
}

func New(d Deps) *API {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &API{Deps: d, slots: make(chan struct{}, commitSlots)}
}

// Handler serves /agent/v1/.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/v1/hello", a.hello)
	mux.HandleFunc("HEAD /agent/v1/blobs/{sha}", a.approved(a.blobExists))
	mux.HandleFunc("PUT /agent/v1/blobs/{sha}", a.approved(a.putBlob))
	mux.HandleFunc("POST /agent/v1/commit", a.approved(a.commit))
	mux.HandleFunc("POST /agent/v1/finish", a.approved(a.finish))
	return mux
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error string `json:"error"`
}

func (a *API) refuse(w http.ResponseWriter, r *http.Request, status int, err error) {
	a.Logger.Warn().Str("agent", r.Header.Get(agentkey.HeaderAgent)).Str("path", r.URL.Path).Int("status", status).
		Msgf("WRN600: agent request refused: %v", err)
	reply(w, status, apiError{err.Error()})
}

// verify checks the signature of r against pub, with body hashing to the
// value of the content header.
func (a *API) verify(r *http.Request, pub []byte, bodySHA string) error {
	if r.Header.Get(agentkey.HeaderContent) != bodySHA {
		return errors.New("ERR600: the body does not match its declared hash")
	}
	ts, err := strconv.ParseInt(r.Header.Get(agentkey.HeaderTime), 10, 64)
	if err != nil {
		return errors.New("ERR601: missing request time")
	}
	return agentkey.Verify(pub, r.Header.Get(agentkey.HeaderSignature), r.Method, r.URL.Path, ts, bodySHA, a.Now())
}

func readBody(r *http.Request, limit int64) ([]byte, string, error) {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, limit))
	if err != nil {
		return nil, "", fmt.Errorf("ERR602: %w", err)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

func (a *API) hello(w http.ResponseWriter, r *http.Request) {
	body, sha, err := readBody(r, 1<<16)
	if err != nil {
		a.refuse(w, r, http.StatusBadRequest, err)
		return
	}
	var h Hello
	if err := json.Unmarshal(body, &h); err != nil {
		a.refuse(w, r, http.StatusBadRequest, fmt.Errorf("ERR603: %w", err))
		return
	}
	pub, err := agentkey.DecodePublic(h.PublicKey)
	if err != nil {
		a.refuse(w, r, http.StatusBadRequest, err)
		return
	}
	// the agent proves it holds the key it presents
	if err := a.verify(r, pub, sha); err != nil {
		a.refuse(w, r, http.StatusUnauthorized, err)
		return
	}
	id := agentkey.ID(pub)
	if r.Header.Get(agentkey.HeaderAgent) != id {
		a.refuse(w, r, http.StatusBadRequest, errors.New("ERR604: the agent id does not match its key"))
		return
	}

	a.mu.Lock()
	ag, err := core.Hello(a.State, domain.Agent{ID: id, PublicKey: h.PublicKey, Name: h.Name, Hostname: h.Hostname, Version: h.Version}, a.Now())
	a.mu.Unlock()
	if err != nil {
		a.Logger.Error().Msgf("ERR605: cannot register agent %s: %v", id, err)
		reply(w, http.StatusInternalServerError, apiError{"registry error"})
		return
	}
	if ag.Status == domain.AgentPending {
		a.Logger.Warn().Str("agent", id).Str("name", h.Name).Str("host", h.Hostname).
			Msg("WRN601: agent waiting for approval (laker agents approve <id>, or the Agents page)")
	}
	reply(w, http.StatusOK, HelloReply{ID: id, Status: ag.Status})
}

type ctxKey struct{}

// approved lets through signed requests of approved agents.
func (a *API) approved(next func(http.ResponseWriter, *http.Request, domain.Agent)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ag, err := core.GetAgent(a.State, r.Header.Get(agentkey.HeaderAgent))
		if err != nil || ag == nil {
			a.refuse(w, r, http.StatusUnauthorized, core.ErrUnknownAgent)
			return
		}
		if ag.Status != domain.AgentApproved {
			a.refuse(w, r, http.StatusForbidden, fmt.Errorf("ERR606: agent %s is %s", ag.ID, ag.Status))
			return
		}
		pub, err := agentkey.DecodePublic(ag.PublicKey)
		if err != nil {
			a.refuse(w, r, http.StatusInternalServerError, err)
			return
		}
		// a blob body is checked against its name later; any other body here
		bodySHA := r.Header.Get(agentkey.HeaderContent)
		if r.Method == http.MethodPut {
			if bodySHA != r.PathValue("sha") {
				a.refuse(w, r, http.StatusBadRequest, errors.New("ERR607: the declared hash is not the blob name"))
				return
			}
		} else if r.Method == http.MethodPost {
			select {
			case a.slots <- struct{}{}:
				defer func() { <-a.slots }()
			case <-r.Context().Done():
				return // the agent went away while waiting
			}
			body, sha, err := readBody(r, MaxCommit)
			if err != nil {
				a.refuse(w, r, http.StatusRequestEntityTooLarge, err)
				return
			}
			bodySHA = sha
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, body))
		}
		if err := a.verify(r, pub, bodySHA); err != nil {
			a.refuse(w, r, http.StatusUnauthorized, err)
			return
		}
		a.mu.Lock()
		_ = core.Touch(a.State, *ag, a.Now())
		a.mu.Unlock()
		next(w, r, *ag)
	}
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (a *API) blobExists(w http.ResponseWriter, r *http.Request, _ domain.Agent) {
	ok, err := a.Ingest.BlobExists(r.Context(), r.PathValue("sha"))
	switch {
	case err != nil:
		w.WriteHeader(http.StatusBadRequest)
	case ok:
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// putBlob stores the body in a local temporary file while hashing it, and
// writes it to the lake only if the hash is the name it was sent under.
func (a *API) putBlob(w http.ResponseWriter, r *http.Request, ag domain.Agent) {
	sha := r.PathValue("sha")
	if !shaRE.MatchString(sha) {
		a.refuse(w, r, http.StatusBadRequest, errors.New("ERR608: not a sha256"))
		return
	}
	if err := os.MkdirAll(a.TmpDir, 0o750); err != nil {
		reply(w, http.StatusInternalServerError, apiError{err.Error()})
		return
	}
	tmp, err := os.CreateTemp(a.TmpDir, "agent-blob-")
	if err != nil {
		reply(w, http.StatusInternalServerError, apiError{err.Error()})
		return
	}
	defer os.Remove(tmp.Name()) // local scratch copy, not the lake
	defer tmp.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r.Body)
	if err != nil {
		a.refuse(w, r, http.StatusBadRequest, fmt.Errorf("ERR609: %w", err))
		return
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		a.refuse(w, r, http.StatusBadRequest, fmt.Errorf("ERR60A: content hashes to %s, not %s", got, sha))
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		reply(w, http.StatusInternalServerError, apiError{err.Error()})
		return
	}
	created, err := a.Ingest.PutBlob(r.Context(), sha, n, tmp)
	if err != nil {
		a.Logger.Error().Str("agent", ag.ID).Msgf("ERR60B: cannot store a blob: %v", err)
		reply(w, http.StatusInternalServerError, apiError{"cannot store the blob"})
		return
	}
	reply(w, http.StatusOK, map[string]bool{"created": created})
}

func (a *API) claim(source, typ string, ag domain.Agent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return core.Claim(a.State, source, domain.Owner{Agent: ag.ID, Type: typ})
}

func (a *API) commit(w http.ResponseWriter, r *http.Request, ag domain.Agent) {
	var c domain.Commit
	if err := json.Unmarshal(r.Context().Value(ctxKey{}).([]byte), &c); err != nil {
		a.refuse(w, r, http.StatusBadRequest, fmt.Errorf("ERR60C: %w", err))
		return
	}
	if err := core.ValidateCommit(c); err != nil {
		a.refuse(w, r, http.StatusBadRequest, err)
		return
	}
	if err := a.claim(c.Adapter, c.Type, ag); err != nil {
		a.refuse(w, r, http.StatusConflict, err)
		return
	}
	if err := a.Ingest.Commit(r.Context(), c); err != nil {
		a.Logger.Error().Str("agent", ag.ID).Str("source", c.Adapter).Msgf("ERR60D: commit failed: %v", err)
		reply(w, http.StatusInternalServerError, apiError{"commit failed"})
		return
	}
	a.Logger.Debug().Str("agent", ag.ID).Str("source", c.Adapter).Int("files", len(c.Files)).Msg("DBG600: commit from an agent")
	reply(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) finish(w http.ResponseWriter, r *http.Request, ag domain.Agent) {
	var run domain.Run
	if err := json.Unmarshal(r.Context().Value(ctxKey{}).([]byte), &run); err != nil {
		a.refuse(w, r, http.StatusBadRequest, fmt.Errorf("ERR60E: %w", err))
		return
	}
	if err := a.claim(run.Adapter, run.Type, ag); err != nil {
		a.refuse(w, r, http.StatusConflict, err)
		return
	}
	if err := a.Ingest.Finish(r.Context(), run); err != nil {
		reply(w, http.StatusInternalServerError, apiError{"cannot record the run"})
		return
	}
	a.Logger.Info().Str("agent", ag.ID).Str("source", run.Adapter).Str("status", string(run.Status)).
		Int("rows", run.Stats.Rows).Msg("INF600: run of an agent recorded")
	reply(w, http.StatusOK, map[string]bool{"ok": true})
}
