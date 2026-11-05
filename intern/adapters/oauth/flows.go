package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/avero-it/laker/intern/core/ports/out"
)

// register is dynamic client registration (RFC 7591). Anyone may register,
// but only with allowed redirect URIs, and nothing is granted until the
// owner approves the client.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.public.Allow() {
		oauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many requests")
		return
	}
	var m oauthex.ClientRegistrationMetadata
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&m); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body is not client metadata")
		return
	}
	if len(m.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range m.RedirectURIs {
		if !s.allowedRedirect(u) {
			s.Logger.Warn().Str("redirect_uri", u).Msg("WRNA00: client registration refused: redirect URI not allowed")
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URI not allowed by this server: "+u)
			return
		}
	}
	method := m.TokenEndpointAuthMethod
	if method == "" {
		method = "client_secret_basic" // the default of RFC 7591 §2
	}
	if !slices.Contains(authMethods, method) {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "token_endpoint_auth_method must be one of "+strings.Join(authMethods, ", "))
		return
	}
	for _, g := range m.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant type "+g)
			return
		}
	}

	now := s.Now()
	if err := s.dropUnused(now); err != nil {
		s.Logger.Error().Msgf("ERRA03: %v", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "cannot store the client")
		return
	}
	id, _ := secret()
	c := Client{ID: id, Name: clientName(m.ClientName), RedirectURIs: m.RedirectURIs, AuthMethod: method, Created: now}
	var plainSecret string
	if method != "none" {
		plainSecret, c.SecretHash = secret()
	}
	if err := s.Store.Apply([]out.StateChange{put(bucketClients, c.ID, c)}); err != nil {
		s.Logger.Error().Msgf("ERRA04: cannot store the client: %v", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "cannot store the client")
		return
	}
	s.Logger.Info().Str("client", c.Name).Msg("INFA00: OAuth client registered, waiting for approval")
	resp := map[string]any{
		"client_id": c.ID, "client_id_issued_at": now.Unix(), "client_name": c.Name,
		"redirect_uris": c.RedirectURIs, "token_endpoint_auth_method": method,
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"scope": Scope,
	}
	if plainSecret != "" {
		resp["client_secret"] = plainSecret
		resp["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, resp)
}

func clientName(n string) string {
	n = strings.TrimSpace(n)
	if n == "" {
		return "unnamed client"
	}
	if r := []rune(n); len(r) > 80 {
		n = string(r[:80])
	}
	return n
}

// dropUnused removes the clients never approved after unusedClientTTL, and
// refuses a new one past maxClients.
func (s *Server) dropUnused(now time.Time) error {
	var changes []out.StateChange
	count := 0
	err := s.Store.ForEach(bucketClients, "", func(k string, v []byte) error {
		var c Client
		if json.Unmarshal(v, &c) == nil && c.Approved.IsZero() && now.Sub(c.Created) > unusedClientTTL {
			changes = append(changes, del(bucketClients, k))
			return nil
		}
		count++
		return nil
	})
	if err != nil {
		return err
	}
	if count >= maxClients {
		return fmt.Errorf("ERRA05: %d clients are registered, revoke some before adding more", count)
	}
	return s.Store.Apply(changes)
}

// authRequest is an authorization request, checked.
type authRequest struct {
	Client      *Client
	RedirectURI string
	State       string
	Challenge   string
	Scope       string
}

// parseAuthorize checks the request. A bad client or redirect URI is shown
// to the owner (redirecting there would be an open redirect); any other
// problem goes back to the client as an OAuth error.
func (s *Server) parseAuthorize(v url.Values) (req *authRequest, page string, back url.Values) {
	c, err := s.client(v.Get("client_id"))
	if err != nil || c == nil {
		return nil, "Unknown client: register it again from the application.", nil
	}
	redirect := v.Get("redirect_uri")
	if redirect == "" && len(c.RedirectURIs) == 1 {
		redirect = c.RedirectURIs[0]
	}
	if !slices.Contains(c.RedirectURIs, redirect) || !s.allowedRedirect(redirect) {
		return nil, "The redirect URI is not the one registered by the client.", nil
	}
	req = &authRequest{Client: c, RedirectURI: redirect, State: v.Get("state"), Challenge: v.Get("code_challenge"), Scope: Scope}
	fail := func(code, desc string) (*authRequest, string, url.Values) {
		return req, "", url.Values{"error": {code}, "error_description": {desc}}
	}
	switch {
	case v.Get("response_type") != "code":
		return fail("unsupported_response_type", "only response_type=code")
	case req.Challenge == "" || v.Get("code_challenge_method") != "S256":
		return fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
	case !s.resourceOK(v.Get("resource")):
		return fail("invalid_target", "the resource is not "+s.Resource())
	}
	for _, sc := range strings.Fields(v.Get("scope")) {
		if sc != Scope {
			return fail("invalid_scope", "the only scope is "+Scope)
		}
	}
	return req, "", nil
}

// resourceOK accepts no resource (older clients) or this one (RFC 8707).
func (s *Server) resourceOK(r string) bool {
	return r == "" || strings.TrimSuffix(r, "/") == s.Resource()
}

func (s *Server) redirectBack(w http.ResponseWriter, r *http.Request, req *authRequest, params url.Values) {
	u, _ := url.Parse(req.RedirectURI)
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	q.Set("iss", s.PublicURL) // RFC 9207
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

type consentData struct {
	Client   string
	Redirect string
	Params   map[string]string
	Error    string
}

func (s *Server) renderConsent(w http.ResponseWriter, status int, req *authRequest, v url.Values, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	data := consentData{Error: msg, Params: map[string]string{}}
	if req != nil {
		data.Client = req.Client.Name
		if u, err := url.Parse(req.RedirectURI); err == nil {
			data.Redirect = u.Host
		}
		for _, k := range []string{"client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method", "resource", "scope", "response_type"} {
			if x := v.Get(k); x != "" {
				data.Params[k] = x
			}
		}
	}
	if err := s.consent.ExecuteTemplate(w, "consent", data); err != nil {
		s.Logger.Error().Msgf("ERRA06: consent page: %v", err)
	}
}

func (s *Server) authorizePage(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	req, page, back := s.parseAuthorize(v)
	switch {
	case page != "":
		s.renderConsent(w, http.StatusBadRequest, nil, v, page)
	case back != nil:
		s.redirectBack(w, r, req, back)
	default:
		s.renderConsent(w, http.StatusOK, req, v, "")
	}
}

// authorize is the owner's answer on the consent page.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	if o := r.Header.Get("Origin"); o != "" && o != s.PublicURL && !sameHost(o, r.Host) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	v := r.PostForm
	req, page, back := s.parseAuthorize(v)
	switch {
	case page != "":
		s.renderConsent(w, http.StatusBadRequest, nil, v, page)
		return
	case back != nil:
		s.redirectBack(w, r, req, back)
		return
	}
	if v.Get("action") != "approve" {
		s.Logger.Info().Str("client", req.Client.Name).Msg("INFA01: OAuth client denied by the owner")
		s.redirectBack(w, r, req, url.Values{"error": {"access_denied"}, "error_description": {"the owner denied access"}})
		return
	}
	// failures alone empty the limiter: a burst of wrong tokens is refused
	// whatever the number of concurrent requests
	if s.failures.Tokens() < 1 {
		s.renderConsent(w, http.StatusTooManyRequests, req, v, "Too many attempts: wait a few seconds")
		return
	}
	if !s.validToken(v.Get("token")) {
		s.failures.Allow()
		s.Logger.Warn().Str("remote", r.RemoteAddr).Msg("WRNA01: wrong token on the OAuth consent page")
		time.Sleep(time.Second)
		s.renderConsent(w, http.StatusUnauthorized, req, v, "Invalid token")
		return
	}

	now := s.Now()
	plain, key := secret()
	req.Client.Approved = now
	changes := []out.StateChange{
		put(bucketCodes, key, code{ClientID: req.Client.ID, RedirectURI: req.RedirectURI, Challenge: req.Challenge, Scope: req.Scope, Expires: now.Add(codeTTL)}),
		put(bucketClients, req.Client.ID, req.Client),
	}
	if err := s.Store.Apply(changes); err != nil {
		s.Logger.Error().Msgf("ERRA07: cannot store the authorization code: %v", err)
		s.redirectBack(w, r, req, url.Values{"error": {"server_error"}})
		return
	}
	s.Logger.Info().Str("client", req.Client.Name).Msg("INFA02: OAuth client approved by the owner")
	s.redirectBack(w, r, req, url.Values{"code": {plain}})
}

func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host == host
}

// tokenResponse is RFC 6749 §5.1.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.public.Allow() {
		oauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many requests")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	c, ok := s.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="laker"`)
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client or wrong secret")
		return
	}
	v := r.PostForm
	if !s.resourceOK(v.Get("resource")) {
		oauthError(w, http.StatusBadRequest, "invalid_target", "the resource is not "+s.Resource())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	var changes []out.StateChange
	switch v.Get("grant_type") {
	case "authorization_code":
		var cd code
		key := hash(v.Get("code"))
		found, err := s.get(bucketCodes, key, &cd)
		if err != nil {
			s.Logger.Error().Msgf("ERRA08: %v", err)
			oauthError(w, http.StatusInternalServerError, "server_error", "")
			return
		}
		if found {
			changes = append(changes, del(bucketCodes, key)) // used once, also when wrong
		}
		switch {
		case !found || now.After(cd.Expires) || cd.ClientID != c.ID:
			_ = s.Store.Apply(changes)
			oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown, used or expired code")
			return
		case v.Get("redirect_uri") != "" && v.Get("redirect_uri") != cd.RedirectURI:
			_ = s.Store.Apply(changes)
			oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri differs from the authorization request")
			return
		case !pkceOK(v.Get("code_verifier"), cd.Challenge):
			_ = s.Store.Apply(changes)
			oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match")
			return
		}
	case "refresh_token":
		var g grant
		key := hash(v.Get("refresh_token"))
		found, err := s.get(bucketTokens, key, &g)
		if err != nil {
			s.Logger.Error().Msgf("ERRA09: %v", err)
			oauthError(w, http.StatusInternalServerError, "server_error", "")
			return
		}
		if !found || g.Kind != "refresh" || g.ClientID != c.ID || now.After(g.Expires) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown, used or expired refresh token")
			return
		}
		changes = append(changes, del(bucketTokens, key)) // rotated
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code or refresh_token")
		return
	}

	access, accessHash := secret()
	refresh, refreshHash := secret()
	c.LastUsed = now
	changes = append(changes,
		put(bucketTokens, accessHash, grant{Kind: "access", ClientID: c.ID, Scope: Scope, Expires: now.Add(accessTTL)}),
		put(bucketTokens, refreshHash, grant{Kind: "refresh", ClientID: c.ID, Scope: Scope, Expires: now.Add(refreshTTL)}),
		put(bucketClients, c.ID, c),
	)
	changes = append(changes, s.expired(now)...)
	if err := s.Store.Apply(changes); err != nil {
		s.Logger.Error().Msgf("ERRA0A: cannot store the tokens: %v", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int(accessTTL.Seconds()),
		RefreshToken: refresh, Scope: Scope,
	})
}

// expired are the deletions of the codes and tokens past their time.
func (s *Server) expired(now time.Time) []out.StateChange {
	var changes []out.StateChange
	for _, b := range []string{bucketCodes, bucketTokens} {
		_ = s.Store.ForEach(b, "", func(k string, v []byte) error {
			var e struct {
				Expires time.Time `json:"expires"`
			}
			if json.Unmarshal(v, &e) != nil || now.After(e.Expires) {
				changes = append(changes, del(b, k))
			}
			return nil
		})
	}
	return changes
}

func pkceOK(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	h := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(h[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// authenticate finds the client of a token or revocation request, checking
// its secret when it has one (client_secret_basic or client_secret_post).
func (s *Server) authenticate(r *http.Request) (*Client, bool) {
	id, sec, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		sec, _ = url.QueryUnescape(sec)
	} else {
		id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	c, err := s.client(id)
	if err != nil || c == nil {
		return nil, false
	}
	if c.SecretHash == "" {
		return c, true
	}
	return c, sec != "" && subtle.ConstantTimeCompare([]byte(hash(sec)), []byte(c.SecretHash)) == 1
}

// revoke is RFC 7009: always 200, whether the token existed or not.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	c, ok := s.authenticate(r)
	if !ok {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client or wrong secret")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hash(r.PostForm.Get("token"))
	var g grant
	if found, _ := s.get(bucketTokens, key, &g); found && g.ClientID == c.ID {
		_ = s.Store.Apply([]out.StateChange{del(bucketTokens, key)})
	}
	w.WriteHeader(http.StatusOK)
}

// verify checks an access token for the MCP endpoint.
func (s *Server) verify(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	var g grant
	found, err := s.get(bucketTokens, hash(token), &g)
	if err != nil {
		return nil, err
	}
	if !found || g.Kind != "access" || s.Now().After(g.Expires) {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{Scopes: strings.Fields(g.Scope), Expiration: g.Expires, UserID: g.ClientID}, nil
}

// Clients are the registered clients, most recently used first.
func (s *Server) Clients() ([]Client, error) {
	var list []Client
	err := s.Store.ForEach(bucketClients, "", func(_ string, v []byte) error {
		var c Client
		if err := json.Unmarshal(v, &c); err != nil {
			return fmt.Errorf("ERRA0B: corrupt client: %w", err)
		}
		c.SecretHash = ""
		list = append(list, c)
		return nil
	})
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if !a.LastUsed.Equal(b.LastUsed) {
			return a.LastUsed.After(b.LastUsed)
		}
		return a.Created.After(b.Created)
	})
	return list, err
}

// RevokeClient removes a client and every token it holds: its next request
// is refused, and it has to be approved again.
func (s *Server) RevokeClient(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.client(id)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("ERRA0C: no client %q", id)
	}
	changes := []out.StateChange{del(bucketClients, id)}
	err = s.Store.ForEach(bucketTokens, "", func(k string, v []byte) error {
		var g grant
		if json.Unmarshal(v, &g) != nil || g.ClientID == id {
			changes = append(changes, del(bucketTokens, k))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.Store.Apply(changes); err != nil {
		return fmt.Errorf("ERRA0D: cannot revoke the client: %w", err)
	}
	s.Logger.Info().Str("client", c.Name).Msg("INFA03: OAuth client revoked")
	return nil
}
