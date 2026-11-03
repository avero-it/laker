package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/avero-it/laker/intern/adapters/sources/chatmedia"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/parquetx"
)

const pageSize = 100

// sync reads every selected chat: the whole history the first time (inside an
// export session), then what is new and what was edited in the lookback window.
func (s *Source) sync(ctx context.Context, tg api, rs out.RunSink) error {
	chats, err := tg.Dialogs(ctx)
	if err != nil {
		return fmt.Errorf("ERR241: cannot list the chats: %w", err)
	}
	var selected []chat
	for _, c := range chats {
		if s.include.Match(c.Title) && !s.exclude.Match(c.Title) {
			selected = append(selected, c)
		}
	}
	s.logger.Debug().Int("chats", len(selected)).Int("all", len(chats)).Msg("DBG240: chats to read")

	takeout := false
	defer func() {
		if takeout {
			if err := tg.EndTakeout(context.WithoutCancel(ctx), true); err != nil {
				s.logger.Warn().Msgf("WRN240: cannot close the export session: %v", err)
			}
		}
	}()

	for _, c := range selected {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := s.state(rs, c.ID)
		if err != nil {
			return err
		}
		if !st.Imported && !takeout {
			if err := tg.BeginTakeout(ctx, s.p.MediaMaxSize); err != nil {
				return err
			}
			takeout = true
			s.logger.Info().Msg("INF240: export session open for the first import")
		}
		if err := s.chat(ctx, tg, rs, c, st); err != nil {
			return fmt.Errorf("ERR242: chat %q: %w", c.Title, err)
		}
	}
	return nil
}

func (s *Source) state(rs out.RunSink, chatID int64) (domain.ChatState, error) {
	var st domain.ChatState
	b, err := rs.GetState(domain.ChatStateKey(chatID))
	if err != nil || b == nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("ERR243: corrupt state of chat %d: %w", chatID, err)
	}
	return st, nil
}

func (s *Source) chat(ctx context.Context, tg api, rs out.RunSink, c chat, st domain.ChatState) error {
	key := domain.ChatStateKey(c.ID)
	pending := 0
	commit := func(force bool) error {
		if pending == 0 && !force {
			return nil
		}
		rs.SetState(key, domain.MustJSON(st))
		pending = 0
		return rs.Commit(ctx)
	}

	// what is new, oldest first
	for {
		msgs, err := tg.After(ctx, c, st.LastID, pageSize)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			if m.ID <= st.LastID {
				continue
			}
			if s.since.IsZero() || !m.Date.Before(s.since) {
				if err := s.record(ctx, tg, rs, c, m, domain.ChatInsert); err != nil {
					return err
				}
				pending++
			}
			st.LastID = m.ID
			if m.EditDate.After(st.EditSeen) {
				st.EditSeen = m.EditDate
			}
			if pending >= s.commitEvery {
				if err := commit(false); err != nil {
					return err
				}
			}
		}
	}
	firstImport := !st.Imported
	st.Imported = true

	// what was edited lately; on the first import everything is fresh already
	if !firstImport && s.p.Lookback > 0 {
		recent, err := tg.Since(ctx, c, time.Now().Add(-s.p.Lookback))
		if err != nil {
			return err
		}
		slices.SortFunc(recent, func(a, b message) int { return a.EditDate.Compare(b.EditDate) })
		for _, m := range recent {
			if m.EditDate.IsZero() || !m.EditDate.After(st.EditSeen) || m.ID > st.LastID {
				continue
			}
			if err := s.record(ctx, tg, rs, c, m, domain.ChatUpdate); err != nil {
				return err
			}
			st.EditSeen = m.EditDate
			pending++
		}
	}
	return commit(firstImport)
}

// record stores one message: its media as a blob, its row, its document.
func (s *Source) record(ctx context.Context, tg api, rs out.RunSink, c chat, m message, op string) error {
	row := domain.ChatRow{
		Source: s.name, Service: "telegram", ChatID: c.ID, ChatTitle: c.Title, ChatType: c.Type,
		MsgID: m.ID, Date: m.Date.UTC(), EditDate: m.EditDate.UTC(), Op: op, FromID: m.FromID, FromName: m.FromName,
		Text: m.Text, ReplyTo: m.ReplyTo, ForwardedFrom: m.Forwarded, SeenAt: s.now().UTC(), RunID: rs.RunID(),
		ChatKey: strconv.FormatInt(c.ID, 10), MsgKey: strconv.FormatInt(m.ID, 10), FromKey: strconv.FormatInt(m.FromID, 10),
	}
	if m.ReplyTo != 0 {
		row.ReplyKey = strconv.FormatInt(m.ReplyTo, 10)
	}
	if m.Media != nil {
		row.Media = []domain.ChatMedia{s.media(ctx, tg, rs, m.Media)}
	}
	parquetx.Append(rs, domain.ChatPrefix(s.name, row.Date), row)
	rs.IndexChat(domain.NewChatDoc(row))
	return nil
}

// media downloads a media within the limits into the lake; what it does not
// store is recorded with the reason. A failed download does not fail the run.
func (s *Source) media(ctx context.Context, tg api, rs out.RunSink, m *media) domain.ChatMedia {
	cm := domain.ChatMedia{Kind: m.Kind, Name: m.Name, MIME: m.MIME, Size: m.Size}
	switch {
	case m.loc == nil:
		return cm // nothing to download: a location, a contact, a poll…
	case !s.p.Media:
		cm.Note = "media disabled"
		return cm
	case !s.types[m.Kind]:
		cm.Note = "type " + m.Kind + " not selected"
		return cm
	case m.Size > s.p.MediaMaxSize:
		cm.Note = fmt.Sprintf("too large: %d bytes", m.Size)
		return cm
	}

	sha, size, err := s.download(ctx, tg, rs, m)
	if err != nil {
		s.logger.Warn().Str("media", m.Name).Msgf("WRN241: media not stored: %v", err)
		cm.Note = "download failed"
		return cm
	}
	cm.SHA256, cm.Size = sha, size
	return cm
}

func (s *Source) download(ctx context.Context, tg api, rs out.RunSink, m *media) (string, int64, error) {
	return chatmedia.Store(ctx, rs, s.tmpDir, "telegram", s.p.MediaMaxSize, func(w io.Writer) error {
		return tg.Download(ctx, m, w)
	})
}
