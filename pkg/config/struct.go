package config

import "time"

// SchemaVersion is the shape of Config this build expects. It goes up by one
// whenever a yaml key is added, renamed, removed or changes type.
const SchemaVersion = 4

// Config is the configuration of laker serve. laker reads no source itself:
// every adapter runs in a laker-agent (AgentConfig), which sends its data to
// the agent API of the web listener.
type Config struct {
	SchemaVersion int    `yaml:"schemaversion"`
	Common        Common `yaml:"common"`
}

type Common struct {
	ID ID `yaml:"id"`
	// Timezone is the IANA name of the zone schedules run in, e.g. "Europe/Rome".
	Timezone string `yaml:"timezone"`
	// DataDir holds what is derived from the lake and can be rebuilt from it:
	// the local state and the search index.
	DataDir  string   `yaml:"datadir"`
	Log      Log      `yaml:"log"`
	Lake     Lake     `yaml:"lake"`
	Snapshot Snapshot `yaml:"snapshot"`
	Web      Web      `yaml:"web"`
	MCP      MCP      `yaml:"mcp"`
}

// Web is the listener of `laker serve`: the browser interface, the API the
// CLI talks to while serve runs, and the agent API (/agent/v1/) the
// laker-agents send their data to. It is required.
type Web struct {
	Enabled bool   `yaml:"enabled"`
	Host    string `yaml:"host"` // 127.0.0.1 unless behind a TLS reverse proxy
	Port    int    `yaml:"port"`
	// Token is asked once by the login page (and sent by the CLI); at least
	// 16 characters.
	Token string `yaml:"token"`
	// PublicURL is the https address the listener is reached at through the
	// reverse proxy, e.g. "https://laker.example.com"; required by mcp.
	PublicURL string `yaml:"publicurl"`
}

// MCP serves the lake to MCP clients such as Claude connectors, read-only, at
// <publicurl>/mcp, behind OAuth: the owner approves each client with the web
// token.
type MCP struct {
	Enabled bool `yaml:"enabled"`
	// RedirectURIs are the only OAuth callbacks a client may register; empty
	// means Claude's (DefaultMCPRedirectURIs).
	RedirectURIs []string `yaml:"redirecturis"`
}

// DefaultMCPRedirectURIs are the OAuth callbacks of Claude's connectors
// (claude.ai, desktop and mobile apps).
var DefaultMCPRedirectURIs = []string{"https://claude.ai/api/mcp/auth_callback"}

// Snapshot copies the local state and search index to the lake (_state/,
// _index/), so a new host restores them instead of rebuilding.
type Snapshot struct {
	// Schedule as for adapters; "manual" only with `laker snapshot`.
	Schedule string `yaml:"schedule"`
}

type ID struct {
	Name string `yaml:"name"`
}

type Log struct {
	Verbose bool `yaml:"verbose"`
	// Use selects the logging sink: "console", "betterstack" or "stdout-json"
	// (see pkg/logger).
	Use         string      `yaml:"use"`
	Betterstack Betterstack `yaml:"betterstack"`
}

// Betterstack configures direct HTTPS log shipping. Only used when
// common.log.use is "betterstack".
type Betterstack struct {
	SourceToken string `yaml:"sourcetoken"`
	// IngestingHost is the source's ingestion URL; empty defaults to
	// "https://in.logs.betterstack.com".
	IngestingHost string `yaml:"ingestinghost"`
}

// Lake is where everything Laker ingests is written, append-only.
type Lake struct {
	// Use is "s3" (production) or "fs" (development).
	Use string `yaml:"use"`
	S3  S3     `yaml:"s3"`
	FS  FS     `yaml:"fs"`
}

type S3 struct {
	// Endpoint of an S3-compatible service, e.g. "https://fsn1.your-objectstorage.com".
	Endpoint  string `yaml:"endpoint"`
	Region    string `yaml:"region"`
	Bucket    string `yaml:"bucket"`
	AccessKey string `yaml:"accesskey"`
	SecretKey string `yaml:"secretkey"`
	PathStyle bool   `yaml:"pathstyle"`
	// ConditionalPut sends If-None-Match: * on every write, so the bucket
	// itself refuses to overwrite an object.
	ConditionalPut bool `yaml:"conditionalput"`
}

type FS struct {
	Path string `yaml:"path"`
}

// Defaults apply to every adapter of an agent.
type Defaults struct {
	Timeout     time.Duration `yaml:"timeout"`
	CommitEvery int           `yaml:"commitevery"`
}

// Adapter is one active ingestion of an agent: a source type, its parameters
// and when it runs.
type Adapter struct {
	Name    string `yaml:"name"`
	Enabled bool   `yaml:"enabled"`
	Type    string `yaml:"type"`
	// Schedule is a 6-field cron expression (with seconds), a descriptor such
	// as "@every 10m" or "@hourly", or "manual" (only `laker-agent run`).
	Schedule string `yaml:"schedule"`
	// Heartbeat, optional, is the address of an uptime monitor (Better Stack
	// heartbeat, healthchecks.io) called after every run: as it is when the
	// run succeeded, with /fail appended when it failed. The monitor then
	// warns when the source fails or stops running.
	Heartbeat string         `yaml:"heartbeat"`
	Params    map[string]any `yaml:"params"`
}
