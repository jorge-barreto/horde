#!/usr/bin/env node
// E2E smoke stack for @horde.io/cdk. Not for production. Deploys a real ECS
// stack under a dedicated slug so the existing TestECS_* harness pattern
// can verify the construct end-to-end.
//
// Secrets: CDK creates the two secret resources here (initially empty).
// The Go bring-up test populates them via PutSecretValue from the local
// .env BEFORE launching any task, so no token literal ever lands in the
// CFN template or template history.
import * as cdk from "aws-cdk-lib";
import * as ecr from "aws-cdk-lib/aws-ecr";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as secretsmanager from "aws-cdk-lib/aws-secretsmanager";
// Import the BUILT construct (lib/), not the TS source (src/). The construct
// stages its Lambda asset via `code.fromAsset(__dirname/status-lambda)`, and
// the esbuild `bundle.js` (the handler entry) only exists under lib/. Importing
// from ../src makes __dirname resolve to src/status-lambda under ts-node, which
// has no bundle.js — the deployed Lambda then fails with "Cannot find module
// 'bundle'". Run `npm run build` before the e2e bring-up. This also matches how
// real consumers load the published package.
import { HordeWorker } from "../lib";

// SLUG must match `bootstrap.Slug("https://github.com/jorge-barreto/horde-cdke2e.git")`.
// The Go test sets that fake remote so `horde push` resolves to this SSM path.
const SLUG = "jorge-barreto-horde-cdke2e";

const CLAUDE_SECRET_NAME = `horde-${SLUG}-claude-code-oauth-token`;
const GIT_SECRET_NAME = `horde-${SLUG}-git-token`;

const app = new cdk.App();
const stack = new cdk.Stack(app, "HordeCdkE2E", {
  stackName: `horde-${SLUG}`,
  env: {
    account: process.env.CDK_DEFAULT_ACCOUNT,
    region: process.env.CDK_DEFAULT_REGION ?? "us-east-1",
  },
  description: "E2E smoke stack for @horde.io/cdk. Not for production.",
});

const repo = new ecr.Repository(stack, "WorkerRepo", {
  repositoryName: `horde-${SLUG}`,
  emptyOnDelete: true,
  removalPolicy: cdk.RemovalPolicy.DESTROY,
  imageScanOnPush: true,
});

// Empty secret resources owned by the stack. The bring-up Go test calls
// PutSecretValue to populate them from the developer's local .env before
// any worker task is launched.
const claudeSecret = new secretsmanager.Secret(stack, "ClaudeToken", {
  secretName: CLAUDE_SECRET_NAME,
  description: "CLAUDE_CODE_OAUTH_TOKEN for the e2e worker (populated post-deploy by bring-up test)",
  removalPolicy: cdk.RemovalPolicy.DESTROY,
});
const gitSecret = new secretsmanager.Secret(stack, "GitToken", {
  secretName: GIT_SECRET_NAME,
  description: "GIT_TOKEN for the e2e worker (populated post-deploy by bring-up test)",
  removalPolicy: cdk.RemovalPolicy.DESTROY,
});

const worker = new HordeWorker(stack, "Worker", {
  projectSlug: SLUG,
  // Canonical repo for run records. The e2e harness sets each worker's git
  // remote to the real horde repo (so the worker can clone + run horde's own
  // .orc/workflows), so the canonical host/path form is this.
  repo: "github.com/jorge-barreto/horde",
  ecrRepository: repo,
  workerImage: ecs.ContainerImage.fromEcrRepository(repo, "latest"),
  secrets: {
    CLAUDE_CODE_OAUTH_TOKEN: claudeSecret,
    GIT_TOKEN: gitSecret,
  },
  // The full TestECS_* suite runs ~17 tests in parallel, each launching a
  // Fargate task. The construct default (5) rate-limits the suite; bump
  // to 20 to match the bootstrap CF stack's e2e budget. Production
  // consumers are expected to tune this per their own load.
  maxConcurrent: 20,
  // Spend cap (#36). A GENEROUS cap that never gates the e2e suite (the
  // script workflows cost ~$0), present only so TestECSSpendCapConfigured can
  // assert the SSM config + drain Lambda env carry the spend-cap wiring.
  maxSpendPerWindow: 1000,
  spendWindow: cdk.Duration.hours(24),
  // Sidecar containers (#7). A Postgres the worker reaches on localhost:5432.
  // TestECSCDK_Sidecar launches the `postgres-probe` workflow against this to
  // prove (a) localhost reachability and (b) that run status is the WORKER's
  // exit code, not this non-essential sidecar's (it is killed non-zero when the
  // worker exits). essential is left at the construct default (false).
  sidecars: [
    {
      containerName: "postgres",
      image: ecs.ContainerImage.fromRegistry("public.ecr.aws/docker/library/postgres:16-alpine"),
      environment: { POSTGRES_HOST_AUTH_METHOD: "trust" },
      // Small cap so the sidecar can't starve the worker (memoryMiB is the
      // task ceiling shared across containers).
      memoryLimitMiB: 512,
    },
  ],
});

new cdk.CfnOutput(stack, "StackNameOut", { value: stack.stackName });
new cdk.CfnOutput(stack, "SlugOut", { value: SLUG });
new cdk.CfnOutput(stack, "SsmPathOut", {
  value: worker.configParameter.parameterName,
});
new cdk.CfnOutput(stack, "ClusterArnOut", { value: worker.cluster.clusterArn });
new cdk.CfnOutput(stack, "EcrRepoUriOut", { value: repo.repositoryUri });
new cdk.CfnOutput(stack, "EcrRepoNameOut", { value: repo.repositoryName });
new cdk.CfnOutput(stack, "ArtifactsBucketOut", {
  value: worker.artifactsBucket.bucketName,
});
new cdk.CfnOutput(stack, "RunsTableOut", { value: worker.runsTable.tableName });
new cdk.CfnOutput(stack, "LogGroupOut", { value: worker.logGroup.logGroupName });
new cdk.CfnOutput(stack, "EventBusNameOut", {
  value: worker.eventBus.eventBusName,
});
new cdk.CfnOutput(stack, "ClaudeSecretArnOut", { value: claudeSecret.secretArn });
new cdk.CfnOutput(stack, "GitSecretArnOut", { value: gitSecret.secretArn });

app.synth();
