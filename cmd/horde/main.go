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
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
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
		},
		Commands: []*cli.Command{
			launchCmd(),
			retryCmd(),
			statusCmd(),
			logsCmd(),
			killCmd(),
			resultsCmd(),
			listCmd(),
			cleanCmd(),
			shellCmd(),
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

			prov, st, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			provName := cmd.String("provider")
			if provName == "" {
				// TODO(d69): when auto-detect succeeds, resolve actual provider name.
				provName = "docker"
			}

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

			launchedBy, err := resolveLaunchedBy(ctx, provName, cwd, nil, cmd.String("profile"))
			if err != nil {
				return err
			}

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
				Provider:   provName,
				Status:     store.StatusPending,
				LaunchedBy: launchedBy,
				StartedAt:  now,
				TimeoutAt:  now.Add(timeout),
			}
			if err := st.CreateRun(ctx, run); err != nil {
				return fmt.Errorf("recording run: %w", err)
			}

			dp, ok := prov.(*provider.DockerProvider)
			if !ok {
				return fmt.Errorf("image build is only supported for the docker provider")
			}

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

func retryCmd() *cli.Command {
	return &cli.Command{
		Name:      "retry",
		Usage:     "Retry a failed or killed run",
		ArgsUsage: "<run-id>",
		Description: `Restarts the existing container for a failed or killed run. The container's
filesystem is preserved, so all code changes and orc state remain intact.
The entrypoint re-runs and orc picks up from where it left off. Use
'horde shell' for manual control over the retry.`,
		Flags: []cli.Flag{
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "Timeout for the retried run",
				Value: 60 * time.Minute,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			runID := cmd.Args().First()
			if runID == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}
			timeout := cmd.Duration("timeout")

			prov, st, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			run, err := st.GetRun(ctx, runID)
			if err != nil {
				if errors.Is(err, store.ErrRunNotFound) {
					return fmt.Errorf("run not found: %s", runID)
				}
				return fmt.Errorf("reading run: %w", err)
			}

			if dp, ok := prov.(*provider.DockerProvider); ok {
				if err := handleLazyCheck(ctx, dp, st, run, homeDir); err != nil {
					return err
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

			// Verify the container is still running (sleeping)
			instStatus, err := prov.Status(ctx, run.InstanceID)
			if err != nil {
				return fmt.Errorf("checking container: %w", err)
			}
			if instStatus.State == "unknown" {
				return fmt.Errorf("container for run %s no longer exists — use 'horde launch' to start fresh", runID)
			}
			if instStatus.State != "running" {
				return fmt.Errorf("container for run %s is stopped — use 'horde launch' to start fresh", runID)
			}

			// Clear previous completion marker and exec orc inside the running container
			dp, ok := prov.(*provider.DockerProvider)
			if !ok {
				return fmt.Errorf("retry is only supported for the docker provider")
			}
			dp.ExecInContainer(ctx, run.InstanceID, "rm -f /workspace/.horde-exit-code")

			ticket := run.Ticket
			orcCmd := "cd /workspace && orc run " + ticket + " --auto --no-color; echo $? > /workspace/.horde-exit-code"
			if run.Workflow != "" {
				orcCmd = "cd /workspace && orc run -w " + run.Workflow + " " + ticket + " --auto --no-color; echo $? > /workspace/.horde-exit-code"
			}
			execCmd := exec.CommandContext(ctx, "docker", "exec", "-d",
				run.InstanceID, "bash", "-c", orcCmd)
			if _, err := execCmd.Output(); err != nil {
				return fmt.Errorf("starting orc retry: %w", err)
			}

			// Update the same run record back to running
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

			prov, st, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			run, err := st.GetRun(ctx, runID)
			if err != nil {
				if errors.Is(err, store.ErrRunNotFound) {
					return fmt.Errorf("run not found: %s", runID)
				}
				return fmt.Errorf("reading run: %w", err)
			}
			if dp, ok := prov.(*provider.DockerProvider); ok {
				if err := handleLazyCheck(ctx, dp, st, run, homeDir); err != nil {
					return err
				}
			}
			if run.TotalCostUSD == nil && (run.Status == store.StatusRunning || run.Status == store.StatusPending) {
				if dp, ok := prov.(*provider.DockerProvider); ok {
					run.TotalCostUSD = fetchLiveCost(ctx, dp, run)
				}
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

			prov, st, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

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
		Description: `Stops a running container and copies artifacts. The container is preserved
for 'horde retry' or 'horde shell'. Use 'horde clean' to remove it.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			runID := cmd.Args().First()
			if runID == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}

			prov, st, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			run, err := st.GetRun(ctx, runID)
			if err != nil {
				if errors.Is(err, store.ErrRunNotFound) {
					return fmt.Errorf("run not found: %s", runID)
				}
				return fmt.Errorf("reading run: %w", err)
			}
			if dp, ok := prov.(*provider.DockerProvider); ok {
				if err := handleLazyCheck(ctx, dp, st, run, homeDir); err != nil {
					return err
				}
			}
			if run.Status != store.StatusPending && run.Status != store.StatusRunning {
				return fmt.Errorf("run %s is already %s", runID, run.Status)
			}
			var cost *float64
			var exitCode *int
			if run.InstanceID != "" {
				resultsDir := filepath.Join(homeDir, ".horde", "results", run.ID)

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

			prov, st, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			run, err := st.GetRun(ctx, runID)
			if err != nil {
				if errors.Is(err, store.ErrRunNotFound) {
					return fmt.Errorf("run not found: %s", runID)
				}
				return fmt.Errorf("reading run: %w", err)
			}
			if dp, ok := prov.(*provider.DockerProvider); ok {
				if err := handleLazyCheck(ctx, dp, st, run, homeDir); err != nil {
					return err
				}
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

			prov, st, cleanup, err := initProviderAndStore(ctx, cmd)
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

			dp, _ := prov.(*provider.DockerProvider)
			for _, run := range runs {
				if dp != nil {
					if err := handleLazyCheck(ctx, dp, st, run, homeDir); err != nil {
						fmt.Fprintf(os.Stderr, "warning: checking run %s: %v\n", run.ID, err)
						continue
					}
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

func cleanCmd() *cli.Command {
	return &cli.Command{
		Name:      "clean",
		Usage:     "Remove stopped containers",
		ArgsUsage: "[run-id]",
		Description: `Removes Docker containers for completed runs. Without arguments, removes
containers for all terminal runs (success, failed, killed). With a run ID,
removes only that run's container. Does not affect running or pending runs.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			prov, st, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			runID := cmd.Args().First()

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
				if run.InstanceID == "" {
					fmt.Printf("Run %s has no container\n", runID)
					return nil
				}
				if dp, ok := prov.(*provider.DockerProvider); ok {
					if err := dp.RemoveContainer(ctx, run.InstanceID); err != nil {
						fmt.Fprintf(os.Stderr, "warning: %v\n", err)
					} else {
						fmt.Printf("Removed container for run %s\n", runID)
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
			runs, err := st.ListByRepo(ctx, repo, false)
			if err != nil {
				return fmt.Errorf("listing runs: %w", err)
			}
			allRuns, err := st.ListByRepo(ctx, repo, true)
			if err != nil {
				return fmt.Errorf("listing runs: %w", err)
			}

			// Terminal runs = all minus active
			activeIDs := make(map[string]bool)
			for _, r := range runs {
				activeIDs[r.ID] = true
			}
			var cleaned int
			for _, r := range allRuns {
				if activeIDs[r.ID] || r.InstanceID == "" {
					continue
				}
				if dp, ok := prov.(*provider.DockerProvider); ok {
					if err := dp.RemoveContainer(ctx, r.InstanceID); err == nil {
						cleaned++
					}
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
		Description: `Opens an interactive bash shell in the run's container. The container
stays alive after orc finishes, so you can inspect files, run orc commands
directly (e.g., 'orc run --resume'), or fix issues manually.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			runID := cmd.Args().First()
			if runID == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}

			prov, st, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			run, err := st.GetRun(ctx, runID)
			if err != nil {
				if errors.Is(err, store.ErrRunNotFound) {
					return fmt.Errorf("run not found: %s", runID)
				}
				return fmt.Errorf("reading run: %w", err)
			}

			if dp, ok := prov.(*provider.DockerProvider); ok {
				if err := handleLazyCheck(ctx, dp, st, run, homeDir); err != nil {
					return err
				}
			}

			if run.InstanceID == "" {
				return fmt.Errorf("run %s has no container", runID)
			}
			instStatus, err := prov.Status(ctx, run.InstanceID)
			if err != nil {
				return fmt.Errorf("checking container: %w", err)
			}
			if instStatus.State == "unknown" {
				return fmt.Errorf("container for run %s no longer exists", runID)
			}
			if instStatus.State != "running" {
				return fmt.Errorf("container for run %s is not running — use 'horde retry' first", runID)
			}

			if _, ok := prov.(*provider.DockerProvider); !ok {
				return fmt.Errorf("shell is only supported for the docker provider")
			}

			// Exec into the running container
			fmt.Fprintf(os.Stderr, "Opening shell for run %s (%s). Type 'exit' to leave.\n", runID, run.Ticket)
			shellExec := exec.CommandContext(ctx, "docker", "exec", "-it",
				"-w", "/workspace",
				run.InstanceID, "bash")
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
	case "running":
		// Check timeout first
		if time.Now().After(run.TimeoutAt) {
			resultsDir := filepath.Join(homeDir, ".horde", "results", run.ID)
			if err := prov.Stop(ctx, provider.StopOpts{InstanceID: run.InstanceID, ResultsDir: resultsDir}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: stopping timed-out container: %v\n", err)
				return nil
			}

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
			return nil
		}

		// Check if orc finished (marker file exists — container stays alive via sleep infinity)
		exitData, err := prov.ReadContainerFile(ctx, run.InstanceID, "/workspace/.horde-exit-code")
		if err != nil {
			return nil // orc still running, nothing to do
		}
		exitCode := 1 // default to failure
		fmt.Sscanf(strings.TrimSpace(string(exitData)), "%d", &exitCode)

		resultsDir := filepath.Join(homeDir, ".horde", "results", run.ID)
		prov.CopyFromContainer(ctx, run.InstanceID, "/workspace/.orc/audit/.", filepath.Join(resultsDir, "audit"))
		prov.CopyFromContainer(ctx, run.InstanceID, "/workspace/.orc/artifacts/.", filepath.Join(resultsDir, "artifacts"))

		if logs, err := prov.Logs(ctx, run.InstanceID, false); err == nil {
			if logData, err := io.ReadAll(logs); err == nil && len(logData) > 0 {
				os.MkdirAll(resultsDir, 0o755)
				os.WriteFile(filepath.Join(resultsDir, "container.log"), logData, 0o644)
			}
			logs.Close()
		}

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

		newStatus := mapExitCode(exitCode)
		now := time.Now()
		if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
			Status:       &newStatus,
			ExitCode:     &exitCode,
			CompletedAt:  &now,
			TotalCostUSD: cost,
		}); err != nil {
			return fmt.Errorf("updating run after completion: %w", err)
		}
		run.Status = newStatus
		run.ExitCode = &exitCode
		run.CompletedAt = &now
		run.TotalCostUSD = cost

	case "stopped":
		// Fallback for containers that were killed or crashed
		resultsDir := filepath.Join(homeDir, ".horde", "results", run.ID)
		prov.CopyFromContainer(ctx, run.InstanceID, "/workspace/.orc/audit/.", filepath.Join(resultsDir, "audit"))
		prov.CopyFromContainer(ctx, run.InstanceID, "/workspace/.orc/artifacts/.", filepath.Join(resultsDir, "artifacts"))

		if logs, err := prov.Logs(ctx, run.InstanceID, false); err == nil {
			if logData, err := io.ReadAll(logs); err == nil && len(logData) > 0 {
				os.MkdirAll(resultsDir, 0o755)
				os.WriteFile(filepath.Join(resultsDir, "container.log"), logData, 0o644)
			}
			logs.Close()
		}

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

		var newStatus store.Status
		if instanceStatus.ExitCode != nil {
			newStatus = mapExitCode(*instanceStatus.ExitCode)
		} else {
			newStatus = store.StatusFailed
		}
		completedNow := time.Now()
		if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
			Status:       &newStatus,
			ExitCode:     instanceStatus.ExitCode,
			CompletedAt:  &completedNow,
			TotalCostUSD: cost,
		}); err != nil {
			return fmt.Errorf("updating run after completion: %w", err)
		}
		run.Status = newStatus
		run.ExitCode = instanceStatus.ExitCode
		run.CompletedAt = &completedNow
		run.TotalCostUSD = cost

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

// initProviderAndStore creates the Provider and Store based on the --provider flag.
// Selection rule: "docker" → DockerProvider + SQLite; "aws-ecs" → ECS + DynamoDB
// (not yet implemented); "" → auto-detect via SSM.
// Returns a cleanup function that must be deferred to release store resources.
func initProviderAndStore(ctx context.Context, cmd *cli.Command) (provider.Provider, store.Store, func(), error) {
	name := cmd.String("provider")
	profile := cmd.String("profile")

	switch name {
	case "docker":
		prov := provider.NewDockerProvider()
		st, cleanup, err := openStore("docker")
		if err != nil {
			return nil, nil, nil, err
		}
		return prov, st, cleanup, nil
	case "aws-ecs":
		awsCfg, err := awscfg.Load(ctx, profile)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("initializing aws-ecs provider: %w", err)
		}
		ssmClient := ssm.NewFromConfig(awsCfg)
		if _, err := config.LoadFromSSM(ctx, ssmClient, config.DefaultSSMPath); err != nil {
			return nil, nil, nil, fmt.Errorf("initializing aws-ecs provider: %s", config.Diagnostic(err))
		}
		return nil, nil, nil, fmt.Errorf("aws-ecs provider is not yet implemented")
	case "":
		awsCfg, err := awscfg.Load(ctx, profile)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("auto-detecting provider: %w\nhint: use --provider docker for local mode", err)
		}
		ssmClient := ssm.NewFromConfig(awsCfg)
		if _, err := config.LoadFromSSM(ctx, ssmClient, config.DefaultSSMPath); err != nil {
			return nil, nil, nil, fmt.Errorf("auto-detecting provider: %s\nhint: use --provider docker for local mode", config.Diagnostic(err))
		}
		return nil, nil, nil, fmt.Errorf("aws-ecs provider is not yet implemented")
	default:
		return nil, nil, nil, fmt.Errorf("unsupported provider %q: valid values are \"docker\" and \"aws-ecs\"", name)
	}
}

// openStore creates the Store for the given provider name.
// Returns a cleanup function that must be deferred to release resources.
func openStore(providerName string) (store.Store, func(), error) {
	switch providerName {
	case "docker":
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("getting home directory: %w", err)
		}
		dbPath := filepath.Join(homeDir, ".horde", "horde.db")
		st, err := store.NewSQLiteStore(dbPath)
		if err != nil {
			return nil, nil, fmt.Errorf("opening store: %w", err)
		}
		return st, func() { st.Close() }, nil
	case "aws-ecs":
		return nil, nil, fmt.Errorf("aws-ecs store is not yet implemented")
	default:
		return nil, nil, fmt.Errorf("openStore: unsupported provider %q", providerName)
	}
}
