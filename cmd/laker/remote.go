package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/avero-it/laker/intern/adapters/web"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/pkg/config"
)

// remote is the JSON API of a running `laker serve`. While serve runs with
// its web interface it holds the local stores, so the CLI asks it instead.
type remote struct {
	base   string
	token  string
	client *http.Client
}

// errNotServing means no serve answers: the CLI opens the local stores.
var errNotServing = errors.New("ERR0A0: laker serve is not answering")

func newRemote(cfg *config.Config) *remote {
	w := cfg.Common.Web
	if !w.Enabled {
		return nil
	}
	host := w.Host
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return &remote{
		base:   "http://" + net.JoinHostPort(host, strconv.Itoa(w.Port)),
		token:  w.Token,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *remote) get(ctx context.Context, path string, q url.Values, out any) error {
	u := r.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("ERR0A1: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		if isDial(err) {
			return errNotServing
		}
		return fmt.Errorf("ERR0A2: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && path[:9] == "/api/doc/" {
		return errNotIndexed
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("ERR0A3: laker serve answered %s: %s", resp.Status, e.Error)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("ERR0A4: %w", err)
	}
	return nil
}

var errNotIndexed = errors.New("ERR0A5: not indexed")

// Search and Document make remote a searcher, like the local index.
func (r *remote) Search(ctx context.Context, q domain.Query) (domain.SearchResult, error) {
	var res domain.SearchResult
	err := r.get(ctx, "/api/search", url.Values{
		"q": {q.Text}, "kind": {q.Kind}, "limit": {strconv.Itoa(q.Limit)},
		"offset": {strconv.Itoa(q.Offset)}, "highlight": {q.Highlight},
	}, &res)
	return res, err
}

func (r *remote) Document(ctx context.Context, id string) (map[string]any, error) {
	var doc map[string]any
	err := r.get(ctx, "/api/doc/"+url.PathEscape(id), nil, &doc)
	if errors.Is(err, errNotIndexed) {
		return nil, nil
	}
	return doc, err
}

func (r *remote) Tree(ctx context.Context, source string, at time.Time) (domain.Tree, error) {
	q := url.Values{}
	if !at.IsZero() {
		q.Set("at", at.Format(time.RFC3339Nano))
	}
	var t domain.Tree
	err := r.get(ctx, "/api/tree/"+url.PathEscape(source), q, &t)
	return t, err
}

func (r *remote) Status(ctx context.Context) ([]web.AdapterStatus, error) {
	var st []web.AdapterStatus
	err := r.get(ctx, "/api/status", nil, &st)
	return st, err
}

// Run asks serve to start a job now.
func (r *remote) Run(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/api/run/"+url.PathEscape(name), nil)
	if err != nil {
		return fmt.Errorf("ERR0A6: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		if isDial(err) {
			return errNotServing
		}
		return fmt.Errorf("ERR0A7: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("ERR0A8: laker serve answered %s: %s", resp.Status, e.Error)
	}
	return nil
}

// runInServe hands a job to a running serve; false if none answers.
func runInServe(cfg *config.Config, name string) bool {
	rm := newRemote(cfg)
	if rm == nil {
		return false
	}
	err := rm.Run(context.Background(), name)
	if errors.Is(err, errNotServing) {
		return false
	}
	if err != nil {
		fatalf("ERR0A9: %v", err)
	}
	fmt.Printf("%s started in laker serve: follow it with laker list or on the status page\n", name)
	return true
}

// isDial tells that nothing listens: serve is not running.
func isDial(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}
