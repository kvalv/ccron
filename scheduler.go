package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	cron    *cron.Cron
	runner  *Runner
	jobsDir string
	ctx     context.Context

	mu      sync.Mutex
	entries map[string]scheduledJob // by job name
	running map[string]bool
}

type scheduledJob struct {
	job     Job
	hash    string
	entryID cron.EntryID
}

func NewScheduler(ctx context.Context, runner *Runner, jobsDir string) *Scheduler {
	return &Scheduler{
		cron:    cron.New(),
		runner:  runner,
		jobsDir: jobsDir,
		ctx:     ctx,
		entries: map[string]scheduledJob{},
		running: map[string]bool{},
	}
}

// Add schedules a job. Callers should hold s.mu, or use addLocked during
// reload. Public Add wraps addLocked with the mutex for external callers
// (currently unused outside start; kept for symmetry with earlier API).
func (s *Scheduler) Add(job Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addLocked(job)
}

func (s *Scheduler) addLocked(job Job) error {
	entryID, err := s.cron.AddFunc(job.Schedule, s.runFunc(job))
	if err != nil {
		return err
	}
	s.entries[job.Name] = scheduledJob{
		job:     job,
		hash:    jobHash(job),
		entryID: entryID,
	}
	return nil
}

// runFunc returns the closure the cron library calls on each tick. Skips the
// run if enabled_if evaluates false, or if the same job is still executing
// from a previous tick.
func (s *Scheduler) runFunc(job Job) func() {
	return func() {
		enabled, err := job.CheckEnabled(s.ctx)
		if err != nil {
			log.Printf("enabled_if for %q failed to evaluate: %v", job.Name, err)
			return
		}
		if !enabled {
			return
		}

		s.mu.Lock()
		if s.running[job.Name] {
			s.mu.Unlock()
			log.Printf("skipping %q: still running from previous tick", job.Name)
			return
		}
		s.running[job.Name] = true
		s.mu.Unlock()

		defer func() {
			s.mu.Lock()
			delete(s.running, job.Name)
			s.mu.Unlock()
		}()

		log.Printf("running scheduled job %q", job.Name)
		if err := s.runner.Run(s.ctx, job); err != nil {
			log.Printf("job %q error: %v", job.Name, err)
		}
	}
}

// Reload diffs the current schedule against the jobs directory by (name,
// content-hash). Unchanged entries keep their cron registration so their
// next-fire time isn't reset — critical with a 30s rescan, since blind
// rebuilds would silently skip ticks that land during the rescan window.
func (s *Scheduler) Reload() error {
	jobs, parseErrors, err := LoadJobs(s.jobsDir)
	if err != nil {
		return fmt.Errorf("reload jobs: %w", err)
	}
	for _, pe := range parseErrors {
		log.Printf("parse error in %s: %v", pe.File, pe.Err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	newByName := make(map[string]Job, len(jobs))
	for _, j := range jobs {
		newByName[j.Name] = j
	}

	for name, existing := range s.entries {
		newJob, present := newByName[name]
		if !present {
			log.Printf("removing job %q", name)
			s.cron.Remove(existing.entryID)
			delete(s.entries, name)
			continue
		}
		if jobHash(newJob) != existing.hash {
			log.Printf("replacing job %q (changed)", name)
			s.cron.Remove(existing.entryID)
			delete(s.entries, name)
			if err := s.addLocked(newJob); err != nil {
				return fmt.Errorf("re-add job %q: %w", name, err)
			}
		}
	}

	for name, newJob := range newByName {
		if _, present := s.entries[name]; present {
			continue
		}
		log.Printf("adding job %q: %s", name, newJob.Schedule)
		if err := s.addLocked(newJob); err != nil {
			return fmt.Errorf("add job %q: %w", name, err)
		}
	}

	log.Printf("reloaded: %d jobs", len(s.entries))
	return nil
}

// ScheduledNames returns the names of jobs currently scheduled. For tests.
func (s *Scheduler) ScheduledNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.entries))
	for n := range s.entries {
		names = append(names, n)
	}
	return names
}

// EntryID returns the cron EntryID for a scheduled job name. For tests that
// need to check identity stability across reloads.
func (s *Scheduler) EntryID(name string) (cron.EntryID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[name]
	return e.entryID, ok
}

// trigger runs the job's closure synchronously as if the cron tick had fired.
// For tests that want to exercise the overlap guard without waiting on cron.
func (s *Scheduler) trigger(name string) {
	s.mu.Lock()
	e, ok := s.entries[name]
	s.mu.Unlock()
	if !ok {
		return
	}
	s.runFunc(e.job)()
}

func (s *Scheduler) Start() {
	s.cron.Start()
}

func (s *Scheduler) Stop() {
	s.cron.Stop()
}

func jobHash(j Job) string {
	// Hash over the fields that affect scheduling or execution. Serializing
	// via JSON keeps the field-ordering stable without hand-rolling a
	// serializer.
	payload, _ := json.Marshal(struct {
		Schedule     string
		Workdir      string
		AllowedTools []string
		Timeout      time.Duration
		EnabledIf    string
		Prompt       string
		Memory       *MemoryConfig
	}{j.Schedule, j.Workdir, j.AllowedTools, j.Timeout, j.EnabledIf, j.Prompt, j.Memory})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
