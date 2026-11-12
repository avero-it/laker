package web

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
)

var statusText = map[domain.AgentStatus]string{
	domain.AgentPending: "pending", domain.AgentApproved: "approved", domain.AgentRevoked: "revoked",
}

type agentView struct {
	ID, Name, Hostname, Version, LastSeen, Sources, StatusText string
	Status                                                     domain.AgentStatus
}

// agentSources are the sources written by agents, by agent id.
func (s *Server) agentSources() (map[string][]string, map[string]domain.Owner) {
	owners, _ := core.Owners(s.State)
	by := map[string][]string{}
	for src, o := range owners {
		if o.Agent != domain.LocalOwner {
			by[o.Agent] = append(by[o.Agent], src)
		}
	}
	for _, v := range by {
		sort.Strings(v)
	}
	return by, owners
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	list, err := core.ListAgents(s.State)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "Cannot read the agents", err)
		return
	}
	sources, _ := s.agentSources()
	var views []agentView
	for _, a := range list {
		views = append(views, agentView{ID: a.ID, Name: a.Name, Hostname: a.Hostname, Version: a.Version,
			LastSeen: s.when(a.LastSeen), Sources: strings.Join(sources[a.ID], ", "),
			Status: a.Status, StatusText: statusText[a.Status]})
	}
	data := map[string]any{"Title": "Agents", "Agents": views}
	switch q := r.URL.Query(); {
	case q.Get("approved") != "":
		data["Notice"] = "Approved: " + q.Get("approved")
	case q.Get("revoked") != "":
		data["Notice"] = "Revoked: " + q.Get("revoked")
	}
	s.render(w, "agents", data)
}

func (s *Server) setAgent(r *http.Request) (string, domain.AgentStatus, error) {
	id := r.PathValue("id")
	status := map[string]domain.AgentStatus{"approve": domain.AgentApproved, "revoke": domain.AgentRevoked}[r.PathValue("action")]
	if status == "" {
		return id, "", errors.New("ERR40E: unknown action")
	}
	if err := core.SetAgentStatus(s.State, id, status); err != nil {
		return id, "", err
	}
	s.Logger.Info().Str("agent", id).Str("status", string(status)).Msg("INF403: agent status changed")
	return id, status, nil
}

func (s *Server) agentAction(w http.ResponseWriter, r *http.Request) {
	id, status, err := s.setAgent(r)
	if err != nil {
		s.fail(w, http.StatusNotFound, "Agent not found", err)
		return
	}
	http.Redirect(w, r, "/agents?"+url.Values{string(map[domain.AgentStatus]string{
		domain.AgentApproved: "approved", domain.AgentRevoked: "revoked"}[status]): {id}}.Encode(), http.StatusSeeOther)
}

func (s *Server) apiAgents(w http.ResponseWriter, r *http.Request) {
	list, err := core.ListAgents(s.State)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) apiAgentAction(w http.ResponseWriter, r *http.Request) {
	id, status, err := s.setAgent(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, apiError{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": string(status)})
}
