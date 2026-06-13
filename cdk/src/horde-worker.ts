import * as cdk from "aws-cdk-lib";
import * as dynamodb from "aws-cdk-lib/aws-dynamodb";
import * as ec2 from "aws-cdk-lib/aws-ec2";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as iam from "aws-cdk-lib/aws-iam";
import * as logs from "aws-cdk-lib/aws-logs";
import * as s3 from "aws-cdk-lib/aws-s3";
import * as ssm from "aws-cdk-lib/aws-ssm";
import { Construct } from "constructs";
import * as events from "aws-cdk-lib/aws-events";
import * as targets from "aws-cdk-lib/aws-events-targets";
import * as lambda from "aws-cdk-lib/aws-lambda";
import * as path from "path";

import type { HordeWorkerProps } from "./horde-worker-props";

/**
 * Name of the worker container in the Fargate task definition. The status-sync
 * Lambda derives run status from THIS container's exit code (never a sidecar's),
 * and the horde CLI's ECS provider uses it to build the CloudWatch log-stream
 * path and target env overrides at launch. It is therefore a cross-language
 * contract — not a configurable prop. Three other places must stay equal to it:
 * the standalone copy in `./status-lambda/index.ts`, `const containerName` in
 * `internal/provider/ecs.go`, and the container `Name` in
 * `internal/bootstrap/templates/stack.yaml.tmpl`.
 */
export const WORKER_CONTAINER_NAME = "horde-worker";

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
   * Caller-defined sidecar containers added to `taskDefinition`, in declaration
   * order. Empty when `props.sidecars` is unset. See `HordeWorkerProps.sidecars`.
   */
  public readonly sidecarContainers: ecs.ContainerDefinition[];

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

  /** Lambda that reconciles run status from ECS Task State Change events. */
  public readonly statusLambda: lambda.Function;

  /** EventBridge rule routing STOPPED task events to `statusLambda`. */
  public readonly statusEventRule: events.Rule;

  /**
   * Custom EventBridge bus carrying horde run-lifecycle events (run.started /
   * run.terminal / run.cost-threshold-exceeded). The drain Lambda subscribes;
   * external consumers (notifications, your own automation) can add their own
   * rules. See `horde docs events`.
   */
  public readonly eventBus: events.EventBus;

  /** Lambda that drains the queue on run.terminal (capacity + spend gated). */
  public readonly drainLambda: lambda.Function;

  /** EventBridge rule routing run.terminal events to `drainLambda`. */
  public readonly drainEventRule: events.Rule;

  constructor(scope: Construct, id: string, props: HordeWorkerProps) {
    super(scope, id);

    const slug = props.projectSlug;
    const cpu = props.cpu ?? 1024;
    const memoryLimitMiB = props.memoryMiB ?? 4096;
    const retention = toRetention(props.logRetentionDays ?? 30);
    const networkMode = props.networkMode ?? "public";

    cdk.Tags.of(this).add("Name", `horde-${slug}`);

    if (props.vpc) {
      this.vpc = props.vpc;
    } else {
      const vpc = new ec2.Vpc(this, "Vpc", {
        maxAzs: 2,
        // 'public': no NAT, tasks egress directly via the IGW. 'private': one
        // NAT Gateway fronting PRIVATE_WITH_EGRESS subnets.
        natGateways: networkMode === "public" ? 0 : 1,
        vpcName: `horde-${slug}-vpc`,
        subnetConfiguration:
          networkMode === "public"
            ? [{ name: "public", subnetType: ec2.SubnetType.PUBLIC, cidrMask: 24 }]
            : [
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

    // Task role: read+write on its own artifact prefix, plus ListBucket on
    // the bucket. Read+List are needed because the worker entrypoint runs
    // `aws s3 sync s3://${ARTIFACTS_BUCKET}/horde-runs/${RUN_ID}/sessions/`
    // (docker/entrypoint.sh) to restore prior agent session state — the
    // sync calls ListObjectsV2 (s3:ListBucket) and GetObject. Mirrors the
    // task-role IAM in internal/bootstrap/templates/stack.yaml.tmpl so the
    // CDK and CloudFormation onboarding paths stay in lockstep.
    this.taskRole.addToPolicy(
      new iam.PolicyStatement({
        sid: "ArtifactsWrite",
        effect: iam.Effect.ALLOW,
        actions: ["s3:PutObject", "s3:AbortMultipartUpload", "s3:GetObject"],
        resources: [`${this.artifactsBucket.bucketArn}/horde-runs/*`],
      }),
    );
    this.taskRole.addToPolicy(
      new iam.PolicyStatement({
        sid: "ArtifactsList",
        effect: iam.Effect.ALLOW,
        actions: ["s3:ListBucket"],
        resources: [this.artifactsBucket.bucketArn],
      }),
    );

    // Wire every entry in props.secrets through ecs.Secret.fromSecretsManager
    // so the canonical pair plus any caller-declared extras flow into the
    // task definition's secrets array. Each ISecret must already exist or
    // be created by the caller; the construct does not create the
    // Secrets Manager entries itself.
    const taskDefSecrets: { [k: string]: ecs.Secret } = {};
    for (const envName of Object.keys(props.secrets)) {
      const isecret = props.secrets[envName];
      if (!isecret) {
        throw new Error(
          `HordeWorker: secrets["${envName}"] is undefined. ` +
            `Every entry must reference a SecretsManager ISecret.`,
        );
      }
      taskDefSecrets[envName] = ecs.Secret.fromSecretsManager(isecret);
    }
    this.container = this.taskDefinition.addContainer("worker", {
      containerName: WORKER_CONTAINER_NAME,
      image: props.workerImage,
      essential: true,
      stopTimeout: cdk.Duration.seconds(120),
      logging: ecs.LogDriver.awsLogs({
        logGroup: this.logGroup,
        streamPrefix: "ecs",
      }),
      secrets: taskDefSecrets,
    });

    // Sidecars are added AFTER the worker, so the worker is container index 0
    // (defensive — the status Lambda finds the worker by name, not by index).
    // Defaults: essential:false so a crashing sidecar doesn't stop the run, and
    // logging routed to the worker log group keyed by the sidecar name. Applied
    // with `??` (not spread order) so an explicit `essential: undefined` from a
    // caller can't fall through to CDK's own `essential: true` default and
    // silently flip a sidecar to essential / drop its logging.
    this.sidecarContainers = (props.sidecars ?? []).map((sidecar, i) => {
      if (sidecar.containerName === WORKER_CONTAINER_NAME) {
        throw new Error(
          `HordeWorker: sidecar containerName "${WORKER_CONTAINER_NAME}" is ` +
            `reserved for the worker container.`,
        );
      }
      const id = sidecar.containerName ?? `Sidecar${i}`;
      return this.taskDefinition.addContainer(id, {
        ...sidecar,
        essential: sidecar.essential ?? false,
        logging:
          sidecar.logging ??
          ecs.LogDriver.awsLogs({
            logGroup: this.logGroup,
            streamPrefix: sidecar.containerName ?? `sidecar${i}`,
          }),
      });
    });

    // SSM config parameter consumed by the horde CLI. JSON keys must match
    // `internal/config/ssm.go::HordeConfig` exactly.
    const ssmPath = props.ssmParameterPath ?? `/horde/${slug}/config`;
    const subnetType =
      networkMode === "public"
        ? ec2.SubnetType.PUBLIC
        : ec2.SubnetType.PRIVATE_WITH_EGRESS;
    const subnetTypeName =
      networkMode === "public" ? "PUBLIC" : "PRIVATE_WITH_EGRESS";
    const selectedSubnetIds = this.vpc.selectSubnets({ subnetType }).subnetIds;
    if (selectedSubnetIds.length === 0) {
      throw new Error(
        `HordeWorker networkMode '${networkMode}' requires ` +
          `ec2.SubnetType.${subnetTypeName} subnets in the VPC, but none were found.`,
      );
    }
    const assignPublicIp = networkMode === "public" ? "ENABLED" : "DISABLED";
    const maxConcurrent = props.maxConcurrent ?? 5;
    const defaultTimeoutMinutes = props.defaultTimeoutMinutes ?? 1440;

    // Custom EventBridge bus for run-lifecycle events. Created before the SSM
    // config so its name (a token) can be written into the CLI config JSON the
    // launch path / lazy drain read to emit run.started.
    this.eventBus = new events.EventBus(this, "RunEventBus", {
      eventBusName: `horde-${slug}`,
    });

    // Spend cap: realized-only (see HordeWorkerProps.maxSpendPerWindow). The
    // window defaults to 24h when a cap is set. Surfaced to SSM config + the
    // drain Lambda env; absent ⇒ concurrency-only gating.
    const spendWindowSeconds =
      props.maxSpendPerWindow !== undefined
        ? (props.spendWindow ?? cdk.Duration.hours(24)).toSeconds()
        : undefined;
    // Go consumers parse spend_window as a duration string; emit "<N>s".
    const spendConfigJSON =
      props.maxSpendPerWindow !== undefined
        ? `,"max_spend_per_window":${props.maxSpendPerWindow},"spend_window":"${spendWindowSeconds}s"`
        : "";

    const subnetsJson = cdk.Fn.join("", [
      "[",
      cdk.Fn.join(",", selectedSubnetIds.map((id) => cdk.Fn.join("", ['"', id, '"']))),
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
      '","assign_public_ip":"',
      assignPublicIp,
      '","log_group":"',
      this.logGroup.logGroupName,
      '","log_stream_prefix":"ecs","artifacts_bucket":"',
      this.artifactsBucket.bucketName,
      '","runs_table":"',
      this.runsTable.tableName,
      '","ecr_repo_uri":"',
      props.ecrRepository.repositoryUri,
      '","event_bus_name":"',
      this.eventBus.eventBusName,
      `","repo":"${props.repo}","max_concurrent":${maxConcurrent},"default_timeout_minutes":${defaultTimeoutMinutes}${spendConfigJSON}}`,
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

    // The Lambda is pre-bundled at package build time (see package.json
    // `build` script: esbuild --bundle ... --outfile=lib/status-lambda/bundle.js).
    // Shipping a bundled .js — rather than a NodejsFunction that esbuilds at
    // consumer synth time — means consumers don't need Docker or a local
    // esbuild install, and synth doesn't probe the consumer's package-lock.
    this.statusLambda = new lambda.Function(this, "StatusLambda", {
      functionName: `horde-${slug}-status-updater`,
      code: lambda.Code.fromAsset(path.join(__dirname, "status-lambda")),
      handler: "bundle.handler",
      runtime: lambda.Runtime.NODEJS_20_X,
      timeout: cdk.Duration.seconds(30),
      memorySize: 256,
      environment: {
        RUNS_TABLE: this.runsTable.tableName,
        ARTIFACTS_BUCKET: this.artifactsBucket.bucketName,
        EVENT_BUS_NAME: this.eventBus.eventBusName,
      },
      logGroup: new logs.LogGroup(this, "StatusLambdaLogGroup", {
        logGroupName: `/aws/lambda/horde-${slug}-status-updater`,
        retention,
        removalPolicy: cdk.RemovalPolicy.DESTROY,
      }),
    });
    cdk.Tags.of(this.statusLambda).add("Name", `horde-${slug}-status-lambda`);
    // The status Lambda emits run.terminal after its authoritative write.
    this.eventBus.grantPutEventsTo(this.statusLambda);

    this.statusLambda.addToRolePolicy(
      new iam.PolicyStatement({
        sid: "DynamoRunsTableRW",
        effect: iam.Effect.ALLOW,
        actions: ["dynamodb:GetItem", "dynamodb:UpdateItem", "dynamodb:Query"],
        resources: [
          this.runsTable.tableArn,
          `${this.runsTable.tableArn}/index/by-instance`,
        ],
      }),
    );
    this.statusLambda.addToRolePolicy(
      new iam.PolicyStatement({
        sid: "ArtifactsRead",
        effect: iam.Effect.ALLOW,
        // ListBucket needed because run-result.json lives under a
        // workflow/ticket-nested prefix not known at invoke time.
        actions: ["s3:GetObject", "s3:ListBucket"],
        resources: [
          this.artifactsBucket.bucketArn,
          `${this.artifactsBucket.bucketArn}/*`,
        ],
      }),
    );

    this.statusEventRule = new events.Rule(this, "StatusEventRule", {
      ruleName: `horde-${slug}-status`,
      description: `Route STOPPED ECS task events on the horde-${slug} cluster to the status lambda`,
      eventPattern: {
        source: ["aws.ecs"],
        detailType: ["ECS Task State Change"],
        detail: {
          clusterArn: [this.cluster.clusterArn],
          lastStatus: ["STOPPED"],
        },
      },
    });
    this.statusEventRule.addTarget(new targets.LambdaFunction(this.statusLambda));

    // --- Queue drain (issue #36) ---
    // The drain Lambda subscribes to run.terminal on the bus. On each event it
    // checks capacity + the realized spend cap, atomically claims the next
    // queued run (queued→pending), RunTasks it, and emits run.started. Bundled
    // standalone like the status Lambda (no consumer-side esbuild/Docker).
    const drainEnv: Record<string, string> = {
      RUNS_TABLE: this.runsTable.tableName,
      EVENT_BUS_NAME: this.eventBus.eventBusName,
      MAX_CONCURRENT: String(maxConcurrent),
      CLUSTER_ARN: this.cluster.clusterArn,
      TASK_DEF_ARN: this.taskDefinition.taskDefinitionArn,
      SUBNETS: cdk.Fn.join(",", selectedSubnetIds),
      SECURITY_GROUP: this.workerSecurityGroup.securityGroupId,
      ASSIGN_PUBLIC_IP: assignPublicIp, // matches the SSM config above
      ARTIFACTS_BUCKET: this.artifactsBucket.bucketName,
    };
    if (props.maxSpendPerWindow !== undefined) {
      drainEnv.MAX_SPEND_PER_WINDOW = String(props.maxSpendPerWindow);
      drainEnv.SPEND_WINDOW_SECONDS = String(spendWindowSeconds);
    }

    this.drainLambda = new lambda.Function(this, "DrainLambda", {
      functionName: `horde-${slug}-queue-drain`,
      code: lambda.Code.fromAsset(path.join(__dirname, "drain-lambda")),
      handler: "bundle.handler",
      runtime: lambda.Runtime.NODEJS_20_X,
      timeout: cdk.Duration.seconds(30),
      memorySize: 256,
      environment: drainEnv,
      logGroup: new logs.LogGroup(this, "DrainLambdaLogGroup", {
        logGroupName: `/aws/lambda/horde-${slug}-queue-drain`,
        retention,
        removalPolicy: cdk.RemovalPolicy.DESTROY,
      }),
    });
    cdk.Tags.of(this.drainLambda).add("Name", `horde-${slug}-drain-lambda`);

    this.drainLambda.addToRolePolicy(
      new iam.PolicyStatement({
        sid: "DynamoRunsTableRW",
        effect: iam.Effect.ALLOW,
        actions: ["dynamodb:GetItem", "dynamodb:UpdateItem", "dynamodb:Query"],
        resources: [this.runsTable.tableArn, `${this.runsTable.tableArn}/index/*`],
      }),
    );
    this.drainLambda.addToRolePolicy(
      new iam.PolicyStatement({
        sid: "EcsRunTask",
        effect: iam.Effect.ALLOW,
        actions: ["ecs:RunTask"],
        resources: ["*"],
        conditions: { ArnEquals: { "ecs:cluster": this.cluster.clusterArn } },
      }),
    );
    this.drainLambda.addToRolePolicy(
      new iam.PolicyStatement({
        sid: "EcsPassRole",
        effect: iam.Effect.ALLOW,
        actions: ["iam:PassRole"],
        resources: [this.taskRole.roleArn, this.executionRole.roleArn],
        conditions: { StringEquals: { "iam:PassedToService": "ecs-tasks.amazonaws.com" } },
      }),
    );
    this.eventBus.grantPutEventsTo(this.drainLambda);

    this.drainEventRule = new events.Rule(this, "DrainEventRule", {
      ruleName: `horde-${slug}-drain`,
      description: `Drain the horde-${slug} queue when a run reaches a terminal state`,
      eventBus: this.eventBus,
      eventPattern: {
        source: ["horde"],
        detailType: ["run.terminal"],
      },
    });
    this.drainEventRule.addTarget(new targets.LambdaFunction(this.drainLambda));

    new cdk.CfnOutput(this, "CliUserManagedPolicyArn", {
      value: this.cliUserPolicy.managedPolicyArn,
      description: "Attach this managed policy to IAM principals that run the horde CLI",
    });
  }
}
