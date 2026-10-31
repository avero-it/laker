package imap

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/emersion/go-imap/v2"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

var _ out.Prober = (*Source)(nil)

// Probe logs in and lists the folders the configuration selects.
func (s *Source) Probe(ctx context.Context) (domain.Probe, error) {
	c, err := s.dial()
	if err != nil {
		return domain.Probe{}, fmt.Errorf("ERR20C: cannot connect to %s:%d: %w", s.p.Host, s.p.Port, err)
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()

	if err := c.Login(s.p.Username, s.p.Password).Wait(); err != nil {
		return domain.Probe{}, fmt.Errorf("ERR20D: login as %s failed: %w", s.p.Username, err)
	}
	defer c.Logout()
	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		return domain.Probe{}, fmt.Errorf("ERR20E: cannot list the folders: %w", err)
	}
	var selected, skipped []string
	for _, b := range boxes {
		if hasAttr(b.Attrs, imap.MailboxAttrNoSelect) || hasAttr(b.Attrs, imap.MailboxAttrNonExistent) {
			continue
		}
		if s.include.Match(b.Mailbox) && !s.exclude.Match(b.Mailbox) {
			selected = append(selected, b.Mailbox)
		} else {
			skipped = append(skipped, b.Mailbox)
		}
	}
	sort.Strings(selected)
	sort.Strings(skipped)
	p := domain.Probe{Summary: fmt.Sprintf("logged in as %s, %d of %d folders selected", s.p.Username, len(selected), len(selected)+len(skipped))}
	if len(selected) > 0 {
		p.Details = append(p.Details, "selected: "+strings.Join(selected, ", "))
	}
	if len(skipped) > 0 {
		p.Details = append(p.Details, "not selected: "+strings.Join(skipped, ", "))
	}
	if len(selected) == 0 {
		return p, fmt.Errorf("ERR20F: no folder matches %v", s.p.Folders)
	}
	return p, nil
}
