package main

import (
	"path/filepath"
	"testing"
)

func TestHydrateDestPaths_DefaultWorkflow(t *testing.T) {
	t.Parallel()
	auditDir, artDir := hydrateDestPaths("/out", "", "PROJ-123", "abc123")
	wantAudit := filepath.Join("/out", ".orc", "audit", "PROJ-123-abc123")
	wantArt := filepath.Join("/out", ".orc", "artifacts", "PROJ-123-abc123")
	if auditDir != wantAudit {
		t.Errorf("audit dir: got %q want %q", auditDir, wantAudit)
	}
	if artDir != wantArt {
		t.Errorf("artifacts dir: got %q want %q", artDir, wantArt)
	}
}

func TestHydrateDestPaths_NamedWorkflow(t *testing.T) {
	t.Parallel()
	auditDir, artDir := hydrateDestPaths("/out", "review", "PROJ-123", "abc123")
	wantAudit := filepath.Join("/out", ".orc", "audit", "review", "PROJ-123-abc123")
	wantArt := filepath.Join("/out", ".orc", "artifacts", "review", "PROJ-123-abc123")
	if auditDir != wantAudit {
		t.Errorf("audit dir: got %q want %q", auditDir, wantAudit)
	}
	if artDir != wantArt {
		t.Errorf("artifacts dir: got %q want %q", artDir, wantArt)
	}
}
