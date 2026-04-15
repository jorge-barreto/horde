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
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
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

func parseRun(item map[string]types.AttributeValue) (*Run, error) {
	idAttr, ok := item[AttrID].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("parsing run: missing or invalid %q attribute", AttrID)
	}
	id := idAttr.Value

	run := &Run{ID: id}

	repoAttr, ok := item[AttrRepo].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("parsing run %q: missing or invalid %q attribute", id, AttrRepo)
	}
	run.Repo = repoAttr.Value

	ticketAttr, ok := item[AttrTicket].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("parsing run %q: missing or invalid %q attribute", id, AttrTicket)
	}
	run.Ticket = ticketAttr.Value

	branchAttr, ok := item[AttrBranch].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("parsing run %q: missing or invalid %q attribute", id, AttrBranch)
	}
	run.Branch = branchAttr.Value

	workflowAttr, ok := item[AttrWorkflow].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("parsing run %q: missing or invalid %q attribute", id, AttrWorkflow)
	}
	run.Workflow = workflowAttr.Value

	providerAttr, ok := item[AttrProvider].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("parsing run %q: missing or invalid %q attribute", id, AttrProvider)
	}
	run.Provider = providerAttr.Value

	instanceIDAttr, ok := item[AttrInstanceID].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("parsing run %q: missing or invalid %q attribute", id, AttrInstanceID)
	}
	run.InstanceID = instanceIDAttr.Value

	launchedByAttr, ok := item[AttrLaunchedBy].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("parsing run %q: missing or invalid %q attribute", id, AttrLaunchedBy)
	}
	run.LaunchedBy = launchedByAttr.Value

	statusAttr, ok := item[AttrStatus].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("parsing run %q: missing or invalid %q attribute", id, AttrStatus)
	}
	run.Status = Status(statusAttr.Value)

	var err error

	startedAtAttr, ok := item[AttrStartedAt].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("parsing run %q: missing or invalid %q attribute", id, AttrStartedAt)
	}
	run.StartedAt, err = time.Parse(time.RFC3339, startedAtAttr.Value)
	if err != nil {
		return nil, fmt.Errorf("parsing run %q: parsing started_at: %w", id, err)
	}

	timeoutAtAttr, ok := item[AttrTimeoutAt].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("parsing run %q: missing or invalid %q attribute", id, AttrTimeoutAt)
	}
	run.TimeoutAt, err = time.Parse(time.RFC3339, timeoutAtAttr.Value)
	if err != nil {
		return nil, fmt.Errorf("parsing run %q: parsing timeout_at: %w", id, err)
	}

	if av, ok := item[AttrExitCode]; ok {
		nv, ok := av.(*types.AttributeValueMemberN)
		if !ok {
			return nil, fmt.Errorf("parsing run %q: invalid %q attribute", id, AttrExitCode)
		}
		v, err := strconv.Atoi(nv.Value)
		if err != nil {
			return nil, fmt.Errorf("parsing run %q: parsing exit_code: %w", id, err)
		}
		run.ExitCode = &v
	}

	if av, ok := item[AttrCompletedAt]; ok {
		sv, ok := av.(*types.AttributeValueMemberS)
		if !ok {
			return nil, fmt.Errorf("parsing run %q: invalid %q attribute", id, AttrCompletedAt)
		}
		t, err := time.Parse(time.RFC3339, sv.Value)
		if err != nil {
			return nil, fmt.Errorf("parsing run %q: parsing completed_at: %w", id, err)
		}
		run.CompletedAt = &t
	}

	if av, ok := item[AttrTotalCostUSD]; ok {
		nv, ok := av.(*types.AttributeValueMemberN)
		if !ok {
			return nil, fmt.Errorf("parsing run %q: invalid %q attribute", id, AttrTotalCostUSD)
		}
		v, err := strconv.ParseFloat(nv.Value, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing run %q: parsing total_cost_usd: %w", id, err)
		}
		run.TotalCostUSD = &v
	}

	if av, ok := item[AttrMetadata]; ok {
		mv, ok := av.(*types.AttributeValueMemberM)
		if !ok {
			return nil, fmt.Errorf("parsing run %q: invalid %q attribute", id, AttrMetadata)
		}
		meta := make(map[string]string, len(mv.Value))
		for k, v := range mv.Value {
			sv, ok := v.(*types.AttributeValueMemberS)
			if !ok {
				return nil, fmt.Errorf("parsing run %q: metadata[%q] is not a string", id, k)
			}
			meta[k] = sv.Value
		}
		run.Metadata = meta
	}

	return run, nil
}

func (s *DynamoStore) GetRun(ctx context.Context, id string) (*Run, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			AttrID: &types.AttributeValueMemberS{Value: id},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("getting run %q: %w", id, err)
	}
	if out.Item == nil {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	return parseRun(out.Item)
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
