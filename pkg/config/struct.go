package config

import "time"

// SchemaVersion is the shape of Config this build expects. It goes up by one
// whenever a yaml key is added, renamed, removed or changes type.
const SchemaVersion = 1

type Config struct {
	SchemaVersion int       `yaml:"schemaversion"`
	Common        Common    `yaml:"common"`
	Adapters      []Adapter `yaml:"adapters"`
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
	Agents   Agents   `yaml:"agents"`
	Defaults Defaults `yaml:"defaults"`
}

// Agents lets laker-agent processes send data to this laker, through the web
// listener (/agent/v1/). Each agent waits for approval on its first contact.
type Agents struct {
	Enabled bool `yaml:"enabled"`
}

// Web is the browser interface served by `laker serve`; the CLI also talks to
// it while serve runs.
type Web struct {
	Enabled bool   `yaml:"enabled"`
	Host    string `yaml:"host"` // 127.0.0.1 unless behind a TLS reverse proxy
	Port    int    `yaml:"port"`
	// Token is asked once by the login page (and sent by the CLI); at least
	// 16 characters.
	Token string `yaml:"token"`
}

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

// Defaults apply to every adapter.
type Defaults struct {
	Timeout     time.Duration `yaml:"timeout"`
	CommitEvery int           `yaml:"commitevery"`
}

// Adapter is one active ingestion: a source type, its parameters and when it runs.
type Adapter struct {
	Name    string `yaml:"name"`
	Enabled bool   `yaml:"enabled"`
	Type    string `yaml:"type"`
	// Schedule is a 6-field cron expression (with seconds), a descriptor such
	// as "@every 10m" or "@hourly", or "manual" (only `laker run`).
	Schedule string         `yaml:"schedule"`
	Params   map[string]any `yaml:"params"`
}
