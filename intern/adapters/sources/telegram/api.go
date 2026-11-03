package telegram

import (
	"context"
	"fmt"
	"io"
	"time"
)

// api is the part of Telegram the source uses. gotdAPI implements it over
// MTProto; the tests use a fake.
type api interface {
	// Self is who the session is logged in as.
	Self(ctx context.Context) (string, error)
	// Dialogs are the private chats and groups (no channel, no secret chat).
	Dialogs(ctx context.Context) ([]chat, error)
	// After returns up to limit messages with an id above minID, oldest first.
	After(ctx context.Context, c chat, minID int64, limit int) ([]message, error)
	// Since returns the messages dated since t or later, newest first.
	Since(ctx context.Context, c chat, t time.Time) ([]message, error)
	// Download writes the content of a media to w.
	Download(ctx context.Context, m *media, w io.Writer) error
	// BeginTakeout opens an export session: the calls after it go through it
	// until EndTakeout. It fails with *TakeoutDelayError when Telegram wants
	// the user to wait.
	BeginTakeout(ctx context.Context, maxFileSize int64) error
	EndTakeout(ctx context.Context, ok bool) error
}

type chat struct {
	ID    int64 // Bot API style: users >0, groups <0
	Title string
	Type  string // private | group
	peer  any    // the input peer of the implementation
}

type message struct {
	ID        int64
	Date      time.Time
	EditDate  time.Time
	FromID    int64
	FromName  string
	Text      string
	ReplyTo   int64
	Forwarded string
	Media     *media
}

type media struct {
	Kind string // photo | video | voice | audio | document | other
	Name string
	MIME string
	Size int64
	loc  any // where the implementation downloads it from
}

// TakeoutDelayError is Telegram asking to confirm the export in the app and
// to wait before it starts.
type TakeoutDelayError struct{ Wait time.Duration }

func (e *TakeoutDelayError) Error() string {
	return fmt.Sprintf("ERR240: Telegram asks to confirm the data export in the Telegram app and to wait %s; the next run tries again",
		e.Wait.Round(time.Minute))
}
