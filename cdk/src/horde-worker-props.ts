import type { Duration } from "aws-cdk-lib";
import type { IVpc } from "aws-cdk-lib/aws-ec2";
import type { ContainerDefinitionOptions, ContainerImage } from "aws-cdk-lib/aws-ecs";
import type { IRepository } from "aws-cdk-lib/aws-ecr";
import type { IBucket } from "aws-cdk-lib/aws-s3";
import type { ISecret } from "aws-cdk-lib/aws-secretsmanager";

/** Network posture for worker tasks. See `HordeWorkerProps.networkMode`. */
export type HordeNetworkMode = "public" | "private";

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
   * Canonical repository identifier for runs in this deployment, in
   * `host/path` form (no scheme, no userinfo, no trailing slash). Examples:
   * `github.com/example/myproj`, `gitlab.com/team/svc`. Whatever you set
   * here becomes the canonical value: every horde CLI invocation against
   * this stack reads it from SSM and uses it verbatim when writing or
   * querying run records on the by-repo GSI, so all run history shares one
   * bucket regardless of how individual launchers' git remotes are configured
   * (e.g. with or without `.git` suffix, https vs ssh).
   */
  readonly repo: string;

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
   * @default — a new VPC. In `networkMode: 'public'` (the default) it has only
   *   public subnets and no NAT Gateway; in `'private'` it adds
   *   PRIVATE_WITH_EGRESS subnets and one NAT Gateway.
   */
  readonly vpc?: IVpc;

  /**
   * Network posture for the worker tasks.
   *
   * - `'public'` (default): tasks run in PUBLIC subnets with a public IPv4
   *   address and reach the internet directly via the Internet Gateway — no
   *   NAT Gateway is created (~$32/mo saved). The worker security group has no
   *   ingress rules, so the public IP enables egress only; nothing on the
   *   internet can open a connection to the task.
   * - `'private'`: tasks run in PRIVATE_WITH_EGRESS subnets with no public IP,
   *   reaching the internet through a NAT Gateway the construct creates. Choose
   *   this to limit the blast radius of a future ingress rule, or when workers
   *   must reach private VPC resources without an internet path.
   *
   * With a bring-your-own `vpc`, this selects which of that VPC's subnets to
   * use; a synth-time error is raised if the VPC has no subnets of the chosen
   * type. The default is `'public'` in all cases — supplying a VPC does not
   * change it.
   *
   * @default 'public'
   */
  readonly networkMode?: HordeNetworkMode;

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
   * Maximum automatic Spot resumes per run before it is left terminal
   * instead of resumed (loop guard). Read by the status Lambda as MAX_RESUMES.
   * @default 5
   */
  readonly maxSpotResumes?: number;

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

  /**
   * Extra containers added to the worker's Fargate task definition. They share
   * the task's network namespace — so the worker reaches them on `localhost` —
   * and the task's lifecycle, starting and stopping with it. Typical uses: a
   * Postgres/Redis the test suite hits on localhost, a headless
   * Playwright/Selenium server, or a Stripe/LocalStack mock.
   *
   * Each entry is passed to `taskDefinition.addContainer()` with two defaults
   * the construct fills in when you omit them (your explicit value always wins):
   *
   *  - `essential: false` — a crashing sidecar does NOT stop the run. Set
   *    `essential: true` for a dependency the worker cannot run without (e.g. a
   *    database), so the task fails fast if it can't start. The worker container
   *    is always `essential: true`; run status is derived from the worker's exit
   *    code, never a sidecar's, regardless of `essential`.
   *  - `logging` — routed to the worker's CloudWatch log group with the
   *    sidecar's `containerName` as the stream prefix, so sidecar output is
   *    captured alongside the worker by default.
   *
   * `memoryMiB` sizes the whole task; sidecars share that ceiling unless you set
   * a per-container `memoryLimitMiB`. The container name `"horde-worker"` is
   * reserved for the worker and rejected at synth time.
   *
   * @default — none; a single-container task definition.
   */
  readonly sidecars?: ContainerDefinitionOptions[];

  /**
   * Maximum realized spend (USD) over `spendWindow` before the queue drain is
   * held. Realized-only: in-flight runs are uncosted until they finish, so a
   * burst can overshoot before any report cost — the `maxConcurrent` limit is
   * the blast-radius backstop (live enforcement is tracked separately). Written
   * to SSM `max_spend_per_window` and read by both the drain Lambda and the
   * lazy CLI drain. Omit to disable the spend cap (concurrency-only gating).
   *
   * @default — none; no spend cap.
   */
  readonly maxSpendPerWindow?: number;

  /**
   * Trailing window for `maxSpendPerWindow`. Written to SSM `spend_window` as a
   * Go duration string (e.g. "24h"). Ignored unless `maxSpendPerWindow` is set.
   *
   * @default — Duration.hours(24) when maxSpendPerWindow is set.
   */
  readonly spendWindow?: Duration;
}
