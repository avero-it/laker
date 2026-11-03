package whatsapp

import (
	"context"
	"errors"
	"io"
	"time"
)

// conn is what the source needs from WhatsApp; meow.go implements it with
// whatsmeow, the tests with a fake.
type conn interface {
	// Self is the paired account.
	Self() string
	// Drain hands every event the server delivers to keep, which persists it
	// before it is acknowledged, until the server has sent what this device
	// missed and nothing came for idle, or wait is over.
	Drain(ctx context.Context, wait, idle time.Duration, keep func(kind string, data []byte) error) error
	// Expand turns a kept event into messages, oldest first.
	Expand(ctx context.Context, kind string, data []byte) ([]message, error)
	// Download writes the content of a media to w.
	Download(ctx context.Context, m *media, w io.Writer) error
}

// ErrNotPaired: the adapter has no session yet.
var ErrNotPaired = errors.New("ERR260: not paired with a phone: run laker-agent whatsapp login <adapter>")

// ErrLoggedOut: the phone removed this linked device.
var ErrLoggedOut = errors.New("ERR261: WhatsApp logged this device out (removed from the phone, or offline too long): run laker-agent whatsapp login <adapter> again")

type chat struct {
	ID    string // JID
	Title string
	Type  string // private | group
}

// message is a new message, or an edit (update) or a deletion of an earlier
// one, which ID then names; Date is when it happened.
type message struct {
	Chat      chat
	Op        string
	ID        string
	Date      time.Time
	FromID    string
	FromName  string
	Text      string
	ReplyTo   string
	Forwarded string
	Media     *media
}

type media struct {
	Kind string // photo | video | voice | audio | document | sticker
	Name string
	MIME string
	Size int64
	// Type and Raw are the downloadable WhatsApp message (protobuf), empty
	// when there is nothing to download.
	Type string
	Raw  []byte
}
