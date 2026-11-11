// Package agentclient is the Ingest of a laker-agent: it delivers blobs,
// commits and run records to laker serve over HTTPS, signing every request
// with the agent key. Every call is idempotent, so failed ones are retried.
package agentclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/adapters/agentapi"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/agentkey"
	"github.com/avero-it/laker/pkg/redact"
)

var _ out.Ingest = (*Client)(nil)

// ErrNotApproved means the server knows the agent but does not let it write.
var ErrNotApproved = errors.New("ERR610: the agent is not approved on laker serve")

// NotApproved tells a run error (kept as text in the run record) caused by
// ErrNotApproved.
func NotApproved(runError string) bool { return strings.Contains(runError, ErrNotApproved.Error()) }

type Client struct {
	base   string
	key    *agentkey.Key
	http   *http.Client
	logger zerolog.Logger
	now    func() time.Time
	// Retries of a failed call, waiting 1s, 2s, 4s… in between.
	Retries int
}

func New(server string, key *agentkey.Key, logger zerolog.Logger) (*Client, error) {
	u, err := url.Parse(strings.TrimSuffix(server, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("ERR611: server %q must be an absolute URL", server)
	}
	if u.Scheme != "https" && !local(u.Hostname()) {
		logger.Warn().Str("server", server).Msg("WRN610: plain http to a remote server: the data travels unencrypted")
	}
	return &Client{base: u.String(), key: key, http: &http.Client{}, logger: logger, now: time.Now, Retries: 3}, nil
}

func local(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("ERR612: laker serve answered %d: %s", e.code, e.msg)
}

// do sends a signed request; body is replayed on retries.
func (c *Client) do(ctx context.Context, method, path string, body io.ReadSeeker, contentSHA string, out any) (int, error) {
	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(time.Duration(1<<(attempt-1)) * time.Second):
			}
			if body != nil {
				if _, err := body.Seek(0, io.SeekStart); err != nil {
					return 0, fmt.Errorf("ERR613: %w", err)
				}
			}
		}
		code, err := c.once(ctx, method, path, body, contentSHA, out)
		if err == nil || (code > 0 && code < 500) {
			return code, err // done, or refused: retrying changes nothing
		}
		lastErr = err
		c.logger.Debug().Str("path", path).Int("attempt", attempt+1).Msgf("DBG610: %v", err)
	}
	return 0, lastErr
}

func (c *Client) once(ctx context.Context, method, path string, body io.Reader, contentSHA string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, fmt.Errorf("ERR614: %w", err)
	}
	ts := c.now().Unix()
	req.Header.Set(agentkey.HeaderAgent, c.key.ID())
	req.Header.Set(agentkey.HeaderTime, strconv.FormatInt(ts, 10))
	req.Header.Set(agentkey.HeaderContent, contentSHA)
	req.Header.Set(agentkey.HeaderSignature, c.key.Sign(method, path, ts, contentSHA))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("ERR615: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && !(method == http.MethodHead && resp.StatusCode == http.StatusNotFound) {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if resp.StatusCode == http.StatusForbidden {
			return resp.StatusCode, fmt.Errorf("%w: %s", ErrNotApproved, e.Error)
		}
		return resp.StatusCode, &statusError{resp.StatusCode, e.Error}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("ERR616: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (c *Client) postJSON(ctx context.Context, path string, v, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("ERR617: %w", err)
	}
	sum := sha256.Sum256(b)
	_, err = c.do(ctx, http.MethodPost, path, bytes.NewReader(b), hex.EncodeToString(sum[:]), out)
	return err
}

// Hello presents the agent; the server answers with its status.
func (c *Client) Hello(ctx context.Context, name, hostname, version string) (agentapi.HelloReply, error) {
	var r agentapi.HelloReply
	err := c.postJSON(ctx, "/agent/v1/hello", agentapi.Hello{
		PublicKey: agentkey.EncodePublic(c.key.Public()), Name: name, Hostname: hostname, Version: version,
	}, &r)
	return r, err
}

func (c *Client) BlobExists(ctx context.Context, sha string) (bool, error) {
	code, err := c.do(ctx, http.MethodHead, "/agent/v1/blobs/"+sha, nil, sha, nil)
	return code == http.StatusOK, err
}

// PutBlob uploads unless the server has the content already. A reader that
// cannot seek is sent once, without retries.
func (c *Client) PutBlob(ctx context.Context, sha string, size int64, r io.Reader) (bool, error) {
	exists, err := c.BlobExists(ctx, sha)
	if err != nil || exists {
		return false, err
	}
	var res struct {
		Created bool `json:"created"`
	}
	path := "/agent/v1/blobs/" + sha
	if rs, ok := r.(io.ReadSeeker); ok {
		_, err = c.do(ctx, http.MethodPut, path, rs, sha, &res)
	} else {
		_, err = c.once(ctx, http.MethodPut, path, r, sha, &res)
	}
	return res.Created, err
}

// MaxPart is about how large a commit request may be: a larger commit is
// sent in pieces, so neither side holds much more than this for one request.
// A var for the tests.
var MaxPart = 32 << 20

// Commit sends the commit, in pieces if it is large. Each piece is a commit
// of its own and idempotent: after a failure the next run sends them again.
func (c *Client) Commit(ctx context.Context, cm domain.Commit) error {
	for _, part := range cm.Split(MaxPart) {
		if err := c.postJSON(ctx, "/agent/v1/commit", part, nil); err != nil {
			return err
		}
	}
	return nil
}

// Finish sends the run record, with any credential its error quotes removed:
// the secrets of the sources never leave the agent.
func (c *Client) Finish(ctx context.Context, run domain.Run) error {
	run.Error = redact.String(run.Error)
	return c.postJSON(ctx, "/agent/v1/finish", run, nil)
}
