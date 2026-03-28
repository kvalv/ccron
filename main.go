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

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "cron", "config.yaml")
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
				Name:    "config",
				Aliases: []string{"c"},
				Value:   defaultConfigPath(),
				Usage:   "path to config file",
			},
			&cli.StringFlag{
				Name:  "log-dir",
				Value: defaultLogDir(),
				Usage: "directory for task logs",
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

func loadFromCtx(ctx context.Context, cmd *cli.Command) (Config, *Runner, error) {
	cfg, err := LoadConfig(cmd.String("config"))
	if err != nil {
		return Config{}, nil, err
	}
	runner := NewRunner(cmd.String("log-dir"))
	return cfg, runner, nil
}

func cmdStart() *cli.Command {
	return &cli.Command{
		Name:  "start",
		Usage: "Start the cron scheduler daemon",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, runner, err := loadFromCtx(ctx, cmd)
			if err != nil {
				return err
			}

			configPath := cmd.String("config")
			sched := NewScheduler(runner, configPath)
			for _, task := range cfg.Tasks {
				log.Printf("scheduling %q: %s", task.Name, task.Schedule)
				if err := sched.Add(task); err != nil {
					return fmt.Errorf("add task %q: %w", task.Name, err)
				}
			}

			sched.Start()
			log.Printf("scheduler running with %d tasks", len(cfg.Tasks))

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGHUP)
			for sig := range sigCh {
				if sig == syscall.SIGHUP {
					log.Println("SIGHUP received, reloading config...")
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
		Usage:     "Run a task immediately",
		ArgsUsage: "<task-name>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() == 0 {
				return fmt.Errorf("task name required")
			}
			cfg, runner, err := loadFromCtx(ctx, cmd)
			if err != nil {
				return err
			}

			name := cmd.Args().First()
			task, ok := cfg.FindTask(name)
			if !ok {
				return fmt.Errorf("task %q not found in config", name)
			}

			log.Printf("running task %q...", name)
			return runner.Run(task)
		},
	}
}

func cmdList() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List configured tasks",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, runner, err := loadFromCtx(ctx, cmd)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSCHEDULE\tWORKDIR\tLAST RUN")
			for _, task := range cfg.Tasks {
				lastRun := "-"
				if path, err := runner.LatestLog(task.Name); err == nil {
					info, _ := os.Stat(path)
					if info != nil {
						lastRun = info.ModTime().Format("2006-01-02 15:04")
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", task.Name, task.Schedule, task.Workdir, lastRun)
			}
			return w.Flush()
		},
	}
}

func cmdLogs() *cli.Command {
	return &cli.Command{
		Name:      "logs",
		Usage:     "Show logs for a task (latest run by default)",
		ArgsUsage: "<task-name>",
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
				return fmt.Errorf("task name required")
			}
			_, runner, err := loadFromCtx(ctx, cmd)
			if err != nil {
				return err
			}

			name := cmd.Args().First()
			n := int(cmd.Int("tail"))

			if cmd.Bool("list") {
				logs, err := runner.ListLogs(name, n)
				if err != nil {
					return err
				}
				for _, l := range logs {
					fmt.Println(l)
				}
				return nil
			}

			// Show content of latest log(s)
			logs, err := runner.ListLogs(name, n)
			if err != nil {
				return err
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
