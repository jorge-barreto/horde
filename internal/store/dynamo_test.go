package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type mockDynamoClient struct {
	describeTableFunc func(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
}

func (m *mockDynamoClient) DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	if m.describeTableFunc == nil {
		return &dynamodb.DescribeTableOutput{}, nil
	}
	return m.describeTableFunc(ctx, params, optFns...)
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
