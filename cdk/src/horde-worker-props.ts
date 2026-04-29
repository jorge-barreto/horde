import type { IVpc } from "aws-cdk-lib/aws-ec2";
import type { ContainerImage } from "aws-cdk-lib/aws-ecs";
import type { IRepository } from "aws-cdk-lib/aws-ecr";
import type { IBucket } from "aws-cdk-lib/aws-s3";
import type { ISecret } from "aws-cdk-lib/aws-secretsmanager";

/**
 * Secrets injected into the worker container at runtime via ECS Secrets
 * Manager `valueFrom` (resolved by the task execution role at container start
 * — never via plain env vars).
 *
 * The two canonical entries (`CLAUDE_CODE_OAUTH_TOKEN`, `GIT_TOKEN`) are
 * required and match the keys baked into the bootstrap CloudFormation
 * template. Extra entries are added via the index signature — each becomes
 * an additional task-definition `secrets:` entry with an IAM grant on
 * both the task role and the execution role. Caller must reference an
 * existing Secrets Manager secret (typically via
 * `secretsmanager.Secret.fromSecretNameV2(...)`).
 */
export interface HordeWorkerSecrets {
  /** Claude Code OAuth token. Becomes env `CLAUDE_CODE_OAUTH_TOKEN`. */
  readonly CLAUDE_CODE_OAUTH_TOKEN: ISecret;
  /** Git provider token (e.g. GitHub PAT). Becomes env `GIT_TOKEN`. */
  readonly GIT_TOKEN: ISecret;
  /** Additional caller-declared secrets — env-var name → ISecret. */
  readonly [envVarName: string]: ISecret;
}

/**
 * Configuration for the HordeWorker construct. Every optional field has a
 * documented default; required fields shape the public API surface and the
 * SSM config blob the CLI consumes.
 */
export interface HordeWorkerProps {
  /**
   * Project slug used to namespace every resource as
   * `horde-<projectSlug>-<resource>`. Also feeds the default
   * `ssmParameterPath`. Must be DNS-safe (lowercase, hyphens).
   */
  readonly projectSlug: string;

  /**
   * Container image to run as the worker task. Typically
   * `ecs.ContainerImage.fromEcrRepository(repo, "latest")`.
   */
  readonly workerImage: ContainerImage;

  /**
   * ECR repository backing `workerImage`. Its URI is written to SSM as
   * `ecr_repo_uri` so `horde push` knows where to push new image versions.
   * Must reference the same repo as `workerImage`.
   */
  readonly ecrRepository: IRepository;

  /** Secrets injected into the worker container at runtime. */
  readonly secrets: HordeWorkerSecrets;

  /**
   * VPC the Fargate tasks run in.
   * @default — a new VPC with 2 private + 2 public subnets is created.
   */
  readonly vpc?: IVpc;

  /**
   * S3 bucket used to store run artifacts under `horde-runs/<runId>/`.
   * @default — a new SSE-S3 bucket with secure-transport policy is created.
   */
  readonly artifactsBucket?: IBucket;

  /**
   * Fargate task CPU units (1024 = 1 vCPU).
   * @default 1024
   */
  readonly cpu?: number;

  /**
   * Fargate task memory in MiB.
   * @default 4096
   */
  readonly memoryMiB?: number;

  /**
   * Maximum simultaneous worker tasks. Enforced client-side by the CLI from
   * SSM `max_concurrent` — not an ECS service quota.
   * @default 5
   */
  readonly maxConcurrent?: number;

  /**
   * Default per-run timeout in minutes when the caller does not pass one.
   * Written to SSM `default_timeout_minutes`.
   * @default 1440 (24 h, matches SPEC.md and the bootstrap CF stack;
   *   the bead description's "60" is a typo).
   */
  readonly defaultTimeoutMinutes?: number;

  /**
   * CloudWatch Logs retention in days for `/ecs/horde-worker-<projectSlug>`.
   * @default 30
   */
  readonly logRetentionDays?: number;

  /**
   * Path of the SSM String parameter the construct writes config JSON to.
   * @default `/horde/<projectSlug>/config` — slug-namespaced to allow multiple
   *   horde stacks per AWS account (matches the bootstrap CF template at
   *   `.horde/cloudformation.yaml`). The Go consumer accepts any path the
   *   CLI is pointed at.
   */
  readonly ssmParameterPath?: string;
}
