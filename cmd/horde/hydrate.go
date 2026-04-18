package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/jorge-barreto/horde/internal/store"
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

func isTerminalStatus(s store.Status) bool {
	switch s {
	case store.StatusSuccess, store.StatusFailed, store.StatusKilled:
		return true
	default:
		return false
	}
}
