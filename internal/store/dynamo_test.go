package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type mockDynamoClient struct {
	describeTableFunc func(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	putItemFunc       func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	getItemFunc       func(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	updateItemFunc    func(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

func (m *mockDynamoClient) DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	if m.describeTableFunc == nil {
		return &dynamodb.DescribeTableOutput{}, nil
	}
	return m.describeTableFunc(ctx, params, optFns...)
}

func (m *mockDynamoClient) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if m.putItemFunc == nil {
		return &dynamodb.PutItemOutput{}, nil
	}
	return m.putItemFunc(ctx, params, optFns...)
}

func (m *mockDynamoClient) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if m.getItemFunc == nil {
		return &dynamodb.GetItemOutput{}, nil
	}
	return m.getItemFunc(ctx, params, optFns...)
}

func (m *mockDynamoClient) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if m.updateItemFunc == nil {
		return &dynamodb.UpdateItemOutput{}, nil
	}
	return m.updateItemFunc(ctx, params, optFns...)
}

func newTestDynamoStore(client dynamoAPI, tableName string) *DynamoStore {
	return &DynamoStore{client: client, tableName: tableName}
}

func intPtr(v int) *int              { return &v }
func timePtr(v time.Time) *time.Time { return &v }
func float64Ptr(v float64) *float64  { return &v }
func statusPtr(v Status) *Status     { return &v }
func stringPtr(v string) *string     { return &v }

func validItem() map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		AttrID:         &types.AttributeValueMemberS{Value: "test-run-id"},
		AttrRepo:       &types.AttributeValueMemberS{Value: "github.com/acme/repo"},
		AttrTicket:     &types.AttributeValueMemberS{Value: "PROJ-1"},
		AttrBranch:     &types.AttributeValueMemberS{Value: "main"},
		AttrWorkflow:   &types.AttributeValueMemberS{Value: "ci"},
		AttrProvider:   &types.AttributeValueMemberS{Value: "docker"},
		AttrInstanceID: &types.AttributeValueMemberS{Value: "abc123"},
		AttrStatus:     &types.AttributeValueMemberS{Value: "pending"},
		AttrLaunchedBy: &types.AttributeValueMemberS{Value: "bob"},
		AttrStartedAt:  &types.AttributeValueMemberS{Value: "2026-04-15T10:00:00Z"},
		AttrTimeoutAt:  &types.AttributeValueMemberS{Value: "2026-04-15T11:00:00Z"},
	}
}

func TestNewDynamoStore_EmptyTableName(t *testing.T) {
	t.Parallel()
	mock := &mockDynamoClient{}
	store, err := newDynamoStore(context.Background(), mock, "")
	if err == nil {
		t.Fatal("expected error for empty table name, got nil")
	}
	if store != nil {
		t.Errorf("expected nil store, got %v", store)
	}
	if !strings.Contains(err.Error(), "table name is empty") {
		t.Errorf("expected error to contain %q, got %q", "table name is empty", err.Error())
	}
}

func TestNewDynamoStore_DescribeTableError(t *testing.T) {
	t.Parallel()
	tableName := "my-table"
	mock := &mockDynamoClient{
		describeTableFunc: func(_ context.Context, _ *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
			return nil, fmt.Errorf("access denied")
		},
	}
	store, err := newDynamoStore(context.Background(), mock, tableName)
	if err == nil {
		t.Fatal("expected error from DescribeTable, got nil")
	}
	if store != nil {
		t.Errorf("expected nil store, got %v", store)
	}
	if !strings.Contains(err.Error(), "pinging DynamoDB table") {
		t.Errorf("error should contain %q, got %q", "pinging DynamoDB table", err.Error())
	}
	if !strings.Contains(err.Error(), tableName) {
		t.Errorf("error should contain table name %q, got %q", tableName, err.Error())
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error should wrap original error, got %q", err.Error())
	}
}

func TestNewDynamoStore_Success(t *testing.T) {
	t.Parallel()
	tableName := "my-table"
	mock := &mockDynamoClient{}
	store, err := newDynamoStore(context.Background(), mock, tableName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}
	if store.tableName != tableName {
		t.Errorf("store.tableName = %q, want %q", store.tableName, tableName)
	}
	if store.client == nil {
		t.Error("store.client should not be nil")
	}
}

func TestNewDynamoStore_PassesTableNameToDescribeTable(t *testing.T) {
	t.Parallel()
	tableName := "my-test-table"
	var capturedInput *dynamodb.DescribeTableInput
	mock := &mockDynamoClient{
		describeTableFunc: func(_ context.Context, params *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
			capturedInput = params
			return &dynamodb.DescribeTableOutput{}, nil
		},
	}
	_, err := newDynamoStore(context.Background(), mock, tableName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedInput == nil {
		t.Fatal("DescribeTable was not called")
	}
	if capturedInput.TableName == nil || *capturedInput.TableName != tableName {
		t.Errorf("DescribeTable called with TableName %v, want %q", capturedInput.TableName, tableName)
	}
}

func TestDynamoStore_CreateRun_AllFields(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	timeoutAt := time.Date(2026, 4, 15, 11, 0, 0, 0, time.UTC)
	completedAt := time.Date(2026, 4, 15, 10, 30, 0, 0, time.UTC)
	run := &Run{
		ID:           "k7m2xp4qr9n3",
		Repo:         "github.com/acme/myrepo",
		Ticket:       "PROJ-42",
		Branch:       "feature/new-thing",
		Workflow:     "ci",
		Provider:     "aws-ecs",
		InstanceID:   "arn:aws:ecs:us-east-1:123456789012:task/abc",
		Status:       StatusRunning,
		ExitCode:     intPtr(0),
		LaunchedBy:   "alice",
		StartedAt:    startedAt,
		CompletedAt:  timePtr(completedAt),
		TimeoutAt:    timeoutAt,
		TotalCostUSD: float64Ptr(1.25),
		Metadata: map[string]string{
			"key1": "val1",
			"key2": "val2",
			"key3": "val3",
		},
	}
	var capturedInput *dynamodb.PutItemInput
	mock := &mockDynamoClient{
		putItemFunc: func(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			capturedInput = params
			return &dynamodb.PutItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.CreateRun(context.Background(), run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedInput == nil {
		t.Fatal("PutItem was not called")
	}
	item := capturedInput.Item
	if len(item) != 15 {
		t.Errorf("expected 15 attributes, got %d", len(item))
	}
	assertS := func(attr, want string) {
		t.Helper()
		v, ok := item[attr].(*types.AttributeValueMemberS)
		if !ok {
			t.Errorf("%s: not *types.AttributeValueMemberS, got %T", attr, item[attr])
			return
		}
		if v.Value != want {
			t.Errorf("%s = %q, want %q", attr, v.Value, want)
		}
	}
	assertS(AttrID, "k7m2xp4qr9n3")
	assertS(AttrRepo, "github.com/acme/myrepo")
	assertS(AttrTicket, "PROJ-42")
	assertS(AttrBranch, "feature/new-thing")
	assertS(AttrWorkflow, "ci")
	assertS(AttrProvider, "aws-ecs")
	assertS(AttrInstanceID, "arn:aws:ecs:us-east-1:123456789012:task/abc")
	assertS(AttrStatus, "running")
	assertS(AttrLaunchedBy, "alice")
	assertS(AttrStartedAt, "2026-04-15T10:00:00Z")
	assertS(AttrCompletedAt, "2026-04-15T10:30:00Z")
	assertS(AttrTimeoutAt, "2026-04-15T11:00:00Z")
	if v, ok := item[AttrExitCode].(*types.AttributeValueMemberN); !ok || v.Value != "0" {
		t.Errorf("AttrExitCode = %v, want N{0}", item[AttrExitCode])
	}
	if v, ok := item[AttrTotalCostUSD].(*types.AttributeValueMemberN); !ok || v.Value != "1.25" {
		t.Errorf("AttrTotalCostUSD = %v, want N{1.25}", item[AttrTotalCostUSD])
	}
	metaAttr, ok := item[AttrMetadata].(*types.AttributeValueMemberM)
	if !ok {
		t.Fatalf("AttrMetadata is not *types.AttributeValueMemberM: %T", item[AttrMetadata])
	}
	if len(metaAttr.Value) != 3 {
		t.Errorf("metadata map has %d keys, want 3", len(metaAttr.Value))
	}
	for _, kv := range []struct{ k, v string }{{"key1", "val1"}, {"key2", "val2"}, {"key3", "val3"}} {
		mv, ok := metaAttr.Value[kv.k].(*types.AttributeValueMemberS)
		if !ok || mv.Value != kv.v {
			t.Errorf("metadata[%q] = %v, want S{%q}", kv.k, metaAttr.Value[kv.k], kv.v)
		}
	}
	if capturedInput.ConditionExpression == nil || *capturedInput.ConditionExpression != "attribute_not_exists(id)" {
		t.Errorf("ConditionExpression = %v, want %q", capturedInput.ConditionExpression, "attribute_not_exists(id)")
	}
}

func TestDynamoStore_CreateRun_NilOptionalFields(t *testing.T) {
	t.Parallel()
	run := &Run{
		ID:         "run-nil-opts",
		Repo:       "github.com/acme/repo",
		Ticket:     "PROJ-1",
		Branch:     "main",
		Workflow:   "ci",
		Provider:   "docker",
		InstanceID: "abc123",
		Status:     StatusPending,
		LaunchedBy: "bob",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(time.Hour),
	}
	var capturedInput *dynamodb.PutItemInput
	mock := &mockDynamoClient{
		putItemFunc: func(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			capturedInput = params
			return &dynamodb.PutItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.CreateRun(context.Background(), run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	item := capturedInput.Item
	if _, ok := item[AttrExitCode]; ok {
		t.Error("AttrExitCode should be absent when ExitCode is nil")
	}
	if _, ok := item[AttrCompletedAt]; ok {
		t.Error("AttrCompletedAt should be absent when CompletedAt is nil")
	}
	if _, ok := item[AttrTotalCostUSD]; ok {
		t.Error("AttrTotalCostUSD should be absent when TotalCostUSD is nil")
	}
	if _, ok := item[AttrMetadata]; ok {
		t.Error("AttrMetadata should be absent when Metadata is nil")
	}
	if _, ok := item[AttrID].(*types.AttributeValueMemberS); !ok {
		t.Error("AttrID should be present as S type")
	}
}

func TestDynamoStore_CreateRun_EmptyMetadata(t *testing.T) {
	t.Parallel()
	run := &Run{
		ID:         "run-empty-meta",
		Repo:       "github.com/acme/repo",
		Ticket:     "PROJ-2",
		Branch:     "main",
		Workflow:   "ci",
		Provider:   "docker",
		InstanceID: "abc123",
		Status:     StatusPending,
		LaunchedBy: "carol",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(time.Hour),
		Metadata:   map[string]string{},
	}
	var capturedInput *dynamodb.PutItemInput
	mock := &mockDynamoClient{
		putItemFunc: func(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			capturedInput = params
			return &dynamodb.PutItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	if err := store.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	metaAttr, ok := capturedInput.Item[AttrMetadata].(*types.AttributeValueMemberM)
	if !ok {
		t.Fatalf("AttrMetadata is not *types.AttributeValueMemberM: %T", capturedInput.Item[AttrMetadata])
	}
	if len(metaAttr.Value) != 0 {
		t.Errorf("expected empty metadata map, got %d entries", len(metaAttr.Value))
	}
}

func TestDynamoStore_CreateRun_PutItemError(t *testing.T) {
	t.Parallel()
	run := &Run{
		ID:         "run-err",
		Repo:       "github.com/acme/repo",
		Ticket:     "PROJ-3",
		Branch:     "main",
		Workflow:   "ci",
		Provider:   "docker",
		InstanceID: "abc123",
		Status:     StatusPending,
		LaunchedBy: "dave",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(time.Hour),
	}
	mock := &mockDynamoClient{
		putItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return nil, fmt.Errorf("internal server error")
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.CreateRun(context.Background(), run)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "creating run") {
		t.Errorf("error %q should contain %q", err.Error(), "creating run")
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Errorf("error %q should contain %q", err.Error(), "internal server error")
	}
}

func TestDynamoStore_CreateRun_DuplicateID(t *testing.T) {
	t.Parallel()
	run := &Run{
		ID:         "run-dup",
		Repo:       "github.com/acme/repo",
		Ticket:     "PROJ-6",
		Branch:     "main",
		Workflow:   "ci",
		Provider:   "docker",
		InstanceID: "abc123",
		Status:     StatusPending,
		LaunchedBy: "grace",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(time.Hour),
	}
	mock := &mockDynamoClient{
		putItemFunc: func(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return nil, &types.ConditionalCheckFailedException{
				Message: aws.String("The conditional request failed"),
			}
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.CreateRun(context.Background(), run)
	if err == nil {
		t.Fatal("expected error for duplicate ID, got nil")
	}
	if !strings.Contains(err.Error(), "creating run") {
		t.Errorf("error %q should contain %q", err.Error(), "creating run")
	}
	if !strings.Contains(err.Error(), "duplicate id") {
		t.Errorf("error %q should contain %q", err.Error(), "duplicate id")
	}
	if !strings.Contains(err.Error(), "run-dup") {
		t.Errorf("error %q should contain run ID %q", err.Error(), "run-dup")
	}
}

func TestDynamoStore_CreateRun_TimesAreUTC(t *testing.T) {
	t.Parallel()
	est := time.FixedZone("EST", -5*3600)
	startedAt := time.Date(2026, 4, 15, 5, 0, 0, 0, est)
	timeoutAt := time.Date(2026, 4, 15, 6, 0, 0, 0, est)
	run := &Run{
		ID:         "run-tz",
		Repo:       "github.com/acme/repo",
		Ticket:     "PROJ-4",
		Branch:     "main",
		Workflow:   "ci",
		Provider:   "docker",
		InstanceID: "abc123",
		Status:     StatusPending,
		LaunchedBy: "eve",
		StartedAt:  startedAt,
		TimeoutAt:  timeoutAt,
	}
	var capturedInput *dynamodb.PutItemInput
	mock := &mockDynamoClient{
		putItemFunc: func(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			capturedInput = params
			return &dynamodb.PutItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	if err := store.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	item := capturedInput.Item
	startedAtStr := item[AttrStartedAt].(*types.AttributeValueMemberS).Value
	if startedAtStr != "2026-04-15T10:00:00Z" {
		t.Errorf("AttrStartedAt = %q, want %q", startedAtStr, "2026-04-15T10:00:00Z")
	}
	timeoutAtStr := item[AttrTimeoutAt].(*types.AttributeValueMemberS).Value
	if timeoutAtStr != "2026-04-15T11:00:00Z" {
		t.Errorf("AttrTimeoutAt = %q, want %q", timeoutAtStr, "2026-04-15T11:00:00Z")
	}
}

func TestDynamoStore_CreateRun_UsesTableName(t *testing.T) {
	t.Parallel()
	run := &Run{
		ID:         "run-tbl",
		Repo:       "github.com/acme/repo",
		Ticket:     "PROJ-5",
		Branch:     "main",
		Workflow:   "ci",
		Provider:   "docker",
		InstanceID: "abc123",
		Status:     StatusPending,
		LaunchedBy: "frank",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(time.Hour),
	}
	var capturedInput *dynamodb.PutItemInput
	mock := &mockDynamoClient{
		putItemFunc: func(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			capturedInput = params
			return &dynamodb.PutItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "my-custom-table")
	if err := store.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedInput == nil {
		t.Fatal("PutItem was not called")
	}
	if capturedInput.TableName == nil || *capturedInput.TableName != "my-custom-table" {
		t.Errorf("PutItem called with TableName %v, want %q", capturedInput.TableName, "my-custom-table")
	}
	if capturedInput.ConditionExpression == nil || *capturedInput.ConditionExpression != "attribute_not_exists(id)" {
		t.Errorf("ConditionExpression = %v, want %q", capturedInput.ConditionExpression, "attribute_not_exists(id)")
	}
}

func TestDynamoStore_GetRun_AllFields(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	timeoutAt := time.Date(2026, 4, 15, 11, 0, 0, 0, time.UTC)
	completedAt := time.Date(2026, 4, 15, 10, 30, 0, 0, time.UTC)
	item := map[string]types.AttributeValue{
		AttrID:           &types.AttributeValueMemberS{Value: "k7m2xp4qr9n3"},
		AttrRepo:         &types.AttributeValueMemberS{Value: "github.com/acme/myrepo"},
		AttrTicket:       &types.AttributeValueMemberS{Value: "PROJ-42"},
		AttrBranch:       &types.AttributeValueMemberS{Value: "feature/new-thing"},
		AttrWorkflow:     &types.AttributeValueMemberS{Value: "ci"},
		AttrProvider:     &types.AttributeValueMemberS{Value: "aws-ecs"},
		AttrInstanceID:   &types.AttributeValueMemberS{Value: "arn:aws:ecs:us-east-1:123456789012:task/abc"},
		AttrStatus:       &types.AttributeValueMemberS{Value: "running"},
		AttrLaunchedBy:   &types.AttributeValueMemberS{Value: "alice"},
		AttrStartedAt:    &types.AttributeValueMemberS{Value: "2026-04-15T10:00:00Z"},
		AttrTimeoutAt:    &types.AttributeValueMemberS{Value: "2026-04-15T11:00:00Z"},
		AttrExitCode:     &types.AttributeValueMemberN{Value: "0"},
		AttrCompletedAt:  &types.AttributeValueMemberS{Value: "2026-04-15T10:30:00Z"},
		AttrTotalCostUSD: &types.AttributeValueMemberN{Value: "1.25"},
		AttrMetadata: &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"key1": &types.AttributeValueMemberS{Value: "val1"},
			"key2": &types.AttributeValueMemberS{Value: "val2"},
		}},
	}
	mock := &mockDynamoClient{
		getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	run, err := store.GetRun(context.Background(), "k7m2xp4qr9n3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if run.ID != "k7m2xp4qr9n3" {
		t.Errorf("ID = %q, want %q", run.ID, "k7m2xp4qr9n3")
	}
	if run.Repo != "github.com/acme/myrepo" {
		t.Errorf("Repo = %q, want %q", run.Repo, "github.com/acme/myrepo")
	}
	if run.Ticket != "PROJ-42" {
		t.Errorf("Ticket = %q, want %q", run.Ticket, "PROJ-42")
	}
	if run.Branch != "feature/new-thing" {
		t.Errorf("Branch = %q, want %q", run.Branch, "feature/new-thing")
	}
	if run.Workflow != "ci" {
		t.Errorf("Workflow = %q, want %q", run.Workflow, "ci")
	}
	if run.Provider != "aws-ecs" {
		t.Errorf("Provider = %q, want %q", run.Provider, "aws-ecs")
	}
	if run.InstanceID != "arn:aws:ecs:us-east-1:123456789012:task/abc" {
		t.Errorf("InstanceID = %q, want %q", run.InstanceID, "arn:aws:ecs:us-east-1:123456789012:task/abc")
	}
	if run.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", run.Status, StatusRunning)
	}
	if run.LaunchedBy != "alice" {
		t.Errorf("LaunchedBy = %q, want %q", run.LaunchedBy, "alice")
	}
	if !run.StartedAt.Equal(startedAt) {
		t.Errorf("StartedAt = %v, want %v", run.StartedAt, startedAt)
	}
	if !run.TimeoutAt.Equal(timeoutAt) {
		t.Errorf("TimeoutAt = %v, want %v", run.TimeoutAt, timeoutAt)
	}
	if run.ExitCode == nil || *run.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", run.ExitCode)
	}
	if run.CompletedAt == nil || !run.CompletedAt.Equal(completedAt) {
		t.Errorf("CompletedAt = %v, want %v", run.CompletedAt, completedAt)
	}
	if run.TotalCostUSD == nil || *run.TotalCostUSD != 1.25 {
		t.Errorf("TotalCostUSD = %v, want 1.25", run.TotalCostUSD)
	}
	if run.Metadata["key1"] != "val1" || run.Metadata["key2"] != "val2" {
		t.Errorf("Metadata = %v, want key1=val1, key2=val2", run.Metadata)
	}
}

func TestDynamoStore_GetRun_NotFound(t *testing.T) {
	t.Parallel()
	mock := &mockDynamoClient{
		getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: nil}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	_, err := store.GetRun(context.Background(), "missing-id")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("expected errors.Is(err, ErrRunNotFound), got %v", err)
	}
	if !strings.Contains(err.Error(), "missing-id") {
		t.Errorf("error %q should contain %q", err.Error(), "missing-id")
	}
}

func TestDynamoStore_GetRun_GetItemError(t *testing.T) {
	t.Parallel()
	mock := &mockDynamoClient{
		getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	_, err := store.GetRun(context.Background(), "some-id")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "getting run") {
		t.Errorf("error %q should contain %q", err.Error(), "getting run")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q should contain %q", err.Error(), "connection refused")
	}
}

func TestDynamoStore_GetRun_NilOptionalFields(t *testing.T) {
	t.Parallel()
	item := map[string]types.AttributeValue{
		AttrID:         &types.AttributeValueMemberS{Value: "run-req-only"},
		AttrRepo:       &types.AttributeValueMemberS{Value: "github.com/acme/repo"},
		AttrTicket:     &types.AttributeValueMemberS{Value: "PROJ-1"},
		AttrBranch:     &types.AttributeValueMemberS{Value: "main"},
		AttrWorkflow:   &types.AttributeValueMemberS{Value: "ci"},
		AttrProvider:   &types.AttributeValueMemberS{Value: "docker"},
		AttrInstanceID: &types.AttributeValueMemberS{Value: "abc123"},
		AttrStatus:     &types.AttributeValueMemberS{Value: "pending"},
		AttrLaunchedBy: &types.AttributeValueMemberS{Value: "bob"},
		AttrStartedAt:  &types.AttributeValueMemberS{Value: "2026-04-15T10:00:00Z"},
		AttrTimeoutAt:  &types.AttributeValueMemberS{Value: "2026-04-15T11:00:00Z"},
	}
	mock := &mockDynamoClient{
		getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	run, err := store.GetRun(context.Background(), "run-req-only")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if run.ExitCode != nil {
		t.Errorf("ExitCode should be nil, got %v", run.ExitCode)
	}
	if run.CompletedAt != nil {
		t.Errorf("CompletedAt should be nil, got %v", run.CompletedAt)
	}
	if run.TotalCostUSD != nil {
		t.Errorf("TotalCostUSD should be nil, got %v", run.TotalCostUSD)
	}
	if run.Metadata != nil {
		t.Errorf("Metadata should be nil, got %v", run.Metadata)
	}
}

func TestDynamoStore_GetRun_UsesTableName(t *testing.T) {
	t.Parallel()
	var capturedInput *dynamodb.GetItemInput
	item := map[string]types.AttributeValue{
		AttrID:         &types.AttributeValueMemberS{Value: "run-tbl"},
		AttrRepo:       &types.AttributeValueMemberS{Value: "github.com/acme/repo"},
		AttrTicket:     &types.AttributeValueMemberS{Value: "PROJ-1"},
		AttrBranch:     &types.AttributeValueMemberS{Value: "main"},
		AttrWorkflow:   &types.AttributeValueMemberS{Value: "ci"},
		AttrProvider:   &types.AttributeValueMemberS{Value: "docker"},
		AttrInstanceID: &types.AttributeValueMemberS{Value: "abc123"},
		AttrStatus:     &types.AttributeValueMemberS{Value: "pending"},
		AttrLaunchedBy: &types.AttributeValueMemberS{Value: "bob"},
		AttrStartedAt:  &types.AttributeValueMemberS{Value: "2026-04-15T10:00:00Z"},
		AttrTimeoutAt:  &types.AttributeValueMemberS{Value: "2026-04-15T11:00:00Z"},
	}
	mock := &mockDynamoClient{
		getItemFunc: func(_ context.Context, params *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			capturedInput = params
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
	}
	store := newTestDynamoStore(mock, "my-custom-table")
	_, err := store.GetRun(context.Background(), "run-tbl")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedInput == nil {
		t.Fatal("GetItem was not called")
	}
	if capturedInput.TableName == nil || *capturedInput.TableName != "my-custom-table" {
		t.Errorf("GetItem called with TableName %v, want %q", capturedInput.TableName, "my-custom-table")
	}
}

func TestDynamoStore_GetRun_UsesCorrectKey(t *testing.T) {
	t.Parallel()
	var capturedInput *dynamodb.GetItemInput
	item := map[string]types.AttributeValue{
		AttrID:         &types.AttributeValueMemberS{Value: "test-run-id"},
		AttrRepo:       &types.AttributeValueMemberS{Value: "github.com/acme/repo"},
		AttrTicket:     &types.AttributeValueMemberS{Value: "PROJ-1"},
		AttrBranch:     &types.AttributeValueMemberS{Value: "main"},
		AttrWorkflow:   &types.AttributeValueMemberS{Value: "ci"},
		AttrProvider:   &types.AttributeValueMemberS{Value: "docker"},
		AttrInstanceID: &types.AttributeValueMemberS{Value: "abc123"},
		AttrStatus:     &types.AttributeValueMemberS{Value: "pending"},
		AttrLaunchedBy: &types.AttributeValueMemberS{Value: "bob"},
		AttrStartedAt:  &types.AttributeValueMemberS{Value: "2026-04-15T10:00:00Z"},
		AttrTimeoutAt:  &types.AttributeValueMemberS{Value: "2026-04-15T11:00:00Z"},
	}
	mock := &mockDynamoClient{
		getItemFunc: func(_ context.Context, params *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			capturedInput = params
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	_, err := store.GetRun(context.Background(), "test-run-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedInput == nil {
		t.Fatal("GetItem was not called")
	}
	if len(capturedInput.Key) != 1 {
		t.Errorf("Key has %d entries, want 1", len(capturedInput.Key))
	}
	keyVal, ok := capturedInput.Key[AttrID].(*types.AttributeValueMemberS)
	if !ok {
		t.Fatalf("Key[AttrID] is not *types.AttributeValueMemberS, got %T", capturedInput.Key[AttrID])
	}
	if keyVal.Value != "test-run-id" {
		t.Errorf("Key[AttrID].Value = %q, want %q", keyVal.Value, "test-run-id")
	}
}

func TestDynamoStore_GetRun_MissingRequiredField(t *testing.T) {
	t.Parallel()
	attrs := []string{
		AttrID, AttrRepo, AttrTicket, AttrBranch, AttrWorkflow,
		AttrProvider, AttrInstanceID, AttrLaunchedBy, AttrStatus,
		AttrStartedAt, AttrTimeoutAt,
	}
	for _, attr := range attrs {
		attr := attr
		t.Run(attr, func(t *testing.T) {
			t.Parallel()
			item := validItem()
			delete(item, attr)
			mock := &mockDynamoClient{
				getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{Item: item}, nil
				},
			}
			store := newTestDynamoStore(mock, "runs-table")
			_, err := store.GetRun(context.Background(), "test-run-id")
			if err == nil {
				t.Fatal("expected error for missing attribute, got nil")
			}
			if !strings.Contains(err.Error(), attr) {
				t.Errorf("expected error to contain %q, got %q", attr, err.Error())
			}
		})
	}
}

func TestDynamoStore_GetRun_WrongTypeRequiredField(t *testing.T) {
	t.Parallel()
	attrs := []string{
		AttrID, AttrRepo, AttrTicket, AttrBranch, AttrWorkflow,
		AttrProvider, AttrInstanceID, AttrLaunchedBy, AttrStatus,
		AttrStartedAt, AttrTimeoutAt,
	}
	for _, attr := range attrs {
		attr := attr
		t.Run(attr, func(t *testing.T) {
			t.Parallel()
			item := validItem()
			item[attr] = &types.AttributeValueMemberN{Value: "999"}
			mock := &mockDynamoClient{
				getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{Item: item}, nil
				},
			}
			store := newTestDynamoStore(mock, "runs-table")
			_, err := store.GetRun(context.Background(), "test-run-id")
			if err == nil {
				t.Fatal("expected error for wrong-type attribute, got nil")
			}
			if !strings.Contains(err.Error(), attr) {
				t.Errorf("expected error to contain %q, got %q", attr, err.Error())
			}
		})
	}
}

func TestDynamoStore_GetRun_WrongTypeOptionalField(t *testing.T) {
	t.Parallel()
	cases := []struct {
		attr     string
		badValue types.AttributeValue
	}{
		{AttrExitCode, &types.AttributeValueMemberS{Value: "not-a-number"}},
		{AttrCompletedAt, &types.AttributeValueMemberN{Value: "12345"}},
		{AttrTotalCostUSD, &types.AttributeValueMemberS{Value: "not-a-number"}},
		{AttrMetadata, &types.AttributeValueMemberS{Value: "not-a-map"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.attr, func(t *testing.T) {
			t.Parallel()
			item := validItem()
			item[tc.attr] = tc.badValue
			mock := &mockDynamoClient{
				getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{Item: item}, nil
				},
			}
			store := newTestDynamoStore(mock, "runs-table")
			_, err := store.GetRun(context.Background(), "test-run-id")
			if err == nil {
				t.Fatal("expected error for wrong-type optional attribute, got nil")
			}
			if !strings.Contains(err.Error(), tc.attr) {
				t.Errorf("expected error to contain %q, got %q", tc.attr, err.Error())
			}
		})
	}
}

func TestDynamoStore_GetRun_UnparseableValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		attr     string
		badValue types.AttributeValue
		errText  string
	}{
		{AttrStartedAt, &types.AttributeValueMemberS{Value: "not-a-date"}, "parsing started_at"},
		{AttrTimeoutAt, &types.AttributeValueMemberS{Value: "not-a-date"}, "parsing timeout_at"},
		{AttrExitCode, &types.AttributeValueMemberN{Value: "not-an-int"}, "parsing exit_code"},
		{AttrCompletedAt, &types.AttributeValueMemberS{Value: "not-a-date"}, "parsing completed_at"},
		{AttrTotalCostUSD, &types.AttributeValueMemberN{Value: "not-a-float"}, "parsing total_cost_usd"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.attr, func(t *testing.T) {
			t.Parallel()
			item := validItem()
			item[tc.attr] = tc.badValue
			mock := &mockDynamoClient{
				getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					return &dynamodb.GetItemOutput{Item: item}, nil
				},
			}
			store := newTestDynamoStore(mock, "runs-table")
			_, err := store.GetRun(context.Background(), "test-run-id")
			if err == nil {
				t.Fatal("expected error for unparseable value, got nil")
			}
			if !strings.Contains(err.Error(), tc.errText) {
				t.Errorf("expected error to contain %q, got %q", tc.errText, err.Error())
			}
		})
	}
}

func TestDynamoStore_GetRun_EmptyAttributeMap(t *testing.T) {
	t.Parallel()
	item := map[string]types.AttributeValue{}
	mock := &mockDynamoClient{
		getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	_, err := store.GetRun(context.Background(), "test-run-id")
	if err == nil {
		t.Fatal("expected error for empty attribute map, got nil")
	}
	if !strings.Contains(err.Error(), AttrID) {
		t.Errorf("expected error to contain %q, got %q", AttrID, err.Error())
	}
}

func TestDynamoStore_GetRun_NonStringMetadataValue(t *testing.T) {
	t.Parallel()
	item := validItem()
	item[AttrMetadata] = &types.AttributeValueMemberM{
		Value: map[string]types.AttributeValue{
			"good-key": &types.AttributeValueMemberS{Value: "val"},
			"bad-key":  &types.AttributeValueMemberN{Value: "123"},
		},
	}
	mock := &mockDynamoClient{
		getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	_, err := store.GetRun(context.Background(), "test-run-id")
	if err == nil {
		t.Fatal("expected error for non-string metadata value, got nil")
	}
	if !strings.Contains(err.Error(), "metadata") {
		t.Errorf("expected error to contain %q, got %q", "metadata", err.Error())
	}
	if !strings.Contains(err.Error(), "bad-key") {
		t.Errorf("expected error to contain %q, got %q", "bad-key", err.Error())
	}
}

func TestDynamoStore_UpdateRun_AllFields(t *testing.T) {
	t.Parallel()
	someTime := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	var capturedInput *dynamodb.UpdateItemInput
	mock := &mockDynamoClient{
		updateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			capturedInput = params
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	update := &RunUpdate{
		Status:       statusPtr(StatusSuccess),
		InstanceID:   stringPtr("new-instance"),
		ExitCode:     intPtr(0),
		CompletedAt:  timePtr(someTime),
		TotalCostUSD: float64Ptr(1.25),
		TimeoutAt:    timePtr(someTime),
		Metadata:     map[string]string{"new": "val"},
	}
	err := store.UpdateRun(context.Background(), "the-id", update)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedInput == nil {
		t.Fatal("UpdateItem was not called")
	}
	if !strings.HasPrefix(*capturedInput.UpdateExpression, "SET ") {
		t.Errorf("UpdateExpression = %q, want to start with %q", *capturedInput.UpdateExpression, "SET ")
	}
	for _, placeholder := range []string{":st", ":iid", ":ec", ":ca", ":cost", ":ta", ":meta"} {
		if !strings.Contains(*capturedInput.UpdateExpression, placeholder) {
			t.Errorf("UpdateExpression %q missing placeholder %q", *capturedInput.UpdateExpression, placeholder)
		}
	}
	if len(capturedInput.ExpressionAttributeValues) != 7 {
		t.Errorf("ExpressionAttributeValues len = %d, want 7", len(capturedInput.ExpressionAttributeValues))
	}
	if capturedInput.ExpressionAttributeNames["#st"] != "status" {
		t.Errorf("ExpressionAttributeNames[\"#st\"] = %q, want %q", capturedInput.ExpressionAttributeNames["#st"], "status")
	}
	if capturedInput.ConditionExpression == nil || *capturedInput.ConditionExpression != "attribute_exists(id)" {
		t.Errorf("ConditionExpression = %v, want %q", capturedInput.ConditionExpression, "attribute_exists(id)")
	}
	keyVal, ok := capturedInput.Key[AttrID].(*types.AttributeValueMemberS)
	if !ok || keyVal.Value != "the-id" {
		t.Errorf("Key[AttrID] = %v, want S{\"the-id\"}", capturedInput.Key[AttrID])
	}
	if capturedInput.TableName == nil || *capturedInput.TableName != "runs-table" {
		t.Errorf("TableName = %v, want %q", capturedInput.TableName, "runs-table")
	}
	caVal, ok := capturedInput.ExpressionAttributeValues[":ca"].(*types.AttributeValueMemberS)
	if !ok || !strings.HasSuffix(caVal.Value, "Z") {
		t.Errorf(":ca = %v, want S ending with Z", capturedInput.ExpressionAttributeValues[":ca"])
	}
	taVal, ok := capturedInput.ExpressionAttributeValues[":ta"].(*types.AttributeValueMemberS)
	if !ok || !strings.HasSuffix(taVal.Value, "Z") {
		t.Errorf(":ta = %v, want S ending with Z", capturedInput.ExpressionAttributeValues[":ta"])
	}
	ecVal, ok := capturedInput.ExpressionAttributeValues[":ec"].(*types.AttributeValueMemberN)
	if !ok || ecVal.Value != "0" {
		t.Errorf(":ec = %v, want N{\"0\"}", capturedInput.ExpressionAttributeValues[":ec"])
	}
	costVal, ok := capturedInput.ExpressionAttributeValues[":cost"].(*types.AttributeValueMemberN)
	if !ok || costVal.Value != "1.25" {
		t.Errorf(":cost = %v, want N{\"1.25\"}", capturedInput.ExpressionAttributeValues[":cost"])
	}
	metaVal, ok := capturedInput.ExpressionAttributeValues[":meta"].(*types.AttributeValueMemberM)
	if !ok {
		t.Fatalf(":meta is not *types.AttributeValueMemberM, got %T", capturedInput.ExpressionAttributeValues[":meta"])
	}
	sv, ok := metaVal.Value["new"].(*types.AttributeValueMemberS)
	if !ok || sv.Value != "val" {
		t.Errorf("metadata[\"new\"] = %v, want S{\"val\"}", metaVal.Value["new"])
	}
}

func TestDynamoStore_UpdateRun_SingleField(t *testing.T) {
	t.Parallel()
	var capturedInput *dynamodb.UpdateItemInput
	mock := &mockDynamoClient{
		updateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			capturedInput = params
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.UpdateRun(context.Background(), "run-1", &RunUpdate{Status: statusPtr(StatusRunning)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedInput.UpdateExpression == nil || *capturedInput.UpdateExpression != "SET #st = :st" {
		t.Errorf("UpdateExpression = %v, want %q", capturedInput.UpdateExpression, "SET #st = :st")
	}
	if len(capturedInput.ExpressionAttributeValues) != 1 {
		t.Errorf("ExpressionAttributeValues len = %d, want 1", len(capturedInput.ExpressionAttributeValues))
	}
	stVal, ok := capturedInput.ExpressionAttributeValues[":st"].(*types.AttributeValueMemberS)
	if !ok || stVal.Value != "running" {
		t.Errorf(":st = %v, want S{\"running\"}", capturedInput.ExpressionAttributeValues[":st"])
	}
	if capturedInput.ExpressionAttributeNames["#st"] != "status" {
		t.Errorf("ExpressionAttributeNames[\"#st\"] = %q, want %q", capturedInput.ExpressionAttributeNames["#st"], "status")
	}
	if capturedInput.ConditionExpression == nil || *capturedInput.ConditionExpression != "attribute_exists(id)" {
		t.Errorf("ConditionExpression = %v, want %q", capturedInput.ConditionExpression, "attribute_exists(id)")
	}
}

func TestDynamoStore_UpdateRun_NoFieldsSet_ExistingRun(t *testing.T) {
	t.Parallel()
	var updateCalled, getCalled bool
	mock := &mockDynamoClient{
		getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			getCalled = true
			return &dynamodb.GetItemOutput{Item: validItem()}, nil
		},
		updateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			updateCalled = true
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.UpdateRun(context.Background(), "test-run-id", &RunUpdate{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updateCalled {
		t.Error("UpdateItem should not be called when no fields are set")
	}
	if !getCalled {
		t.Error("GetItem should be called when no fields are set")
	}
}

func TestDynamoStore_UpdateRun_NoFieldsSet_NotFound(t *testing.T) {
	t.Parallel()
	mock := &mockDynamoClient{
		getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: nil}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.UpdateRun(context.Background(), "missing-id", &RunUpdate{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("expected errors.Is(err, ErrRunNotFound), got %v", err)
	}
	if !strings.Contains(err.Error(), "missing-id") {
		t.Errorf("error %q should contain %q", err.Error(), "missing-id")
	}
}

func TestDynamoStore_UpdateRun_NotFound(t *testing.T) {
	t.Parallel()
	mock := &mockDynamoClient{
		updateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, &types.ConditionalCheckFailedException{Message: aws.String("The conditional request failed")}
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.UpdateRun(context.Background(), "missing-id", &RunUpdate{Status: statusPtr(StatusRunning)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("expected errors.Is(err, ErrRunNotFound), got %v", err)
	}
	if !strings.Contains(err.Error(), "missing-id") {
		t.Errorf("error %q should contain %q", err.Error(), "missing-id")
	}
}

func TestDynamoStore_UpdateRun_UpdateItemError(t *testing.T) {
	t.Parallel()
	mock := &mockDynamoClient{
		updateItemFunc: func(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return nil, fmt.Errorf("throttled")
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.UpdateRun(context.Background(), "run-1", &RunUpdate{Status: statusPtr(StatusRunning)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "updating run") {
		t.Errorf("error %q should contain %q", err.Error(), "updating run")
	}
	if !strings.Contains(err.Error(), "throttled") {
		t.Errorf("error %q should contain %q", err.Error(), "throttled")
	}
}

func TestDynamoStore_UpdateRun_UsesTableName(t *testing.T) {
	t.Parallel()
	var capturedInput *dynamodb.UpdateItemInput
	mock := &mockDynamoClient{
		updateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			capturedInput = params
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "my-custom-table")
	err := store.UpdateRun(context.Background(), "run-1", &RunUpdate{Status: statusPtr(StatusRunning)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedInput.TableName == nil || *capturedInput.TableName != "my-custom-table" {
		t.Errorf("TableName = %v, want %q", capturedInput.TableName, "my-custom-table")
	}
	if capturedInput.ConditionExpression == nil || *capturedInput.ConditionExpression != "attribute_exists(id)" {
		t.Errorf("ConditionExpression = %v, want %q", capturedInput.ConditionExpression, "attribute_exists(id)")
	}
}

func TestDynamoStore_UpdateRun_UTCNormalization(t *testing.T) {
	t.Parallel()
	est := time.FixedZone("EST", -5*3600)
	completedAt := time.Date(2026, 4, 15, 5, 0, 0, 0, est)
	timeoutAt := time.Date(2026, 4, 15, 6, 0, 0, 0, est)
	var capturedInput *dynamodb.UpdateItemInput
	mock := &mockDynamoClient{
		updateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			capturedInput = params
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.UpdateRun(context.Background(), "run-1", &RunUpdate{
		CompletedAt: timePtr(completedAt),
		TimeoutAt:   timePtr(timeoutAt),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	caVal, ok := capturedInput.ExpressionAttributeValues[":ca"].(*types.AttributeValueMemberS)
	if !ok || caVal.Value != "2026-04-15T10:00:00Z" {
		t.Errorf(":ca = %v, want S{\"2026-04-15T10:00:00Z\"}", capturedInput.ExpressionAttributeValues[":ca"])
	}
	taVal, ok := capturedInput.ExpressionAttributeValues[":ta"].(*types.AttributeValueMemberS)
	if !ok || taVal.Value != "2026-04-15T11:00:00Z" {
		t.Errorf(":ta = %v, want S{\"2026-04-15T11:00:00Z\"}", capturedInput.ExpressionAttributeValues[":ta"])
	}
}

func TestDynamoStore_UpdateRun_MetadataOverwrite(t *testing.T) {
	t.Parallel()
	var capturedInput *dynamodb.UpdateItemInput
	mock := &mockDynamoClient{
		updateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			capturedInput = params
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.UpdateRun(context.Background(), "run-1", &RunUpdate{
		Metadata: map[string]string{"new": "val"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	metaVal, ok := capturedInput.ExpressionAttributeValues[":meta"].(*types.AttributeValueMemberM)
	if !ok {
		t.Fatalf(":meta is not *types.AttributeValueMemberM, got %T", capturedInput.ExpressionAttributeValues[":meta"])
	}
	sv, ok := metaVal.Value["new"].(*types.AttributeValueMemberS)
	if !ok || sv.Value != "val" {
		t.Errorf("metadata[\"new\"] = %v, want S{\"val\"}", metaVal.Value["new"])
	}
	if len(capturedInput.ExpressionAttributeNames) > 0 {
		t.Errorf("ExpressionAttributeNames should be nil or empty when only metadata is updated, got %v", capturedInput.ExpressionAttributeNames)
	}
}

func TestDynamoStore_UpdateRun_StatusReservedWord(t *testing.T) {
	t.Parallel()
	var capturedInput *dynamodb.UpdateItemInput
	mock := &mockDynamoClient{
		updateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			capturedInput = params
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.UpdateRun(context.Background(), "run-1", &RunUpdate{Status: statusPtr(StatusRunning)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedInput.ExpressionAttributeNames["#st"] != "status" {
		t.Errorf("ExpressionAttributeNames[\"#st\"] = %q, want %q", capturedInput.ExpressionAttributeNames["#st"], "status")
	}
	if !strings.Contains(*capturedInput.UpdateExpression, "#st") {
		t.Errorf("UpdateExpression %q should contain %q", *capturedInput.UpdateExpression, "#st")
	}
	if strings.Contains(*capturedInput.UpdateExpression, " status ") {
		t.Errorf("UpdateExpression %q should not contain bare reserved word %q", *capturedInput.UpdateExpression, " status ")
	}
}

func TestDynamoStore_UpdateRun_NoFieldsSet_GetItemError(t *testing.T) {
	t.Parallel()
	mock := &mockDynamoClient{
		getItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}
	store := newTestDynamoStore(mock, "runs-table")
	err := store.UpdateRun(context.Background(), "run-1", &RunUpdate{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "checking run") {
		t.Errorf("error %q should contain %q", err.Error(), "checking run")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q should contain %q", err.Error(), "connection refused")
	}
}
