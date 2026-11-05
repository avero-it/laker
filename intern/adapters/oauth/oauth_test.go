package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/adapters/state/bbolt"
)

const (
	testToken    = "0123456789abcdef-web"
	testRedirect = "https://claude.ai/api/mcp/auth_callback"
	verifier     = "a-verifier-of-at-least-forty-three-characters-0123456789"
)

type env struct {
	t   *testing.T
	s   *Server
	srv *httptest.Server
	now time.Time
	// client never follows redirects: the test reads them
	c *http.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store, err := bbolt.Open(filepath.Join(t.TempDir(), "oauth.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	// the SDK middleware checks the expiration against the real clock
	e := &env{t: t, now: time.Now()}
	mux := http.NewServeMux()
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	e.s, err = New(Options{
		PublicURL: e.srv.URL, RedirectURIs: []string{testRedirect}, Token: testToken,
		Store: store, Logger: zerolog.Nop(), Now: func() time.Time { return e.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Routes(mux)
	mux.Handle(ResourcePath, e.s.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("lake"))
	})))
	e.c = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e
}

func (e *env) json(resp *http.Response, v any) {
	e.t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) register(body string) (*http.Response, map[string]any) {
	e.t.Helper()
	resp, err := e.c.Post(e.srv.URL+"/oauth/register", "application/json", strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	var m map[string]any
	e.json(resp, &m)
	return resp, m
}

func challenge(v string) string {
	h := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func (e *env) authParams(clientID string) url.Values {
	return url.Values{
		"client_id": {clientID}, "redirect_uri": {testRedirect}, "response_type": {"code"},
		"state": {"xyz"}, "code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"},
		"resource": {e.s.Resource()}, "scope": {Scope},
	}
}

// approve posts the consent form and returns the redirect it answers.
func (e *env) approve(clientID, token string) *url.URL {
	e.t.Helper()
	v := e.authParams(clientID)
	v.Set("token", token)
	v.Set("action", "approve")
	resp, err := e.c.PostForm(e.srv.URL+"/oauth/authorize", v)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return nil
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *env) token(form url.Values) (int, map[string]any) {
	e.t.Helper()
	resp, err := e.c.PostForm(e.srv.URL+"/oauth/token", form)
	if err != nil {
		e.t.Fatal(err)
	}
	var m map[string]any
	e.json(resp, &m)
	return resp.StatusCode, m
}

func (e *env) callMCP(access string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+ResourcePath, nil)
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	resp, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestMetadata(t *testing.T) {
	e := newEnv(t)
	resp := e.callMCP("")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if h := resp.Header.Get("WWW-Authenticate"); !strings.Contains(h, `resource_metadata="`+e.srv.URL+"/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("WWW-Authenticate: %q", h)
	}

	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		r, err := e.c.Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		var prm map[string]any
		e.json(r, &prm)
		if prm["resource"] != e.srv.URL+"/mcp" || prm["authorization_servers"].([]any)[0] != e.srv.URL {
			t.Fatalf("%s: %v", p, prm)
		}
	}

	r, err := e.c.Get(e.srv.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	var as map[string]any
	e.json(r, &as)
	if as["issuer"] != e.srv.URL || as["registration_endpoint"] != e.srv.URL+"/oauth/register" {
		t.Fatalf("metadata: %v", as)
	}
	if m := as["code_challenge_methods_supported"].([]any); len(m) != 1 || m[0] != "S256" {
		t.Fatalf("PKCE methods: %v", m)
	}
	if _, ok := as["jwks_uri"]; ok {
		t.Fatal("jwks_uri must not be advertised")
	}
}

func TestRegisterRefusesOtherRedirects(t *testing.T) {
	e := newEnv(t)
	resp, m := e.register(`{"redirect_uris":["https://evil.example/cb"],"token_endpoint_auth_method":"none"}`)
	if resp.StatusCode != http.StatusBadRequest || m["error"] != "invalid_redirect_uri" {
		t.Fatalf("%d %v", resp.StatusCode, m)
	}
}

func TestFullFlow(t *testing.T) {
	e := newEnv(t)
	resp, reg := e.register(`{"redirect_uris":["` + testRedirect + `"],"token_endpoint_auth_method":"none","client_name":"Claude"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %v", resp.StatusCode, reg)
	}
	id := reg["client_id"].(string)
	if _, ok := reg["client_secret"]; ok {
		t.Fatal("a public client got a secret")
	}

	// the consent page names the client and asks for the token
	r, err := e.c.Get(e.srv.URL + "/oauth/authorize?" + e.authParams(id).Encode())
	if err != nil {
		t.Fatal(err)
	}
	var page strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		page.Write(buf[:n])
		if err != nil {
			break
		}
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK || !strings.Contains(page.String(), "Claude") || !strings.Contains(page.String(), `name="token"`) {
		t.Fatalf("consent page: %d", r.StatusCode)
	}

	if u := e.approve(id, "wrong-token-123456"); u != nil {
		t.Fatalf("a wrong token was accepted: %v", u)
	}
	u := e.approve(id, testToken)
	if u == nil || u.Query().Get("state") != "xyz" || u.Query().Get("iss") != e.srv.URL || u.Query().Get("code") == "" {
		t.Fatalf("redirect: %v", u)
	}
	code := u.Query().Get("code")

	base := url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {code},
		"redirect_uri": {testRedirect}, "resource": {e.s.Resource()}}
	bad := url.Values{}
	for k, v := range base {
		bad[k] = v
	}
	bad.Set("code_verifier", strings.Repeat("x", 50))
	if st, m := e.token(bad); st != http.StatusBadRequest || m["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %v", st, m)
	}
	// a wrong verifier burns the code
	base.Set("code_verifier", verifier)
	if st, _ := e.token(base); st != http.StatusBadRequest {
		t.Fatalf("a burnt code was accepted: %d", st)
	}

	// again, with the right verifier first
	code = e.approve(id, testToken).Query().Get("code")
	base.Set("code", code)
	st, tok := e.token(base)
	if st != http.StatusOK || tok["token_type"] != "Bearer" {
		t.Fatalf("token: %d %v", st, tok)
	}
	if st, _ := e.token(base); st != http.StatusBadRequest {
		t.Fatal("a code was used twice")
	}
	access, refresh := tok["access_token"].(string), tok["refresh_token"].(string)
	if resp := e.callMCP(access); resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp with token: %d", resp.StatusCode)
	}
	if resp := e.callMCP(testToken); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the web token opened mcp: %d", resp.StatusCode)
	}

	// refresh rotates: the old refresh token stops working
	rf := url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {refresh}}
	st, tok2 := e.token(rf)
	if st != http.StatusOK || tok2["access_token"] == access {
		t.Fatalf("refresh: %d %v", st, tok2)
	}
	if st, _ := e.token(rf); st != http.StatusBadRequest {
		t.Fatal("a refresh token was used twice")
	}

	// access tokens expire
	e.now = e.now.Add(2 * time.Hour)
	if resp := e.callMCP(tok2["access_token"].(string)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", resp.StatusCode)
	}

	// revoking the client ends everything it holds
	clients, err := e.s.Clients()
	if err != nil || len(clients) != 1 || clients[0].Approved.IsZero() {
		t.Fatalf("clients: %v %v", clients, err)
	}
	if err := e.s.RevokeClient(id); err != nil {
		t.Fatal(err)
	}
	rf.Set("refresh_token", tok2["refresh_token"].(string))
	if st, _ := e.token(rf); st != http.StatusUnauthorized {
		t.Fatalf("a revoked client refreshed: %d", st)
	}
}

func TestWrongResourceAndConfidentialClient(t *testing.T) {
	e := newEnv(t)
	_, reg := e.register(`{"redirect_uris":["` + testRedirect + `"],"token_endpoint_auth_method":"client_secret_post"}`)
	id, sec := reg["client_id"].(string), reg["client_secret"].(string)
	if sec == "" {
		t.Fatal("no secret")
	}

	v := e.authParams(id)
	v.Set("resource", "https://other.example/mcp")
	r, err := e.c.Get(e.srv.URL + "/oauth/authorize?" + v.Encode())
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if loc, _ := url.Parse(r.Header.Get("Location")); r.StatusCode != http.StatusFound || loc.Query().Get("error") != "invalid_target" {
		t.Fatalf("wrong resource: %d %v", r.StatusCode, r.Header.Get("Location"))
	}

	code := e.approve(id, testToken).Query().Get("code")
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {code}, "code_verifier": {verifier}}
	if st, m := e.token(form); st != http.StatusUnauthorized || m["error"] != "invalid_client" {
		t.Fatalf("no secret: %d %v", st, m)
	}
	code = e.approve(id, testToken).Query().Get("code")
	form.Set("code", code)
	form.Set("client_secret", sec)
	if st, m := e.token(form); st != http.StatusOK {
		t.Fatalf("with secret: %d %v", st, m)
	}
}

func TestUnknownRedirectIsShownNotFollowed(t *testing.T) {
	e := newEnv(t)
	_, reg := e.register(`{"redirect_uris":["` + testRedirect + `"],"token_endpoint_auth_method":"none"}`)
	v := e.authParams(reg["client_id"].(string))
	v.Set("redirect_uri", "https://evil.example/cb")
	r, err := e.c.Get(e.srv.URL + "/oauth/authorize?" + v.Encode())
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest || r.Header.Get("Location") != "" {
		t.Fatalf("%d %q", r.StatusCode, r.Header.Get("Location"))
	}
}
