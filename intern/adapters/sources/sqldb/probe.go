package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

var _ out.Prober = (*Source)(nil)

// Probe connects, opens the read-only snapshot a run would use and lists the
// tables the configuration selects.
func (s *Source) Probe(ctx context.Context) (domain.Probe, error) {
	db, err := sql.Open(s.d.driver, s.dsn)
	if err != nil {
		return domain.Probe{}, fmt.Errorf("ERR234: %w", err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return domain.Probe{}, fmt.Errorf("ERR235: cannot connect: %w", err)
	}
	defer conn.Close()
	for _, q := range s.d.begin {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return domain.Probe{}, fmt.Errorf("ERR236: cannot open a read-only snapshot: %w", err)
		}
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), s.d.commit)

	tables, err := s.tables(ctx, conn)
	if err != nil {
		return domain.Probe{}, err
	}
	p := domain.Probe{Summary: fmt.Sprintf("connected, %d tables selected", len(tables))}
	if len(tables) > 0 {
		p.Details = []string{strings.Join(tables, ", ")}
		return p, nil
	}
	return p, fmt.Errorf("ERR237: no table matches the configured patterns")
}
