package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var _ Store = (*DynamoStore)(nil)

type dynamoAPI interface {
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

type DynamoStore struct {
	client    dynamoAPI
	tableName string
}

// Close is a no-op. The underlying AWS SDK client holds no resources
// that need explicit release; present to satisfy the Store interface.
func (s *DynamoStore) Close() error { return nil }

func NewDynamoStore(ctx context.Context, cfg aws.Config, tableName string) (*DynamoStore, error) {
	client := dynamodb.NewFromConfig(cfg)
	return newDynamoStore(ctx, client, tableName)
}

func newDynamoStore(_ context.Context, client dynamoAPI, tableName string) (*DynamoStore, error) {
	if tableName == "" {
		return nil, fmt.Errorf("creating DynamoDB store: table name is empty")
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
		AttrStatus:     &types.AttributeValueMemberS{Value: string(run.Status)},
		AttrLaunchedBy: &types.AttributeValueMemberS{Value: run.LaunchedBy},
		AttrStartedAt:  &types.AttributeValueMemberS{Value: run.StartedAt.UTC().Format(time.RFC3339)},
		AttrTimeoutAt:  &types.AttributeValueMemberS{Value: run.TimeoutAt.UTC().Format(time.RFC3339)},
		AttrPriority:   &types.AttributeValueMemberS{Value: string(run.Priority)},
	}
	// enqueued_at is set only for queued runs; mirror the completed_at
	// conditional-write pattern (zero time means "not enqueued").
	if !run.EnqueuedAt.IsZero() {
		item[AttrEnqueuedAt] = &types.AttributeValueMemberS{Value: run.EnqueuedAt.UTC().Format(time.RFC3339)}
	}
	// instance_id is the GSI "by-instance" partition key. DynamoDB rejects
	// empty strings on GSI keys, so only set it when known (UpdateRun fills
	// it in once the provider returns a real instance ID).
	if run.InstanceID != "" {
		item[AttrInstanceID] = &types.AttributeValueMemberS{Value: run.InstanceID}
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
	if run.Tokens != nil {
		item[AttrInputTokens] = &types.AttributeValueMemberN{Value: strconv.Itoa(run.Tokens.InputTokens)}
		item[AttrOutputTokens] = &types.AttributeValueMemberN{Value: strconv.Itoa(run.Tokens.OutputTokens)}
		item[AttrCacheCreationTokens] = &types.AttributeValueMemberN{Value: strconv.Itoa(run.Tokens.CacheCreationTokens)}
		item[AttrCacheReadTokens] = &types.AttributeValueMemberN{Value: strconv.Itoa(run.Tokens.CacheReadTokens)}
		item[AttrTurns] = &types.AttributeValueMemberN{Value: strconv.Itoa(run.Tokens.Turns)}
	}
	if run.Metadata != nil {
		metaMap := make(map[string]types.AttributeValue, len(run.Metadata))
		for k, v := range run.Metadata {
			metaMap[k] = &types.AttributeValueMemberS{Value: v}
		}
		item[AttrMetadata] = &types.AttributeValueMemberM{Value: metaMap}
	}
	if run.Labels != nil {
		labelMap := make(map[string]types.AttributeValue, len(run.Labels))
		for k, v := range run.Labels {
			labelMap[k] = &types.AttributeValueMemberS{Value: v}
		}
		item[AttrLabels] = &types.AttributeValueMemberM{Value: labelMap}
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

	// instance_id is optional at creation time and populated by UpdateRun
	// once the provider returns the real instance ID. Absence is not an error.
	if raw, present := item[AttrInstanceID]; present {
		instanceIDAttr, ok := raw.(*types.AttributeValueMemberS)
		if !ok {
			return nil, fmt.Errorf("parsing run %q: invalid %q attribute type", id, AttrInstanceID)
		}
		run.InstanceID = instanceIDAttr.Value
	}

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

	if av, ok := item[AttrPriority]; ok {
		sv, ok := av.(*types.AttributeValueMemberS)
		if !ok {
			return nil, fmt.Errorf("parsing run %q: invalid %q attribute", id, AttrPriority)
		}
		run.Priority = Priority(sv.Value)
	}

	if av, ok := item[AttrEnqueuedAt]; ok {
		sv, ok := av.(*types.AttributeValueMemberS)
		if !ok {
			return nil, fmt.Errorf("parsing run %q: invalid %q attribute", id, AttrEnqueuedAt)
		}
		t, err := time.Parse(time.RFC3339, sv.Value)
		if err != nil {
			return nil, fmt.Errorf("parsing run %q: parsing enqueued_at: %w", id, err)
		}
		run.EnqueuedAt = t
	}

	// Token counts: all five attributes are written together (or not at all)
	// by CreateRun/UpdateRun, but parse each defensively. Presence of ANY of
	// them yields a non-nil Tokens; a missing individual attribute reads as 0.
	parseTokenAttr := func(name string) (int, bool, error) {
		av, ok := item[name]
		if !ok {
			return 0, false, nil
		}
		nv, ok := av.(*types.AttributeValueMemberN)
		if !ok {
			return 0, false, fmt.Errorf("parsing run %q: invalid %q attribute", id, name)
		}
		v, err := strconv.Atoi(nv.Value)
		if err != nil {
			return 0, false, fmt.Errorf("parsing run %q: parsing %s: %w", id, name, err)
		}
		return v, true, nil
	}
	var tokens TokenUsage
	var anyToken bool
	for _, ta := range []struct {
		name string
		dst  *int
	}{
		{AttrInputTokens, &tokens.InputTokens},
		{AttrOutputTokens, &tokens.OutputTokens},
		{AttrCacheCreationTokens, &tokens.CacheCreationTokens},
		{AttrCacheReadTokens, &tokens.CacheReadTokens},
		{AttrTurns, &tokens.Turns},
	} {
		v, present, err := parseTokenAttr(ta.name)
		if err != nil {
			return nil, err
		}
		if present {
			anyToken = true
			*ta.dst = v
		}
	}
	if anyToken {
		run.Tokens = &tokens
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

	if av, ok := item[AttrLabels]; ok {
		mv, ok := av.(*types.AttributeValueMemberM)
		if !ok {
			return nil, fmt.Errorf("parsing run %q: invalid %q attribute", id, AttrLabels)
		}
		labels := make(map[string]string, len(mv.Value))
		for k, v := range mv.Value {
			sv, ok := v.(*types.AttributeValueMemberS)
			if !ok {
				return nil, fmt.Errorf("parsing run %q: labels[%q] is not a string", id, k)
			}
			labels[k] = sv.Value
		}
		run.Labels = labels
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
	run, err := parseRun(out.Item)
	if err != nil {
		return nil, fmt.Errorf("getting run %q: %w", id, err)
	}
	return run, nil
}

func (s *DynamoStore) UpdateRun(ctx context.Context, id string, update *RunUpdate) error {
	var setClauses []string
	exprAttrValues := map[string]types.AttributeValue{}
	exprAttrNames := map[string]string{}

	if update.Status != nil {
		setClauses = append(setClauses, "#st = :st")
		exprAttrNames["#st"] = "status"
		exprAttrValues[":st"] = &types.AttributeValueMemberS{Value: string(*update.Status)}
	}
	if update.InstanceID != nil {
		setClauses = append(setClauses, "instance_id = :iid")
		exprAttrValues[":iid"] = &types.AttributeValueMemberS{Value: *update.InstanceID}
	}
	if update.Metadata != nil {
		metaMap := make(map[string]types.AttributeValue, len(update.Metadata))
		for k, v := range update.Metadata {
			metaMap[k] = &types.AttributeValueMemberS{Value: v}
		}
		setClauses = append(setClauses, "metadata = :meta")
		exprAttrValues[":meta"] = &types.AttributeValueMemberM{Value: metaMap}
	}
	if update.ExitCode != nil {
		setClauses = append(setClauses, "exit_code = :ec")
		exprAttrValues[":ec"] = &types.AttributeValueMemberN{Value: strconv.Itoa(*update.ExitCode)}
	}
	if update.CompletedAt != nil {
		setClauses = append(setClauses, "completed_at = :ca")
		exprAttrValues[":ca"] = &types.AttributeValueMemberS{Value: update.CompletedAt.UTC().Format(time.RFC3339)}
	}
	if update.TotalCostUSD != nil {
		setClauses = append(setClauses, "total_cost_usd = :cost")
		exprAttrValues[":cost"] = &types.AttributeValueMemberN{Value: strconv.FormatFloat(*update.TotalCostUSD, 'f', -1, 64)}
	}
	if update.Tokens != nil {
		setClauses = append(setClauses,
			"input_tokens = :it",
			"output_tokens = :ot",
			"cache_creation_tokens = :cct",
			"cache_read_tokens = :crt",
			"turns = :tn",
		)
		exprAttrValues[":it"] = &types.AttributeValueMemberN{Value: strconv.Itoa(update.Tokens.InputTokens)}
		exprAttrValues[":ot"] = &types.AttributeValueMemberN{Value: strconv.Itoa(update.Tokens.OutputTokens)}
		exprAttrValues[":cct"] = &types.AttributeValueMemberN{Value: strconv.Itoa(update.Tokens.CacheCreationTokens)}
		exprAttrValues[":crt"] = &types.AttributeValueMemberN{Value: strconv.Itoa(update.Tokens.CacheReadTokens)}
		exprAttrValues[":tn"] = &types.AttributeValueMemberN{Value: strconv.Itoa(update.Tokens.Turns)}
	}
	if update.TimeoutAt != nil {
		setClauses = append(setClauses, "timeout_at = :ta")
		exprAttrValues[":ta"] = &types.AttributeValueMemberS{Value: update.TimeoutAt.UTC().Format(time.RFC3339)}
	}
	if update.Priority != nil {
		// "priority" is a DynamoDB reserved word; alias it.
		setClauses = append(setClauses, "#prio = :prio")
		exprAttrNames["#prio"] = AttrPriority
		exprAttrValues[":prio"] = &types.AttributeValueMemberS{Value: string(*update.Priority)}
	}

	if len(setClauses) == 0 {
		out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(s.tableName),
			Key: map[string]types.AttributeValue{
				AttrID: &types.AttributeValueMemberS{Value: id},
			},
		})
		if err != nil {
			return fmt.Errorf("checking run %q exists: %w", id, err)
		}
		if out.Item == nil {
			return fmt.Errorf("%w: %s", ErrRunNotFound, id)
		}
		return nil
	}

	input := &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			AttrID: &types.AttributeValueMemberS{Value: id},
		},
		UpdateExpression:          aws.String("SET " + strings.Join(setClauses, ", ")),
		ExpressionAttributeValues: exprAttrValues,
		ConditionExpression:       aws.String("attribute_exists(id)"),
	}
	if len(exprAttrNames) > 0 {
		input.ExpressionAttributeNames = exprAttrNames
	}

	_, err := s.client.UpdateItem(ctx, input)
	if err != nil {
		var ccfe *types.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return fmt.Errorf("%w: %s", ErrRunNotFound, id)
		}
		return fmt.Errorf("updating run %q: %w", id, err)
	}
	return nil
}

// ListRuns queries the by-repo GSI and pushes filtering server-side via a
// FilterExpression so that at scale (often many thousands of rows per repo)
// the query stays fast and the wire payload small. The started_at range goes
// in the KeyConditionExpression (sort-key range on the index); status set,
// workflow, ticket, and each label pair go in the FilterExpression, all
// AND-combined. The filter semantics mirror the shared matchesFilter helper;
// the conformance suite runs identical cases against both stores.
func (s *DynamoStore) ListRuns(ctx context.Context, filter RunFilter) ([]*Run, error) {
	names := map[string]string{"#repo": AttrRepo}
	values := map[string]types.AttributeValue{
		":repo": &types.AttributeValueMemberS{Value: filter.Repo},
	}

	// KeyConditionExpression: repo partition + optional started_at range.
	keyCond := "#repo = :repo"
	names["#started"] = AttrStartedAt
	switch {
	case filter.Since != nil && filter.Until != nil:
		keyCond += " AND #started BETWEEN :since AND :until"
		values[":since"] = &types.AttributeValueMemberS{Value: filter.Since.UTC().Format(time.RFC3339)}
		values[":until"] = &types.AttributeValueMemberS{Value: filter.Until.UTC().Format(time.RFC3339)}
	case filter.Since != nil:
		keyCond += " AND #started >= :since"
		values[":since"] = &types.AttributeValueMemberS{Value: filter.Since.UTC().Format(time.RFC3339)}
	case filter.Until != nil:
		keyCond += " AND #started <= :until"
		values[":until"] = &types.AttributeValueMemberS{Value: filter.Until.UTC().Format(time.RFC3339)}
	default:
		delete(names, "#started")
	}

	// FilterExpression: status set, workflow, ticket, labels (all AND).
	var filters []string

	if len(filter.Statuses) > 0 {
		names["#st"] = AttrStatus
		var placeholders []string
		for i, st := range filter.Statuses {
			ph := fmt.Sprintf(":st%d", i)
			placeholders = append(placeholders, ph)
			values[ph] = &types.AttributeValueMemberS{Value: string(st)}
		}
		filters = append(filters, fmt.Sprintf("#st IN (%s)", strings.Join(placeholders, ", ")))
	}
	if filter.Workflow != "" {
		names["#wf"] = AttrWorkflow
		values[":wf"] = &types.AttributeValueMemberS{Value: filter.Workflow}
		filters = append(filters, "#wf = :wf")
	}
	if filter.Ticket != "" {
		names["#tk"] = AttrTicket
		values[":tk"] = &types.AttributeValueMemberS{Value: filter.Ticket}
		filters = append(filters, "#tk = :tk")
	}
	if len(filter.Labels) > 0 {
		names["#labels"] = AttrLabels
		// Deterministic ordering so the expression is stable across calls.
		keys := make([]string, 0, len(filter.Labels))
		for k := range filter.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			nameKey := fmt.Sprintf("#lk%d", i)
			valKey := fmt.Sprintf(":lv%d", i)
			names[nameKey] = k
			values[valKey] = &types.AttributeValueMemberS{Value: filter.Labels[k]}
			filters = append(filters, fmt.Sprintf("#labels.%s = %s", nameKey, valKey))
		}
	}

	input := &dynamodb.QueryInput{
		TableName:                 aws.String(s.tableName),
		IndexName:                 aws.String(GSIByRepo),
		KeyConditionExpression:    aws.String(keyCond),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
		ScanIndexForward:          aws.Bool(false),
	}
	if len(filters) > 0 {
		input.FilterExpression = aws.String(strings.Join(filters, " AND "))
	}

	runs := make([]*Run, 0)
	for {
		out, err := s.client.Query(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("listing runs: %w", err)
		}
		for _, item := range out.Items {
			run, err := parseRun(item)
			if err != nil {
				return nil, fmt.Errorf("listing runs: %w", err)
			}
			runs = append(runs, run)
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		input.ExclusiveStartKey = out.LastEvaluatedKey
	}
	return runs, nil
}

func (s *DynamoStore) ListByRepo(ctx context.Context, repo string, activeOnly bool) ([]*Run, error) {
	input := &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String(GSIByRepo),
		KeyConditionExpression: aws.String("#repo = :repo"),
		ExpressionAttributeNames: map[string]string{
			"#repo": AttrRepo,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":repo": &types.AttributeValueMemberS{Value: repo},
		},
		ScanIndexForward: aws.Bool(false),
	}

	if activeOnly {
		input.FilterExpression = aws.String("#st IN (:pending, :running)")
		input.ExpressionAttributeNames["#st"] = AttrStatus
		input.ExpressionAttributeValues[":pending"] = &types.AttributeValueMemberS{Value: string(StatusPending)}
		input.ExpressionAttributeValues[":running"] = &types.AttributeValueMemberS{Value: string(StatusRunning)}
	}

	runs := make([]*Run, 0)
	for {
		out, err := s.client.Query(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("listing runs by repo: %w", err)
		}
		for _, item := range out.Items {
			run, err := parseRun(item)
			if err != nil {
				return nil, fmt.Errorf("listing runs by repo: %w", err)
			}
			runs = append(runs, run)
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		input.ExclusiveStartKey = out.LastEvaluatedKey
	}
	return runs, nil
}

func (s *DynamoStore) FindActiveByTicket(ctx context.Context, repo, ticket, workflow string) ([]*Run, error) {
	// "workflow" is a DynamoDB reserved word; alias it. Active = a slot is or is
	// about to be consumed: pending, running, OR queued (a queued run for the
	// same (ticket, workflow) must block a duplicate enqueue).
	input := &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String(GSIByTicket),
		KeyConditionExpression: aws.String("#ticket = :ticket"),
		FilterExpression:       aws.String("#repo = :repo AND #wf = :wf AND #st IN (:pending, :running, :queued)"),
		ExpressionAttributeNames: map[string]string{
			"#ticket": AttrTicket,
			"#repo":   AttrRepo,
			"#wf":     AttrWorkflow,
			"#st":     AttrStatus,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":ticket":  &types.AttributeValueMemberS{Value: ticket},
			":repo":    &types.AttributeValueMemberS{Value: repo},
			":wf":      &types.AttributeValueMemberS{Value: workflow},
			":pending": &types.AttributeValueMemberS{Value: string(StatusPending)},
			":running": &types.AttributeValueMemberS{Value: string(StatusRunning)},
			":queued":  &types.AttributeValueMemberS{Value: string(StatusQueued)},
		},
		ScanIndexForward: aws.Bool(false),
	}

	runs := make([]*Run, 0)
	for {
		out, err := s.client.Query(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("finding active runs by ticket: %w", err)
		}
		for _, item := range out.Items {
			run, err := parseRun(item)
			if err != nil {
				return nil, fmt.Errorf("finding active runs by ticket: %w", err)
			}
			runs = append(runs, run)
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		input.ExclusiveStartKey = out.LastEvaluatedKey
	}
	return runs, nil
}

func (s *DynamoStore) CountActive(ctx context.Context) (int, error) {
	total := 0
	for _, status := range []Status{StatusPending, StatusRunning} {
		input := &dynamodb.QueryInput{
			TableName:              aws.String(s.tableName),
			IndexName:              aws.String(GSIByStatus),
			KeyConditionExpression: aws.String("#st = :st"),
			ExpressionAttributeNames: map[string]string{
				"#st": AttrStatus,
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":st": &types.AttributeValueMemberS{Value: string(status)},
			},
			Select: types.SelectCount,
		}
		for {
			out, err := s.client.Query(ctx, input)
			if err != nil {
				return 0, fmt.Errorf("counting active runs: %w", err)
			}
			total += int(out.Count)
			if out.LastEvaluatedKey == nil {
				break
			}
			input.ExclusiveStartKey = out.LastEvaluatedKey
		}
	}
	return total, nil
}

func (s *DynamoStore) ListActive(ctx context.Context) ([]*Run, error) {
	runs := make([]*Run, 0)
	for _, status := range []Status{StatusPending, StatusRunning} {
		input := &dynamodb.QueryInput{
			TableName:              aws.String(s.tableName),
			IndexName:              aws.String(GSIByStatus),
			KeyConditionExpression: aws.String("#st = :st"),
			ExpressionAttributeNames: map[string]string{
				"#st": AttrStatus,
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":st": &types.AttributeValueMemberS{Value: string(status)},
			},
			ScanIndexForward: aws.Bool(false),
		}
		for {
			out, err := s.client.Query(ctx, input)
			if err != nil {
				return nil, fmt.Errorf("listing active runs: %w", err)
			}
			for _, item := range out.Items {
				run, err := parseRun(item)
				if err != nil {
					return nil, fmt.Errorf("listing active runs: %w", err)
				}
				runs = append(runs, run)
			}
			if out.LastEvaluatedKey == nil {
				break
			}
			input.ExclusiveStartKey = out.LastEvaluatedKey
		}
	}
	// Each per-status query is sorted by started_at desc within its block,
	// but the Store contract is global descending across statuses. Sort
	// once after concatenation so callers see the same order as SQLite.
	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})
	return runs, nil
}
