package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jorge-barreto/horde/internal/provider"
	"github.com/urfave/cli/v3"
)

// hydrateDestPaths returns (auditDir, artifactsDir) for hydrating a run
// into intoDir. Layout matches orc's expected tree with the single twist
// that the leaf ticket segment is "<ticket>-<run-id>" to prevent collisions
// across multiple runs of the same ticket.
func hydrateDestPaths(intoDir, workflow, ticket, runID string) (audit, artifacts string) {
	leaf := ticket + "-" + runID
	if workflow == "" {
		audit = filepath.Join(intoDir, ".orc", "audit", leaf)
		artifacts = filepath.Join(intoDir, ".orc", "artifacts", leaf)
		return
	}
	audit = filepath.Join(intoDir, ".orc", "audit", workflow, leaf)
	artifacts = filepath.Join(intoDir, ".orc", "artifacts", workflow, leaf)
	return
}

type hydrateStatus string

const (
	hydrateStatusHydrated hydrateStatus = "hydrated"
	hydrateStatusSkipped  hydrateStatus = "skipped"
	hydrateStatusFailed   hydrateStatus = "failed"
)

type hydrateOutcome struct {
	RunID  string
	Status hydrateStatus
	Err    error
}

func hydrateSummary(outs []hydrateOutcome) string {
	var h, s, f int
	for _, o := range outs {
		switch o.Status {
		case hydrateStatusHydrated:
			h++
		case hydrateStatusSkipped:
			s++
		case hydrateStatusFailed:
			f++
		}
	}
	return fmt.Sprintf("hydrated: %d, skipped: %d, failed: %d", h, s, f)
}

func hydrateHasFailure(outs []hydrateOutcome) bool {
	for _, o := range outs {
		if o.Status == hydrateStatusFailed {
			return true
		}
	}
	return false
}

func hydrateWriteFailures(w io.Writer, outs []hydrateOutcome) {
	for _, o := range outs {
		if o.Status != hydrateStatusFailed {
			continue
		}
		fmt.Fprintf(w, "  %s: %v\n", o.RunID, o.Err)
	}
}

func hydrateCmd() *cli.Command {
	return hydrateCmdWith(defaultFactoryDeps())
}

func hydrateCmdWith(deps factoryDeps) *cli.Command {
	return &cli.Command{
		Name:      "hydrate",
		Usage:     "Copy run artifacts to a local directory for orc improve/doctor",
		ArgsUsage: "<run-id> [<run-id>...] --into <dir>",
		Description: `Materializes .orc/audit/ and .orc/artifacts/ from one or more
completed runs into --into <dir>. Each run is written under a
"<ticket>-<run-id>" leaf segment to avoid collisions across runs of
the same ticket:

  <dir>/.orc/audit/<ticket>-<run-id>/...
  <dir>/.orc/artifacts/<ticket>-<run-id>/...

For runs that used a named workflow, the workflow name is inserted
before the leaf, matching orc's named-workflow layout.

Runs whose destination subdirectory already exists are skipped. To
re-hydrate, delete the subdirectory and re-run.

Exit 0 if all run-ids were hydrated or skipped. Exit non-zero if any
run-id failed (missing, still running, transport error, etc.); the
successful runs are still materialized.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "into",
				Usage:    "Destination directory (will be created)",
				Required: true,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			into := cmd.String("into")
			runIDs := cmd.Args().Slice()
			if len(runIDs) == 0 {
				return fmt.Errorf("missing required argument: one or more <run-id>")
			}
			if err := os.MkdirAll(into, 0o755); err != nil {
				return fmt.Errorf("creating --into directory: %w", err)
			}

			profile := cmd.String("profile")
			provFlag := cmd.String("provider")

			outcomes := make([]hydrateOutcome, 0, len(runIDs))
			for _, runID := range runIDs {
				outcomes = append(outcomes, hydrateOne(ctx, deps, provFlag, profile, runID, into))
			}

			if hydrateHasFailure(outcomes) {
				fmt.Fprintln(os.Stderr, "failures:")
				hydrateWriteFailures(os.Stderr, outcomes)
			}
			fmt.Println(hydrateSummary(outcomes))

			if hydrateHasFailure(outcomes) {
				return cli.Exit("", 1)
			}
			return nil
		},
	}
}

func hydrateOne(ctx context.Context, deps factoryDeps, provFlag, profile, runID, into string) hydrateOutcome {
	prov, _, run, cleanup, err := initFromRunIDWith(ctx, provFlag, profile, runID, deps)
	if err != nil {
		return hydrateOutcome{RunID: runID, Status: hydrateStatusFailed, Err: err}
	}
	defer cleanup()

	if !run.Status.IsTerminal() {
		return hydrateOutcome{RunID: runID, Status: hydrateStatusFailed,
			Err: fmt.Errorf("run is not terminal (status: %s)", run.Status)}
	}

	auditDir, artifactsDir := hydrateDestPaths(into, run.Workflow, run.Ticket, run.ID)

	// Idempotent: if the audit dir has already been materialized, skip.
	// (Some workflows produce no artifacts/, so we only check auditDir.)
	if dirExists(auditDir) {
		return hydrateOutcome{RunID: runID, Status: hydrateStatusSkipped}
	}

	if err := prov.HydrateRun(ctx, provider.HydrateOpts{
		RunID:            run.ID,
		Workflow:         run.Workflow,
		Ticket:           run.Ticket,
		InstanceID:       run.InstanceID,
		Metadata:         run.Metadata,
		DestAuditDir:     auditDir,
		DestArtifactsDir: artifactsDir,
		DestConfigDir:    filepath.Join(into, ".orc"),
	}); err != nil {
		var nf *provider.FileNotFoundError
		if errors.As(err, &nf) {
			return hydrateOutcome{RunID: runID, Status: hydrateStatusFailed,
				Err: fmt.Errorf("artifacts not available: %w", err)}
		}
		return hydrateOutcome{RunID: runID, Status: hydrateStatusFailed, Err: err}
	}
	return hydrateOutcome{RunID: runID, Status: hydrateStatusHydrated}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
