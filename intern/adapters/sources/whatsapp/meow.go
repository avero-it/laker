package whatsapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver" // SQLite as WebAssembly, no cgo
	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/avero-it/laker/intern/core/domain"
)

const deviceName = "laker-agent"

// openStore opens (and creates) the session database, mode 0600.
func (s *Source) openStore(ctx context.Context) (*sqlstore.Container, *store.Device, error) {
	if err := os.MkdirAll(filepath.Dir(s.p.Session), 0o700); err != nil {
		return nil, nil, fmt.Errorf("ERR26E: %w", err)
	}
	f, err := os.OpenFile(s.p.Session, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("ERR26F: %w", err)
	}
	f.Close()
	if err := os.Chmod(s.p.Session, 0o600); err != nil {
		return nil, nil, fmt.Errorf("ERR270: %w", err)
	}
	db, err := sql.Open("sqlite3", "file:"+s.p.Session+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, nil, fmt.Errorf("ERR271: %w", err)
	}
	c := sqlstore.NewWithDB(db, "sqlite3", meowLog{s.logger})
	if err := c.Upgrade(ctx); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("ERR272: cannot prepare the session database: %w", err)
	}
	dev, err := c.GetFirstDevice(ctx)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("ERR273: cannot read the session: %w", err)
	}
	return c, dev, nil
}

func (s *Source) paired(ctx context.Context) (string, error) {
	c, dev, err := s.openStore(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if dev.ID == nil {
		return "", ErrNotPaired
	}
	who := "+" + dev.ID.User
	if dev.PushName != "" {
		who = dev.PushName + " (" + who + ")"
	}
	return who, nil
}

func (s *Source) client(dev *store.Device) *whatsmeow.Client {
	store.SetOSInfo(deviceName, [3]uint32{1, 0, 0})
	store.DeviceProps.RequireFullSync = proto.Bool(true)
	store.DeviceProps.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(uint32(s.p.HistoryDays))
	cli := whatsmeow.NewClient(dev, meowLog{s.logger})
	// an event is acknowledged only after it is in the spool
	cli.SynchronousAck = true
	// history blobs are downloaded from the spool, not on arrival
	cli.ManualHistorySyncDownload = true
	cli.EnableAutoReconnect = false
	return cli
}

func (s *Source) openMeow(ctx context.Context, f func(ctx context.Context, c conn) error) error {
	c, dev, err := s.openStore(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if dev.ID == nil {
		return ErrNotPaired
	}
	m := &meowConn{cli: s.client(dev), names: map[types.JID]string{}}
	defer m.cli.Disconnect()
	return f(ctx, m)
}

type meowConn struct {
	cli   *whatsmeow.Client
	names map[types.JID]string
}

func (m *meowConn) Self() string { return "+" + m.cli.Store.ID.User }

// liveMessage is a message event as spooled.
type liveMessage struct {
	Info types.MessageInfo `json:"info"`
	Raw  []byte            `json:"raw"`
}

func (m *meowConn) Drain(ctx context.Context, wait, idle time.Duration, keep func(kind string, data []byte) error) error {
	activity := make(chan struct{}, 1)
	offline := make(chan struct{})
	stop := make(chan error, 1)
	var once sync.Once
	fail := func(err error) {
		select {
		case stop <- err:
		default:
		}
	}
	id := m.cli.AddEventHandlerWithSuccessStatus(func(evt any) bool {
		switch e := evt.(type) {
		case *events.Message:
			kind, data, err := encode(e)
			if err != nil || kind == "" {
				return true
			}
			if err := keep(kind, data); err != nil {
				fail(err)
				return false // not acknowledged: the server sends it again
			}
			select {
			case activity <- struct{}{}:
			default:
			}
		case *events.OfflineSyncCompleted:
			once.Do(func() { close(offline) })
		case *events.LoggedOut:
			fail(ErrLoggedOut)
		case *events.Disconnected:
			fail(errors.New("ERR274: disconnected by WhatsApp"))
		case *events.StreamReplaced:
			fail(errors.New("ERR275: the same session connected elsewhere"))
		}
		return true
	})
	defer m.cli.RemoveEventHandler(id)

	if err := m.cli.ConnectContext(ctx); err != nil {
		return fmt.Errorf("ERR276: cannot connect to WhatsApp: %w", err)
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	var quiet <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-stop:
			return err
		case <-deadline.C:
			return nil
		case <-offline:
			offline = nil
			quiet = time.After(idle)
		case <-activity:
			if offline == nil {
				quiet = time.After(idle)
			}
		case <-quiet:
			return nil
		}
	}
}

// encode turns an event into what the spool keeps: a history notification
// (its blob is downloaded later) or a message.
func encode(e *events.Message) (string, []byte, error) {
	if n := e.Message.GetProtocolMessage().GetHistorySyncNotification(); n != nil {
		b, err := proto.Marshal(n)
		return "history", b, err
	}
	raw := e.RawMessage
	if raw == nil {
		raw = e.Message
	}
	b, err := proto.Marshal(raw)
	if err != nil {
		return "", nil, err
	}
	out, err := json.Marshal(liveMessage{Info: e.Info, Raw: b})
	return "message", out, err
}

func (m *meowConn) Expand(ctx context.Context, kind string, data []byte) ([]message, error) {
	switch kind {
	case "message":
		var lm liveMessage
		if err := json.Unmarshal(data, &lm); err != nil {
			return nil, fmt.Errorf("ERR277: %w", err)
		}
		raw := &waE2E.Message{}
		if err := proto.Unmarshal(lm.Raw, raw); err != nil {
			return nil, fmt.Errorf("ERR278: %w", err)
		}
		e := (&events.Message{Info: lm.Info, RawMessage: raw}).UnwrapRaw()
		if msg, ok := m.convert(ctx, e, ""); ok {
			return []message{msg}, nil
		}
		return nil, nil
	case "history":
		n := &waE2E.HistorySyncNotification{}
		if err := proto.Unmarshal(data, n); err != nil {
			return nil, fmt.Errorf("ERR279: %w", err)
		}
		h, err := m.cli.DownloadHistorySync(ctx, n, true)
		if err != nil {
			return nil, fmt.Errorf("ERR27A: cannot download the history: %w", err)
		}
		var out []message
		for _, conv := range h.GetConversations() {
			jid, err := types.ParseJID(conv.GetID())
			if err != nil {
				continue
			}
			for _, hm := range conv.GetMessages() {
				e, err := m.cli.ParseWebMessage(jid, hm.GetMessage())
				if err != nil {
					continue
				}
				if msg, ok := m.convert(ctx, e, conv.GetName()); ok {
					out = append(out, msg)
				}
			}
		}
		slices.SortStableFunc(out, func(a, b message) int { return a.Date.Compare(b.Date) })
		return out, nil
	}
	return nil, nil
}

func chatType(j types.JID) string {
	switch j.Server {
	case types.DefaultUserServer, types.HiddenUserServer:
		return "private"
	case types.GroupServer:
		return "group"
	}
	return "" // status, broadcast lists, channels: not read
}

// convert maps a WhatsApp message; false for what is not kept (reactions,
// receipts, status updates, channels).
func (m *meowConn) convert(ctx context.Context, e *events.Message, chatName string) (message, bool) {
	typ := chatType(e.Info.Chat)
	if typ == "" || e.Message == nil {
		return message{}, false
	}
	if chatName != "" {
		m.names[e.Info.Chat] = chatName
	}
	out := message{
		Chat: chat{ID: e.Info.Chat.ToNonAD().String(), Title: m.chatTitle(ctx, e.Info.Chat), Type: typ},
		Op:   domain.ChatInsert, ID: e.Info.ID, Date: e.Info.Timestamp,
		FromID: e.Info.Sender.ToNonAD().String(), FromName: m.senderName(ctx, e.Info),
	}
	msg := e.Message
	if pm := msg.GetProtocolMessage(); pm != nil {
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			out.Op, out.ID = domain.ChatDelete, pm.GetKey().GetID()
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			out.Op, out.ID = domain.ChatUpdate, pm.GetKey().GetID()
			out.Text = text(pm.GetEditedMessage())
		default:
			return message{}, false
		}
		return out, out.ID != ""
	}
	out.Text = text(msg)
	out.Media = mediaOf(msg)
	if ci := contextInfo(msg); ci != nil {
		out.ReplyTo = ci.GetStanzaID()
		if ci.GetIsForwarded() {
			out.Forwarded = "forwarded"
		}
	}
	return out, out.Text != "" || out.Media != nil
}

func text(msg *waE2E.Message) string {
	switch {
	case msg.GetConversation() != "":
		return msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetText()
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetCaption()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetCaption()
	case msg.GetLocationMessage() != nil:
		l := msg.GetLocationMessage()
		return strings.TrimSpace(fmt.Sprintf("location %.6f,%.6f %s %s", l.GetDegreesLatitude(), l.GetDegreesLongitude(), l.GetName(), l.GetAddress()))
	case msg.GetContactMessage() != nil:
		return "contact: " + msg.GetContactMessage().GetDisplayName()
	case msg.GetPollCreationMessage() != nil:
		return "poll: " + msg.GetPollCreationMessage().GetName()
	}
	return ""
}

type withContext interface{ GetContextInfo() *waE2E.ContextInfo }

func contextInfo(msg *waE2E.Message) *waE2E.ContextInfo {
	for _, c := range []withContext{msg.GetExtendedTextMessage(), msg.GetImageMessage(), msg.GetVideoMessage(),
		msg.GetAudioMessage(), msg.GetDocumentMessage(), msg.GetStickerMessage()} {
		if ci := c.GetContextInfo(); ci != nil {
			return ci
		}
	}
	return nil
}

func ext(mime string) string {
	mime, _, _ = strings.Cut(mime, ";")
	if _, sub, ok := strings.Cut(mime, "/"); ok && sub != "" {
		return "." + strings.TrimPrefix(sub, "x-")
	}
	return ""
}

func mediaOf(msg *waE2E.Message) *media {
	marshal := func(p proto.Message) []byte {
		b, _ := proto.Marshal(p)
		return b
	}
	switch {
	case msg.GetImageMessage() != nil:
		x := msg.GetImageMessage()
		return &media{Kind: "photo", Name: "photo" + ext(x.GetMimetype()), MIME: x.GetMimetype(), Size: int64(x.GetFileLength()), Type: "image", Raw: marshal(x)}
	case msg.GetVideoMessage() != nil:
		x := msg.GetVideoMessage()
		return &media{Kind: "video", Name: "video" + ext(x.GetMimetype()), MIME: x.GetMimetype(), Size: int64(x.GetFileLength()), Type: "video", Raw: marshal(x)}
	case msg.GetAudioMessage() != nil:
		x := msg.GetAudioMessage()
		kind := "audio"
		if x.GetPTT() {
			kind = "voice"
		}
		return &media{Kind: kind, Name: kind + ext(x.GetMimetype()), MIME: x.GetMimetype(), Size: int64(x.GetFileLength()), Type: "audio", Raw: marshal(x)}
	case msg.GetDocumentMessage() != nil:
		x := msg.GetDocumentMessage()
		name := filepath.Base(x.GetFileName())
		if name == "." || name == "/" || name == "" {
			name = "document" + ext(x.GetMimetype())
		}
		return &media{Kind: "document", Name: name, MIME: x.GetMimetype(), Size: int64(x.GetFileLength()), Type: "document", Raw: marshal(x)}
	case msg.GetStickerMessage() != nil:
		x := msg.GetStickerMessage()
		return &media{Kind: "sticker", Name: "sticker" + ext(x.GetMimetype()), MIME: x.GetMimetype(), Size: int64(x.GetFileLength()), Type: "sticker", Raw: marshal(x)}
	}
	return nil
}

func (m *meowConn) Download(ctx context.Context, md *media, w io.Writer) error {
	var dm interface {
		proto.Message
		whatsmeow.DownloadableMessage
	}
	switch md.Type {
	case "image":
		dm = &waE2E.ImageMessage{}
	case "video":
		dm = &waE2E.VideoMessage{}
	case "audio":
		dm = &waE2E.AudioMessage{}
	case "document":
		dm = &waE2E.DocumentMessage{}
	case "sticker":
		dm = &waE2E.StickerMessage{}
	default:
		return fmt.Errorf("ERR27B: unknown media type %q", md.Type)
	}
	if err := proto.Unmarshal(md.Raw, dm); err != nil {
		return fmt.Errorf("ERR27C: %w", err)
	}
	b, err := m.cli.Download(ctx, dm)
	if err != nil {
		return fmt.Errorf("ERR27D: %w", err)
	}
	_, err = w.Write(b)
	return err
}

// chatTitle is the group subject or the contact name, else the phone number.
func (m *meowConn) chatTitle(ctx context.Context, j types.JID) string {
	j = j.ToNonAD()
	if n, ok := m.names[j]; ok {
		return n
	}
	n := ""
	if m.cli == nil {
		// no connection (tests): the phone number
	} else if j.Server == types.GroupServer {
		if g, err := m.cli.GetGroupInfo(ctx, j); err == nil {
			n = g.Name
		}
	} else {
		n = m.contactName(ctx, j)
	}
	if n == "" {
		n = phone(j)
	}
	m.names[j] = n
	return n
}

func (m *meowConn) senderName(ctx context.Context, info types.MessageInfo) string {
	if info.IsFromMe {
		return "me"
	}
	if n := m.contactName(ctx, info.Sender.ToNonAD()); n != "" {
		return n
	}
	if info.PushName != "" {
		return info.PushName
	}
	return phone(info.Sender)
}

func (m *meowConn) contactName(ctx context.Context, j types.JID) string {
	if m.cli == nil {
		return ""
	}
	c, err := m.cli.Store.Contacts.GetContact(ctx, j)
	if err != nil || !c.Found {
		return ""
	}
	for _, n := range []string{c.FullName, c.FirstName, c.PushName, c.BusinessName} {
		if n != "" {
			return n
		}
	}
	return ""
}

func phone(j types.JID) string {
	if j.Server == types.DefaultUserServer {
		return "+" + j.User
	}
	return j.User
}

// meowLog forwards whatsmeow's warnings and errors; its debug output is dropped.
type meowLog struct{ lg zerolog.Logger }

func (l meowLog) Warnf(msg string, args ...any) {
	l.lg.Warn().Str("detail", fmt.Sprintf(msg, args...)).Msg("WRN263: whatsmeow")
}
func (l meowLog) Errorf(msg string, args ...any) {
	l.lg.Error().Str("detail", fmt.Sprintf(msg, args...)).Msg("ERR27E: whatsmeow")
}
func (l meowLog) Infof(string, ...any)    {}
func (l meowLog) Debugf(string, ...any)   {}
func (l meowLog) Sub(string) waLog.Logger { return l }
