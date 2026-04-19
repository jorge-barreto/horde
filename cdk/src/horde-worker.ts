import * as cdk from "aws-cdk-lib";
import * as dynamodb from "aws-cdk-lib/aws-dynamodb";
import * as ec2 from "aws-cdk-lib/aws-ec2";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as iam from "aws-cdk-lib/aws-iam";
import * as logs from "aws-cdk-lib/aws-logs";
import * as s3 from "aws-cdk-lib/aws-s3";
import * as ssm from "aws-cdk-lib/aws-ssm";
import { Construct } from "constructs";

import type { HordeWorkerProps } from "./horde-worker-props";

const RETENTION_DAYS: ReadonlyMap<number, logs.RetentionDays> = new Map([
  [1, logs.RetentionDays.ONE_DAY],
  [3, logs.RetentionDays.THREE_DAYS],
  [5, logs.RetentionDays.FIVE_DAYS],
  [7, logs.RetentionDays.ONE_WEEK],
  [14, logs.RetentionDays.TWO_WEEKS],
  [30, logs.RetentionDays.ONE_MONTH],
  [60, logs.RetentionDays.TWO_MONTHS],
  [90, logs.RetentionDays.THREE_MONTHS],
  [120, logs.RetentionDays.FOUR_MONTHS],
  [150, logs.RetentionDays.FIVE_MONTHS],
  [180, logs.RetentionDays.SIX_MONTHS],
  [365, logs.RetentionDays.ONE_YEAR],
  [400, logs.RetentionDays.THIRTEEN_MONTHS],
  [545, logs.RetentionDays.EIGHTEEN_MONTHS],
  [731, logs.RetentionDays.TWO_YEARS],
  [1096, logs.RetentionDays.THREE_YEARS],
  [1827, logs.RetentionDays.FIVE_YEARS],
  [2192, logs.RetentionDays.SIX_YEARS],
  [2557, logs.RetentionDays.SEVEN_YEARS],
  [2922, logs.RetentionDays.EIGHT_YEARS],
  [3288, logs.RetentionDays.NINE_YEARS],
  [3653, logs.RetentionDays.TEN_YEARS],
]);

function toRetention(days: number): logs.RetentionDays {
  const value = RETENTION_DAYS.get(days);
  if (value === undefined) {
    throw new Error(
      `HordeWorker: logRetentionDays=${days} is not a CloudWatch-Logs supported value. ` +
        `Use one of: ${[...RETENTION_DAYS.keys()].join(", ")}.`,
    );
  }
  return value;
}

/**
 * `HordeWorker` provisions the AWS infrastructure horde needs to run worker
 * tasks on ECS Fargate.
 *
 * Built up across beads 5fh.3 -> 5fh.14. Each bead extends the construct
 * without changing the IDs of the resources created here, so that the
 * CloudFormation logical IDs stay stable across the series.
 *
 * Bead 5fh.3 contributes the skeleton: VPC, cluster, log group, placeholder
 * IAM roles, Fargate task definition, and the worker container. Subsequent
 * beads add secrets (5fh.4), tighten the IAM roles (5fh.5/6), wire
 * Dynamo/S3/SSM/EventBridge/Lambda and a CLI managed policy.
 *
 * Note on timeouts: ECS does not enforce a maximum task wall-clock; the
 * `stopTimeout` field on the container only controls SIGTERM->SIGKILL grace.
 * The per-run timeout from `defaultTimeoutMinutes` is enforced server-side by
 * the EventBridge-driven status Lambda introduced in 5fh.12/13.
 */
export class HordeWorker extends Construct {
  /** VPC the Fargate tasks run in. Either provided by the caller or created here. */
  public readonly vpc: ec2.IVpc;

  /** ECS cluster hosting the worker task definition. */
  public readonly cluster: ecs.Cluster;

  /** CloudWatch log group receiving the worker container's stdout/stderr. */
  public readonly logGroup: logs.LogGroup;

  /**
   * Task role assumed by the running container. Bead 5fh.3 creates this as a
   * minimal placeholder. Beads 5fh.5/7/8/10 attach scoped policies via
   * `this.taskRole.addToPolicy(...)`.
   */
  public readonly taskRole: iam.Role;

  /**
   * Task execution role used by the ECS agent to pull the image, write logs,
   * and resolve secrets at container start. Bead 5fh.6 layers the inline
   * `secretsmanager:GetSecretValue` policy on top.
   */
  public readonly executionRole: iam.Role;

  /** Fargate task definition for the worker container. */
  public readonly taskDefinition: ecs.FargateTaskDefinition;

  /** The worker container inside `taskDefinition`. */
  public readonly container: ecs.ContainerDefinition;

  /**
   * S3 bucket holding run artifacts (logs, run-result.json) under
   * `horde-runs/<runId>/`. Either provided by the caller or created here.
   */
  public readonly artifactsBucket: s3.IBucket;

  /**
   * DynamoDB table backing the run history. Partition key `id`; four GSIs
   * for repo/ticket/status/instance lookups. Consumed by the CLI's
   * `internal/store/dynamo.go` and by the status-sync Lambda.
   */
  public readonly runsTable: dynamodb.Table;

  /**
   * Egress-only security group attached to the worker task at `RunTask` time
   * by the CLI. The SG ID is published in SSM so the CLI can find it.
   */
  public readonly workerSecurityGroup: ec2.SecurityGroup;

  /**
   * SSM String parameter holding the JSON config blob the CLI reads at
   * startup. The shape matches `internal/config/ssm.go::HordeConfig`.
   */
  public readonly configParameter: ssm.StringParameter;

  /**
   * Managed policy that grants the horde CLI everything it needs to launch,
   * inspect, and terminate runs. Attach to whichever IAM principal your
   * developers/CI use (group, role, or user). Also exposed as CfnOutput
   * `CliUserManagedPolicyArn`.
   */
  public readonly cliUserPolicy: iam.ManagedPolicy;

  constructor(scope: Construct, id: string, props: HordeWorkerProps) {
    super(scope, id);

    const slug = props.projectSlug;
    const cpu = props.cpu ?? 1024;
    const memoryLimitMiB = props.memoryMiB ?? 4096;
    const retention = toRetention(props.logRetentionDays ?? 30);

    cdk.Tags.of(this).add("Name", `horde-${slug}`);

    if (props.vpc) {
      this.vpc = props.vpc;
    } else {
      const vpc = new ec2.Vpc(this, "Vpc", {
        maxAzs: 2,
        natGateways: 1,
        vpcName: `horde-${slug}-vpc`,
        subnetConfiguration: [
          { name: "public", subnetType: ec2.SubnetType.PUBLIC, cidrMask: 24 },
          {
            name: "private",
            subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS,
            cidrMask: 24,
          },
        ],
      });
      cdk.Tags.of(vpc).add("Name", `horde-${slug}-vpc`);
      this.vpc = vpc;
    }

    this.cluster = new ecs.Cluster(this, "Cluster", {
      vpc: this.vpc,
      clusterName: `horde-${slug}`,
    });
    cdk.Tags.of(this.cluster).add("Name", `horde-${slug}-cluster`);

    this.logGroup = new logs.LogGroup(this, "LogGroup", {
      logGroupName: `/ecs/horde-worker-${slug}`,
      retention,
      removalPolicy: cdk.RemovalPolicy.DESTROY,
    });

    this.taskRole = new iam.Role(this, "TaskRole", {
      assumedBy: new iam.ServicePrincipal("ecs-tasks.amazonaws.com"),
      roleName: `horde-${slug}-task-${cdk.Aws.REGION}`,
      description: `horde worker task role for ${slug}`,
    });
    cdk.Tags.of(this.taskRole).add("Name", `horde-${slug}-task-role`);

    this.executionRole = new iam.Role(this, "ExecutionRole", {
      assumedBy: new iam.ServicePrincipal("ecs-tasks.amazonaws.com"),
      roleName: `horde-${slug}-exec-${cdk.Aws.REGION}`,
      description: `horde worker task execution role for ${slug}`,
      managedPolicies: [
        iam.ManagedPolicy.fromAwsManagedPolicyName(
          "service-role/AmazonECSTaskExecutionRolePolicy",
        ),
      ],
    });
    cdk.Tags.of(this.executionRole).add("Name", `horde-${slug}-exec-role`);

    this.taskDefinition = new ecs.FargateTaskDefinition(this, "TaskDefinition", {
      family: `horde-${slug}`,
      cpu,
      memoryLimitMiB,
      taskRole: this.taskRole,
      executionRole: this.executionRole,
    });

    if (props.artifactsBucket) {
      this.artifactsBucket = props.artifactsBucket;
    } else {
      const bucket = new s3.Bucket(this, "ArtifactsBucket", {
        bucketName: `horde-artifacts-${slug}-${cdk.Aws.ACCOUNT_ID}`,
        encryption: s3.BucketEncryption.S3_MANAGED,
        blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
        enforceSSL: true,
        removalPolicy: cdk.RemovalPolicy.DESTROY,
        autoDeleteObjects: true,
      });
      cdk.Tags.of(bucket).add("Name", `horde-${slug}-artifacts`);
      this.artifactsBucket = bucket;
    }

    // Egress-only worker SG. allowAllOutbound:false then a single 443 rule.
    this.workerSecurityGroup = new ec2.SecurityGroup(this, "WorkerSecurityGroup", {
      vpc: this.vpc,
      description: "horde worker egress-only (443 outbound for git/api/ecr)",
      allowAllOutbound: false,
    });
    this.workerSecurityGroup.addEgressRule(
      ec2.Peer.anyIpv4(),
      ec2.Port.tcp(443),
      "https outbound",
    );
    cdk.Tags.of(this.workerSecurityGroup).add("Name", `horde-${slug}-worker-sg`);

    this.runsTable = new dynamodb.Table(this, "RunsTable", {
      tableName: `horde-runs-${slug}`,
      partitionKey: { name: "id", type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      removalPolicy: cdk.RemovalPolicy.DESTROY,
    });
    cdk.Tags.of(this.runsTable).add("Name", `horde-${slug}-runs`);
    this.runsTable.addGlobalSecondaryIndex({
      indexName: "by-repo",
      partitionKey: { name: "repo", type: dynamodb.AttributeType.STRING },
      sortKey: { name: "started_at", type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.ALL,
    });
    this.runsTable.addGlobalSecondaryIndex({
      indexName: "by-ticket",
      partitionKey: { name: "ticket", type: dynamodb.AttributeType.STRING },
      sortKey: { name: "started_at", type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.ALL,
    });
    this.runsTable.addGlobalSecondaryIndex({
      indexName: "by-status",
      partitionKey: { name: "status", type: dynamodb.AttributeType.STRING },
      sortKey: { name: "started_at", type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.ALL,
    });
    this.runsTable.addGlobalSecondaryIndex({
      indexName: "by-instance",
      partitionKey: { name: "instance_id", type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.ALL,
    });

    // Task role: write-only access to its own artifact prefix.
    // Reading happens via the CLI (with the cli-user managed policy).
    this.taskRole.addToPolicy(
      new iam.PolicyStatement({
        sid: "ArtifactsWrite",
        effect: iam.Effect.ALLOW,
        actions: ["s3:PutObject", "s3:AbortMultipartUpload"],
        resources: [`${this.artifactsBucket.bucketArn}/horde-runs/*`],
      }),
    );

    this.container = this.taskDefinition.addContainer("worker", {
      containerName: "horde-worker",
      image: props.workerImage,
      essential: true,
      stopTimeout: cdk.Duration.seconds(120),
      logging: ecs.LogDriver.awsLogs({
        logGroup: this.logGroup,
        streamPrefix: "ecs",
      }),
      secrets: {
        CLAUDE_CODE_OAUTH_TOKEN: ecs.Secret.fromSecretsManager(
          props.secrets.CLAUDE_CODE_OAUTH_TOKEN,
        ),
        GIT_TOKEN: ecs.Secret.fromSecretsManager(props.secrets.GIT_TOKEN),
      },
    });

    // SSM config parameter consumed by the horde CLI. JSON keys must match
    // `internal/config/ssm.go::HordeConfig` exactly.
    const ssmPath = props.ssmParameterPath ?? `/horde/${slug}/config`;
    const privateSubnetIds = this.vpc.selectSubnets({
      subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS,
    }).subnetIds;
    const maxConcurrent = props.maxConcurrent ?? 5;
    const defaultTimeoutMinutes = props.defaultTimeoutMinutes ?? 1440;

    const subnetsJson = cdk.Fn.join("", [
      "[",
      cdk.Fn.join(",", privateSubnetIds.map((id) => cdk.Fn.join("", ['"', id, '"']))),
      "]",
    ]);
    const configJson = cdk.Fn.join("", [
      '{"cluster_arn":"',
      this.cluster.clusterArn,
      '","task_definition_arn":"',
      this.taskDefinition.taskDefinitionArn,
      '","subnets":',
      subnetsJson,
      ',"security_group":"',
      this.workerSecurityGroup.securityGroupId,
      '","assign_public_ip":"DISABLED","log_group":"',
      this.logGroup.logGroupName,
      '","log_stream_prefix":"ecs","artifacts_bucket":"',
      this.artifactsBucket.bucketName,
      '","runs_table":"',
      this.runsTable.tableName,
      '","ecr_repo_uri":"',
      props.ecrRepository.repositoryUri,
      `","max_concurrent":${maxConcurrent},"default_timeout_minutes":${defaultTimeoutMinutes}}`,
    ]);

    this.configParameter = new ssm.StringParameter(this, "ConfigParameter", {
      parameterName: ssmPath,
      stringValue: configJson,
      description: `horde CLI configuration for project ${slug}`,
    });

    this.cliUserPolicy = new iam.ManagedPolicy(this, "CliUserPolicy", {
      managedPolicyName: `horde-${slug}-cli-${cdk.Aws.REGION}`,
      description: "Permissions required by the horde CLI to launch, inspect, and terminate runs",
      statements: [
        new iam.PolicyStatement({
          sid: "SsmRead",
          effect: iam.Effect.ALLOW,
          actions: ["ssm:GetParameter"],
          resources: [this.configParameter.parameterArn],
        }),
        new iam.PolicyStatement({
          sid: "EcsRun",
          effect: iam.Effect.ALLOW,
          actions: ["ecs:RunTask", "ecs:DescribeTasks", "ecs:StopTask", "ecs:ListTasks"],
          resources: ["*"],
          conditions: { ArnEquals: { "ecs:cluster": this.cluster.clusterArn } },
        }),
        new iam.PolicyStatement({
          sid: "EcsPassRole",
          effect: iam.Effect.ALLOW,
          actions: ["iam:PassRole"],
          resources: [this.taskRole.roleArn, this.executionRole.roleArn],
          conditions: { StringEquals: { "iam:PassedToService": "ecs-tasks.amazonaws.com" } },
        }),
        new iam.PolicyStatement({
          sid: "DynamoRunsTable",
          effect: iam.Effect.ALLOW,
          actions: [
            "dynamodb:PutItem",
            "dynamodb:GetItem",
            "dynamodb:UpdateItem",
            "dynamodb:DeleteItem",
            "dynamodb:Query",
            "dynamodb:Scan",
          ],
          resources: [this.runsTable.tableArn, `${this.runsTable.tableArn}/index/*`],
        }),
        new iam.PolicyStatement({
          sid: "LogsRead",
          effect: iam.Effect.ALLOW,
          actions: ["logs:GetLogEvents", "logs:FilterLogEvents", "logs:DescribeLogStreams"],
          resources: [this.logGroup.logGroupArn, `${this.logGroup.logGroupArn}:*`],
        }),
        new iam.PolicyStatement({
          sid: "ArtifactsRead",
          effect: iam.Effect.ALLOW,
          actions: ["s3:GetObject", "s3:ListBucket"],
          resources: [this.artifactsBucket.bucketArn, `${this.artifactsBucket.bucketArn}/*`],
        }),
      ],
    });

    new cdk.CfnOutput(this, "CliUserManagedPolicyArn", {
      value: this.cliUserPolicy.managedPolicyArn,
      description: "Attach this managed policy to IAM principals that run the horde CLI",
    });
  }
}
