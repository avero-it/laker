// Package logger builds the zerolog loggers of Laker.
//
// One Factory per process owns the sinks (stdout and, optionally, the
// Betterstack writer); every component gets its own logger from it, with a
// colored console prefix. The Betterstack writer is shared, so there is one
// HTTP batcher per process and Close flushes it once on shutdown.
package logger

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/pkg/logger/betterstack"
	"github.com/avero-it/laker/pkg/version"
)

// The console prints StampNano: keep the sub-second part in the timestamps.
func init() { zerolog.TimeFieldFormat = time.RFC3339Nano }

// Outputs accepted by Options.Use.
const (
	UseConsole     = "console"
	UseBetterstack = "betterstack"
	UseStdoutJSON  = "stdout-json"
)

// Options selects the sinks:
//   - "console" (default/""): human-readable, colored, prefix-tagged text on
//     stdout only.
//   - "betterstack": the same console text on stdout, plus structured JSON
//     shipped to Betterstack's HTTP log-ingestion endpoint.
//   - "stdout-json": structured JSON on stdout only, for hosts whose log
//     agent already collects and forwards stdout.
type Options struct {
	Verbose          bool
	Use              string
	BetterstackToken string
	BetterstackHost  string
}

// Factory hands out component loggers that share the same sinks.
type Factory struct {
	app    version.AppInfo
	use    string
	level  zerolog.Level
	shared []io.Writer // sinks every component writes to besides its console
	closer func()
}

// hiddenFields are present in every JSON line but kept off the console.
var hiddenFields = map[string]struct{}{
	"appName":         {},
	"appVersion":      {},
	"commitHash":      {},
	"commitTimestamp": {},
	"goVersion":       {},
	"appFunction":     {},
	"className":       {},
}

// New builds the Factory. Close must be called on shutdown so queued lines
// reach Betterstack.
func New(app version.AppInfo, opts Options) (*Factory, error) {
	f := &Factory{app: app, use: opts.Use, level: zerolog.InfoLevel, closer: func() {}}
	if opts.Verbose {
		f.level = zerolog.DebugLevel
	}

	switch opts.Use {
	case UseConsole, "":
		f.use = UseConsole
	case UseStdoutJSON:
		f.shared = append(f.shared, os.Stdout)
	case UseBetterstack:
		bs, err := betterstack.New(opts.BetterstackToken, opts.BetterstackHost)
		if err != nil {
			return nil, fmt.Errorf("ERR731: betterstack log adapter: %w", err)
		}
		f.shared = append(f.shared, bs)
		f.closer = func() {
			if err := bs.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "betterstack: failed to close log writer: %v\n", err)
			}
		}
	default:
		return nil, fmt.Errorf("ERR732: unsupported logger: %s", opts.Use)
	}

	return f, nil
}

// For returns the logger of one component; colorFn paints its console prefix,
// e.g. color.New(color.FgCyan).SprintFunc().
func (f *Factory) For(component string, colorFn func(...interface{}) string) zerolog.Logger {
	outputs := append([]io.Writer{}, f.shared...)
	if f.use != UseStdoutJSON {
		outputs = append(outputs, newConsole(component, colorFn))
	}

	return zerolog.New(zerolog.MultiLevelWriter(outputs...)).
		Level(f.level).
		With().
		Timestamp().
		Fields([]interface{}{
			"appName", f.app.Name,
			"appVersion", f.app.Version,
			"commitHash", f.app.CommitHash,
			"commitTimestamp", f.app.CommitTimestamp,
			"goVersion", f.app.GoVersion,
			"appFunction", f.app.AppFunction,
			"className", component,
		}).
		Logger()
}

// Close flushes the shared sinks. It is safe to call once.
func (f *Factory) Close() { f.closer() }

func newConsole(component string, colorFn func(...interface{}) string) zerolog.ConsoleWriter {
	return zerolog.NewConsoleWriter(func(w *zerolog.ConsoleWriter) {
		w.Out = os.Stdout
		w.TimeFormat = time.StampNano
		w.FormatMessage = func(m interface{}) string {
			return fmt.Sprintf("%s %v", colorFn("["+component+"]"), m)
		}

		// flag to tell FormatFieldValue to skip the value of a hidden name
		var skipValue bool

		w.FormatFieldName = func(i interface{}) string {
			if s, ok := i.(string); ok {
				if _, hidden := hiddenFields[s]; hidden {
					skipValue = true
					return ""
				}
			}
			return fmt.Sprintf("%s=", i)
		}
		w.FormatFieldValue = func(i interface{}) string {
			if skipValue {
				skipValue = false
				return ""
			}
			switch v := i.(type) {
			case json.RawMessage: // bools, nulls and objects come raw
				return string(v)
			case []byte:
				return string(v)
			}
			return fmt.Sprintf("%v", i)
		}
	})
}

// Console returns a plain console logger, for the commands that run before a
// configuration is loaded (or without one).
func Console(component string, colorFn func(...interface{}) string) zerolog.Logger {
	return zerolog.New(newConsole(component, colorFn)).With().Timestamp().Logger()
}
