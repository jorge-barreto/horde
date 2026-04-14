package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	horde "github.com/jorge-barreto/horde"
	"github.com/jorge-barreto/horde/internal/awscfg"
	"github.com/jorge-barreto/horde/internal/config"
	"github.com/jorge-barreto/horde/internal/docs"
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
		Description: `horde runs orc workflows on ephemeral Docker containers (or ECS Fargate
in v0.2). It clones a repo, runs orc, collects results, and tears down.

horde must be run from inside a git repository — the repo URL is inferred
from the local git remote. Run 'horde docs' for detailed documentation.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "provider",
				Value: "docker",
				Usage: "Override provider selection (docker or aws-ecs)",
			},
			&cli.StringFlag{
				Name:  "profile",
				Usage: "AWS named profile (passed through to AWS SDK)",
			},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			p := cmd.String("provider")
			if p != "docker" {
				return ctx, fmt.Errorf("unsupported provider %q: only \"docker\" is supported in v0.1", p)
			}
			return ctx, nil
		},
		Commands: []*cli.Command{
			launchCmd(),
			resumeCmd(),
			statusCmd(),
			logsCmd(),
			killCmd(),
			resultsCmd(),
			listCmd(),
			docsCmd(),
		},
	}
}

func launchCmd() *cli.Command {
	return &cli.Command{
		Name:      "launch",
		Usage:     "Launch an orc workflow",
		ArgsUsage: "<ticket>",
		Description: `Builds the worker Docker image if needed, validates the .env file,
and launches a container that clones the repo and runs orc. Prints the
run ID on success. Use --force to launch even if a run with the same
ticket is already active.`,
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

			launchedBy, err := resolveLaunchedBy(ctx, cmd.String("provider"), cwd, nil, cmd.String("profile"))
			if err != nil {
				return err
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

			workerFS, err := fs.Sub(horde.WorkerFiles, "docker")
			if err != nil {
				return fmt.Errorf("accessing worker files: %w", err)
			}
			if err := prov.EnsureImage(ctx, workerFS, cwd, os.Stderr); err != nil {
				failedStatus := store.StatusFailed
				now := time.Now()
				if updateErr := st.UpdateRun(ctx, id, &store.RunUpdate{Status: &failedStatus, CompletedAt: &now}); updateErr != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to mark run as failed: %v\n", updateErr)
				}
				return fmt.Errorf("preparing worker image: %w", err)
			}

			projCfg, err := config.LoadProjectConfig(cwd)
			if err != nil {
				return err
			}

			result, err := prov.Launch(ctx, provider.LaunchOpts{
				Repo:     repo,
				Ticket:   ticket,
				Branch:   branch,
				Workflow: workflow,
				RunID:    id,
				EnvFile:  envPath,
				Mounts:   projCfg.ResolveMounts(cwd),
			})
			if err != nil {
				failedStatus := store.StatusFailed
				now := time.Now()
				if updateErr := st.UpdateRun(ctx, id, &store.RunUpdate{Status: &failedStatus, CompletedAt: &now}); updateErr != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to mark run as failed: %v\n", updateErr)
				}
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

func resumeCmd() *cli.Command {
	return &cli.Command{
		Name:      "resume",
		Usage:     "Resume a failed or killed run",
		ArgsUsage: "<run-id>",
		Description: `Creates a new run that picks up where the previous one left off. Clones
from the horde/<ticket> branch to recover committed code, restores orc
artifacts and audit data, and retries from the failed phase if detected
in the previous run's results. See 'horde docs resume' for details.`,
		Flags: []cli.Flag{
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "Timeout for the resumed run",
				Value: 60 * time.Minute,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			prevID := cmd.Args().First()
			if prevID == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}
			timeout := cmd.Duration("timeout")

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
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

			prev, err := st.GetRun(ctx, prevID)
			if err != nil {
				if errors.Is(err, store.ErrRunNotFound) {
					return fmt.Errorf("run not found: %s", prevID)
				}
				return fmt.Errorf("reading run: %w", err)
			}
			if prev.Status == store.StatusRunning || prev.Status == store.StatusPending {
				return fmt.Errorf("run %s is still %s — kill it first or wait for completion", prevID, prev.Status)
			}

			resumeDir := filepath.Join(homeDir, ".horde", "results", prevID)
			if _, err := os.Stat(resumeDir); err != nil {
				return fmt.Errorf("no results found for run %s — nothing to resume from", prevID)
			}

			// Find the failed phase from the previous run's result
			var retryPhase string
			var resultPath string
			if prev.Workflow != "" {
				resultPath = filepath.Join(resumeDir, "audit", prev.Workflow, prev.Ticket, "run-result.json")
			} else {
				resultPath = filepath.Join(resumeDir, "audit", prev.Ticket, "run-result.json")
			}
			if data, err := os.ReadFile(resultPath); err == nil {
				var rr struct {
					FailedPhase string `json:"failed_phase"`
				}
				if json.Unmarshal(data, &rr) == nil && rr.FailedPhase != "" {
					retryPhase = rr.FailedPhase
				}
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

			prov := provider.NewDockerProvider()

			workerFS, err := fs.Sub(horde.WorkerFiles, "docker")
			if err != nil {
				return fmt.Errorf("accessing worker files: %w", err)
			}
			if err := prov.EnsureImage(ctx, workerFS, cwd, os.Stderr); err != nil {
				failedStatus := store.StatusFailed
				now := time.Now()
				st.UpdateRun(ctx, id, &store.RunUpdate{Status: &failedStatus, CompletedAt: &now})
				return fmt.Errorf("preparing worker image: %w", err)
			}

			launchedBy, err := resolveLaunchedBy(ctx, cmd.String("provider"), cwd, nil, cmd.String("profile"))
			if err != nil {
				return err
			}
			now := time.Now()
			run := &store.Run{
				ID:         id,
				Repo:       repo,
				Ticket:     prev.Ticket,
				Branch:     prev.Branch,
				Workflow:   prev.Workflow,
				Provider:   "docker",
				Status:     store.StatusPending,
				LaunchedBy: launchedBy,
				StartedAt:  now,
				TimeoutAt:  now.Add(timeout),
			}
			if err := st.CreateRun(ctx, run); err != nil {
				return fmt.Errorf("recording run: %w", err)
			}

			projCfg, err := config.LoadProjectConfig(cwd)
			if err != nil {
				return err
			}

			resumeBranch := "horde/" + prev.Ticket
			result, err := prov.Launch(ctx, provider.LaunchOpts{
				Repo:       repo,
				Ticket:     prev.Ticket,
				Branch:     resumeBranch,
				Workflow:   prev.Workflow,
				RunID:      id,
				EnvFile:    envPath,
				Mounts:     projCfg.ResolveMounts(cwd),
				ResumeDir:  resumeDir,
				RetryPhase: retryPhase,
			})
			if err != nil {
				failedStatus := store.StatusFailed
				now := time.Now()
				if updateErr := st.UpdateRun(ctx, id, &store.RunUpdate{Status: &failedStatus, CompletedAt: &now}); updateErr != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to mark run as failed: %v\n", updateErr)
				}
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

			if retryPhase != "" {
				fmt.Fprintf(os.Stderr, "Resuming %s from phase %q (run %s)\n", prev.Ticket, retryPhase, prevID)
			} else {
				fmt.Fprintf(os.Stderr, "Resuming %s from run %s\n", prev.Ticket, prevID)
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
		Description: `Shows run detail: ID, ticket, status, exit code, duration, cost, and
who launched it. For running containers, reads live cost from the
container. Also detects completed or timed-out runs and triggers
result collection.`,
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
				return fmt.Errorf("opening store: %w", err)
			}
			defer st.Close()
			run, err := st.GetRun(ctx, runID)
			if err != nil {
				if errors.Is(err, store.ErrRunNotFound) {
					return fmt.Errorf("run not found: %s", runID)
				}
				return fmt.Errorf("reading run: %w", err)
			}
			prov := provider.NewDockerProvider()
			if err := handleLazyCheck(ctx, prov, st, run, homeDir); err != nil {
				return err
			}
			if run.TotalCostUSD == nil && (run.Status == store.StatusRunning || run.Status == store.StatusPending) {
				run.TotalCostUSD = fetchLiveCost(ctx, prov, run)
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
		Description: `Streams container stdout/stderr. With --follow, tails in real time
until the run completes. For completed runs whose container has been
removed, falls back to the saved container.log in the results directory.`,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "follow",
				Usage: "Follow log output",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			runID := cmd.Args().First()
			if runID == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}
			follow := cmd.Bool("follow")
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}
			dbPath := filepath.Join(homeDir, ".horde", "horde.db")
			st, err := store.NewSQLiteStore(dbPath)
			if err != nil {
				return fmt.Errorf("opening store: %w", err)
			}
			defer st.Close()
			run, err := st.GetRun(ctx, runID)
			if err != nil {
				if errors.Is(err, store.ErrRunNotFound) {
					return fmt.Errorf("run not found: %s", runID)
				}
				return fmt.Errorf("reading run: %w", err)
			}
			if run.Status == store.StatusSuccess || run.Status == store.StatusFailed || run.Status == store.StatusKilled {
				// Container is gone — try saved logs
				logPath := filepath.Join(homeDir, ".horde", "results", runID, "container.log")
				if data, err := os.ReadFile(logPath); err == nil {
					os.Stdout.Write(data)
					return nil
				}
				return fmt.Errorf("logs unavailable: run %s is %s (container removed, no saved logs)", runID, run.Status)
			}
			if run.InstanceID == "" {
				return fmt.Errorf("logs unavailable: run %s has no container yet", runID)
			}
			prov := provider.NewDockerProvider()
			reader, err := prov.Logs(ctx, run.InstanceID, follow)
			if err != nil {
				return fmt.Errorf("reading logs: %w", err)
			}
			defer reader.Close()
			_, err = io.Copy(os.Stdout, reader)
			if err != nil {
				return fmt.Errorf("streaming logs: %w", err)
			}
			return nil
		},
	}
}

func killCmd() *cli.Command {
	return &cli.Command{
		Name:      "kill",
		Usage:     "Kill a running run",
		ArgsUsage: "<run-id>",
		Description: `Stops a running container, saves logs and workspace patch (best-effort),
copies artifacts from the container, updates the run status to killed,
and removes the container. The saved state can be used by 'horde resume'.`,
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
				return fmt.Errorf("opening store: %w", err)
			}
			defer st.Close()
			run, err := st.GetRun(ctx, runID)
			if err != nil {
				if errors.Is(err, store.ErrRunNotFound) {
					return fmt.Errorf("run not found: %s", runID)
				}
				return fmt.Errorf("reading run: %w", err)
			}
			if run.Status != store.StatusPending && run.Status != store.StatusRunning {
				return fmt.Errorf("run %s is already %s", runID, run.Status)
			}
			var cost *float64
			var exitCode *int
			if run.InstanceID != "" {
				resultsDir := filepath.Join(homeDir, ".horde", "results", run.ID)
				prov := provider.NewDockerProvider()

				// Capture container logs and workspace patch before kill removes the container
				if logs, err := prov.Logs(ctx, run.InstanceID, false); err == nil {
					if logData, err := io.ReadAll(logs); err == nil && len(logData) > 0 {
						os.MkdirAll(resultsDir, 0o755)
						os.WriteFile(filepath.Join(resultsDir, "container.log"), logData, 0o644)
					}
					logs.Close()
				}
				saveWorkspacePatch(ctx, prov, run.InstanceID, resultsDir)

				if err := prov.Kill(ctx, provider.KillOpts{
					InstanceID: run.InstanceID,
					ResultsDir: resultsDir,
				}); err != nil {
					return fmt.Errorf("killing run: %w", err)
				}

				// Best-effort: read run-result.json for cost and exit code
				var resultPath string
				if run.Workflow != "" {
					resultPath = filepath.Join(resultsDir, "audit", run.Workflow, run.Ticket, "run-result.json")
				} else {
					resultPath = filepath.Join(resultsDir, "audit", run.Ticket, "run-result.json")
				}
				if data, err := os.ReadFile(resultPath); err == nil {
					var rr runResult
					if json.Unmarshal(data, &rr) == nil {
						cost = rr.TotalCostUSD
						exitCode = rr.ExitCode
					}
				}
			}
			killedStatus := store.StatusKilled
			now := time.Now()
			if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
				Status:       &killedStatus,
				CompletedAt:  &now,
				TotalCostUSD: cost,
				ExitCode:     exitCode,
			}); err != nil {
				return fmt.Errorf("updating run: %w", err)
			}
			fmt.Printf("Killed run %s\n", runID)
			return nil
		},
	}
}

func resultsCmd() *cli.Command {
	return &cli.Command{
		Name:      "results",
		Usage:     "Show results of a run",
		ArgsUsage: "<run-id>",
		Description: `Displays the run's result summary from run-result.json: overall status,
total cost, total duration, and a per-phase breakdown. Reports partial
information if the result file is missing (e.g., orc crashed early).`,
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
				return fmt.Errorf("opening store: %w", err)
			}
			defer st.Close()
			run, err := st.GetRun(ctx, runID)
			if err != nil {
				if errors.Is(err, store.ErrRunNotFound) {
					return fmt.Errorf("run not found: %s", runID)
				}
				return fmt.Errorf("reading run: %w", err)
			}
			prov := provider.NewDockerProvider()
			if err := handleLazyCheck(ctx, prov, st, run, homeDir); err != nil {
				return err
			}
			if run.Status == store.StatusPending || run.Status == store.StatusRunning {
				fmt.Printf("Run %s is still in progress (status: %s)\n", run.ID, run.Status)
				return nil
			}
			var resultPath string
			if run.Workflow != "" {
				resultPath = filepath.Join(".orc", "audit", run.Workflow, run.Ticket, "run-result.json")
			} else {
				resultPath = filepath.Join(".orc", "audit", run.Ticket, "run-result.json")
			}
			data, err := prov.ReadFile(ctx, provider.ReadFileOpts{
				RunID:      run.ID,
				Path:       resultPath,
				InstanceID: run.InstanceID,
				Metadata:   run.Metadata,
			})
			if err != nil {
				printPartialResults(run)
				return nil
			}
			var result fullRunResult
			if err := json.Unmarshal(data, &result); err != nil {
				printPartialResults(run)
				return nil
			}
			printFullResults(run, &result)
			return nil
		},
	}
}

func listCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List runs for the current repo",
		Description: `Lists runs scoped to the current repo (inferred from git remote).
By default shows only active runs (pending/running). Use --all to
include completed, failed, and killed runs.`,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "all",
				Usage: "Include completed, failed, and killed runs",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			all := cmd.Bool("all")

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}

			repo, err := config.RepoURL(cwd)
			if err != nil {
				return err
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

			runs, err := st.ListByRepo(ctx, repo, !all)
			if err != nil {
				return fmt.Errorf("listing runs: %w", err)
			}

			prov := provider.NewDockerProvider()
			for _, run := range runs {
				if err := handleLazyCheck(ctx, prov, st, run, homeDir); err != nil {
					fmt.Fprintf(os.Stderr, "warning: checking run %s: %v\n", run.ID, err)
					continue
				}
				if run.TotalCostUSD == nil && (run.Status == store.StatusRunning || run.Status == store.StatusPending) {
					run.TotalCostUSD = fetchLiveCost(ctx, prov, run)
				}
			}

			if !all {
				filtered := runs[:0]
				for _, run := range runs {
					if run.Status == store.StatusPending || run.Status == store.StatusRunning {
						filtered = append(filtered, run)
					}
				}
				runs = filtered
			}

			if len(runs) == 0 {
				if all {
					fmt.Println("No runs found for this repo.")
				} else {
					fmt.Println("No active runs for this repo.")
				}
				return nil
			}

			printRunTable(runs)
			return nil
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
	ExitCode     *int     `json:"exit_code"`
}

func docsCmd() *cli.Command {
	return &cli.Command{
		Name:      "docs",
		Usage:     "Show documentation",
		ArgsUsage: "[topic]",
		Description: `Without arguments, lists available documentation topics. With a topic
name, displays the full article.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			name := cmd.Args().First()
			if name == "" {
				fmt.Print("\nAvailable topics:\n\n")
				for _, t := range docs.All() {
					fmt.Printf("  %-14s %s\n", t.Name, t.Summary)
				}
				fmt.Println("\nRun 'horde docs <topic>' to read a topic.")
				return nil
			}
			t, err := docs.Get(name)
			if err != nil {
				return err
			}
			fmt.Print(t.Content)
			return nil
		},
	}
}

type liveCosts struct {
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// fetchLiveCost reads the current cost from a running container's costs.json.
func fetchLiveCost(ctx context.Context, prov *provider.DockerProvider, run *store.Run) *float64 {
	if run.InstanceID == "" {
		return nil
	}
	var costsPath string
	if run.Workflow != "" {
		costsPath = "/workspace/.orc/audit/" + run.Workflow + "/" + run.Ticket + "/costs.json"
	} else {
		costsPath = "/workspace/.orc/audit/" + run.Ticket + "/costs.json"
	}
	data, err := prov.ReadContainerFile(ctx, run.InstanceID, costsPath)
	if err != nil {
		return nil
	}
	var lc liveCosts
	if json.Unmarshal(data, &lc) != nil || lc.TotalCostUSD == 0 {
		return nil
	}
	return &lc.TotalCostUSD
}

// saveWorkspacePatch captures all code changes (committed + uncommitted) from the
// container as a patch file in resultsDir. Best-effort — errors are silently ignored.
func saveWorkspacePatch(ctx context.Context, prov *provider.DockerProvider, instanceID, resultsDir string) {
	baseRef, err := prov.ReadContainerFile(ctx, instanceID, "/workspace/.horde-base-ref")
	if err != nil {
		return
	}
	ref := strings.TrimSpace(string(baseRef))
	if ref == "" {
		return
	}

	script := fmt.Sprintf("cd /workspace && git add -A && git diff --cached %s", ref)
	patchData, err := prov.ExecInContainer(ctx, instanceID, script)
	if err != nil || len(patchData) == 0 {
		return
	}

	os.MkdirAll(resultsDir, 0o755)
	os.WriteFile(filepath.Join(resultsDir, "workspace.patch"), patchData, 0o644)
}

type fullRunResult struct {
	ExitCode      int           `json:"exit_code"`
	Status        string        `json:"status"`
	Ticket        string        `json:"ticket"`
	Workflow      string        `json:"workflow"`
	TotalCostUSD  *float64      `json:"total_cost_usd"`
	TotalDuration string        `json:"total_duration"`
	Phases        []phaseResult `json:"phases"`
}

type phaseResult struct {
	Name     string  `json:"name"`
	CostUSD  float64 `json:"cost_usd"`
	Duration string  `json:"duration"`
	Status   string  `json:"status"`
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
		return fmt.Errorf("checking instance status: %w", err)
	}

	switch instanceStatus.State {
	case "stopped":
		resultsDir := filepath.Join(homeDir, ".horde", "results", run.ID)
		// Best-effort copy — errors ignored
		prov.CopyFromContainer(ctx, run.InstanceID, "/workspace/.orc/audit/.", filepath.Join(resultsDir, "audit"))
		prov.CopyFromContainer(ctx, run.InstanceID, "/workspace/.orc/artifacts/.", filepath.Join(resultsDir, "artifacts"))

		// Capture container stdout/stderr and workspace patch before removal
		if logs, err := prov.Logs(ctx, run.InstanceID, false); err == nil {
			if logData, err := io.ReadAll(logs); err == nil && len(logData) > 0 {
				os.MkdirAll(resultsDir, 0o755)
				os.WriteFile(filepath.Join(resultsDir, "container.log"), logData, 0o644)
			}
			logs.Close()
		}
		saveWorkspacePatch(ctx, prov, run.InstanceID, resultsDir)

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

		// Determine CompletedAt — guard against Docker returning a zero time
		var completedAt *time.Time
		if instanceStatus.FinishedAt != nil && !instanceStatus.FinishedAt.IsZero() {
			completedAt = instanceStatus.FinishedAt
		} else {
			now := time.Now()
			completedAt = &now
		}

		var newStatus store.Status
		if instanceStatus.ExitCode != nil {
			newStatus = mapExitCode(*instanceStatus.ExitCode)
		} else {
			newStatus = store.StatusFailed
		}
		update := &store.RunUpdate{
			Status:       &newStatus,
			ExitCode:     instanceStatus.ExitCode,
			CompletedAt:  completedAt,
			TotalCostUSD: cost,
		}
		if err := st.UpdateRun(ctx, run.ID, update); err != nil {
			return fmt.Errorf("updating run after completion: %w", err)
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
				return nil
			}

			// Best-effort: read run-result.json for cost and exit code
			var cost *float64
			var exitCode *int
			var resultPath string
			if run.Workflow != "" {
				resultPath = filepath.Join(resultsDir, "audit", run.Workflow, run.Ticket, "run-result.json")
			} else {
				resultPath = filepath.Join(resultsDir, "audit", run.Ticket, "run-result.json")
			}
			if data, err := os.ReadFile(resultPath); err == nil {
				var rr runResult
				if json.Unmarshal(data, &rr) == nil {
					cost = rr.TotalCostUSD
					exitCode = rr.ExitCode
				}
			}

			killedStatus := store.StatusKilled
			now := time.Now()
			if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
				Status:       &killedStatus,
				CompletedAt:  &now,
				TotalCostUSD: cost,
				ExitCode:     exitCode,
			}); err != nil {
				return fmt.Errorf("updating run after timeout: %w", err)
			}
			run.Status = store.StatusKilled
			run.CompletedAt = &now
			run.TotalCostUSD = cost
			run.ExitCode = exitCode
		}

	case "unknown":
		failedStatus := store.StatusFailed
		now := time.Now()
		if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{Status: &failedStatus, CompletedAt: &now}); err != nil {
			return fmt.Errorf("updating run after unknown container: %w", err)
		}
		run.Status = store.StatusFailed
		run.CompletedAt = &now
	}

	return nil
}

func printRunTable(runs []*store.Run) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "RUN ID\tTICKET\tBRANCH\tSTATUS\tDURATION\tCOST")
	for _, run := range runs {
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

		cost := "-"
		if run.TotalCostUSD != nil {
			cost = fmt.Sprintf("$%.2f", *run.TotalCostUSD)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			run.ID, run.Ticket, branch, run.Status, duration, cost)
	}
	w.Flush()
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
	if run.Workflow != "" {
		fmt.Printf("Workflow:    %s\n", run.Workflow)
	}
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

func printFullResults(run *store.Run, result *fullRunResult) {
	fmt.Printf("Run:            %s\n", run.ID)
	fmt.Printf("Ticket:         %s\n", run.Ticket)
	if run.Workflow != "" {
		fmt.Printf("Workflow:       %s\n", run.Workflow)
	}
	fmt.Printf("Status:         %s\n", result.Status)
	if result.TotalCostUSD != nil {
		fmt.Printf("Total Cost:     $%.2f\n", *result.TotalCostUSD)
	}
	fmt.Printf("Total Duration: %s\n", result.TotalDuration)

	if len(result.Phases) > 0 {
		fmt.Println()
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PHASE\tSTATUS\tCOST\tDURATION")
		for _, p := range result.Phases {
			fmt.Fprintf(w, "%s\t%s\t$%.2f\t%s\n", p.Name, p.Status, p.CostUSD, p.Duration)
		}
		w.Flush()
	}
}

func printPartialResults(run *store.Run) {
	fmt.Printf("Run:    %s\n", run.ID)
	fmt.Printf("Ticket: %s\n", run.Ticket)
	if run.Workflow != "" {
		fmt.Printf("Workflow: %s\n", run.Workflow)
	}
	fmt.Printf("Status: %s\n", run.Status)
	if run.ExitCode != nil {
		fmt.Printf("Exit Code: %d\n", *run.ExitCode)
	}
	if run.TotalCostUSD != nil {
		fmt.Printf("Cost:   $%.2f\n", *run.TotalCostUSD)
	}
	fmt.Println()
	fmt.Println("Detailed results unavailable (run-result.json not found).")
}

// resolveLaunchedBy returns the identity string for run records.
// Docker uses the local git user name; aws-ecs uses the IAM ARN from STS.
func resolveLaunchedBy(ctx context.Context, providerName string, cwd string, awsCfg *aws.Config, profile string) (string, error) {
	switch providerName {
	case "docker":
		return config.LaunchedBy(cwd), nil
	case "aws-ecs":
		if awsCfg == nil {
			return "", fmt.Errorf("resolving launched_by: AWS config required for aws-ecs provider")
		}
		arn, err := awscfg.CallerIdentity(ctx, *awsCfg, profile)
		if err != nil {
			return "", fmt.Errorf("resolving launched_by: %w", err)
		}
		return arn, nil
	default:
		return "", fmt.Errorf("resolving launched_by: unsupported provider %q", providerName)
	}
}
