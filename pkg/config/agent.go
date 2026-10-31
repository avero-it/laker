package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// AgentSchemaVersion is the shape of AgentConfig this build expects.
const AgentSchemaVersion = 1

// AgentConfig is the configuration of laker-agent: where laker serve is, and
// the adapters this agent runs near the data.
type AgentConfig struct {
	SchemaVersion int         `yaml:"schemaversion"`
	Agent         AgentCommon `yaml:"agent"`
	Adapters      []Adapter   `yaml:"adapters"`
}

type AgentCommon struct {
	// Name is how laker shows this agent; the identity is the key.
	Name string `yaml:"name"`
	// Server is the URL of laker serve, https unless on this machine.
	Server   string `yaml:"server"`
	Timezone string `yaml:"timezone"`
	// DataDir holds the agent key, the checkpoints and scratch files.
	DataDir  string   `yaml:"datadir"`
	Log      Log      `yaml:"log"`
	Defaults Defaults `yaml:"defaults"`
}

// AgentSearchPaths are tried in order when no file is given.
var AgentSearchPaths = []string{"/etc/laker-agent/config.yaml", "./laker-agent.yaml"}

func LocateAgent(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	for _, p := range AgentSearchPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("ERR740: no configuration file found in %s", strings.Join(AgentSearchPaths, ", "))
}

func LoadAgent(path string) (*AgentConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ERR741: unable to read the configuration file: %w", err)
	}
	return ParseAgent(b)
}

func ParseAgent(b []byte) (*AgentConfig, error) {
	cfg := &AgentConfig{}
	return cfg, parseInto(b, cfg)
}

// ParseAgentRaw decodes without expanding ${VAR}: for rewriting a file
// without writing its secrets into it.
func ParseAgentRaw(b []byte) (*AgentConfig, error) {
	cfg := &AgentConfig{}
	return cfg, decodeInto(b, cfg)
}

func ValidateAgent(cfg *AgentConfig, knownTypes map[string]bool) error {
	var e errs
	if cfg.SchemaVersion != AgentSchemaVersion {
		e.add(fmt.Errorf("ERR742: schemaversion is %d, this build expects %d", cfg.SchemaVersion, AgentSchemaVersion))
	}
	a := cfg.Agent
	if !nameRE.MatchString(a.Name) {
		e.add(fmt.Errorf("ERR743: agent.name %q must match %s", a.Name, nameRE))
	}
	if u, err := url.Parse(a.Server); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		e.add(fmt.Errorf("ERR744: agent.server %q must be an http(s) URL", a.Server))
	}
	if a.Timezone == "" {
		e.add(fmt.Errorf("ERR745: agent.timezone is required"))
	} else if _, err := time.LoadLocation(a.Timezone); err != nil {
		e.add(fmt.Errorf("ERR746: agent.timezone %q: %v", a.Timezone, err))
	}
	if a.DataDir == "" {
		e.add(fmt.Errorf("ERR747: agent.datadir is required"))
	}
	validateLog(&e, "agent", a.Log)
	if a.Defaults.Timeout <= 0 || a.Defaults.CommitEvery <= 0 {
		e.add(fmt.Errorf("ERR748: agent.defaults.timeout and commitevery must be positive"))
	}
	validateAdapters(&e, cfg.Adapters, knownTypes)
	return e.join()
}
