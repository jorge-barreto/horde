# @horde/cdk

AWS CDK construct that provisions the infrastructure [horde](https://github.com/jorge-barreto/horde) needs to run worker tasks on ECS Fargate: VPC, cluster, task definition, DynamoDB runs table with GSIs, S3 artifacts bucket, SSM config parameter, EventBridge rule, status-sync Lambda, scoped IAM, and a managed policy for CLI users.

## Install

```bash
npm install @horde/cdk aws-cdk-lib constructs
```

`aws-cdk-lib` (^2) and `constructs` (^10) are peer dependencies — install them in your CDK app.

## Usage

```ts
import { App, Stack } from "aws-cdk-lib";
import * as ecr from "aws-cdk-lib/aws-ecr";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as secretsmanager from "aws-cdk-lib/aws-secretsmanager";
import { HordeWorker } from "@horde/cdk";

const app = new App();
const stack = new Stack(app, "HordeStack");

const repo = new ecr.Repository(stack, "WorkerImage", {
  repositoryName: "horde-my-org-my-repo",
});

new HordeWorker(stack, "Horde", {
  projectSlug: "my-org-my-repo",
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
