package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	horde "github.com/jorge-barreto/horde"
	"github.com/jorge-barreto/horde/internal/event"
	"github.com/jorge-barreto/horde/internal/provider"
	"github.com/jorge-barreto/horde/internal/runid"
	"github.com/jorge-barreto/horde/internal/store"
	"github.com/urfave/cli/v3"
)

// execLocalAllowed returns an error when --local is used with a provider that
// has no host working tree. This is a pure function so it can be unit-tested
// without requiring a live provider.
func execLocalAllowed(provName string) error {
	if provName != "docker" {
		return fmt.Errorf("--local requires the docker provider; on %s use a committed source (it has no host working tree to seed from)", provName)
	}
	return nil
}

func execCmd() *cli.Command {
	return &cli.Command{
		Name:      "exec",
		Usage:     "Run an arbitrary orc subcommand in a worker",
		ArgsUsage: "[--local] -- <orc-args>...",
		Description: `Runs an arbitrary orc subcommand (eval, validate, test, …) in a worker
and tracks it as a run. Everything after -- is opaque orc argv; horde does
not parse it. Unlike 'horde launch' there is no --workflow requirement and
no duplicate-ticket guard — this is the general passthrough, 'launch' is the
specialized orc run path.

--local (docker only) seeds the run's workspace from the current working
tree (committed AND uncommitted changes), so the run sees your local state
without a remote round-trip and without mutating your checkout. It is
recorded only in local history. On aws-ecs --local is an error.

With --json, output is a single JSON object with a status field
(launched/capped); capped exits 0. See 'horde docs json'.`,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "local",
				Usage: "Seed the workspace from the current working tree (docker only); see 'horde docs exec'",
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "Timeout for the run",
				Value: 24 * time.Hour,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			orcArgv := cmd.Args().Slice()
			if len(orcArgv) == 0 {
				return fmt.Errorf("missing orc args: usage is 'horde exec [--local] -- <orc-subcommand> [args...]'")
			}
			subcommand := orcArgv[0]
			orcArgs := orcArgv[1:]
			local := cmd.Bool("local")
			timeout := cmd.Duration("timeout")
			jsonOut := cmd.Bool("json")

			prov, st, maxConcurrent, provName, awsCfg, hordeCfg, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			if local {
				if err := execLocalAllowed(provName); err != nil {
					return err
				}
			}

			activeCount, err := st.CountActive(ctx)
			if err != nil {
				return fmt.Errorf("checking concurrency: %w", err)
			}
			if activeCount >= maxConcurrent {
				reason := fmt.Sprintf("max concurrent runs reached (%d/%d)", activeCount, maxConcurrent)
				if jsonOut {
					return writeJSONTo(cmd.Writer, execCappedV1(orcArgv, local, reason))
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

			resolver := newResolver(cmd)
			repo, err := resolveCanonicalRepo(hordeCfg, resolver)
			if err != nil {
				// On the --local path there may be no git remote. The worker
				// entrypoint has an EARLY guard `if [ -z "${REPO_URL:-}" ]` that
				// fires before the skip-clone branch, so even a --local run must
				// launch with a non-empty Repo/REPO_URL. Substitute a synthetic
				// key derived from the working-directory name so the run is
				// scoped to this project without requiring a remote.
				if local {
					repo = "local/" + filepath.Base(cwd)
				} else {
					return err
				}
			}

			envPath, spec, secretRemap, err := resolveSecretsForLaunch(provName, resolver)
			if err != nil {
				return err
			}
			_ = spec // exec does not warn on ECS secret collisions (no --env surface in v1)

			id, err := runid.Generate()
			if err != nil {
				return err
			}
			launchedBy, err := resolveLaunchedBy(ctx, provName, cwd, awsCfg, cmd.String("profile"))
			if err != nil {
				return err
			}

			now := time.Now()
			run := &store.Run{
				ID:         id,
				Repo:       repo,
				Provider:   provName,
				Status:     store.StatusPending,
				Labels:     map[string]string{"horde.exec": subcommand},
				LaunchedBy: launchedBy,
				StartedAt:  now,
				TimeoutAt:  now.Add(timeout),
			}
			if err := st.CreateRun(ctx, run); err != nil {
				return fmt.Errorf("recording run: %w", err)
			}

			// Docker: ensure image, then (if --local) seed the per-run workspace
			// from the working tree so the entrypoint skips the clone step.
			if dp, ok := prov.(*provider.DockerProvider); ok {
				workerFS, err := fs.Sub(horde.WorkerFiles, "docker")
				if err != nil {
					return fmt.Errorf("accessing worker files: %w", err)
				}
				if err := dp.EnsureImage(ctx, workerFS, cwd, os.Stderr); err != nil {
					markFailed(ctx, st, id)
					return fmt.Errorf("preparing worker image: %w", err)
				}
				if local {
					wsDir := provider.WorkspacePath(homeDir, id)
					if err := os.MkdirAll(wsDir, 0o777); err != nil {
						markFailed(ctx, st, id)
						return fmt.Errorf("creating workspace: %w", err)
					}
					if err := provider.SeedWorkspaceFromWorkingTree(ctx, cwd, wsDir); err != nil {
						markFailed(ctx, st, id)
						return fmt.Errorf("seeding local workspace: %w", err)
					}
				}
			}

			projCfg, err := resolver.ProjectConfig()
			if err != nil {
				return err
			}
			result, err := prov.Launch(ctx, provider.LaunchOpts{
				Repo:           repo,
				RunID:          id,
				EnvFile:        envPath,
				Mounts:         projCfg.ResolveMounts(resolver.EnvFileDir()),
				HomeDir:        homeDir,
				OrcArgs:        orcArgs,
				OrcSubcommand:  subcommand,
				SecretEnvRemap: secretRemap,
			})
			if err != nil {
				markFailed(ctx, st, id)
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

			run.Status = store.StatusRunning
			run.InstanceID = result.InstanceID
			run.Metadata = result.Metadata
			if err := newEmitter(awsCfg, hordeCfg).Emit(ctx, event.TypeRunStarted, event.DetailFromRun(run)); err != nil {
				fmt.Fprintf(os.Stderr, "warning: emitting run.started: %v\n", err)
			}

			if jsonOut {
				return writeJSONTo(cmd.Writer, execLaunchedV1(id, orcArgv, local))
			}
			fmt.Fprintln(cmd.Writer, id)
			return nil
		},
	}
}

// markFailed flips a run to failed (best-effort) on a launch-path error.
// Using a shared helper avoids duplicating the pattern inline.
func markFailed(ctx context.Context, st store.Store, id string) {
	failed := store.StatusFailed
	now := time.Now()
	if err := st.UpdateRun(ctx, id, &store.RunUpdate{Status: &failed, CompletedAt: &now}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to mark run as failed: %v\n", err)
	}
}
