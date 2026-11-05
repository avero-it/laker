package web

import (
	"errors"
	"net/http"
	"net/url"
)

var errMCPDisabled = errors.New("ERR410: common.mcp is not enabled")

type connectorView struct {
	ID, Name, Redirects, Created, Approved, LastUsed string
}

// connectors lists the OAuth clients of MCP, so the owner can revoke them.
func (s *Server) connectors(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Title": "Connectors", "Enabled": s.OAuth != nil}
	if s.OAuth != nil {
		list, err := s.OAuth.Clients()
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "Cannot read the connectors", err)
			return
		}
		var views []connectorView
		for _, c := range list {
			v := connectorView{ID: c.ID, Name: c.Name, Created: s.when(c.Created), Approved: "never", LastUsed: "never"}
			for i, u := range c.RedirectURIs {
				if p, err := url.Parse(u); err == nil {
					if i > 0 {
						v.Redirects += ", "
					}
					v.Redirects += p.Host
				}
			}
			if !c.Approved.IsZero() {
				v.Approved = s.when(c.Approved)
			}
			if !c.LastUsed.IsZero() {
				v.LastUsed = s.when(c.LastUsed)
			}
			views = append(views, v)
		}
		data["Connectors"] = views
		data["URL"] = s.OAuth.Resource()
	}
	if n := r.URL.Query().Get("revoked"); n != "" {
		data["Notice"] = "Revoked: " + n
	}
	s.render(w, "connectors", data)
}

func (s *Server) revokeConnector(w http.ResponseWriter, r *http.Request) {
	if s.OAuth == nil {
		s.fail(w, http.StatusNotFound, "MCP is disabled", errMCPDisabled)
		return
	}
	id := r.PathValue("id")
	if err := s.OAuth.RevokeClient(id); err != nil {
		s.fail(w, http.StatusNotFound, "Connector not found", err)
		return
	}
	http.Redirect(w, r, "/connectors?"+url.Values{"revoked": {id}}.Encode(), http.StatusSeeOther)
}
