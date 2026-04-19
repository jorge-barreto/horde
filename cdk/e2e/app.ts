#!/usr/bin/env node
// E2E smoke stack for @horde/cdk. Not for production. Deploys a real ECS
// stack under a dedicated slug so the existing TestECS_* harness pattern
// can verify the construct end-to-end without colliding with the
// horde-bootstrap CF stack.
//
// Secrets are reused by name from the bootstrap CF stack (so we don't
// re-upload Claude/Git tokens for the e2e). If you have not deployed the
// bootstrap stack at slug "jorge-barreto-horde", create those two secrets
// in Secrets Manager first.
import * as cdk from "aws-cdk-lib";
import * as ecr from "aws-cdk-lib/aws-ecr";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as secretsmanager from "aws-cdk-lib/aws-secretsmanager";
import { HordeWorker } from "../src";

// SLUG must match `bootstrap.Slug("https://github.com/jorge-barreto/horde-cdke2e.git")`.
// The Go test sets that fake remote so `horde push` resolves to this SSM path.
const SLUG = "jorge-barreto-horde-cdke2e";

const BOOTSTRAP_SLUG = "jorge-barreto-horde";
const CLAUDE_SECRET_NAME = `horde-${BOOTSTRAP_SLUG}-claude-code-oauth-token`;
const GIT_SECRET_NAME = `horde-${BOOTSTRAP_SLUG}-git-token`;

const app = new cdk.App();
const stack = new cdk.Stack(app, "HordeCdkE2E", {
  stackName: `horde-${SLUG}`,
  env: {
    account: process.env.CDK_DEFAULT_ACCOUNT,
    region: process.env.CDK_DEFAULT_REGION ?? "us-east-1",
  },
  description: "E2E smoke stack for @horde/cdk. Not for production.",
});

const repo = new ecr.Repository(stack, "WorkerRepo", {
  repositoryName: `horde-${SLUG}`,
  emptyOnDelete: true,
  removalPolicy: cdk.RemovalPolicy.DESTROY,
  imageScanOnPush: true,
});

const claudeSecret = secretsmanager.Secret.fromSecretNameV2(
  stack,
  "ClaudeToken",
  CLAUDE_SECRET_NAME,
);
const gitSecret = secretsmanager.Secret.fromSecretNameV2(
  stack,
  "GitToken",
  GIT_SECRET_NAME,
);

const worker = new HordeWorker(stack, "Worker", {
  projectSlug: SLUG,
  ecrRepository: repo,
  workerImage: ecs.ContainerImage.fromEcrRepository(repo, "latest"),
  secrets: {
    CLAUDE_CODE_OAUTH_TOKEN: claudeSecret,
    GIT_TOKEN: gitSecret,
  },
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

app.synth();
