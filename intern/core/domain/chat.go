package domain

import (
	"strconv"
	"time"
)

// Chat operations recorded in chat/messages.
const (
	ChatInsert = "insert"
	// ChatUpdate is a message edited after it was stored: a new row with the
	// new text, the old one stays.
	ChatUpdate = "update"
	// ChatDelete is a message deleted at the source (WhatsApp): a new row with
	// its last content, the message stays in the lake.
	ChatDelete = "delete"
)

// ChatRow is one message of a chat (Telegram, WhatsApp), as stored in
// chat/messages. Telegram ids are numbers, WhatsApp ids are strings: the
// *_key columns hold the id as text for every service (rows written before
// they existed have them empty; use Chat() and Msg()).
type ChatRow struct {
	Source        string      `parquet:"source,id(1)"` // adapter name
	Service       string      `parquet:"service,id(2)"`
	ChatID        int64       `parquet:"chat_id,id(3)"`
	ChatTitle     string      `parquet:"chat_title,id(4)"`
	ChatType      string      `parquet:"chat_type,id(5)"` // private | group
	MsgID         int64       `parquet:"msg_id,id(6)"`
	Date          time.Time   `parquet:"date,id(7)"`
	EditDate      time.Time   `parquet:"edit_date,id(8)"`
	Op            string      `parquet:"op,id(9)"`
	FromID        int64       `parquet:"from_id,id(10)"`
	FromName      string      `parquet:"from_name,id(11)"`
	Text          string      `parquet:"text,id(12)"`
	ReplyTo       int64       `parquet:"reply_to,id(13)"`
	ForwardedFrom string      `parquet:"forwarded_from,id(14)"`
	Media         []ChatMedia `parquet:"media,id(15),list"`
	SeenAt        time.Time   `parquet:"seen_at,id(16)"`
	RunID         string      `parquet:"run_id,id(17)"`
	ChatKey       string      `parquet:"chat_key,id(18)"`
	MsgKey        string      `parquet:"msg_key,id(19)"`
	ReplyKey      string      `parquet:"reply_key,id(20)"`
	FromKey       string      `parquet:"from_key,id(21)"`
}

// Chat is the id of the chat as text.
func (r ChatRow) Chat() string {
	if r.ChatKey != "" {
		return r.ChatKey
	}
	return strconv.FormatInt(r.ChatID, 10)
}

// Msg is the id of the message as text.
func (r ChatRow) Msg() string {
	if r.MsgKey != "" {
		return r.MsgKey
	}
	return strconv.FormatInt(r.MsgID, 10)
}

// ChatMedia is a photo, document, voice note… of a message; SHA256 names its
// blob, empty when it was not stored (Note says why).
type ChatMedia struct {
	Kind   string `parquet:"kind"`
	Name   string `parquet:"name"`
	MIME   string `parquet:"mime"`
	Size   int64  `parquet:"size"`
	SHA256 string `parquet:"sha256"`
	Note   string `parquet:"note"`
}

// ChatDoc is a message in the search index.
type ChatDoc struct {
	Kind    string    `json:"kind"`
	Source  string    `json:"source"`
	ChatID  string    `json:"chat_id"`
	Chat    string    `json:"chat"`
	MsgID   int64     `json:"msg_id"`
	MsgKey  string    `json:"msg_key"`
	From    string    `json:"from"`
	Text    string    `json:"text"`
	Media   []string  `json:"media"`
	Blobs   []string  `json:"blobs"`
	Date    time.Time `json:"date"`
	Edited  bool      `json:"edited"`
	Deleted bool      `json:"deleted"`
}

func (ChatDoc) Type() string { return "chat" }

// ChatDocID is the index id of a message.
func ChatDocID(source, chat, msg string) string {
	return "chat:" + source + ":" + chat + ":" + msg
}

// ID is the index id of the document.
func (d ChatDoc) ID() string {
	msg := d.MsgKey
	if msg == "" {
		msg = strconv.FormatInt(d.MsgID, 10)
	}
	return ChatDocID(d.Source, d.ChatID, msg)
}

func NewChatDoc(r ChatRow) ChatDoc {
	d := ChatDoc{Kind: "chat", Source: r.Source, ChatID: r.Chat(), Chat: r.ChatTitle,
		MsgID: r.MsgID, MsgKey: r.Msg(), From: r.FromName, Text: r.Text, Date: r.Date,
		Edited: !r.EditDate.IsZero() && r.EditDate.Year() > 1970, Deleted: r.Op == ChatDelete}
	for _, m := range r.Media {
		// Blobs[i] is the content of Media[i], "" when it was not stored
		d.Media = append(d.Media, m.Name)
		d.Blobs = append(d.Blobs, m.SHA256)
	}
	return d
}

// ChatPrefix is where the messages of a source live in the lake.
func ChatPrefix(source string, month time.Time) string {
	return "chat/messages/source=" + source + "/month=" + month.UTC().Format("2006-01")
}

// ChatMsgStateKey is the last version of one message, for the sources that
// receive edits and deletions without the message (WhatsApp).
func ChatMsgStateKey(chat, msg string) string { return "msg:" + chat + ":" + msg }

// ChatStateKey is the checkpoint of one chat.
func ChatStateKey(chat int64) string { return "chat:" + strconv.FormatInt(chat, 10) }

type ChatState struct {
	LastID   int64     `json:"last_id"`
	Imported bool      `json:"imported"`  // the first import reached the end of the history
	EditSeen time.Time `json:"edit_seen"` // latest edit date recorded
}
