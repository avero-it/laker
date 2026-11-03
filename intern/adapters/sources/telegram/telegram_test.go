package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/intern/coretest"
)

type fake struct {
	chats      []chat
	msgs       map[int64][]message // per chat, ascending ids
	files      map[string][]byte
	delay      time.Duration // TAKEOUT_INIT_DELAY once
	failAfter  int           // After calls before failing, 0: never
	afterCalls int
	begun      int
	ended      int
	inTakeout  bool
}

func (f *fake) Self(context.Context) (string, error) { return "Me", nil }
func (f *fake) Dialogs(context.Context) ([]chat, error) {
	return f.chats, nil
}
func (f *fake) After(_ context.Context, c chat, minID int64, limit int) ([]message, error) {
	f.afterCalls++
	if f.failAfter > 0 && f.afterCalls > f.failAfter {
		return nil, errors.New("connection lost")
	}
	var out []message
	for _, m := range f.msgs[c.ID] {
		if m.ID > minID && len(out) < limit {
			out = append(out, m)
		}
	}
	return out, nil
}
func (f *fake) Since(_ context.Context, c chat, t time.Time) ([]message, error) {
	var out []message
	for _, m := range f.msgs[c.ID] {
		if !m.Date.Before(t) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}
func (f *fake) Download(_ context.Context, m *media, w io.Writer) error {
	_, err := io.Copy(w, bytes.NewReader(f.files[m.loc.(string)]))
	return err
}
func (f *fake) BeginTakeout(context.Context, int64) error {
	if f.delay > 0 {
		d := f.delay
		f.delay = 0
		return &TakeoutDelayError{Wait: d}
	}
	f.begun++
	f.inTakeout = true
	return nil
}
func (f *fake) EndTakeout(context.Context, bool) error {
	f.ended++
	f.inTakeout = false
	return nil
}

func newFake() *fake {
	old := time.Now().Add(-60 * 24 * time.Hour)
	f := &fake{
		chats: []chat{
			{ID: 10, Title: "Anna", Type: "private"},
			{ID: -20, Title: "Family", Type: "group"},
			{ID: 30, Title: "Promo bot", Type: "private"},
		},
		msgs:  map[int64][]message{},
		files: map[string][]byte{"photo1": []byte("jpeg bytes"), "big": bytes.Repeat([]byte("x"), 2000)},
	}
	for i := int64(1); i <= 250; i++ {
		f.msgs[10] = append(f.msgs[10], message{ID: i, Date: old.Add(time.Duration(i) * time.Minute), FromID: 10, FromName: "Anna", Text: fmt.Sprintf("message %d", i)})
	}
	f.msgs[10][41].Text = "shall we order pizza tonight?"
	f.msgs[-20] = []message{
		{ID: 1, Date: old, FromName: "Dad", Text: "holiday photos", Media: &media{Kind: "photo", Name: "photo-1.jpg", MIME: "image/jpeg", Size: 10, loc: "photo1"}},
		{ID: 2, Date: old, FromName: "Mum", Text: "the full video", Media: &media{Kind: "video", Name: "v.mp4", Size: 2000, loc: "big"}},
		{ID: 3, Date: old, FromName: "Dad", Media: &media{Kind: "other", Name: "Geo"}},
	}
	f.msgs[30] = []message{{ID: 1, Date: old, Text: "buy now"}}
	return f
}

func newSource(t *testing.T, f *fake) (*Source, core.Adapter) {
	t.Helper()
	s, err := New("tg", map[string]any{
		"apiid": "123", "apihash": "abc", "exclude": []any{"Promo*"}, "mediamaxsize": 1000,
	}, 50, t.TempDir(), t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	s.open = func(ctx context.Context, fn func(context.Context, api) error) error { return fn(ctx, f) }
	return s, core.Adapter{Name: "tg", Type: Type, Source: s}
}

func rows(t *testing.T, env *coretest.Env) []domain.ChatRow {
	var out []domain.ChatRow
	if err := core.EachChat(context.Background(), env.Lake, func(r domain.ChatRow) error { out = append(out, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestImportThenSync(t *testing.T) {
	env := coretest.New(t)
	ctx := context.Background()
	f := newFake()
	_, a := newSource(t, f)

	run := env.Runner.Run(ctx, a, false)
	if run.Status != domain.RunOK {
		t.Fatal(run.Error)
	}
	all := rows(t, env)
	if len(all) != 253 || f.begun != 1 || f.ended != 1 {
		t.Fatalf("first import: %d rows, takeout begun %d ended %d", len(all), f.begun, f.ended)
	}
	media := map[string]domain.ChatMedia{}
	for _, r := range all {
		if r.ChatTitle == "Promo bot" {
			t.Error("an excluded chat was read")
		}
		for _, m := range r.Media {
			media[m.Name] = m
		}
	}
	if m := media["photo-1.jpg"]; m.SHA256 == "" || m.Size != 10 {
		t.Errorf("photo: %+v", m)
	}
	if m := media["v.mp4"]; m.SHA256 != "" || !strings.Contains(m.Note, "too large") {
		t.Errorf("large video: %+v", m)
	}
	if m := media["Geo"]; m.SHA256 != "" || m.Note != "" {
		t.Errorf("location: %+v", m)
	}

	env.With(t, func(_ out.StateStore, idx out.Indexer) {
		res, err := idx.Search(ctx, domain.Query{Text: "pizza", Kind: "chat"})
		if err != nil || res.Total != 1 || res.Hits[0].Fields["chat"] != "Anna" {
			t.Errorf("search: %+v %v", res, err)
		}
	})

	// a new message, and an old one edited
	now := time.Now()
	f.msgs[10] = append(f.msgs[10], message{ID: 251, Date: now, FromName: "Anna", Text: "see you"})
	f.msgs[10][249].Text, f.msgs[10][249].Date, f.msgs[10][249].EditDate = "message 250 (edited)", now.Add(-time.Hour), now
	run = env.Runner.Run(ctx, a, false)
	if run.Status != domain.RunOK || run.Stats.Rows != 2 || f.begun != 1 {
		t.Fatalf("sync: %+v %s, takeouts %d", run.Stats, run.Error, f.begun)
	}
	ops := map[string]int{}
	for _, r := range rows(t, env) {
		ops[r.Op]++
	}
	if ops["insert"] != 254 || ops["update"] != 1 {
		t.Errorf("ops: %v", ops)
	}

	// nothing new: nothing written
	if run := env.Runner.Run(ctx, a, false); run.Stats.Rows != 0 {
		t.Errorf("idle run wrote %d rows", run.Stats.Rows)
	}

	// the state rebuilt from the lake resumes at the same place
	env.With(t, func(st out.StateStore, _ out.Indexer) {
		want, _ := st.Get(domain.SourceBucket("tg"), domain.ChatStateKey(10))
		fresh := coretest.New(t)
		fresh.With(t, func(st2 out.StateStore, idx out.Indexer) {
			core.RebuildState(ctx, env.Lake, st2)
			got, _ := st2.Get(domain.SourceBucket("tg"), domain.ChatStateKey(10))
			if !strings.Contains(string(got), `"last_id":251`) || !strings.Contains(string(want), `"last_id":251`) {
				t.Errorf("rebuilt state %s, live %s", got, want)
			}
			core.Reindex(ctx, env.Lake, idx, zerolog.Nop())
			if res, _ := idx.Search(ctx, domain.Query{Text: "edited"}); res.Total != 1 {
				t.Errorf("reindex: the edit is not the indexed version (%d hits)", res.Total)
			}
		})
	})
}

func TestTakeoutDelay(t *testing.T) {
	env := coretest.New(t)
	f := newFake()
	f.delay = 20 * time.Hour
	_, a := newSource(t, f)
	run := env.Runner.Run(context.Background(), a, false)
	if run.Status != domain.RunFailed || !strings.Contains(run.Error, "confirm the data export in the Telegram app") || !strings.Contains(run.Error, "20h") {
		t.Fatalf("run: %+v", run)
	}
	if len(rows(t, env)) != 0 {
		t.Error("rows written before the export was allowed")
	}
	if run := env.Runner.Run(context.Background(), a, false); run.Status != domain.RunOK {
		t.Fatalf("the next run should import: %s", run.Error)
	}
}

func TestResumeAfterInterruptedImport(t *testing.T) {
	env := coretest.New(t)
	f := newFake()
	f.failAfter = 2 // two pages of 100, then the connection drops
	_, a := newSource(t, f)
	if run := env.Runner.Run(context.Background(), a, false); run.Status != domain.RunFailed {
		t.Fatalf("first run should fail: %+v", run)
	}
	if f.ended != 1 {
		t.Errorf("the export session was not closed after the failure")
	}
	partial := len(rows(t, env))
	if partial != 200 {
		t.Fatalf("committed before the failure: %d rows, want 200", partial)
	}
	f.failAfter = 0
	if run := env.Runner.Run(context.Background(), a, false); run.Status != domain.RunOK {
		t.Fatal(run.Error)
	}
	ids := map[[2]int64]int{}
	for _, r := range rows(t, env) {
		ids[[2]int64{r.ChatID, r.MsgID}]++
	}
	if len(ids) != 253 {
		t.Errorf("%d distinct messages, want 253", len(ids))
	}
	for k, n := range ids {
		if n > 1 {
			t.Errorf("message %v stored %d times", k, n)
		}
	}
}

func TestMediaOf(t *testing.T) {
	photo := mediaOf(&tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 7, Sizes: []tg.PhotoSizeClass{
		&tg.PhotoSize{Type: "m", Size: 100}, &tg.PhotoSizeProgressive{Type: "y", Sizes: []int{10, 900}},
	}}})
	if photo.Kind != "photo" || photo.Size != 900 || photo.loc.(*tg.InputPhotoFileLocation).ThumbSize != "y" {
		t.Errorf("photo: %+v", photo)
	}
	voice := mediaOf(&tg.MessageMediaDocument{Document: &tg.Document{ID: 8, MimeType: "audio/ogg", Size: 50,
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{Voice: true}}}})
	if voice.Kind != "voice" || !strings.HasPrefix(voice.Name, "voice-8") {
		t.Errorf("voice: %+v", voice)
	}
	doc := mediaOf(&tg.MessageMediaDocument{Document: &tg.Document{ID: 9, MimeType: "application/pdf",
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "invoice.pdf"}}}})
	if doc.Kind != "document" || doc.Name != "invoice.pdf" {
		t.Errorf("document: %+v", doc)
	}
	if geo := mediaOf(&tg.MessageMediaGeo{}); geo.loc != nil || geo.Kind != "other" {
		t.Errorf("geo: %+v", geo)
	}
}

func TestParams(t *testing.T) {
	if _, err := New("x", map[string]any{"apihash": "h"}, 1, "", "", zerolog.Nop()); err == nil {
		t.Error("missing apiid accepted")
	}
	if _, err := New("x", map[string]any{"apiid": "1", "apihash": "h", "mediatypes": []any{"sticker"}}, 1, "", "", zerolog.Nop()); err == nil {
		t.Error("unknown media type accepted")
	}
	s, err := New("x", map[string]any{"apiid": "1", "apihash": "h"}, 1, "/data", "", zerolog.Nop())
	if err != nil || s.SessionPath() != "/data/telegram-x.session" || s.p.Lookback != 7*24*time.Hour {
		t.Errorf("defaults: %+v %v", s.p, err)
	}
}
