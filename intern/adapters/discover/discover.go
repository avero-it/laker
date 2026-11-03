// Package discover looks, read-only and locally, for what a laker-agent could
// ingest on this machine: mail accounts of the mail clients, document folders,
// network shares, Postgres and MySQL databases (also in Docker containers).
// It reads configuration files and asks local ports and the Docker socket;
// it sends nothing anywhere. Credentials it finds are returned as Secrets,
// for the caller to propose one by one: they are never written by this
// package.
package discover

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/avero-it/laker/pkg/config"
)

// Env is the machine as the detectors see it; tests replace it.
type Env struct {
	Home     string
	GOOS     string
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	Glob     func(string) ([]string, error)
	Stat     func(string) (os.FileInfo, error)
	// Dial reaches local ports and the Docker socket.
	Dial         func(ctx context.Context, network, addr string) (net.Conn, error)
	DockerSocket string
}

// OSEnv is the real machine.
func OSEnv() Env {
	home, _ := os.UserHomeDir()
	d := &net.Dialer{Timeout: time.Second}
	return Env{
		Home: home, GOOS: runtime.GOOS, Getenv: os.Getenv, ReadFile: os.ReadFile,
		Glob: filepath.Glob, Stat: os.Stat, Dial: d.DialContext, DockerSocket: "/var/run/docker.sock",
	}
}

// Secret is a credential an adapter needs. Value is what was found ("" if
// nothing was: the user types it or sets Var). Origin says where it was seen.
type Secret struct {
	Var    string `json:"var"`
	Origin string `json:"origin"`
	Value  string `json:"-"`
	// Quote is how the value goes into its parameter: "" as it is, "libpq"
	// for a single-quoted value in a Postgres key=value DSN.
	Quote string `json:"-"`
}

// Found tells that a value was found.
func (s Secret) Found() bool { return s.Value != "" }

// Masked shows the shape of a found value without giving it away.
func (s Secret) Masked() string {
	if len(s.Value) <= 2 {
		return strings.Repeat("*", len(s.Value))
	}
	return s.Value[:1] + strings.Repeat("*", min(len(s.Value)-2, 8)) + s.Value[len(s.Value)-1:]
}

// Encoded is the value as it must be stored for its parameter.
func (s Secret) Encoded() string {
	if s.Quote == "libpq" {
		return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s.Value)
	}
	return s.Value
}

// Candidate is a source this machine seems to have.
type Candidate struct {
	Adapter config.GenAdapter `json:"adapter"`
	Secrets []Secret          `json:"secrets,omitempty"`
	// Key identifies what the candidate reads (host, port, database, path),
	// to merge what two detectors found.
	Key   string   `json:"-"`
	Notes []string `json:"notes,omitempty"`
}

type detector func(ctx context.Context, env Env, prefix string) []Candidate

var detectors = []detector{thunderbird, evolution, folders, shares, postgres, mysql, docker, telegram, whatsapp}

// Run asks every detector; adapter names start with prefix (the agent name)
// and are unique.
func Run(ctx context.Context, env Env, prefix string) []Candidate {
	var all []Candidate
	byKey := map[string]int{}
	for _, d := range detectors {
		for _, c := range d(ctx, env, prefix) {
			if i, ok := byKey[c.Key]; ok && c.Key != "" {
				all[i] = merge(all[i], c)
				continue
			}
			byKey[c.Key] = len(all)
			all = append(all, c)
		}
	}
	used := map[string]int{}
	for i := range all {
		name := all[i].Adapter.Name
		if n := used[name]; n > 0 {
			all[i].Adapter.Name = fmt.Sprintf("%s-%d", name, n+1)
			for j := range all[i].Secrets {
				all[i].Secrets[j].Var = fmt.Sprintf("%s_%d", all[i].Secrets[j].Var, n+1)
			}
			replaceVars(&all[i])
		}
		used[name]++
	}
	return all
}

// merge keeps the first candidate and fills in what the second knows: a found
// secret, more notes.
func merge(a, b Candidate) Candidate {
	for i := range a.Secrets {
		if !a.Secrets[i].Found() {
			for _, s := range b.Secrets {
				if s.Found() {
					a.Secrets[i].Value, a.Secrets[i].Origin, a.Secrets[i].Quote = s.Value, s.Origin, s.Quote
				}
			}
		}
	}
	a.Notes = append(a.Notes, b.Notes...)
	return a
}

var placeholderRE = regexp.MustCompile(`\$\{[A-Z0-9_]+\}`)

// replaceVars points the placeholders of the params at the current Secret vars.
func replaceVars(c *Candidate) {
	for i, p := range c.Adapter.Params {
		s, ok := p.Value.(string)
		if !ok || len(c.Secrets) == 0 {
			continue
		}
		c.Adapter.Params[i].Value = placeholderRE.ReplaceAllString(s, "${"+c.Secrets[0].Var+"}")
	}
}

var nonName = regexp.MustCompile(`[^a-z0-9]+`)

// name builds an adapter name: prefix-kind-hint, within the name rules.
func name(prefix, kind, hint string) string {
	n := strings.Trim(nonName.ReplaceAllString(strings.ToLower(hint), "-"), "-")
	full := strings.Trim(prefix+"-"+kind+"-"+n, "-")
	if len(full) > 63 {
		full = strings.TrimRight(full[:63], "-")
	}
	return full
}

// varName is the environment variable of a secret of adapter.
func varName(adapter, what string) string {
	return "LAKER_" + strings.ToUpper(nonName.ReplaceAllString(adapter, "_")) + "_" + what
}

// reachable tells that something listens on a local TCP port.
func reachable(ctx context.Context, env Env, port int) bool {
	c, err := env.Dial(ctx, "tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		return false
	}
	c.Close()
	return true
}
