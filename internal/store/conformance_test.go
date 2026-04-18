package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// functionalDynamo is an in-memory implementation of dynamoAPI used in conformance tests.
type functionalDynamo struct {
	mu    sync.Mutex
	items []map[string]types.AttributeValue
}

func getS(item map[string]types.AttributeValue, key string) string {
	if v, ok := item[key].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func copyItem(item map[string]types.AttributeValue) map[string]types.AttributeValue {
	out := make(map[string]types.AttributeValue, len(item))
	for k, v := range item {
		out[k] = v
	}
	return out
}

func (f *functionalDynamo) DescribeTable(_ context.Context, _ *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, nil
}

func (f *functionalDynamo) PutItem(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if params.ConditionExpression != nil && *params.ConditionExpression == "attribute_not_exists(id)" {
		id := getS(params.Item, "id")
		for _, item := range f.items {
			if getS(item, "id") == id {
				return nil, &types.ConditionalCheckFailedException{}
			}
		}
	}
	f.items = append(f.items, copyItem(params.Item))
	return &dynamodb.PutItemOutput{}, nil
}

func (f *functionalDynamo) GetItem(_ context.Context, params *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	targetID := getS(params.Key, "id")
	for _, item := range f.items {
		if getS(item, "id") == targetID {
			return &dynamodb.GetItemOutput{Item: copyItem(item)}, nil
		}
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (f *functionalDynamo) UpdateItem(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	targetID := getS(params.Key, "id")

	idx := -1
	for i, item := range f.items {
		if getS(item, "id") == targetID {
			idx = i
			break
		}
	}

	if params.ConditionExpression != nil && *params.ConditionExpression == "attribute_exists(id)" && idx == -1 {
		return nil, &types.ConditionalCheckFailedException{}
	}
	if idx == -1 || params.UpdateExpression == nil {
		return &dynamodb.UpdateItemOutput{}, nil
	}

	expr := strings.TrimPrefix(*params.UpdateExpression, "SET ")
	for _, clause := range strings.Split(expr, ", ") {
		parts := strings.SplitN(clause, " = ", 2)
		if len(parts) != 2 {
			continue
		}
		attrName := parts[0]
		placeholder := parts[1]

		if strings.HasPrefix(attrName, "#") {
			if resolved, ok := params.ExpressionAttributeNames[attrName]; ok {
				attrName = resolved
			}
		}
		if val, ok := params.ExpressionAttributeValues[placeholder]; ok {
			f.items[idx][attrName] = val
		}
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func (f *functionalDynamo) Query(_ context.Context, params *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Determine partition key attribute from index name.
	var partitionKeyAttr string
	if params.IndexName != nil {
		switch *params.IndexName {
		case GSIByRepo:
			partitionKeyAttr = "repo"
		case GSIByTicket:
			partitionKeyAttr = "ticket"
		case GSIByStatus:
			partitionKeyAttr = "status"
		}
	}

	// Extract partition key value from KeyConditionExpression.
	var partitionVal string
	if params.KeyConditionExpression != nil {
		parts := strings.SplitN(*params.KeyConditionExpression, " = ", 2)
		if len(parts) == 2 {
			placeholder := strings.TrimSpace(parts[1])
			if sv, ok := params.ExpressionAttributeValues[placeholder].(*types.AttributeValueMemberS); ok {
				partitionVal = sv.Value
			}
		}
	}

	// Filter by partition key.
	var matching []map[string]types.AttributeValue
	for _, item := range f.items {
		if getS(item, partitionKeyAttr) == partitionVal {
			matching = append(matching, copyItem(item))
		}
	}

	// Apply FilterExpression.
	if params.FilterExpression != nil {
		filterExpr := *params.FilterExpression
		if strings.Contains(filterExpr, "IN (:pending, :running)") {
			pendingVal := ""
			runningVal := ""
			if sv, ok := params.ExpressionAttributeValues[":pending"].(*types.AttributeValueMemberS); ok {
				pendingVal = sv.Value
			}
			if sv, ok := params.ExpressionAttributeValues[":running"].(*types.AttributeValueMemberS); ok {
				runningVal = sv.Value
			}

			repoVal := ""
			hasRepoFilter := strings.Contains(filterExpr, "#repo = :repo")
			if hasRepoFilter {
				if sv, ok := params.ExpressionAttributeValues[":repo"].(*types.AttributeValueMemberS); ok {
					repoVal = sv.Value
				}
			}

			var filtered []map[string]types.AttributeValue
			for _, item := range matching {
				st := getS(item, "status")
				if st != pendingVal && st != runningVal {
					continue
				}
				if hasRepoFilter && getS(item, "repo") != repoVal {
					continue
				}
				filtered = append(filtered, item)
			}
			matching = filtered
		}
	}

	// Sort by started_at. Match real DynamoDB: ascending by default, descending
	// only when ScanIndexForward is explicitly false.
	descending := params.ScanIndexForward != nil && !*params.ScanIndexForward
	sort.Slice(matching, func(i, j int) bool {
		a := getS(matching[i], "started_at")
		b := getS(matching[j], "started_at")
		if descending {
			return a > b
		}
		return a < b
	})

	if params.Select == types.SelectCount {
		return &dynamodb.QueryOutput{Count: int32(len(matching))}, nil
	}
	return &dynamodb.QueryOutput{Items: matching}, nil
}

// conformanceRun builds a Run with all required fields populated.
func conformanceRun(id, repo, ticket string, status Status) *Run {
	now := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	return &Run{
		ID:         id,
		Repo:       repo,
		Ticket:     ticket,
		Branch:     "main",
		Workflow:   "default",
		Provider:   "docker",
		InstanceID: "container-" + id,
		Status:     status,
		LaunchedBy: "testuser",
		StartedAt:  now,
		TimeoutAt:  now.Add(60 * time.Minute),
	}
}

// RunStoreConformance runs the shared conformance suite against any Store implementation.
func RunStoreConformance(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("CreateGetRun/AllFields", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		now := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		run.ExitCode = ptr(0)
		run.CompletedAt = ptr(now.Add(5 * time.Minute))
		run.TotalCostUSD = ptr(1.23)
		run.Metadata = map[string]string{"key": "val", "k2": "v2"}

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.ID != run.ID {
			t.Errorf("ID: got %q, want %q", got.ID, run.ID)
		}
		if got.Repo != run.Repo {
			t.Errorf("Repo: got %q, want %q", got.Repo, run.Repo)
		}
		if got.Ticket != run.Ticket {
			t.Errorf("Ticket: got %q, want %q", got.Ticket, run.Ticket)
		}
		if got.Branch != run.Branch {
			t.Errorf("Branch: got %q, want %q", got.Branch, run.Branch)
		}
		if got.Workflow != run.Workflow {
			t.Errorf("Workflow: got %q, want %q", got.Workflow, run.Workflow)
		}
		if got.Provider != run.Provider {
			t.Errorf("Provider: got %q, want %q", got.Provider, run.Provider)
		}
		if got.InstanceID != run.InstanceID {
			t.Errorf("InstanceID: got %q, want %q", got.InstanceID, run.InstanceID)
		}
		if got.Status != run.Status {
			t.Errorf("Status: got %q, want %q", got.Status, run.Status)
		}
		if got.LaunchedBy != run.LaunchedBy {
			t.Errorf("LaunchedBy: got %q, want %q", got.LaunchedBy, run.LaunchedBy)
		}
		if !got.StartedAt.Equal(run.StartedAt) {
			t.Errorf("StartedAt: got %v, want %v", got.StartedAt, run.StartedAt)
		}
		if !got.TimeoutAt.Equal(run.TimeoutAt) {
			t.Errorf("TimeoutAt: got %v, want %v", got.TimeoutAt, run.TimeoutAt)
		}
		if got.ExitCode == nil || *got.ExitCode != *run.ExitCode {
			t.Errorf("ExitCode: got %v, want %v", got.ExitCode, run.ExitCode)
		}
		if got.CompletedAt == nil || !got.CompletedAt.Equal(*run.CompletedAt) {
			t.Errorf("CompletedAt: got %v, want %v", got.CompletedAt, run.CompletedAt)
		}
		if got.TotalCostUSD == nil || *got.TotalCostUSD != *run.TotalCostUSD {
			t.Errorf("TotalCostUSD: got %v, want %v", got.TotalCostUSD, run.TotalCostUSD)
		}
		if len(got.Metadata) != 2 {
			t.Errorf("Metadata len: got %d, want 2", len(got.Metadata))
		}
		if got.Metadata["key"] != "val" {
			t.Errorf("Metadata[key]: got %q, want %q", got.Metadata["key"], "val")
		}
		if got.Metadata["k2"] != "v2" {
			t.Errorf("Metadata[k2]: got %q, want %q", got.Metadata["k2"], "v2")
		}
	})

	t.Run("CreateGetRun/NilOptionalFields", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.ExitCode != nil {
			t.Errorf("ExitCode: expected nil, got %v", got.ExitCode)
		}
		if got.CompletedAt != nil {
			t.Errorf("CompletedAt: expected nil, got %v", got.CompletedAt)
		}
		if got.TotalCostUSD != nil {
			t.Errorf("TotalCostUSD: expected nil, got %v", got.TotalCostUSD)
		}
		if got.Metadata != nil {
			t.Errorf("Metadata: expected nil, got %v", got.Metadata)
		}
	})

	t.Run("CreateGetRun/EmptyMetadata", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		run.Metadata = map[string]string{}

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Metadata == nil {
			t.Error("Metadata: expected non-nil empty map, got nil")
		}
		if len(got.Metadata) != 0 {
			t.Errorf("Metadata len: got %d, want 0", len(got.Metadata))
		}
	})

	t.Run("CreateGetRun/UTCNormalization", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		est := time.FixedZone("EST", -5*3600)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		run.StartedAt = time.Date(2026, 4, 15, 5, 0, 0, 0, est).Truncate(time.Second)
		run.TimeoutAt = time.Date(2026, 4, 15, 6, 0, 0, 0, est).Truncate(time.Second)

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.StartedAt.Location() != time.UTC {
			t.Errorf("StartedAt.Location(): got %v, want UTC", got.StartedAt.Location())
		}
		if !got.StartedAt.Equal(run.StartedAt) {
			t.Errorf("StartedAt: got %v, want %v", got.StartedAt, run.StartedAt)
		}
		if got.TimeoutAt.Location() != time.UTC {
			t.Errorf("TimeoutAt.Location(): got %v, want UTC", got.TimeoutAt.Location())
		}
		if !got.TimeoutAt.Equal(run.TimeoutAt) {
			t.Errorf("TimeoutAt: got %v, want %v", got.TimeoutAt, run.TimeoutAt)
		}
	})

	t.Run("GetRun/NotFound", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		_, err := s.GetRun(ctx, "nonexistent-id")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, ErrRunNotFound) {
			t.Errorf("expected ErrRunNotFound, got: %v", err)
		}
		if !strings.Contains(err.Error(), "nonexistent-id") {
			t.Errorf("error %q does not contain %q", err.Error(), "nonexistent-id")
		}
	})

	t.Run("UpdateRun/SingleField", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if err := s.UpdateRun(ctx, "r1", &RunUpdate{Status: ptr(StatusRunning)}); err != nil {
			t.Fatalf("UpdateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Status != StatusRunning {
			t.Errorf("Status: got %q, want %q", got.Status, StatusRunning)
		}
		if got.InstanceID != run.InstanceID {
			t.Errorf("InstanceID unchanged: got %q, want %q", got.InstanceID, run.InstanceID)
		}
		if got.ExitCode != nil {
			t.Errorf("ExitCode: expected nil, got %v", got.ExitCode)
		}
		if got.CompletedAt != nil {
			t.Errorf("CompletedAt: expected nil, got %v", got.CompletedAt)
		}
		if got.TotalCostUSD != nil {
			t.Errorf("TotalCostUSD: expected nil, got %v", got.TotalCostUSD)
		}
		if got.Metadata != nil {
			t.Errorf("Metadata: expected nil, got %v", got.Metadata)
		}
	})

	t.Run("UpdateRun/MultipleFields", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		completedAt := time.Date(2026, 4, 15, 10, 5, 0, 0, time.UTC)
		update := &RunUpdate{
			Status:       ptr(StatusSuccess),
			InstanceID:   ptr("new-container"),
			ExitCode:     ptr(42),
			CompletedAt:  ptr(completedAt),
			TotalCostUSD: ptr(9.99),
			Metadata:     map[string]string{"updated": "true"},
		}
		if err := s.UpdateRun(ctx, "r1", update); err != nil {
			t.Fatalf("UpdateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Status != StatusSuccess {
			t.Errorf("Status: got %q, want %q", got.Status, StatusSuccess)
		}
		if got.InstanceID != "new-container" {
			t.Errorf("InstanceID: got %q, want %q", got.InstanceID, "new-container")
		}
		if got.ExitCode == nil || *got.ExitCode != 42 {
			t.Errorf("ExitCode: got %v, want 42", got.ExitCode)
		}
		if got.CompletedAt == nil || !got.CompletedAt.Equal(completedAt) {
			t.Errorf("CompletedAt: got %v, want %v", got.CompletedAt, completedAt)
		}
		if got.TotalCostUSD == nil || *got.TotalCostUSD != 9.99 {
			t.Errorf("TotalCostUSD: got %v, want 9.99", got.TotalCostUSD)
		}
		if got.Metadata["updated"] != "true" {
			t.Errorf("Metadata[updated]: got %q, want %q", got.Metadata["updated"], "true")
		}
	})

	t.Run("UpdateRun/NotFound", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		err := s.UpdateRun(ctx, "no-such-id", &RunUpdate{Status: ptr(StatusRunning)})
		if !errors.Is(err, ErrRunNotFound) {
			t.Errorf("expected ErrRunNotFound, got: %v", err)
		}
		if !strings.Contains(err.Error(), "no-such-id") {
			t.Errorf("error %q does not contain %q", err.Error(), "no-such-id")
		}
	})

	t.Run("UpdateRun/NoFieldsSet", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if err := s.UpdateRun(ctx, "r1", &RunUpdate{}); err != nil {
			t.Fatalf("UpdateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Status != run.Status {
			t.Errorf("Status unchanged: got %q, want %q", got.Status, run.Status)
		}
		if got.InstanceID != run.InstanceID {
			t.Errorf("InstanceID unchanged: got %q, want %q", got.InstanceID, run.InstanceID)
		}
	})

	t.Run("UpdateRun/NoFieldsSetNotFound", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		err := s.UpdateRun(ctx, "nonexistent", &RunUpdate{})
		if !errors.Is(err, ErrRunNotFound) {
			t.Errorf("expected ErrRunNotFound, got: %v", err)
		}
	})

	t.Run("UpdateRun/FieldIsolation", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		run.InstanceID = "original-instance"
		run.Metadata = map[string]string{"persist": "yes"}

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if err := s.UpdateRun(ctx, "r1", &RunUpdate{Status: ptr(StatusRunning)}); err != nil {
			t.Fatalf("UpdateRun status: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.InstanceID != "original-instance" {
			t.Errorf("InstanceID: got %q, want %q", got.InstanceID, "original-instance")
		}
		if got.Metadata["persist"] != "yes" {
			t.Errorf("Metadata[persist]: got %q, want %q", got.Metadata["persist"], "yes")
		}

		if err := s.UpdateRun(ctx, "r1", &RunUpdate{ExitCode: ptr(0), TotalCostUSD: ptr(1.5)}); err != nil {
			t.Fatalf("UpdateRun exitcode+cost: %v", err)
		}
		got, err = s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Status != StatusRunning {
			t.Errorf("Status: got %q, want %q", got.Status, StatusRunning)
		}
		if got.InstanceID != "original-instance" {
			t.Errorf("InstanceID: got %q, want %q", got.InstanceID, "original-instance")
		}
		if got.Metadata["persist"] != "yes" {
			t.Errorf("Metadata[persist]: got %q, want %q", got.Metadata["persist"], "yes")
		}
	})

	t.Run("UpdateRun/MetadataOverwrite", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		run.Metadata = map[string]string{"a": "1"}

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if err := s.UpdateRun(ctx, "r1", &RunUpdate{Metadata: map[string]string{"b": "2"}}); err != nil {
			t.Fatalf("UpdateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Metadata["b"] != "2" {
			t.Errorf("Metadata[b]: got %q, want %q", got.Metadata["b"], "2")
		}
		if _, ok := got.Metadata["a"]; ok {
			t.Error("Metadata should not contain key 'a' after overwrite")
		}

		if err := s.UpdateRun(ctx, "r1", &RunUpdate{Metadata: map[string]string{}}); err != nil {
			t.Fatalf("UpdateRun empty metadata: %v", err)
		}
		got, err = s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Metadata == nil {
			t.Error("Metadata: expected non-nil empty map after empty overwrite, got nil")
		}
		if len(got.Metadata) != 0 {
			t.Errorf("Metadata len: got %d, want 0", len(got.Metadata))
		}
	})

	t.Run("UpdateRun/UTCNormalization", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		est := time.FixedZone("EST", -5*3600)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusRunning)

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		someTime := time.Date(2026, 4, 15, 5, 30, 0, 0, est)
		if err := s.UpdateRun(ctx, "r1", &RunUpdate{CompletedAt: ptr(someTime)}); err != nil {
			t.Fatalf("UpdateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.CompletedAt == nil {
			t.Fatal("CompletedAt: expected non-nil")
		}
		if got.CompletedAt.Location() != time.UTC {
			t.Errorf("CompletedAt.Location(): got %v, want UTC", got.CompletedAt.Location())
		}
		if !got.CompletedAt.Equal(someTime) {
			t.Errorf("CompletedAt: got %v, want %v", got.CompletedAt, someTime)
		}
	})

	t.Run("ListByRepo/FiltersAndOrder", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/listrepo"

		run1 := conformanceRun("run-1", repo, "PROJ-1", StatusPending)
		run1.StartedAt = time.Date(2026, 4, 15, 9, 58, 0, 0, time.UTC)
		run1.TimeoutAt = run1.StartedAt.Add(60 * time.Minute)

		run2 := conformanceRun("run-2", repo, "PROJ-2", StatusPending)
		// run2 StartedAt stays at 2026-04-15 10:00:00 UTC from conformanceRun (later than run1)

		run3 := conformanceRun("run-3", "github.com/other/repo", "PROJ-3", StatusPending)

		for _, r := range []*Run{run1, run2, run3} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun %s: %v", r.ID, err)
			}
		}

		results, err := s.ListByRepo(ctx, repo, false)
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("len: got %d, want 2", len(results))
		}
		if results[0].ID != "run-2" {
			t.Errorf("results[0].ID: got %q, want %q", results[0].ID, "run-2")
		}
		if results[1].ID != "run-1" {
			t.Errorf("results[1].ID: got %q, want %q", results[1].ID, "run-1")
		}
	})

	t.Run("ListByRepo/ActiveOnly", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/activerepo"

		pend := conformanceRun("run-pend", repo, "PROJ-1", StatusPending)
		pend.StartedAt = time.Date(2026, 4, 15, 9, 58, 0, 0, time.UTC)
		pend.TimeoutAt = pend.StartedAt.Add(60 * time.Minute)

		running := conformanceRun("run-run", repo, "PROJ-2", StatusRunning)
		running.StartedAt = time.Date(2026, 4, 15, 9, 59, 0, 0, time.UTC)
		running.TimeoutAt = running.StartedAt.Add(60 * time.Minute)

		done := conformanceRun("run-done", repo, "PROJ-3", StatusSuccess)
		failed := conformanceRun("run-failed", repo, "PROJ-4", StatusFailed)
		killed := conformanceRun("run-killed", repo, "PROJ-5", StatusKilled)

		for _, r := range []*Run{pend, running, done, failed, killed} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun %s: %v", r.ID, err)
			}
		}

		active, err := s.ListByRepo(ctx, repo, true)
		if err != nil {
			t.Fatalf("ListByRepo active: %v", err)
		}
		if len(active) != 2 {
			t.Fatalf("active len: got %d, want 2", len(active))
		}
		for _, r := range active {
			if r.Status != StatusPending && r.Status != StatusRunning {
				t.Errorf("unexpected status %q in active results", r.Status)
			}
		}

		all, err := s.ListByRepo(ctx, repo, false)
		if err != nil {
			t.Fatalf("ListByRepo all: %v", err)
		}
		if len(all) != 5 {
			t.Fatalf("all len: got %d, want 5", len(all))
		}
	})

	t.Run("ListByRepo/EmptyResult", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		results, err := s.ListByRepo(ctx, "no-such-repo", false)
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		if results == nil {
			t.Error("expected non-nil slice, got nil")
		}
		if len(results) != 0 {
			t.Errorf("len: got %d, want 0", len(results))
		}
	})

	t.Run("FindActiveByTicket/Match", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/ticketrepo"
		ticket := "PROJ-42"

		run1 := conformanceRun("run-1", repo, ticket, StatusPending)    // active, matching, newest
		run1Older := conformanceRun("run-1-older", repo, ticket, StatusRunning)
		run1Older.StartedAt = run1.StartedAt.Add(-1 * time.Hour)
		run2 := conformanceRun("run-2", repo, ticket, StatusSuccess)    // inactive
		run3 := conformanceRun("run-3", repo, "PROJ-99", StatusRunning) // different ticket

		for _, r := range []*Run{run1, run1Older, run2, run3} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun %s: %v", r.ID, err)
			}
		}

		results, err := s.FindActiveByTicket(ctx, repo, ticket)
		if err != nil {
			t.Fatalf("FindActiveByTicket: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("len: got %d, want 2", len(results))
		}
		if results[0].ID != "run-1" {
			t.Errorf("results[0].ID: got %q, want %q (newest first)", results[0].ID, "run-1")
		}
		if results[1].ID != "run-1-older" {
			t.Errorf("results[1].ID: got %q, want %q", results[1].ID, "run-1-older")
		}
	})

	t.Run("FindActiveByTicket/NoMatch", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/ticketrepo"
		ticket := "PROJ-42"

		run := conformanceRun("run-1", repo, ticket, StatusSuccess)
		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		results, err := s.FindActiveByTicket(ctx, repo, ticket)
		if err != nil {
			t.Fatalf("FindActiveByTicket: %v", err)
		}
		if results == nil {
			t.Error("expected non-nil slice, got nil")
		}
		if len(results) != 0 {
			t.Errorf("len: got %d, want 0", len(results))
		}
	})

	t.Run("FindActiveByTicket/RepoMismatch", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		ticket := "PROJ-42"

		run := conformanceRun("run-1", "github.com/other/repo", ticket, StatusPending)
		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		results, err := s.FindActiveByTicket(ctx, "github.com/org/repo", ticket)
		if err != nil {
			t.Fatalf("FindActiveByTicket: %v", err)
		}
		if results == nil {
			t.Error("expected non-nil slice, got nil")
		}
		if len(results) != 0 {
			t.Errorf("len: got %d, want 0", len(results))
		}
	})

	t.Run("CountActive/Empty", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		count, err := s.CountActive(ctx)
		if err != nil {
			t.Fatalf("CountActive: %v", err)
		}
		if count != 0 {
			t.Errorf("count: got %d, want 0", count)
		}
	})

	t.Run("CountActive/MixedStatuses", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/countrepo"
		statuses := []Status{StatusPending, StatusRunning, StatusSuccess, StatusFailed, StatusKilled}
		for i, st := range statuses {
			run := conformanceRun(fmt.Sprintf("run-%d", i), repo, "PROJ-1", st)
			run.StartedAt = time.Date(2026, 4, 15, 10, i, 0, 0, time.UTC)
			run.TimeoutAt = run.StartedAt.Add(60 * time.Minute)
			if err := s.CreateRun(ctx, run); err != nil {
				t.Fatalf("CreateRun run-%d: %v", i, err)
			}
		}
		count, err := s.CountActive(ctx)
		if err != nil {
			t.Fatalf("CountActive: %v", err)
		}
		if count != 2 {
			t.Errorf("count: got %d, want 2", count)
		}
	})

	t.Run("CountActive/CrossRepo", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run1 := conformanceRun("run-1", "github.com/org/repo1", "PROJ-1", StatusPending)
		run2 := conformanceRun("run-2", "github.com/org/repo2", "PROJ-2", StatusPending)
		run2.StartedAt = run1.StartedAt.Add(time.Minute)
		run2.TimeoutAt = run2.StartedAt.Add(60 * time.Minute)

		for _, r := range []*Run{run1, run2} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun %s: %v", r.ID, err)
			}
		}
		count, err := s.CountActive(ctx)
		if err != nil {
			t.Fatalf("CountActive: %v", err)
		}
		if count != 2 {
			t.Errorf("count: got %d, want 2", count)
		}
	})

	t.Run("ListActive/Empty", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		runs, err := s.ListActive(ctx)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		if len(runs) != 0 {
			t.Errorf("len: got %d, want 0", len(runs))
		}
	})

	t.Run("ListActive/MixedStatuses", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/listrepo"
		statuses := []Status{StatusPending, StatusRunning, StatusSuccess, StatusFailed, StatusKilled}
		for i, st := range statuses {
			run := conformanceRun(fmt.Sprintf("run-%d", i), repo, "PROJ-1", st)
			run.StartedAt = time.Date(2026, 4, 15, 10, i, 0, 0, time.UTC)
			run.TimeoutAt = run.StartedAt.Add(60 * time.Minute)
			if err := s.CreateRun(ctx, run); err != nil {
				t.Fatalf("CreateRun run-%d: %v", i, err)
			}
		}
		runs, err := s.ListActive(ctx)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		if len(runs) != 2 {
			t.Errorf("len: got %d, want 2", len(runs))
		}
		for _, r := range runs {
			if r.ID == "" {
				t.Error("run has empty ID")
			}
			if r.Ticket == "" {
				t.Error("run has empty Ticket")
			}
		}
	})

	t.Run("ListActive/CrossRepo", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run1 := conformanceRun("run-1", "github.com/org/repo1", "PROJ-1", StatusPending)
		run2 := conformanceRun("run-2", "github.com/org/repo2", "PROJ-2", StatusPending)
		run2.StartedAt = run1.StartedAt.Add(time.Minute)
		run2.TimeoutAt = run2.StartedAt.Add(60 * time.Minute)

		for _, r := range []*Run{run1, run2} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun %s: %v", r.ID, err)
			}
		}
		runs, err := s.ListActive(ctx)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		if len(runs) != 2 {
			t.Errorf("len: got %d, want 2", len(runs))
		}
	})

	t.Run("MetadataRoundTrip/Nil", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		// Metadata is nil by default from conformanceRun.

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Metadata != nil {
			t.Errorf("Metadata: expected nil, got %v", got.Metadata)
		}
	})

	t.Run("MetadataRoundTrip/Empty", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		run.Metadata = map[string]string{}

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Metadata == nil {
			t.Error("Metadata: expected non-nil empty map, got nil")
		}
		if len(got.Metadata) != 0 {
			t.Errorf("Metadata len: got %d, want 0", len(got.Metadata))
		}
	})

	t.Run("MetadataRoundTrip/Populated", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		run.Metadata = map[string]string{
			"cluster_arn": "arn:aws:ecs:us-east-1:123:cluster/horde",
			"log_group":   "/ecs/horde-worker",
		}

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if len(got.Metadata) != 2 {
			t.Fatalf("Metadata len: got %d, want 2", len(got.Metadata))
		}
		if got.Metadata["cluster_arn"] != "arn:aws:ecs:us-east-1:123:cluster/horde" {
			t.Errorf("Metadata[cluster_arn]: got %q", got.Metadata["cluster_arn"])
		}
		if got.Metadata["log_group"] != "/ecs/horde-worker" {
			t.Errorf("Metadata[log_group]: got %q", got.Metadata["log_group"])
		}
	})
}

func TestSQLiteStore_Conformance(t *testing.T) {
	t.Parallel()
	RunStoreConformance(t, func(t *testing.T) Store {
		dbPath := filepath.Join(t.TempDir(), "horde.db")
		s, err := NewSQLiteStore(dbPath)
		if err != nil {
			t.Fatalf("NewSQLiteStore: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

// TestFunctionalDynamo_ScanIndexForwardDefault pins the fake's sort behavior
// to match real DynamoDB: unset ScanIndexForward means ascending. A wrong
// default here would let future query methods pass tests while producing
// backwards-ordered results in production.
func TestFunctionalDynamo_ScanIndexForwardDefault(t *testing.T) {
	t.Parallel()
	f := &functionalDynamo{}
	for _, started := range []string{"2026-01-03T00:00:00Z", "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"} {
		f.items = append(f.items, map[string]types.AttributeValue{
			"id":         &types.AttributeValueMemberS{Value: started},
			"repo":       &types.AttributeValueMemberS{Value: "r"},
			"started_at": &types.AttributeValueMemberS{Value: started},
		})
	}
	indexName := GSIByRepo
	keyExpr := "repo = :r"
	cases := []struct {
		name    string
		forward *bool
		want    []string
	}{
		{"nil defaults to ascending", nil, []string{"2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z", "2026-01-03T00:00:00Z"}},
		{"explicit true ascends", boolPtr(true), []string{"2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z", "2026-01-03T00:00:00Z"}},
		{"explicit false descends", boolPtr(false), []string{"2026-01-03T00:00:00Z", "2026-01-02T00:00:00Z", "2026-01-01T00:00:00Z"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := f.Query(context.Background(), &dynamodb.QueryInput{
				IndexName:              &indexName,
				KeyConditionExpression: &keyExpr,
				ExpressionAttributeValues: map[string]types.AttributeValue{
					":r": &types.AttributeValueMemberS{Value: "r"},
				},
				ScanIndexForward: tc.forward,
			})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			var got []string
			for _, it := range out.Items {
				got = append(got, getS(it, "started_at"))
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestDynamoStore_Conformance(t *testing.T) {
	t.Parallel()
	RunStoreConformance(t, func(t *testing.T) Store {
		return &DynamoStore{client: &functionalDynamo{}, tableName: "conformance-test"}
	})
}
