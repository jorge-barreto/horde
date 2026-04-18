package main

import (
	"path/filepath"
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
