package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type mockDynamoClient struct {
	describeTableFunc func(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	putItemFunc       func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
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

func newTestDynamoStore(client dynamoAPI, tableName string) *DynamoStore {
	return &DynamoStore{client: client, tableName: tableName}
}

func intPtr(v int) *int              { return &v }
func timePtr(v time.Time) *time.Time { return &v }
func float64Ptr(v float64) *float64  { return &v }

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
	if v, ok := item[AttrID].(*types.AttributeValueMemberS); !ok || v.Value != "k7m2xp4qr9n3" {
		t.Errorf("AttrID = %v, want S{k7m2xp4qr9n3}", item[AttrID])
	}
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
	startedAtStr := item[AttrStartedAt].(*types.AttributeValueMemberS).Value
	if !strings.HasSuffix(startedAtStr, "Z") {
		t.Errorf("AttrStartedAt %q does not end in Z", startedAtStr)
	}
	timeoutAtStr := item[AttrTimeoutAt].(*types.AttributeValueMemberS).Value
	if !strings.HasSuffix(timeoutAtStr, "Z") {
		t.Errorf("AttrTimeoutAt %q does not end in Z", timeoutAtStr)
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
	if !strings.HasSuffix(startedAtStr, "Z") {
		t.Errorf("AttrStartedAt %q does not end in Z", startedAtStr)
	}
	timeoutAtStr := item[AttrTimeoutAt].(*types.AttributeValueMemberS).Value
	if !strings.HasSuffix(timeoutAtStr, "Z") {
		t.Errorf("AttrTimeoutAt %q does not end in Z", timeoutAtStr)
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
}
