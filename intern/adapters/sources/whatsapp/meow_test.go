package whatsapp

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/avero-it/laker/intern/core/domain"
)

func roundTrip(t *testing.T, m *meowConn, e *events.Message) []message {
	t.Helper()
	kind, data, err := encode(e)
	if err != nil || kind != "message" {
		t.Fatalf("encode: %s %v", kind, err)
	}
	out, err := m.Expand(context.Background(), kind, data)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConvert(t *testing.T) {
	m := &meowConn{names: map[types.JID]string{}}
	group := types.NewJID("120363000000000001", types.GroupServer)
	anna := types.NewJID("15550100001", types.DefaultUserServer)
	m.names[group] = "Family"
	info := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: group, Sender: anna, IsGroup: true},
		ID:            "ABC", PushName: "Anna", Timestamp: time.Unix(1700000000, 0),
	}

	// a photo with a caption, in reply to another message, inside an ephemeral wrapper
	raw := &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{Caption: proto.String("at the beach"), Mimetype: proto.String("image/jpeg"),
			FileLength: proto.Uint64(1234), DirectPath: proto.String("/v/t62/x"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("PREV"), IsForwarded: proto.Bool(true)}},
	}}}
	got := roundTrip(t, m, &events.Message{Info: info, RawMessage: raw})
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	g := got[0]
	if g.Chat.Title != "Family" || g.Chat.Type != "group" || g.FromName != "Anna" || g.Text != "at the beach" ||
		g.ReplyTo != "PREV" || g.Forwarded == "" || g.Op != domain.ChatInsert || !g.Date.Equal(info.Timestamp) {
		t.Errorf("photo: %+v", g)
	}
	if g.Media == nil || g.Media.Kind != "photo" || g.Media.Name != "photo.jpeg" || g.Media.Size != 1234 {
		t.Fatalf("media: %+v", g.Media)
	}
	var img waE2E.ImageMessage
	if err := proto.Unmarshal(g.Media.Raw, &img); err != nil || img.GetDirectPath() != "/v/t62/x" {
		t.Errorf("downloadable kept: %v %v", img.GetDirectPath(), err)
	}

	// an edit and a deletion name the original message
	edit := &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
			Key:           &waCommon.MessageKey{ID: proto.String("ABC")},
			EditedMessage: &waE2E.Message{Conversation: proto.String("at the lake")}},
	}}}
	info.ID = "EDIT1"
	if got := roundTrip(t, m, &events.Message{Info: info, RawMessage: edit}); len(got) != 1 || got[0].Op != domain.ChatUpdate || got[0].ID != "ABC" || got[0].Text != "at the lake" {
		t.Errorf("edit: %+v", got)
	}
	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key: &waCommon.MessageKey{ID: proto.String("ABC")}}}
	if got := roundTrip(t, m, &events.Message{Info: info, RawMessage: revoke}); len(got) != 1 || got[0].Op != domain.ChatDelete || got[0].ID != "ABC" {
		t.Errorf("revoke: %+v", got)
	}

	// a reaction, a status update: nothing
	react := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}
	if got := roundTrip(t, m, &events.Message{Info: info, RawMessage: react}); len(got) != 0 {
		t.Errorf("reaction: %+v", got)
	}
	info.Chat = types.StatusBroadcastJID
	if got := roundTrip(t, m, &events.Message{Info: info, RawMessage: &waE2E.Message{Conversation: proto.String("hi")}}); len(got) != 0 {
		t.Errorf("status: %+v", got)
	}

	// a private chat without a known name is the phone number; a voice note
	info.Chat, info.IsFromMe = anna, true
	voice := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Mimetype: proto.String("audio/ogg; codecs=opus")}}
	got = roundTrip(t, m, &events.Message{Info: info, RawMessage: voice})
	if len(got) != 1 || got[0].Chat.Title != "+15550100001" || got[0].FromName != "me" || got[0].Media.Kind != "voice" || got[0].Media.Name != "voice.ogg" {
		t.Errorf("voice: %+v %+v", got, got[0].Media)
	}
}
