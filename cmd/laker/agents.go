package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"
	"time"

	"github.com/avero-it/laker/intern/adapters/state/bbolt"
	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
)

func commandAgentsList(configPath string) {
	cfg := mustConfig(configPath)
	var list []domain.Agent
	rm := newRemote(cfg)
	err := errNotServing
	if rm != nil {
		err = rm.get(context.Background(), "/api/agents", nil, &list)
	}
	if errors.Is(err, errNotServing) {
		state, oerr := bbolt.Open(statePath(cfg), cliWait)
		if oerr != nil {
			fatalf("ERR0B0: %v", oerr)
		}
		defer state.Close()
		list, err = core.ListAgents(state)
	}
	if err != nil {
		fatalf("ERR0B1: %v", err)
	}

	loc, _ := time.LoadLocation(cfg.Common.Timezone)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "ID\tNAME\tHOST\tSTATUS\tLAST SEEN")
	for _, a := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.Name, a.Hostname, a.Status, a.LastSeen.In(loc).Format("2006-01-02 15:04"))
	}
}

func commandAgentsSet(configPath, id string, status domain.AgentStatus) {
	cfg := mustConfig(configPath)
	action := map[domain.AgentStatus]string{domain.AgentApproved: "approve", domain.AgentRevoked: "revoke"}[status]
	if rm := newRemote(cfg); rm != nil {
		err := rm.post(context.Background(), "/api/agents/"+url.PathEscape(id)+"/"+action)
		if err == nil {
			fmt.Printf("%s: %s\n", id, status)
			return
		}
		if !errors.Is(err, errNotServing) {
			fatalf("ERR0B2: %v", err)
		}
	}
	state, err := bbolt.Open(statePath(cfg), cliWait)
	if err != nil {
		fatalf("ERR0B3: %v", err)
	}
	defer state.Close()
	if err := core.SetAgentStatus(state, id, status); err != nil {
		fatalf("ERR0B4: %s: %v", id, err)
	}
	fmt.Printf("%s: %s\n", id, status)
}

// post sends a bodiless POST to the API of serve.
func (r *remote) post(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+path, nil)
	if err != nil {
		return fmt.Errorf("ERR0B5: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		if isDial(err) {
			return errNotServing
		}
		return fmt.Errorf("ERR0B6: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ERR0B7: laker serve answered %s", resp.Status)
	}
	return nil
}
