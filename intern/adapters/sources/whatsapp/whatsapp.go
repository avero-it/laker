// Package whatsapp is the source that keeps the private chats and groups of a
// WhatsApp account: messages as Parquet rows, media as blobs, every message
// in the search index. The agent is a linked device of the phone (as
// WhatsApp Web), paired by `laker-agent whatsapp login`. The phone sends the
// history once, at pairing; then every run receives what arrived since the
// last one: new messages, edits and deletions (recorded as new rows, never
// removed). WhatsApp unlinks a device that stays offline for about two weeks.
// Status updates, channels and broadcast lists are not read.
package whatsapp

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/glob"
)

const Type = "whatsapp"

type Params struct {
	// Session is the database of the linked device (default:
	// <datadir>/whatsapp-<adapter>.db). It gives access to the account: it
	// never leaves this machine.
	Session string `yaml:"session"`
	// Chats and Exclude are patterns on chat names (default: every chat).
	Chats   []string `yaml:"chats"`
	Exclude []string `yaml:"exclude"`
	// Media stores photos, documents, voice notes… up to MediaMaxSize bytes
	// (default 50 MB) and of the MediaTypes kinds.
	Media        *bool    `yaml:"media"`
	MediaMaxSize int64    `yaml:"mediamaxsize"`
	MediaTypes   []string `yaml:"mediatypes"`
	// HistoryDays is how much history the phone is asked for at pairing
	// (default 3650; the phone may send less).
	HistoryDays int `yaml:"historydays"`
	// Wait is the longest a run stays connected to receive (default 10m).
	Wait time.Duration `yaml:"wait"`
	// Since skips messages older than this day (YYYY-MM-DD).
	Since string `yaml:"since"`
}

type resolved struct {
	Session      string
	Spool        string
	Media        bool
	MediaMaxSize int64
	HistoryDays  int
	Wait         time.Duration
	Idle         time.Duration
}

var allTypes = []string{"photo", "video", "voice", "audio", "document", "sticker"}

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
	// open connects to WhatsApp and runs f with the connection; tests replace it.
	open func(ctx context.Context, f func(ctx context.Context, c conn) error) error
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
	r := resolved{Session: p.Session, Media: p.Media == nil || *p.Media, MediaMaxSize: p.MediaMaxSize,
		HistoryDays: p.HistoryDays, Wait: p.Wait, Idle: 30 * time.Second}
	if r.Session == "" {
		r.Session = filepath.Join(dataDir, "whatsapp-"+name+".db")
	}
	r.Spool = r.Session + ".spool"
	if r.MediaMaxSize == 0 {
		r.MediaMaxSize = 50 << 20
	}
	if r.HistoryDays == 0 {
		r.HistoryDays = 3650
	}
	if r.Wait == 0 {
		r.Wait = 10 * time.Minute
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
			return nil, fmt.Errorf("ERR26C: media type %q is not one of %v", t, allTypes)
		}
		types[t] = true
	}
	s := &Source{
		name: name, p: r, include: glob.CompileAll(p.Chats), exclude: glob.CompileAll(p.Exclude), types: types,
		commitEvery: max(commitEvery, 1), tmpDir: tmpDir, logger: logger, now: time.Now,
	}
	if p.Since != "" {
		var err error
		if s.since, err = time.Parse("2006-01-02", p.Since); err != nil {
			return nil, fmt.Errorf("ERR26D: since %q must be YYYY-MM-DD", p.Since)
		}
	}
	s.open = s.openMeow
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
	return s.open(ctx, func(ctx context.Context, c conn) error { return s.sync(ctx, c, rs) })
}

// Probe checks the session: paired, and to which account. It does not
// connect, so it acknowledges nothing.
func (s *Source) Probe(ctx context.Context) (domain.Probe, error) {
	who, err := s.paired(ctx)
	if err != nil {
		return domain.Probe{}, err
	}
	return domain.Probe{Summary: "paired as " + who + "; run at least every few days, or WhatsApp unlinks the device"}, nil
}

// SessionPath is where the session of the adapter is kept.
func (s *Source) SessionPath() string { return s.p.Session }

// Stub is a documented, disabled whatsapp adapter for `config generate`.
func Stub() config.GenAdapter {
	return config.GenAdapter{
		Name: "whatsapp", Type: Type, Schedule: "0 0 * * * *",
		Comment: "WhatsApp private chats and groups, as a linked device of the phone: pair it with\n" +
			"laker-agent whatsapp login <name>. Run it at least every few days, or WhatsApp unlinks it.",
		Params: []config.GenParam{
			{Key: "chats", Value: []string{"**"}, Comment: "patterns on chat names"},
			{Key: "exclude", Value: []string{}},
			{Key: "media", Value: true},
			{Key: "mediamaxsize", Value: 52428800, Comment: "bytes; larger media are recorded, not stored"},
			{Key: "mediatypes", Value: allTypes},
			{Key: "historydays", Value: 3650, Comment: "history asked for at pairing; the phone may send less"},
			{Key: "wait", Value: "10m", Comment: "longest a run stays connected"},
			{Key: "since", Value: "", Comment: "YYYY-MM-DD skips older messages"},
		},
	}
}
