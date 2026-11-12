package config

import (
	"strings"
	"testing"
)

var types = map[string]bool{"imap": true}

func TestExampleIsValid(t *testing.T) {
	t.Setenv("LAKER_S3_ACCESS_KEY", "ak")
	t.Setenv("LAKER_S3_SECRET_KEY", "sk")
	t.Setenv("LAKER_WEB_TOKEN", "0123456789abcdef")

	cfg, err := Parse(Example)
	if err != nil {
		t.Fatalf("Parse(example) error = %v", err)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate(example) error = %v", err)
	}
	if cfg.Common.Lake.S3.AccessKey != "ak" || cfg.Common.Lake.S3.SecretKey != "sk" {
		t.Errorf("secrets not expanded: %+v", cfg.Common.Lake.S3)
	}
}

// laker reads no source: an old configuration with adapters, or with the
// removed agents and defaults sections, is refused, not silently ignored.
func TestLakerHasNoAdapters(t *testing.T) {
	for _, extra := range []string{
		"adapters: []\n",
		"common:\n  agents:\n    enabled: true\n",
		"common:\n  defaults:\n    timeout: 2h\n",
	} {
		if _, err := Parse([]byte("schemaversion: 4\n" + extra)); err == nil {
			t.Errorf("accepted:\n%s", extra)
		}
	}
}

func TestMissingEnvIsAnError(t *testing.T) {
	_, err := Parse(Example)
	if err == nil || !strings.Contains(err.Error(), "LAKER_S3_ACCESS_KEY") {
		t.Fatalf("error = %v, want it to name LAKER_S3_ACCESS_KEY", err)
	}
}

func TestExtraneousKeyIsAnError(t *testing.T) {
	_, err := Parse([]byte("schemaversion: 1\ncommon:\n  id:\n    name: x\n    nickname: y\n"))
	if err == nil || !strings.Contains(err.Error(), "nickname") {
		t.Fatalf("error = %v, want it to name the extraneous key", err)
	}
}

func TestParamsAreExpanded(t *testing.T) {
	t.Setenv("IMAP_PASS", "s3cr$t")
	cfg, err := ParseAgent([]byte(`
adapters:
  - name: mail
    enabled: true
    params:
      password: ${IMAP_PASS}
      folders: ["${IMAP_PASS}", INBOX]
      nested: { a: "x-${IMAP_PASS}" }
`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Adapters[0].Params
	if p["password"] != "s3cr$t" {
		t.Errorf("password = %v", p["password"])
	}
	if f := p["folders"].([]any); f[0] != "s3cr$t" || f[1] != "INBOX" {
		t.Errorf("folders = %v", f)
	}
	if n := p["nested"].(map[string]any); n["a"] != "x-s3cr$t" {
		t.Errorf("nested = %v", n)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := &Config{
		SchemaVersion: SchemaVersion,
		Common: Common{
			ID: ID{Name: "laker"}, Timezone: "Mars/Olympus", DataDir: "/tmp/x",
			Log:      Log{Use: "console"},
			Lake:     Lake{Use: "s3", S3: S3{Endpoint: "nope"}},
			Snapshot: Snapshot{Schedule: ScheduleManual},
		},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, code := range []string{"ERR713", "ERR717", "ERR718", "ERR794"} {
		if !strings.Contains(err.Error(), code) {
			t.Errorf("missing %s in:\n%v", code, err)
		}
	}
}

func TestValidateAgentReportsEveryAdapterProblem(t *testing.T) {
	cfg, err := ParseAgent(RenderAgent(AgentDefaults(), nil))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Adapters = []Adapter{
		{Name: "mail", Type: "imap", Schedule: "@every 10m"},
		{Name: "mail", Type: "pop3", Schedule: "every day"},
		{Name: "files", Type: "imap", Schedule: "manual", Heartbeat: "uptime.example.com/secret-token"},
		{Name: "docs", Type: "imap", Schedule: "manual", Heartbeat: "https://uptime.example.com/api/v1/heartbeat/abc"},
	}
	err = ValidateAgent(cfg, types)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, code := range []string{"ERR71E", "ERR71F", "ERR720", "ERR728"} {
		if !strings.Contains(err.Error(), code) {
			t.Errorf("missing %s in:\n%v", code, err)
		}
	}
	// one bad heartbeat, reported without its address (it holds a token)
	if strings.Count(err.Error(), "ERR728") != 1 || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("heartbeat errors:\n%v", err)
	}
}

func TestManualScheduleIsValid(t *testing.T) {
	cfg, err := ParseAgent(RenderAgent(AgentDefaults(), nil))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Adapters = []Adapter{{Name: "mail", Type: "imap", Schedule: ScheduleManual}}
	if err := ValidateAgent(cfg, types); err != nil {
		t.Fatal(err)
	}
}

func TestMCPNeedsAnHTTPSPublicURL(t *testing.T) {
	t.Setenv("LAKER_S3_ACCESS_KEY", "ak")
	t.Setenv("LAKER_S3_SECRET_KEY", "sk")
	t.Setenv("LAKER_WEB_TOKEN", "0123456789abcdef")
	cfg, _ := Parse(Example)
	cfg.Common.MCP = MCP{Enabled: true, RedirectURIs: []string{"http://localhost/cb"}}
	err := Validate(cfg)
	for _, code := range []string{"ERR78A", "ERR78B"} {
		if err == nil || !strings.Contains(err.Error(), code) {
			t.Errorf("missing %s in: %v", code, err)
		}
	}
	cfg.Common.Web.PublicURL = "http://laker.example.com/sub"
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "ERR788") {
		t.Errorf("bad publicurl accepted: %v", err)
	}
	cfg.Common.Web.PublicURL = "https://laker.example.com"
	cfg.Common.MCP.RedirectURIs = nil
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestAgentDefaultsAreValid(t *testing.T) {
	cfg, err := ParseAgent(RenderAgent(AgentDefaults(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAgent(cfg, types); err != nil {
		t.Fatal(err)
	}
	cfg.Agent.Server = "ftp://x"
	if err := ValidateAgent(cfg, types); err == nil || !strings.Contains(err.Error(), "ERR744") {
		t.Errorf("bad server accepted: %v", err)
	}
}

func TestDisabledAdaptersNeedNoVariable(t *testing.T) {
	cfg, err := ParseAgent([]byte("adapters:\n  - name: off\n    enabled: false\n    params:\n      password: ${NOT_SET_ANYWHERE}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Adapters[0].Params["password"] != "${NOT_SET_ANYWHERE}" {
		t.Errorf("disabled params: %v", cfg.Adapters[0].Params)
	}
	if _, err := ParseAgent([]byte("adapters:\n  - name: on\n    enabled: true\n    params:\n      password: ${NOT_SET_ANYWHERE}\n")); err == nil {
		t.Error("an enabled adapter with a missing variable was accepted")
	}
}
