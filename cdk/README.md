# @horde/cdk

AWS CDK construct that provisions the infrastructure [horde](https://github.com/jorge-barreto/horde) needs to run worker tasks on ECS Fargate: VPC, cluster, task definition, DynamoDB runs table with GSIs, S3 artifacts bucket, SSM config parameter, EventBridge rule, status-sync Lambda, scoped IAM, and a managed policy for CLI users.

> Status: scaffold only. The `HordeWorker` construct lands in subsequent commits. See `horde` epic 5fh.

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
npm run build
npm test
```
