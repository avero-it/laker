package web

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/avero-it/laker/intern/core/domain"
)

// chatIDRE matches Telegram ids (numbers) and WhatsApp ids (JIDs).
var chatIDRE = regexp.MustCompile(`^-?[0-9A-Za-z][0-9A-Za-z@._:-]*$`)

// chatWindow is the period of a conversation one page shows.
const chatWindow = 3 * 24 * time.Hour

type chatMediaView struct{ Name, SHA string }

type chatMessageView struct {
	ID                    string
	When, From            string
	Text                  string
	Edited, Deleted, Here bool
	Media                 []chatMediaView
}

func list(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = str(e)
		}
		return out
	}
	return nil
}

// chat shows a conversation around a date (and a message), from the index:
// every message is a document with its chat, its date and its text.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	source, chatID := r.PathValue("source"), r.PathValue("chat")
	if !chatIDRE.MatchString(chatID) {
		s.fail(w, http.StatusNotFound, "Chat not found", fmt.Errorf("ERR40F: %q is not a chat id", chatID))
		return
	}
	q := r.URL.Query()
	center, err := time.Parse(time.RFC3339, q.Get("date"))
	if err != nil {
		center = time.Now()
	}
	around := q.Get("around")
	from, to := center.Add(-chatWindow), center.Add(chatWindow)

	res, err := s.Index.Search(r.Context(), domain.Query{
		Text: fmt.Sprintf(`+source:%q +chat_id:%q +date:>=%q +date:<=%q`, source, chatID,
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)),
		Kind: "chat", Limit: 1000, Sort: []string{"date", "msg_id"},
	})
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "Cannot read the chat", err)
		return
	}
	title := chatID
	var msgs []chatMessageView
	for _, h := range res.Hits {
		f := h.Fields
		title = str(f["chat"])
		m := chatMessageView{ID: str(f["msg_key"]), When: s.when(f["date"]), From: str(f["from"]), Text: str(f["text"])}
		m.Here, m.Edited, m.Deleted = m.ID != "" && m.ID == around, f["edited"] == true, f["deleted"] == true
		names, blobs := list(f["media"]), list(f["blobs"])
		for i, n := range names {
			mv := chatMediaView{Name: n}
			if i < len(blobs) && shaRE.MatchString(blobs[i]) {
				mv.SHA = blobs[i]
			}
			m.Media = append(m.Media, mv)
		}
		msgs = append(msgs, m)
	}
	link := func(at time.Time) string {
		return "/chat/" + url.PathEscape(source) + "/" + url.PathEscape(chatID) + "?" + url.Values{"date": {at.UTC().Format(time.RFC3339)}}.Encode()
	}
	s.render(w, "chat", map[string]any{
		"Title": title, "Chat": title, "Source": source, "Messages": msgs,
		"From": s.when(from), "To": s.when(to),
		"Earlier": link(center.Add(-2 * chatWindow)), "Later": link(center.Add(2 * chatWindow)),
	})
}
