package main

import (
	"context"
	"encoding/json"
	"errors"
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
			runID := cmd.Args().First()
			if runID == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}
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
			run, err := st.GetRun(ctx, runID)
			if err != nil {
				if errors.Is(err, store.ErrRunNotFound) {
					return fmt.Errorf("run not found: %s", runID)
				}
				return err
			}
			prov := provider.NewDockerProvider()
			if err := handleLazyCheck(ctx, prov, st, run, homeDir); err != nil {
				return err
			}
			printRunStatus(run)
			return nil
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

func mapExitCode(code int) store.Status {
	switch code {
	case 0:
		return store.StatusSuccess
	case 5:
		return store.StatusKilled
	default:
		return store.StatusFailed
	}
}

type runResult struct {
	TotalCostUSD *float64 `json:"total_cost_usd"`
}

func handleLazyCheck(ctx context.Context, prov *provider.DockerProvider, st store.Store, run *store.Run, homeDir string) error {
	if run.Status != store.StatusPending && run.Status != store.StatusRunning {
		return nil
	}
	if run.InstanceID == "" {
		return nil
	}

	instanceStatus, err := prov.Status(ctx, run.InstanceID)
	if err != nil {
		return err
	}

	switch instanceStatus.State {
	case "stopped":
		resultsDir := filepath.Join(homeDir, ".horde", "results", run.ID)
		// Best-effort copy — errors ignored
		prov.CopyFromContainer(ctx, run.InstanceID, "/workspace/.orc/audit/.", filepath.Join(resultsDir, "audit"))
		prov.CopyFromContainer(ctx, run.InstanceID, "/workspace/.orc/artifacts/.", filepath.Join(resultsDir, "artifacts"))

		// Parse run-result.json for cost (best effort)
		var resultPath string
		if run.Workflow != "" {
			resultPath = filepath.Join(resultsDir, "audit", run.Workflow, run.Ticket, "run-result.json")
		} else {
			resultPath = filepath.Join(resultsDir, "audit", run.Ticket, "run-result.json")
		}
		var cost *float64
		if data, err := os.ReadFile(resultPath); err == nil {
			var rr runResult
			if json.Unmarshal(data, &rr) == nil {
				cost = rr.TotalCostUSD
			}
		}

		// Determine CompletedAt
		var completedAt *time.Time
		if instanceStatus.FinishedAt != nil {
			completedAt = instanceStatus.FinishedAt
		} else {
			now := time.Now()
			completedAt = &now
		}

		newStatus := mapExitCode(*instanceStatus.ExitCode)
		update := &store.RunUpdate{
			Status:       &newStatus,
			ExitCode:     instanceStatus.ExitCode,
			CompletedAt:  completedAt,
			TotalCostUSD: cost,
		}
		if err := st.UpdateRun(ctx, run.ID, update); err != nil {
			return err
		}
		// Best-effort remove — error ignored
		prov.RemoveContainer(ctx, run.InstanceID)

		// Mutate run in place
		run.Status = newStatus
		run.ExitCode = instanceStatus.ExitCode
		run.CompletedAt = completedAt
		run.TotalCostUSD = cost

	case "running":
		if time.Now().After(run.TimeoutAt) {
			resultsDir := filepath.Join(homeDir, ".horde", "results", run.ID)
			if err := prov.Kill(ctx, provider.KillOpts{InstanceID: run.InstanceID, ResultsDir: resultsDir}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: killing timed-out container: %v\n", err)
			}
			failedStatus := store.StatusFailed
			now := time.Now()
			if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{Status: &failedStatus, CompletedAt: &now}); err != nil {
				return err
			}
			run.Status = store.StatusFailed
			run.CompletedAt = &now
		}

	case "unknown":
		failedStatus := store.StatusFailed
		now := time.Now()
		if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{Status: &failedStatus, CompletedAt: &now}); err != nil {
			return err
		}
		run.Status = store.StatusFailed
		run.CompletedAt = &now
	}

	return nil
}

func printRunStatus(run *store.Run) {
	branch := run.Branch
	if branch == "" {
		branch = "(default)"
	}

	var duration time.Duration
	if run.CompletedAt != nil {
		duration = run.CompletedAt.Sub(run.StartedAt)
	} else {
		duration = time.Since(run.StartedAt)
	}
	duration = duration.Truncate(time.Second)

	fmt.Printf("Run:         %s\n", run.ID)
	fmt.Printf("Ticket:      %s\n", run.Ticket)
	fmt.Printf("Branch:      %s\n", branch)
	fmt.Printf("Status:      %s\n", run.Status)
	if run.ExitCode != nil {
		fmt.Printf("Exit code:   %d\n", *run.ExitCode)
	}
	fmt.Printf("Duration:    %s\n", duration)
	if run.TotalCostUSD != nil {
		fmt.Printf("Cost:        $%.2f\n", *run.TotalCostUSD)
	}
	fmt.Printf("Launched by: %s\n", run.LaunchedBy)
}
