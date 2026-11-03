package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"sort"
	"strings"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"golang.org/x/time/rate"
)

// ErrNotLoggedIn means the session file is missing or no longer valid.
var ErrNotLoggedIn = errors.New("ERR24C: not logged in to Telegram: run laker-agent telegram login <adapter>")

// client builds the MTProto client: the session file of the adapter, waits
// on FLOOD_WAIT instead of failing, and a request rate Telegram tolerates.
func (s *Source) client() *telegram.Client {
	return telegram.NewClient(s.p.APIID, s.p.APIHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: s.p.Session},
		NoUpdates:      true,
		Device:         telegram.DeviceConfig{DeviceModel: "laker-agent", SystemVersion: "laker", AppVersion: "laker"},
		Middlewares: []telegram.Middleware{
			floodwait.NewSimpleWaiter().WithMaxWait(15 * time.Minute).WithMaxRetries(10),
			ratelimit.New(rate.Every(200*time.Millisecond), 5),
		},
	})
}

// openGotd connects with the saved session and runs f; it refuses to start
// a login on its own (that needs the user: laker-agent telegram login).
func (s *Source) openGotd(ctx context.Context, f func(ctx context.Context, tg api) error) error {
	c := s.client()
	return c.Run(ctx, func(ctx context.Context) error {
		status, err := c.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("ERR24D: %w", err)
		}
		if !status.Authorized {
			return ErrNotLoggedIn
		}
		a := &gotdAPI{client: c, plain: c.API(), dl: downloader.NewDownloader()}
		a.raw = a.plain
		if status.User != nil {
			a.selfID, a.selfName = status.User.ID, userName(status.User)
		}
		return f(ctx, a)
	})
}

type gotdAPI struct {
	client   *telegram.Client
	plain    *tg.Client
	raw      *tg.Client // plain, or through the export session
	dl       *downloader.Downloader
	selfID   int64
	selfName string
	takeout  int64
}

var _ api = (*gotdAPI)(nil)

func userName(u *tg.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" && u.Username != "" {
		name = "@" + u.Username
	}
	if name == "" {
		name = fmt.Sprint(u.ID)
	}
	return name
}

func (a *gotdAPI) Self(ctx context.Context) (string, error) {
	u, err := a.client.Self(ctx)
	if err != nil {
		return "", fmt.Errorf("ERR24E: %w", err)
	}
	return userName(u), nil
}

// channelID turns a channel or supergroup id into the Bot API form.
func channelID(id int64) int64 { return -1000000000000 - id }

func (a *gotdAPI) Dialogs(ctx context.Context) ([]chat, error) {
	it := query.GetDialogs(a.raw).BatchSize(100).Iter()
	var out []chat
	for it.Next(ctx) {
		e := it.Value()
		switch p := e.Peer.(type) {
		case *tg.InputPeerSelf:
			out = append(out, chat{ID: a.selfID, Title: "Saved Messages", Type: "private", peer: p})
		case *tg.InputPeerUser:
			title := fmt.Sprint(p.UserID)
			if u, ok := e.Entities.User(p.UserID); ok {
				title = userName(u)
			}
			out = append(out, chat{ID: p.UserID, Title: title, Type: "private", peer: p})
		case *tg.InputPeerChat:
			title := fmt.Sprint(p.ChatID)
			if c, ok := e.Entities.Chat(p.ChatID); ok {
				title = c.Title
			}
			out = append(out, chat{ID: -p.ChatID, Title: title, Type: "group", peer: p})
		case *tg.InputPeerChannel:
			ch, ok := e.Entities.Channel(p.ChannelID)
			if !ok || ch.Broadcast {
				continue // channels are not read
			}
			out = append(out, chat{ID: channelID(p.ChannelID), Title: ch.Title, Type: "group", peer: p})
		}
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("ERR24F: %w", err)
	}
	return out, nil
}

func (a *gotdAPI) history(ctx context.Context, req *tg.MessagesGetHistoryRequest) ([]message, error) {
	res, err := a.raw.MessagesGetHistory(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("ERR250: %w", err)
	}
	mod, ok := res.AsModified()
	if !ok {
		return nil, nil
	}
	users := map[int64]*tg.User{}
	for _, u := range mod.GetUsers() {
		if u, ok := u.(*tg.User); ok {
			users[u.ID] = u
		}
	}
	chats := map[int64]string{}
	for _, c := range mod.GetChats() {
		switch c := c.(type) {
		case *tg.Chat:
			chats[-c.ID] = c.Title
		case *tg.Channel:
			chats[channelID(c.ID)] = c.Title
		}
	}
	var out []message
	for _, m := range mod.GetMessages() {
		if msg, ok := m.(*tg.Message); ok { // service and empty messages are skipped
			out = append(out, a.convert(msg, req.Peer, users, chats))
		}
	}
	return out, nil
}

func (a *gotdAPI) peerName(p tg.PeerClass, users map[int64]*tg.User, chats map[int64]string) (int64, string) {
	switch p := p.(type) {
	case *tg.PeerUser:
		if u, ok := users[p.UserID]; ok {
			return p.UserID, userName(u)
		}
		return p.UserID, fmt.Sprint(p.UserID)
	case *tg.PeerChat:
		return -p.ChatID, chats[-p.ChatID]
	case *tg.PeerChannel:
		return channelID(p.ChannelID), chats[channelID(p.ChannelID)]
	}
	return 0, ""
}

func (a *gotdAPI) convert(m *tg.Message, peer tg.InputPeerClass, users map[int64]*tg.User, chats map[int64]string) message {
	out := message{ID: int64(m.ID), Date: time.Unix(int64(m.Date), 0), Text: m.Message}
	if d, ok := m.GetEditDate(); ok {
		out.EditDate = time.Unix(int64(d), 0)
	}
	switch {
	case m.Out:
		out.FromID, out.FromName = a.selfID, a.selfName
	case m.FromID != nil:
		out.FromID, out.FromName = a.peerName(m.FromID, users, chats)
	default: // private chat: the other user
		if p, ok := peer.(*tg.InputPeerUser); ok {
			out.FromID, out.FromName = a.peerName(&tg.PeerUser{UserID: p.UserID}, users, chats)
		}
	}
	if r, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok {
		out.ReplyTo = int64(r.ReplyToMsgID)
	}
	if f, ok := m.GetFwdFrom(); ok {
		if name, ok := f.GetFromName(); ok {
			out.Forwarded = name
		} else if p, ok := f.GetFromID(); ok {
			_, out.Forwarded = a.peerName(p, users, chats)
		}
	}
	out.Media = mediaOf(m.Media)
	return out
}

func mediaOf(mm tg.MessageMediaClass) *media {
	switch m := mm.(type) {
	case nil:
		return nil
	case *tg.MessageMediaPhoto:
		p, ok := m.Photo.(*tg.Photo)
		if !ok {
			return nil
		}
		thumb, size := "", 0
		for _, s := range p.Sizes {
			switch s := s.(type) {
			case *tg.PhotoSize:
				if s.Size > size {
					thumb, size = s.Type, s.Size
				}
			case *tg.PhotoSizeProgressive:
				if n := len(s.Sizes); n > 0 && s.Sizes[n-1] > size {
					thumb, size = s.Type, s.Sizes[n-1]
				}
			}
		}
		return &media{Kind: "photo", Name: fmt.Sprintf("photo-%d.jpg", p.ID), MIME: "image/jpeg", Size: int64(size),
			loc: &tg.InputPhotoFileLocation{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference, ThumbSize: thumb}}
	case *tg.MessageMediaDocument:
		d, ok := m.Document.(*tg.Document)
		if !ok {
			return nil
		}
		md := &media{Kind: "document", MIME: d.MimeType, Size: d.Size,
			loc: &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}}
		for _, at := range d.Attributes {
			switch at := at.(type) {
			case *tg.DocumentAttributeFilename:
				md.Name = at.FileName
			case *tg.DocumentAttributeVideo:
				md.Kind = "video"
			case *tg.DocumentAttributeAudio:
				md.Kind = "audio"
				if at.Voice {
					md.Kind = "voice"
				}
			}
		}
		if md.Name == "" {
			ext := ".bin"
			if exts, _ := mime.ExtensionsByType(d.MimeType); len(exts) > 0 {
				ext = exts[0]
			}
			md.Name = fmt.Sprintf("%s-%d%s", md.Kind, d.ID, ext)
		}
		return md
	default:
		// a location, a contact, a poll, a web page preview: no file
		return &media{Kind: "other", Name: strings.TrimPrefix(mm.TypeName(), "messageMedia")}
	}
}

// After asks for the page just above minID: Telegram counts add_offset
// backwards from offset_id, so a negative one reads towards newer messages.
func (a *gotdAPI) After(ctx context.Context, c chat, minID int64, limit int) ([]message, error) {
	msgs, err := a.history(ctx, &tg.MessagesGetHistoryRequest{
		Peer: c.peer.(tg.InputPeerClass), OffsetID: int(minID) + 1, AddOffset: -limit, Limit: limit, MinID: int(minID),
	})
	if err != nil {
		return nil, err
	}
	var out []message
	for _, m := range msgs {
		if m.ID > minID {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// sinceCap bounds the lookback of one chat.
const sinceCap = 2000

func (a *gotdAPI) Since(ctx context.Context, c chat, t time.Time) ([]message, error) {
	var out []message
	offset := 0
	for len(out) < sinceCap {
		msgs, err := a.history(ctx, &tg.MessagesGetHistoryRequest{Peer: c.peer.(tg.InputPeerClass), OffsetID: offset, Limit: pageSize})
		if err != nil {
			return nil, err
		}
		if len(msgs) == 0 {
			return out, nil
		}
		for _, m := range msgs {
			if m.Date.Before(t) {
				return out, nil
			}
			out = append(out, m)
			offset = int(m.ID)
		}
	}
	return out, nil
}

func (a *gotdAPI) Download(ctx context.Context, m *media, w io.Writer) error {
	_, err := a.dl.Download(a.raw, m.loc.(tg.InputFileLocationClass)).Stream(ctx, w)
	if err != nil {
		return fmt.Errorf("ERR251: %w", err)
	}
	return nil
}

// takeoutInvoker sends every request inside the export session.
type takeoutInvoker struct {
	next tg.Invoker
	id   int64
}

func (t takeoutInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	obj, ok := input.(bin.Object)
	if !ok {
		return t.next.Invoke(ctx, input, output)
	}
	return t.next.Invoke(ctx, &tg.InvokeWithTakeoutRequest{TakeoutID: t.id, Query: obj}, output)
}

func (a *gotdAPI) BeginTakeout(ctx context.Context, maxFileSize int64) error {
	res, err := a.plain.AccountInitTakeoutSession(ctx, &tg.AccountInitTakeoutSessionRequest{
		MessageUsers: true, MessageChats: true, MessageMegagroups: true, Files: true, FileMaxSize: maxFileSize,
	})
	if e, ok := tgerr.AsType(err, "TAKEOUT_INIT_DELAY"); ok {
		return &TakeoutDelayError{Wait: time.Duration(e.Argument) * time.Second}
	}
	if err != nil {
		return fmt.Errorf("ERR252: cannot open the export session: %w", err)
	}
	a.takeout = res.ID
	a.raw = tg.NewClient(takeoutInvoker{next: a.client, id: res.ID})
	return nil
}

func (a *gotdAPI) EndTakeout(ctx context.Context, ok bool) error {
	if a.takeout == 0 {
		return nil
	}
	_, err := a.raw.AccountFinishTakeoutSession(ctx, &tg.AccountFinishTakeoutSessionRequest{Success: ok})
	a.raw, a.takeout = a.plain, 0
	if err != nil {
		return fmt.Errorf("ERR253: %w", err)
	}
	return nil
}
