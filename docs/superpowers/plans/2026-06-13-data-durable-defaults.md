# Data-Durable `HordeWorker` Defaults Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the `HordeWorker` CDK construct default its two data stores (DynamoDB runs table, S3 artifacts bucket) to `RETAIN` with PITR on the table, while giving ephemeral/dev stacks an explicit `dataRemovalPolicy: DESTROY` opt-out.

**Architecture:** Add a `DataRemovalPolicy` union type (`RETAIN | DESTROY` only) and two optional props (`dataRemovalPolicy`, `pointInTimeRecovery`) to `HordeWorkerProps`. Wire them into the construct: bucket + table removal policy from the prop (default RETAIN), `autoDeleteObjects` coupled to DESTROY, PITR on the table (default on), and a defensive synth-time throw for a JS-side SNAPSHOT. The e2e stack opts into DESTROY so teardown stays clean. The CFN bootstrap path is untouched.

**Tech Stack:** TypeScript, aws-cdk-lib 2.250.0, Jest (`aws-cdk-lib/assertions` Template/Match + snapshots).

---

## Context for the implementer

- Source lives in `cdk/src/` (`.ts`); `cdk/lib/` is **compiled output** — never hand-edit it. `npm run build` regenerates `lib/`.
- Tests are in `cdk/test/`; run them from inside `cdk/`.
- Two snapshot tests (`cdk/test/horde-worker.test.ts.snap`, `cdk/test/horde-worker-matrix.test.ts.snap`) will change because the default synth now carries RETAIN + PITR and drops the auto-delete custom resource. Regenerate with `npm test -- -u`.
- CDK API note: the boolean `pointInTimeRecovery` prop on `dynamodb.Table` is **deprecated** in 2.250.0. Use `pointInTimeRecoverySpecification: { pointInTimeRecoveryEnabled: <bool> }`. The synthesized CloudFormation key is `PointInTimeRecoverySpecification.PointInTimeRecoveryEnabled`.
- Run all `npm` / `jest` commands with the working directory set to `cdk/` (e.g. `npm --prefix cdk test`), to avoid a `cd` permission prompt.

---

## Task 1: Add `DataRemovalPolicy` type and props

**Files:**
- Modify: `cdk/src/horde-worker-props.ts` (add import, type, two props)
- Modify: `cdk/src/index.ts:2` (re-export the new type)
- Test: `cdk/test/horde-worker-props.test.ts` (compile-time type tests)

- [ ] **Step 1: Write the failing compile-time test**

Append these `it` blocks inside the existing `describe("HordeWorkerProps", ...)` in `cdk/test/horde-worker-props.test.ts`. Add the import at the top of the file alongside the existing import line:

```ts
import { RemovalPolicy } from "aws-cdk-lib";
import type { DataRemovalPolicy, HordeNetworkMode, HordeWorkerProps } from "../src";
```

```ts
  it("accepts RETAIN and DESTROY as DataRemovalPolicy", () => {
    const a: DataRemovalPolicy = RemovalPolicy.RETAIN;
    const b: DataRemovalPolicy = RemovalPolicy.DESTROY;
    expect([a, b]).toEqual([RemovalPolicy.RETAIN, RemovalPolicy.DESTROY]);
  });

  it("rejects SNAPSHOT as DataRemovalPolicy (compile-time)", () => {
    // @ts-expect-error – SNAPSHOT is not a valid DataRemovalPolicy
    const bad: DataRemovalPolicy = RemovalPolicy.SNAPSHOT;
    expect(bad).toBeDefined();
  });

  it("accepts dataRemovalPolicy and pointInTimeRecovery props", () => {
    const partial: Pick<HordeWorkerProps, "dataRemovalPolicy" | "pointInTimeRecovery"> = {
      dataRemovalPolicy: RemovalPolicy.DESTROY,
      pointInTimeRecovery: false,
    };
    expect(partial.dataRemovalPolicy).toBe(RemovalPolicy.DESTROY);
    expect(partial.pointInTimeRecovery).toBe(false);
  });
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `npm --prefix cdk test -- horde-worker-props`
Expected: FAIL — `DataRemovalPolicy` is not exported from `../src`, and `dataRemovalPolicy`/`pointInTimeRecovery` are not on `HordeWorkerProps`. (TypeScript compile errors via ts-jest.)

- [ ] **Step 3: Add the type and props**

In `cdk/src/horde-worker-props.ts`, add to the existing top imports:

```ts
import type { Duration, RemovalPolicy } from "aws-cdk-lib";
```

(The file already imports `Duration` as a type-only import on line 1 — merge `RemovalPolicy` into that same line.)

Add the union type next to the existing `HordeNetworkMode` type (after line 9):

```ts
/**
 * RemovalPolicy values valid for HordeWorker's data stores. Only RETAIN and
 * DESTROY are permitted — SNAPSHOT is excluded because S3 buckets have no
 * snapshot policy, so a single shared SNAPSHOT value would snapshot the
 * DynamoDB table while silently degrading to RETAIN for the artifacts bucket
 * (a "half-snapshot" surprise — exactly the silent-data-loss class this fixes).
 */
export type DataRemovalPolicy = RemovalPolicy.RETAIN | RemovalPolicy.DESTROY;
```

Add the two props inside `HordeWorkerProps`, immediately after the `artifactsBucket?` prop (currently ending line 108):

```ts
  /**
   * RemovalPolicy applied to the two DATA-bearing resources: the DynamoDB runs
   * table and the S3 artifacts bucket. These hold durable run history and run
   * artifacts, so the default is RETAIN — `cdk destroy` tears down compute but
   * leaves your data behind. Set DESTROY for ephemeral/dev stacks you want to
   * fully clean up; that also flips the bucket's autoDeleteObjects on so the
   * delete succeeds on a non-empty bucket. The worker/Lambda log groups are
   * always DESTROY (logs are ephemeral) regardless of this setting. Only
   * applies to resources the construct creates — a caller-provided
   * `artifactsBucket` keeps whatever policy the caller set on it.
   * @default RemovalPolicy.RETAIN
   */
  readonly dataRemovalPolicy?: DataRemovalPolicy;

  /**
   * Enable DynamoDB point-in-time recovery on the runs table — 35-day
   * continuous backup with per-second restore. Cheap insurance for a history
   * table; guards the bad-write / accidental-delete failure that RETAIN (a
   * teardown guard) does not.
   * @default true
   */
  readonly pointInTimeRecovery?: boolean;
```

In `cdk/src/index.ts`, add `DataRemovalPolicy` to the type re-export on line 2:

```ts
export type { DataRemovalPolicy, HordeNetworkMode, HordeWorkerProps, HordeWorkerSecrets } from "./horde-worker-props";
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `npm --prefix cdk test -- horde-worker-props`
Expected: PASS (all `HordeWorkerProps` tests green).

- [ ] **Step 5: Commit**

```bash
git add cdk/src/horde-worker-props.ts cdk/src/index.ts cdk/test/horde-worker-props.test.ts
git commit -m "cdk: add DataRemovalPolicy type and data-durability props (#62)"
```

---

## Task 2: Wire removal policy + PITR into the construct

**Files:**
- Modify: `cdk/src/horde-worker.ts` (resolve prop, defensive throw, bucket, table)
- Test: `cdk/test/horde-worker.test.ts` (new `describe` block for data durability)

- [ ] **Step 1: Write the failing tests**

Add a new `describe` block at the end of `cdk/test/horde-worker.test.ts` (the file already imports `App`, `Stack`, `Match`, `Template`, `ecr`, `ecs`, `secretsmanager`). Add `import { RemovalPolicy } from "aws-cdk-lib";` to the top imports. This block defines its own configurable `synthWith` helper so it can pass the new props:

```ts
import { RemovalPolicy } from "aws-cdk-lib";

describe("HordeWorker data durability (#62)", () => {
  function synthWith(props: { dataRemovalPolicy?: RemovalPolicy; pointInTimeRecovery?: boolean }): Template {
    const app = new App();
    const stack = new Stack(app, "TestStack", {
      env: { account: "111111111111", region: "us-east-1" },
    });
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
      ...props,
    });
    return Template.fromStack(stack);
  }

  it("defaults the runs table to RETAIN", () => {
    synthWith({}).hasResource("AWS::DynamoDB::Table", {
      DeletionPolicy: "Retain",
      UpdateReplacePolicy: "Retain",
    });
  });

  it("defaults the artifacts bucket to RETAIN", () => {
    synthWith({}).hasResource("AWS::S3::Bucket", {
      DeletionPolicy: "Retain",
      UpdateReplacePolicy: "Retain",
    });
  });

  it("enables point-in-time recovery on the runs table by default", () => {
    synthWith({}).hasResourceProperties("AWS::DynamoDB::Table", {
      PointInTimeRecoverySpecification: { PointInTimeRecoveryEnabled: true },
    });
  });

  it("does not create an auto-delete custom resource by default", () => {
    synthWith({}).resourceCountIs("Custom::S3AutoDeleteObjects", 0);
  });

  it("sets DESTROY on both data stores when dataRemovalPolicy is DESTROY", () => {
    const t = synthWith({ dataRemovalPolicy: RemovalPolicy.DESTROY });
    t.hasResource("AWS::DynamoDB::Table", { DeletionPolicy: "Delete" });
    t.hasResource("AWS::S3::Bucket", { DeletionPolicy: "Delete" });
  });

  it("creates the auto-delete custom resource when dataRemovalPolicy is DESTROY", () => {
    synthWith({ dataRemovalPolicy: RemovalPolicy.DESTROY }).resourceCountIs(
      "Custom::S3AutoDeleteObjects",
      1,
    );
  });

  it("disables PITR when pointInTimeRecovery is false", () => {
    synthWith({ pointInTimeRecovery: false }).hasResourceProperties("AWS::DynamoDB::Table", {
      PointInTimeRecoverySpecification: { PointInTimeRecoveryEnabled: false },
    });
  });

  it("throws at synth if a SNAPSHOT policy is forced through (JS-side bypass)", () => {
    expect(() =>
      // cast bypasses the DataRemovalPolicy union to simulate a JS caller
      synthWith({ dataRemovalPolicy: RemovalPolicy.SNAPSHOT as RemovalPolicy }),
    ).toThrow(/SNAPSHOT is not supported/);
  });
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `npm --prefix cdk test -- horde-worker.test`
Expected: FAIL — table/bucket synth as `Delete` (not `Retain`), no `PointInTimeRecoverySpecification`, the auto-delete custom resource is present by default, and SNAPSHOT does not throw.

- [ ] **Step 3: Implement the wiring**

In `cdk/src/horde-worker.ts`, inside the constructor, just after the existing default-resolving block (after `const networkMode = props.networkMode ?? "public";`, currently line 179), add:

```ts
    const dataRemovalPolicy = props.dataRemovalPolicy ?? cdk.RemovalPolicy.RETAIN;
    // The DataRemovalPolicy type already excludes SNAPSHOT, but RemovalPolicy is
    // a plain enum so a JS / `any`-typed caller can still slip it through. S3 has
    // no snapshot policy (it would silently degrade to RETAIN for the bucket),
    // so fail loudly at synth rather than ship a half-snapshot posture.
    if (dataRemovalPolicy === cdk.RemovalPolicy.SNAPSHOT) {
      throw new Error(
        "HordeWorker: dataRemovalPolicy SNAPSHOT is not supported — S3 buckets " +
          "have no snapshot policy. Use RemovalPolicy.RETAIN (default) or DESTROY.",
      );
    }
```

Change the ArtifactsBucket creation (currently lines 251-258) from:

```ts
      const bucket = new s3.Bucket(this, "ArtifactsBucket", {
        bucketName: `horde-artifacts-${slug}-${cdk.Aws.ACCOUNT_ID}`,
        encryption: s3.BucketEncryption.S3_MANAGED,
        blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
        enforceSSL: true,
        removalPolicy: cdk.RemovalPolicy.DESTROY,
        autoDeleteObjects: true,
      });
```

to:

```ts
      const bucket = new s3.Bucket(this, "ArtifactsBucket", {
        bucketName: `horde-artifacts-${slug}-${cdk.Aws.ACCOUNT_ID}`,
        encryption: s3.BucketEncryption.S3_MANAGED,
        blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
        enforceSSL: true,
        removalPolicy: dataRemovalPolicy,
        // Only auto-empty when we are actually destroying. With RETAIN,
        // autoDeleteObjects is meaningless and CDK warns; coupling it to DESTROY
        // keeps the two coherent and drops the custom resource from the default.
        autoDeleteObjects: dataRemovalPolicy === cdk.RemovalPolicy.DESTROY,
      });
```

Change the RunsTable creation (currently lines 276-281) from:

```ts
    this.runsTable = new dynamodb.Table(this, "RunsTable", {
      tableName: `horde-runs-${slug}`,
      partitionKey: { name: "id", type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      removalPolicy: cdk.RemovalPolicy.DESTROY,
    });
```

to:

```ts
    this.runsTable = new dynamodb.Table(this, "RunsTable", {
      tableName: `horde-runs-${slug}`,
      partitionKey: { name: "id", type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      removalPolicy: dataRemovalPolicy,
      pointInTimeRecoverySpecification: {
        pointInTimeRecoveryEnabled: props.pointInTimeRecovery ?? true,
      },
    });
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `npm --prefix cdk test -- horde-worker.test`
Expected: PASS for the new `data durability` block. The two `toMatchSnapshot` assertions in this file (and the matrix file) may now FAIL — that is expected and handled in Task 3.

- [ ] **Step 5: Commit**

```bash
git add cdk/src/horde-worker.ts cdk/test/horde-worker.test.ts
git commit -m "cdk: default data stores to RETAIN + PITR, opt-out via dataRemovalPolicy (#62)"
```

---

## Task 3: Regenerate snapshots and verify the matrix path

**Files:**
- Modify: `cdk/test/horde-worker.test.ts.snap` (regenerated)
- Modify: `cdk/test/horde-worker-matrix.test.ts.snap` (regenerated)
- Test: `cdk/test/horde-worker-matrix.test.ts` (add a provided-bucket policy assertion)

- [ ] **Step 1: Add a matrix assertion that a caller-provided bucket is untouched**

In `cdk/test/horde-worker-matrix.test.ts`, find the existing `describe.each(cells)` structural-assertions block (the one that already references `ourBucket` / `HordeArtifactsBucket` around line 81-83). Add this `it` inside the structural `describe.each` (it only runs meaningfully for the `withBucket: true` cells, so guard on the cell):

```ts
  it("never stamps a DeletionPolicy on a caller-provided bucket", () => {
    const { template } = buildStack(opts);
    if (opts.withBucket) {
      // The construct must not create its own ArtifactsBucket when one is given.
      const buckets = template.findResources("AWS::S3::Bucket");
      const ours = Object.keys(buckets).filter((id) => id.startsWith("HordeArtifactsBucket"));
      expect(ours).toHaveLength(0);
    }
  });
```

(If the exact `describe.each` signature differs, place the `it` next to the existing `ourBucket` assertion so it shares the same `opts` binding.)

- [ ] **Step 2: Run to verify the new assertion passes and snapshots fail**

Run: `npm --prefix cdk test`
Expected: the new matrix assertion PASSES; the two snapshot tests FAIL with "snapshot does not match" (the default template now has Retain + PITR and no auto-delete custom resource). This confirms the snapshots are the only stale artifact.

- [ ] **Step 3: Regenerate the snapshots**

Run: `npm --prefix cdk test -- -u`
Expected: PASS, snapshots rewritten.

- [ ] **Step 4: Sanity-check the regenerated snapshot diff**

Run: `git --no-pager diff -- cdk/test/horde-worker.test.ts.snap cdk/test/horde-worker-matrix.test.ts.snap`
Expected: the diff shows, for the default cells, the DynamoDB table gaining `DeletionPolicy: Retain` / `UpdateReplacePolicy: Retain` + `PointInTimeRecoverySpecification`, the S3 bucket gaining `Retain`, and the `Custom::S3AutoDeleteObjects` resource + its Lambda/role/policy being removed. No unrelated churn. If anything else changed, stop and investigate.

- [ ] **Step 5: Commit**

```bash
git add cdk/test/horde-worker-matrix.test.ts cdk/test/horde-worker.test.ts.snap cdk/test/horde-worker-matrix.test.ts.snap
git commit -m "cdk: regenerate snapshots for RETAIN+PITR default (#62)"
```

---

## Task 4: e2e stack opts into DESTROY

**Files:**
- Modify: `cdk/e2e/app.ts` (add `dataRemovalPolicy: DESTROY` to the `HordeWorker` props)

- [ ] **Step 1: Add the opt-out to the e2e worker**

In `cdk/e2e/app.ts`, inside the `new HordeWorker(stack, "Worker", { ... })` props (the block starting line 61), add the following near the top of the props object, right after `projectSlug: SLUG,`:

```ts
  // E2E is ephemeral: data RETAIN-by-default would orphan the runs table +
  // artifacts bucket (and a small cost) after every `make e2e-down`. Opt into
  // DESTROY so teardown is clean; this also exercises the opt-out path live.
  // Production consumers omit this prop and keep the RETAIN default.
  dataRemovalPolicy: cdk.RemovalPolicy.DESTROY,
```

(`cdk` is already imported as `import * as cdk from "aws-cdk-lib";` at the top of the file.)

- [ ] **Step 2: Build to verify the e2e app type-checks**

Run: `npm --prefix cdk run build`
Expected: `tsc` succeeds (no type error on the new prop; `RemovalPolicy.DESTROY` satisfies `DataRemovalPolicy`).

- [ ] **Step 3: Commit**

```bash
git add cdk/e2e/app.ts
git commit -m "cdk(e2e): opt into DESTROY for ephemeral data stores (#62)"
```

---

## Task 5: Document teardown behavior in the README

**Files:**
- Modify: `cdk/README.md` (add a "Data durability & teardown" section)

- [ ] **Step 1: Add the documentation section**

In `cdk/README.md`, insert a new section immediately before the `## Sidecar containers` heading (currently line 49):

````markdown
## Data durability & teardown

`HordeWorker` provisions two **data-bearing** resources — the DynamoDB runs
table (run history + GSIs) and the S3 artifacts bucket (per-run plans, test/
review output, logs). They default to `RemovalPolicy.RETAIN`, so a `cdk destroy`
(or a `cdk deploy` that replaces them) tears down compute, networking, and log
groups but **leaves your run history and artifacts behind**. The runs table also
has point-in-time recovery enabled by default (35-day continuous backup), which
guards accidental writes/deletes that RETAIN alone does not.

For an ephemeral or dev stack you want to fully clean up, opt into destruction:

```ts
import { RemovalPolicy } from "aws-cdk-lib";

new HordeWorker(stack, "Horde", {
  // ...required props...
  dataRemovalPolicy: RemovalPolicy.DESTROY, // table + bucket deleted on `cdk destroy`
  pointInTimeRecovery: false,               // optional: skip PITR for throwaway stacks
});
```

`dataRemovalPolicy: DESTROY` also enables the bucket's auto-delete so a non-empty
bucket is emptied before removal. Only `RETAIN` and `DESTROY` are accepted
(`SNAPSHOT` is rejected — S3 has no snapshot policy). A **caller-provided**
`artifactsBucket` is never re-policied by the construct; it keeps whatever policy
you set on it. Log groups are always removed on teardown regardless of this prop.
````

- [ ] **Step 2: Verify the README renders sensibly**

Run: `git --no-pager diff -- cdk/README.md`
Expected: the new section appears between the `repo` paragraph and `## Sidecar containers`, with a fenced `ts` example. No other content changed.

- [ ] **Step 3: Commit**

```bash
git add cdk/README.md
git commit -m "cdk: document data durability & teardown behavior (#62)"
```

---

## Task 6: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Run the full CDK build + test suite**

Run: `npm --prefix cdk run build && npm --prefix cdk test`
Expected: `tsc` clean, all Jest suites PASS including the regenerated snapshots. This is the same command the `cdk-test` CI job runs.

- [ ] **Step 2: Confirm no stray edits to compiled output or the CFN path**

Run: `git status --short`
Expected: a clean tree (everything committed). If `cdk/lib/**` shows as modified, that is the rebuilt output — it is normally gitignored; confirm with `git check-ignore cdk/lib/horde-worker.js` (should print the path if ignored). Do NOT commit `cdk/lib/**` unless the repo already tracks it. `internal/bootstrap/templates/stack.yaml.tmpl` must be unchanged (CFN path is intentionally out of scope).

- [ ] **Step 3: Confirm the issue's acceptance criteria are met**

Verify by inspection against issue #62:
- RunsTable + ArtifactsBucket default to RETAIN ✓ (Task 2 tests)
- `autoDeleteObjects` dropped from the default ✓ (coupled to DESTROY)
- opt-out prop exposed ✓ (`dataRemovalPolicy`)
- PITR on by default ✓ (`pointInTimeRecovery` default true)
- teardown behavior documented ✓ (Task 5)

---

## Self-review notes

- **Spec coverage:** props (Task 1) · construct wiring incl. SNAPSHOT guard + autoDeleteObjects coupling + PITR (Task 2) · snapshot regen + provided-bucket guard (Task 3) · e2e opt-out (Task 4) · README (Task 5) · CFN explicitly untouched (verified Task 6). All spec sections map to a task.
- **Type consistency:** `DataRemovalPolicy = RemovalPolicy.RETAIN | RemovalPolicy.DESTROY`, prop names `dataRemovalPolicy` / `pointInTimeRecovery`, and the DynamoDB `pointInTimeRecoverySpecification.pointInTimeRecoveryEnabled` API are used identically across Tasks 1–5.
- **CDK API:** uses the non-deprecated `pointInTimeRecoverySpecification` (deprecated boolean avoided); CloudFormation key `PointInTimeRecoverySpecification.PointInTimeRecoveryEnabled` matches the test assertions.
- **Release:** out of scope for these tasks; propose a lockstep minor bump (0.6.0 → 0.7.0) after the plan completes.
