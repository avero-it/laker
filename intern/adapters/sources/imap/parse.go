package imap

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"

	_ "github.com/emersion/go-message/charset" // decode non-UTF-8 parts
	"github.com/emersion/go-message/mail"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/pkg/extract"
)

// maxBodyText caps the text kept per message for the lake row and the index.
const maxBodyText = 1 << 20

// parseMail fills the header and body fields of row from a raw RFC 5322
// message. A message that cannot be fully parsed still yields a row: what
// failed is recorded in row.Note.
func parseMail(raw []byte, row *domain.MailRow) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if mr == nil {
		row.Note = fmt.Sprintf("parse: %v", err)
		return
	}
	defer mr.Close()

	h := mr.Header
	row.Subject, _ = h.Subject()
	row.Date, _ = h.Date()
	row.MessageID, _ = h.MessageID()
	if from, err := h.AddressList("From"); err == nil && len(from) > 0 {
		row.From = formatAddress(from[0])
	} else {
		row.From = h.Get("From")
	}
	row.To = addresses(h, "To")
	row.Cc = addresses(h, "Cc")
	if ids, _ := h.MsgIDList("In-Reply-To"); len(ids) > 0 {
		row.InReplyTo = ids[0]
	}
	refs, _ := h.MsgIDList("References")
	switch {
	case len(refs) > 0:
		row.ThreadKey = refs[0]
	case row.InReplyTo != "":
		row.ThreadKey = row.InReplyTo
	default:
		row.ThreadKey = row.MessageID
	}

	var plain, htmlText strings.Builder
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			row.Note = fmt.Sprintf("parse: %v", err)
			break
		}
		switch ph := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := ph.ContentType()
			b, _ := io.ReadAll(io.LimitReader(p.Body, maxBodyText))
			switch ct {
			case "text/plain":
				plain.Write(b)
				plain.WriteString("\n")
			case "text/html":
				row.HasHTML = true
				htmlText.WriteString(htmlToText(b))
				htmlText.WriteString("\n")
			}
		case *mail.AttachmentHeader:
			name, _ := ph.Filename()
			ct, _, _ := ph.ContentType()
			n, _ := io.Copy(io.Discard, p.Body)
			row.Attachments = append(row.Attachments, domain.Attachment{Name: decodeWord(name), MIME: ct, Size: n})
		}
	}

	text := plain.String()
	if strings.TrimSpace(text) == "" {
		text = htmlText.String()
	}
	if len(text) > maxBodyText {
		text = text[:maxBodyText]
	}
	row.BodyText = strings.ToValidUTF8(strings.TrimSpace(text), "")
}

func addresses(h mail.Header, key string) []string {
	list, err := h.AddressList(key)
	if err != nil {
		if v := h.Get(key); v != "" {
			return []string{v}
		}
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, formatAddress(a))
	}
	return out
}

// formatAddress is "Name <addr>" in plain UTF-8 (String() would re-encode
// the name as an RFC 2047 word, which nobody can search for).
func formatAddress(a *mail.Address) string {
	if a.Name == "" {
		return a.Address
	}
	return a.Name + " <" + a.Address + ">"
}

func decodeWord(s string) string {
	if d, err := new(mime.WordDecoder).DecodeHeader(s); err == nil {
		return d
	}
	return s
}

// htmlToText is the visible text of an HTML body, on one line.
func htmlToText(b []byte) string { return strings.Join(strings.Fields(extract.HTML(b)), " ") }
