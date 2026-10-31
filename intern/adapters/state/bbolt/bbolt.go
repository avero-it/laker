// Package bbolt is the StateStore on a local bbolt file.
package bbolt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/avero-it/laker/intern/core/ports/out"
)

var _ out.StateStore = (*Store)(nil)

type Store struct{ db *bolt.DB }

// ErrBusy means another process holds the file.
var ErrBusy = errors.New("ERR500: the state is in use by another laker process")

// Open opens (or creates) the file at path, waiting up to timeout for another
// process to release it.
func Open(path string, timeout time.Duration) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: timeout})
	if errors.Is(err, bolt.ErrTimeout) {
		return nil, ErrBusy
	}
	if err != nil {
		return nil, fmt.Errorf("ERR501: cannot open the state %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Get(bucket, key string) ([]byte, error) {
	var v []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(bucket)); b != nil {
			if raw := b.Get([]byte(key)); raw != nil {
				v = append([]byte{}, raw...)
			}
		}
		return nil
	})
	return v, err
}

func (s *Store) ForEach(bucket, prefix string, fn func(key string, value []byte) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		p := []byte(prefix)
		for k, v := c.Seek(p); k != nil && bytes.HasPrefix(k, p); k, v = c.Next() {
			if err := fn(string(k), v); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Apply(changes []out.StateChange) error {
	if len(changes) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, c := range changes {
			b, err := tx.CreateBucketIfNotExists([]byte(c.Bucket))
			if err != nil {
				return fmt.Errorf("ERR502: cannot create bucket %s: %w", c.Bucket, err)
			}
			if c.Value == nil {
				err = b.Delete([]byte(c.Key))
			} else {
				err = b.Put([]byte(c.Key), c.Value)
			}
			if err != nil {
				return fmt.Errorf("ERR503: cannot write %s/%s: %w", c.Bucket, c.Key, err)
			}
		}
		return nil
	})
}

func (s *Store) Snapshot(w io.Writer) (int64, error) {
	var n int64
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		n, err = tx.WriteTo(w)
		return err
	})
	return n, err
}

func (s *Store) Close() error { return s.db.Close() }
