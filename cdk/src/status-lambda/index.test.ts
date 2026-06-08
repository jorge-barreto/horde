// Unit tests for the status-sync Lambda handler (bead 5fh.16).
// Mocks the DynamoDB and S3 clients via aws-sdk-client-mock and feeds
// synthetic ECS Task State Change events through the exported handler.
import {
  ConditionalCheckFailedException,
  DynamoDBClient,
  QueryCommand,
  UpdateItemCommand,
} from "@aws-sdk/client-dynamodb";
import { GetObjectCommand, ListObjectsV2Command, S3Client } from "@aws-sdk/client-s3";
import { mockClient } from "aws-sdk-client-mock";
import { Readable } from "stream";
import type { Context, EventBridgeEvent } from "aws-lambda";

const RUNS_TABLE = "horde-runs-test";
const ARTIFACTS_BUCKET = "horde-artifacts-test";

process.env.RUNS_TABLE = RUNS_TABLE;
process.env.ARTIFACTS_BUCKET = ARTIFACTS_BUCKET;

// Import after env is set so module-level clients see the right names.
// eslint-disable-next-line @typescript-eslint/no-var-requires
const { handler } = require("./index") as typeof import("./index");

const ddbMock = mockClient(DynamoDBClient);
const s3Mock = mockClient(S3Client);

// Lambda Context is unused by our handler; a minimal stub keeps tsc happy.
const ctx = {} as Context;

interface EcsContainer {
  exitCode?: number;
  name?: string;
}

interface EcsDetail {
  taskArn?: string;
  lastStatus?: string;
  stoppedAt?: string;
  stopCode?: string;
  stoppedReason?: string;
  containers?: EcsContainer[];
  clusterArn?: string;
}

function event(detail: EcsDetail): EventBridgeEvent<"ECS Task State Change", EcsDetail> {
  return {
    version: "0",
    id: "evt-1",
    "detail-type": "ECS Task State Change",
    source: "aws.ecs",
    account: "111111111111",
    time: "2026-04-19T00:00:00Z",
    region: "us-east-1",
    resources: [],
    detail,
  };
}

function streamFromString(s: string): unknown {
  // The S3 client expects a body with transformToString(). We provide a
  // matching shim. Cast at the call site since the SDK's stream type is
  // platform-conditional.
  return {
    transformToString: async () => s,
    pipe: () => Readable.from([Buffer.from(s)]),
  };
}

beforeEach(() => {
  ddbMock.reset();
  s3Mock.reset();
});

describe("status-lambda handler (5fh.16)", () => {
  it("skips when lastStatus is not STOPPED", async () => {
    const r = await handler(event({ lastStatus: "RUNNING", taskArn: "arn:task/x" }), ctx, () => {});
    expect(r).toEqual({ skipped: "not stopped" });
    expect(ddbMock.calls()).toHaveLength(0);
    expect(s3Mock.calls()).toHaveLength(0);
  });

  it("skips when taskArn is missing", async () => {
    const r = await handler(event({ lastStatus: "STOPPED" }), ctx, () => {});
    expect(r).toEqual({ skipped: "no taskArn" });
    expect(ddbMock.calls()).toHaveLength(0);
  });

  it("skips when by-instance lookup returns no items (task not managed by horde)", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [] });
    const r = await handler(
      event({ lastStatus: "STOPPED", taskArn: "arn:task/foreign" }),
      ctx,
      () => {},
    );
    expect(r).toEqual({ skipped: "task not managed by horde" });
    expect(ddbMock.commandCalls(UpdateItemCommand)).toHaveLength(0);
  });

  it("maps exitCode 0 to success and updates with completed_at + exit_code", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-abc" } }] });
    ddbMock.on(UpdateItemCommand).resolves({});
    // No run-result.json found.
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    const r = await handler(
      event({
        lastStatus: "STOPPED",
        taskArn: "arn:task/abc",
        stoppedAt: "2026-04-19T00:00:01Z",
        containers: [{ exitCode: 0, name: "horde-worker" }],
      }),
      ctx,
      () => {},
    );

    expect(r).toEqual({ updated: true, runId: "run-abc", status: "success", exitCode: 0 });

    const calls = ddbMock.commandCalls(UpdateItemCommand);
    expect(calls).toHaveLength(1);
    const input = calls[0].args[0].input;
    expect(input.TableName).toBe(RUNS_TABLE);
    expect(input.Key).toEqual({ id: { S: "run-abc" } });
    expect(input.UpdateExpression).toContain("#s = :s");
    expect(input.UpdateExpression).toContain("#ca = :ca");
    expect(input.UpdateExpression).toContain("#ec = :ec");
    expect(input.UpdateExpression).not.toContain("#tc"); // no cost set when no run-result.json
    expect(input.ExpressionAttributeValues?.[":s"]).toEqual({ S: "success" });
    expect(input.ExpressionAttributeValues?.[":ec"]).toEqual({ N: "0" });
    expect(input.ConditionExpression).toMatch(/attribute_not_exists\(#s\)/);
    expect(input.ConditionExpression).toMatch(
      /NOT \(#s IN \(:success, :failed, :killed, :timed_out, :rate_limited\)\)/,
    );
  });

  it("maps exitCode 5 to killed", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-k" } }] });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    const r = await handler(
      event({
        lastStatus: "STOPPED",
        taskArn: "arn:task/k",
        containers: [{ exitCode: 5 }],
      }),
      ctx,
      () => {},
    );
    expect(r).toMatchObject({ updated: true, status: "killed", exitCode: 5 });
  });

  it("maps a generic non-zero exit (1) to failed", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-f" } }] });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    const r = await handler(
      event({ lastStatus: "STOPPED", taskArn: "arn:task/f", containers: [{ exitCode: 1 }] }),
      ctx,
      () => {},
    );
    expect(r).toMatchObject({ updated: true, status: "failed", exitCode: 1 });
  });

  it("maps exitCode 2 to timed_out and 4 to rate_limited (recoverable)", async () => {
    for (const [code, want] of [
      [2, "timed_out"],
      [4, "rate_limited"],
    ] as const) {
      ddbMock.reset();
      ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: `run-${code}` } }] });
      ddbMock.on(UpdateItemCommand).resolves({});
      s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

      const r = await handler(
        event({ lastStatus: "STOPPED", taskArn: `arn:task/${code}`, containers: [{ exitCode: code }] }),
        ctx,
        () => {},
      );
      expect(r).toMatchObject({ updated: true, status: want, exitCode: code });
    }
  });

  it("records stopCode/stoppedReason via a seed update then a nested update", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-spot" } }] });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    await handler(
      event({
        lastStatus: "STOPPED",
        taskArn: "arn:task/spot",
        stopCode: "TerminationNotice",
        stoppedReason: "Your Spot Task was interrupted",
        containers: [{ exitCode: 5 }],
      }),
      ctx,
      () => {},
    );

    // Three updates, in order: (0) seed metadata with if_not_exists,
    // (1) nested stop_code/stop_reason writes, (2) the guarded status update.
    // The seed and nested writes MUST be separate calls — DynamoDB rejects
    // referencing `metadata` and `metadata.x` in one expression. The stop
    // reason is written BEFORE the status update so it lands even when the
    // status guard skips (already-terminal).
    const calls = ddbMock.commandCalls(UpdateItemCommand);
    expect(calls).toHaveLength(3);

    const seed = calls[0].args[0].input;
    const seedExpr = seed.UpdateExpression ?? "";
    expect(seedExpr).toBe("SET #m = if_not_exists(#m, :emptymap)");
    expect(seed.ExpressionAttributeValues?.[":emptymap"]).toEqual({ M: {} });

    const meta = calls[1].args[0].input;
    const metaExpr = meta.UpdateExpression ?? "";
    expect(metaExpr).toContain("#m.#sc = :sc");
    expect(metaExpr).toContain("#m.#sr = :sr");
    expect(metaExpr).not.toContain("if_not_exists"); // no overlap with the seed
    expect(meta.ExpressionAttributeNames?.["#sc"]).toBe("stop_code");
    expect(meta.ExpressionAttributeNames?.["#sr"]).toBe("stop_reason");
    expect(meta.ExpressionAttributeValues?.[":sc"]).toEqual({ S: "TerminationNotice" });
    expect(meta.ExpressionAttributeValues?.[":sr"]).toEqual({
      S: "Your Spot Task was interrupted",
    });

    // The status update comes last and carries the terminal guard.
    const statusUpd = calls[2].args[0].input;
    expect(statusUpd.UpdateExpression).toContain("#s = :s");
    expect(statusUpd.ConditionExpression).toContain("attribute_not_exists(#s)");
  });

  it("records the stop reason even when the status update is already terminal", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-killed" } }] });
    // First two updates (stop-reason seed + nested) succeed; the third (status)
    // hits the terminal guard and is rejected — as it would for a synchronous
    // `horde kill` that already set "killed".
    let n = 0;
    ddbMock.on(UpdateItemCommand).callsFake(() => {
      n += 1;
      if (n === 3) {
        throw new ConditionalCheckFailedException({ message: "already terminal", $metadata: {} });
      }
      return {};
    });
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    const r = await handler(
      event({
        lastStatus: "STOPPED",
        taskArn: "arn:task/killed",
        stopCode: "TerminationNotice",
        stoppedReason: "spot interruption",
        containers: [{ exitCode: 5 }],
      }),
      ctx,
      () => {},
    );

    // The handler reports the status as already-terminal, but the two
    // stop-reason writes ran BEFORE the guarded status update — so the reason
    // was recorded regardless.
    expect(r).toEqual({ skipped: "already terminal", runId: "run-killed" });
    const calls = ddbMock.commandCalls(UpdateItemCommand);
    expect(calls).toHaveLength(3);
    expect(calls[0].args[0].input.UpdateExpression).toContain("if_not_exists");
    expect(calls[1].args[0].input.UpdateExpression).toContain("#m.#sc = :sc");
  });

  it("omits metadata writes when no stop reason is present", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-nostop" } }] });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    await handler(
      event({ lastStatus: "STOPPED", taskArn: "arn:task/ns", containers: [{ exitCode: 0 }] }),
      ctx,
      () => {},
    );
    const input = ddbMock.commandCalls(UpdateItemCommand)[0].args[0].input;
    expect(input.UpdateExpression).not.toContain("#m");
  });

  it("includes total_cost_usd from run-result.json when present", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-cost" } }] });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({
      Contents: [{ Key: "horde-runs/run-cost/audit/wf/T-1/run-result.json" }],
    });
    s3Mock.on(GetObjectCommand).resolves({
      Body: streamFromString(JSON.stringify({ total_cost_usd: 1.23, exit_code: 0 })),
    } as never);

    const r = await handler(
      event({
        lastStatus: "STOPPED",
        taskArn: "arn:task/cost",
        containers: [{ exitCode: 0 }],
      }),
      ctx,
      () => {},
    );
    expect(r).toMatchObject({ updated: true, status: "success" });

    const update = ddbMock.commandCalls(UpdateItemCommand)[0].args[0].input;
    expect(update.UpdateExpression).toContain("#tc = :tc");
    expect(update.ExpressionAttributeValues?.[":tc"]).toEqual({ N: "1.23" });
  });

  it("proceeds without cost when run-result.json is missing", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-nocost" } }] });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    const r = await handler(
      event({ lastStatus: "STOPPED", taskArn: "arn:task/nc", containers: [{ exitCode: 0 }] }),
      ctx,
      () => {},
    );
    expect(r).toMatchObject({ updated: true, status: "success" });
    const input = ddbMock.commandCalls(UpdateItemCommand)[0].args[0].input;
    expect(input.UpdateExpression).not.toContain("#tc");
  });

  it("proceeds without cost when run-result.json fails to parse", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-bad" } }] });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({
      Contents: [{ Key: "horde-runs/run-bad/audit/wf/T-1/run-result.json" }],
    });
    s3Mock.on(GetObjectCommand).resolves({
      Body: streamFromString("not valid json {"),
    } as never);

    const r = await handler(
      event({ lastStatus: "STOPPED", taskArn: "arn:task/bad", containers: [{ exitCode: 0 }] }),
      ctx,
      () => {},
    );
    expect(r).toMatchObject({ updated: true, status: "success" });
    const input = ddbMock.commandCalls(UpdateItemCommand)[0].args[0].input;
    expect(input.UpdateExpression).not.toContain("#tc");
  });

  it("treats ConditionalCheckFailedException as idempotent skip", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-dup" } }] });
    ddbMock.on(UpdateItemCommand).rejects(
      new ConditionalCheckFailedException({
        message: "duplicate event",
        $metadata: {},
      }),
    );
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    const r = await handler(
      event({ lastStatus: "STOPPED", taskArn: "arn:task/dup", containers: [{ exitCode: 0 }] }),
      ctx,
      () => {},
    );
    expect(r).toEqual({ skipped: "already terminal", runId: "run-dup" });
  });

  it("rethrows non-ConditionalCheckFailed errors from UpdateItem", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-err" } }] });
    ddbMock.on(UpdateItemCommand).rejects(new Error("ProvisionedThroughputExceeded"));
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    await expect(
      handler(
        event({ lastStatus: "STOPPED", taskArn: "arn:task/e", containers: [{ exitCode: 0 }] }),
        ctx,
        () => {},
      ),
    ).rejects.toThrow(/ProvisionedThroughputExceeded/);
  });

  it("uses NULL marker when stoppedAt is absent", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-nots" } }] });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    await handler(
      event({ lastStatus: "STOPPED", taskArn: "arn:task/n", containers: [{ exitCode: 0 }] }),
      ctx,
      () => {},
    );
    const input = ddbMock.commandCalls(UpdateItemCommand)[0].args[0].input;
    expect(input.ExpressionAttributeValues?.[":ca"]).toEqual({ NULL: true });
  });

  it("queries the by-instance GSI with the task ARN as instance_id", async () => {
    ddbMock.on(QueryCommand).resolves({ Items: [{ id: { S: "run-q" } }] });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    await handler(
      event({
        lastStatus: "STOPPED",
        taskArn: "arn:task/q",
        containers: [{ exitCode: 0 }],
      }),
      ctx,
      () => {},
    );

    const q = ddbMock.commandCalls(QueryCommand)[0].args[0].input;
    expect(q.IndexName).toBe("by-instance");
    expect(q.KeyConditionExpression).toBe("instance_id = :iid");
    expect(q.ExpressionAttributeValues?.[":iid"]).toEqual({ S: "arn:task/q" });
    expect(q.Limit).toBe(1);
  });
});
