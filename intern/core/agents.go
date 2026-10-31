package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

var (
	ErrUnknownAgent = errors.New("ERR150: unknown agent")
	ErrNotOwner     = errors.New("ERR151: the source belongs to someone else")
)

// GetAgent reads an agent, nil if unknown.
func GetAgent(state out.StateStore, id string) (*domain.Agent, error) {
	b, err := state.Get(domain.BucketAgents, id)
	if err != nil || b == nil {
		return nil, err
	}
	var a domain.Agent
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("ERR152: corrupt agent %s: %w", id, err)
	}
	return &a, nil
}

func putAgent(state out.StateStore, a domain.Agent) error {
	return state.Apply([]out.StateChange{{Bucket: domain.BucketAgents, Key: a.ID, Value: domain.MustJSON(a)}})
}

// Hello registers an agent that presents itself: a new one waits to be
// approved; a known one gets its name, host and version refreshed.
func Hello(state out.StateStore, a domain.Agent, now time.Time) (domain.Agent, error) {
	known, err := GetAgent(state, a.ID)
	if err != nil {
		return a, err
	}
	if known == nil {
		a.Status, a.FirstSeen, a.LastSeen = domain.AgentPending, now, now
		return a, putAgent(state, a)
	}
	known.Name, known.Hostname, known.Version, known.LastSeen = a.Name, a.Hostname, a.Version, now
	return *known, putAgent(state, *known)
}

// Touch records that an approved agent was active.
func Touch(state out.StateStore, a domain.Agent, now time.Time) error {
	if now.Sub(a.LastSeen) < time.Minute {
		return nil
	}
	a.LastSeen = now
	return putAgent(state, a)
}

// SetAgentStatus approves or revokes an agent.
func SetAgentStatus(state out.StateStore, id string, status domain.AgentStatus) error {
	a, err := GetAgent(state, id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrUnknownAgent
	}
	a.Status = status
	return putAgent(state, *a)
}

func ListAgents(state out.StateStore) ([]domain.Agent, error) {
	var out []domain.Agent
	err := state.ForEach(domain.BucketAgents, "", func(_ string, v []byte) error {
		var a domain.Agent
		if err := json.Unmarshal(v, &a); err != nil {
			return err
		}
		out = append(out, a)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeen.Before(out[j].FirstSeen) })
	return out, err
}

// Claim makes owner the writer of source, if nobody else is.
func Claim(state out.StateStore, source string, owner domain.Owner) error {
	b, err := state.Get(domain.BucketOwners, source)
	if err != nil {
		return err
	}
	if b != nil {
		var cur domain.Owner
		if err := json.Unmarshal(b, &cur); err != nil {
			return fmt.Errorf("ERR153: corrupt owner of %s: %w", source, err)
		}
		if cur.Agent != owner.Agent {
			return fmt.Errorf("%w: %s is written by %s", ErrNotOwner, source, cur.Agent)
		}
		if cur.Type == owner.Type {
			return nil
		}
	}
	return state.Apply([]out.StateChange{{Bucket: domain.BucketOwners, Key: source, Value: domain.MustJSON(owner)}})
}

// Owners maps every source to who writes it.
func Owners(state out.StateStore) (map[string]domain.Owner, error) {
	out := map[string]domain.Owner{}
	err := state.ForEach(domain.BucketOwners, "", func(k string, v []byte) error {
		var o domain.Owner
		if err := json.Unmarshal(v, &o); err != nil {
			return err
		}
		out[k] = o
		return nil
	})
	return out, err
}

// ValidateCommit checks that a commit of an agent writes only where its
// source lives in the lake and indexes only documents of that source: an
// agent can never write over another source.
func ValidateCommit(c domain.Commit) error {
	src := c.Adapter
	if !validName(src) {
		return fmt.Errorf("ERR154: invalid source name %q", src)
	}
	prefixes := []string{
		"mail/messages/account=" + src + "/",
		"files/inventory/source=" + src + "/",
		"files/text/source=" + src + "/",
		"db/" + src + "/",
	}
	for _, f := range c.Files {
		if err := domain.ValidKey(f.Key); err != nil {
			return err
		}
		ok := false
		for _, p := range prefixes {
			if strings.HasPrefix(f.Key, p) && strings.HasSuffix(f.Key, ".parquet") {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("ERR155: %s is outside the source %s", f.Key, src)
		}
	}
	for _, d := range c.Mail {
		if _, err := domain.BlobKey(d.SHA256); err != nil {
			return err
		}
	}
	for _, d := range c.FileDocs {
		if d.Source != src {
			return fmt.Errorf("ERR156: document of source %s in a commit of %s", d.Source, src)
		}
	}
	for _, id := range c.Unindex {
		if !strings.HasPrefix(id, "file:"+src+":") {
			return fmt.Errorf("ERR157: cannot unindex %s from %s", id, src)
		}
	}
	return nil
}

func validName(s string) bool {
	if len(s) < 2 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0) {
			return false
		}
	}
	return true
}
