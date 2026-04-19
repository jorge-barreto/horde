import { App, Stack } from "aws-cdk-lib";
import { Match, Template } from "aws-cdk-lib/assertions";
import * as ecr from "aws-cdk-lib/aws-ecr";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as secretsmanager from "aws-cdk-lib/aws-secretsmanager";
import { HordeWorker } from "../src";

function synth() {
  const app = new App();
  const stack = new Stack(app, "TestStack", {
    env: { account: "111111111111", region: "us-east-1" },
  });
  const repo = ecr.Repository.fromRepositoryName(stack, "Repo", "horde-test");
  new HordeWorker(stack, "Horde", {
    projectSlug: "test",
    workerImage: ecs.ContainerImage.fromRegistry("public.ecr.aws/horde/test:latest"),
    ecrRepository: repo,
    secrets: {
      CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "Claude", "horde/claude"),
      GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "Git", "horde/git"),
    },
  });
  return Template.fromStack(stack);
}

describe("HordeWorker (5fh.3 skeleton)", () => {
  it("creates a VPC tagged horde-<slug>-vpc", () => {
    const t = synth();
    t.hasResourceProperties("AWS::EC2::VPC", {
      Tags: Match.arrayWith([{ Key: "Name", Value: "horde-test-vpc" }]),
    });
  });

  it("creates an ECS cluster named horde-<slug>", () => {
    synth().hasResourceProperties("AWS::ECS::Cluster", { ClusterName: "horde-test" });
  });

  it("creates a log group with the expected name and 30-day retention", () => {
    synth().hasResourceProperties("AWS::Logs::LogGroup", {
      LogGroupName: "/ecs/horde-worker-test",
      RetentionInDays: 30,
    });
  });

  it("creates a Fargate task definition with the worker container", () => {
    synth().hasResourceProperties("AWS::ECS::TaskDefinition", {
      Family: "horde-test",
      Cpu: "1024",
      Memory: "4096",
      RequiresCompatibilities: ["FARGATE"],
      NetworkMode: "awsvpc",
      ContainerDefinitions: Match.arrayWith([
        Match.objectLike({
          Name: "horde-worker",
          Image: "public.ecr.aws/horde/test:latest",
          Essential: true,
          LogConfiguration: Match.objectLike({
            LogDriver: "awslogs",
            Options: Match.objectLike({ "awslogs-stream-prefix": "ecs" }),
          }),
        }),
      ]),
    });
  });

  it("injects CLAUDE_CODE_OAUTH_TOKEN and GIT_TOKEN as ECS secrets via valueFrom", () => {
    const t = synth();
    t.hasResourceProperties("AWS::ECS::TaskDefinition", {
      ContainerDefinitions: Match.arrayWith([
        Match.objectLike({
          Name: "horde-worker",
          Secrets: Match.arrayWith([
            Match.objectLike({ Name: "CLAUDE_CODE_OAUTH_TOKEN", ValueFrom: Match.anyValue() }),
            Match.objectLike({ Name: "GIT_TOKEN", ValueFrom: Match.anyValue() }),
          ]),
        }),
      ]),
    });
  });

  it("does NOT pass secrets as plain Environment vars", () => {
    const t = synth();
    const tds = t.findResources("AWS::ECS::TaskDefinition");
    for (const td of Object.values(tds)) {
      const containers = td.Properties.ContainerDefinitions ?? [];
      for (const c of containers) {
        const env = c.Environment ?? [];
        for (const e of env) {
          expect(e.Name).not.toBe("CLAUDE_CODE_OAUTH_TOKEN");
          expect(e.Name).not.toBe("GIT_TOKEN");
        }
      }
    }
  });

  it("creates an S3 artifacts bucket with public access blocked and SSE-S3", () => {
    const t = synth();
    t.hasResourceProperties("AWS::S3::Bucket", {
      BucketEncryption: {
        ServerSideEncryptionConfiguration: [
          { ServerSideEncryptionByDefault: { SSEAlgorithm: "AES256" } },
        ],
      },
      PublicAccessBlockConfiguration: {
        BlockPublicAcls: true,
        BlockPublicPolicy: true,
        IgnorePublicAcls: true,
        RestrictPublicBuckets: true,
      },
    });
  });

  it("denies non-TLS requests on the artifacts bucket", () => {
    const t = synth();
    t.hasResourceProperties("AWS::S3::BucketPolicy", {
      PolicyDocument: Match.objectLike({
        Statement: Match.arrayWith([
          Match.objectLike({
            Effect: "Deny",
            Action: "s3:*",
            Condition: { Bool: { "aws:SecureTransport": "false" } },
          }),
        ]),
      }),
    });
  });

  it("matches the saved snapshot", () => {
    expect(synth().toJSON()).toMatchSnapshot();
  });

  it("rejects an unsupported logRetentionDays value", () => {
    const app = new App();
    const stack = new Stack(app, "BadStack");
    const repo = ecr.Repository.fromRepositoryName(stack, "Repo", "horde-test");
    expect(
      () =>
        new HordeWorker(stack, "Horde", {
          projectSlug: "test",
          workerImage: ecs.ContainerImage.fromRegistry("public.ecr.aws/horde/test:latest"),
          ecrRepository: repo,
          secrets: {
            CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "C", "c"),
            GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "G", "g"),
          },
          logRetentionDays: 42,
        }),
    ).toThrow(/logRetentionDays=42/);
  });
});
