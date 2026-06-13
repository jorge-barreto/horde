// Tests for the `sidecars` prop (issue #7): additional containers added to the
// worker's Fargate task definition. Verifies the construct's defaults
// (essential:false, shared-log-group logging), that caller values win, the
// reserved-name guard, and that the worker container is never displaced.
import { App, Stack } from "aws-cdk-lib";
import { Match, Template } from "aws-cdk-lib/assertions";
import * as ecr from "aws-cdk-lib/aws-ecr";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as secretsmanager from "aws-cdk-lib/aws-secretsmanager";
import { HordeWorker, WORKER_CONTAINER_NAME, type HordeWorkerProps } from "../src";

const ENV = { account: "111111111111", region: "us-east-1" };

function synthWith(sidecars: HordeWorkerProps["sidecars"]): Template {
  const app = new App();
  const stack = new Stack(app, "TestStack", { env: ENV });
  const repo = ecr.Repository.fromRepositoryName(stack, "Repo", "horde-test");
  new HordeWorker(stack, "Horde", {
    projectSlug: "test",
    repo: "github.com/example/test",
    workerImage: ecs.ContainerImage.fromRegistry("public.ecr.aws/horde/test:latest"),
    ecrRepository: repo,
    secrets: {
      CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "Claude", "horde/claude"),
      GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "Git", "horde/git"),
    },
    sidecars,
  });
  return Template.fromStack(stack);
}

function buildWith(sidecars: HordeWorkerProps["sidecars"]): HordeWorker {
  const app = new App();
  const stack = new Stack(app, "TestStack", { env: ENV });
  const repo = ecr.Repository.fromRepositoryName(stack, "Repo", "horde-test");
  return new HordeWorker(stack, "Horde", {
    projectSlug: "test",
    repo: "github.com/example/test",
    workerImage: ecs.ContainerImage.fromRegistry("public.ecr.aws/horde/test:latest"),
    ecrRepository: repo,
    secrets: {
      CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "Claude", "horde/claude"),
      GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "Git", "horde/git"),
    },
    sidecars,
  });
}

describe("HordeWorker sidecars (#7)", () => {
  it("adds a sidecar as a second container in the task definition", () => {
    const t = synthWith([
      { containerName: "postgres", image: ecs.ContainerImage.fromRegistry("postgres:16") },
    ]);
    const tds = t.findResources("AWS::ECS::TaskDefinition");
    const td = Object.values(tds)[0];
    const names = (td.Properties.ContainerDefinitions ?? []).map((c: { Name: string }) => c.Name);
    expect(names).toContain(WORKER_CONTAINER_NAME);
    expect(names).toContain("postgres");
    expect(names).toHaveLength(2);
  });

  it("defaults a sidecar to Essential: false; worker stays Essential: true", () => {
    const t = synthWith([
      { containerName: "postgres", image: ecs.ContainerImage.fromRegistry("postgres:16") },
    ]);
    t.hasResourceProperties("AWS::ECS::TaskDefinition", {
      ContainerDefinitions: Match.arrayWith([
        Match.objectLike({ Name: WORKER_CONTAINER_NAME, Essential: true }),
        Match.objectLike({ Name: "postgres", Essential: false }),
      ]),
    });
  });

  it("honors an explicit Essential: true on a sidecar", () => {
    const t = synthWith([
      {
        containerName: "postgres",
        image: ecs.ContainerImage.fromRegistry("postgres:16"),
        essential: true,
      },
    ]);
    t.hasResourceProperties("AWS::ECS::TaskDefinition", {
      ContainerDefinitions: Match.arrayWith([
        Match.objectLike({ Name: "postgres", Essential: true }),
      ]),
    });
  });

  it("defaults sidecar logging to the worker log group with its name as the stream prefix", () => {
    const t = synthWith([
      { containerName: "postgres", image: ecs.ContainerImage.fromRegistry("postgres:16") },
    ]);
    t.hasResourceProperties("AWS::ECS::TaskDefinition", {
      ContainerDefinitions: Match.arrayWith([
        Match.objectLike({
          Name: "postgres",
          LogConfiguration: Match.objectLike({
            LogDriver: "awslogs",
            Options: Match.objectLike({ "awslogs-stream-prefix": "postgres" }),
          }),
        }),
      ]),
    });
  });

  it("rejects a sidecar that reuses the reserved worker container name", () => {
    expect(() =>
      buildWith([
        { containerName: WORKER_CONTAINER_NAME, image: ecs.ContainerImage.fromRegistry("x:1") },
      ]),
    ).toThrow(/reserved/);
  });

  it("exposes the added sidecar containers via sidecarContainers", () => {
    const worker = buildWith([
      { containerName: "postgres", image: ecs.ContainerImage.fromRegistry("postgres:16") },
      { containerName: "redis", image: ecs.ContainerImage.fromRegistry("redis:7") },
    ]);
    expect(worker.sidecarContainers).toHaveLength(2);
    expect(worker.sidecarContainers.map((c) => c.containerName)).toEqual(["postgres", "redis"]);
  });

  it("creates a single-container task definition when no sidecars are given", () => {
    const t = synthWith(undefined);
    const td = Object.values(t.findResources("AWS::ECS::TaskDefinition"))[0];
    expect(td.Properties.ContainerDefinitions).toHaveLength(1);
  });
});

describe("worker container name (lockstep with Go provider)", () => {
  it("synthesizes the worker container with Name equal to WORKER_CONTAINER_NAME", () => {
    const t = synthWith(undefined);
    t.hasResourceProperties("AWS::ECS::TaskDefinition", {
      ContainerDefinitions: Match.arrayWith([
        Match.objectLike({ Name: WORKER_CONTAINER_NAME }),
      ]),
    });
    // Guards against an accidental rename that would silently break the
    // status-Lambda filter and the Go ECS provider's log-stream path — both
    // of which hardcode this string.
    expect(WORKER_CONTAINER_NAME).toBe("horde-worker");
  });
});
