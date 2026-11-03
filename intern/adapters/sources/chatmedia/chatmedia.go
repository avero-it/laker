// Package chatmedia stores the media of chat messages (Telegram, WhatsApp)
// in the lake: streamed to a local scratch file while hashing, then put as a
// blob, never larger than the configured limit.
package chatmedia

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/avero-it/laker/intern/core/ports/out"
)

// ErrTooLarge stops a download that grows past the limit.
var ErrTooLarge = errors.New("ERR247: the media is larger than mediamaxsize")

// Store writes what fetch produces (at most max bytes) as a blob and returns
// its SHA-256 and size.
func Store(ctx context.Context, rs out.RunSink, tmpDir, prefix string, max int64, fetch func(io.Writer) error) (string, int64, error) {
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return "", 0, fmt.Errorf("ERR244: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, prefix+"-")
	if err != nil {
		return "", 0, fmt.Errorf("ERR245: %w", err)
	}
	defer os.Remove(tmp.Name()) // local scratch copy, not the lake
	defer tmp.Close()

	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(tmp, h), max: max}
	if err := fetch(cw); err != nil {
		return "", 0, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", 0, fmt.Errorf("ERR246: %w", err)
	}
	sha := hex.EncodeToString(h.Sum(nil))
	_, err = rs.PutBlob(ctx, sha, cw.n, tmp)
	return sha, cw.n, err
}

type countingWriter struct {
	w   io.Writer
	n   int64
	max int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.max {
		return 0, ErrTooLarge
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
