# @horde.io/cdk

AWS CDK construct that provisions the infrastructure [horde](https://github.com/jorge-barreto/horde) needs to run worker tasks on ECS Fargate: VPC, cluster, task definition, DynamoDB runs table with GSIs, S3 artifacts bucket, SSM config parameter, EventBridge rule, status-sync Lambda, scoped IAM, and a managed policy for CLI users.

## Install

```bash
npm install @horde.io/cdk aws-cdk-lib constructs
```

`aws-cdk-lib` (^2) and `constructs` (^10) are peer dependencies — install them in your CDK app.

## Usage

```ts
import { App, Stack } from "aws-cdk-lib";
import * as ecr from "aws-cdk-lib/aws-ecr";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as secretsmanager from "aws-cdk-lib/aws-secretsmanager";
import { HordeWorker } from "@horde.io/cdk";

const app = new App();
const stack = new Stack(app, "HordeStack");

const repo = new ecr.Repository(stack, "WorkerImage", {
  repositoryName: "horde-my-org-my-repo",
});

new HordeWorker(stack, "Horde", {
  projectSlug: "my-org-my-repo",
  repo: "github.com/my-org/my-repo",
  workerImage: ecs.ContainerImage.fromEcrRepository(repo, "latest"),
  ecrRepository: repo,
  secrets: {
    CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(
      stack, "ClaudeToken", "horde/claude-code-oauth-token"),
    GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(
      stack, "GitToken", "horde/git-token"),
  },
});
```

`repo` is the canonical repository identifier in `host/path` form (no scheme,
no trailing slash). The horde CLI reads it from SSM and uses it verbatim when
writing or querying run records, so every CI runner and dev box launching
against this stack writes to the same `by-repo` bucket — no drift from local
git remote variations (with vs. without `.git`, https vs. ssh).

## Data durability & teardown

`HordeWorker` provisions two **data-bearing** resources — the DynamoDB runs
table (run history + GSIs) and the S3 artifacts bucket (per-run plans, test/
review output, logs). They default to `RemovalPolicy.RETAIN`, so a `cdk destroy`
(or a `cdk deploy` that replaces them) tears down compute, networking, and log
groups but **leaves your run history and artifacts behind**. The runs table also
has point-in-time recovery enabled by default (35-day continuous backup), which
guards accidental writes/deletes that RETAIN alone does not.

For an ephemeral or dev stack you want to fully clean up, opt into destruction:

```ts
import { RemovalPolicy } from "aws-cdk-lib";

new HordeWorker(stack, "Horde", {
  // ...required props...
  dataRemovalPolicy: RemovalPolicy.DESTROY, // table + bucket deleted on `cdk destroy`
  pointInTimeRecovery: false,               // optional: skip PITR for throwaway stacks
});
```

`dataRemovalPolicy: DESTROY` also enables the bucket's auto-delete so a non-empty
bucket is emptied before removal. Only `RETAIN` and `DESTROY` are accepted
(`SNAPSHOT` is rejected — S3 has no snapshot policy). A **caller-provided**
`artifactsBucket` is never re-policied by the construct; it keeps whatever policy
you set on it. Log groups are always removed on teardown regardless of this prop.

## Sidecar containers

Some projects need an auxiliary service alongside the worker for the duration of
a run — a test database, a headless browser server, a mock API. The `sidecars`
prop adds extra containers to the worker's Fargate task definition. They share
the task's network namespace, so the worker reaches them on `localhost`, and
they start and stop with it.

```ts
new HordeWorker(stack, "Horde", {
  // ...required props...
  memoryMiB: 4096, // task ceiling, shared across all containers
  sidecars: [
    {
      containerName: "postgres",
      image: ecs.ContainerImage.fromRegistry("postgres:16"),
      environment: { POSTGRES_PASSWORD: "dev" },
      memoryLimitMiB: 1024, // optional per-container cap
      // essential defaults to false — a crashing sidecar won't stop the run
    },
  ],
});
```

Each entry is a standard CDK
[`ContainerDefinitionOptions`](https://docs.aws.amazon.com/cdk/api/v2/docs/aws-cdk-lib.aws_ecs.ContainerDefinitionOptions.html),
so `healthCheck`, `portMappings`, `secrets`, `command`, and `dependsOn` are all
available. The construct fills in two defaults when you omit them (your explicit
value always wins):

- **`essential: false`** — a crashing sidecar does not fail the run. Set
  `essential: true` for a hard dependency (e.g. a database the worker can't run
  without), so the task fails fast if it can't start.
- **`logging`** — routed to the worker's CloudWatch log group with the
  sidecar's `containerName` as the stream prefix.

Run status always reflects the **worker** container's exit code, never a
sidecar's — even an essential sidecar that exits non-zero. `memoryMiB` sizes the
whole task; sidecars share that ceiling unless you set a per-container
`memoryLimitMiB`. The name `horde-worker` is reserved and rejected at synth time.

> Sidecars are a CDK-construct feature. The `horde bootstrap` (CloudFormation)
> path and the local `--provider docker` runner do not provision sidecars.

## Development

```bash
npm install
npm run build   # tsc + esbuild bundle for the status Lambda
npm test
```

The status Lambda is bundled at package build time (via `esbuild`) into
`lib/status-lambda/bundle.js` and shipped pre-compiled. Consumers do not
need Docker or a local `esbuild` install to synth — the construct uses
`lambda.Code.fromAsset` on the published bundle.

## Releasing

Bump the version in `package.json`, rebuild and test, then publish:

```bash
npm version patch    # or minor / major
npm publish          # prepublishOnly hook runs clean+build+test
```

`prepublishOnly` runs `npm run clean && npm run build && npm test` before
the tarball is uploaded, so a broken build never reaches the registry.
