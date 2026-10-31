package betterstack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestNew_RequiresToken(t *testing.T) {
	if _, err := New("", ""); err == nil {
		t.Fatal("expected error when token is empty")
	}
}

func TestWriter_FlushesOnClose(t *testing.T) {
	var mu sync.Mutex
	var gotBody []json.RawMessage
	var gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	writer, err := New("test-token", server.URL)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	lines := []string{
		`{"level":"info","message":"first"}`,
		`{"level":"error","message":"second"}`,
	}
	for _, line := range lines {
		if _, err := writer.Write([]byte(line)); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-token")
	}
	if len(gotBody) != len(lines) {
		t.Fatalf("got %d line(s), want %d", len(gotBody), len(lines))
	}
}

func TestWriter_FlushesOnBatchSize(t *testing.T) {
	received := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		if len(batch) == batchSize {
			select {
			case received <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	writer, err := New("test-token", server.URL)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer writer.Close()

	for i := 0; i < batchSize; i++ {
		if _, err := writer.Write([]byte(`{"message":"line"}`)); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}

	select {
	case <-received:
	case <-time.After(flushInterval):
		t.Fatal("timed out waiting for size-triggered flush")
	}
}

func TestWriter_NeverBlocksWhenQueueFull(t *testing.T) {
	unblock := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(unblock)
		server.Close()
	}()

	writer, err := New("test-token", server.URL)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < queueCapacity*2; i++ {
			if _, err := writer.Write([]byte(`{"message":"line"}`)); err != nil {
				t.Errorf("Write() error = %v", err)
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write() blocked while queue was full")
	}
}
