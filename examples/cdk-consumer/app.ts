#!/usr/bin/env node
// Minimal CDK app that provisions a horde worker cluster via @horde.io/cdk.
// Drop this into your own repo, change `SLUG` to match your git remote,
// then `npm run deploy`. After that, `horde push` (from your project) builds
// and uploads the worker image to the ECR repo this stack creates.
import * as cdk from "aws-cdk-lib";
import * as ecr from "aws-cdk-lib/aws-ecr";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as secretsmanager from "aws-cdk-lib/aws-secretsmanager";
import { HordeWorker } from "@horde.io/cdk";

// SLUG must match what `horde push` derives from your git remote — see
// `horde docs bootstrap` for the derivation rule (owner-repo, lowercased,
// non-alphanumerics collapsed to '-'). For example:
//   git@github.com:acme/widgets.git  -> "acme-widgets"
const SLUG = process.env.HORDE_PROJECT_SLUG ?? "acme-widgets";

const app = new cdk.App();
const stack = new cdk.Stack(app, "HordeWorkerStack", {
  stackName: `horde-${SLUG}`,
  env: {
    // Prefer an explicit env. Falls back to the deploying shell's defaults.
    account: process.env.CDK_DEFAULT_ACCOUNT,
    region: process.env.CDK_DEFAULT_REGION ?? "us-east-1",
  },
  description: `horde worker cluster for ${SLUG}`,
});

// ECR repo the stack owns. `horde push` pushes the worker image here.
const repo = new ecr.Repository(stack, "WorkerRepo", {
  repositoryName: `horde-${SLUG}`,
  imageScanOnPush: true,
  // For a real deployment you'll usually want emptyOnDelete=false and
  // RETAIN. These settings make the example safe to tear down with
  // `npm run destroy`.
  emptyOnDelete: true,
  removalPolicy: cdk.RemovalPolicy.DESTROY,
});

// Empty Secrets Manager secrets owned by the stack. After `npm run deploy`,
// populate them with:
//   aws secretsmanager put-secret-value --secret-id horde-<slug>-claude-code-oauth-token --secret-string "$CLAUDE_CODE_OAUTH_TOKEN"
//   aws secretsmanager put-secret-value --secret-id horde-<slug>-git-token              --secret-string "$GIT_TOKEN"
// The worker entrypoint reads CLAUDE_CODE_OAUTH_TOKEN for claude-code auth
// and GIT_TOKEN for private-repo clones.
const claudeSecret = new secretsmanager.Secret(stack, "ClaudeToken", {
  secretName: `horde-${SLUG}-claude-code-oauth-token`,
  description: "CLAUDE_CODE_OAUTH_TOKEN for the horde worker (populate post-deploy).",
  removalPolicy: cdk.RemovalPolicy.DESTROY,
});
const gitSecret = new secretsmanager.Secret(stack, "GitToken", {
  secretName: `horde-${SLUG}-git-token`,
  description: "GIT_TOKEN for the horde worker's git-askpass helper (populate post-deploy).",
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
  // Defaults below are shown for discoverability — all are optional:
  // cpu: 1024,
  // memoryMiB: 4096,
  // maxConcurrent: 5,
  // defaultTimeoutMinutes: 1440,
  // logRetentionDays: 30,
});

// CFN outputs useful for wiring horde locally.
new cdk.CfnOutput(stack, "SsmConfigPath", {
  value: worker.configParameter.parameterName,
  description: "SSM parameter horde reads to discover this cluster.",
});
new cdk.CfnOutput(stack, "EcrRepoUri", {
  value: repo.repositoryUri,
  description: "ECR URI that `horde push` uploads the worker image to.",
});
new cdk.CfnOutput(stack, "CliUserManagedPolicyArn", {
  value: worker.cliUserPolicy.managedPolicyArn,
  description: "Attach this managed policy to the IAM user or role that will run `horde launch`.",
});

app.synth();
