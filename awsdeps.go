package horde

// AWS SDK v2 dependencies for v0.2 (ECS, DynamoDB, SSM, S3, CloudWatch, STS).
// Blank imports pin these as direct dependencies in go.mod.
// Remove this file once each package is imported by actual implementation code.

import (
	_ "github.com/aws/aws-sdk-go-v2/aws"
	_ "github.com/aws/aws-sdk-go-v2/config"
	_ "github.com/aws/aws-sdk-go-v2/credentials"
	_ "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	_ "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	_ "github.com/aws/aws-sdk-go-v2/service/ecs"
	_ "github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/aws/aws-sdk-go-v2/service/ssm"
	_ "github.com/aws/aws-sdk-go-v2/service/sts"
)
