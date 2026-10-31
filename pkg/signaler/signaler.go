// Package signaler turns the OS signals that end the process into the
// cancellation of a context, so every component shuts down through ctx.
package signaler

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
)

// Context returns a context cancelled on SIGINT, SIGTERM or SIGHUP. SIGHUP
// ends the process too: systemd restarts it with the new configuration.
// A second signal while shutting down exits at once.
func Context(parent context.Context, logger zerolog.Logger) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		select {
		case sig := <-c:
			logger.Info().Str("signal", sig.String()).Msg("INF740: got signal, shutting down...")
			cancel()
		case <-ctx.Done():
			signal.Stop(c)
			return
		}

		sig := <-c
		logger.Warn().Str("signal", sig.String()).Msg("WRN740: got a second signal, exiting now")
		os.Exit(1)
	}()

	return ctx, cancel
}
