// Package web is the browser interface of Laker, served by `laker serve`:
// search, mail reading, file browsing (also at a past date), run status and
// manual runs, plus a small JSON API the CLI uses while serve holds the
// local stores. Everything is behind a token; it binds to localhost by
// default and belongs behind a TLS reverse proxy if exposed.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/adapters/oauth"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const cookieName = "laker_token"

// Adapter is a configured adapter, as the status page shows it.
type Adapter struct {
	Name     string
	Type     string
	Schedule string
	Enabled  bool
}

// Runs starts and observes the jobs of the scheduler.
type Runs interface {
	Trigger(name string) error
	Running(name string) bool
	Next(name string) time.Time
}

type Deps struct {
	Lake     out.Lake
	Index    out.Indexer
	State    out.StateStore
	Adapters []Adapter
	Runs     Runs
	LakeInfo domain.LakeInfo
	Location *time.Location
	Token    string
	Version  string
	Logger   zerolog.Logger
	// Agents receives the data of the agents (/agent/v1/): the only way data
	// reaches the lake. nil only in tests.
	Agents http.Handler
	// OAuth guards MCP (/mcp, behind its own tokens, not the web token);
	// both nil if MCP is disabled.
	OAuth *oauth.Server
	MCP   http.Handler
}

type Server struct {
	Deps
	pages map[string]*template.Template
}

func New(d Deps) (*Server, error) {
	if d.Token == "" {
		return nil, errors.New("ERR401: a token is required")
	}
	s := &Server{Deps: d, pages: map[string]*template.Template{}}
	funcs := template.FuncMap{}
	for _, page := range []string{"search", "mail", "chat", "sources", "files", "history", "status", "agents", "connectors", "error"} {
		t, err := template.New("layout").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("ERR402: template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	t, err := template.New("login").ParseFS(templateFS, "templates/login.html")
	if err != nil {
		return nil, fmt.Errorf("ERR403: template login: %w", err)
	}
	s.pages["login"] = t
	return s, nil
}

// Handler is the whole site.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)

	auth := func(h http.HandlerFunc) http.Handler { return s.requireToken(h) }
	mux.Handle("GET /{$}", auth(s.search))
	mux.Handle("GET /mail/{sha}", auth(s.mail))
	mux.Handle("GET /mail/{sha}/eml", auth(s.eml))
	mux.Handle("GET /mail/{sha}/part/{n}", auth(s.attachment))
	mux.Handle("GET /chat/{source}/{chat}", auth(s.chat))
	mux.Handle("GET /files", auth(s.sources))
	mux.Handle("GET /files/{source}", auth(s.files))
	mux.Handle("GET /files/{source}/history", auth(s.history))
	mux.Handle("GET /blob/{sha}/{name...}", auth(s.blob))
	mux.Handle("GET /status", auth(s.status))
	mux.Handle("POST /run/{name}", auth(s.run))
	mux.Handle("GET /agents", auth(s.agents))
	mux.Handle("POST /agents/{id}/{action}", auth(s.agentAction))
	mux.Handle("GET /connectors", auth(s.connectors))
	mux.Handle("POST /connectors/{id}/revoke", auth(s.revokeConnector))
	if s.OAuth != nil && s.MCP != nil {
		// behind OAuth access tokens, not behind the web token
		s.OAuth.Routes(mux)
		mux.Handle(oauth.ResourcePath, s.OAuth.Protect(s.MCP))
	}
	if s.Agents != nil {
		// signed by the agent keys, not behind the token
		mux.Handle("/agent/v1/", s.Agents)
	}

	mux.Handle("GET /api/search", auth(s.apiSearch))
	mux.Handle("GET /api/doc/{id}", auth(s.apiDoc))
	mux.Handle("GET /api/tree/{source}", auth(s.apiTree))
	mux.Handle("GET /api/status", auth(s.apiStatus))
	mux.Handle("POST /api/run/{name}", auth(s.apiRun))
	mux.Handle("GET /api/agents", auth(s.apiAgents))
	mux.Handle("POST /api/agents/{id}/{action}", auth(s.apiAgentAction))

	return s.headers(mux)
}

// headers adds the security headers, answers the health check and logs.
func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		// "no-referrer" would make browsers send "Origin: null" on our own
		// form POSTs, which sameOrigin refuses; nothing leaks cross-site
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		if h.Get("Content-Security-Policy") == "" {
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; frame-ancestors 'self'")
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		// the path only: queries can hold what is being searched for
		s.Logger.Debug().Str("method", r.Method).Str("path", r.URL.Path).Dur("took", time.Since(start)).Msg("DBG400: request")
	})
}

func (s *Server) validToken(t string) bool {
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1
}

// requireToken accepts the session cookie or "Authorization: Bearer <token>".
// POSTs must also come from this site.
func (s *Server) requireToken(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if c, err := r.Cookie(cookieName); err == nil && token == "" {
			token = c.Value
		}
		if !s.validToken(token) {
			if strings.HasPrefix(r.URL.Path, "/api/") || r.Method != http.MethodGet {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not a browser form, or a browser that sends no Origin on same-site POSTs
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.renderLogin(w, r.URL.Query().Get("next"), "")
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	next := localPath(r.FormValue("next"))
	if !s.validToken(r.FormValue("token")) {
		s.Logger.Warn().Str("remote", r.RemoteAddr).Msg("WRN400: wrong token")
		time.Sleep(time.Second) // slows down guessing
		s.renderLogin(w, next, "Invalid token")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.Token, Path: "/", MaxAge: 30 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// localPath keeps redirects on this site.
func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "/"
	}
	return p
}

func (s *Server) renderLogin(w http.ResponseWriter, next, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if msg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	if err := s.pages["login"].ExecuteTemplate(w, "login", map[string]string{"Next": localPath(next), "Error": msg}); err != nil {
		s.Logger.Error().Msgf("ERR404: login page: %v", err)
	}
}

func (s *Server) render(w http.ResponseWriter, page string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, "layout", data); err != nil {
		s.Logger.Error().Str("page", page).Msgf("ERR405: render: %v", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, title string, err error) {
	if status >= 500 {
		s.Logger.Error().Msgf("ERR406: %s: %v", title, err)
	}
	w.WriteHeader(status)
	s.render(w, "error", map[string]any{"Title": title, "Message": err.Error()})
}

// Serve listens on addr until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr: addr, Handler: s.Handler(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.Logger.Info().Str("addr", addr).Msg("INF400: web interface listening")

	select {
	case err := <-errc:
		return fmt.Errorf("ERR407: web server: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
