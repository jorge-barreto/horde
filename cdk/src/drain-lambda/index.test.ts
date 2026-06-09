// Unit tests for the drain Lambda. Mocks DynamoDB, ECS, and EventBridge via
// aws-sdk-client-mock and feeds synthetic run.terminal events through the
// exported handler.
import {
  ConditionalCheckFailedException,
  DynamoDBClient,
  QueryCommand,
  UpdateItemCommand,
} from "@aws-sdk/client-dynamodb";
import { ECSClient, RunTaskCommand } from "@aws-sdk/client-ecs";
import { EventBridgeClient, PutEventsCommand } from "@aws-sdk/client-eventbridge";
import { mockClient } from "aws-sdk-client-mock";
import type { Context, EventBridgeEvent } from "aws-lambda";

process.env.RUNS_TABLE = "horde-runs-test";
process.env.EVENT_BUS_NAME = "horde-test-bus";
process.env.MAX_CONCURRENT = "5";
process.env.CLUSTER_ARN = "arn:cluster";
process.env.TASK_DEF_ARN = "arn:taskdef";
process.env.SUBNETS = "subnet-1,subnet-2";
process.env.SECURITY_GROUP = "sg-1";
process.env.ASSIGN_PUBLIC_IP = "ENABLED";
process.env.ARTIFACTS_BUCKET = "horde-artifacts-test";

// eslint-disable-next-line @typescript-eslint/no-var-requires
const { handler } = require("./index") as typeof import("./index");

const ddbMock = mockClient(DynamoDBClient);
const ecsMock = mockClient(ECSClient);
const ebMock = mockClient(EventBridgeClient);

const ctx = {} as Context;

interface TerminalDetail {
  version?: number;
  run_id?: string;
  repo?: string;
  status?: string;
}

function terminalEvent(detail: TerminalDetail): EventBridgeEvent<"run.terminal", TerminalDetail> {
  return {
    version: "0",
    id: "evt-1",
    "detail-type": "run.terminal",
    source: "horde",
    account: "111111111111",
    time: "2026-06-08T00:00:00Z",
    region: "us-east-1",
    resources: [],
    detail,
  };
}

// A queued run item as returned from the by-repo GSI query.
function queuedItem(id: string, priority: string, enqueuedAt: string) {
  return {
    id: { S: id },
    repo: { S: "github.com/o/r" },
    ticket: { S: id },
    branch: { S: "" },
    workflow: { S: "w" },
    status: { S: "queued" },
    priority: { S: priority },
    enqueued_at: { S: enqueuedAt },
  };
}

beforeEach(() => {
  ddbMock.reset();
  ecsMock.reset();
  ebMock.reset();
});

// CountActive issues one Query per status (pending, running) against by-status;
// ClaimNextQueued issues a by-repo Query for queued items. Distinguish by IndexName.
function setupQueries(opts: { active: number; queued: ReturnType<typeof queuedItem>[] }) {
  ddbMock.on(QueryCommand).callsFake((input) => {
    if (input.IndexName === "by-status") {
      // Split the active count across the first status query.
      return { Count: opts.active, Items: [] };
    }
    if (input.IndexName === "by-repo") {
      return { Items: opts.queued };
    }
    return { Items: [], Count: 0 };
  });
}

it("claims the highest-priority oldest queued run, RunTasks it, and emits run.started", async () => {
  setupQueries({
    active: 0,
    queued: [
      queuedItem("low", "low", "2026-06-08T09:00:00Z"),
      queuedItem("high-new", "high", "2026-06-08T09:02:00Z"),
      queuedItem("high-old", "high", "2026-06-08T09:01:00Z"),
    ],
  });
  ddbMock.on(UpdateItemCommand).resolves({});
  ecsMock.on(RunTaskCommand).resolves({ tasks: [{ taskArn: "arn:task/new" }] });
  ebMock.on(PutEventsCommand).resolves({ FailedEntryCount: 0 });

  await handler(terminalEvent({ version: 1, run_id: "done", repo: "github.com/o/r", status: "success" }), ctx, () => {});

  const claim = ddbMock.commandCalls(UpdateItemCommand)[0].args[0].input;
  expect(claim.Key).toEqual({ id: { S: "high-old" } }); // highest priority, oldest
  expect(claim.ConditionExpression).toContain(":queued");

  const run = ecsMock.commandCalls(RunTaskCommand);
  expect(run).toHaveLength(1);
  expect(run[0].args[0].input.cluster).toBe("arn:cluster");
  expect(run[0].args[0].input.overrides?.containerOverrides?.[0].name).toBe("horde-worker");

  const emitted = ebMock.commandCalls(PutEventsCommand);
  expect(emitted).toHaveLength(1);
  expect(emitted[0].args[0].input.Entries?.[0].DetailType).toBe("run.started");
});

it("does not claim when at capacity", async () => {
  setupQueries({ active: 5, queued: [queuedItem("q", "med", "2026-06-08T09:00:00Z")] });
  ddbMock.on(UpdateItemCommand).resolves({});
  ecsMock.on(RunTaskCommand).resolves({ tasks: [{ taskArn: "arn:task/new" }] });

  await handler(terminalEvent({ version: 1, run_id: "done", repo: "github.com/o/r", status: "success" }), ctx, () => {});

  expect(ddbMock.commandCalls(UpdateItemCommand)).toHaveLength(0);
  expect(ecsMock.commandCalls(RunTaskCommand)).toHaveLength(0);
});

it("tries the next candidate when the conditional claim loses a race", async () => {
  setupQueries({
    active: 0,
    queued: [
      queuedItem("a", "high", "2026-06-08T09:00:00Z"),
      queuedItem("b", "high", "2026-06-08T09:01:00Z"),
    ],
  });
  // First claim (a) loses the race; second claim (b) wins.
  let claimCalls = 0;
  ddbMock.on(UpdateItemCommand).callsFake(() => {
    claimCalls++;
    if (claimCalls === 1) {
      throw new ConditionalCheckFailedException({ message: "lost race", $metadata: {} });
    }
    return {};
  });
  ecsMock.on(RunTaskCommand).resolves({ tasks: [{ taskArn: "arn:task/b" }] });
  ebMock.on(PutEventsCommand).resolves({ FailedEntryCount: 0 });

  await handler(terminalEvent({ version: 1, run_id: "done", repo: "github.com/o/r", status: "success" }), ctx, () => {});

  // a failed, b claimed → RunTask for b.
  const run = ecsMock.commandCalls(RunTaskCommand);
  expect(run).toHaveLength(1);
});
