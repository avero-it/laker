// Package heartbeat tells an uptime monitor (Better Stack heartbeats,
// healthchecks.io and the like) how a run ended: the monitor raises the
// alarm when a failure is reported or when nothing arrives in time, which
// also covers a process that is no longer running.
package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Timeout bounds one attempt; a ping is tried twice.
const Timeout = 10 * time.Second

var client = &http.Client{Timeout: Timeout}

// Ping calls target when ok, target + "/fail" otherwise, as those monitors
// expect. Nothing about the run is sent. The address holds a token, so it is
// in none of the errors.
func Ping(ctx context.Context, target string, ok bool) error {
	if !ok {
		u, err := url.Parse(target)
		if err != nil {
			return errors.New("ERR790: the heartbeat address is not a URL")
		}
		u.Path = strings.TrimSuffix(u.Path, "/") + "/fail"
		target = u.String()
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err = once(ctx, target); err == nil {
			return nil
		}
	}
	return err
}

func once(ctx context.Context, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return errors.New("ERR791: the heartbeat address is not a URL")
	}
	resp, err := client.Do(req)
	if err != nil {
		// *url.Error quotes the address: keep only what went wrong
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("ERR792: heartbeat not delivered: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ERR793: the heartbeat monitor answered %d", resp.StatusCode)
	}
	return nil
}
