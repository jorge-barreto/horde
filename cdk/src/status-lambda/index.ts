/**
 * Status-sync Lambda for the horde worker. Triggered by EventBridge on
 * "ECS Task State Change" where detail.lastStatus == STOPPED. Maps the
 * ECS exit code to a terminal run status, pulls total_cost_usd from
 * run-result.json in S3 (best-effort), and idempotently updates the
 * runs table row keyed by the task ARN in the `by-instance` GSI.
 *
 * It also records the ECS stop reason (detail.stopCode / detail.stoppedReason)
 * into the run's `metadata` map, so callers — and the planned spot
 * auto-resume — can tell WHY a task stopped (e.g. stopCode "TerminationNotice"
 * for a Fargate spot interruption) without re-deriving it from logs.
 *
 * The Python lambda in internal/bootstrap/templates/stack.yaml.tmpl is a
 * transliteration of this handler and MUST be kept in lockstep — exit-code
 * mapping (0=success, 2=timed_out, 4=rate_limited, 5=killed, else failed),
 * the metadata stop-reason capture, and the terminal-state idempotency
 * guard. The TypeScript port is authoritative going forward.
 */
import type { EventBridgeEvent, Handler } from "aws-lambda";
import {
  DynamoDBClient,
  QueryCommand,
  UpdateItemCommand,
  ConditionalCheckFailedException,
  type AttributeValue,
} from "@aws-sdk/client-dynamodb";
import {
  S3Client,
  ListObjectsV2Command,
  GetObjectCommand,
} from "@aws-sdk/client-s3";

const RUNS_TABLE = process.env.RUNS_TABLE ?? "";
const ARTIFACTS_BUCKET = process.env.ARTIFACTS_BUCKET ?? "";

const ddb = new DynamoDBClient({});
const s3 = new S3Client({});

interface EcsContainer {
  readonly exitCode?: number;
  readonly name?: string;
}

interface EcsTaskStateChange {
  readonly taskArn?: string;
  readonly lastStatus?: string;
  readonly stoppedAt?: string;
  // stopCode is the ECS-level reason the task stopped, e.g.
  // "TerminationNotice" (spot interruption), "EssentialContainerExited",
  // "TaskFailedToStart", "UserInitiated". stoppedReason is free text.
  readonly stopCode?: string;
  readonly stoppedReason?: string;
  readonly containers?: readonly EcsContainer[];
  readonly clusterArn?: string;
}

type Result =
  | { readonly skipped: string; readonly runId?: string; readonly status?: string }
  | {
      readonly updated: true;
      readonly runId: string;
      readonly status: string;
      readonly exitCode: number | null;
    };

async function findRunId(taskArn: string): Promise<string | null> {
  const out = await ddb.send(
    new QueryCommand({
      TableName: RUNS_TABLE,
      IndexName: "by-instance",
      KeyConditionExpression: "instance_id = :iid",
      ExpressionAttributeValues: { ":iid": { S: taskArn } },
      Limit: 1,
    }),
  );
  const items = out.Items ?? [];
  if (items.length === 0) return null;
  const id = items[0].id;
  return id && "S" in id ? id.S ?? null : null;
}

async function fetchTotalCost(runId: string): Promise<number | null> {
  const prefix = `horde-runs/${runId}/`;
  try {
    const listing = await s3.send(
      new ListObjectsV2Command({ Bucket: ARTIFACTS_BUCKET, Prefix: prefix }),
    );
    const match = (listing.Contents ?? []).find((o) => o.Key?.endsWith("/run-result.json"));
    if (!match?.Key) return null;
    const obj = await s3.send(new GetObjectCommand({ Bucket: ARTIFACTS_BUCKET, Key: match.Key }));
    const body = await obj.Body?.transformToString();
    if (!body) return null;
    const data = JSON.parse(body) as { total_cost_usd?: unknown };
    const raw = data.total_cost_usd;
    if (typeof raw === "number") return raw;
    if (typeof raw === "string" && raw !== "") {
      const n = Number(raw);
      return Number.isFinite(n) ? n : null;
    }
    return null;
  } catch (err) {
    console.log("status-lambda: fetchTotalCost non-fatal error", {
      runId,
      error: err instanceof Error ? err.message : String(err),
    });
    return null;
  }
}

type TerminalStatus = "success" | "failed" | "killed" | "timed_out" | "rate_limited";

// Mirrors internal/provider/docker.go::mapExitCode. orc exit codes:
// 0 success / 2 phase-timeout / 4 cost-or-rate-limit / 5 signal(killed) / else failed.
// timed_out and rate_limited are terminal-but-recoverable.
function mapStatus(exitCode: number | null): TerminalStatus {
  switch (exitCode) {
    case 0:
      return "success";
    case 2:
      return "timed_out";
    case 4:
      return "rate_limited";
    case 5:
      return "killed";
    default:
      return "failed";
  }
}

export const handler: Handler<
  EventBridgeEvent<"ECS Task State Change", EcsTaskStateChange>,
  Result
> = async (event): Promise<Result> => {
  const detail = event.detail ?? ({} as EcsTaskStateChange);
  if (detail.lastStatus !== "STOPPED") {
    console.log("status-lambda: skip, not stopped", { lastStatus: detail.lastStatus });
    return { skipped: "not stopped" };
  }
  const taskArn = detail.taskArn;
  if (!taskArn) {
    console.log("status-lambda: skip, no taskArn");
    return { skipped: "no taskArn" };
  }

  const runId = await findRunId(taskArn);
  if (!runId) {
    console.log("status-lambda: skip, task not managed by horde", { taskArn });
    return { skipped: "task not managed by horde" };
  }

  const containers = detail.containers ?? [];
  const exitCode =
    containers.length > 0 && typeof containers[0].exitCode === "number"
      ? containers[0].exitCode
      : null;
  const status = mapStatus(exitCode);

  const stoppedAt = detail.stoppedAt;
  const cost = await fetchTotalCost(runId);

  const names: Record<string, string> = {
    "#s": "status",
    "#ca": "completed_at",
  };
  const values: Record<string, AttributeValue> = {
    ":s": { S: status },
    ":ca": stoppedAt ? { S: stoppedAt } : { NULL: true },
    ":success": { S: "success" },
    ":failed": { S: "failed" },
    ":killed": { S: "killed" },
    ":timed_out": { S: "timed_out" },
    ":rate_limited": { S: "rate_limited" },
  };
  const setExprs: string[] = ["#s = :s", "#ca = :ca"];
  if (exitCode !== null) {
    names["#ec"] = "exit_code";
    values[":ec"] = { N: String(exitCode) };
    setExprs.push("#ec = :ec");
  }
  if (cost !== null) {
    names["#tc"] = "total_cost_usd";
    values[":tc"] = { N: String(cost) };
    setExprs.push("#tc = :tc");
  }

  // The metadata attribute may not exist (CreateRun omits an empty map). Seed
  // it with if_not_exists in THIS update so the nested stop_code/stop_reason
  // writes below have a parent map to target. The seed and the nested writes
  // cannot share one UpdateExpression — DynamoDB rejects an expression that
  // references both `metadata` and `metadata.x` ("document paths overlap") —
  // so the nested writes go in a second UpdateItem after this one commits.
  const hasStopReason = Boolean(detail.stopCode || detail.stoppedReason);
  if (hasStopReason) {
    names["#m"] = "metadata";
    values[":emptymap"] = { M: {} };
    setExprs.push("#m = if_not_exists(#m, :emptymap)");
  }

  try {
    await ddb.send(
      new UpdateItemCommand({
        TableName: RUNS_TABLE,
        Key: { id: { S: runId } },
        UpdateExpression: "SET " + setExprs.join(", "),
        ConditionExpression:
          "attribute_not_exists(#s) OR NOT (#s IN (:success, :failed, :killed, :timed_out, :rate_limited))",
        ExpressionAttributeNames: names,
        ExpressionAttributeValues: values,
      }),
    );
  } catch (err) {
    if (err instanceof ConditionalCheckFailedException) {
      console.log("status-lambda: skip, already terminal", { runId });
      return { skipped: "already terminal", runId };
    }
    throw err;
  }

  // Second update: write the stop reason into the (now-guaranteed-present)
  // metadata map. Separate call so the seed and the nested paths don't overlap.
  if (hasStopReason) {
    const metaNames: Record<string, string> = { "#m": "metadata" };
    const metaValues: Record<string, AttributeValue> = {};
    const metaSets: string[] = [];
    if (detail.stopCode) {
      metaNames["#sc"] = "stop_code";
      metaValues[":sc"] = { S: detail.stopCode };
      metaSets.push("#m.#sc = :sc");
    }
    if (detail.stoppedReason) {
      metaNames["#sr"] = "stop_reason";
      metaValues[":sr"] = { S: detail.stoppedReason };
      metaSets.push("#m.#sr = :sr");
    }
    await ddb.send(
      new UpdateItemCommand({
        TableName: RUNS_TABLE,
        Key: { id: { S: runId } },
        UpdateExpression: "SET " + metaSets.join(", "),
        ExpressionAttributeNames: metaNames,
        ExpressionAttributeValues: metaValues,
      }),
    );
  }

  console.log("status-lambda: updated", { runId, status, exitCode, hasCost: cost !== null });
  return { updated: true, runId, status, exitCode };
};
