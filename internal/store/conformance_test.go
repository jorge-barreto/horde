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
		if m, ok := v.(*types.AttributeValueMemberM); ok {
			inner := make(map[string]types.AttributeValue, len(m.Value))
			for ik, iv := range m.Value {
				inner[ik] = iv
			}
			out[k] = &types.AttributeValueMemberM{Value: inner}
			continue
		}
		out[k] = v
	}
	return out
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

	// Parse the KeyConditionExpression: partition equality plus an optional
	// started_at range (>=, <=, or BETWEEN) on the sort key. This mirrors the
	// real GSI key-condition surface the store uses.
	var partitionVal, sinceVal, untilVal string
	if params.KeyConditionExpression != nil {
		keyExpr := *params.KeyConditionExpression
		resolve := func(ph string) string {
			if sv, ok := params.ExpressionAttributeValues[strings.TrimSpace(ph)].(*types.AttributeValueMemberS); ok {
				return sv.Value
			}
			return ""
		}
		// Pull out a BETWEEN range first so its internal " AND " is not split
		// as a clause separator. "#started BETWEEN :since AND :until".
		if idx := strings.Index(keyExpr, "BETWEEN"); idx != -1 {
			rest := keyExpr[idx+len("BETWEEN"):]
			bounds := strings.SplitN(rest, " AND ", 2)
			if len(bounds) == 2 {
				sinceVal = resolve(bounds[0])
				untilVal = resolve(bounds[1])
			}
			keyExpr = keyExpr[:idx] // leave the partition (and a dangling "AND #started")
		}
		for _, clause := range strings.Split(keyExpr, " AND ") {
			clause = strings.TrimSpace(clause)
			switch {
			case clause == "" || strings.HasSuffix(clause, "#started"):
				// dangling "#started" left by trimming a BETWEEN; ignore.
			case strings.Contains(clause, ">="):
				sinceVal = resolve(clause[strings.Index(clause, ">=")+2:])
			case strings.Contains(clause, "<="):
				untilVal = resolve(clause[strings.Index(clause, "<=")+2:])
			case strings.Contains(clause, " = "):
				parts := strings.SplitN(clause, " = ", 2)
				partitionVal = resolve(parts[1])
			}
		}
	}

	// Filter by partition key and any started_at range.
	var matching []map[string]types.AttributeValue
	for _, item := range f.items {
		if getS(item, partitionKeyAttr) != partitionVal {
			continue
		}
		started := getS(item, "started_at")
		if sinceVal != "" && started < sinceVal {
			continue
		}
		if untilVal != "" && started > untilVal {
			continue
		}
		matching = append(matching, copyItem(item))
	}

	// Apply FilterExpression generically: support the AND-combined predicates
	// the store emits — "#x = :y" equality (including dotted "#labels.#k = :v"
	// map access) and "#st IN (:s0, :s1, …)" set membership. This keeps the
	// fake honest against the real query path rather than recognizing one
	// hardcoded pattern.
	if params.FilterExpression != nil {
		resolveName := func(tok string) string {
			tok = strings.TrimSpace(tok)
			// Dotted map access: "#labels.#k0" -> labels key.
			if strings.Contains(tok, ".") {
				dot := strings.SplitN(tok, ".", 2)
				container := params.ExpressionAttributeNames[dot[0]]
				key := dot[1]
				if resolved, ok := params.ExpressionAttributeNames[key]; ok {
					key = resolved
				}
				return container + "." + key
			}
			if resolved, ok := params.ExpressionAttributeNames[tok]; ok {
				return resolved
			}
			return tok
		}
		resolveVal := func(ph string) string {
			if sv, ok := params.ExpressionAttributeValues[strings.TrimSpace(ph)].(*types.AttributeValueMemberS); ok {
				return sv.Value
			}
			return ""
		}
		// itemFieldValue resolves a (possibly dotted, map-access) attribute path
		// to the item's stored string value.
		itemFieldValue := func(item map[string]types.AttributeValue, path string) string {
			if !strings.Contains(path, ".") {
				return getS(item, path)
			}
			parts := strings.SplitN(path, ".", 2)
			mv, ok := item[parts[0]].(*types.AttributeValueMemberM)
			if !ok {
				return ""
			}
			if sv, ok := mv.Value[parts[1]].(*types.AttributeValueMemberS); ok {
				return sv.Value
			}
			return ""
		}

		var filtered []map[string]types.AttributeValue
		for _, item := range matching {
			ok := true
			for _, clause := range strings.Split(*params.FilterExpression, " AND ") {
				clause = strings.TrimSpace(clause)
				if idx := strings.Index(clause, " IN ("); idx != -1 {
					field := resolveName(clause[:idx])
					inner := clause[idx+len(" IN (") : strings.LastIndex(clause, ")")]
					matched := false
					for _, ph := range strings.Split(inner, ",") {
						if itemFieldValue(item, field) == resolveVal(ph) {
							matched = true
							break
						}
					}
					if !matched {
						ok = false
						break
					}
					continue
				}
				if parts := strings.SplitN(clause, " = ", 2); len(parts) == 2 {
					field := resolveName(parts[0])
					if itemFieldValue(item, field) != resolveVal(parts[1]) {
						ok = false
						break
					}
				}
			}
			if ok {
				filtered = append(filtered, item)
			}
		}
		matching = filtered
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

func runIDs(runs []*Run) []string {
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	return ids
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
		run.Tokens = &TokenUsage{
			InputTokens:         100,
			OutputTokens:        200,
			CacheCreationTokens: 300,
			CacheReadTokens:     400,
			Turns:               5,
		}
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
		if got.Tokens == nil {
			t.Fatalf("Tokens: got nil, want %+v", run.Tokens)
		}
		if *got.Tokens != *run.Tokens {
			t.Errorf("Tokens: got %+v, want %+v", *got.Tokens, *run.Tokens)
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
		if got.Tokens != nil {
			t.Errorf("Tokens: expected nil, got %+v", got.Tokens)
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

	t.Run("CreateRun/DuplicateID", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("dup-id", "github.com/org/repo", "PROJ-1", StatusPending)
		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("first CreateRun: %v", err)
		}
		dup := conformanceRun("dup-id", "github.com/org/other", "PROJ-2", StatusPending)
		err := s.CreateRun(ctx, dup)
		if err == nil {
			t.Fatal("second CreateRun: expected error, got nil")
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

	t.Run("CreateGetRun/RecoverableStatuses", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		// timed_out / rate_limited must round-trip through the store like any
		// other status and classify as terminal. They are written by the
		// status Lambda / Finalize for orc exit codes 2 and 4 respectively,
		// and `horde retry` keys off IsTerminal to accept them.
		for i, status := range []Status{StatusTimedOut, StatusRateLimited} {
			if !status.IsTerminal() {
				t.Errorf("%q must classify as terminal", status)
			}
			id := "rec" + string(rune('0'+i))
			run := conformanceRun(id, "github.com/org/repo", "PROJ-1", status)
			if err := s.CreateRun(ctx, run); err != nil {
				t.Fatalf("CreateRun(%s): %v", status, err)
			}
			got, err := s.GetRun(ctx, id)
			if err != nil {
				t.Fatalf("GetRun(%s): %v", status, err)
			}
			if got.Status != status {
				t.Errorf("Status: got %q, want %q", got.Status, status)
			}
		}
	})

	t.Run("CreateGetRun/CapacityResumeCount", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		r := conformanceRun("cap-1", "github.com/org/capr", "T-1", StatusRunning)
		r.Capacity = CapacityOnDemand
		r.ResumeCount = 2
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRun(ctx, "cap-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Capacity != CapacityOnDemand {
			t.Errorf("Capacity = %q, want on-demand", got.Capacity)
		}
		if got.ResumeCount != 2 {
			t.Errorf("ResumeCount = %d, want 2", got.ResumeCount)
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
		if got.Tokens != nil {
			t.Errorf("Tokens: expected nil, got %+v", got.Tokens)
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
		timeoutAt := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
		update := &RunUpdate{
			Status:       ptr(StatusSuccess),
			InstanceID:   ptr("new-container"),
			ExitCode:     ptr(42),
			CompletedAt:  ptr(completedAt),
			TotalCostUSD: ptr(9.99),
			Tokens: &TokenUsage{
				InputTokens:         11,
				OutputTokens:        22,
				CacheCreationTokens: 33,
				CacheReadTokens:     44,
				Turns:               2,
			},
			Metadata:  map[string]string{"updated": "true"},
			TimeoutAt: ptr(timeoutAt),
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
		if got.Tokens == nil {
			t.Fatalf("Tokens after update: got nil, want non-nil")
		}
		if got.Tokens.InputTokens != 11 || got.Tokens.OutputTokens != 22 ||
			got.Tokens.CacheCreationTokens != 33 || got.Tokens.CacheReadTokens != 44 ||
			got.Tokens.Turns != 2 {
			t.Errorf("Tokens after update: got %+v", *got.Tokens)
		}
		if got.Metadata["updated"] != "true" {
			t.Errorf("Metadata[updated]: got %q, want %q", got.Metadata["updated"], "true")
		}
		if !got.TimeoutAt.Equal(timeoutAt) {
			t.Errorf("TimeoutAt: got %v, want %v", got.TimeoutAt, timeoutAt)
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

		newTimeout := time.Date(2026, 4, 16, 2, 0, 0, 0, est)
		if err := s.UpdateRun(ctx, "r1", &RunUpdate{TimeoutAt: ptr(newTimeout)}); err != nil {
			t.Fatalf("UpdateRun TimeoutAt: %v", err)
		}
		got, err = s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.TimeoutAt.Location() != time.UTC {
			t.Errorf("TimeoutAt.Location(): got %v, want UTC", got.TimeoutAt.Location())
		}
		if !got.TimeoutAt.Equal(newTimeout) {
			t.Errorf("TimeoutAt: got %v, want %v", got.TimeoutAt, newTimeout)
		}
	})

	t.Run("UpdateRun/CapacityResumeCount", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		r := conformanceRun("cap-2", "github.com/org/capr2", "T-2", StatusQueued)
		r.Capacity = CapacitySpot
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		newCount := 1
		newCap := CapacityOnDemand
		if err := s.UpdateRun(ctx, "cap-2", &RunUpdate{ResumeCount: &newCount, Capacity: &newCap}); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRun(ctx, "cap-2")
		if err != nil {
			t.Fatal(err)
		}
		if got.ResumeCount != 1 {
			t.Errorf("ResumeCount = %d, want 1", got.ResumeCount)
		}
		if got.Capacity != CapacityOnDemand {
			t.Errorf("Capacity = %q, want on-demand", got.Capacity)
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

	t.Run("ListRuns/RepoScopeAndOrder", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/lr-scope"

		early := conformanceRun("lr-early", repo, "PROJ-1", StatusPending)
		early.StartedAt = time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC)
		early.TimeoutAt = early.StartedAt.Add(time.Hour)
		late := conformanceRun("lr-late", repo, "PROJ-2", StatusPending)
		late.StartedAt = time.Date(2026, 4, 15, 11, 0, 0, 0, time.UTC)
		late.TimeoutAt = late.StartedAt.Add(time.Hour)
		other := conformanceRun("lr-other", "github.com/org/different", "PROJ-3", StatusPending)

		for _, r := range []*Run{early, late, other} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun %s: %v", r.ID, err)
			}
		}

		got, err := s.ListRuns(ctx, RunFilter{Repo: repo})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		// Repo-scoped, newest first.
		if want := []string{"lr-late", "lr-early"}; !reflect.DeepEqual(runIDs(got), want) {
			t.Errorf("ids = %v, want %v", runIDs(got), want)
		}
	})

	t.Run("ListRuns/StatusSet", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/lr-status"
		for _, st := range []Status{StatusPending, StatusRunning, StatusSuccess, StatusFailed} {
			r := conformanceRun("lr-"+string(st), repo, "PROJ-1", st)
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
		}
		got, err := s.ListRuns(ctx, RunFilter{Repo: repo, Statuses: []Status{StatusPending, StatusRunning}})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		for _, r := range got {
			if r.Status != StatusPending && r.Status != StatusRunning {
				t.Errorf("unexpected status %q", r.Status)
			}
		}
	})

	t.Run("ListRuns/Workflow", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/lr-wf"
		impl := conformanceRun("lr-impl", repo, "PROJ-1", StatusSuccess)
		impl.Workflow = "implement-ticket"
		qa := conformanceRun("lr-qa", repo, "PROJ-2", StatusSuccess)
		qa.Workflow = "qa-pr"
		for _, r := range []*Run{impl, qa} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
		}
		got, err := s.ListRuns(ctx, RunFilter{Repo: repo, Workflow: "implement-ticket"})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 1 || got[0].ID != "lr-impl" {
			t.Fatalf("ids = %v, want [lr-impl]", runIDs(got))
		}
	})

	t.Run("ListRuns/Ticket", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/lr-ticket"
		a := conformanceRun("lr-a", repo, "KS-1", StatusSuccess)
		b := conformanceRun("lr-b", repo, "KS-2", StatusSuccess)
		for _, r := range []*Run{a, b} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
		}
		got, err := s.ListRuns(ctx, RunFilter{Repo: repo, Ticket: "KS-2"})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 1 || got[0].ID != "lr-b" {
			t.Fatalf("ids = %v, want [lr-b]", runIDs(got))
		}
	})

	t.Run("ListRuns/SingleLabel", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/lr-label1"
		match := conformanceRun("lr-match", repo, "PROJ-1", StatusSuccess)
		match.Labels = map[string]string{"epic": "KS-100"}
		nomatch := conformanceRun("lr-nomatch", repo, "PROJ-2", StatusSuccess)
		nomatch.Labels = map[string]string{"epic": "KS-200"}
		nolabel := conformanceRun("lr-nolabel", repo, "PROJ-3", StatusSuccess)
		for _, r := range []*Run{match, nomatch, nolabel} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
		}
		got, err := s.ListRuns(ctx, RunFilter{Repo: repo, Labels: map[string]string{"epic": "KS-100"}})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 1 || got[0].ID != "lr-match" {
			t.Fatalf("ids = %v, want [lr-match]", runIDs(got))
		}
	})

	t.Run("ListRuns/MultiLabelAND", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/lr-label2"
		both := conformanceRun("lr-both", repo, "PROJ-1", StatusSuccess)
		both.Labels = map[string]string{"epic": "KS-100", "variant": "v3"}
		onlyEpic := conformanceRun("lr-epic", repo, "PROJ-2", StatusSuccess)
		onlyEpic.Labels = map[string]string{"epic": "KS-100", "variant": "v2"}
		for _, r := range []*Run{both, onlyEpic} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
		}
		// AND: both keys must match.
		got, err := s.ListRuns(ctx, RunFilter{Repo: repo, Labels: map[string]string{"epic": "KS-100", "variant": "v3"}})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 1 || got[0].ID != "lr-both" {
			t.Fatalf("ids = %v, want [lr-both]", runIDs(got))
		}
	})

	t.Run("ListRuns/TimeRange", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/lr-time"
		mk := func(id string, h int) *Run {
			r := conformanceRun(id, repo, "PROJ-1", StatusSuccess)
			r.StartedAt = time.Date(2026, 4, 15, h, 0, 0, 0, time.UTC)
			r.TimeoutAt = r.StartedAt.Add(time.Hour)
			return r
		}
		for _, r := range []*Run{mk("lr-8", 8), mk("lr-10", 10), mk("lr-12", 12)} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
		}
		since := time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC)
		until := time.Date(2026, 4, 15, 11, 0, 0, 0, time.UTC)
		got, err := s.ListRuns(ctx, RunFilter{Repo: repo, Since: &since, Until: &until})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 1 || got[0].ID != "lr-10" {
			t.Fatalf("ids = %v, want [lr-10]", runIDs(got))
		}
	})

	// Since/Until are documented inclusive. A run whose StartedAt equals a bound
	// must be returned, and both stores must agree (SQLite filters in Go, DynamoDB
	// via BETWEEN/>=/<=). This pins inclusivity so flipping >= to > can't slip by.
	t.Run("ListRuns/TimeRangeBoundsInclusive", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/lr-bounds"
		mk := func(id string, h int) *Run {
			r := conformanceRun(id, repo, "PROJ-1", StatusSuccess)
			r.StartedAt = time.Date(2026, 4, 15, h, 0, 0, 0, time.UTC)
			r.TimeoutAt = r.StartedAt.Add(time.Hour)
			return r
		}
		for _, r := range []*Run{mk("lr-lo", 9), mk("lr-mid", 10), mk("lr-hi", 11), mk("lr-out", 12)} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
		}
		// Bounds land exactly on lr-lo (since) and lr-hi (until).
		since := time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC)
		until := time.Date(2026, 4, 15, 11, 0, 0, 0, time.UTC)
		got, err := s.ListRuns(ctx, RunFilter{Repo: repo, Since: &since, Until: &until})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		ids := map[string]bool{}
		for _, r := range got {
			ids[r.ID] = true
		}
		if len(got) != 3 || !ids["lr-lo"] || !ids["lr-mid"] || !ids["lr-hi"] {
			t.Errorf("ids = %v, want lr-lo+lr-mid+lr-hi (bounds inclusive, lr-out excluded)", runIDs(got))
		}
		if ids["lr-out"] {
			t.Error("lr-out (12:00, past until) should be excluded")
		}
	})

	t.Run("ListRuns/CombinedFilters", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/lr-combo"
		want := conformanceRun("lr-want", repo, "KS-1", StatusRunning)
		want.Workflow = "implement-ticket"
		want.Labels = map[string]string{"epic": "KS-100"}
		// Differs by status.
		wrongStatus := conformanceRun("lr-status", repo, "KS-1", StatusSuccess)
		wrongStatus.Workflow = "implement-ticket"
		wrongStatus.Labels = map[string]string{"epic": "KS-100"}
		// Differs by label.
		wrongLabel := conformanceRun("lr-label", repo, "KS-1", StatusRunning)
		wrongLabel.Workflow = "implement-ticket"
		wrongLabel.Labels = map[string]string{"epic": "KS-999"}
		for _, r := range []*Run{want, wrongStatus, wrongLabel} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
		}
		got, err := s.ListRuns(ctx, RunFilter{
			Repo:     repo,
			Statuses: []Status{StatusRunning},
			Workflow: "implement-ticket",
			Ticket:   "KS-1",
			Labels:   map[string]string{"epic": "KS-100"},
		})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 1 || got[0].ID != "lr-want" {
			t.Fatalf("ids = %v, want [lr-want]", runIDs(got))
		}
	})

	t.Run("ListRuns/NoFiltersReturnsAllForRepo", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/lr-all"
		for _, st := range []Status{StatusPending, StatusSuccess, StatusFailed} {
			r := conformanceRun("lr-"+string(st), repo, "PROJ-1", st)
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
		}
		got, err := s.ListRuns(ctx, RunFilter{Repo: repo})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3 (no status filter = all statuses)", len(got))
		}
	})

	t.Run("ListRuns/EmptyResultNonNil", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		got, err := s.ListRuns(ctx, RunFilter{Repo: "github.com/org/nope"})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if got == nil {
			t.Error("expected non-nil empty slice, got nil")
		}
		if len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
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

		run1 := conformanceRun("run-1", repo, ticket, StatusPending) // active, matching, newest
		run1Older := conformanceRun("run-1-older", repo, ticket, StatusRunning)
		run1Older.StartedAt = run1.StartedAt.Add(-1 * time.Hour)
		run2 := conformanceRun("run-2", repo, ticket, StatusSuccess)    // inactive
		run3 := conformanceRun("run-3", repo, "PROJ-99", StatusRunning) // different ticket

		for _, r := range []*Run{run1, run1Older, run2, run3} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun %s: %v", r.ID, err)
			}
		}

		results, err := s.FindActiveByTicket(ctx, repo, ticket, "default")
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

		results, err := s.FindActiveByTicket(ctx, repo, ticket, "default")
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

		results, err := s.FindActiveByTicket(ctx, "github.com/org/repo", ticket, "default")
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
			// ListActive contract: only pending or running rows are returned.
			// Terminal statuses (success/failed/killed) leaking through here
			// would corrupt CountActive and break duplicate-ticket protection.
			if r.Status != StatusPending && r.Status != StatusRunning {
				t.Errorf("run %s status: got %q, want pending or running", r.ID, r.Status)
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

	t.Run("ListActive/SortedByStartedAtDesc", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		// Three runs of mixed pending/running status with strictly
		// increasing StartedAt values. ListActive must return them in
		// descending order regardless of how a backend partitions the
		// query (e.g. DynamoDB's separate by-status GSI lookups).
		base := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
		entries := []struct {
			id     string
			status Status
			offset time.Duration
		}{
			{"sort-pending-old", StatusPending, 0},
			{"sort-running-mid", StatusRunning, 1 * time.Minute},
			{"sort-pending-new", StatusPending, 2 * time.Minute},
		}
		for _, e := range entries {
			r := conformanceRun(e.id, "github.com/org/repo", "PROJ-1", e.status)
			r.StartedAt = base.Add(e.offset)
			r.TimeoutAt = r.StartedAt.Add(60 * time.Minute)
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatalf("CreateRun %s: %v", r.ID, err)
			}
		}
		runs, err := s.ListActive(ctx)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		if len(runs) != 3 {
			t.Fatalf("len: got %d, want 3", len(runs))
		}
		want := []string{"sort-pending-new", "sort-running-mid", "sort-pending-old"}
		for i, r := range runs {
			if r.ID != want[i] {
				t.Errorf("position %d: got %q, want %q (full order: %v)",
					i, r.ID, want[i], runIDs(runs))
			}
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

	t.Run("LabelsRoundTrip/Nil", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		// Labels is nil by default from conformanceRun.

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Labels != nil {
			t.Errorf("Labels: expected nil, got %v", got.Labels)
		}
	})

	t.Run("LabelsRoundTrip/Empty", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		run.Labels = map[string]string{}

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Labels == nil {
			t.Error("Labels: expected non-nil empty map, got nil")
		}
		if len(got.Labels) != 0 {
			t.Errorf("Labels len: got %d, want 0", len(got.Labels))
		}
	})

	t.Run("LabelsRoundTrip/Populated", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		run.Labels = map[string]string{
			"epic":    "KS-100",
			"variant": "v3",
		}

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if len(got.Labels) != 2 {
			t.Fatalf("Labels len: got %d, want 2", len(got.Labels))
		}
		if got.Labels["epic"] != "KS-100" {
			t.Errorf("Labels[epic]: got %q, want %q", got.Labels["epic"], "KS-100")
		}
		if got.Labels["variant"] != "v3" {
			t.Errorf("Labels[variant]: got %q, want %q", got.Labels["variant"], "v3")
		}
	})

	// Labels and Metadata are independent fields that must not bleed into each
	// other — provider-internal Metadata (cluster_arn, …) and user Labels share
	// nothing despite both being map[string]string.
	t.Run("LabelsRoundTrip/IndependentFromMetadata", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		run := conformanceRun("r1", "github.com/org/repo", "PROJ-1", StatusPending)
		run.Metadata = map[string]string{"cluster_arn": "arn:aws:ecs:x"}
		run.Labels = map[string]string{"epic": "KS-100"}

		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if len(got.Metadata) != 1 || got.Metadata["cluster_arn"] != "arn:aws:ecs:x" {
			t.Errorf("Metadata: got %v, want {cluster_arn: arn:aws:ecs:x}", got.Metadata)
		}
		if len(got.Labels) != 1 || got.Labels["epic"] != "KS-100" {
			t.Errorf("Labels: got %v, want {epic: KS-100}", got.Labels)
		}
		if _, leaked := got.Labels["cluster_arn"]; leaked {
			t.Error("Labels leaked a Metadata key")
		}
		if _, leaked := got.Metadata["epic"]; leaked {
			t.Error("Metadata leaked a Labels key")
		}
	})

	// TokensRoundTrip/AllListPaths guards the SQLite SELECT/scan column-order
	// coupling: every list query carries its own literal column list feeding
	// the shared scanRun, so a reorder/typo of the trailing token columns in
	// ONE query would corrupt that path while GetRun-based tests still pass.
	// Asserting distinct non-zero token VALUES (not just non-nil) through all
	// four list paths catches a positional mismatch in any single SELECT, for
	// both stores.
	t.Run("TokensRoundTrip/AllListPaths", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		repo := "github.com/org/tokens-repo"
		run := conformanceRun("tk1", repo, "TOK-1", StatusRunning)
		want := &TokenUsage{
			InputTokens:         101,
			OutputTokens:        202,
			CacheCreationTokens: 303,
			CacheReadTokens:     404,
			Turns:               5,
		}
		run.Tokens = want
		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		check := func(path string, runs []*Run) {
			var found *Run
			for _, r := range runs {
				if r.ID == "tk1" {
					found = r
					break
				}
			}
			if found == nil {
				t.Fatalf("%s: run tk1 not returned", path)
			}
			if found.Tokens == nil {
				t.Fatalf("%s: Tokens nil, want %+v", path, *want)
			}
			if *found.Tokens != *want {
				t.Errorf("%s: Tokens = %+v, want %+v", path, *found.Tokens, *want)
			}
		}

		byRepo, err := s.ListByRepo(ctx, repo, false)
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		check("ListByRepo", byRepo)

		listRuns, err := s.ListRuns(ctx, RunFilter{Repo: repo})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		check("ListRuns", listRuns)

		byTicket, err := s.FindActiveByTicket(ctx, repo, "TOK-1", "default")
		if err != nil {
			t.Fatalf("FindActiveByTicket: %v", err)
		}
		check("FindActiveByTicket", byTicket)

		active, err := s.ListActive(ctx)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		check("ListActive", active)
	})

	t.Run("Queue/ClaimOrder", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		t0 := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)
		mk := func(id string, p Priority, off time.Duration) *Run {
			return &Run{ID: id, Repo: "qr", Ticket: id, Workflow: "w", Provider: "aws-ecs",
				Status: StatusQueued, Priority: p, EnqueuedAt: t0.Add(off), LaunchedBy: "me",
				TimeoutAt: t0.Add(time.Hour)}
		}
		for _, r := range []*Run{mk("a", PriorityLow, 0), mk("b", PriorityHigh, time.Minute), mk("c", PriorityHigh, 0)} {
			if err := s.CreateRun(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.ClaimNextQueued(ctx, "qr")
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.ID != "c" {
			t.Fatalf("want c (high, oldest), got %+v", got)
		}
		if got.Status != StatusPending {
			t.Errorf("claimed status = %q, want pending", got.Status)
		}
	})

	t.Run("Queue/ClaimEmpty", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		got, err := s.ClaimNextQueued(ctx, "none")
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("want nil, got %+v", got)
		}
	})

	t.Run("Queue/QueuedBlocksDuplicate", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		now := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)
		if err := s.CreateRun(ctx, &Run{ID: "qd", Repo: "qr2", Ticket: "T-9", Workflow: "w",
			Provider: "aws-ecs", Status: StatusQueued, Priority: PriorityMed, LaunchedBy: "me",
			EnqueuedAt: now, TimeoutAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		got, err := s.FindActiveByTicket(ctx, "qr2", "T-9", "w")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("queued run should block duplicate; got %d active", len(got))
		}
		// A different workflow for the same ticket is NOT a duplicate.
		other, err := s.FindActiveByTicket(ctx, "qr2", "T-9", "other-wf")
		if err != nil {
			t.Fatal(err)
		}
		if len(other) != 0 {
			t.Fatalf("different workflow must not match; got %d", len(other))
		}
	})

	t.Run("Queue/StatusRoundTrip", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		now := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)
		for _, st := range []Status{StatusQueued, StatusCancelled} {
			id := "rt-" + string(st)
			if err := s.CreateRun(ctx, &Run{ID: id, Repo: "rtr", Ticket: id, Provider: "aws-ecs",
				Status: st, LaunchedBy: "me", StartedAt: now, TimeoutAt: now.Add(time.Hour)}); err != nil {
				t.Fatalf("CreateRun(%s): %v", st, err)
			}
			got, err := s.GetRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != st {
				t.Errorf("status = %q, want %q", got.Status, st)
			}
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

// TestCopyItem_DeepCopiesMetadata pins that mutating a returned metadata map
// does not corrupt subsequent reads. A shallow copy would silently alias the
// inner map[string]AttributeValue.
func TestCopyItem_DeepCopiesMetadata(t *testing.T) {
	t.Parallel()
	orig := map[string]types.AttributeValue{
		"id": &types.AttributeValueMemberS{Value: "x"},
		"metadata": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"cluster_arn": &types.AttributeValueMemberS{Value: "arn:original"},
		}},
	}
	got := copyItem(orig)
	// Mutate the returned metadata.
	got["metadata"].(*types.AttributeValueMemberM).Value["cluster_arn"] = &types.AttributeValueMemberS{Value: "arn:mutated"}
	// Original must be untouched.
	origMeta := orig["metadata"].(*types.AttributeValueMemberM).Value["cluster_arn"].(*types.AttributeValueMemberS).Value
	if origMeta != "arn:original" {
		t.Errorf("original metadata mutated: got %q, want %q", origMeta, "arn:original")
	}
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
