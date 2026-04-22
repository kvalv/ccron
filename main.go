package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"

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

func main() {
	app := &cli.Command{
		Name:  "claude-cronjob",
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
		},
		Commands: []*cli.Command{
			cmdStart(),
			cmdExec(),
			cmdList(),
			cmdLogs(),
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
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

func cmdStart() *cli.Command {
	return &cli.Command{
		Name:  "start",
		Usage: "Start the cron scheduler daemon",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			jobs, parseErrors, runner, err := loadFromCtx(ctx, cmd)
			if err != nil {
				return err
			}
			for _, pe := range parseErrors {
				log.Printf("parse error in %s: %v", pe.File, pe.Err)
			}

			jobsDir := cmd.String("jobs-dir")
			sched := NewScheduler(runner, jobsDir)
			for _, job := range jobs {
				log.Printf("scheduling %q: %s", job.Name, job.Schedule)
				if err := sched.Add(job); err != nil {
					return fmt.Errorf("add job %q: %w", job.Name, err)
				}
			}

			sched.Start()
			log.Printf("scheduler running with %d jobs", len(jobs))

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGHUP)
			for sig := range sigCh {
				if sig == syscall.SIGHUP {
					log.Println("SIGHUP received, reloading jobs...")
					if err := sched.Reload(); err != nil {
						log.Printf("reload failed: %v", err)
					}
					continue
				}
				break
			}

			log.Println("shutting down...")
			sched.Stop()
			return nil
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
			return runner.Run(job)
		},
	}
}

func cmdList() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List configured jobs",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			jobs, _, runner, err := loadFromCtx(ctx, cmd)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSCHEDULE\tWORKDIR\tLAST RUN")
			for _, job := range jobs {
				lastRun := "-"
				if path, err := runner.LatestLog(job.Name); err == nil {
					info, _ := os.Stat(path)
					if info != nil {
						lastRun = info.ModTime().Format("2006-01-02 15:04")
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", job.Name, job.Schedule, job.Workdir, lastRun)
			}
			return w.Flush()
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
