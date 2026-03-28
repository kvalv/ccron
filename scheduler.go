package main

import (
	"fmt"
	"log"

	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	cron       *cron.Cron
	runner     *Runner
	configPath string
}

func NewScheduler(runner *Runner, configPath string) *Scheduler {
	return &Scheduler{
		cron:       cron.New(),
		runner:     runner,
		configPath: configPath,
	}
}

func (s *Scheduler) Add(task Task) error {
	t := task
	_, err := s.cron.AddFunc(t.Schedule, func() {
		log.Printf("running scheduled task %q", t.Name)
		if err := s.runner.Run(t); err != nil {
			log.Printf("task %q error: %v", t.Name, err)
		}
	})
	return err
}

func (s *Scheduler) Reload() error {
	cfg, err := LoadConfig(s.configPath)
	if err != nil {
		return fmt.Errorf("reload config: %w", err)
	}

	// Stop old cron, create new one
	s.cron.Stop()
	s.cron = cron.New()

	for _, task := range cfg.Tasks {
		log.Printf("scheduling %q: %s", task.Name, task.Schedule)
		if err := s.Add(task); err != nil {
			return fmt.Errorf("add task %q: %w", task.Name, err)
		}
	}

	s.cron.Start()
	log.Printf("reloaded: %d tasks", len(cfg.Tasks))
	return nil
}

func (s *Scheduler) Start() {
	s.cron.Start()
}

func (s *Scheduler) Stop() {
	s.cron.Stop()
}
