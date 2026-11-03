package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/intern/coretest"
)

// fake delivers queued events; an event of kind "broken" cannot be expanded
// while broken is set (a history blob not downloadable yet).
type fake struct {
	queue  []fakeEvent
	files  map[string][]byte
	broken bool
	expand int
}

type fakeEvent struct {
	kind string
	msgs []message
}

func (f *fake) Self() string { return "+15550100000" }
func (f *fake) Drain(_ context.Context, _, _ time.Duration, keep func(string, []byte) error) error {
	for len(f.queue) > 0 {
		b, _ := json.Marshal(f.queue[0].msgs)
		if err := keep(f.queue[0].kind, b); err != nil {
			return err
		}
		f.queue = f.queue[1:]
	}
	return nil
}
func (f *fake) Expand(_ context.Context, kind string, data []byte) ([]message, error) {
	f.expand++
	if kind == "broken" && f.broken {
		return nil, errors.New("blob not ready")
	}
	var msgs []message
	err := json.Unmarshal(data, &msgs)
	return msgs, err
}
func (f *fake) Download(_ context.Context, m *media, w io.Writer) error {
	b, ok := f.files[string(m.Raw)]
	if !ok {
		return errors.New("404 media expired")
	}
	_, err := io.Copy(w, bytes.NewReader(b))
	return err
}

var (
	anna   = chat{ID: "15550100001@s.whatsapp.net", Title: "Anna", Type: "private"}
	family = chat{ID: "120363000000000001@g.us", Title: "Family", Type: "group"}
	promo  = chat{ID: "15550100009@s.whatsapp.net", Title: "Promo", Type: "private"}
	status = chat{ID: "status@broadcast", Title: "status", Type: ""}
)

func msg(c chat, id string, at time.Time, from, text string) message {
	return message{Chat: c, Op: domain.ChatInsert, ID: id, Date: at, FromID: from, FromName: from, Text: text}
}

func newSource(t *testing.T, f *fake) (*Source, core.Adapter) {
	t.Helper()
	s, err := New("wa", map[string]any{"exclude": []any{"Promo*"}, "mediamaxsize": 1000}, 2, t.TempDir(), t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	s.open = func(ctx context.Context, fn func(context.Context, conn) error) error { return fn(ctx, f) }
	return s, core.Adapter{Name: "wa", Type: Type, Source: s}
}

func rows(t *testing.T, env *coretest.Env) []domain.ChatRow {
	var out []domain.ChatRow
	if err := core.EachChat(context.Background(), env.Lake, func(r domain.ChatRow) error { out = append(out, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHistoryThenLive(t *testing.T) {
	env := coretest.New(t)
	ctx := context.Background()
	old := time.Now().Add(-400 * 24 * time.Hour).UTC().Truncate(time.Second)
	f := &fake{files: map[string][]byte{"img1": []byte("jpeg bytes"), "big": bytes.Repeat([]byte("x"), 2000)}}
	photo := msg(family, "F1", old, "Dad", "holiday")
	photo.Media = &media{Kind: "photo", Name: "photo.jpeg", MIME: "image/jpeg", Size: 10, Type: "image", Raw: []byte("img1")}
	video := msg(family, "F2", old.Add(time.Minute), "Mum", "")
	video.Media = &media{Kind: "video", Name: "video.mp4", Size: 2000, Type: "video", Raw: []byte("big")}
	gone := msg(family, "F3", old.Add(2*time.Minute), "Mum", "")
	gone.Media = &media{Kind: "document", Name: "old.pdf", Size: 10, Type: "document", Raw: []byte("expired")}
	f.queue = []fakeEvent{
		{"history", []message{
			msg(anna, "A1", old, "Anna", "shall we order pizza tonight?"),
			msg(anna, "A2", old.Add(time.Hour), "me", "yes"),
			photo, video, gone,
			msg(promo, "P1", old, "Promo", "buy now"),
			msg(status, "S1", old, "x", "a status"),
		}},
	}
	s, a := newSource(t, f)

	run := env.Runner.Run(ctx, a, false)
	if run.Status != domain.RunOK || run.Stats.Rows != 5 {
		t.Fatalf("history: %+v %s", run.Stats, run.Error)
	}
	byName := map[string]domain.ChatMedia{}
	for _, r := range rows(t, env) {
		if r.ChatTitle == "Promo" || r.ChatKey == status.ID {
			t.Errorf("not selected but recorded: %+v", r)
		}
		if r.Service != "whatsapp" || r.Op != domain.ChatInsert {
			t.Errorf("row: %+v", r)
		}
		for _, m := range r.Media {
			byName[m.Name] = m
		}
	}
	if m := byName["photo.jpeg"]; m.SHA256 == "" || m.Size != 10 {
		t.Errorf("photo: %+v", m)
	}
	if m := byName["video.mp4"]; m.SHA256 != "" || !strings.Contains(m.Note, "too large") {
		t.Errorf("large video: %+v", m)
	}
	if m := byName["old.pdf"]; m.SHA256 != "" || !strings.Contains(m.Note, "download failed") {
		t.Errorf("expired document: %+v", m)
	}
	if left, _ := os.ReadDir(s.p.Spool); len(left) != 0 {
		t.Errorf("spool not emptied: %d", len(left))
	}

	// live: a new message, the same history message again, an edit, a deletion
	now := time.Now().UTC().Truncate(time.Second)
	edit := message{Chat: anna, Op: domain.ChatUpdate, ID: "A1", Date: now, Text: "shall we order sushi tonight?"}
	del := message{Chat: family, Op: domain.ChatDelete, ID: "F1", Date: now}
	f.queue = []fakeEvent{
		{"message", []message{msg(anna, "A3", now, "Anna", "see you")}},
		{"message", []message{msg(anna, "A2", old.Add(time.Hour), "me", "yes")}},
		{"message", []message{edit}},
		{"message", []message{edit}},
		{"message", []message{del}},
	}
	run = env.Runner.Run(ctx, a, false)
	if run.Status != domain.RunOK || run.Stats.Rows != 3 {
		t.Fatalf("live: %+v %s", run.Stats, run.Error)
	}
	ops := map[string]int{}
	for _, r := range rows(t, env) {
		ops[r.Op]++
		if r.Op == domain.ChatDelete && (r.Text != "holiday" || len(r.Media) != 1 || r.Date != old) {
			t.Errorf("a deletion keeps the last content: %+v", r)
		}
	}
	if ops["insert"] != 6 || ops["update"] != 1 || ops["delete"] != 1 {
		t.Errorf("ops: %v", ops)
	}

	env.With(t, func(st out.StateStore, idx out.Indexer) {
		if res, _ := idx.Search(ctx, domain.Query{Text: "sushi", Kind: "chat"}); res.Total != 1 {
			t.Errorf("the edit is not indexed: %d", res.Total)
		}
		res, _ := idx.Search(ctx, domain.Query{Text: "holiday", Kind: "chat"})
		if res.Total != 1 || res.Hits[0].Fields["deleted"] != true {
			t.Errorf("a deleted message stays searchable, marked: %+v", res)
		}

		// the state rebuilt from the lake knows every message in its last version
		want, _ := st.Get(domain.SourceBucket("wa"), domain.ChatMsgStateKey(anna.ID, "A1"))
		fresh := coretest.New(t)
		fresh.With(t, func(st2 out.StateStore, idx2 out.Indexer) {
			if _, err := core.RebuildState(ctx, env.Lake, st2); err != nil {
				t.Fatal(err)
			}
			got, _ := st2.Get(domain.SourceBucket("wa"), domain.ChatMsgStateKey(anna.ID, "A1"))
			var w, g domain.ChatRow
			json.Unmarshal(want, &w)
			json.Unmarshal(got, &g)
			if g.Text != w.Text || g.Op != domain.ChatUpdate {
				t.Errorf("rebuilt %+v, live %+v", g, w)
			}
			core.Reindex(ctx, env.Lake, idx2, zerolog.Nop())
			if res, _ := idx2.Search(ctx, domain.Query{Text: "sushi"}); res.Total != 1 {
				t.Errorf("reindex: %d", res.Total)
			}
		})
	})
}

// an event that cannot be processed yet stays in the spool, the others go on
func TestSpoolKeepsWhatFails(t *testing.T) {
	env := coretest.New(t)
	ctx := context.Background()
	now := time.Now().UTC()
	f := &fake{broken: true, queue: []fakeEvent{
		{"broken", []message{msg(anna, "B1", now, "Anna", "from the history")}},
		{"message", []message{msg(anna, "L1", now, "Anna", "live")}},
	}}
	s, a := newSource(t, f)
	if run := env.Runner.Run(ctx, a, false); run.Status != domain.RunOK || run.Stats.Rows != 1 {
		t.Fatalf("first: %+v %s", run.Stats, run.Error)
	}
	if left, _ := os.ReadDir(s.p.Spool); len(left) != 1 {
		t.Fatalf("spool: %d events left", len(left))
	}
	f.broken = false
	if run := env.Runner.Run(ctx, a, false); run.Status != domain.RunOK || run.Stats.Rows != 1 {
		t.Fatalf("second: %+v %s", run.Stats, run.Error)
	}
	if left, _ := os.ReadDir(s.p.Spool); len(left) != 0 {
		t.Errorf("spool: %d events left", len(left))
	}
}

// an edit can arrive before its original: both are recorded
func TestEditBeforeOriginal(t *testing.T) {
	env := coretest.New(t)
	ctx := context.Background()
	old, now := time.Now().Add(-time.Hour).UTC().Truncate(time.Second), time.Now().UTC().Truncate(time.Second)
	f := &fake{queue: []fakeEvent{
		{"message", []message{{Chat: anna, Op: domain.ChatUpdate, ID: "E1", Date: now, Text: "fixed text"}}},
		{"history", []message{msg(anna, "E1", old, "Anna", "first text")}},
	}}
	_, a := newSource(t, f)
	if run := env.Runner.Run(ctx, a, false); run.Status != domain.RunOK || run.Stats.Rows != 2 {
		t.Fatalf("%+v %s", run.Stats, run.Error)
	}
	env.With(t, func(_ out.StateStore, idx out.Indexer) {
		res, _ := idx.Search(ctx, domain.Query{Text: "fixed", Kind: "chat"})
		if res.Total != 1 || res.Hits[0].Fields["from"] != "Anna" {
			t.Errorf("indexed: %+v", res)
		}
	})
}

func TestParams(t *testing.T) {
	if _, err := New("wa", map[string]any{"mediatypes": []any{"gif"}}, 1, t.TempDir(), t.TempDir(), zerolog.Nop()); err == nil {
		t.Error("unknown media type accepted")
	}
	s, err := New("wa", nil, 1, "/var/lib/laker-agent", t.TempDir(), zerolog.Nop())
	if err != nil || s.SessionPath() != "/var/lib/laker-agent/whatsapp-wa.db" || s.p.Wait != 10*time.Minute {
		t.Errorf("defaults: %+v %v", s.p, err)
	}
}

func TestProbeNotPaired(t *testing.T) {
	s, err := New("wa", nil, 1, t.TempDir(), t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Probe(context.Background()); !errors.Is(err, ErrNotPaired) {
		t.Errorf("probe: %v", err)
	}
	if fi, err := os.Stat(s.SessionPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("session file: %v %v", fi, err)
	}
}
