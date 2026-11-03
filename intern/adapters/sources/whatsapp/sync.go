package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/avero-it/laker/intern/adapters/sources/chatmedia"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/parquetx"
)

// sync takes what the server delivers into the spool, then turns the spool
// into rows. Events that arrive while the spool is processed are kept for
// the next run.
func (s *Source) sync(ctx context.Context, c conn, rs out.RunSink) error {
	sp, err := newSpool(s.p.Spool)
	if err != nil {
		return err
	}
	drainErr := c.Drain(ctx, s.p.Wait, s.p.Idle, sp.put)
	if drainErr != nil {
		// what was kept is processed anyway
		s.logger.Warn().Msgf("WRN260: receiving stopped early: %v", drainErr)
	}
	if err := s.process(ctx, c, rs, sp); err != nil {
		return err
	}
	return drainErr
}

func (s *Source) process(ctx context.Context, c conn, rs out.RunSink, sp *spool) error {
	evs, err := sp.list()
	if err != nil {
		return err
	}
	s.logger.Debug().Int("events", len(evs)).Msg("DBG260: events to process")
	pending, written := 0, 0
	var finished []spooled
	commit := func() error {
		if err := rs.Commit(ctx); err != nil {
			return err
		}
		pending = 0
		if err := sp.done(finished); err != nil {
			return err
		}
		finished = finished[:0]
		return nil
	}
	for _, ev := range evs {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := os.ReadFile(ev.path)
		if err != nil {
			return fmt.Errorf("ERR26A: %w", err)
		}
		msgs, err := c.Expand(ctx, ev.kind, data)
		if err != nil {
			// kept for the next run: a history blob may not be downloadable yet
			s.logger.Warn().Str("event", ev.kind).Msgf("WRN261: event not processed, kept for the next run: %v", err)
			continue
		}
		for _, m := range msgs {
			ok, err := s.apply(ctx, c, rs, m)
			if err != nil {
				return err
			}
			if ok {
				pending++
				written++
			}
			if pending >= s.commitEvery {
				if err := commit(); err != nil {
					return err
				}
			}
		}
		finished = append(finished, ev)
	}
	if written > 0 {
		s.logger.Info().Int("messages", written).Msg("INF260: messages recorded")
	}
	return commit()
}

// apply records one message, edit or deletion; false when there is nothing
// new (a message delivered twice, a chat not selected).
func (s *Source) apply(ctx context.Context, c conn, rs out.RunSink, m message) (bool, error) {
	if m.Chat.Type != "private" && m.Chat.Type != "group" {
		return false, nil
	}
	key := domain.ChatMsgStateKey(m.Chat.ID, m.ID)
	var prev *domain.ChatRow
	if b, err := rs.GetState(key); err != nil {
		return false, err
	} else if b != nil {
		prev = &domain.ChatRow{}
		if err := json.Unmarshal(b, prev); err != nil {
			return false, fmt.Errorf("ERR26B: corrupt state of message %s: %w", m.ID, err)
		}
	}
	title := m.Chat.Title
	if prev != nil && prev.ChatTitle != "" {
		title = prev.ChatTitle
	}
	if !s.include.Match(title) || s.exclude.Match(title) {
		return false, nil
	}

	var row domain.ChatRow
	switch m.Op {
	case domain.ChatInsert:
		// an edit or deletion can come before its original (history sent
		// after the live events): the original is still recorded
		late := prev != nil && prev.Op != domain.ChatInsert && prev.Date.Equal(prev.EditDate)
		if (prev != nil && !late) || (!s.since.IsZero() && m.Date.Before(s.since)) {
			return false, nil
		}
		row = domain.ChatRow{
			ChatKey: m.Chat.ID, ChatTitle: title, ChatType: m.Chat.Type, MsgKey: m.ID, Date: m.Date.UTC(),
			FromKey: m.FromID, FromName: m.FromName, Text: m.Text, ReplyKey: m.ReplyTo, ForwardedFrom: m.Forwarded,
		}
		if m.Media != nil {
			row.Media = []domain.ChatMedia{s.media(ctx, c, rs, m.Media)}
		}
		if late {
			row.Source, row.Service, row.Op = s.name, "whatsapp", domain.ChatInsert
			row.SeenAt, row.RunID = s.now().UTC(), rs.RunID()
			parquetx.Append(rs, domain.ChatPrefix(s.name, row.Date), row)
			// the index and the state keep the later version, now complete
			later := *prev
			later.Date, later.FromKey, later.FromName, later.ReplyKey, later.Media = row.Date, row.FromKey, row.FromName, row.ReplyKey, row.Media
			if later.Op == domain.ChatDelete {
				later.Text = row.Text
			}
			rs.IndexChat(domain.NewChatDoc(later))
			rs.SetState(key, domain.MustJSON(later))
			return true, nil
		}
	case domain.ChatUpdate, domain.ChatDelete:
		if prev != nil && (prev.Op == domain.ChatDelete || !m.Date.After(prev.EditDate)) {
			return false, nil
		}
		if prev != nil {
			row = *prev // the last version, with the new text or marked deleted
		} else {
			// the original is older than what this device received
			row = domain.ChatRow{ChatKey: m.Chat.ID, ChatTitle: title, ChatType: m.Chat.Type, MsgKey: m.ID,
				FromKey: m.FromID, FromName: m.FromName}
		}
		if m.Op == domain.ChatUpdate {
			row.Text = m.Text
		}
		row.EditDate = m.Date.UTC()
	default:
		return false, nil
	}
	row.Source, row.Service, row.Op = s.name, "whatsapp", m.Op
	row.SeenAt, row.RunID = s.now().UTC(), rs.RunID()
	if row.Date.IsZero() {
		row.Date = row.EditDate
	}

	parquetx.Append(rs, domain.ChatPrefix(s.name, row.Date), row)
	rs.IndexChat(domain.NewChatDoc(row))
	rs.SetState(key, domain.MustJSON(row))
	return true, nil
}

// media downloads a media within the limits into the lake; what it does not
// store is recorded with the reason. A failed download does not fail the run.
func (s *Source) media(ctx context.Context, c conn, rs out.RunSink, m *media) domain.ChatMedia {
	cm := domain.ChatMedia{Kind: m.Kind, Name: m.Name, MIME: m.MIME, Size: m.Size}
	switch {
	case len(m.Raw) == 0:
		return cm
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
	sha, size, err := chatmedia.Store(ctx, rs, s.tmpDir, "whatsapp", s.p.MediaMaxSize, func(w io.Writer) error {
		return c.Download(ctx, m, w)
	})
	if err != nil {
		s.logger.Warn().Str("media", m.Name).Msgf("WRN262: media not stored: %v", err)
		cm.Note = "download failed (expired on WhatsApp's servers?)"
		return cm
	}
	cm.SHA256, cm.Size = sha, size
	return cm
}
