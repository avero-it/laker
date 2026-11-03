package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/intern/coretest"
	"github.com/avero-it/laker/pkg/parquetx"
)

const token = "0123456789abcdef"

const eml = "From: Anna <anna@example.com>\r\nTo: me@example.com\r\nSubject: Quote <script>x</script>\r\n" +
	"Date: Mon, 06 Oct 2025 10:00:00 +0200\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: multipart/related; boundary=R\r\n\r\n" +
	"--R\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
	`<p onclick="evil()">Hello</p><script>alert(1)</script><img src="https://tracker.example/p.gif"><img src="cid:logo1">` + "\r\n" +
	"--R\r\nContent-Type: image/png\r\nContent-Id: <logo1>\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw0KGgo=\r\n--R--\r\n" +
	"--B\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"offer.pdf\"\r\n\r\n%PDF fake\r\n--B--\r\n"

type fakeSource struct{}

func sum(b string) string { s := sha256.Sum256([]byte(b)); return hex.EncodeToString(s[:]) }

func (fakeSource) Run(ctx context.Context, rs out.RunSink) error {
	date := time.Date(2025, 10, 6, 8, 0, 0, 0, time.UTC)
	sha := sum(eml)
	rs.PutBlob(ctx, sha, int64(len(eml)), strings.NewReader(eml))
	row := domain.MailRow{SHA256: sha, Account: "acc", Folder: "INBOX", Date: date, From: "Anna <anna@example.com>",
		Subject: "Quote <script>x</script>", BodyText: "Hello, here is the quote", SeenAt: date, RunID: rs.RunID()}
	parquetx.Append(rs, domain.MailPrefix("acc", date), row)
	rs.IndexMail(domain.NewMailDoc(row, domain.MailLocations{Accounts: []string{"acc"}, Folders: []string{"INBOX"}}))

	for i, text := range []string{"are you coming to dinner?", "yes, with the photos", "see you"} {
		cr := domain.ChatRow{Source: "tg", Service: "telegram", ChatID: -20, ChatTitle: "Family", ChatType: "group",
			MsgID: int64(i + 1), Date: date.Add(time.Duration(i) * time.Minute), FromName: "Dad", Text: text, Op: "insert", SeenAt: date, RunID: rs.RunID()}
		if i == 1 {
			cr.Media = []domain.ChatMedia{{Kind: "photo", Name: "p.jpg", SHA256: sum("text")}}
		}
		parquetx.Append(rs, domain.ChatPrefix("tg", date), cr)
		rs.IndexChat(domain.NewChatDoc(cr))
	}

	for _, f := range []struct{ path, content string }{{"/home/me/doc/page.html", "<script>alert(1)</script>"}, {"/home/me/doc/a.txt", "text"}} {
		s := sum(f.content)
		rs.PutBlob(ctx, s, int64(len(f.content)), strings.NewReader(f.content))
		fr := domain.FileRow{Source: "files", Path: f.path, Op: domain.FileAdd, SHA256: s, Size: int64(len(f.content)), Mtime: date, SeenAt: date, RunID: rs.RunID()}
		parquetx.Append(rs, domain.FilePrefix("files", date), fr)
		rs.IndexFile(domain.NewFileDoc(fr))
		rs.SetState(domain.InventoryKey(f.path), domain.MustJSON(domain.InventoryEntry{Size: fr.Size, MtimeNano: date.UnixNano(), SHA256: s}))
	}
	return nil
}

type fakeRuns struct{ triggered []string }

func (f *fakeRuns) Trigger(name string) error {
	if name != "files" {
		return core.ErrUnknownJob
	}
	f.triggered = append(f.triggered, name)
	return nil
}
func (f *fakeRuns) Running(string) bool   { return false }
func (f *fakeRuns) Next(string) time.Time { return time.Time{} }

func setup(t *testing.T) (*httptest.Server, *fakeRuns) {
	t.Helper()
	env := coretest.New(t)
	ctx := context.Background()
	if r := env.Runner.Run(ctx, core.Adapter{Name: "files", Type: "fs", Source: fakeSource{}}, false); r.Status != domain.RunOK {
		t.Fatal(r.Error)
	}
	state, index, release, err := env.Stores.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	runs := &fakeRuns{}
	srv, err := New(Deps{Lake: env.Lake, Index: index, State: state, Runs: runs, Location: time.UTC, Token: token,
		Adapters: []Adapter{{Name: "files", Type: "fs", Schedule: "@hourly", Enabled: true}}, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, runs
}

// client logs in through the form and keeps the cookie.
func client(t *testing.T, ts *httptest.Server) *http.Client {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"token": {token}, "next": {"/"}})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %v %v", resp, err)
	}
	return c
}

func body(t *testing.T, c *http.Client, u string) (int, string, http.Header) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestAuth(t *testing.T) {
	ts, _ := setup(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, _ := noRedirect.Get(ts.URL + "/?q=x")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?next=") {
		t.Errorf("unauthenticated page: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, _, _ := body(t, http.DefaultClient, ts.URL+"/api/search?q=x"); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated api: %d", code)
	}
	resp, _ = http.PostForm(ts.URL+"/login", url.Values{"token": {"wrong"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
	resp, _ = noRedirect.PostForm(ts.URL+"/login", url.Values{"token": {token}, "next": {"//evil.example"}})
	if resp.Header.Get("Location") != "/" {
		t.Errorf("open redirect: %s", resp.Header.Get("Location"))
	}
	if c := resp.Cookies(); len(c) != 1 || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie = %+v", c)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/search?q=quote", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = http.DefaultClient.Do(req)
	var res domain.SearchResult
	json.NewDecoder(resp.Body).Decode(&res)
	if res.Total != 1 {
		t.Errorf("api search: %+v", res)
	}
	// "no-referrer" makes browsers send "Origin: null" on the login form,
	// which the same-origin check refuses
	if resp, _ := http.Get(ts.URL + "/login"); resp.Header.Get("Referrer-Policy") != "same-origin" {
		t.Errorf("referrer policy = %q", resp.Header.Get("Referrer-Policy"))
	}
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")
	if resp, _ := noRedirect.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Errorf("login from an opaque origin: %d", resp.StatusCode)
	}
	if resp, _ := http.Head(ts.URL + "/"); resp.StatusCode != http.StatusOK {
		t.Errorf("health: %d", resp.StatusCode)
	}
}

func TestSearchEscapesAndHighlights(t *testing.T) {
	ts, _ := setup(t)
	c := client(t, ts)
	code, html, _ := body(t, c, ts.URL+"/?q=quote")
	if code != 200 || !strings.Contains(html, "<mark>quote</mark>") {
		t.Fatalf("search page: %d\n%s", code, html)
	}
	if strings.Contains(html, "<script>x</script>") || !strings.Contains(html, "&lt;script&gt;") {
		t.Error("subject not escaped")
	}
	if _, html, _ := body(t, c, ts.URL+"/?q="); !strings.Contains(html, "Latest") {
		t.Error("empty query should list the latest items")
	}
}

func TestMailHTMLIsSanitized(t *testing.T) {
	ts, _ := setup(t)
	c := client(t, ts)
	sha := sum(eml)

	_, page, _ := body(t, c, ts.URL+"/mail/"+sha)
	if !strings.Contains(page, "offer.pdf") || !strings.Contains(page, "Hello, here is the quote") || !strings.Contains(page, "HTML version") {
		t.Fatalf("mail page:\n%s", page)
	}

	_, page, hdr := body(t, c, ts.URL+"/mail/"+sha+"?html=1")
	for _, bad := range []string{"alert(1)", "evil()", "tracker.example"} {
		if strings.Contains(page, bad) {
			t.Errorf("%s survived the sanitizer", bad)
		}
	}
	if !strings.Contains(page, "data:image/png;base64") || !strings.Contains(page, "Load 1 remote images") {
		t.Errorf("cid image inline / remote image blocked: %s", page)
	}
	if !strings.Contains(hdr.Get("Content-Security-Policy"), "img-src 'self' data:;") {
		t.Errorf("csp = %s", hdr.Get("Content-Security-Policy"))
	}
	if _, page, hdr = body(t, c, ts.URL+"/mail/"+sha+"?html=1&images=1"); !strings.Contains(page, "tracker.example") || !strings.Contains(hdr.Get("Content-Security-Policy"), "https:") {
		t.Error("remote images on request")
	}

	_, raw, hdr := body(t, c, ts.URL+"/mail/"+sha+"/eml")
	if raw != eml || !strings.HasPrefix(hdr.Get("Content-Disposition"), "attachment") {
		t.Error("eml download")
	}
	// parts: 0 html, 1 inline image, 2 attachment
	if strings.Contains(page, "/part/1") {
		t.Error("the inline image is listed as an attachment")
	}
	code, pdf, hdr := body(t, c, ts.URL+"/mail/"+sha+"/part/2")
	if code != 200 || !strings.Contains(pdf, "%PDF") || !strings.Contains(hdr.Get("Content-Disposition"), "offer.pdf") {
		t.Errorf("attachment: %d %q %v", code, pdf, hdr)
	}
}

func TestFilesAndBlobs(t *testing.T) {
	ts, runs := setup(t)
	c := client(t, ts)

	_, page, _ := body(t, c, ts.URL+"/files/files")
	if !strings.Contains(page, "page.html") || !strings.Contains(page, "a.txt") {
		t.Fatalf("listing:\n%s", page)
	}
	if code, _, _ := body(t, c, ts.URL+"/files/nope"); code != 404 {
		t.Errorf("unknown source: %d", code)
	}
	_, page, _ = body(t, c, ts.URL+"/files/files/history?path=/home/me/doc/a.txt")
	if !strings.Contains(page, "added") {
		t.Errorf("history:\n%s", page)
	}
	_, page, _ = body(t, c, ts.URL+"/files/files?at=2025-01-01")
	if !strings.Contains(page, "No files") {
		t.Errorf("tree before the first run:\n%s", page)
	}

	// a stored HTML file is never served as HTML
	_, _, hdr := body(t, c, ts.URL+"/blob/"+sum("<script>alert(1)</script>")+"/page.html?inline=1")
	if hdr.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(hdr.Get("Content-Disposition"), "attachment") {
		t.Errorf("html blob headers: %v", hdr)
	}
	_, txt, hdr := body(t, c, ts.URL+"/blob/"+sum("text")+"/a.txt?inline=1")
	if txt != "text" || !strings.HasPrefix(hdr.Get("Content-Disposition"), "inline") {
		t.Errorf("text blob: %q %v", txt, hdr)
	}

	// manual run: same origin ok, cross-site refused
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/run/files", nil)
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := c.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site run: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/run/files", nil)
	req.Header.Set("Origin", ts.URL)
	resp, _ := c.Do(req)
	b, _ := io.ReadAll(resp.Body)
	if len(runs.triggered) != 1 || !strings.Contains(string(b), "Started: files") {
		t.Errorf("run: %v\n%s", runs.triggered, b)
	}
}

func TestChat(t *testing.T) {
	ts, _ := setup(t)
	c := client(t, ts)
	_, page, _ := body(t, c, ts.URL+"/?q=dinner&kind=chat")
	if !strings.Contains(page, "Family") || !strings.Contains(page, "/chat/tg/-20?") {
		t.Fatalf("chat hit:\n%s", page)
	}
	i := strings.Index(page, "/chat/tg/-20?")
	link := strings.ReplaceAll(page[i:i+strings.Index(page[i:], `"`)], "&amp;", "&")
	code, conv, _ := body(t, c, ts.URL+link)
	if code != 200 || !strings.Contains(conv, "are you coming to dinner?") || !strings.Contains(conv, "see you") ||
		!strings.Contains(conv, `class="hit here"`) || !strings.Contains(conv, "/blob/"+sum("text")+"/p.jpg") {
		t.Errorf("conversation (%d):\n%s", code, conv)
	}
	if code, _, _ := body(t, c, ts.URL+"/chat/tg/bad%20id%3F"); code != 404 {
		t.Errorf("bad chat id: %d", code)
	}
}
