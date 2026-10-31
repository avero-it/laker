package core

import (
	"sync"

	"github.com/avero-it/laker/intern/core/ports/out"
)

// Stores opens the local state and index on first use and closes them when
// the last user releases them. Both are single-process files: closing them
// between runs lets the CLI (search, ls, …) use them while serve is idle.
type Stores struct {
	OpenState func() (out.StateStore, error)
	OpenIndex func() (out.Indexer, error) // nil: no index

	mu    sync.Mutex
	refs  int
	state out.StateStore
	index out.Indexer
}

// Acquire returns the open stores; release must be called once when done.
func (s *Stores) Acquire() (out.StateStore, out.Indexer, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.refs == 0 {
		st, err := s.OpenState()
		if err != nil {
			return nil, nil, nil, err
		}
		var idx out.Indexer
		if s.OpenIndex != nil { // an agent has no index
			if idx, err = s.OpenIndex(); err != nil {
				st.Close()
				return nil, nil, nil, err
			}
		}
		s.state, s.index = st, idx
	}
	s.refs++

	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.refs--
			if s.refs == 0 {
				if s.index != nil {
					s.index.Close()
				}
				s.state.Close()
				s.state, s.index = nil, nil
			}
		})
	}
	return s.state, s.index, release, nil
}
