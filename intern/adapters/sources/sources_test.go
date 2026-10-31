package sources

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/pkg/config"
)

// The generated configuration must decode strictly into the params of every
// source: a stub that drifts from its source fails here.
func TestGeneratedConfigIsValid(t *testing.T) {
	t.Setenv("LAKER_MAIL_PASSWORD", "x")
	t.Setenv("LAKER_PG_DSN", "postgres://u@db/erp")
	t.Setenv("LAKER_MYSQL_DSN", "u:p@tcp(db:3306)/blog")

	common := config.AgentDefaults()
	stubs := Stubs()
	for i := range stubs {
		stubs[i].Name = common.Name + "-" + stubs[i].Name
		stubs[i].Enabled = true // so their params are expanded and built below
	}
	b := config.RenderAgent(common, stubs)

	cfg, err := config.ParseAgent(b)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if err := config.ValidateAgent(cfg, Types); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if len(cfg.Adapters) != len(Types) {
		t.Errorf("%d adapters, want one per type (%d)", len(cfg.Adapters), len(Types))
	}
	for _, a := range cfg.Adapters {
		if _, err := Build(a, Env{CommitEvery: 1}, zerolog.Nop()); err != nil {
			t.Errorf("%s: %v", a.Name, err)
		}
	}
	for _, want := range []string{"# an app password", "# tls (993)", "laker-agent.env"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("comment %q missing:\n%s", want, b)
		}
	}
}
