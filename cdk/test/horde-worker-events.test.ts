// Tests for the run-lifecycle event backbone wiring (issue #36): the custom
// EventBridge bus, the status Lambda's EVENT_BUS_NAME env + events:PutEvents
// grant, the drain Lambda, the run.terminal rule targeting it, its IAM, and the
// spend-cap props flowing into SSM config + drain env.
import { App, Stack, Duration } from "aws-cdk-lib";
import { Match, Template } from "aws-cdk-lib/assertions";
import * as ecr from "aws-cdk-lib/aws-ecr";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as secretsmanager from "aws-cdk-lib/aws-secretsmanager";
import { HordeWorker, type HordeWorkerProps } from "../src";

const ENV = { account: "111111111111", region: "us-east-1" };

function synth(extra: Partial<HordeWorkerProps> = {}): Template {
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
    ...extra,
  });
  return Template.fromStack(stack);
}

describe("HordeWorker event backbone (#36)", () => {
  it("creates a custom EventBridge bus", () => {
    synth().resourceCountIs("AWS::Events::EventBus", 1);
  });

  it("gives the status lambda EVENT_BUS_NAME and events:PutEvents", () => {
    const t = synth();
    // Two lambdas now (status + drain); at least one has EVENT_BUS_NAME.
    t.hasResourceProperties("AWS::Lambda::Function", {
      Environment: { Variables: Match.objectLike({ EVENT_BUS_NAME: Match.anyValue() }) },
    });
    // An IAM policy somewhere grants events:PutEvents.
    t.hasResourceProperties("AWS::IAM::Policy", {
      PolicyDocument: {
        Statement: Match.arrayWith([
          Match.objectLike({ Action: "events:PutEvents" }),
        ]),
      },
    });
  });

  it("creates a drain lambda with the queue env vars", () => {
    const t = synth({ maxConcurrent: 7 });
    t.hasResourceProperties("AWS::Lambda::Function", {
      Environment: {
        Variables: Match.objectLike({
          RUNS_TABLE: Match.anyValue(),
          EVENT_BUS_NAME: Match.anyValue(),
          MAX_CONCURRENT: "7",
          CLUSTER_ARN: Match.anyValue(),
          TASK_DEF_ARN: Match.anyValue(),
        }),
      },
    });
  });

  it("routes run.terminal and run.requeued events on the horde bus to the drain lambda", () => {
    const t = synth();
    t.hasResourceProperties("AWS::Events::Rule", {
      EventPattern: {
        source: ["horde"],
        "detail-type": ["run.terminal", "run.requeued"],
      },
    });
  });

  it("grants the drain lambda ecs:RunTask, iam:PassRole, dynamo, and events:PutEvents", () => {
    const t = synth();
    const policies = Object.values(t.findResources("AWS::IAM::Policy"));
    const allActions = policies.flatMap((p) =>
      ((p.Properties.PolicyDocument.Statement ?? []) as Array<{ Action: string | string[] }>).flatMap((s) =>
        Array.isArray(s.Action) ? s.Action : [s.Action],
      ),
    );
    for (const want of ["ecs:RunTask", "iam:PassRole", "dynamodb:UpdateItem", "dynamodb:Query", "events:PutEvents"]) {
      expect(allActions).toContain(want);
    }
  });

  it("omits MAX_SPEND_PER_WINDOW from the drain env when no cap is set", () => {
    const t = synth();
    const fns = Object.values(t.findResources("AWS::Lambda::Function"));
    const drain = fns.find(
      (f) => f.Properties.Environment?.Variables?.MAX_CONCURRENT !== undefined,
    );
    expect(drain).toBeDefined();
    expect(drain!.Properties.Environment.Variables.MAX_SPEND_PER_WINDOW).toBeUndefined();
  });

  it("flows the spend cap into the drain env and SSM config when set", () => {
    const t = synth({ maxSpendPerWindow: 200, spendWindow: Duration.hours(12) });
    const fns = Object.values(t.findResources("AWS::Lambda::Function"));
    const drain = fns.find(
      (f) => f.Properties.Environment?.Variables?.MAX_CONCURRENT !== undefined,
    );
    expect(drain).toBeDefined();
    expect(drain!.Properties.Environment.Variables.MAX_SPEND_PER_WINDOW).toBe("200");
    expect(drain!.Properties.Environment.Variables.SPEND_WINDOW_SECONDS).toBe("43200");
  });
});
