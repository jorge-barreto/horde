/**
 * Status-sync Lambda for the horde worker. Triggered by EventBridge on
 * "ECS Task State Change" where detail.lastStatus == STOPPED. Maps the
 * ECS exit code to a terminal run status, pulls total_cost_usd from
 * run-result.json in S3 (best-effort), and idempotently updates the
 * runs table row keyed by the task ARN in the `by-instance` GSI.
 *
 * Port of the Python lambda in .horde/cloudformation.yaml with one
 * intentional deviation: exit_code 5 maps to "killed" (matching
 * internal/provider/docker.go::resolveStoppedExitCode and the bead 5fh.13
 * specification), while the Python source only distinguishes 0 vs non-zero.
 * The TypeScript port is authoritative going forward.
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

function mapStatus(exitCode: number | null): "success" | "failed" | "killed" {
  if (exitCode === 0) return "success";
  if (exitCode === 5) return "killed";
  return "failed";
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

  try {
    await ddb.send(
      new UpdateItemCommand({
        TableName: RUNS_TABLE,
        Key: { id: { S: runId } },
        UpdateExpression: "SET " + setExprs.join(", "),
        ConditionExpression:
          "attribute_not_exists(#s) OR NOT (#s IN (:success, :failed, :killed))",
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

  console.log("status-lambda: updated", { runId, status, exitCode, hasCost: cost !== null });
  return { updated: true, runId, status, exitCode };
};
