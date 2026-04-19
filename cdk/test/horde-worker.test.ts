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
