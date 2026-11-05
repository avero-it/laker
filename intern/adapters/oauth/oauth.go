// Package oauth is the OAuth 2.1 authorization server in front of the MCP
// endpoint of `laker serve`, as Claude connectors (and any MCP client) expect
// it: protected resource metadata (RFC 9728), authorization server metadata
// (RFC 8414), dynamic client registration (RFC 7591), the authorization code
// grant with PKCE S256 only, rotating refresh tokens and revocation.
//
// Laker has one owner and no identity provider: a client is approved on a
// consent page by typing the web token. Clients may register only the
// configured redirect URIs. Tokens are opaque; only their SHA-256 is stored,
// in a store of its own (it is not derived from the lake: losing it only
// means approving the clients again).
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/rs/zerolog"
	"golang.org/x/time/rate"

	"github.com/avero-it/laker/intern/core/ports/out"
)

//go:embed templates/*.html
var templateFS embed.FS

// Scope is the only scope: read the lake.
const Scope = "lake:read"

// ResourcePath is where the protected resource (the MCP endpoint) is served.
const ResourcePath = "/mcp"

const (
	codeTTL    = time.Minute
	accessTTL  = time.Hour
	refreshTTL = 60 * 24 * time.Hour
	// a registration nobody approves is dropped after a day
	unusedClientTTL = 24 * time.Hour
	maxClients      = 100
)

const (
	bucketClients = "oauth_clients"
	bucketCodes   = "oauth_codes"
	bucketTokens  = "oauth_tokens"
)

type Options struct {
	// PublicURL is the https origin the clients reach Laker at; it is the
	// issuer, and PublicURL+ResourcePath is the resource.
	PublicURL string
	// RedirectURIs are the only callbacks a client may register.
	RedirectURIs []string
	// Token is the web token the owner types to approve a client.
	Token  string
	Store  out.StateStore
	Logger zerolog.Logger
	// Now is time.Now unless a test sets it.
	Now func() time.Time
}

type Server struct {
	Options
	consent *template.Template
	// mu serializes the read-then-write of codes and tokens, so a code or a
	// refresh token is used once even under concurrent requests
	mu sync.Mutex
	// failures slows down guessing the token on the consent page, whatever
	// the number of concurrent requests
	failures *rate.Limiter
	// public limits the unauthenticated endpoints that write (register, token)
	public *rate.Limiter
}

// Client is a registered client, as the web interface lists it.
type Client struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	AuthMethod   string    `json:"auth_method"`
	SecretHash   string    `json:"secret_hash,omitempty"`
	Created      time.Time `json:"created"`
	// Approved is the last time the owner approved it, zero if never.
	Approved time.Time `json:"approved"`
	LastUsed time.Time `json:"last_used"`
}

type code struct {
	ClientID    string    `json:"client_id"`
	RedirectURI string    `json:"redirect_uri"`
	Challenge   string    `json:"challenge"`
	Scope       string    `json:"scope"`
	Expires     time.Time `json:"expires"`
}

type grant struct {
	Kind     string    `json:"kind"` // "access" or "refresh"
	ClientID string    `json:"client_id"`
	Scope    string    `json:"scope"`
	Expires  time.Time `json:"expires"`
}

func New(o Options) (*Server, error) {
	if o.PublicURL == "" || o.Token == "" || o.Store == nil || len(o.RedirectURIs) == 0 {
		return nil, errors.New("ERRA00: oauth needs a public URL, the web token, a store and redirect URIs")
	}
	o.PublicURL = strings.TrimSuffix(o.PublicURL, "/")
	if o.Now == nil {
		o.Now = time.Now
	}
	t, err := template.ParseFS(templateFS, "templates/consent.html")
	if err != nil {
		return nil, fmt.Errorf("ERRA01: consent template: %w", err)
	}
	return &Server{
		Options: o, consent: t,
		failures: rate.NewLimiter(rate.Every(time.Second), 3),
		public:   rate.NewLimiter(rate.Every(100*time.Millisecond), 20),
	}, nil
}

// Resource is the URL of the MCP endpoint, the audience of every token.
func (s *Server) Resource() string { return s.PublicURL + ResourcePath }

func (s *Server) resourceMetadataURL() string {
	return s.PublicURL + "/.well-known/oauth-protected-resource" + ResourcePath
}

// Routes adds the metadata and OAuth endpoints to mux.
func (s *Server) Routes(mux *http.ServeMux) {
	prm := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.Resource(),
		AuthorizationServers:   []string{s.PublicURL},
		ScopesSupported:        []string{Scope},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Laker",
	})
	mux.Handle("/.well-known/oauth-protected-resource", prm)
	mux.Handle("/.well-known/oauth-protected-resource"+ResourcePath, prm)
	mux.Handle("/.well-known/oauth-authorization-server", cors(http.HandlerFunc(s.metadata)))
	mux.Handle("/oauth/register", cors(http.HandlerFunc(s.register)))
	mux.HandleFunc("GET /oauth/authorize", s.authorizePage)
	mux.HandleFunc("POST /oauth/authorize", s.authorize)
	mux.Handle("/oauth/token", cors(http.HandlerFunc(s.token)))
	mux.Handle("/oauth/revoke", cors(http.HandlerFunc(s.revoke)))
}

// Protect lets through only the requests with a valid access token for the
// resource, and answers the others 401 with the metadata address.
func (s *Server) Protect(h http.Handler) http.Handler {
	return auth.RequireBearerToken(s.verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.resourceMetadataURL(),
		Scopes:              []string{Scope},
	})(h)
}

// cors allows any origin: these endpoints use no cookie, and browser based
// MCP clients (the inspector, for one) call them directly.
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// asMetadata is RFC 8414; oauthex.AuthServerMeta always writes jwks_uri,
// which an empty value makes wrong.
type asMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	AuthorizationResponseIss          bool     `json:"authorization_response_iss_parameter_supported"`
}

var authMethods = []string{"none", "client_secret_post", "client_secret_basic"}

func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, asMetadata{
		Issuer:                            s.PublicURL,
		AuthorizationEndpoint:             s.PublicURL + "/oauth/authorize",
		TokenEndpoint:                     s.PublicURL + "/oauth/token",
		RegistrationEndpoint:              s.PublicURL + "/oauth/register",
		RevocationEndpoint:                s.PublicURL + "/oauth/revoke",
		ScopesSupported:                   []string{Scope},
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: authMethods,
		CodeChallengeMethodsSupported:     []string{"S256"},
		AuthorizationResponseIss:          true,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// oauthError is the error body of RFC 6749 §5.2 and RFC 7591 §3.2.2.
func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

// secret is a new random value and the hash that is stored of it.
func secret() (string, string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	return v, hash(v)
}

func hash(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

func (s *Server) validToken(t string) bool {
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1
}

func (s *Server) allowedRedirect(u string) bool { return slices.Contains(s.RedirectURIs, u) }

func (s *Server) get(bucket, key string, v any) (bool, error) {
	raw, err := s.Store.Get(bucket, key)
	if err != nil || raw == nil {
		return false, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, fmt.Errorf("ERRA02: corrupt %s entry: %w", bucket, err)
	}
	return true, nil
}

func put(bucket, key string, v any) out.StateChange {
	raw, _ := json.Marshal(v)
	return out.StateChange{Bucket: bucket, Key: key, Value: raw}
}

func del(bucket, key string) out.StateChange { return out.StateChange{Bucket: bucket, Key: key} }

func (s *Server) client(id string) (*Client, error) {
	var c Client
	ok, err := s.get(bucketClients, id, &c)
	if !ok || err != nil {
		return nil, err
	}
	return &c, nil
}
