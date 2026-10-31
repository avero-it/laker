// Package betterstack ships structured logs to Betterstack's HTTP
// source-ingestion endpoint (https://betterstack.com/docs/logs/http-rest-api/).
//
// The writer is designed to sit behind zerolog as a plain io.Writer: each
// Write call receives one already-encoded JSON log line. Lines are queued and
// shipped asynchronously in batches, so a slow or unreachable Betterstack
// endpoint never blocks the caller's logging (and therefore request) path.
// When the queue is full, or a batch fails to send, lines are dropped and a
// warning is printed to stderr — logging must never take the app down.
package betterstack

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultIngestingHost = "https://in.logs.betterstack.com"
	queueCapacity        = 2000
	batchSize            = 100
	flushInterval        = 2 * time.Second
	requestTimeout       = 5 * time.Second
)

// Writer is an io.Writer that ships each written line to Betterstack.
// Close must be called to flush any queued lines before the process exits.
type Writer interface {
	io.Writer
	io.Closer
}

type httpWriter struct {
	endpoint string
	token    string
	client   *http.Client

	queue chan []byte
	wg    sync.WaitGroup

	dropped atomic.Int64
}

// New creates a Writer that ships logs to the given Betterstack source. token
// is the source's ingestion token; host is the source's ingestion URL (empty
// defaults to the shared Betterstack endpoint).
func New(token, host string) (Writer, error) {
	if token == "" {
		return nil, fmt.Errorf("ERR730: betterstack: source token is required")
	}
	if host == "" {
		host = defaultIngestingHost
	}

	w := &httpWriter{
		endpoint: host,
		token:    token,
		client:   &http.Client{Timeout: requestTimeout},
		queue:    make(chan []byte, queueCapacity),
	}

	w.wg.Add(1)
	go w.run()

	return w, nil
}

// Write queues p for shipping and always reports success: a full queue drops
// the line (counted and periodically reported to stderr) rather than
// blocking or failing the caller's log call.
func (w *httpWriter) Write(p []byte) (int, error) {
	line := make([]byte, len(p))
	copy(line, p)

	select {
	case w.queue <- line:
	default:
		w.dropped.Add(1)
	}

	return len(p), nil
}

// Close stops accepting new writes, flushes whatever is already queued, and
// waits for the final flush to complete.
func (w *httpWriter) Close() error {
	close(w.queue)
	w.wg.Wait()
	return nil
}

func (w *httpWriter) run() {
	defer w.wg.Done()

	batch := make([][]byte, 0, batchSize)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case line, ok := <-w.queue:
			if !ok {
				w.flush(batch)
				return
			}
			batch = append(batch, line)
			if len(batch) >= batchSize {
				w.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				w.flush(batch)
				batch = batch[:0]
			}
			if dropped := w.dropped.Swap(0); dropped > 0 {
				fmt.Fprintf(os.Stderr, "betterstack: dropped %d log line(s), queue was full\n", dropped)
			}
		}
	}
}

func (w *httpWriter) flush(batch [][]byte) {
	if len(batch) == 0 {
		return
	}

	var body bytes.Buffer
	body.WriteByte('[')
	for i, line := range batch {
		if i > 0 {
			body.WriteByte(',')
		}
		body.Write(line)
	}
	body.WriteByte(']')

	req, err := http.NewRequest(http.MethodPost, w.endpoint, &body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "betterstack: failed to build request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.token)

	resp, err := w.client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "betterstack: failed to send %d log line(s): %v\n", len(batch), err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "betterstack: ingestion rejected %d log line(s): status %s\n", len(batch), resp.Status)
	}
}
