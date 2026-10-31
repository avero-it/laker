package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
)

// Job is something run on a schedule, or only on demand if Schedule is nil.
type Job struct {
	Name     string
	Schedule cron.Schedule
	Run      func(ctx context.Context)
}

var (
	ErrRunning    = errors.New("ERR142: already running")
	ErrUnknownJob = errors.New("ERR143: no such job")
)

type job struct {
	Job
	mu      sync.Mutex // held while running: never two runs of the same job
	running atomic.Bool
}

// Scheduler runs jobs on their schedules and on demand (Trigger). A run due
// while the previous one of the same job is still going is skipped.
type Scheduler struct {
	location *time.Location
	logger   zerolog.Logger
	jobs     map[string]*job
	order    []string

	ctx context.Context
	wg  sync.WaitGroup
}

// NewScheduler prepares the jobs; ctx ends them (Serve returns once it is done).
func NewScheduler(ctx context.Context, loc *time.Location, logger zerolog.Logger, jobs []Job) *Scheduler {
	s := &Scheduler{location: loc, logger: logger, jobs: map[string]*job{}, ctx: ctx}
	for _, j := range jobs {
		s.jobs[j.Name] = &job{Job: j}
		s.order = append(s.order, j.Name)
	}
	return s
}

// run executes j unless it is already running.
func (s *Scheduler) run(j *job) bool {
	if !j.mu.TryLock() {
		return false
	}
	defer j.mu.Unlock()
	if s.ctx.Err() != nil {
		return true
	}
	j.running.Store(true)
	defer j.running.Store(false)
	j.Run(s.ctx)
	return true
}

// Serve blocks until the context is cancelled, then waits for the running jobs.
func (s *Scheduler) Serve() {
	c := cron.New(cron.WithLocation(s.location))
	for _, name := range s.order {
		j := s.jobs[name]
		if j.Schedule == nil {
			continue
		}
		c.Schedule(j.Schedule, cron.FuncJob(func() {
			if !s.run(j) {
				s.logger.Warn().Str("job", j.Name).Msg("WRN100: still running, this run is skipped")
			}
		}))
		s.logger.Info().Str("job", j.Name).Time("next", j.Schedule.Next(time.Now().In(s.location))).Msg("INF102: scheduled")
	}

	c.Start()
	<-s.ctx.Done()
	s.logger.Info().Msg("INF103: stopping, waiting for the running jobs")
	<-c.Stop().Done()
	s.wg.Wait()
}

// Trigger starts a run of the job now, in the background.
func (s *Scheduler) Trigger(name string) error {
	j, ok := s.jobs[name]
	if !ok {
		return ErrUnknownJob
	}
	if j.running.Load() {
		return ErrRunning
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if !s.run(j) {
			s.logger.Warn().Str("job", name).Msg("WRN101: already running, the manual run is skipped")
		}
	}()
	return nil
}

// Running tells whether the job is running now.
func (s *Scheduler) Running(name string) bool {
	j, ok := s.jobs[name]
	return ok && j.running.Load()
}

// Next is the next scheduled run of the job, zero if it runs only on demand.
func (s *Scheduler) Next(name string) time.Time {
	j, ok := s.jobs[name]
	if !ok || j.Schedule == nil {
		return time.Time{}
	}
	return j.Schedule.Next(time.Now().In(s.location))
}
