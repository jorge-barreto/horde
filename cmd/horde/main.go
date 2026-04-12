package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jorge-barreto/horde/internal/config"
	"github.com/jorge-barreto/horde/internal/provider"
	"github.com/jorge-barreto/horde/internal/runid"
	"github.com/jorge-barreto/horde/internal/store"
	"github.com/urfave/cli/v3"
)

func main() {
	if err := newApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:  "horde",
		Usage: "Cloud launcher for orc workflows",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "provider",
				Value: "docker",
				Usage: "Override provider selection (docker or aws-ecs)",
			},
		},
		Commands: []*cli.Command{
			launchCmd(),
			statusCmd(),
			logsCmd(),
			killCmd(),
			resultsCmd(),
			listCmd(),
		},
	}
}

func launchCmd() *cli.Command {
	return &cli.Command{
		Name:      "launch",
		Usage:     "Launch an orc workflow",
		ArgsUsage: "<ticket>",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "branch",
				Usage: "Git branch to use",
			},
			&cli.StringFlag{
				Name:  "workflow",
				Usage: "Workflow to run",
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "Timeout for the run",
				Value: 60 * time.Minute,
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "Force launch even if already running",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ticket := cmd.Args().First()
			if ticket == "" {
				return fmt.Errorf("missing required argument: <ticket>")
			}

			branch := cmd.String("branch")
			workflow := cmd.String("workflow")
			timeout := cmd.Duration("timeout")
			force := cmd.Bool("force")

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}

			repo, err := config.RepoURL(cwd)
			if err != nil {
				return err
			}

			envPath, err := config.ValidateEnvFile(cwd)
			if err != nil {
				return err
			}

			id, err := runid.Generate()
			if err != nil {
				return err
			}

			launchedBy := config.LaunchedBy(cwd)

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}
			dbPath := filepath.Join(homeDir, ".horde", "horde.db")
			st, err := store.NewSQLiteStore(dbPath)
			if err != nil {
				return err
			}
			defer st.Close()

			active, err := st.FindActiveByTicket(ctx, repo, ticket)
			if err != nil {
				return fmt.Errorf("checking active runs: %w", err)
			}
			if len(active) > 0 && !force {
				fmt.Fprintf(os.Stderr, "ticket %s already has an active run (%s)\n", ticket, active[0].ID)
				return fmt.Errorf("duplicate active ticket (use --force to override)")
			}

			now := time.Now()
			run := &store.Run{
				ID:         id,
				Repo:       repo,
				Ticket:     ticket,
				Branch:     branch,
				Workflow:   workflow,
				Provider:   "docker",
				Status:     store.StatusPending,
				LaunchedBy: launchedBy,
				StartedAt:  now,
				TimeoutAt:  now.Add(timeout),
			}
			if err := st.CreateRun(ctx, run); err != nil {
				return fmt.Errorf("recording run: %w", err)
			}

			prov := provider.NewDockerProvider()
			result, err := prov.Launch(ctx, provider.LaunchOpts{
				Repo:     repo,
				Ticket:   ticket,
				Branch:   branch,
				Workflow: workflow,
				RunID:    id,
				EnvFile:  envPath,
			})
			if err != nil {
				failedStatus := store.StatusFailed
				st.UpdateRun(ctx, id, &store.RunUpdate{Status: &failedStatus})
				return err
			}

			runningStatus := store.StatusRunning
			if err := st.UpdateRun(ctx, id, &store.RunUpdate{
				Status:     &runningStatus,
				InstanceID: &result.InstanceID,
				Metadata:   result.Metadata,
			}); err != nil {
				return fmt.Errorf("updating run status: %w", err)
			}

			fmt.Println(id)
			return nil
		},
	}
}

func statusCmd() *cli.Command {
	return &cli.Command{
		Name:      "status",
		Usage:     "Show status of a run",
		ArgsUsage: "<run-id>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("not implemented")
		},
	}
}

func logsCmd() *cli.Command {
	return &cli.Command{
		Name:      "logs",
		Usage:     "Show logs for a run",
		ArgsUsage: "<run-id>",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "follow",
				Usage: "Follow log output",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("not implemented")
		},
	}
}

func killCmd() *cli.Command {
	return &cli.Command{
		Name:      "kill",
		Usage:     "Kill a running run",
		ArgsUsage: "<run-id>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("not implemented")
		},
	}
}

func resultsCmd() *cli.Command {
	return &cli.Command{
		Name:      "results",
		Usage:     "Show results of a run",
		ArgsUsage: "<run-id>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("not implemented")
		},
	}
}

func listCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List runs",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "all",
				Usage: "List all runs",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("not implemented")
		},
	}
}
