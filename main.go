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

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/robfig/cron/v3"
	"github.com/urfave/cli/v3"
)

func defaultBaseDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "cron")
}

func buildApp() *cli.Command {
	return &cli.Command{
		Name:  "ccron",
		Usage: "Cron scheduler for Claude Code prompts",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "base-dir",
				Aliases: []string{"b"},
				Value:   defaultBaseDir(),
				Usage:   "base directory: holds *.md job files; ccron manages logs/, state/, memory/ subdirs",
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
			cmdMemoryMCP(),
		},
	}
}

func main() {
	if err := buildApp().Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

func loadFromCtx(_ context.Context, cmd *cli.Command) ([]Job, []JobError, *Runner, error) {
	base := cmd.String("base-dir")
	jobs, parseErrors, err := LoadJobs(base)
	if err != nil {
		return nil, nil, nil, err
	}
	runner := NewRunner(base)
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

			sched := NewScheduler(ctx, runner, cmd.String("base-dir"))
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

// cmdMemoryMCP is the hidden subcommand spawned by the runner as a stdio MCP
// server for a single job's memory store. Not user-facing; exposed only so
// `claude --mcp-config <ours>` can launch it.
func cmdMemoryMCP() *cli.Command {
	return &cli.Command{
		Name:   "memory-mcp",
		Hidden: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "job", Required: true},
			&cli.StringFlag{Name: "memory-dir", Required: true},
			&cli.IntFlag{Name: "max-records", Required: true},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			store := &Store{
				Dir: cmd.String("memory-dir"),
				Cap: int(cmd.Int("max-records")),
			}
			server := buildMemoryMCPServer(store)
			return server.Run(ctx, &mcp.StdioTransport{})
		},
	}
}

func cmdLogs() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "Show recent run logs across all jobs (optionally filtered by --job)",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "job",
				Usage: "filter to a single job by name",
			},
			&cli.IntFlag{
				Name:    "tail",
				Aliases: []string{"n"},
				Value:   1,
				Usage:   "number of recent runs to show",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			_, _, runner, err := loadFromCtx(ctx, cmd)
			if err != nil {
				return err
			}

			jobFilter := cmd.String("job")
			n := int(cmd.Int("tail"))

			var logs []string
			if jobFilter != "" {
				logs, err = runner.ListLogs(jobFilter, n)
			} else {
				logs, err = runner.ListAllLogs(n)
			}
			if err != nil {
				return err
			}

			for _, l := range logs {
				// Header (job/filename) when printing more than one file so
				// the reader can tell runs apart — especially when they're
				// from different jobs.
				if len(logs) > 1 {
					rel, relErr := filepath.Rel(runner.LogDir, l)
					if relErr != nil {
						rel = filepath.Base(l)
					}
					fmt.Printf("=== %s ===\n", rel)
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
