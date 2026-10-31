package imap

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/intern/coretest"
)

func message(subject, body string) []byte {
	return []byte("From: John Smith <john@example.com>\r\n" +
		"To: me@example.com\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Mon, 06 Oct 2025 10:00:00 +0200\r\n" +
		"Message-ID: <" + strings.ReplaceAll(subject, " ", "-") + "@example.com>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
}

func startServer(t *testing.T) (*imapmemserver.User, int) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("me", "pw")
	mem.AddUser(user)
	for _, f := range []string{"INBOX", "Archive", "Spam"} {
		if err := user.Create(f, nil); err != nil {
			t.Fatal(err)
		}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return user, ln.Addr().(*net.TCPAddr).Port
}

func appendMsg(t *testing.T, u *imapmemserver.User, folder string, raw []byte) {
	t.Helper()
	if _, err := u.Append(folder, bytes.NewReader(raw), &imap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func newSource(t *testing.T, port int) core.Adapter {
	t.Helper()
	src, err := New("mail-test", map[string]any{
		"host": "127.0.0.1", "port": port, "security": "none",
		"username": "me", "password": "pw", "exclude": []any{"Spam"},
	}, 2, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return core.Adapter{Name: "mail-test", Type: Type, Source: src}
}

func TestIngestSearchAndResume(t *testing.T) {
	user, port := startServer(t)
	invoice := message("October invoice", "The invoices of the month are attached.")
	appendMsg(t, user, "INBOX", invoice)
	appendMsg(t, user, "INBOX", message("Dinner", "See you tonight?"))
	appendMsg(t, user, "Archive", invoice) // same message, second place
	appendMsg(t, user, "Spam", message("Offer", "Buy now"))

	env := coretest.New(t)
	ctx := context.Background()
	a := newSource(t, port)

	run := env.Runner.Run(ctx, a, false)
	if run.Status != domain.RunOK {
		t.Fatalf("run failed: %s", run.Error)
	}
	if run.Stats.Rows != 3 || run.Stats.BlobsNew != 2 || run.Stats.BlobsDedup != 1 {
		t.Fatalf("stats = %+v, want 3 rows, 2 new blobs, 1 dedup", run.Stats)
	}

	env.With(t, func(state out.StateStore, index out.Indexer) {
		res, err := index.Search(ctx, domain.Query{Text: "invoices"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Total != 1 {
			t.Fatalf("search invoices: %d hits, want 1 (one message in two folders)", res.Total)
		}
		if folders := fmt.Sprint(res.Hits[0].Fields["folder"]); !strings.Contains(folders, "INBOX") || !strings.Contains(folders, "Archive") {
			t.Errorf("folders = %s, want both", folders)
		}
		if res, _ := index.Search(ctx, domain.Query{Text: "buy"}); res.Total != 0 {
			t.Errorf("excluded folder was indexed")
		}
	})

	// nothing new: no rows
	if run := env.Runner.Run(ctx, a, false); run.Stats.Rows != 0 {
		t.Fatalf("second run rows = %d, want 0", run.Stats.Rows)
	}

	appendMsg(t, user, "INBOX", message("New", "A new message"))
	if run := env.Runner.Run(ctx, a, false); run.Stats.Rows != 1 || run.Stats.BlobsNew != 1 {
		t.Fatalf("third run = %+v, want 1 row, 1 new blob", run.Stats)
	}

	var rows []domain.MailRow
	if err := core.EachMail(ctx, env.Lake, func(r domain.MailRow) error { rows = append(rows, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows in the lake = %d, want 4", len(rows))
	}
	for _, r := range rows {
		if r.From != "John Smith <john@example.com>" || r.SHA256 == "" || r.Date.IsZero() {
			t.Errorf("row = %+v", r)
		}
	}

	// the original is in the lake, byte for byte
	key, _ := domain.BlobKey(rows[0].SHA256)
	if ok, _ := env.Lake.Exists(ctx, key); !ok {
		t.Error("blob missing")
	}

	// state rebuilt from the lake resumes at the same place
	env.With(t, func(state out.StateStore, _ out.Indexer) {
		want, _ := state.Get(domain.SourceBucket("mail-test"), domain.FolderStateKey("INBOX"))
		fresh := coretest.New(t)
		fresh.With(t, func(st out.StateStore, _ out.Indexer) {
			if _, err := core.RebuildState(ctx, env.Lake, st); err != nil {
				t.Fatal(err)
			}
			got, _ := st.Get(domain.SourceBucket("mail-test"), domain.FolderStateKey("INBOX"))
			if string(got) != string(want) {
				t.Errorf("rebuilt INBOX state = %s, want %s", got, want)
			}
		})
	})
}

func TestDryRunWritesNothing(t *testing.T) {
	user, port := startServer(t)
	appendMsg(t, user, "INBOX", message("Test", "test"))
	env := coretest.New(t)

	run := env.Runner.Run(context.Background(), newSource(t, port), true)
	if run.Status != domain.RunOK || run.Stats.Rows != 1 || run.Stats.BlobsNew != 1 {
		t.Fatalf("dry run = %+v %s", run.Stats, run.Error)
	}
	n := 0
	_ = env.Lake.List(context.Background(), "", func(string) error { n++; return nil })
	if n != 0 {
		t.Fatalf("dry run wrote %d object(s)", n)
	}
}

func TestParseMultipart(t *testing.T) {
	raw := "From: =?UTF-8?Q?Jos=C3=A9?= <jose@example.es>\r\n" +
		"To: a@example.com, B <b@example.com>\r\n" +
		"Subject: =?ISO-8859-1?Q?R=E9sum=E9?=\r\n" +
		"References: <root@x> <mid@x>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=XX\r\n\r\n" +
		"--XX\r\nContent-Type: text/html; charset=iso-8859-1\r\n\r\n" +
		"<html><style>p{}</style><p>Caf\xe9 <b>ready</b></p></html>\r\n" +
		"--XX\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"invoice.pdf\"\r\n\r\n" +
		"%PDF-1.4 fake\r\n--XX--\r\n"
	var row domain.MailRow
	parseMail([]byte(raw), &row)

	if row.Subject != "Résumé" || !strings.Contains(row.From, "José") {
		t.Errorf("headers: subject=%q from=%q", row.Subject, row.From)
	}
	if len(row.To) != 2 || row.ThreadKey != "root@x" {
		t.Errorf("to=%v thread=%q", row.To, row.ThreadKey)
	}
	if !row.HasHTML || row.BodyText != "Café ready" {
		t.Errorf("body = %q (html=%v)", row.BodyText, row.HasHTML)
	}
	if len(row.Attachments) != 1 || row.Attachments[0].Name != "invoice.pdf" || row.Attachments[0].MIME != "application/pdf" {
		t.Errorf("attachments = %+v", row.Attachments)
	}
	if row.Note != "" {
		t.Errorf("note = %q", row.Note)
	}
}

func TestParamsAreStrict(t *testing.T) {
	_, err := New("x", map[string]any{"host": "h", "username": "u", "password": "p", "folder": "INBOX"}, 1, zerolog.Nop())
	if err == nil || !strings.Contains(err.Error(), "folder") {
		t.Fatalf("err = %v, want the unknown key named", err)
	}
	s, err := New("x", map[string]any{"host": "h", "username": "u", "password": "p"}, 1, zerolog.Nop())
	if err != nil || s.p.Port != 993 || s.p.Security != "tls" {
		t.Fatalf("defaults: %+v %v", s.p, err)
	}
}
