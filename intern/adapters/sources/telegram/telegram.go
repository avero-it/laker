// Package telegram is the source that keeps the private chats and groups of a
// Telegram account: messages as Parquet rows, media as blobs, every message
// in the search index. It logs in with the user API (MTProto) as the user,
// with a session created by `laker-agent telegram login`. The first import
// of a chat goes through an export (takeout) session, as Telegram Desktop's
// "Export chat history"; later runs read what is new and what was edited.
// Channels and secret chats are not read; deletions are not seen.
package telegram

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/glob"
)

const Type = "telegram"

type Params struct {
	// APIID and APIHash identify the application: register your own at
	// https://my.telegram.org (API development tools).
	APIID   string `yaml:"apiid"`
	APIHash string `yaml:"apihash"`
	// Phone is the account, in international format; used by the login.
	Phone string `yaml:"phone"`
	// Session is the file of the logged-in session (default:
	// <datadir>/telegram-<adapter>.session). It gives full access to the
	// account: it never leaves this machine.
	Session string `yaml:"session"`
	// Chats and Exclude are patterns on chat titles (default: every chat).
	Chats   []string `yaml:"chats"`
	Exclude []string `yaml:"exclude"`
	// Media stores photos, documents, voice notes… up to MediaMaxSize bytes
	// (default 50 MB) and of the MediaTypes kinds.
	Media        *bool    `yaml:"media"`
	MediaMaxSize int64    `yaml:"mediamaxsize"`
	MediaTypes   []string `yaml:"mediatypes"`
	// Lookback is how far back edits are looked for at every run (default 7 days).
	Lookback time.Duration `yaml:"lookback"`
	// Since limits the first import to messages from this day (YYYY-MM-DD).
	Since string `yaml:"since"`
}

type resolved struct {
	APIID        int
	APIHash      string
	Phone        string
	Session      string
	Media        bool
	MediaMaxSize int64
	Lookback     time.Duration
}

var allTypes = []string{"photo", "video", "voice", "audio", "document"}

type Source struct {
	name        string
	p           resolved
	include     glob.Set
	exclude     glob.Set
	types       map[string]bool
	since       time.Time
	commitEvery int
	tmpDir      string
	logger      zerolog.Logger
	now         func() time.Time
	// open connects to Telegram and runs f with the api; tests replace it.
	open func(ctx context.Context, f func(ctx context.Context, tg api) error) error
}

var (
	_ out.Source = (*Source)(nil)
	_ out.Prober = (*Source)(nil)
)

func New(name string, params map[string]any, commitEvery int, dataDir, tmpDir string, logger zerolog.Logger) (*Source, error) {
	var p Params
	if err := config.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	id, err := strconv.Atoi(p.APIID)
	if err != nil || id <= 0 || p.APIHash == "" {
		return nil, fmt.Errorf("ERR248: apiid (a number) and apihash are required: register them at https://my.telegram.org")
	}
	r := resolved{APIID: id, APIHash: p.APIHash, Phone: p.Phone, Session: p.Session, Media: p.Media == nil || *p.Media,
		MediaMaxSize: p.MediaMaxSize, Lookback: p.Lookback}
	if r.Session == "" {
		r.Session = filepath.Join(dataDir, "telegram-"+name+".session")
	}
	if r.MediaMaxSize == 0 {
		r.MediaMaxSize = 50 << 20
	}
	if r.Lookback == 0 {
		r.Lookback = 7 * 24 * time.Hour
	}
	if len(p.Chats) == 0 {
		p.Chats = []string{"**"}
	}
	if len(p.MediaTypes) == 0 {
		p.MediaTypes = allTypes
	}
	types := map[string]bool{}
	for _, t := range p.MediaTypes {
		if !contains(allTypes, t) {
			return nil, fmt.Errorf("ERR249: media type %q is not one of %v", t, allTypes)
		}
		types[t] = true
	}
	s := &Source{
		name: name, p: r, include: glob.CompileAll(p.Chats), exclude: glob.CompileAll(p.Exclude), types: types,
		commitEvery: commitEvery, tmpDir: tmpDir, logger: logger, now: time.Now,
	}
	if p.Since != "" {
		if s.since, err = time.Parse("2006-01-02", p.Since); err != nil {
			return nil, fmt.Errorf("ERR24A: since %q must be YYYY-MM-DD", p.Since)
		}
	}
	s.open = s.openGotd
	return s, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Source) Run(ctx context.Context, rs out.RunSink) error {
	return s.open(ctx, func(ctx context.Context, tg api) error { return s.sync(ctx, tg, rs) })
}

// Probe checks the session and counts the chats the configuration selects.
func (s *Source) Probe(ctx context.Context) (domain.Probe, error) {
	var p domain.Probe
	err := s.open(ctx, func(ctx context.Context, tg api) error {
		self, err := tg.Self(ctx)
		if err != nil {
			return err
		}
		chats, err := tg.Dialogs(ctx)
		if err != nil {
			return fmt.Errorf("ERR24B: cannot list the chats: %w", err)
		}
		n := 0
		for _, c := range chats {
			if s.include.Match(c.Title) && !s.exclude.Match(c.Title) {
				n++
			}
		}
		p.Summary = fmt.Sprintf("logged in as %s, %d of %d private chats and groups selected", self, n, len(chats))
		return nil
	})
	return p, err
}

// Stub is a documented, disabled telegram adapter for `config generate`.
func Stub() config.GenAdapter {
	return config.GenAdapter{
		Name: "telegram", Type: Type, Schedule: "0 */30 * * * *",
		Comment: "Telegram private chats and groups. Register apiid/apihash at https://my.telegram.org, then\n" +
			"run laker-agent telegram login <name>. The first import is an export that Telegram may delay ~24 h.",
		Params: []config.GenParam{
			{Key: "apiid", Value: "${LAKER_TELEGRAM_API_ID}"},
			{Key: "apihash", Value: "${LAKER_TELEGRAM_API_HASH}"},
			{Key: "phone", Value: "+15550100000", Comment: "the account, international format"},
			{Key: "chats", Value: []string{"**"}, Comment: "patterns on chat titles"},
			{Key: "exclude", Value: []string{}},
			{Key: "media", Value: true},
			{Key: "mediamaxsize", Value: 52428800, Comment: "bytes; larger media are recorded, not stored"},
			{Key: "mediatypes", Value: allTypes},
			{Key: "lookback", Value: "168h", Comment: "how far back edits are looked for at every run"},
			{Key: "since", Value: "", Comment: "YYYY-MM-DD limits the first import"},
		},
	}
}
