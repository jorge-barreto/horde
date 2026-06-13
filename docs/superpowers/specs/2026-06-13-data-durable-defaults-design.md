# Data-durable defaults for `HordeWorker` (issue #62)

**Date:** 2026-06-13
**Scope:** CDK construct only (`@horde.io/cdk`). No Go, SSM-contract, or bootstrap-CloudFormation changes. The CFN bootstrap path (`internal/bootstrap/templates/stack.yaml.tmpl`) is intentionally left untouched — it is being retired, and per the issue triage this fix is CDK-first.

## Problem

`HordeWorker` creates its two **data-bearing** resources with `RemovalPolicy.DESTROY`:

- the **DynamoDB runs table** (`RunsTable`, `horde-worker.ts:276-281`) — all run history (id/status/phase, plus the by-repo / by-ticket / by-status / by-instance GSIs), and
- the **S3 artifacts bucket** (`ArtifactsBucket`, `horde-worker.ts:251-258`) — every run's plan, test output, review output, and logs. The bucket additionally sets `autoDeleteObjects: true`.

As a result, a routine `cdk destroy` (or a `cdk deploy` that happens to replace either resource) **silently and irrecoverably deletes all run history and all run artifacts**, with no backup and no prompt. There is no PITR on the table either, so a bad write/delete is also unrecoverable.

This already bit a downstream consumer (PrepDesk): a cost-saving teardown of an idle factory stack deleted the entire run history and artifacts bucket. The ECR repo and Secrets survived (they are `RETAIN`/external), which made the loss especially surprising — the stack kept worker *images* but threw away run *history*.

These are durable user data, not ephemeral infrastructure. Tearing down compute should never throw away history. The fix is to default the two stores to `RETAIN`, enable PITR on the table, and give ephemeral/dev stacks an explicit opt-out.

The `LogGroup` resources (worker, status Lambda, drain Lambda) keep `DESTROY` — logs are ephemeral and this issue does not touch them.

## Design

### 1. New props (`horde-worker-props.ts`)

A purpose-built union type so the prop can only carry the two values that make sense for this construct:

```ts
import { RemovalPolicy } from "aws-cdk-lib";

/**
 * RemovalPolicy values valid for HordeWorker's data stores. Only RETAIN and
 * DESTROY are permitted — SNAPSHOT is excluded because S3 buckets have no
 * snapshot policy, so a single shared SNAPSHOT value would snapshot the
 * DynamoDB table while silently degrading to RETAIN for the artifacts bucket
 * (a "half-snapshot" surprise — exactly the silent-data-loss class this fixes).
 */
export type DataRemovalPolicy = RemovalPolicy.RETAIN | RemovalPolicy.DESTROY;
```

```ts
/**
 * RemovalPolicy applied to the two DATA-bearing resources: the DynamoDB runs
 * table and the S3 artifacts bucket. These hold durable run history and run
 * artifacts, so the default is RETAIN — `cdk destroy` tears down compute but
 * leaves your data behind. Set DESTROY for ephemeral/dev stacks you want to
 * fully clean up; that also flips the bucket's autoDeleteObjects on so the
 * delete actually succeeds on a non-empty bucket. The worker/Lambda log groups
 * are always DESTROY (logs are ephemeral) regardless of this setting.
 * @default RemovalPolicy.RETAIN
 */
readonly dataRemovalPolicy?: DataRemovalPolicy;

/**
 * Enable DynamoDB point-in-time recovery on the runs table — 35-day continuous
 * backup with per-second restore. Cheap insurance for a history table; guards
 * the bad-write/accidental-delete failure that RETAIN (a teardown guard) does
 * not.
 * @default true
 */
readonly pointInTimeRecovery?: boolean;
```

### 2. Construct wiring (`horde-worker.ts`)

```ts
const dataRemovalPolicy = props.dataRemovalPolicy ?? cdk.RemovalPolicy.RETAIN;

// Defensive: the type already excludes SNAPSHOT, but RemovalPolicy is a plain
// enum, so a JS / `any`-typed caller can still slip it through. Fail loudly at
// synth rather than silently mis-retaining the bucket.
if (dataRemovalPolicy === cdk.RemovalPolicy.SNAPSHOT) {
  throw new Error(
    "HordeWorker: dataRemovalPolicy SNAPSHOT is not supported — S3 buckets " +
      "have no snapshot policy. Use RemovalPolicy.RETAIN (default) or DESTROY.",
  );
}
```

- **ArtifactsBucket** (the `else` branch that creates our own bucket):
  - `removalPolicy: dataRemovalPolicy`
  - `autoDeleteObjects: dataRemovalPolicy === cdk.RemovalPolicy.DESTROY` — only auto-empty when we are actually destroying. With RETAIN, `autoDeleteObjects` is meaningless and CDK emits a warning; coupling it to DESTROY keeps the two coherent and removes the auto-delete custom resource from the default (RETAIN) synth.
- **RunsTable**:
  - `removalPolicy: dataRemovalPolicy`
  - `pointInTimeRecovery: props.pointInTimeRecovery ?? true`
- A caller-supplied `artifactsBucket` (the `if (props.artifactsBucket)` branch) is untouched — we only set policy on resources we create. Same for all other external/caller resources.
- Log groups unchanged (`DESTROY`).

### 3. e2e stack (`cdk/e2e/app.ts`)

Add `dataRemovalPolicy: cdk.RemovalPolicy.DESTROY` to the `HordeWorker` props. RETAIN-by-default would otherwise orphan the runs table + artifacts bucket (and leave a small cost) after every `make e2e-down`. This matches the file's existing `DESTROY` on its ECR repo and secrets, keeps the dev/CI teardown loop clean, and doubles as live coverage of the opt-out path.

### 4. Tests (`cdk/test/`)

- **Default stack** (no `dataRemovalPolicy`): `RunsTable` and `ArtifactsBucket` carry `DeletionPolicy: Retain` and `UpdateReplacePolicy: Retain`; the table has `PointInTimeRecoverySpecification.PointInTimeRecoveryEnabled: true`; **no** `Custom::S3AutoDeleteObjects` resource is synthesized.
- **`dataRemovalPolicy: DESTROY` stack**: both resources `Delete`; the `autoDeleteObjects` custom resource is present.
- **`pointInTimeRecovery: false`**: table synthesizes without PITR enabled.
- **SNAPSHOT forced through** (cast to bypass the type): synth throws the clear error.
- Regenerate the two `.snap` snapshots (`jest -u`) — RETAIN + PITR change the default synthesized template.

### 5. Docs (`cdk/README.md`)

Add a short **"Data durability & teardown"** section: what survives `cdk destroy` by default (run history + artifacts RETAINed; compute, networking, and logs removed), how to opt into full teardown (`dataRemovalPolicy: RemovalPolicy.DESTROY`), and the PITR default. Satisfies issue request #4; the props are also self-documenting via the JSDoc above.

## Out of scope

- **CFN bootstrap template** — left as-is. (Per issue triage + CLAUDE.md: the CloudFormation onboarding path is being retired; this fix is CDK-first.)
- No new prop for `autoDeleteObjects` — it is derived from `dataRemovalPolicy` (a separate prop would only let a caller create the incoherent RETAIN-but-auto-delete combination).

## Release

User-facing CDK behavior change → propose a **minor** bump in lockstep when the work is complete (0.6.0 → 0.7.0), following the repo's both-artifacts-same-number convention. Proposed at the end, not part of this change.
