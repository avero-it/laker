// Package imap is the source that copies the messages of an IMAP mailbox into
// the lake: the raw message as a blob, one row per folder occurrence, one
// document in the index.
package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/glob"
	"github.com/avero-it/laker/pkg/parquetx"
)

const Type = "imap"

type Params struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// Security is "tls" (default, port 993), "starttls" (143) or "none".
	Security string `yaml:"security"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Folders and Exclude are patterns on folder names: * and ** match
	// anything, every other character is literal ("[Gmail]/All Mail").
	Folders []string `yaml:"folders"`
	Exclude []string `yaml:"exclude"`
	// Since limits the first run of each folder to messages received from
	// this day (YYYY-MM-DD); empty means everything.
	Since string `yaml:"since"`
	// MaxSize skips messages larger than this many bytes (default 100 MB):
	// their row records the skip, no blob is stored.
	MaxSize int64 `yaml:"maxsize"`
}

type Source struct {
	name        string
	p           Params
	since       time.Time
	include     glob.Set
	exclude     glob.Set
	commitEvery int
	logger      zerolog.Logger
	now         func() time.Time
}

var _ out.Source = (*Source)(nil)

func New(name string, params map[string]any, commitEvery int, logger zerolog.Logger) (*Source, error) {
	var p Params
	if err := config.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Host == "" || p.Username == "" || p.Password == "" {
		return nil, fmt.Errorf("ERR200: host, username and password are required")
	}
	switch p.Security {
	case "":
		p.Security = "tls"
	case "tls", "starttls", "none":
	default:
		return nil, fmt.Errorf("ERR201: security %q must be tls, starttls or none", p.Security)
	}
	if p.Port == 0 {
		p.Port = 993
		if p.Security != "tls" {
			p.Port = 143
		}
	}
	if len(p.Folders) == 0 {
		p.Folders = []string{"**"}
	}
	if p.MaxSize == 0 {
		p.MaxSize = 100 << 20
	}
	s := &Source{
		name: name, p: p, include: glob.CompileAll(p.Folders), exclude: glob.CompileAll(p.Exclude),
		commitEvery: commitEvery, logger: logger, now: time.Now,
	}
	if p.Since != "" {
		t, err := time.Parse("2006-01-02", p.Since)
		if err != nil {
			return nil, fmt.Errorf("ERR202: since %q must be YYYY-MM-DD", p.Since)
		}
		s.since = t
	}
	return s, nil
}

func (s *Source) dial() (*imapclient.Client, error) {
	addr := net.JoinHostPort(s.p.Host, strconv.Itoa(s.p.Port))
	opts := &imapclient.Options{Dialer: &net.Dialer{Timeout: 30 * time.Second}}
	switch s.p.Security {
	case "starttls":
		return imapclient.DialStartTLS(addr, opts)
	case "none":
		return imapclient.DialInsecure(addr, opts)
	default:
		return imapclient.DialTLS(addr, opts)
	}
}

func (s *Source) Run(ctx context.Context, rs out.RunSink) error {
	c, err := s.dial()
	if err != nil {
		return fmt.Errorf("ERR203: cannot connect to %s: %w", s.p.Host, err)
	}
	defer c.Close()
	// a cancelled run must not wait for a slow server
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()

	if err := c.Login(s.p.Username, s.p.Password).Wait(); err != nil {
		return fmt.Errorf("ERR204: login as %s failed: %w", s.p.Username, err)
	}

	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		return fmt.Errorf("ERR205: cannot list the folders: %w", err)
	}
	var folders []string
	for _, b := range boxes {
		if hasAttr(b.Attrs, imap.MailboxAttrNoSelect) || hasAttr(b.Attrs, imap.MailboxAttrNonExistent) {
			continue
		}
		if s.include.Match(b.Mailbox) && !s.exclude.Match(b.Mailbox) {
			folders = append(folders, b.Mailbox)
		}
	}
	sort.Strings(folders)
	s.logger.Debug().Strs("folders", folders).Msg("DBG200: folders to read")

	for _, f := range folders {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.folder(ctx, c, rs, f); err != nil {
			return fmt.Errorf("ERR206: folder %s: %w", f, err)
		}
	}

	if err := c.Logout().Wait(); err != nil {
		s.logger.Debug().Msgf("DBG201: logout: %v", err)
	}
	return nil
}

func (s *Source) folder(ctx context.Context, c *imapclient.Client, rs out.RunSink, folder string) error {
	sel, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return fmt.Errorf("ERR207: select: %w", err)
	}

	key := domain.FolderStateKey(folder)
	var st domain.FolderState
	if b, err := rs.GetState(key); err != nil {
		return err
	} else if b != nil {
		if err := json.Unmarshal(b, &st); err != nil {
			return fmt.Errorf("ERR208: corrupt state of %s: %w", folder, err)
		}
	}
	if st.UIDValidity != int64(sel.UIDValidity) {
		if st.UIDValidity != 0 {
			s.logger.Warn().Str("folder", folder).Msg("WRN200: UIDVALIDITY changed, reading the folder again (blobs are not duplicated)")
		}
		st = domain.FolderState{UIDValidity: int64(sel.UIDValidity)}
		rs.SetState(key, domain.MustJSON(st))
	}
	if sel.NumMessages == 0 {
		return nil
	}

	criteria := &imap.SearchCriteria{UID: []imap.UIDSet{{{Start: imap.UID(st.LastUID + 1), Stop: 0}}}}
	if st.LastUID == 0 && !s.since.IsZero() {
		criteria.Since = s.since
	}
	data, err := c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return fmt.Errorf("ERR209: search: %w", err)
	}
	var uids []imap.UID
	for _, u := range data.AllUIDs() {
		// "n:*" always matches the last message, even below n
		if int64(u) > st.LastUID {
			uids = append(uids, u)
		}
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	if len(uids) == 0 {
		return nil
	}
	s.logger.Info().Str("folder", folder).Int("new", len(uids)).Msg("INF200: new messages")

	for start := 0; start < len(uids); start += s.commitEvery {
		chunk := uids[start:min(start+s.commitEvery, len(uids))]
		if err := s.fetch(ctx, c, rs, folder, &st, chunk); err != nil {
			return err
		}
		rs.SetState(key, domain.MustJSON(st))
		if err := rs.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Source) fetch(ctx context.Context, c *imapclient.Client, rs out.RunSink, folder string, st *domain.FolderState, uids []imap.UID) error {
	meta, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		UID: true, Flags: true, InternalDate: true, RFC822Size: true,
	}).Collect()
	if err != nil {
		return fmt.Errorf("ERR20A: fetch: %w", err)
	}
	byUID := map[imap.UID]*imapclient.FetchMessageBuffer{}
	var wanted []imap.UID
	for _, m := range meta {
		byUID[m.UID] = m
		if m.RFC822Size <= s.p.MaxSize {
			wanted = append(wanted, m.UID)
		}
	}

	section := &imap.FetchItemBodySection{Peek: true}
	bodies := map[imap.UID][]byte{}
	if len(wanted) > 0 {
		msgs, err := c.Fetch(imap.UIDSetNum(wanted...), &imap.FetchOptions{
			UID: true, BodySection: []*imap.FetchItemBodySection{section},
		}).Collect()
		if err != nil {
			return fmt.Errorf("ERR20B: fetch bodies: %w", err)
		}
		for _, m := range msgs {
			bodies[m.UID] = m.FindBodySection(section)
		}
	}

	for _, uid := range uids {
		m, ok := byUID[uid]
		if !ok {
			continue // expunged meanwhile
		}
		row := domain.MailRow{
			Account: s.name, Mailbox: s.p.Username, Folder: folder,
			UIDValidity: st.UIDValidity, UID: int64(uid), Flags: flags(m.Flags),
			Size: m.RFC822Size, InternalDate: m.InternalDate, SeenAt: s.now().UTC(), RunID: rs.RunID(),
		}

		raw, ok := bodies[uid]
		switch {
		case m.RFC822Size > s.p.MaxSize:
			row.Note = fmt.Sprintf("too large: %d bytes", m.RFC822Size)
			s.logger.Warn().Str("folder", folder).Int64("uid", row.UID).Int64("size", m.RFC822Size).Msg("WRN201: message too large, not stored")
		case !ok:
			row.Note = "body not returned by the server"
		default:
			sum := sha256.Sum256(raw)
			row.SHA256 = hex.EncodeToString(sum[:])
			if _, err := rs.PutBlob(ctx, row.SHA256, int64(len(raw)), bytes.NewReader(raw)); err != nil {
				return err
			}
			parseMail(raw, &row)
		}

		month := row.Date
		if month.IsZero() || month.Year() < 1971 {
			month = row.InternalDate
		}
		if month.IsZero() {
			month = row.SeenAt
		}
		parquetx.Append(rs, domain.MailPrefix(s.name, month), row)
		if row.SHA256 != "" {
			rs.IndexMail(domain.NewMailDoc(row, domain.MailLocations{Accounts: []string{s.name}, Folders: []string{folder}}))
		}
		st.LastUID = int64(uid)
	}
	return nil
}

func hasAttr(attrs []imap.MailboxAttr, a imap.MailboxAttr) bool {
	for _, x := range attrs {
		if x == a {
			return true
		}
	}
	return false
}

func flags(f []imap.Flag) []string {
	out := make([]string, len(f))
	for i, x := range f {
		out[i] = string(x)
	}
	return out
}
