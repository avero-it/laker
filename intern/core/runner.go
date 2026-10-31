package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/parquetx"
	"github.com/avero-it/laker/pkg/redact"
)

// Adapter is a configured source ready to run.
type Adapter struct {
	Name    string
	Type    string
	Source  out.Source
	Timeout time.Duration
}

// Runner executes one run of an adapter: the source reads from its origin
// with its checkpoints in State, and delivers to Ingest.
type Runner struct {
	Ingest out.Ingest
	// State opens the store of the checkpoints for the length of a run.
	State  func() (out.StateStore, func(), error)
	Logger zerolog.Logger
	Now    func() time.Time
}

// LocalRunner is the runner of laker serve: everything in this process.
func LocalRunner(lake out.Lake, stores *Stores, logger zerolog.Logger) *Runner {
	return &Runner{
		Ingest: &LocalIngest{Lake: lake, Stores: stores},
		State: func() (out.StateStore, func(), error) {
			st, _, release, err := stores.Acquire()
			return st, release, err
		},
		Logger: logger,
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run executes the adapter once. With dryRun the source reads and compares as
// usual, but nothing is written: no blob, row, index document or state.
func (r *Runner) Run(ctx context.Context, a Adapter, dryRun bool) domain.Run {
	run := domain.Run{ID: ulid.Make().String(), Adapter: a.Name, Type: a.Type, StartedAt: r.now(), DryRun: dryRun}
	lg := r.Logger.With().Str("adapter", a.Name).Str("type", a.Type).Str("run_id", run.ID).Logger()

	finish := func(err error) domain.Run {
		run.EndedAt = r.now()
		run.Duration = run.EndedAt.Sub(run.StartedAt)
		run.Status = domain.RunOK
		if err != nil {
			// errors can quote a DSN: no credential goes into the run
			// record (sent to laker by an agent) nor into the logs
			run.Status = domain.RunFailed
			run.Error = redact.String(err.Error())
		}
		ev, msg := lg.Info(), "INF100: run finished"
		if err != nil {
			ev, msg = lg.Error().Str("error", run.Error), "ERR10D: run failed"
		}
		ev.Str("status", string(run.Status)).
			Int("rows", run.Stats.Rows).Int("files", run.Stats.Files).
			Int("blobs_new", run.Stats.BlobsNew).Int("blobs_dedup", run.Stats.BlobsDedup).
			Int64("bytes", run.Stats.Bytes).Int64("duration_ms", run.Duration.Milliseconds()).
			Bool("dry_run", dryRun).
			Msg(msg)
		return run
	}

	state, release, err := r.State()
	if err != nil {
		return finish(fmt.Errorf("ERR103: cannot open the local state: %w", err))
	}
	defer release()

	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}

	ingest := r.Ingest
	if dryRun {
		ingest = dryIngest{r.Ingest}
	}
	lg.Info().Bool("dry_run", dryRun).Msg("INF101: run started")
	rs := &runSink{runner: r, ingest: ingest, run: &run, state: state, logger: lg, dryRun: dryRun}
	rs.reset()

	err = a.Source.Run(ctx, rs)
	if err == nil {
		err = rs.Commit(ctx)
	}
	if dryRun {
		return finish(err)
	}
	if err == nil {
		run.EndedAt, run.Status = r.now(), domain.RunOK
		run.Duration = run.EndedAt.Sub(run.StartedAt)
		rec := []out.StateChange{{Bucket: domain.BucketRuns, Key: a.Name, Value: domain.MustJSON(run)}}
		if err = state.Apply(rec); err == nil {
			err = ingest.Finish(ctx, run)
		}
		return finish(err)
	}
	// a failed run is recorded too (never its checkpoint), so the status
	// pages show why; best effort: the run has failed already
	failed := finish(err)
	rec := []out.StateChange{{Bucket: domain.BucketRuns, Key: a.Name, Value: domain.MustJSON(failed)}}
	if serr := state.Apply(rec); serr != nil {
		lg.Warn().Msgf("WRN102: cannot record the failed run: %v", serr)
	}
	if ferr := ingest.Finish(context.WithoutCancel(ctx), failed); ferr != nil {
		lg.Warn().Str("error", redact.String(ferr.Error())).Msg("WRN103: cannot report the failed run")
	}
	return failed
}

// LastRun is the last recorded run of an adapter, nil if none.
func LastRun(state out.StateStore, adapter string) (*domain.Run, error) {
	b, err := state.Get(domain.BucketRuns, adapter)
	if err != nil || b == nil {
		return nil, err
	}
	var run domain.Run
	if err := json.Unmarshal(b, &run); err != nil {
		return nil, fmt.Errorf("ERR104: corrupt run record of %s: %w", adapter, err)
	}
	return &run, nil
}

var _ out.RunSink = (*runSink)(nil)

type runSink struct {
	runner *Runner
	ingest out.Ingest
	run    *domain.Run
	state  out.StateStore
	logger zerolog.Logger
	dryRun bool

	commits int // commits done, numbers the commits
	files   int // parquet files written, numbers them

	tables  map[string]parquetx.Buffer
	mail    map[string]*domain.MailDoc // pending mail docs by sha
	fileDoc map[string]domain.FileDoc
	unindex []string
	staged  map[string][]byte // checkpoint changes; nil value: delete
	stats   domain.RunStats   // since the last commit
}

func (s *runSink) reset() {
	s.tables = map[string]parquetx.Buffer{}
	s.mail = map[string]*domain.MailDoc{}
	s.fileDoc = map[string]domain.FileDoc{}
	s.unindex = nil
	if !s.dryRun || s.staged == nil {
		// a dry run never applies its state, so it keeps it staged for the
		// whole run: the source still sees its own progress
		s.staged = map[string][]byte{}
	}
	s.stats = domain.RunStats{}
}

func (s *runSink) RunID() string { return s.run.ID }

func (s *runSink) Table(key string, newBuffer func() parquetx.Buffer) parquetx.Buffer {
	b, ok := s.tables[key]
	if !ok {
		b = newBuffer()
		s.tables[key] = b
	}
	return b
}

func (s *runSink) PutBlob(ctx context.Context, sha string, size int64, r io.Reader) (bool, error) {
	created, err := s.ingest.PutBlob(ctx, sha, size, r)
	if err != nil {
		return false, err
	}
	if created {
		s.stats.BlobsNew++
		s.stats.Bytes += size
	} else {
		s.stats.BlobsDedup++
	}
	return created, nil
}

// IndexMail queues a message with the folders this run saw it in; the
// ingest merges them with the places already known.
func (s *runSink) IndexMail(doc domain.MailDoc) {
	if pending, ok := s.mail[doc.SHA256]; ok {
		for _, f := range doc.Folders {
			if !slices.Contains(pending.Folders, f) {
				pending.Folders = append(pending.Folders, f)
			}
		}
		return
	}
	doc.Kind = "mail"
	s.mail[doc.SHA256] = &doc
}

func (s *runSink) IndexFile(doc domain.FileDoc) {
	doc.Kind = "file"
	s.fileDoc[domain.FileDocID(doc.Source, doc.Path)] = doc
}

func (s *runSink) UnindexFile(id string) {
	delete(s.fileDoc, id)
	s.unindex = append(s.unindex, id)
}

func (s *runSink) bucket() string { return domain.SourceBucket(s.run.Adapter) }

func (s *runSink) GetState(key string) ([]byte, error) {
	if v, ok := s.staged[key]; ok {
		return v, nil
	}
	return s.state.Get(s.bucket(), key)
}

// ForEachState visits committed and staged keys with the prefix, staged values
// winning; the order is not specified.
func (s *runSink) ForEachState(prefix string, fn func(key string, value []byte) error) error {
	err := s.state.ForEach(s.bucket(), prefix, func(k string, v []byte) error {
		if _, staged := s.staged[k]; staged {
			return nil
		}
		return fn(k, v)
	})
	if err != nil {
		return err
	}
	for k, v := range s.staged {
		if v != nil && strings.HasPrefix(k, prefix) {
			if err := fn(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *runSink) SetState(key string, value []byte) { s.staged[key] = value }
func (s *runSink) DeleteState(key string)            { s.staged[key] = nil }

// Commit hands the parquet files and index changes to the ingest, then saves
// the checkpoints. If it fails half way, the checkpoints still point before
// this commit and the next run redoes it (the ingest is idempotent).
func (s *runSink) Commit(ctx context.Context) error {
	c := domain.Commit{
		Run: s.run.ID, Adapter: s.run.Adapter, Type: s.run.Type, Seq: s.commits,
		Unindex: s.unindex, BlobsNew: s.stats.BlobsNew, BlobsDedup: s.stats.BlobsDedup, Bytes: s.stats.Bytes,
	}
	rows := 0
	keys := make([]string, 0, len(s.tables))
	for k, b := range s.tables {
		rows += b.Len()
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var buf bytes.Buffer
		if err := s.tables[k].WriteParquet(&buf); err != nil {
			return fmt.Errorf("ERR105: cannot encode %s: %w", k, err)
		}
		c.Files = append(c.Files, domain.CommitFile{Key: fmt.Sprintf("%s/%s-%04d.parquet", k, s.run.ID, s.files), Data: buf.Bytes()})
		s.files++
	}
	for _, d := range s.mail {
		c.Mail = append(c.Mail, *d)
	}
	for _, d := range s.fileDoc {
		c.FileDocs = append(c.FileDocs, d)
	}

	s.run.Stats.Rows += rows
	s.run.Stats.BlobsNew += s.stats.BlobsNew
	s.run.Stats.BlobsDedup += s.stats.BlobsDedup
	s.run.Stats.Bytes += s.stats.Bytes

	if s.dryRun {
		s.logger.Debug().Int("rows", rows).Int("docs", len(c.Mail)+len(c.FileDocs)).Msg("DBG100: dry run, commit skipped")
		s.reset()
		return nil
	}
	if c.Empty() && len(s.staged) == 0 {
		s.reset()
		return nil
	}

	if err := s.ingest.Commit(ctx, c); err != nil {
		return err
	}
	s.run.Stats.Files += len(c.Files)

	changes := make([]out.StateChange, 0, len(s.staged))
	for k, v := range s.staged {
		changes = append(changes, out.StateChange{Bucket: s.bucket(), Key: k, Value: v})
	}
	if err := s.state.Apply(changes); err != nil {
		return fmt.Errorf("ERR108: cannot save the state: %w", err)
	}

	s.commits++
	s.run.Stats.Commits++
	s.logger.Debug().Int("seq", c.Seq).Int("rows", rows).Int("files", len(c.Files)).Int("blobs_new", c.BlobsNew).
		Msg("DBG101: committed")
	s.reset()
	return nil
}

// ErrStop can be returned by callbacks to end an iteration early.
var ErrStop = errors.New("ERR109: stop")
