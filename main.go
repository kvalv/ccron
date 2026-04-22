package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/urfave/cli/v3"
)

func defaultJobsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "cron", "jobs")
}

func defaultLogDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "cron", "logs")
}

func buildApp() *cli.Command {
	return &cli.Command{
		Name:  "ccron",
		Usage: "Cron scheduler for Claude Code prompts",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "jobs-dir",
				Aliases: []string{"j"},
				Value:   defaultJobsDir(),
				Usage:   "directory containing job *.md files",
			},
			&cli.StringFlag{
				Name:  "log-dir",
				Value: defaultLogDir(),
				Usage: "directory for job logs",
			},
			&cli.IntFlag{
				Name:  "log-retention-days",
				Value: 30,
				Usage: "delete log files older than N days",
			},
		},
		Action: cmdStatus,
		Commands: []*cli.Command{
			cmdStart(),
			cmdExec(),
			cmdValidate(),
			cmdLogs(),
		},
	}
}

func main() {
	if err := buildApp().Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

func loadFromCtx(_ context.Context, cmd *cli.Command) ([]Job, []JobError, *Runner, error) {
	jobs, parseErrors, err := LoadJobs(cmd.String("jobs-dir"))
	if err != nil {
		return nil, nil, nil, err
	}
	runner := NewRunner(cmd.String("log-dir"))
	return jobs, parseErrors, runner, nil
}

// cmdStatus is the default action: print a status table and exit.
func cmdStatus(ctx context.Context, cmd *cli.Command) error {
	jobs, parseErrors, runner, err := loadFromCtx(ctx, cmd)
	if err != nil {
		return err
	}

	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
	sort.Slice(parseErrors, func(i, j int) bool { return parseErrors[i].File < parseErrors[j].File })

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCHEDULE\tNEXT RUN\tLAST RUN\tDURATION\tSTATUS")

	now := time.Now()
	for _, job := range jobs {
		nextRun := "-"
		if sched, err := cron.ParseStandard(job.Schedule); err == nil {
			nextRun = sched.Next(now).Format("2006-01-02 15:04")
		}

		lastRun, duration, status := "-", "-", "never run"
		if state, ok := runner.ReadState(job.Name); ok {
			lastRun = state.StartedAt.Format("2006-01-02 15:04")
			duration = (time.Duration(state.DurationMs) * time.Millisecond).Round(time.Millisecond).String()
			if state.ExitCode == 0 {
				status = "ok"
			} else {
				status = "FAIL"
			}
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			job.Name, job.Schedule, nextRun, lastRun, duration, status)
	}

	for _, pe := range parseErrors {
		name := strings.TrimSuffix(pe.File, ".md")
		fmt.Fprintf(w, "%s\t-\t-\t-\t-\tparse error: %s\n", name, pe.Err.Error())
	}

	return w.Flush()
}

func cmdStart() *cli.Command {
	return &cli.Command{
		Name:  "start",
		Usage: "Start the cron scheduler daemon",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			_, parseErrors, runner, err := loadFromCtx(ctx, cmd)
			if err != nil {
				return err
			}
			for _, pe := range parseErrors {
				log.Printf("parse error in %s: %v", pe.File, pe.Err)
			}

			jobsDir := cmd.String("jobs-dir")
			sched := NewScheduler(ctx, runner, jobsDir)
			if err := sched.Reload(); err != nil {
				return err
			}
			sched.Start()
			log.Printf("scheduler running with %d jobs", len(sched.ScheduledNames()))

			retention := time.Duration(cmd.Int("log-retention-days")) * 24 * time.Hour
			if err := runner.PruneLogs(retention); err != nil {
				log.Printf("prune logs: %v", err)
			}

			rescan := time.NewTicker(30 * time.Second)
			defer rescan.Stop()
			prune := time.NewTicker(1 * time.Hour)
			defer prune.Stop()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGHUP)

			for {
				select {
				case sig := <-sigCh:
					if sig == syscall.SIGHUP {
						log.Println("SIGHUP received, reloading jobs...")
						if err := sched.Reload(); err != nil {
							log.Printf("reload failed: %v", err)
						}
						continue
					}
					log.Println("shutting down...")
					sched.Stop()
					return nil
				case <-rescan.C:
					if err := sched.Reload(); err != nil {
						log.Printf("rescan reload failed: %v", err)
					}
				case <-prune.C:
					if err := runner.PruneLogs(retention); err != nil {
						log.Printf("prune logs: %v", err)
					}
				}
			}
		},
	}
}

func cmdExec() *cli.Command {
	return &cli.Command{
		Name:      "exec",
		Usage:     "Run a job immediately",
		ArgsUsage: "<job-name>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() == 0 {
				return fmt.Errorf("job name required")
			}
			jobs, _, runner, err := loadFromCtx(ctx, cmd)
			if err != nil {
				return err
			}

			name := cmd.Args().First()
			job, ok := FindJob(jobs, name)
			if !ok {
				return fmt.Errorf("job %q not found", name)
			}

			log.Printf("running job %q...", name)
			return runner.Run(ctx, job)
		},
	}
}

func cmdValidate() *cli.Command {
	return &cli.Command{
		Name:  "validate",
		Usage: "Parse all job files and report errors; exit non-zero on any failure",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			jobs, parseErrors, _, err := loadFromCtx(ctx, cmd)
			if err != nil {
				return err
			}
			for _, pe := range parseErrors {
				fmt.Fprintf(os.Stderr, "%s: %v\n", pe.File, pe.Err)
			}
			fmt.Fprintf(os.Stdout, "%d valid, %d invalid\n", len(jobs), len(parseErrors))
			if len(parseErrors) > 0 {
				return fmt.Errorf("validation failed for %d file(s)", len(parseErrors))
			}
			return nil
		},
	}
}

func cmdLogs() *cli.Command {
	return &cli.Command{
		Name:      "logs",
		Usage:     "Show logs for a job (latest run by default)",
		ArgsUsage: "<job-name>",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:    "tail",
				Aliases: []string{"n"},
				Value:   1,
				Usage:   "number of recent runs to show",
			},
			&cli.BoolFlag{
				Name:  "list",
				Usage: "list log files instead of showing content",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() == 0 {
				return fmt.Errorf("job name required")
			}
			_, _, runner, err := loadFromCtx(ctx, cmd)
			if err != nil {
				return err
			}

			name := cmd.Args().First()
			n := int(cmd.Int("tail"))

			logs, err := runner.ListLogs(name, n)
			if err != nil {
				return err
			}
			if cmd.Bool("list") {
				for _, l := range logs {
					fmt.Println(l)
				}
				return nil
			}
			for _, l := range logs {
				if n > 1 {
					fmt.Printf("=== %s ===\n", filepath.Base(l))
				}
				data, err := os.ReadFile(l)
				if err != nil {
					return err
				}
				fmt.Print(string(data))
			}
			return nil
		},
	}
}
