package domain

import "time"

type AgentStatus string

const (
	AgentPending  AgentStatus = "pending"
	AgentApproved AgentStatus = "approved"
	AgentRevoked  AgentStatus = "revoked"
)

// Agent is a laker-agent known to laker serve.
type Agent struct {
	ID        string      `json:"id"`
	PublicKey string      `json:"public_key"` // base64 Ed25519
	Name      string      `json:"name"`
	Hostname  string      `json:"hostname"`
	Version   string      `json:"version"`
	Status    AgentStatus `json:"status"`
	FirstSeen time.Time   `json:"first_seen"`
	LastSeen  time.Time   `json:"last_seen"`
}

// Owner is who writes a source: an agent, or laker serve itself (LocalOwner).
type Owner struct {
	Agent string `json:"agent"`
	Type  string `json:"type"`
}

const LocalOwner = "local"

const (
	BucketAgents = "_agents"
	BucketOwners = "_owners"
)
