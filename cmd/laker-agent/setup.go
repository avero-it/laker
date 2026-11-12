package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/avero-it/laker/intern/adapters/discover"
	"github.com/avero-it/laker/intern/adapters/sources"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/pkg/config"
)

// The commands that help write laker-agent.yaml. They connect to the sources
// to check them, and send nothing to laker.

func commandGenerate(out string, force, withDiscovered bool) {
	if _, err := os.Stat(out); err == nil && !force {
		fatalf("ERR815: %s exists, use --force to overwrite it", out)
	}
	common := config.AgentDefaults()
	var adapters []config.GenAdapter
	for _, s := range sources.Stubs() {
		s.Name = common.Name + "-" + s.Name
		adapters = append(adapters, s)
	}
	if withDiscovered {
		for _, c := range discover.Run(context.Background(), discover.OSEnv(), common.Name) {
			adapters = append(adapters, c.Adapter)
		}
	}
	if err := os.WriteFile(out, config.RenderAgent(common, adapters), 0o600); err != nil {
		fatalf("ERR816: %v", err)
	}
	fmt.Printf("wrote %s: %d adapters, all disabled. Edit it, enable what you need, then: laker-agent config validate\n", out, len(adapters))
}

type probeResult struct {
	Adapter string       `json:"adapter"`
	Type    string       `json:"type"`
	Status  string       `json:"status"` // ok | failed | skipped
	Probe   domain.Probe `json:"probe"`
	Error   string       `json:"error,omitempty"`
}

func commandValidate(configPath string, offline, asJSON bool) {
	cfg := mustConfig(configPath) // the form: exits on any problem
	var results []probeResult
	failed := false
	for _, a := range cfg.Adapters {
		r := probeResult{Adapter: a.Name, Type: a.Type, Status: "skipped"}
		switch {
		case !a.Enabled:
			r.Probe.Summary = "disabled"
		case offline:
			r.Probe.Summary = "not probed (--offline)"
		default:
			p, err := sources.Probe(context.Background(), a, sources.Env{CommitEvery: 1, DataDir: cfg.Agent.DataDir, TmpDir: tmpDir(cfg)})
			r.Probe, r.Status = p, "ok"
			if err != nil {
				r.Status, r.Error, failed = "failed", err.Error(), true
			}
		}
		results = append(results, r)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
	} else {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ADAPTER\tTYPE\tSTATUS\tRESULT")
		for _, r := range results {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Adapter, r.Type, r.Status, r.Probe.Summary)
			for _, d := range r.Probe.Details {
				fmt.Fprintf(w, "\t\t\t  %s\n", d)
			}
			if r.Error != "" {
				fmt.Fprintf(w, "\t\t\t  %s\n", r.Error)
			}
		}
		w.Flush()
		fmt.Println("configuration is valid")
	}
	if failed {
		os.Exit(1)
	}
}

// prompter asks questions on the terminal (or on stdin with --interactive).
type prompter struct {
	in       *bufio.Reader
	terminal bool
}

func (p *prompter) ask(q string) string {
	fmt.Print(q)
	line, _ := p.in.ReadString('\n')
	return strings.TrimSpace(line)
}

func (p *prompter) yes(q string) bool {
	a := strings.ToLower(p.ask(q + " [y/N] "))
	return a == "y" || a == "yes"
}

// secret reads a value without echoing it on a terminal.
func (p *prompter) secret(q string) string {
	if !p.terminal {
		return p.ask(q)
	}
	fmt.Print(q)
	b, _ := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	return strings.TrimSpace(string(b))
}

var varRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// resolve turns a candidate into an adapter to probe, with its placeholders
// replaced by the given values or the environment.
func resolve(c config.GenAdapter, values map[string]string) config.Adapter {
	params := map[string]any{}
	for _, p := range c.Params {
		v := p.Value
		if s, ok := v.(string); ok {
			v = varRE.ReplaceAllStringFunc(s, func(m string) string {
				name := varRE.FindStringSubmatch(m)[1]
				if val, ok := values[name]; ok {
					return val
				}
				return os.Getenv(name)
			})
		}
		params[p.Key] = v
	}
	return config.Adapter{Name: c.Name, Type: c.Type, Enabled: true, Schedule: c.Schedule, Params: params}
}

// existing reads the configuration to extend, keeping its ${VAR} as written.
func existing(path string) (config.AgentCommon, []config.GenAdapter, bool) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return config.AgentDefaults(), nil, false
	}
	if err != nil {
		fatalf("ERR817: %v", err)
	}
	cfg, err := config.ParseAgentRaw(b)
	if err != nil {
		fatalf("ERR818: %s: %v", path, err)
	}
	var adapters []config.GenAdapter
	for _, a := range cfg.Adapters {
		g := config.GenAdapter{Name: a.Name, Type: a.Type, Enabled: a.Enabled, Schedule: a.Schedule, Heartbeat: a.Heartbeat}
		keys := make([]string, 0, len(a.Params))
		for k := range a.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			g.Params = append(g.Params, config.GenParam{Key: k, Value: a.Params[k]})
		}
		adapters = append(adapters, g)
	}
	return cfg.Agent, adapters, true
}

func commandDiscover(configPath, write string, asJSON, interactive bool) {
	if write == "" {
		if p, err := config.LocateAgent(configPath); err == nil {
			write = p
		} else {
			write = "laker-agent.yaml"
		}
	}
	common, current, exists := existing(write)
	candidates := discover.Run(context.Background(), discover.OSEnv(), common.Name)

	have := map[string]bool{}
	for _, a := range current {
		have[a.Name] = true
	}
	var fresh []discover.Candidate
	for _, c := range candidates {
		if !have[c.Adapter.Name] {
			fresh = append(fresh, c)
		}
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(fresh) // secret values are not serialized
		return
	}
	if len(fresh) == 0 {
		fmt.Println("nothing new found on this machine")
		return
	}

	terminal := term.IsTerminal(int(os.Stdin.Fd()))
	if !terminal && !interactive {
		// no one to ask: everything is added disabled, no secret is saved
		for _, c := range fresh {
			printCandidate(c)
			current = append(current, c.Adapter)
		}
		writeConfig(write, common, current, exists)
		fmt.Printf("\nadded %d adapters, disabled, to %s; no secret was saved. Run laker-agent config discover on a terminal to go through them.\n", len(fresh), write)
		return
	}

	p := &prompter{in: bufio.NewReader(os.Stdin), terminal: terminal}
	secrets := map[string]string{}
	added := 0
	for _, c := range fresh {
		fmt.Println()
		printCandidate(c)
		if !p.yes("add it?") {
			continue
		}
		values := map[string]string{}
		for _, s := range c.Secrets {
			switch {
			case s.Found() && p.yes(fmt.Sprintf("found %s in %s: save it in %s?", s.Masked(), s.Origin, envFileName)):
				values[s.Var] = s.Encoded()
			default:
				v := p.secret(fmt.Sprintf("value of %s (empty: set it later in %s or the environment): ", s.Var, envFileName))
				if v != "" {
					values[s.Var] = discover.Secret{Value: v, Quote: s.Quote}.Encoded()
				}
			}
		}
		a := resolve(c.Adapter, values)
		probe, err := sources.Probe(context.Background(), a, sources.Env{CommitEvery: 1, DataDir: common.DataDir, TmpDir: os.TempDir()})
		if err != nil {
			fmt.Printf("  check failed: %v\n", err)
		} else {
			fmt.Printf("  check ok: %s\n", probe.Summary)
			for _, d := range probe.Details {
				fmt.Printf("    %s\n", d)
			}
		}
		c.Adapter.Enabled = err == nil || p.yes("enable it anyway?")
		for k, v := range values {
			secrets[k] = v
		}
		current = append(current, c.Adapter)
		added++
	}
	if added == 0 {
		fmt.Println("\nnothing added")
		return
	}
	writeConfig(write, common, current, exists)
	if err := saveSecrets(envPath(write), secrets); err != nil {
		fatalf("ERR819: %v", err)
	}
	fmt.Printf("\nadded %d adapters to %s", added, write)
	if len(secrets) > 0 {
		fmt.Printf(", %d secrets to %s (mode 0600)", len(secrets), envPath(write))
	}
	fmt.Println(". Check them with: laker-agent config validate")
}

func printCandidate(c discover.Candidate) {
	fmt.Printf("%s (%s): %s\n", c.Adapter.Name, c.Adapter.Type, strings.TrimSuffix(c.Adapter.Comment, "."))
	for _, prm := range c.Adapter.Params {
		if s, ok := prm.Value.(string); ok && (prm.Key == "host" || prm.Key == "username" || prm.Key == "dsn") {
			fmt.Printf("  %s: %s\n", prm.Key, s)
		}
		if l, ok := prm.Value.([]string); ok && prm.Key == "paths" {
			fmt.Printf("  paths: %s\n", strings.Join(l, ", "))
		}
	}
	for _, s := range c.Secrets {
		if s.Found() {
			fmt.Printf("  credential found: %s (%s)\n", s.Masked(), s.Origin)
		} else {
			fmt.Printf("  credential needed: %s\n", s.Var)
		}
	}
	for _, n := range c.Notes {
		fmt.Printf("  note: %s\n", n)
	}
}

// writeConfig rewrites the configuration; the previous one is kept as .bak.
func writeConfig(path string, common config.AgentCommon, adapters []config.GenAdapter, exists bool) {
	if exists {
		if err := os.Rename(path, path+".bak"); err != nil {
			fatalf("ERR81A: %v", err)
		}
	}
	if err := os.WriteFile(path, config.RenderAgent(common, adapters), 0o600); err != nil {
		fatalf("ERR81B: %v", err)
	}
}
