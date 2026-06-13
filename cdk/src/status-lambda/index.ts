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
 * Exit-code mapping (0=success, 2=timed_out, 4=rate_limited, 5=killed, else
 * failed), the metadata stop-reason capture, and the terminal-state
 * idempotency guard are the contract the Go lazy-reconciliation path
 * (internal/provider/ecs.go::Status) must agree with.
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
import { EventBridgeClient, PutEventsCommand } from "@aws-sdk/client-eventbridge";

const RUNS_TABLE = process.env.RUNS_TABLE ?? "";
const ARTIFACTS_BUCKET = process.env.ARTIFACTS_BUCKET ?? "";

// The worker container's name in the task definition. Run status is derived
// from THIS container's exit code, never a sidecar's. Must stay equal to
// WORKER_CONTAINER_NAME in ../horde-worker.ts — this Lambda is esbuild-bundled
// standalone and cannot import construct code, so it keeps its own copy. The
// Go const in internal/provider/ecs.go must also match.
const WORKER_CONTAINER_NAME = "horde-worker";

const ddb = new DynamoDBClient({});
const s3 = new S3Client({});
const eb = new EventBridgeClient({});

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
    }
  | { readonly requeued: string };

async function findRun(
  taskArn: string,
): Promise<{ runId: string; repo: string; labels: Record<string, string>; resumeCount: number } | null> {
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
  const runId = id && "S" in id ? id.S ?? null : null;
  if (!runId) return null;
  // repo comes from the by-instance GSI item (ProjectionType ALL). The drain
  // Lambda needs it to query this repo's queued backlog off the run.terminal event.
  const repoAttr = items[0].repo;
  const repo = repoAttr && "S" in repoAttr ? repoAttr.S ?? "" : "";
  // labels (a DynamoDB Map of String values) are carried on run.terminal so the
  // event Detail shape is consistent with run.started (which the Go CLI emits
  // with labels) — letting external consumers filter both event types uniformly.
  const labels: Record<string, string> = {};
  const labelsAttr = items[0].labels;
  if (labelsAttr && "M" in labelsAttr && labelsAttr.M) {
    for (const [k, v] of Object.entries(labelsAttr.M)) {
      if (v && "S" in v && v.S !== undefined) labels[k] = v.S;
    }
  }
  const rcAttr = items[0].resume_count;
  const resumeCount = rcAttr && "N" in rcAttr ? Number(rcAttr.N ?? "0") : 0;
  return { runId, repo, labels, resumeCount };
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

interface TokenUsage {
  readonly input_tokens: number;
  readonly output_tokens: number;
  readonly cache_creation_tokens: number;
  readonly cache_read_tokens: number;
  readonly turns: number;
}

// fetchTokenUsage pulls per-run token totals from costs.json in S3 (best-effort,
// mirrors fetchTotalCost). orc writes the token totals to costs.json today;
// turns is summed across the per-phase array (orc has no run-total turns).
// Attribute names written downstream match the Go store consts — a
// cross-language contract.
async function fetchTokenUsage(runId: string): Promise<TokenUsage | null> {
  const prefix = `horde-runs/${runId}/`;
  try {
    const listing = await s3.send(
      new ListObjectsV2Command({ Bucket: ARTIFACTS_BUCKET, Prefix: prefix }),
    );
    const match = (listing.Contents ?? []).find((o) => o.Key?.endsWith("/costs.json"));
    if (!match?.Key) return null;
    const obj = await s3.send(new GetObjectCommand({ Bucket: ARTIFACTS_BUCKET, Key: match.Key }));
    const body = await obj.Body?.transformToString();
    if (!body) return null;
    const data = JSON.parse(body) as {
      total_input_tokens?: number;
      total_output_tokens?: number;
      total_cache_creation_input_tokens?: number;
      total_cache_read_input_tokens?: number;
      phases?: ReadonlyArray<{ turns?: number }>;
    };
    // Coerce to an integer token count: accept a number or numeric string,
    // truncate toward zero, reject booleans and anything non-finite -> 0. orc
    // emits integers, so this only matters for malformed costs.json — but the
    // Go store must read the same values back.
    const num = (v: unknown): number => {
      if (typeof v === "boolean") return 0;
      const n = typeof v === "number" ? v : Number(v);
      return Number.isFinite(n) ? Math.trunc(n) : 0;
    };
    const turns = (data.phases ?? []).reduce((acc, p) => acc + num(p.turns), 0);
    return {
      input_tokens: num(data.total_input_tokens),
      output_tokens: num(data.total_output_tokens),
      cache_creation_tokens: num(data.total_cache_creation_input_tokens),
      cache_read_tokens: num(data.total_cache_read_input_tokens),
      turns,
    };
  } catch (err) {
    console.log("status-lambda: fetchTokenUsage non-fatal error", {
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

  const found = await findRun(taskArn);
  if (!found) {
    console.log("status-lambda: skip, task not managed by horde", { taskArn });
    return { skipped: "task not managed by horde" };
  }
  const { runId, repo, labels, resumeCount } = found;

  // Status is the WORKER container's exit code, not the task's. With sidecars
  // in the task def the event's `containers` order is not guaranteed, so find
  // the worker by name; fall back to the first container for legacy
  // single-container events that may omit the name field.
  const containers = detail.containers ?? [];
  const worker =
    containers.find((c) => c.name === WORKER_CONTAINER_NAME) ?? containers[0];
  const exitCode =
    worker && typeof worker.exitCode === "number" ? worker.exitCode : null;
  const status = mapStatus(exitCode);

  const stoppedAt = detail.stoppedAt;
  const cost = await fetchTotalCost(runId);
  const tokens = await fetchTokenUsage(runId);

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
    ":cancelled": { S: "cancelled" },
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
  if (tokens !== null) {
    names["#it"] = "input_tokens";
    names["#ot"] = "output_tokens";
    names["#cct"] = "cache_creation_tokens";
    names["#crt"] = "cache_read_tokens";
    names["#tn"] = "turns";
    values[":it"] = { N: String(tokens.input_tokens) };
    values[":ot"] = { N: String(tokens.output_tokens) };
    values[":cct"] = { N: String(tokens.cache_creation_tokens) };
    values[":crt"] = { N: String(tokens.cache_read_tokens) };
    values[":tn"] = { N: String(tokens.turns) };
    setExprs.push("#it = :it", "#ot = :ot", "#cct = :cct", "#crt = :crt", "#tn = :tn");
  }

  // Record the ECS stop reason FIRST and unconditionally. It is diagnostic,
  // idempotent metadata that must land even when the status is already
  // terminal — e.g. `horde kill` sets `killed` synchronously, so the status
  // update below is skipped by the terminal guard, but we still want the
  // stop_code/stop_reason recorded (and the spot follow-up keys off it).
  // Done as two writes: seed `metadata` with if_not_exists, then the nested
  // keys — they can't share one expression (DynamoDB "document paths overlap").
  if (detail.stopCode || detail.stoppedReason) {
    await ddb.send(
      new UpdateItemCommand({
        TableName: RUNS_TABLE,
        Key: { id: { S: runId } },
        UpdateExpression: "SET #m = if_not_exists(#m, :emptymap)",
        ExpressionAttributeNames: { "#m": "metadata" },
        ExpressionAttributeValues: { ":emptymap": { M: {} } },
      }),
    );
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

  // Spot auto-resume: a TerminationNotice means AWS reclaimed the task, not a
  // failure. If the run is under its resume budget, re-queue it (status=queued,
  // top priority, resume_count++) so the drain re-launches it with the same run
  // ID — workspace + session already synced to S3. Conditional on
  // not-already-terminal so a racing `horde kill` (UserInitiated, sets killed
  // synchronously) wins. UserInitiated never reaches this branch. At the cap we
  // fall through to the normal terminal write below.
  const maxResumes = Number(process.env.MAX_RESUMES ?? "5");
  if (detail.stopCode === "TerminationNotice" && resumeCount < maxResumes) {
    try {
      // REMOVE instance_id de-indexes the dead task ARN from the by-instance
      // GSI (sparse) so late events for the dead task can't match. started_at is
      // RESET to the zero-time sentinel — NOT removed — because it is the by-repo
      // GSI RANGE key: DynamoDB drops any item missing a GSI key attribute from
      // that index, and both drainers find queued runs ONLY via the by-repo GSI,
      // so removing started_at would silently lose the run. The drain re-sets it
      // to the real launch time at claim. completed_at/exit_code are intentionally
      // NOT touched: the re-queue path returns before the terminal write that
      // would set them, so they're absent on a resuming run — nothing stale to clear.
      await ddb.send(
        new UpdateItemCommand({
          TableName: RUNS_TABLE,
          Key: { id: { S: runId } },
          UpdateExpression:
            "SET #s = :queued, #prio = :highest, resume_count = :rc, started_at = :zerotime REMOVE instance_id",
          ConditionExpression:
            "attribute_not_exists(#s) OR NOT (#s IN (:success, :failed, :killed, :timed_out, :rate_limited, :cancelled))",
          ExpressionAttributeNames: { "#s": "status", "#prio": "priority" },
          ExpressionAttributeValues: {
            ":queued": { S: "queued" },
            ":highest": { S: "highest" },
            ":rc": { N: String(resumeCount + 1) },
            ":zerotime": { S: "0001-01-01T00:00:00Z" },
            ":success": { S: "success" },
            ":failed": { S: "failed" },
            ":killed": { S: "killed" },
            ":timed_out": { S: "timed_out" },
            ":rate_limited": { S: "rate_limited" },
            ":cancelled": { S: "cancelled" },
          },
        }),
      );
      console.log("status-lambda: re-queued Spot-interrupted run", {
        runId,
        resumeCount: resumeCount + 1,
      });
      // Emit run.requeued (NOT run.terminal — a re-queued run is a continuation,
      // and run.terminal is a public contract external consumers rely on). The
      // drain rule subscribes to run.requeued too; the Detail carries `repo` so
      // the drain can find this repo's backlog. Best-effort, after the
      // authoritative re-queue write — a failed PutEvents never reverses it.
      const eventBusName = process.env.EVENT_BUS_NAME ?? "";
      if (eventBusName) {
        try {
          await eb.send(
            new PutEventsCommand({
              Entries: [
                {
                  EventBusName: eventBusName,
                  Source: "horde",
                  DetailType: "run.requeued",
                  Detail: JSON.stringify({
                    version: 1,
                    run_id: runId,
                    repo,
                    status: "queued",
                    resume_count: resumeCount + 1,
                    stop_code: detail.stopCode ?? "",
                  }),
                },
              ],
            }),
          );
        } catch (err) {
          console.error("status-lambda: emit run.requeued failed (non-fatal)", { runId, err });
        }
      }
      return { requeued: runId };
    } catch (err) {
      if (err instanceof ConditionalCheckFailedException) {
        console.log("status-lambda: run already terminal, not re-queuing (kill race)", { runId });
        return { skipped: "already terminal", runId };
      }
      throw err;
    }
  }

  // Status update, guarded so a duplicate/late event can't overwrite a
  // run that already reached a terminal state.
  try {
    await ddb.send(
      new UpdateItemCommand({
        TableName: RUNS_TABLE,
        Key: { id: { S: runId } },
        UpdateExpression: "SET " + setExprs.join(", "),
        ConditionExpression:
          "attribute_not_exists(#s) OR NOT (#s IN (:success, :failed, :killed, :timed_out, :rate_limited, :cancelled))",
        ExpressionAttributeNames: names,
        ExpressionAttributeValues: values,
      }),
    );
  } catch (err) {
    if (err instanceof ConditionalCheckFailedException) {
      console.log("status-lambda: status already terminal (stop reason still recorded)", { runId });
      return { skipped: "already terminal", runId };
    }
    throw err;
  }

  // Emit run.terminal to the bus AFTER the authoritative DynamoDB write (only on
  // the branch where the terminal write actually applied — not the already-
  // terminal CCFE path, which returned above). Best-effort: a failed PutEvents
  // logs but never reverses the store write. EVENT_BUS_NAME is read at call time
  // so deployments without the bus (or tests) simply skip emission. The Detail
  // is horde-vocabulary and carries `repo` so the drain Lambda can find this
  // repo's queued backlog. Stable, versioned public contract (see internal/event).
  const eventBusName = process.env.EVENT_BUS_NAME ?? "";
  if (eventBusName) {
    try {
      await eb.send(
        new PutEventsCommand({
          Entries: [
            {
              EventBusName: eventBusName,
              Source: "horde",
              DetailType: "run.terminal",
              Detail: JSON.stringify({
                version: 1,
                run_id: runId,
                repo,
                status,
                exit_code: exitCode,
                total_cost_usd: cost,
                labels,
                stop_code: detail.stopCode ?? "",
                stop_reason: detail.stoppedReason ?? "",
              }),
            },
          ],
        }),
      );
    } catch (err) {
      console.error("status-lambda: emit run.terminal failed (non-fatal)", { runId, err });
    }
  }

  console.log("status-lambda: updated", { runId, status, exitCode, hasCost: cost !== null, hasTokens: tokens !== null });
  return { updated: true, runId, status, exitCode };
};
