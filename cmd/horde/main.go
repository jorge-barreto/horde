package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
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
	// Merge .env from cwd into the process environment (real env wins). This
	// lets commands like `horde bootstrap deploy` pick up CLAUDE_CODE_OAUTH_TOKEN
	// and GIT_TOKEN directly from the project's .env without the caller having
	// to `source` it. A missing .env is a silent no-op.
	if cwd, err := os.Getwd(); err == nil {
		if err := config.ApplyDotEnvToProcess(cwd); err != nil {
			fmt.Fprintf(os.Stderr, "warning: loading .env: %v\n", err)
		}
	}
	if err := newApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:  "horde",
		Usage: "Cloud launcher for orc workflows",
		Description: `horde runs orc workflows on ephemeral containers (Docker locally,
ECS Fargate in AWS). It clones a repo, runs orc, collects results, and tears down.

horde must be run from inside a git repository — the repo URL is inferred
from the local git remote. Run 'horde docs' for detailed documentation.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "provider",
				Usage: "Provider (docker or aws-ecs); omit for auto-detection via SSM",
			},
			&cli.StringFlag{
				Name:  "profile",
				Usage: "AWS named profile (passed through to AWS SDK)",
			},
			&cli.BoolFlag{
				Name:  "json",
				Usage: "Machine-readable JSON output (status, results, list)",
			},
		},
		Commands: []*cli.Command{
			launchCmd(),
			retryCmd(),
			statusCmd(),
			logsCmd(),
			killCmd(),
			resultsCmd(),
			hydrateCmd(),
			listCmd(),
			cleanCmd(),
			shellCmd(),
			bootstrapCmd(),
			pushCmd(),
			docsCmd(),
		},
	}
}

func launchCmd() *cli.Command {
	return &cli.Command{
		Name:      "launch",
		Usage:     "Launch an orc workflow",
		ArgsUsage: "<ticket> [-- <orc-args>...]",
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
				Value: 24 * time.Hour,
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
			orcArgs := cmd.Args().Tail()

			branch := cmd.String("branch")
			workflow := cmd.String("workflow")
			timeout := cmd.Duration("timeout")
			force := cmd.Bool("force")

			prov, st, maxConcurrent, provName, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			// Concurrency check: reject launch if at capacity.
			activeCount, err := st.CountActive(ctx)
			if err != nil {
				return fmt.Errorf("checking concurrency: %w", err)
			}
			if activeCount >= maxConcurrent {
				activeRuns, listErr := st.ListActive(ctx)
				if listErr != nil {
					return fmt.Errorf("at capacity (%d/%d active runs) but failed to list them: %w", activeCount, maxConcurrent, listErr)
				}
				fmt.Fprintf(os.Stderr, "at capacity: %d/%d active runs\n", activeCount, maxConcurrent)
				for _, r := range activeRuns {
					fmt.Fprintf(os.Stderr, "  %s  %s\n", r.ID, r.Ticket)
				}
				return fmt.Errorf("max concurrent runs reached (%d/%d)", activeCount, maxConcurrent)
			}

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}

			repo, err := config.RepoURL(cwd)
			if err != nil {
				return err
			}

			var envPath string
			if provName == "docker" {
				envPath, err = config.ValidateEnvFile(cwd)
				if err != nil {
					return err
				}
			}

			id, err := runid.Generate()
			if err != nil {
				return err
			}

			launchedBy, err := resolveLaunchedBy(ctx, provName, cwd, nil, cmd.String("profile"))
			if err != nil {
				return err
			}

			active, err := st.FindActiveByTicket(ctx, repo, ticket)
			if err != nil {
				return fmt.Errorf("checking active runs: %w", err)
			}
			// Reconcile stale records: container may have died since last check.
			stillActive := active[:0]
			for _, r := range active {
				origStatus := r.Status
				_ = prov.Finalize(ctx, r, homeDir)
				if r.Status != origStatus {
					_ = st.UpdateRun(ctx, r.ID, &store.RunUpdate{
						Status:       &r.Status,
						ExitCode:     r.ExitCode,
						CompletedAt:  r.CompletedAt,
						TotalCostUSD: r.TotalCostUSD,
					})
				}
				if r.Status == store.StatusPending || r.Status == store.StatusRunning {
					stillActive = append(stillActive, r)
				}
			}
			active = stillActive
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
				Provider:   provName,
				Status:     store.StatusPending,
				LaunchedBy: launchedBy,
				StartedAt:  now,
				TimeoutAt:  now.Add(timeout),
			}
			if err := st.CreateRun(ctx, run); err != nil {
				return fmt.Errorf("recording run: %w", err)
			}

			if dp, ok := prov.(*provider.DockerProvider); ok {
				workerFS, err := fs.Sub(horde.WorkerFiles, "docker")
				if err != nil {
					return fmt.Errorf("accessing worker files: %w", err)
				}
				if err := dp.EnsureImage(ctx, workerFS, cwd, os.Stderr); err != nil {
					failedStatus := store.StatusFailed
					now := time.Now()
					if updateErr := st.UpdateRun(ctx, id, &store.RunUpdate{Status: &failedStatus, CompletedAt: &now}); updateErr != nil {
						fmt.Fprintf(os.Stderr, "warning: failed to mark run as failed: %v\n", updateErr)
					}
					return fmt.Errorf("preparing worker image: %w", err)
				}
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
				HomeDir:  homeDir,
				OrcArgs:  orcArgs,
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

func retryCmd() *cli.Command {
	return &cli.Command{
		Name:      "retry",
		Usage:     "Retry a failed or killed run",
		ArgsUsage: "<run-id> [-- <orc-args>...]",
		Description: `Retries a failed or killed run by launching a new container against the
preserved workspace — orc picks up from where it left off. If the old
container is still alive, it is stopped first.

By default, --resume is passed to orc so it preserves artifacts and
resumes any interrupted agent session. Override with explicit orc args:
  horde retry abc123 -- --retry implement
  horde retry abc123 -- --from plan`,
		Flags: []cli.Flag{
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "Timeout for the retried run",
				Value: 24 * time.Hour,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			runID := cmd.Args().First()
			if runID == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}
			orcArgs := cmd.Args().Tail()
			if len(orcArgs) == 0 {
				orcArgs = []string{"--resume"}
				fmt.Fprintln(os.Stderr, "Passing --resume to orc (override with -- <orc-args>)")
			}
			timeout := cmd.Duration("timeout")

			prov, st, run, cleanup, err := initFromRunID(ctx, cmd, runID)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			origStatus := run.Status
			if err := prov.Finalize(ctx, run, homeDir); err != nil {
				return err
			}
			if run.Status != origStatus {
				if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
					Status:       &run.Status,
					ExitCode:     run.ExitCode,
					CompletedAt:  run.CompletedAt,
					TotalCostUSD: run.TotalCostUSD,
				}); err != nil {
					return fmt.Errorf("updating run: %w", err)
				}
			}

			if run.Status == store.StatusPending {
				return fmt.Errorf("run %s is still pending", runID)
			}
			if run.Status == store.StatusRunning {
				return fmt.Errorf("run %s is still running — kill it first or wait for completion", runID)
			}
			if run.Status == store.StatusSuccess {
				return fmt.Errorf("run %s already succeeded — nothing to retry", runID)
			}

			// Stop old container if still running
			instStatus, err := prov.Status(ctx, run.InstanceID)
			if err != nil {
				return fmt.Errorf("checking container: %w", err)
			}
			if instStatus.State == provider.StateRunning {
				if err := prov.Stop(ctx, provider.StopOpts{InstanceID: run.InstanceID}); err != nil {
					return fmt.Errorf("stopping old container: %w", err)
				}
			}

			// Relaunch with preserved workspace
			workspaceDir := provider.WorkspacePath(homeDir, run.ID)
			if _, err := os.Stat(filepath.Join(workspaceDir, ".git")); err != nil {
				return fmt.Errorf("workspace for run %s not found at %s — use 'horde launch' to start fresh", runID, workspaceDir)
			}

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}
			var envPath string
			if run.Provider == "docker" {
				envPath, err = config.ValidateEnvFile(cwd)
				if err != nil {
					return err
				}
			}
			projCfg, err := config.LoadProjectConfig(cwd)
			if err != nil {
				return err
			}

			if dp, ok := prov.(*provider.DockerProvider); ok {
				workerFS, err := fs.Sub(horde.WorkerFiles, "docker")
				if err != nil {
					return fmt.Errorf("accessing worker files: %w", err)
				}
				if err := dp.EnsureImage(ctx, workerFS, cwd, os.Stderr); err != nil {
					return fmt.Errorf("preparing worker image: %w", err)
				}
			}

			// Remove stale exit code marker if present (legacy containers)
			os.Remove(filepath.Join(workspaceDir, ".horde-exit-code"))

			result, err := prov.Launch(ctx, provider.LaunchOpts{
				Repo:     run.Repo,
				Ticket:   run.Ticket,
				Branch:   run.Branch,
				Workflow: run.Workflow,
				RunID:    run.ID,
				EnvFile:  envPath,
				Mounts:   projCfg.ResolveMounts(cwd),
				HomeDir:  homeDir,
				OrcArgs:  orcArgs,
			})
			if err != nil {
				return fmt.Errorf("relaunching container for retry: %w", err)
			}

			// Update run with new container ID
			if err := st.UpdateRun(ctx, runID, &store.RunUpdate{
				InstanceID: &result.InstanceID,
			}); err != nil {
				return fmt.Errorf("updating instance ID: %w", err)
			}

			// Update run back to running with fresh timeout
			runningStatus := store.StatusRunning
			now := time.Now()
			timeoutAt := now.Add(timeout)
			if err := st.UpdateRun(ctx, runID, &store.RunUpdate{
				Status:    &runningStatus,
				TimeoutAt: &timeoutAt,
			}); err != nil {
				return fmt.Errorf("updating run status: %w", err)
			}

			fmt.Fprintf(os.Stderr, "Retrying %s (run %s)\n", run.Ticket, runID)
			fmt.Println(runID)
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

			prov, st, run, cleanup, err := initFromRunID(ctx, cmd, runID)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			origStatus := run.Status
			if err := prov.Finalize(ctx, run, homeDir); err != nil {
				return err
			}
			if run.Status != origStatus {
				if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
					Status:       &run.Status,
					ExitCode:     run.ExitCode,
					CompletedAt:  run.CompletedAt,
					TotalCostUSD: run.TotalCostUSD,
				}); err != nil {
					return fmt.Errorf("updating run: %w", err)
				}
			}
			if run.TotalCostUSD == nil && (run.Status == store.StatusRunning || run.Status == store.StatusPending) {
				if dp, ok := prov.(*provider.DockerProvider); ok {
					run.TotalCostUSD = fetchLiveCost(ctx, dp, run)
				}
			}
			if cmd.Bool("json") {
				return writeJSON(statusToV1(run))
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
until the run completes; press Ctrl+C to detach. For completed runs
whose container has been removed, falls back to the saved container.log
in the results directory.`,
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

			prov, _, run, cleanup, err := initFromRunID(ctx, cmd, runID)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			if run.Status.IsTerminal() {
				// Docker: container is gone, try saved logs on disk.
				// ECS: CloudWatch Logs persist after task stops, so fall
				// through to prov.Logs() which queries CloudWatch.
				if run.Provider == "docker" {
					logPath := filepath.Join(provider.LocalResultsDir(homeDir, runID), "container.log")
					if data, err := os.ReadFile(logPath); err == nil {
						os.Stdout.Write(data)
						return nil
					}
					return fmt.Errorf("logs unavailable: run %s is %s (container removed, no saved logs)", runID, run.Status)
				}
			}
			if run.InstanceID == "" {
				return fmt.Errorf("logs unavailable: run %s has no container yet", runID)
			}
			if follow {
				// Catch SIGINT/SIGTERM so Ctrl+C closes the reader cleanly
				// rather than killing the process mid-stream and orphaning
				// the `docker logs --follow` child.
				sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
				defer stop()
				reader, err := prov.Logs(sigCtx, run.InstanceID, follow)
				if err != nil {
					return fmt.Errorf("reading logs: %w", err)
				}
				var once sync.Once
				closeReader := func() { once.Do(func() { reader.Close() }) }
				defer closeReader()
				go func() {
					<-sigCtx.Done()
					closeReader()
				}()
				if _, err := io.Copy(os.Stdout, reader); err != nil && sigCtx.Err() == nil {
					return fmt.Errorf("streaming logs: %w", err)
				}
				return nil
			}
			reader, err := prov.Logs(ctx, run.InstanceID, follow)
			if err != nil {
				return fmt.Errorf("reading logs: %w", err)
			}
			defer reader.Close()
			if _, err := io.Copy(os.Stdout, reader); err != nil {
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
		Description: `Stops a running container and copies artifacts. The container is preserved
for 'horde retry' or 'horde shell'. Use 'horde clean' to remove it.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			runID := cmd.Args().First()
			if runID == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}

			prov, st, run, cleanup, err := initFromRunID(ctx, cmd, runID)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			origStatus := run.Status
			if err := prov.Finalize(ctx, run, homeDir); err != nil {
				return err
			}
			if run.Status != origStatus {
				if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
					Status:       &run.Status,
					ExitCode:     run.ExitCode,
					CompletedAt:  run.CompletedAt,
					TotalCostUSD: run.TotalCostUSD,
				}); err != nil {
					return fmt.Errorf("updating run: %w", err)
				}
			}
			if run.Status.IsTerminal() {
				return fmt.Errorf("run %s is already %s", runID, run.Status)
			}
			var cost *float64
			var exitCode *int
			if run.InstanceID != "" {
				resultsDir := provider.LocalResultsDir(homeDir, run.ID)

				// Capture container logs before stopping
				if logs, err := prov.Logs(ctx, run.InstanceID, false); err == nil {
					if logData, err := io.ReadAll(logs); err == nil && len(logData) > 0 {
						os.MkdirAll(resultsDir, 0o755)
						os.WriteFile(filepath.Join(resultsDir, "container.log"), logData, 0o644)
					}
					logs.Close()
				}

				if err := prov.Stop(ctx, provider.StopOpts{
					InstanceID: run.InstanceID,
					ResultsDir: resultsDir,
				}); err != nil {
					return fmt.Errorf("killing run: %w", err)
				}

				// Best-effort: read run-result.json for cost and exit code
				resultPath := filepath.Join(resultsDir, provider.AuditRelPath(run.Workflow, run.Ticket, "run-result.json"))
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
			wsDir := provider.WorkspacePath(homeDir, runID)
			if _, err := os.Stat(wsDir); err == nil {
				fmt.Fprintf(os.Stderr, "Workspace preserved at %s\n", wsDir)
			}
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

			prov, st, run, cleanup, err := initFromRunID(ctx, cmd, runID)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			origStatus := run.Status
			if err := prov.Finalize(ctx, run, homeDir); err != nil {
				return err
			}
			if run.Status != origStatus {
				if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
					Status:       &run.Status,
					ExitCode:     run.ExitCode,
					CompletedAt:  run.CompletedAt,
					TotalCostUSD: run.TotalCostUSD,
				}); err != nil {
					return fmt.Errorf("updating run: %w", err)
				}
			}
			if run.Status == store.StatusPending || run.Status == store.StatusRunning {
				if cmd.Bool("json") {
					return writeJSON(partialResultsToV1(run))
				}
				fmt.Printf("Run %s is still in progress (status: %s)\n", run.ID, run.Status)
				return nil
			}
			resultPath := filepath.Join(".orc", provider.AuditRelPath(run.Workflow, run.Ticket, "run-result.json"))
			data, err := prov.ReadFile(ctx, provider.ReadFileOpts{
				RunID:      run.ID,
				Path:       resultPath,
				InstanceID: run.InstanceID,
				Metadata:   run.Metadata,
			})
			if err != nil {
				if cmd.Bool("json") {
					return writeJSON(partialResultsToV1(run))
				}
				printPartialResults(run)
				return nil
			}
			var result fullRunResult
			if err := json.Unmarshal(data, &result); err != nil {
				if cmd.Bool("json") {
					return writeJSON(partialResultsToV1(run))
				}
				printPartialResults(run)
				return nil
			}
			if cmd.Bool("json") {
				return writeJSON(fullResultsToV1(run, &result))
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

			prov, st, _, _, cleanup, err := initProviderAndStoreWith(ctx, cmd.String("provider"), cmd.String("profile"), defaultFactoryDeps())
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}

			repo, err := config.RepoURL(cwd)
			if err != nil {
				return err
			}

			runs, err := st.ListByRepo(ctx, repo, !all)
			if err != nil {
				return fmt.Errorf("listing runs: %w", err)
			}

			for _, run := range runs {
				origStatus := run.Status
				if err := prov.Finalize(ctx, run, homeDir); err != nil {
					fmt.Fprintf(os.Stderr, "warning: checking run %s: %v\n", run.ID, err)
					continue
				}
				if run.Status != origStatus {
					if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
						Status:       &run.Status,
						ExitCode:     run.ExitCode,
						CompletedAt:  run.CompletedAt,
						TotalCostUSD: run.TotalCostUSD,
					}); err != nil {
						fmt.Fprintf(os.Stderr, "warning: updating run %s: %v\n", run.ID, err)
					}
				}
				if dp, ok := prov.(*provider.DockerProvider); ok {
					if run.TotalCostUSD == nil && (run.Status == store.StatusRunning || run.Status == store.StatusPending) {
						run.TotalCostUSD = fetchLiveCost(ctx, dp, run)
					}
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

			if cmd.Bool("json") {
				return writeJSON(listToV1(runs))
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

type runResult struct {
	TotalCostUSD *float64 `json:"total_cost_usd"`
	ExitCode     *int     `json:"exit_code"`
}

func cleanCmd() *cli.Command {
	return &cli.Command{
		Name:      "clean",
		Usage:     "Remove stopped containers",
		ArgsUsage: "[run-id]",
		Description: `Removes Docker containers for completed runs. Without arguments, removes
containers for all terminal runs (success, failed, killed). With a run ID,
removes only that run's container. Does not affect running or pending runs.
Workspaces are preserved by default. Use --purge to also remove workspace
directories (all code changes will be lost).`,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "purge",
				Usage: "Also remove workspace directories",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			prov, st, _, _, cleanup, err := initProviderAndStoreWith(ctx, cmd.String("provider"), cmd.String("profile"), defaultFactoryDeps())
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, _ := os.UserHomeDir()
			runID := cmd.Args().First()

			purge := cmd.Bool("purge")

			if runID != "" {
				// Clean a specific run
				run, err := st.GetRun(ctx, runID)
				if err != nil {
					if errors.Is(err, store.ErrRunNotFound) {
						return fmt.Errorf("run not found: %s", runID)
					}
					return fmt.Errorf("reading run: %w", err)
				}
				if run.Status == store.StatusRunning || run.Status == store.StatusPending {
					return fmt.Errorf("run %s is still %s — kill it first", runID, run.Status)
				}
				if dp, ok := prov.(*provider.DockerProvider); ok && run.InstanceID != "" {
					if err := dp.RemoveContainer(ctx, run.InstanceID); err != nil {
						fmt.Fprintf(os.Stderr, "warning: %v\n", err)
					} else {
						fmt.Printf("Removed container for run %s\n", runID)
					}
				}
				homeDir, _ := os.UserHomeDir()
				if homeDir != "" {
					workspaceDir := provider.WorkspacePath(homeDir, runID)
					sessionsDir := provider.SessionsPath(homeDir, runID)
					if purge {
						if err := removeWorkspace(ctx, workspaceDir); err != nil {
							fmt.Fprintf(os.Stderr, "warning: removing workspace: %v\n", err)
						} else {
							fmt.Printf("Removed workspace for run %s\n", runID)
						}
						if err := removeWorkspace(ctx, sessionsDir); err != nil {
							fmt.Fprintf(os.Stderr, "warning: removing sessions: %v\n", err)
						}
					} else if _, err := os.Stat(workspaceDir); err == nil {
						fmt.Fprintf(os.Stderr, "note: workspace preserved at %s (use --purge to remove)\n", workspaceDir)
					}
				}
				return nil
			}

			// Clean all terminal runs
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}
			repo, err := config.RepoURL(cwd)
			if err != nil {
				return err
			}
			allRuns, err := st.ListByRepo(ctx, repo, false)
			if err != nil {
				return fmt.Errorf("listing runs: %w", err)
			}
			activeRuns, err := st.ListByRepo(ctx, repo, true)
			if err != nil {
				return fmt.Errorf("listing runs: %w", err)
			}

			// Terminal runs = all minus active
			activeIDs := make(map[string]bool)
			for _, r := range activeRuns {
				activeIDs[r.ID] = true
			}
			var cleaned int
			for _, r := range allRuns {
				if activeIDs[r.ID] {
					continue
				}
				if dp, ok := prov.(*provider.DockerProvider); ok && r.InstanceID != "" {
					if err := dp.RemoveContainer(ctx, r.InstanceID); err != nil {
						fmt.Fprintf(os.Stderr, "warning: removing container for run %s: %v\n", r.ID, err)
					} else {
						cleaned++
					}
				}
				if purge && homeDir != "" {
					removeWorkspace(ctx, provider.WorkspacePath(homeDir, r.ID))
					removeWorkspace(ctx, provider.SessionsPath(homeDir, r.ID))
				}
			}
			fmt.Printf("Removed %d container(s)\n", cleaned)
			return nil
		},
	}
}

func shellCmd() *cli.Command {
	return &cli.Command{
		Name:      "shell",
		Usage:     "Open a shell in a run's container",
		ArgsUsage: "<run-id>",
		Description: `Opens an interactive bash shell in the run's container. If the container
is gone but the workspace persists on the host, launches an ephemeral
container for shell access. You can inspect files, run orc commands
directly (e.g., 'orc run --resume'), or fix issues manually.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			runID := cmd.Args().First()
			if runID == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}

			prov, st, run, cleanup, err := initFromRunID(ctx, cmd, runID)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			origStatus := run.Status
			if err := prov.Finalize(ctx, run, homeDir); err != nil {
				return err
			}
			if run.Status != origStatus {
				if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
					Status:       &run.Status,
					ExitCode:     run.ExitCode,
					CompletedAt:  run.CompletedAt,
					TotalCostUSD: run.TotalCostUSD,
				}); err != nil {
					return fmt.Errorf("updating run: %w", err)
				}
			}

			if run.InstanceID == "" {
				return fmt.Errorf("run %s has no container", runID)
			}
			instStatus, err := prov.Status(ctx, run.InstanceID)
			if err != nil {
				return fmt.Errorf("checking container: %w", err)
			}
			if instStatus.State == provider.StateRunning {
				// Container alive — exec directly
				fmt.Fprintf(os.Stderr, "Opening shell for run %s (%s). Type 'exit' to leave.\n", runID, run.Ticket)
				shellExec := exec.CommandContext(ctx, "docker", "exec", "-it",
					"-w", "/workspace",
					run.InstanceID, "bash")
				shellExec.Stdin = os.Stdin
				shellExec.Stdout = os.Stdout
				shellExec.Stderr = os.Stderr
				shellExec.Run()
				return nil
			}

			// Container stopped or gone — check for workspace on host
			workspaceDir := provider.WorkspacePath(homeDir, run.ID)
			if _, err := os.Stat(filepath.Join(workspaceDir, ".git")); err != nil {
				if instStatus.State == provider.StateUnknown {
					return fmt.Errorf("container for run %s no longer exists and no workspace found", runID)
				}
				return fmt.Errorf("container for run %s is not running — use 'horde retry' first", runID)
			}

			// Launch ephemeral container with workspace mount
			fmt.Fprintf(os.Stderr, "Container gone — launching shell with workspace from %s\n", workspaceDir)

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}
			envPath, _ := config.ValidateEnvFile(cwd)
			projCfg, _ := config.LoadProjectConfig(cwd)

			dockerArgs := []string{"run", "--rm", "-it", "-w", "/workspace"}
			if envPath != "" {
				dockerArgs = append(dockerArgs, "--env-file", envPath)
			}
			dockerArgs = append(dockerArgs, "-v", workspaceDir+":/workspace")
			if projCfg != nil {
				for _, m := range projCfg.ResolveMounts(cwd) {
					dockerArgs = append(dockerArgs, "-v", m)
				}
			}
			shellImage := provider.DockerImage
			if dp, ok := prov.(*provider.DockerProvider); ok {
				shellImage = dp.Image
			}
			dockerArgs = append(dockerArgs, shellImage, "bash")

			shellExec := exec.CommandContext(ctx, "docker", dockerArgs...)
			shellExec.Stdin = os.Stdin
			shellExec.Stdout = os.Stdout
			shellExec.Stderr = os.Stderr
			shellExec.Run()
			return nil
		},
	}
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
	costsPath := "/workspace/.orc/" + provider.AuditRelPath(run.Workflow, run.Ticket, "costs.json")
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

// removeWorkspace deletes a workspace directory, using a Docker container
// to handle root-owned files created inside containers.
func removeWorkspace(ctx context.Context, dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	// Delete contents as root via Docker (can't rm the mount point itself).
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--entrypoint", "",
		"-v", dir+":/cleanup",
		"horde-worker-base:latest", "sh", "-c", "rm -rf /cleanup/*  /cleanup/.[!.]* /cleanup/..?*")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker rm: %w", err)
	}
	// Now the host can remove the empty directory.
	return os.Remove(dir)
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
	if run.InstanceID != "" {
		cid := run.InstanceID
		if len(cid) > 12 {
			cid = cid[:12]
		}
		fmt.Printf("Container:   %s\n", cid)
	}
	if run.ExitCode != nil {
		fmt.Printf("Exit code:   %d\n", *run.ExitCode)
	}
	fmt.Printf("Duration:    %s\n", duration)
	if run.TotalCostUSD != nil {
		fmt.Printf("Cost:        $%.2f\n", *run.TotalCostUSD)
	}
	fmt.Printf("Launched by: %s\n", run.LaunchedBy)
	if homeDir, err := os.UserHomeDir(); err == nil {
		wsDir := provider.WorkspacePath(homeDir, run.ID)
		if _, err := os.Stat(wsDir); err == nil {
			fmt.Printf("Workspace:   %s\n", wsDir)
		}
	}
}

func printFullResults(run *store.Run, result *fullRunResult) {
	fmt.Printf("Run:            %s\n", run.ID)
	fmt.Printf("Ticket:         %s\n", run.Ticket)
	if run.Workflow != "" {
		fmt.Printf("Workflow:       %s\n", run.Workflow)
	}
	fmt.Printf("Status:         %s\n", run.Status)
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
		fmt.Printf("Exit code: %d\n", *run.ExitCode)
	}
	if run.TotalCostUSD != nil {
		fmt.Printf("Cost:   $%.2f\n", *run.TotalCostUSD)
	}
	fmt.Println()
	fmt.Println("Detailed results unavailable (run-result.json not found).")
}

// Test seams for resolveLaunchedBy. Package-level vars so tests can
// substitute fakes without depending on ambient AWS credentials.
var (
	awscfgLoad           = awscfg.Load
	awscfgCallerIdentity = awscfg.CallerIdentity
)

// resolveLaunchedBy returns the identity string for run records.
// Docker uses the local git user name; aws-ecs uses the IAM ARN from STS.
func resolveLaunchedBy(ctx context.Context, providerName string, cwd string, awsCfg *aws.Config, profile string) (string, error) {
	switch providerName {
	case "docker":
		return config.LaunchedBy(cwd), nil
	case "aws-ecs":
		if awsCfg == nil {
			cfg, err := awscfgLoad(ctx, profile)
			if err != nil {
				return "", fmt.Errorf("resolving launched_by: %w", err)
			}
			awsCfg = &cfg
		}
		arn, err := awscfgCallerIdentity(ctx, *awsCfg, profile)
		if err != nil {
			return "", fmt.Errorf("resolving launched_by: %w", err)
		}
		return arn, nil
	default:
		return "", fmt.Errorf("resolving launched_by: unsupported provider %q", providerName)
	}
}

// initProviderAndStore creates the Provider and Store based on the --provider flag.
// Selection rule: "docker" → DockerProvider + SQLite; "aws-ecs" → ECS + DynamoDB
// (not yet implemented); "" → auto-detect via SSM.
// Returns a cleanup function that must be deferred to release store resources.
func initProviderAndStore(ctx context.Context, cmd *cli.Command) (provider.Provider, store.Store, int, string, func(), error) {
	return initProviderAndStoreWith(ctx, cmd.String("provider"), cmd.String("profile"), defaultFactoryDeps())
}

// initFromRunID opens the store, looks up the run, and creates the provider
// from the stored run record. If --provider is set, it overrides the stored value.
func initFromRunID(ctx context.Context, cmd *cli.Command, runID string) (provider.Provider, store.Store, *store.Run, func(), error) {
	return initFromRunIDWith(ctx, cmd.String("provider"), cmd.String("profile"), runID, defaultFactoryDeps())
}
