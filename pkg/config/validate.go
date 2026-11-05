package config

import (
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/robfig/cron/v3"
)

// ScheduleManual marks an adapter that only runs with `laker run`.
const ScheduleManual = "manual"

// CronParser parses adapter schedules: 6 fields with seconds, or a descriptor.
var CronParser = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// Validate checks what decoding cannot: required values, allowed choices,
// schedules, and that every adapter type is one this build knows. It reports
// every problem, not just the first.
func Validate(cfg *Config, knownTypes map[string]bool) error {
	var e errs

	if cfg.SchemaVersion != SchemaVersion {
		e.add(fmt.Errorf("ERR710: schemaversion is %d, this build expects %d", cfg.SchemaVersion, SchemaVersion))
	}

	c := cfg.Common
	if !nameRE.MatchString(c.ID.Name) {
		e.add(fmt.Errorf("ERR711: common.id.name %q must match %s", c.ID.Name, nameRE))
	}
	if c.Timezone == "" {
		e.add(fmt.Errorf("ERR712: common.timezone is required"))
	} else if _, err := time.LoadLocation(c.Timezone); err != nil {
		e.add(fmt.Errorf("ERR713: common.timezone %q: %v", c.Timezone, err))
	}
	if c.DataDir == "" {
		e.add(fmt.Errorf("ERR714: common.datadir is required"))
	}

	validateLog(&e, "common", c.Log)

	switch c.Lake.Use {
	case "s3":
		s := c.Lake.S3
		if u, err := url.Parse(s.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
			e.add(fmt.Errorf("ERR717: common.lake.s3.endpoint %q must be an absolute URL", s.Endpoint))
		}
		for key, val := range map[string]string{
			"region": s.Region, "bucket": s.Bucket, "accesskey": s.AccessKey, "secretkey": s.SecretKey,
		} {
			if val == "" {
				e.add(fmt.Errorf("ERR718: common.lake.s3.%s is required", key))
			}
		}
	case "fs":
		if c.Lake.FS.Path == "" {
			e.add(fmt.Errorf("ERR719: common.lake.fs.path is required"))
		}
	default:
		e.add(fmt.Errorf("ERR71A: common.lake.use %q must be s3 or fs", c.Lake.Use))
	}

	if c.Snapshot.Schedule != ScheduleManual {
		if _, err := CronParser.Parse(c.Snapshot.Schedule); err != nil {
			e.add(fmt.Errorf("ERR723: common.snapshot.schedule %q: %v", c.Snapshot.Schedule, err))
		}
	}
	if w := c.Web; w.Enabled {
		if len(w.Token) < 16 {
			e.add(fmt.Errorf("ERR724: common.web.token must be at least 16 characters"))
		}
		if w.Port < 1 || w.Port > 65535 {
			e.add(fmt.Errorf("ERR725: common.web.port %d is not a valid port", w.Port))
		}
		if w.Host == "" {
			e.add(fmt.Errorf("ERR726: common.web.host is required"))
		}
	}
	if c.Web.PublicURL != "" {
		if u, err := url.Parse(c.Web.PublicURL); err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			e.add(fmt.Errorf("ERR788: common.web.publicurl %q must be an https URL with no path, e.g. https://laker.example.com", c.Web.PublicURL))
		}
	}
	if c.MCP.Enabled {
		if !c.Web.Enabled {
			e.add(fmt.Errorf("ERR789: common.mcp needs common.web: MCP is served by its listener"))
		}
		if c.Web.PublicURL == "" {
			e.add(fmt.Errorf("ERR78A: common.mcp needs common.web.publicurl: OAuth clients reach Laker at that address"))
		}
		for _, r := range c.MCP.RedirectURIs {
			if u, err := url.Parse(r); err != nil || u.Scheme != "https" || u.Host == "" || u.Fragment != "" {
				e.add(fmt.Errorf("ERR78B: common.mcp.redirecturis: %q must be an absolute https URL", r))
			}
		}
	}
	if c.Agents.Enabled && !c.Web.Enabled {
		e.add(fmt.Errorf("ERR727: common.agents needs common.web: the agents send their data to its listener"))
	}
	if c.Defaults.Timeout <= 0 {
		e.add(fmt.Errorf("ERR71B: common.defaults.timeout must be positive"))
	}
	if c.Defaults.CommitEvery <= 0 {
		e.add(fmt.Errorf("ERR71C: common.defaults.commitevery must be positive"))
	}

	validateAdapters(&e, cfg.Adapters, knownTypes)

	return e.join()
}

func validateLog(e *errs, where string, l Log) {
	switch l.Use {
	case "console", "stdout-json":
	case "betterstack":
		if l.Betterstack.SourceToken == "" {
			e.add(fmt.Errorf("ERR715: %s.log.betterstack.sourcetoken is required when %s.log.use is betterstack", where, where))
		}
	default:
		e.add(fmt.Errorf("ERR716: %s.log.use %q must be console, betterstack or stdout-json", where, l.Use))
	}
}

func validateAdapters(e *errs, adapters []Adapter, knownTypes map[string]bool) {
	seen := map[string]bool{}
	for i, a := range adapters {
		where := fmt.Sprintf("adapters[%d] (%s)", i, a.Name)
		if !nameRE.MatchString(a.Name) {
			e.add(fmt.Errorf("ERR71D: %s: name must match %s", where, nameRE))
		}
		if seen[a.Name] {
			e.add(fmt.Errorf("ERR71E: %s: name is used twice", where))
		}
		seen[a.Name] = true
		if !knownTypes[a.Type] {
			e.add(fmt.Errorf("ERR71F: %s: unknown type %q", where, a.Type))
		}
		if a.Schedule != ScheduleManual {
			if _, err := CronParser.Parse(a.Schedule); err != nil {
				e.add(fmt.Errorf("ERR720: %s: schedule %q: %v", where, a.Schedule, err))
			}
		}
	}
}
