import * as cdk from "aws-cdk-lib";
import * as ec2 from "aws-cdk-lib/aws-ec2";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as iam from "aws-cdk-lib/aws-iam";
import * as logs from "aws-cdk-lib/aws-logs";
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
  }
}
