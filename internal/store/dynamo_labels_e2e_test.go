package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/jorge-barreto/horde/internal/awscfg"
)

// TestDynamoLabels_LiveE2E exercises the label + filter query path against a
// REAL DynamoDB runs table. It is developer-local only (never CI): it runs only
// when HORDE_E2E_LABELS=1, and needs:
//
//	HORDE_E2E_LABELS=1
//	HORDE_E2E_PROFILE=prepdesk            (AWS profile with a live SSO token)
//	HORDE_E2E_RUNS_TABLE=horde-runs-...   (the deployed runs table name)
//
// It writes rows under a unique synthetic repo so it never collides with real
// run history, then deletes every row it created.
func TestDynamoLabels_LiveE2E(t *testing.T) {
	if os.Getenv("HORDE_E2E_LABELS") != "1" {
		t.Skip("set HORDE_E2E_LABELS=1 (plus HORDE_E2E_PROFILE, HORDE_E2E_RUNS_TABLE) to run the live DynamoDB label test")
	}
	profile := os.Getenv("HORDE_E2E_PROFILE")
	table := os.Getenv("HORDE_E2E_RUNS_TABLE")
	if table == "" {
		t.Fatal("HORDE_E2E_RUNS_TABLE is required")
	}

	ctx := context.Background()
	cfg, err := awscfg.Load(ctx, profile)
	if err != nil {
		t.Fatalf("loading AWS config: %v", err)
	}
	st, err := NewDynamoStore(ctx, cfg, table)
	if err != nil {
		t.Fatalf("NewDynamoStore: %v", err)
	}
	raw := dynamodb.NewFromConfig(cfg)

	// Unique repo so this run's rows are isolated and easy to clean up.
	repo := fmt.Sprintf("e2e.test/labels-%d", time.Now().UnixNano())
	base := time.Now().Add(-3 * time.Hour).UTC()

	mk := func(id, ticket, workflow string, status Status, labels map[string]string, cost float64, startedOffset time.Duration) *Run {
		r := &Run{
			ID: id, Repo: repo, Ticket: ticket, Workflow: workflow, Provider: "aws-ecs",
			InstanceID: "arn:aws:ecs:test:" + id, Status: status, LaunchedBy: "e2e",
			StartedAt: base.Add(startedOffset), TimeoutAt: base.Add(24 * time.Hour),
			Labels: labels, TotalCostUSD: &cost,
		}
		return r
	}
	runs := []*Run{
		mk("e2elbl0000a1", "KS-1", "implement-ticket", StatusRunning, map[string]string{"epic": "KS-100", "variant": "v3"}, 1.00, 0),
		mk("e2elbl0000a2", "KS-2", "implement-ticket", StatusSuccess, map[string]string{"epic": "KS-100", "variant": "v2"}, 2.00, 30*time.Minute),
		mk("e2elbl0000a3", "KS-3", "qa-pr", StatusSuccess, map[string]string{"epic": "KS-200"}, 4.00, 60*time.Minute),
		mk("e2elbl0000a4", "KS-4", "qa-pr", StatusFailed, nil, 8.00, 90*time.Minute),
	}

	for _, r := range runs {
		if err := st.CreateRun(ctx, r); err != nil {
			t.Fatalf("CreateRun %s: %v", r.ID, err)
		}
	}
	t.Cleanup(func() {
		for _, r := range runs {
			_, _ = raw.DeleteItem(context.Background(), &dynamodb.DeleteItemInput{
				TableName: aws.String(table),
				Key:       map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: r.ID}},
			})
		}
	})

	idSet := func(rs []*Run) map[string]bool {
		m := map[string]bool{}
		for _, r := range rs {
			m[r.ID] = true
		}
		return m
	}

	// 1. Label round-trip on a real read.
	got, err := st.GetRun(ctx, "e2elbl0000a1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Labels["epic"] != "KS-100" || got.Labels["variant"] != "v3" {
		t.Fatalf("labels round-trip: got %v", got.Labels)
	}

	// 2. Single label filter via server-side FilterExpression.
	res, err := st.ListRuns(ctx, RunFilter{Repo: repo, Labels: map[string]string{"epic": "KS-100"}})
	if err != nil {
		t.Fatalf("ListRuns(label): %v", err)
	}
	if s := idSet(res); len(s) != 2 || !s["e2elbl0000a1"] || !s["e2elbl0000a2"] {
		t.Errorf("label filter ids = %v, want a1+a2", idsOf(res))
	}

	// 3. Multi-label AND.
	res, err = st.ListRuns(ctx, RunFilter{Repo: repo, Labels: map[string]string{"epic": "KS-100", "variant": "v3"}})
	if err != nil {
		t.Fatalf("ListRuns(multi-label): %v", err)
	}
	if len(res) != 1 || res[0].ID != "e2elbl0000a1" {
		t.Errorf("multi-label ids = %v, want [a1]", idsOf(res))
	}

	// 4. Status set.
	res, err = st.ListRuns(ctx, RunFilter{Repo: repo, Statuses: []Status{StatusSuccess}})
	if err != nil {
		t.Fatalf("ListRuns(status): %v", err)
	}
	if s := idSet(res); len(s) != 2 || !s["e2elbl0000a2"] || !s["e2elbl0000a3"] {
		t.Errorf("status filter ids = %v, want a2+a3", idsOf(res))
	}

	// 5. Workflow + ticket.
	res, err = st.ListRuns(ctx, RunFilter{Repo: repo, Workflow: "qa-pr", Ticket: "KS-4"})
	if err != nil {
		t.Fatalf("ListRuns(workflow+ticket): %v", err)
	}
	if len(res) != 1 || res[0].ID != "e2elbl0000a4" {
		t.Errorf("workflow+ticket ids = %v, want [a4]", idsOf(res))
	}

	// 6. Time range on the started_at sort key (key condition).
	since := base.Add(20 * time.Minute)
	until := base.Add(70 * time.Minute)
	res, err = st.ListRuns(ctx, RunFilter{Repo: repo, Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("ListRuns(time range): %v", err)
	}
	if s := idSet(res); len(s) != 2 || !s["e2elbl0000a2"] || !s["e2elbl0000a3"] {
		t.Errorf("time-range ids = %v, want a2+a3", idsOf(res))
	}

	// 7. Combined filter (status + workflow + label).
	res, err = st.ListRuns(ctx, RunFilter{
		Repo: repo, Statuses: []Status{StatusSuccess}, Workflow: "implement-ticket",
		Labels: map[string]string{"epic": "KS-100"},
	})
	if err != nil {
		t.Fatalf("ListRuns(combined): %v", err)
	}
	if len(res) != 1 || res[0].ID != "e2elbl0000a2" {
		t.Errorf("combined ids = %v, want [a2]", idsOf(res))
	}

	// 8. Newest-first ordering across the full repo set.
	res, err = st.ListRuns(ctx, RunFilter{Repo: repo})
	if err != nil {
		t.Fatalf("ListRuns(all): %v", err)
	}
	if len(res) != 4 {
		t.Fatalf("all: got %d rows, want 4", len(res))
	}
	if res[0].ID != "e2elbl0000a4" || res[3].ID != "e2elbl0000a1" {
		t.Errorf("ordering = %v, want newest (a4) first, oldest (a1) last", idsOf(res))
	}

	t.Logf("live DynamoDB label/filter e2e passed against table %q (repo %q)", table, repo)
}

func idsOf(rs []*Run) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
