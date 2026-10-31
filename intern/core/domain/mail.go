package domain

import "time"

// MailRow is one occurrence of a message in a mailbox folder, as stored in
// mail/messages. The message itself is the blob named by SHA256; the same
// message in two folders or accounts is one blob and two rows.
type MailRow struct {
	SHA256       string       `parquet:"sha256,id(1)"`
	Account      string       `parquet:"account,id(2)"` // adapter name
	Mailbox      string       `parquet:"mailbox,id(3)"` // login, e.g. me@example.com
	Folder       string       `parquet:"folder,id(4)"`
	UIDValidity  int64        `parquet:"uidvalidity,id(5)"`
	UID          int64        `parquet:"uid,id(6)"`
	MessageID    string       `parquet:"message_id,id(7)"`
	ThreadKey    string       `parquet:"thread_key,id(8)"`
	InReplyTo    string       `parquet:"in_reply_to,id(9)"`
	Date         time.Time    `parquet:"date,id(10)"`
	From         string       `parquet:"from,id(11)"`
	To           []string     `parquet:"to,id(12),list"`
	Cc           []string     `parquet:"cc,id(13),list"`
	Subject      string       `parquet:"subject,id(14)"`
	BodyText     string       `parquet:"body_text,id(15)"`
	HasHTML      bool         `parquet:"has_html,id(16)"`
	Attachments  []Attachment `parquet:"attachments,id(17),list"`
	Flags        []string     `parquet:"flags,id(18),list"`
	Size         int64        `parquet:"size,id(19)"`
	InternalDate time.Time    `parquet:"internal_date,id(20)"`
	SeenAt       time.Time    `parquet:"seen_at,id(21)"`
	RunID        string       `parquet:"run_id,id(22)"`
	// Note says why something is missing, e.g. "parse: …" or "too large".
	Note string `parquet:"note,id(23)"`
}

type Attachment struct {
	Name string `parquet:"name"`
	MIME string `parquet:"mime"`
	Size int64  `parquet:"size"`
}

// MailDoc is a message in the search index, one per SHA256 whatever the
// number of places it was found in.
type MailDoc struct {
	Kind        string    `json:"kind"`
	SHA256      string    `json:"sha256"`
	Accounts    []string  `json:"account"`
	Folders     []string  `json:"folder"`
	MessageID   string    `json:"message_id"`
	Date        time.Time `json:"date"`
	From        string    `json:"from"`
	To          []string  `json:"to"`
	Cc          []string  `json:"cc"`
	Subject     string    `json:"subject"`
	Body        string    `json:"body"`
	Attachments []string  `json:"attachments"`
	Size        int64     `json:"size"`
}

func (MailDoc) Type() string { return "mail" }

// MailDocID is the index id of the message with the given SHA-256.
func MailDocID(sha string) string { return "mail:" + sha }

// MailPrefix is where the rows of an account live in the lake.
func MailPrefix(account string, month time.Time) string {
	return "mail/messages/account=" + account + "/month=" + month.UTC().Format("2006-01")
}

// NewMailDoc is the index document of a mail row seen at locs.
func NewMailDoc(r MailRow, locs MailLocations) MailDoc {
	names := make([]string, 0, len(r.Attachments))
	for _, a := range r.Attachments {
		names = append(names, a.Name)
	}
	date := r.Date
	if date.IsZero() {
		date = r.InternalDate
	}
	return MailDoc{
		Kind: "mail", SHA256: r.SHA256, Accounts: locs.Accounts, Folders: locs.Folders,
		MessageID: r.MessageID, Date: date, From: r.From, To: r.To, Cc: r.Cc,
		Subject: r.Subject, Body: r.BodyText, Attachments: names, Size: r.Size,
	}
}
