package whatsapp

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// spool keeps every event WhatsApp delivers on local disk before it is
// acknowledged: once acknowledged the server never sends it again, so an
// event is removed only after what it holds is committed to the lake.
type spool struct {
	dir  string
	mu   sync.Mutex
	last int64
}

func newSpool(dir string) (*spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ERR262: %w", err)
	}
	return &spool{dir: dir}, nil
}

// put writes one event durably (fsync, then rename).
func (s *spool) put(kind string, data []byte) error {
	s.mu.Lock()
	seq := max(time.Now().UnixNano(), s.last+1)
	s.last = seq
	s.mu.Unlock()
	name := filepath.Join(s.dir, fmt.Sprintf("%020d.%s", seq, kind))
	f, err := os.OpenFile(name+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("ERR263: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("ERR264: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("ERR265: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("ERR266: %w", err)
	}
	if err := os.Rename(name+".tmp", name); err != nil {
		return fmt.Errorf("ERR267: %w", err)
	}
	return nil
}

type spooled struct{ path, kind string }

// list is the events in the order they arrived.
func (s *spool) list() ([]spooled, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("ERR268: %w", err)
	}
	var out []spooled
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || strings.HasSuffix(n, ".tmp") {
			continue
		}
		dot := strings.IndexByte(n, '.')
		if dot < 0 {
			continue
		}
		out = append(out, spooled{path: filepath.Join(s.dir, n), kind: n[dot+1:]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// done removes events whose content is in the lake (local scratch, not the lake).
func (s *spool) done(evs []spooled) error {
	for _, e := range evs {
		if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("ERR269: %w", err)
		}
	}
	return nil
}
