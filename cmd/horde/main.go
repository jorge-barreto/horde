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
	"regexp"
	"sort"
	"strings"
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

// Overridden at build time via -ldflags '-X main.version=... -X main.commit=... -X main.buildDate=...'.
// make build / make install populate these from git; goreleaser uses the tag.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
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
		// An empty message is the cli.Exit("", code) / emittedExit convention
		// for "exit non-zero, message already shown" — don't print a bare
		// "error:" line in that case (the command already emitted its output,
		// e.g. the hydrate summary or a JSON envelope).
		if msg := err.Error(); msg != "" {
			fmt.Fprintf(os.Stderr, "error: %v\n", msg)
		}
		os.Exit(1)
	}
}

// setOutputs assigns w to the root command's Writer/ErrWriter and to every
// subcommand. urfave/cli v3 initializes a subcommand's nil Writer to
// os.Stdout independently of the parent, so simply setting Writer on the
// root has no effect on subcommand output. Tests use this to capture
// JSON output via a bytes.Buffer instead of swapping os.Stdout.
func setOutputs(app *cli.Command, w io.Writer) {
	app.Writer = w
	app.ErrWriter = w
	for _, sub := range app.Commands {
		sub.Writer = w
		sub.ErrWriter = w
	}
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:    "horde",
		Usage:   "Cloud launcher for orc workflows",
		Version: versionString(),
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
				Usage: "Machine-readable JSON output (launch, retry, status, results, list, kill, clean, hydrate, push)",
			},
		},
		// ExitErrHandler is the single place that turns a command error into a
		// JSON error envelope on stdout when --json is set. urfave/cli routes
		// every command error here via the root command, so cmd.Bool("json")
		// resolves the global flag. main() still prints "error: <msg>" to
		// stderr and exits non-zero — this only adds the machine-readable
		// object on stdout. A command that already emitted its own JSON wraps
		// errJSONEmitted around its error so we don't print a second envelope.
		ExitErrHandler: func(ctx context.Context, cmd *cli.Command, err error) {
			if err == nil || !cmd.Bool("json") {
				return
			}
			if errors.Is(err, errJSONEmitted) {
				return
			}
			_ = writeJSONTo(cmd.Writer, errorEnvelopeV1(err))
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
			updateCmd(),
			versionCmd(),
			docsCmd(),
		},
	}
}

func launchCmd() *cli.Command {
	return &cli.Command{
		Name:      "launch",
		Usage:     "Launch an orc workflow",
		ArgsUsage: "<ticket> [-- <orc-args>...]",
		Description: `Builds the worker Docker image if needed, validates the .env file
(every secret declared in .horde/config.yaml's secrets: block plus the
two canonical defaults), and launches a container that clones the repo
and runs orc. Prints the run ID on success. Use --force to launch even
if a run with the same ticket is already active.

Concurrency: docker provider caps active runs at 100 (pending + running);
aws-ecs uses the bootstrap stack's max_concurrent (default 20). Hitting
the cap fails with "max concurrent runs reached (N/N)" — wait for or
kill some runs before launching more.

With --json, output is a single JSON object with a stable status field
(launched/capped/duplicate/error); capped and duplicate then exit 0 (not 1)
so a caller can branch on status. See 'horde docs json' for the contract.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "branch",
				Usage: "Git branch to use",
			},
			&cli.StringFlag{
				// Not Required: the Action validates this itself (empty or
				// whitespace-only is rejected below). A urfave Required flag
				// fails *before* the Action and prints help to stdout without
				// routing through the root ExitErrHandler — which would break
				// the --json contract that stdout carries only a JSON object.
				Name:  "workflow",
				Usage: "Orc workflow to run (required, e.g. implement-ticket)",
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
			&cli.StringSliceFlag{
				Name:    "env",
				Aliases: []string{"e"},
				Usage:   "Set a per-launch env var (KEY=VALUE); repeatable. Overrides project secrets of the same key on docker (see 'horde docs config' for the ECS caveat). Applies to launch only; 'horde retry' does not carry it forward.",
			},
			&cli.StringSliceFlag{
				Name:  "label",
				Usage: "Attach a key=value label to the run (repeatable); filterable via `horde list --label`",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ticket := cmd.Args().First()
			if ticket == "" {
				return fmt.Errorf("missing required argument: <ticket>")
			}
			orcArgs := cmd.Args().Tail()

			branch := cmd.String("branch")
			workflow := strings.TrimSpace(cmd.String("workflow"))
			timeout := cmd.Duration("timeout")
			force := cmd.Bool("force")
			jsonOut := cmd.Bool("json")

			extraEnv, err := parseEnvFlags(cmd.StringSlice("env"))
			if err != nil {
				return err
			}

			labels, err := parseLabels(cmd.StringSlice("label"))
			if err != nil {
				return err
			}

			if workflow == "" {
				return fmt.Errorf("--workflow is required (e.g. --workflow implement-ticket)")
			}
			if strings.ContainsAny(workflow, "/\\") || strings.Contains(workflow, "..") {
				return fmt.Errorf("--workflow %q is invalid: must not contain '/', '\\', or '..'", workflow)
			}

			prov, st, maxConcurrent, provName, awsCfg, hordeCfg, cleanup, err := initProviderAndStore(ctx, cmd)
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
				reason := fmt.Sprintf("max concurrent runs reached (%d/%d)", activeCount, maxConcurrent)
				if jsonOut {
					// Capped is a protocol-level success: the caller should
					// retry later, not treat it as a failure. Exit 0 with the
					// status in the JSON.
					return writeJSONTo(cmd.Writer, launchCappedV1(ticket, workflow, branch, reason))
				}
				activeRuns, listErr := st.ListActive(ctx)
				if listErr != nil {
					return fmt.Errorf("at capacity (%d/%d active runs) but failed to list them: %w", activeCount, maxConcurrent, listErr)
				}
				fmt.Fprintf(os.Stderr, "at capacity: %d/%d active runs\n", activeCount, maxConcurrent)
				for _, r := range activeRuns {
					fmt.Fprintf(os.Stderr, "  %s  %s\n", r.ID, r.Ticket)
				}
				return fmt.Errorf("%s", reason)
			}

			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("getting home directory: %w", err)
			}

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}

			repo, err := resolveCanonicalRepo(hordeCfg, cwd)
			if err != nil {
				return err
			}

			envPath, spec, secretRemap, err := resolveSecretsForLaunch(provName, cwd)
			if err != nil {
				return err
			}

			// On ECS a per-launch --env override of a declared secret has no
			// effect: the secret lives on the task definition and AWS gives it
			// precedence over the RunTask environment override. Warn rather
			// than fail — the var is still injected, and non-secret keys (plus
			// every key on docker) override as expected.
			for _, k := range secretCollisionsOnECS(provName, spec, extraEnv) {
				fmt.Fprintf(os.Stderr, "warning: --env %s overrides a declared secret; on ECS the task-definition secret takes precedence, so this override will not take effect\n", k)
			}

			id, err := runid.Generate()
			if err != nil {
				return err
			}

			launchedBy, err := resolveLaunchedBy(ctx, provName, cwd, awsCfg, cmd.String("profile"))
			if err != nil {
				return err
			}

			active, err := st.FindActiveByTicket(ctx, repo, ticket)
			if err != nil {
				return fmt.Errorf("checking active runs: %w", err)
			}
			// Reconcile stale records: container may have died since last check.
			// Reconciliation is best-effort — a failed Finalize/UpdateRun must
			// not block the new launch, but we log so users can diagnose if
			// duplicate-ticket protection misbehaves due to a stuck record.
			stillActive := active[:0]
			for _, r := range active {
				if err := finalizeAndSync(ctx, prov, st, r, homeDir); err != nil {
					fmt.Fprintf(os.Stderr, "warning: %v\n", err)
				}
				if r.Status == store.StatusPending || r.Status == store.StatusRunning {
					stillActive = append(stillActive, r)
				}
			}
			active = stillActive
			if len(active) > 0 && !force {
				reason := "duplicate active ticket (use --force to override)"
				if jsonOut {
					// Duplicate is a protocol-level success: the caller learns
					// the existing run and decides what it means. Exit 0.
					return writeJSONTo(cmd.Writer, launchDuplicateV1(ticket, workflow, branch, active[0].ID, reason))
				}
				fmt.Fprintf(os.Stderr, "ticket %s already has an active run (%s)\n", ticket, active[0].ID)
				return fmt.Errorf("%s", reason)
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
				Labels:     labels,
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
				Repo:           repo,
				Ticket:         ticket,
				Branch:         branch,
				Workflow:       workflow,
				RunID:          id,
				EnvFile:        envPath,
				Mounts:         projCfg.ResolveMounts(cwd),
				HomeDir:        homeDir,
				OrcArgs:        orcArgs,
				SecretEnvRemap: secretRemap,
				ExtraEnv:       extraEnv,
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

			if jsonOut {
				return writeJSONTo(cmd.Writer, launchLaunchedV1(id, ticket, workflow, branch))
			}
			fmt.Println(id)
			return nil
		},
	}
}

func retryCmd() *cli.Command {
	return &cli.Command{
		Name:      "retry",
		Usage:     "Retry a failed, killed, timed-out, or rate-limited run",
		ArgsUsage: "<run-id> [-- <orc-args>...]",
		Description: `Retries a run that ended in any recoverable state (failed, killed,
timed_out, rate_limited) by relaunching against the same run. orc picks
up from where it left off. If the old worker is still alive, it is
stopped first.

On Docker the preserved on-host workspace is reused in place. On ECS a
fresh Fargate task is launched with the same run ID: the worker restores
the agent session (~/.claude) and the full working tree (/workspace,
including committed and uncommitted changes) from S3, then re-enters orc.

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

			if err := finalizeAndSync(ctx, prov, st, run, homeDir); err != nil {
				return err
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

			// Relaunch with the preserved workspace. On Docker the workspace
			// is an on-host bind-mount dir that orc re-enters in place, so it
			// must exist with a .git. On ECS there is no local workspace: the
			// worker restores the working tree (/workspace) and ~/.claude from
			// S3, keyed by RUN_ID — so the local workspace guard and the
			// on-host exit-code marker are Docker-only.
			if run.Provider == config.ProviderDocker {
				workspaceDir := provider.WorkspacePath(homeDir, run.ID)
				if _, err := os.Stat(filepath.Join(workspaceDir, ".git")); err != nil {
					return fmt.Errorf("workspace for run %s not found at %s — use 'horde launch' to start fresh", runID, workspaceDir)
				}
				// Remove stale exit code marker if present (legacy containers)
				os.Remove(filepath.Join(workspaceDir, ".horde-exit-code"))
			}

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}
			envPath, _, secretRemap, err := resolveSecretsForLaunch(run.Provider, cwd)
			if err != nil {
				return err
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

			result, err := prov.Launch(ctx, provider.LaunchOpts{
				Repo:           run.Repo,
				Ticket:         run.Ticket,
				Branch:         run.Branch,
				Workflow:       run.Workflow,
				RunID:          run.ID,
				EnvFile:        envPath,
				Mounts:         projCfg.ResolveMounts(cwd),
				HomeDir:        homeDir,
				OrcArgs:        orcArgs,
				SecretEnvRemap: secretRemap,
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

			if cmd.Bool("json") {
				return writeJSONTo(cmd.Writer, retryV1(runID, run.Ticket))
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
		Description: `Shows run detail: ID, ticket, status, exit code, duration, cost,
token usage, and who launched it. For running containers, reads live cost
and token counts from the container. Also detects completed or timed-out runs and triggers
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

			if err := finalizeAndSync(ctx, prov, st, run, homeDir); err != nil {
				return err
			}
			if run.TotalCostUSD == nil && (run.Status == store.StatusRunning || run.Status == store.StatusPending) {
				if dp, ok := prov.(*provider.DockerProvider); ok {
					cost, tokens := fetchLiveTelemetry(ctx, dp, run)
					run.TotalCostUSD = cost
					if run.Tokens == nil {
						run.Tokens = tokens
					}
				}
			}
			if cmd.Bool("json") {
				return writeJSONTo(cmd.Writer, statusToV1(run))
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

			if err := finalizeAndSync(ctx, prov, st, run, homeDir); err != nil {
				return err
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
					if logData, err := io.ReadAll(logs); err == nil {
						provider.SaveContainerLog(resultsDir, run.ID, logData)
					}
					logs.Close()
				}

				if err := prov.Stop(ctx, provider.StopOpts{
					InstanceID: run.InstanceID,
					ResultsDir: resultsDir,
				}); err != nil {
					return fmt.Errorf("killing run: %w", err)
				}

				// Best-effort: read run-result.json for cost and exit code.
				// orc may have crashed before writing it — the helper returns
				// nils in that case and we record killed without those fields.
				cost, exitCode = provider.ReadRunResult(homeDir, run)
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
			if cmd.Bool("json") {
				return writeJSONTo(cmd.Writer, killV1(runID, exitCode, cost))
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
total cost, total duration, token usage, and a per-phase breakdown. Reports partial
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

			if err := finalizeAndSync(ctx, prov, st, run, homeDir); err != nil {
				return err
			}
			if run.Status == store.StatusPending || run.Status == store.StatusRunning {
				if cmd.Bool("json") {
					return writeJSONTo(cmd.Writer, partialResultsToV1(run))
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
					return writeJSONTo(cmd.Writer, partialResultsToV1(run))
				}
				printPartialResults(run)
				return nil
			}
			var result fullRunResult
			if err := json.Unmarshal(data, &result); err != nil {
				if cmd.Bool("json") {
					return writeJSONTo(cmd.Writer, partialResultsToV1(run))
				}
				printPartialResults(run)
				return nil
			}
			if cmd.Bool("json") {
				return writeJSONTo(cmd.Writer, fullResultsToV1(run, &result))
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
include terminal runs (success, failed, killed, timed_out, rate_limited).

Filters (AND-combined, all scoped to the current repo):
  --label key=value   only runs carrying this label (repeatable; all must match)
  --status <status>   only runs in this status (repeatable); implies --all's breadth
  --workflow <name>   only runs of this workflow
  --ticket <id>       only runs for this ticket
  --since <when>      only runs started at/after <when>
  --until <when>      only runs started at/before <when>

<when> is an RFC3339 timestamp (2026-04-01 or 2026-04-01T12:00:00Z) or a
duration-ago (1h, 30m, 7d).`,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "all",
				Usage: "Include terminal runs (success/failed/killed/timed_out/rate_limited)",
			},
			&cli.StringSliceFlag{
				Name:  "label",
				Usage: "Only runs carrying this key=value label (repeatable; AND-combined)",
			},
			&cli.StringSliceFlag{
				Name:  "status",
				Usage: "Only runs in this status (repeatable); implies --all's breadth",
			},
			&cli.StringFlag{
				Name:  "workflow",
				Usage: "Only runs of this workflow",
			},
			&cli.StringFlag{
				Name:  "ticket",
				Usage: "Only runs for this ticket",
			},
			&cli.StringFlag{
				Name:  "since",
				Usage: "Only runs started at/after this time (RFC3339 or duration-ago like 1h, 7d)",
			},
			&cli.StringFlag{
				Name:  "until",
				Usage: "Only runs started at/before this time (RFC3339 or duration-ago like 1h, 7d)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			all := cmd.Bool("all")

			labels, err := parseLabels(cmd.StringSlice("label"))
			if err != nil {
				return err
			}

			statuses, err := parseStatuses(cmd.StringSlice("status"))
			if err != nil {
				return err
			}

			since, err := parseWhen(cmd.String("since"))
			if err != nil {
				return fmt.Errorf("--since: %w", err)
			}
			until, err := parseWhen(cmd.String("until"))
			if err != nil {
				return fmt.Errorf("--until: %w", err)
			}

			prov, st, _, _, _, hordeCfg, cleanup, err := initProviderAndStoreWith(ctx, cmd.String("provider"), cmd.String("profile"), defaultFactoryDeps())
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

			repo, err := resolveCanonicalRepo(hordeCfg, cwd)
			if err != nil {
				return err
			}

			// Fetch with the non-status filters. Status is applied AFTER
			// finalize, because finalizeAndSync can flip a run's status
			// (running → terminal) during this call — filtering on the stored
			// status first would drop runs that just completed.
			runs, err := st.ListRuns(ctx, store.RunFilter{
				Repo:     repo,
				Workflow: cmd.String("workflow"),
				Ticket:   cmd.String("ticket"),
				Labels:   labels,
				Since:    since,
				Until:    until,
			})
			if err != nil {
				return fmt.Errorf("listing runs: %w", err)
			}

			for _, run := range runs {
				if err := finalizeAndSync(ctx, prov, st, run, homeDir); err != nil {
					fmt.Fprintf(os.Stderr, "warning: %v\n", err)
					continue
				}
				if dp, ok := prov.(*provider.DockerProvider); ok {
					if run.TotalCostUSD == nil && (run.Status == store.StatusRunning || run.Status == store.StatusPending) {
						cost, tokens := fetchLiveTelemetry(ctx, dp, run)
						run.TotalCostUSD = cost
						if run.Tokens == nil {
							run.Tokens = tokens
						}
					}
				}
			}

			// Resolve the effective status filter:
			//   - explicit --status wins (and spans terminal runs too)
			//   - else --all = no status filter
			//   - else default to active-only (pending/running)
			runs = applyStatusFilter(runs, statuses, all)

			if cmd.Bool("json") {
				return writeJSONTo(cmd.Writer, listToV1(runs))
			}

			if len(runs) == 0 {
				filtered := len(statuses) > 0 || cmd.String("workflow") != "" || cmd.String("ticket") != "" || len(labels) > 0 || since != nil || until != nil
				switch {
				case filtered:
					fmt.Println("No matching runs for this repo.")
				case all:
					fmt.Println("No runs found for this repo.")
				default:
					fmt.Println("No active runs for this repo.")
				}
				return nil
			}

			printRunTable(runs)
			printRunSummary(runs)
			return nil
		},
	}
}

// applyStatusFilter narrows runs by the effective status dimension. An explicit
// status set takes precedence and spans all statuses (including terminal). With
// no status set, --all returns everything and the default keeps only active
// (pending/running) runs.
func applyStatusFilter(runs []*store.Run, statuses []store.Status, all bool) []*store.Run {
	if len(statuses) > 0 {
		want := make(map[store.Status]bool, len(statuses))
		for _, s := range statuses {
			want[s] = true
		}
		filtered := runs[:0]
		for _, run := range runs {
			if want[run.Status] {
				filtered = append(filtered, run)
			}
		}
		return filtered
	}
	if all {
		return runs
	}
	filtered := runs[:0]
	for _, run := range runs {
		if run.Status == store.StatusPending || run.Status == store.StatusRunning {
			filtered = append(filtered, run)
		}
	}
	return filtered
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
			prov, st, _, _, _, hordeCfg, cleanup, err := initProviderAndStoreWith(ctx, cmd.String("provider"), cmd.String("profile"), defaultFactoryDeps())
			if err != nil {
				return err
			}
			defer cleanup()

			homeDir, _ := os.UserHomeDir()
			runID := cmd.Args().First()

			purge := cmd.Bool("purge")
			jsonOut := cmd.Bool("json")

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
				var removed []string
				if dp, ok := prov.(*provider.DockerProvider); ok && run.InstanceID != "" {
					if err := dp.RemoveContainer(ctx, run.InstanceID); err != nil {
						fmt.Fprintf(os.Stderr, "warning: %v\n", err)
					} else {
						removed = append(removed, runID)
						if !jsonOut {
							fmt.Printf("Removed container for run %s\n", runID)
						}
					}
				}
				homeDir, _ := os.UserHomeDir()
				if homeDir != "" {
					workspaceDir := provider.WorkspacePath(homeDir, runID)
					sessionsDir := provider.SessionsPath(homeDir, runID)
					if purge {
						if err := removeWorkspace(ctx, workspaceDir); err != nil {
							fmt.Fprintf(os.Stderr, "warning: removing workspace: %v\n", err)
						} else if !jsonOut {
							fmt.Printf("Removed workspace for run %s\n", runID)
						}
						if err := removeWorkspace(ctx, sessionsDir); err != nil {
							fmt.Fprintf(os.Stderr, "warning: removing sessions: %v\n", err)
						}
					} else if _, err := os.Stat(workspaceDir); err == nil {
						fmt.Fprintf(os.Stderr, "note: workspace preserved at %s (use --purge to remove)\n", workspaceDir)
					}
				}
				if jsonOut {
					return writeJSONTo(cmd.Writer, cleanV1(removed))
				}
				return nil
			}

			// Clean all terminal runs
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}
			repo, err := resolveCanonicalRepo(hordeCfg, cwd)
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
			var removed []string
			for _, r := range allRuns {
				if activeIDs[r.ID] {
					continue
				}
				if dp, ok := prov.(*provider.DockerProvider); ok && r.InstanceID != "" {
					if err := dp.RemoveContainer(ctx, r.InstanceID); err != nil {
						fmt.Fprintf(os.Stderr, "warning: removing container for run %s: %v\n", r.ID, err)
					} else {
						removed = append(removed, r.ID)
					}
				}
				if purge && homeDir != "" {
					removeWorkspace(ctx, provider.WorkspacePath(homeDir, r.ID))
					removeWorkspace(ctx, provider.SessionsPath(homeDir, r.ID))
				}
			}
			if jsonOut {
				return writeJSONTo(cmd.Writer, cleanV1(removed))
			}
			fmt.Printf("Removed %d container(s)\n", len(removed))
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

			if err := finalizeAndSync(ctx, prov, st, run, homeDir); err != nil {
				return err
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
	TotalCostUSD             float64 `json:"total_cost_usd"`
	TotalInputTokens         int     `json:"total_input_tokens"`
	TotalOutputTokens        int     `json:"total_output_tokens"`
	TotalCacheCreationTokens int     `json:"total_cache_creation_input_tokens"`
	TotalCacheReadTokens     int     `json:"total_cache_read_input_tokens"`
	Phases                   []struct {
		Turns int `json:"turns"`
	} `json:"phases"`
}

// envKeyPattern matches a valid POSIX-style env-var name.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnvKeys are the horde-managed control vars wired from other launch
// flags / run metadata. A --env override of any of these would break the run,
// so parseEnvFlags rejects them.
var reservedEnvKeys = map[string]bool{
	"REPO_URL":         true,
	"TICKET":           true,
	"BRANCH":           true,
	"WORKFLOW":         true,
	"RUN_ID":           true,
	"ARTIFACTS_BUCKET": true,
	"ORC_EXTRA_ARGS":   true,
}

// parseEnvFlags turns repeatable --env KEY=VALUE entries into a map. KEY must
// be a valid env-var name and must not be one of the horde-managed control
// vars. VALUE may be empty and may itself contain '='. On a duplicate key the
// last occurrence wins (consistent with docker run -e).
func parseEnvFlags(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, entry := range raw {
		key, val, found := strings.Cut(entry, "=")
		if !found {
			return nil, fmt.Errorf("parsing --env %q: expected KEY=VALUE", entry)
		}
		if !envKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("parsing --env %q: invalid key %q (must match [A-Za-z_][A-Za-z0-9_]*)", entry, key)
		}
		if reservedEnvKeys[key] {
			return nil, fmt.Errorf("parsing --env %q: %q is reserved by horde and cannot be overridden", entry, key)
		}
		out[key] = val
	}
	return out, nil
}

// secretCollisionsOnECS returns the sorted --env keys that collide with a
// declared secret when launching on ECS. Such overrides cannot take effect
// there (AWS gives the task-definition secret precedence over the RunTask
// environment override), so the caller warns the user. Returns nil for any
// other provider, where per-launch --env overrides declared secrets normally.
func secretCollisionsOnECS(provName string, spec config.SecretSpec, extraEnv map[string]string) []string {
	if provName != config.ProviderECS {
		return nil
	}
	var collisions []string
	for k := range extraEnv {
		if _, declared := spec[k]; declared {
			collisions = append(collisions, k)
		}
	}
	sort.Strings(collisions)
	return collisions
}

// resolveSecretsForLaunch loads .horde/config.yaml, merges in the canonical
// secret defaults, and validates the result against the active provider.
// For docker it also verifies dir/.env covers every host env-var name the
// spec references. Returns the (possibly empty) envPath, the merged spec,
// and a non-canonical "container-name -> host-env-name" remap for the
// docker provider; the ECS provider gets nil remap (its task definition
// is baked at bootstrap time).
func resolveSecretsForLaunch(provName, dir string) (envPath string, spec config.SecretSpec, remap map[string]string, err error) {
	cfg, err := config.LoadProjectConfig(dir)
	if err != nil {
		return "", nil, nil, err
	}
	spec = config.MergeSecrets(cfg.Secrets)
	if err := spec.ValidateForProvider(provName); err != nil {
		return "", nil, nil, err
	}
	if provName == config.ProviderDocker {
		envPath, err = config.ValidateEnvFileFor(dir, spec)
		if err != nil {
			return "", nil, nil, err
		}
		remap = map[string]string{}
		for containerName, src := range spec {
			if src.Env == "" {
				continue
			}
			// Canonicals are covered by --env-file (their container-name
			// matches the host-name verbatim by definition). Skip identity
			// mappings to keep argv minimal.
			if config.IsCanonical(containerName) && src.Env == containerName {
				continue
			}
			if src.Env == containerName {
				continue
			}
			remap[containerName] = src.Env
		}
		// For non-canonical entries whose container-name matches
		// host-name, --env-file already publishes them under the right
		// name (since .env contains them and docker forwards everything
		// in the file). No extra -e needed in that case either.
	}
	return envPath, spec, remap, nil
}

// fetchLiveTelemetry reads current cost and token totals from a running
// container's costs.json. Both are best-effort (nil when unavailable). This is
// the Docker lazy-live path: the store has no values for a running run until
// Finalize, so status/list read the live file on demand.
func fetchLiveTelemetry(ctx context.Context, prov *provider.DockerProvider, run *store.Run) (*float64, *store.TokenUsage) {
	if run.InstanceID == "" {
		return nil, nil
	}
	costsPath := "/workspace/.orc/" + provider.AuditRelPath(run.Workflow, run.Ticket, "costs.json")
	data, err := prov.ReadContainerFile(ctx, run.InstanceID, costsPath)
	if err != nil {
		return nil, nil
	}
	var lc liveCosts
	if json.Unmarshal(data, &lc) != nil {
		return nil, nil
	}
	var cost *float64
	if lc.TotalCostUSD != 0 {
		cost = &lc.TotalCostUSD
	}
	turns := 0
	for _, p := range lc.Phases {
		turns += p.Turns
	}
	var tokens *store.TokenUsage
	if lc.TotalInputTokens != 0 || lc.TotalOutputTokens != 0 ||
		lc.TotalCacheCreationTokens != 0 || lc.TotalCacheReadTokens != 0 || turns != 0 {
		tokens = &store.TokenUsage{
			InputTokens:         lc.TotalInputTokens,
			OutputTokens:        lc.TotalOutputTokens,
			CacheCreationTokens: lc.TotalCacheCreationTokens,
			CacheReadTokens:     lc.TotalCacheReadTokens,
			Turns:               turns,
		}
	}
	return cost, tokens
}

type fullRunResult struct {
	ExitCode      int           `json:"exit_code"`
	Status        string        `json:"status"`
	Ticket        string        `json:"ticket"`
	Workflow      string        `json:"workflow"`
	TotalCostUSD  *float64      `json:"total_cost_usd"`
	TotalDuration string        `json:"total_duration"`
	Phases        []phaseResult `json:"phases"`

	TotalInputTokens         *int `json:"total_input_tokens"`
	TotalOutputTokens        *int `json:"total_output_tokens"`
	TotalCacheCreationTokens *int `json:"total_cache_creation_input_tokens"`
	TotalCacheReadTokens     *int `json:"total_cache_read_input_tokens"`
	Turns                    *int `json:"turns"`
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
	fmt.Fprintln(w, "RUN ID\tTICKET\tWORKFLOW\tBRANCH\tSTATUS\tDURATION\tCOST")
	for _, run := range runs {
		branch := run.Branch
		if branch == "" {
			branch = "(default)"
		}

		workflow := run.Workflow
		if workflow == "" {
			workflow = "-"
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

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			run.ID, run.Ticket, workflow, branch, run.Status, duration, cost)
	}
	w.Flush()
}

// printRunSummary prints a one-line cohort rollup under the run table: the
// number of runs shown and their total known cost. This is the human-readable
// face of the ListV1.summary aggregate used by `--json`.
func printRunSummary(runs []*store.Run) {
	var total float64
	for _, run := range runs {
		if run.TotalCostUSD != nil {
			total += *run.TotalCostUSD
		}
	}
	noun := "runs"
	if len(runs) == 1 {
		noun = "run"
	}
	fmt.Printf("\n%d %s, $%.2f total\n", len(runs), noun, total)
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
	} else {
		fmt.Printf("Cost:        -\n")
	}
	if run.Tokens != nil {
		fmt.Printf("Tokens:      %s\n", formatTokens(run.Tokens))
	}
	fmt.Printf("Launched by: %s\n", run.LaunchedBy)
	if len(run.Labels) > 0 {
		fmt.Printf("Labels:      %s\n", formatLabels(run.Labels))
	}
	if homeDir, err := os.UserHomeDir(); err == nil {
		wsDir := provider.WorkspacePath(homeDir, run.ID)
		if _, err := os.Stat(wsDir); err == nil {
			fmt.Printf("Workspace:   %s\n", wsDir)
		}
	}
}

// formatTokens renders token usage as a compact one-line summary for the human
// status/results output, e.g. "in 54791, out 87915, cache 9463873 (529692 write
// / 8934181 read), 12 turns".
func formatTokens(t *store.TokenUsage) string {
	return fmt.Sprintf("in %d, out %d, cache %d (%d write / %d read), %d turns",
		t.InputTokens, t.OutputTokens,
		t.CacheCreationTokens+t.CacheReadTokens, t.CacheCreationTokens, t.CacheReadTokens,
		t.Turns)
}

// formatLabels renders a label map as "k=v, k2=v2" with keys sorted for stable,
// deterministic output.
func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, ", ")
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
	if run.Tokens != nil {
		fmt.Printf("Total Tokens:   %s\n", formatTokens(run.Tokens))
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

// resolveCanonicalRepo returns the canonical repository identifier used to
// scope run records. On aws-ecs the value comes from SSM (cfg.Repo), so every
// CLI invocation against the same deployment writes and queries the same
// string regardless of the local git remote. Docker has no SSM analog, so
// it falls back to deriving from the local git remote — fine in practice
// because the docker provider is single-user.
func resolveCanonicalRepo(cfg *config.HordeConfig, cwd string) (string, error) {
	if cfg != nil && cfg.Repo != "" {
		return cfg.Repo, nil
	}
	return config.RepoURL(cwd)
}

// initProviderAndStore creates the Provider and Store based on the --provider flag.
// Selection rule: "docker" → DockerProvider + SQLite; "aws-ecs" → ECS + DynamoDB;
// "" → auto-detect via SSM. The returned *aws.Config is non-nil for aws-ecs so
// callers can reuse the loaded config for STS / SDK calls without a duplicate
// awscfg.Load round-trip. The returned *config.HordeConfig is also non-nil for
// aws-ecs so callers can read deployment-scoped values (e.g. cfg.Repo). Returns
// a cleanup function that must be deferred to release store resources.
func initProviderAndStore(ctx context.Context, cmd *cli.Command) (provider.Provider, store.Store, int, string, *aws.Config, *config.HordeConfig, func(), error) {
	return initProviderAndStoreWith(ctx, cmd.String("provider"), cmd.String("profile"), defaultFactoryDeps())
}

// initFromRunID opens the store, looks up the run, and creates the provider
// from the stored run record. If --provider is set, it overrides the stored value.
func initFromRunID(ctx context.Context, cmd *cli.Command, runID string) (provider.Provider, store.Store, *store.Run, func(), error) {
	return initFromRunIDWith(ctx, cmd.String("provider"), cmd.String("profile"), runID, defaultFactoryDeps())
}

// finalizeAndSync runs prov.Finalize on the in-memory run and persists any
// resulting status change via st.UpdateRun. The pattern was duplicated at 7
// sites — extracting it keeps the Finalize+UpdateRun protocol in one place
// so a fix to either side only has to be made once.
//
// Callers that want to abort on failure check the returned error directly;
// callers that want best-effort reconciliation (launch reconciliation, list)
// log the error and continue. Either way the run pointer is mutated in place.
func finalizeAndSync(ctx context.Context, prov provider.Provider, st store.Store, run *store.Run, homeDir string) error {
	origStatus := run.Status
	if err := prov.Finalize(ctx, run, homeDir); err != nil {
		return fmt.Errorf("finalizing run %s: %w", run.ID, err)
	}
	if run.Status == origStatus {
		return nil
	}
	if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
		Status:       &run.Status,
		ExitCode:     run.ExitCode,
		CompletedAt:  run.CompletedAt,
		TotalCostUSD: run.TotalCostUSD,
	}); err != nil {
		return fmt.Errorf("updating run %s: %w", run.ID, err)
	}
	return nil
}
