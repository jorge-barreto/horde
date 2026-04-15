package store

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type dynamoAPI interface {
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
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
