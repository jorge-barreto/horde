/**
 * Drain Lambda for the horde queue. Triggered by EventBridge on the horde bus
 * for `run.terminal` events (a slot just freed). It drains AT MOST ONE queued
 * run per event: check capacity, check the realized spend cap, then claim the
 * highest-priority oldest-enqueued queued run (atomic conditional update
 * queued→pending), RunTask it, and emit run.started.
 *
 * Mechanical only — it never decides priority from ticket meaning. The
 * prioritization "brain" lives above horde and curates the backlog via
 * `horde queue prioritize` / `--priority` / `--force`.
 *
 * The Go lazy-CLI drainer (cmd/horde/drain.go) is the backstop with identical
 * logic. The store's atomic conditional update is the single double-launch guard
 * shared by both.
 */
import type { EventBridgeEvent, Handler } from "aws-lambda";
import {
  DynamoDBClient,
  QueryCommand,
  UpdateItemCommand,
  ConditionalCheckFailedException,
  type AttributeValue,
} from "@aws-sdk/client-dynamodb";
import { ECSClient, RunTaskCommand } from "@aws-sdk/client-ecs";
import { EventBridgeClient, PutEventsCommand } from "@aws-sdk/client-eventbridge";

const RUNS_TABLE = process.env.RUNS_TABLE ?? "";
const WORKER_CONTAINER_NAME = "horde-worker"; // cross-language contract; see ecs.go / status-lambda

const ddb = new DynamoDBClient({});
const ecs = new ECSClient({});
const eb = new EventBridgeClient({});

interface TerminalDetail {
  readonly repo?: string;
}

interface QueuedRun {
  id: string;
  repo: string;
  ticket: string;
  branch: string;
  workflow: string;
  priority: string;
  enqueuedAt: string; // RFC3339; drain-order tiebreaker (oldest first)
  capacity: string; // "spot" | "on-demand" | "" (empty => spot)
}

const PRIORITY_ORDINAL: Record<string, number> = {
  lowest: 0,
  low: 1,
  med: 2,
  high: 3,
  highest: 4,
};

function ordinal(p: string): number {
  return p in PRIORITY_ORDINAL ? PRIORITY_ORDINAL[p] : PRIORITY_ORDINAL.med;
}

function s(attr: AttributeValue | undefined): string {
  return attr && "S" in attr ? attr.S ?? "" : "";
}

// countActive sums pending + running across the by-status GSI.
async function countActive(): Promise<number> {
  let total = 0;
  for (const status of ["pending", "running"]) {
    let startKey: Record<string, AttributeValue> | undefined;
    do {
      const out = await ddb.send(
        new QueryCommand({
          TableName: RUNS_TABLE,
          IndexName: "by-status",
          KeyConditionExpression: "#s = :s",
          ExpressionAttributeNames: { "#s": "status" },
          ExpressionAttributeValues: { ":s": { S: status } },
          Select: "COUNT",
          ExclusiveStartKey: startKey,
        }),
      );
      total += out.Count ?? 0;
      startKey = out.LastEvaluatedKey as Record<string, AttributeValue> | undefined;
    } while (startKey);
  }
  return total;
}

// listQueued fetches the repo's queued runs from the by-repo GSI.
async function listQueued(repo: string): Promise<QueuedRun[]> {
  const runs: QueuedRun[] = [];
  let startKey: Record<string, AttributeValue> | undefined;
  do {
    const out = await ddb.send(
      new QueryCommand({
        TableName: RUNS_TABLE,
        IndexName: "by-repo",
        KeyConditionExpression: "#repo = :repo",
        FilterExpression: "#s = :queued",
        ExpressionAttributeNames: { "#repo": "repo", "#s": "status" },
        ExpressionAttributeValues: { ":repo": { S: repo }, ":queued": { S: "queued" } },
        ExclusiveStartKey: startKey,
      }),
    );
    for (const item of out.Items ?? []) {
      runs.push({
        id: s(item.id),
        repo: s(item.repo),
        ticket: s(item.ticket),
        branch: s(item.branch),
        workflow: s(item.workflow),
        priority: s(item.priority),
        enqueuedAt: s(item.enqueued_at),
        capacity: s(item.capacity),
      });
    }
    startKey = out.LastEvaluatedKey as Record<string, AttributeValue> | undefined;
  } while (startKey);
  return runs;
}

// realizedSpendOK returns false when the trailing-window realized spend meets or
// exceeds the cap. Realized-only — in-flight runs are uncosted until they finish
// (see follow-on #52). Disabled (always OK) when no cap/window is configured.
async function realizedSpendOK(repo: string): Promise<boolean> {
  const cap = Number(process.env.MAX_SPEND_PER_WINDOW ?? "0");
  const windowSeconds = Number(process.env.SPEND_WINDOW_SECONDS ?? "0");
  if (!(cap > 0) || !(windowSeconds > 0)) return true;
  const since = new Date(Date.now() - windowSeconds * 1000);
  let total = 0;
  let startKey: Record<string, AttributeValue> | undefined;
  do {
    const out = await ddb.send(
      new QueryCommand({
        TableName: RUNS_TABLE,
        IndexName: "by-repo",
        KeyConditionExpression: "#repo = :repo",
        ExpressionAttributeNames: { "#repo": "repo" },
        ExpressionAttributeValues: { ":repo": { S: repo } },
        ExclusiveStartKey: startKey,
      }),
    );
    for (const item of out.Items ?? []) {
      const costAttr = item.total_cost_usd;
      const completedAttr = item.completed_at;
      if (!costAttr || !("N" in costAttr)) continue;
      if (!completedAttr || !("S" in completedAttr)) continue;
      const completedAt = new Date(completedAttr.S ?? "");
      if (completedAt < since) continue;
      total += Number(costAttr.N);
    }
    startKey = out.LastEvaluatedKey as Record<string, AttributeValue> | undefined;
  } while (startKey);
  return total < cap;
}

function runTaskInput(run: QueuedRun) {
  const env = [
    { name: "REPO_URL", value: run.repo },
    { name: "TICKET", value: run.ticket },
    { name: "BRANCH", value: run.branch },
    { name: "WORKFLOW", value: run.workflow },
    { name: "RUN_ID", value: run.id },
    { name: "ARTIFACTS_BUCKET", value: process.env.ARTIFACTS_BUCKET ?? "" },
  ];
  return {
    taskDefinition: process.env.TASK_DEF_ARN,
    cluster: process.env.CLUSTER_ARN,
    capacityProviderStrategy: [
      {
        capacityProvider: run.capacity === "on-demand" ? "FARGATE" : "FARGATE_SPOT",
        weight: 1,
      },
    ],
    count: 1,
    networkConfiguration: {
      awsvpcConfiguration: {
        subnets: (process.env.SUBNETS ?? "").split(",").filter(Boolean),
        securityGroups: [process.env.SECURITY_GROUP ?? ""],
        assignPublicIp: (process.env.ASSIGN_PUBLIC_IP === "DISABLED" ? "DISABLED" : "ENABLED") as
          | "DISABLED"
          | "ENABLED",
      },
    },
    overrides: {
      containerOverrides: [{ name: WORKER_CONTAINER_NAME, environment: env }],
    },
    tags: [
      { key: "horde-run-id", value: run.id },
      { key: "horde-ticket", value: run.ticket },
    ],
  };
}

async function emit(detailType: string, run: QueuedRun, extra: Record<string, unknown> = {}): Promise<void> {
  const busName = process.env.EVENT_BUS_NAME ?? "";
  if (!busName) return;
  try {
    await eb.send(
      new PutEventsCommand({
        Entries: [
          {
            EventBusName: busName,
            Source: "horde",
            DetailType: detailType,
            Detail: JSON.stringify({
              version: 1,
              run_id: run.id,
              repo: run.repo,
              ticket: run.ticket,
              workflow: run.workflow,
              ...extra,
            }),
          },
        ],
      }),
    );
  } catch (err) {
    console.error("drain-lambda: emit failed (non-fatal)", { detailType, runId: run.id, err });
  }
}

export const handler: Handler<EventBridgeEvent<"run.terminal", TerminalDetail>, { drained?: string; skipped?: string }> =
  async (event) => {
    const repo = event.detail?.repo;
    if (!repo) {
      console.log("drain-lambda: skip, no repo in event detail");
      return { skipped: "no repo" };
    }

    const maxConcurrent = Number(process.env.MAX_CONCURRENT ?? "0");
    const active = await countActive();
    if (active >= maxConcurrent) {
      console.log("drain-lambda: skip, at capacity", { active, maxConcurrent });
      return { skipped: "at capacity" };
    }

    // Drain order: highest priority first, then oldest enqueued_at. Mirrors the
    // store's ClaimNextQueued ordering (sqlite CASE / dynamo in-memory sort).
    const candidates = (await listQueued(repo)).sort((a, b) => {
      const oa = ordinal(a.priority);
      const ob = ordinal(b.priority);
      if (oa !== ob) return ob - oa; // higher priority first
      return a.enqueuedAt.localeCompare(b.enqueuedAt); // oldest first (RFC3339 sorts lexically)
    });

    if (candidates.length === 0) {
      console.log("drain-lambda: queue empty", { repo });
      return { skipped: "queue empty" };
    }

    if (!(await realizedSpendOK(repo))) {
      console.log("drain-lambda: held by spend cap", { repo });
      await emit("run.cost-threshold-exceeded", candidates[0]);
      return { skipped: "spend cap" };
    }

    // Walk candidates highest-priority-first; the first conditional claim that
    // wins is ours. A lost race (someone else claimed it) tries the next.
    for (const run of candidates) {
      try {
        await ddb.send(
          new UpdateItemCommand({
            TableName: RUNS_TABLE,
            Key: { id: { S: run.id } },
            UpdateExpression: "SET #s = :pending",
            ConditionExpression: "#s = :queued",
            ExpressionAttributeNames: { "#s": "status" },
            ExpressionAttributeValues: {
              ":pending": { S: "pending" },
              ":queued": { S: "queued" },
            },
          }),
        );
      } catch (err) {
        if (err instanceof ConditionalCheckFailedException) continue; // lost race
        throw err;
      }

      // Claimed. RunTask, then record running + instance_id; emit run.started.
      try {
        const out = await ecs.send(new RunTaskCommand(runTaskInput(run)));
        const taskArn = out.tasks?.[0]?.taskArn ?? "";
        const startedAt = new Date().toISOString();
        await ddb.send(
          new UpdateItemCommand({
            TableName: RUNS_TABLE,
            Key: { id: { S: run.id } },
            UpdateExpression: "SET #s = :running, instance_id = :iid, started_at = :sa",
            ExpressionAttributeNames: { "#s": "status" },
            ExpressionAttributeValues: {
              ":running": { S: "running" },
              ":iid": { S: taskArn },
              ":sa": { S: startedAt },
            },
          }),
        );
        await emit("run.started", run, { status: "running" });
        console.log("drain-lambda: drained", { runId: run.id, taskArn });
        return { drained: run.id };
      } catch (err) {
        // RunTask failed after claim: mark failed (NOT re-queued — avoid a poison
        // loop). Surfaces in `horde list` for a human/agent to retry.
        console.error("drain-lambda: RunTask failed after claim", { runId: run.id, err });
        await ddb.send(
          new UpdateItemCommand({
            TableName: RUNS_TABLE,
            Key: { id: { S: run.id } },
            UpdateExpression: "SET #s = :failed, completed_at = :ca",
            ExpressionAttributeNames: { "#s": "status" },
            ExpressionAttributeValues: {
              ":failed": { S: "failed" },
              ":ca": { S: new Date().toISOString() },
            },
          }),
        );
        return { skipped: "run task failed" };
      }
    }

    console.log("drain-lambda: all candidates lost the claim race", { repo });
    return { skipped: "no claim won" };
  };
