package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// GenParam is one documented parameter of a generated adapter.
type GenParam struct {
	Key     string `json:"key"`
	Value   any    `json:"value"`
	Comment string `json:"comment,omitempty"`
}

// GenAdapter is an adapter written by `laker-agent config generate` or
// `config discover`, with comments.
type GenAdapter struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Enabled  bool   `json:"enabled"`
	Schedule string `json:"schedule"`
	// Heartbeat is written only when set.
	Heartbeat string     `json:"heartbeat,omitempty"`
	Comment   string     `json:"comment,omitempty"` // above the adapter
	Params    []GenParam `json:"params"`
}

// AgentDefaults guesses the agent section for this machine.
func AgentDefaults() AgentCommon {
	host, _ := os.Hostname()
	name := strings.Trim(regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(strings.Split(host, ".")[0]), "-"), "-")
	if !nameRE.MatchString(name) {
		name = "agent"
	}
	return AgentCommon{
		Name: name, Server: "https://laker.example.com", Timezone: localTimezone(), DataDir: defaultAgentDataDir(),
		Log:      Log{Use: "console"},
		Defaults: Defaults{Timeout: 2 * time.Hour, CommitEvery: 200},
	}
}

func localTimezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return strings.TrimPrefix(tz, ":")
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			return target[i+len("zoneinfo/"):]
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b))
	}
	if n := time.Local.String(); n != "" && n != "Local" {
		return n
	}
	return "UTC"
}

func defaultAgentDataDir() string {
	home, _ := os.UserHomeDir()
	switch {
	case runtime.GOOS == "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "laker-agent")
		}
	case runtime.GOOS == "darwin":
		return filepath.Join(home, "Library", "Application Support", "laker-agent")
	case os.Geteuid() == 0:
		return "/var/lib/laker-agent"
	}
	return filepath.Join(home, ".local", "share", "laker-agent")
}

func scalar(v any) *yaml.Node {
	n := &yaml.Node{}
	if err := n.Encode(v); err != nil {
		panic(err) // plain values only
	}
	if n.Kind == yaml.SequenceNode {
		n.Style = yaml.FlowStyle
	}
	return n
}

func key(k, comment string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: k, HeadComment: comment}
}

func mapping(pairs ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Content: pairs}
}

// RenderAgent writes a commented laker-agent configuration.
func RenderAgent(c AgentCommon, adapters []GenAdapter) []byte {
	agent := mapping(
		key("name", "how laker shows this agent; the identity is the key in datadir"), scalar(c.Name),
		key("server", "URL of laker serve: https unless on this machine"), scalar(c.Server),
		key("timezone", ""), scalar(c.Timezone),
		key("datadir", "the agent key (its identity: keep it), checkpoints, scratch files"), scalar(c.DataDir),
		key("log", "use: console | betterstack | stdout-json"), mapping(
			key("verbose", ""), scalar(c.Log.Verbose),
			key("use", ""), scalar(c.Log.Use),
			key("betterstack", ""), mapping(
				key("sourcetoken", ""), scalar(c.Log.Betterstack.SourceToken),
				key("ingestinghost", ""), scalar(c.Log.Betterstack.IngestingHost),
			),
		),
		key("defaults", ""), mapping(
			key("timeout", ""), scalar(shortDuration(c.Defaults.Timeout)),
			key("commitevery", "items per commit; lower it for very large mails"), scalar(c.Defaults.CommitEvery),
		),
	)

	list := &yaml.Node{Kind: yaml.SequenceNode}
	for _, a := range adapters {
		params := mapping()
		for _, p := range a.Params {
			params.Content = append(params.Content, key(p.Key, p.Comment), scalar(p.Value))
		}
		item := mapping(
			key("name", ""), scalar(a.Name),
			key("enabled", ""), scalar(a.Enabled),
			key("type", ""), scalar(a.Type),
			key("schedule", ""), scalar(a.Schedule),
		)
		if a.Heartbeat != "" {
			item.Content = append(item.Content, key("heartbeat", ""), scalar(a.Heartbeat))
		}
		item.Content = append(item.Content, key("params", ""), params)
		item.HeadComment = a.Comment
		list.Content = append(list.Content, item)
	}

	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{mapping(
		key("schemaversion", ""), scalar(AgentSchemaVersion),
		key("agent", ""), agent,
		key("adapters", "Adapter names are source names in the lake: unique across laker and all its agents.\n"+
			"Secrets are ${VARIABLES}: set them in the environment or in laker-agent.env next to this file.\n"+
			"schedule: 6-field cron with seconds (\"0 0 3 * * *\"), @every 10m, @hourly, or manual.\n"+
			"heartbeat (optional): address of an uptime monitor called after every run, with /fail when it failed."), list,
	)}}
	doc.HeadComment = "laker-agent configuration. Check it with: laker-agent config validate"

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		panic(fmt.Sprintf("render: %v", err)) // the nodes above are always valid
	}
	return buf.Bytes()
}

// shortDuration writes 2h for 2h0m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
