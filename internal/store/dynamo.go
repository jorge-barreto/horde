package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var _ Store = (*DynamoStore)(nil)

type dynamoAPI interface {
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

type DynamoStore struct {
	client    dynamoAPI
	tableName string
}

func NewDynamoStore(ctx context.Context, cfg aws.Config, tableName string) (*DynamoStore, error) {
	client := dynamodb.NewFromConfig(cfg)
	return newDynamoStore(ctx, client, tableName)
}

func newDynamoStore(ctx context.Context, client dynamoAPI, tableName string) (*DynamoStore, error) {
	if tableName == "" {
		return nil, fmt.Errorf("creating DynamoDB store: table name is empty")
	}
	if _, err := client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(tableName),
	}); err != nil {
		return nil, fmt.Errorf("pinging DynamoDB table %q: %w", tableName, err)
	}
	return &DynamoStore{client: client, tableName: tableName}, nil
}

func (s *DynamoStore) CreateRun(ctx context.Context, run *Run) error {
	item := map[string]types.AttributeValue{
		AttrID:         &types.AttributeValueMemberS{Value: run.ID},
		AttrRepo:       &types.AttributeValueMemberS{Value: run.Repo},
		AttrTicket:     &types.AttributeValueMemberS{Value: run.Ticket},
		AttrBranch:     &types.AttributeValueMemberS{Value: run.Branch},
		AttrWorkflow:   &types.AttributeValueMemberS{Value: run.Workflow},
		AttrProvider:   &types.AttributeValueMemberS{Value: run.Provider},
		AttrInstanceID: &types.AttributeValueMemberS{Value: run.InstanceID},
		AttrStatus:     &types.AttributeValueMemberS{Value: string(run.Status)},
		AttrLaunchedBy: &types.AttributeValueMemberS{Value: run.LaunchedBy},
		AttrStartedAt:  &types.AttributeValueMemberS{Value: run.StartedAt.UTC().Format(time.RFC3339)},
		AttrTimeoutAt:  &types.AttributeValueMemberS{Value: run.TimeoutAt.UTC().Format(time.RFC3339)},
	}
	if run.ExitCode != nil {
		item[AttrExitCode] = &types.AttributeValueMemberN{Value: strconv.Itoa(*run.ExitCode)}
	}
	if run.CompletedAt != nil {
		item[AttrCompletedAt] = &types.AttributeValueMemberS{Value: run.CompletedAt.UTC().Format(time.RFC3339)}
	}
	if run.TotalCostUSD != nil {
		item[AttrTotalCostUSD] = &types.AttributeValueMemberN{Value: strconv.FormatFloat(*run.TotalCostUSD, 'f', -1, 64)}
	}
	if run.Metadata != nil {
		metaMap := make(map[string]types.AttributeValue, len(run.Metadata))
		for k, v := range run.Metadata {
			metaMap[k] = &types.AttributeValueMemberS{Value: v}
		}
		item[AttrMetadata] = &types.AttributeValueMemberM{Value: metaMap}
	}
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.tableName),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(id)"),
	})
	if err != nil {
		var ccfe *types.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return fmt.Errorf("creating run: duplicate id %q", run.ID)
		}
		return fmt.Errorf("creating run: %w", err)
	}
	return nil
}

func (s *DynamoStore) GetRun(ctx context.Context, id string) (*Run, error) {
	return nil, fmt.Errorf("DynamoStore.GetRun: not implemented")
}

func (s *DynamoStore) UpdateRun(ctx context.Context, id string, update *RunUpdate) error {
	return fmt.Errorf("DynamoStore.UpdateRun: not implemented")
}

func (s *DynamoStore) ListByRepo(ctx context.Context, repo string, activeOnly bool) ([]*Run, error) {
	return nil, fmt.Errorf("DynamoStore.ListByRepo: not implemented")
}

func (s *DynamoStore) FindActiveByTicket(ctx context.Context, repo string, ticket string) ([]*Run, error) {
	return nil, fmt.Errorf("DynamoStore.FindActiveByTicket: not implemented")
}

func (s *DynamoStore) CountActive(ctx context.Context) (int, error) {
	return 0, fmt.Errorf("DynamoStore.CountActive: not implemented")
}
