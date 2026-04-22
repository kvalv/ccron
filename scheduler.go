package main

import (
	"fmt"
	"log"

	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	cron    *cron.Cron
	runner  *Runner
	jobsDir string
}

func NewScheduler(runner *Runner, jobsDir string) *Scheduler {
	return &Scheduler{
		cron:    cron.New(),
		runner:  runner,
		jobsDir: jobsDir,
	}
}

func (s *Scheduler) Add(job Job) error {
	j := job
	_, err := s.cron.AddFunc(j.Schedule, func() {
		log.Printf("running scheduled job %q", j.Name)
		if err := s.runner.Run(j); err != nil {
			log.Printf("job %q error: %v", j.Name, err)
		}
	})
	return err
}

func (s *Scheduler) Reload() error {
	jobs, parseErrors, err := LoadJobs(s.jobsDir)
	if err != nil {
		return fmt.Errorf("reload jobs: %w", err)
	}
	for _, pe := range parseErrors {
		log.Printf("parse error in %s: %v", pe.File, pe.Err)
	}

	s.cron.Stop()
	s.cron = cron.New()

	for _, job := range jobs {
		log.Printf("scheduling %q: %s", job.Name, job.Schedule)
		if err := s.Add(job); err != nil {
			return fmt.Errorf("add job %q: %w", job.Name, err)
		}
	}

	s.cron.Start()
	log.Printf("reloaded: %d jobs", len(jobs))
	return nil
}

func (s *Scheduler) Start() {
	s.cron.Start()
}

func (s *Scheduler) Stop() {
	s.cron.Stop()
}
