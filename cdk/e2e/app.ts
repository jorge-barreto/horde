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
import { HordeWorker } from "../src";

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
new cdk.CfnOutput(stack, "ClaudeSecretArnOut", { value: claudeSecret.secretArn });
new cdk.CfnOutput(stack, "GitSecretArnOut", { value: gitSecret.secretArn });

app.synth();
