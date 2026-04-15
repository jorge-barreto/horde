package store

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Attribute name constants for DynamoDB items.
const (
	AttrID              = "id"
	AttrRepo            = "repo"
	AttrTicket          = "ticket"
	AttrBranch          = "branch"
	AttrWorkflow        = "workflow"
	AttrProvider        = "provider"
	AttrInstanceID      = "instance_id"
	AttrStatus          = "status"
	AttrExitCode        = "exit_code"
	AttrLaunchedBy      = "launched_by"
	AttrStartedAt       = "started_at"
	AttrCompletedAt     = "completed_at"
	AttrTimeoutAt       = "timeout_at"
	AttrTotalCostUSD    = "total_cost_usd"
	AttrClusterARN      = "cluster_arn"
	AttrLogGroup        = "log_group"
	AttrArtifactsBucket = "artifacts_bucket"
	AttrArtifactsURI    = "artifacts_uri"
	AttrTTL             = "ttl"
)

// GSI name constants.
const (
	GSIByRepo     = "by-repo"
	GSIByTicket   = "by-ticket"
	GSIByStatus   = "by-status"
	GSIByInstance = "by-instance"
)

// TableKeySchema is the primary key schema for the runs table.
var TableKeySchema = []types.KeySchemaElement{
	{AttributeName: aws.String(AttrID), KeyType: types.KeyTypeHash},
}

// AttributeDefinitions lists only the attributes used in table or GSI key schemas.
// DynamoDB rejects definitions for attributes not used in any key schema.
var AttributeDefinitions = []types.AttributeDefinition{
	{AttributeName: aws.String(AttrID), AttributeType: types.ScalarAttributeTypeS},
	{AttributeName: aws.String(AttrRepo), AttributeType: types.ScalarAttributeTypeS},
	{AttributeName: aws.String(AttrTicket), AttributeType: types.ScalarAttributeTypeS},
	{AttributeName: aws.String(AttrStatus), AttributeType: types.ScalarAttributeTypeS},
	{AttributeName: aws.String(AttrInstanceID), AttributeType: types.ScalarAttributeTypeS},
	{AttributeName: aws.String(AttrStartedAt), AttributeType: types.ScalarAttributeTypeS},
}

// GlobalSecondaryIndexes defines all GSIs for the runs table.
var GlobalSecondaryIndexes = []types.GlobalSecondaryIndex{
	{
		IndexName: aws.String(GSIByRepo),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String(AttrRepo), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String(AttrStartedAt), KeyType: types.KeyTypeRange},
		},
		Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
	},
	{
		IndexName: aws.String(GSIByTicket),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String(AttrTicket), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String(AttrStartedAt), KeyType: types.KeyTypeRange},
		},
		Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
	},
	{
		IndexName: aws.String(GSIByStatus),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String(AttrStatus), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String(AttrStartedAt), KeyType: types.KeyTypeRange},
		},
		Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
	},
	{
		IndexName: aws.String(GSIByInstance),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String(AttrInstanceID), KeyType: types.KeyTypeHash},
			// no sort key — one task ARN maps to at most one run
		},
		Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
	},
}
