package heartbeat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPing(t *testing.T) {
	var got []string
	status := http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		w.WriteHeader(status)
	}))
	defer ts.Close()
	ctx := context.Background()

	if err := Ping(ctx, ts.URL+"/api/v1/heartbeat/secret-token", true); err != nil {
		t.Fatal(err)
	}
	if err := Ping(ctx, ts.URL+"/api/v1/heartbeat/secret-token/", false); err != nil {
		t.Fatal(err)
	}
	if want := "GET /api/v1/heartbeat/secret-token,GET /api/v1/heartbeat/secret-token/fail"; strings.Join(got, ",") != want {
		t.Errorf("requests = %v", got)
	}

	// a refusal is tried twice, and reported without the address
	got, status = nil, http.StatusNotFound
	err := Ping(ctx, ts.URL+"/api/v1/heartbeat/secret-token", true)
	if err == nil || len(got) != 2 || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("refused ping: %v after %d requests", err, len(got))
	}

	// so is a monitor that cannot be reached
	ts.Close()
	err = Ping(ctx, ts.URL+"/api/v1/heartbeat/secret-token", true)
	if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), ts.URL) {
		t.Errorf("unreachable monitor: %v", err)
	}
}
