# Sidecar containers in the worker task definition

Issue: https://github.com/jorge-barreto/horde/issues/7

## Context

Some projects need auxiliary containers running alongside the worker for the
duration of a run: a Postgres/MySQL/Redis instance the test suite reaches on
`localhost`, a headless Playwright/Selenium server, a Stripe/LocalStack mock,
or a policy-mandated OTel sidecar. Today the `HordeWorker` CDK construct
provisions a single-container Fargate task definition, so users bake the
service into the worker image and start it from the entrypoint. That bloats the
image, ties `initdb` to image-build time, and forces both processes to share one
container's memory — prepdesk had to bump its task from 4 GB to 8 GB to fit
embedded Postgres. A sidecar is the Fargate-native answer.

Triage (issue comment) scoped v1:

- **CDK-only.** A `sidecars` prop on `HordeWorkerProps`. The construct builds
  `ContainerDefinitions` programmatically, so sidecars are just additional
  `addContainer` calls.
- **Docker provider parity: explicit non-goal.** No compose synthesis / docker
  network for `--provider docker`.
- **CFN bootstrap path: deferred.** Templating arbitrary `ContainerDefinitions`
  YAML via Go `text/template` is the wrong tool. The Python status Lambda in the
  template still changes (lockstep, below), but no sidecar *prop* surface is
  added to bootstrap.

### The load-bearing correctness trap

Status sync keys off the **worker container's** exit, not the task's. Both
status Lambdas (TypeScript in `cdk/src/status-lambda/index.ts`, Python in
`internal/bootstrap/templates/stack.yaml.tmpl`) currently read
`containers[0].exitCode` with no name filter. The ECS `Task State Change`
event's `containers` array order is **not** documented to be stable. Once
sidecars exist, a sidecar could land at index 0 and the Lambda would record the
sidecar's exit code as the run status — a silent wrong-status bug. This is the
part the triage flagged as most likely to bite.

## Design

### 1. The `sidecars` prop (thin wrapper over CDK's type)

Add to `HordeWorkerProps` (`cdk/src/horde-worker-props.ts`):

```ts
/**
 * Additional containers added to the worker's Fargate task definition,
 * sharing its network namespace (reachable on localhost) and lifecycle.
 *
 * Each entry is passed to taskDefinition.addContainer() with two defaults
 * applied when omitted:
 *   - essential: false  — a crashing sidecar does not kill the run. Set
 *     true for a dependency the worker cannot run without (e.g. a DB).
 *   - logging          — routed to the worker's CloudWatch log group with
 *     the sidecar's containerName as the stream prefix.
 *
 * memoryMiB sizes the whole task; sidecars share that ceiling unless you set
 * a per-container memoryLimitMiB. The containerName "horde-worker" is reserved
 * and rejected at synth time.
 *
 * @default — no sidecars; single-container task definition.
 */
readonly sidecars?: ecs.ContainerDefinitionOptions[];
```

Reuse CDK's native `ContainerDefinitionOptions` rather than a curated subset —
callers get `healthCheck`, `dependsOn`, `portMappings`, `command`,
`memoryLimitMiB`, `secrets` for free, and there's no parallel type to maintain.

In the constructor (`cdk/src/horde-worker.ts`), **after** the worker
`addContainer` call:

```ts
this.sidecarContainers = (props.sidecars ?? []).map((s) => {
  if (s.containerName === WORKER_CONTAINER_NAME) {
    throw new Error(
      `HordeWorker: sidecar containerName "${WORKER_CONTAINER_NAME}" is ` +
        `reserved for the worker container.`,
    );
  }
  // addContainer(id, options): id is the construct id. Use the sidecar's
  // containerName as the id when present (must be unique within the task def),
  // else a stable index-derived id like `Sidecar${i}`.
  return this.taskDefinition.addContainer(s.containerName ?? `Sidecar${i}`, {
    essential: false,
    logging: ecs.LogDriver.awsLogs({
      logGroup: this.logGroup,
      streamPrefix: s.containerName ?? `sidecar${i}`,
    }),
    ...s, // caller's explicit essential/logging override the defaults
  });
});
```

Defaults are applied with `essential: s.essential ?? false` and
`logging: s.logging ?? <shared-group>` — NOT by spreading `s` last — so a caller
passing an explicit `essential: undefined` can't fall through to CDK's own
`essential: true` default and silently flip a sidecar to essential. The snippet
is illustrative of intent, not final code. Expose
`public readonly sidecarContainers: ecs.ContainerDefinition[]`.

The worker container is still added first (defensive), but correctness no longer
depends on ordering — see §2.

### 2. Centralize the worker name; find the worker by name

The worker container name `"horde-worker"` is a **cross-language contract**,
hardcoded today in:

- `cdk/src/horde-worker.ts` (`containerName`)
- `internal/provider/ecs.go:28` (`const containerName`) — used for env overrides
  at launch and to build the CloudWatch log-stream path when reading logs
- `internal/bootstrap/templates/stack.yaml.tmpl` (container `Name`)

Within CDK, replace the literal with a single exported constant referenced by the
worker's `containerName`, the status-Lambda filter, and the sidecar reject-guard:

```ts
export const WORKER_CONTAINER_NAME = "horde-worker";
```

Renaming the constant moves the container, the filter, and the guard together —
no drift. **Not exposed as a prop**: the Go provider hardcodes the name for
log-stream paths and env-override targeting and does not read it from SSM, so a
configurable name would silently break `horde logs` and env injection for that
deployment unless also plumbed through the SSM config blob → out of scope.
Multiple horde stacks already coexist via `projectSlug` namespacing (separate
clusters/SSM paths), so a configurable container name solves no real use case.

Both Lambdas change exit-code extraction from "first container" to "the container
named `horde-worker`", with a legacy fallback to `containers[0]` when no named
container is present (preserves behavior for any single-container event lacking
the name field):

TypeScript (`cdk/src/status-lambda/index.ts`):

```ts
const containers = detail.containers ?? [];
const worker =
  containers.find((c) => c.name === WORKER_CONTAINER_NAME) ?? containers[0];
const exitCode =
  worker && typeof worker.exitCode === "number" ? worker.exitCode : null;
```

(The Lambda gets its own copy of the constant — it is bundled standalone by
esbuild and cannot import construct code that pulls in `aws-cdk-lib`. Keep the
two definitions adjacent / commented as paired.)

Python (`internal/bootstrap/templates/stack.yaml.tmpl`), kept in lockstep:

```python
containers = detail.get("containers", [])
worker = next((c for c in containers if c.get("name") == "horde-worker"), None)
if worker is None and containers:
    worker = containers[0]
exit_code = worker.get("exitCode") if worker else None
```

This is a no-op for current single-container stacks and future-proofs the
bootstrap path if it ever gains a sidecar.

**The third reader: the Go provider's lazy reconciliation.** The status Lambdas
are not the only code that derives run status from a container's exit code.
`internal/provider/ecs.go::Status()` reads `task.Containers[0].ExitCode`, and
`Finalize()` (the lazy-status path used by `horde status`/`list`/`results` when
the Lambda is behind or failed) feeds that into `mapExitCode` to write the run's
terminal status. With a sidecar present this has the *same* index-0 bug — a
sidecar killed non-zero when the worker exits 0 could be reconciled as `failed`.
So the find-by-name fix must be applied here too: a `workerContainer(containers)`
helper finds the `horde-worker` container by name with a `containers[0]`
fallback, mirroring the Lambdas. Covered by
`TestECSProvider_Status_SidecarExitIgnored` (sidecar 137 at index 0, worker 0 at
index 1 → exit 0) and `TestECSProvider_Status_NoWorkerNameFallback`.

### 3. Testing

**Synth/unit (CI — `cd cdk && npm test`):**

- Task def with one sidecar synthesizes two `ContainerDefinitions`; worker
  `Essential: true`, sidecar defaults `Essential: false`.
- A sidecar with explicit `essential: true` is honored.
- Sidecar logging defaults to the shared log group with its name as
  `awslogs-stream-prefix`.
- A sidecar with `containerName: "horde-worker"` throws at synth.
- The synthesized worker container `Name` equals `WORKER_CONTAINER_NAME`
  (documents the Go/Python lockstep dependency; a rename is caught here).

**Status-Lambda unit (`cdk/src/status-lambda/index.test.ts`):**

- Event `containers: [{name:"db",exitCode:1},{name:"horde-worker",exitCode:0}]`
  → status `success`, exitCode 0. Proves name-filter beats array order.
- Event `containers: [{exitCode:0}]` (no name) → `success`. Proves legacy
  fallback.

**Go provider unit (`internal/provider/ecs_test.go`):**

- `TestECSProvider_Status_SidecarExitIgnored`: task with
  `[{name:"postgres",exit:137},{name:"horde-worker",exit:0}]` → `ExitCode == 0`.
  Proves the reconciliation path finds the worker by name, not index.
- `TestECSProvider_Status_NoWorkerNameFallback`: single unnamed container → that
  container's exit. Proves the legacy fallback.

These deterministic unit tests are the **real proof** of the find-by-name fix,
across all three readers (TS Lambda, Python Lambda, Go provider).

**Live e2e (developer-local, `make e2e-*`, never CI): `TestECS_Sidecar`**

Proves localhost reachability on real Fargate:

- Add a Postgres sidecar to `cdk/e2e/app.ts` via the `sidecars` prop
  (`postgres:16-alpine`, `POSTGRES_HOST_AUTH_METHOD: "trust"`, mem-capped,
  essential defaults to false).
- Add `.orc/workflows/postgres-probe.yaml` — a `type: script` phase that probes
  `localhost:5432` with a dependency-free bash `/dev/tcp/localhost/5432` check
  (the worker image has no postgres client), retrying briefly. Exit 0 on reach.
- `TestECS_Sidecar` (in `test/integration/ecs_sidecar_test.go`) runs under the
  full `TestECS` suite with `HORDE_E2E_ECS_BACKEND=cdk`. It launches the probe
  workflow, polls to terminal, and asserts `status == "success"` + exit 0. The
  worker clones the canonical repo and runs the workflow from the checked-out
  branch, so before this lands on `main` set `HORDE_E2E_SIDECAR_BRANCH=<branch>`
  (pushed to GitHub) so the worker finds the workflow; post-merge the default
  (main) checkout has it.

Scope of the e2e: it reliably proves the worker **reaches the sidecar on
localhost** (the user-facing feature). It does **not**, on its own, prove the
find-by-name fix — ECS does not guarantee container order, so the test cannot
force the sidecar ahead of the worker; on a run where the worker sorts first the
old index-0 logic would also pass. The name-filter is proven by the unit tests
above. **Verified green on prepdesk** (run `1aawbgjq8v70` → success/0), plus the
broader `TestECS` suite passed against the sidecar-bearing stack.

### 4. Docs & version

- TSDoc on the `sidecars` prop (above).
- `cdk/README.md`: a "Sidecar containers" section with the Postgres test-DB
  example from the issue.
- `internal/docs/content.go` (`topicCDK`): a short sidecars note under "What it
  Provisions".
- Bump `cdk/package.json` `0.3.0` → `0.4.0` (backward-compatible feature).

## Out of scope

- Docker-provider sidecar support (compose / docker network) — explicit non-goal.
- CFN bootstrap sidecar *prop* surface — deferred. Only the Python Lambda's
  worker-by-name filter changes, for lockstep.
- Making the worker container name caller-configurable.

## Files touched

- `cdk/src/horde-worker-props.ts` — `sidecars` prop + TSDoc
- `cdk/src/horde-worker.ts` — `WORKER_CONTAINER_NAME` const, sidecar loop, guard,
  `sidecarContainers` field
- `cdk/src/status-lambda/index.ts` — find-worker-by-name filter (TS Lambda)
- `cdk/src/index.ts` — export `WORKER_CONTAINER_NAME` for tests
- `cdk/test/horde-worker-sidecars.test.ts` — synth assertions
- `cdk/src/status-lambda/index.test.ts` — filter unit tests
- `cdk/e2e/app.ts` — Postgres sidecar
- `cdk/package.json` + `package-lock.json` — 0.4.0
- `cdk/README.md` — docs
- `internal/bootstrap/templates/stack.yaml.tmpl` — Python Lambda filter (lockstep)
- `internal/provider/ecs.go` — `workerContainer` helper; `Status()` finds the
  worker by name (the third reader — Go lazy reconciliation)
- `internal/provider/ecs_test.go` — `Status` sidecar + fallback unit tests
- `internal/docs/content.go` — `topicCDK` note
- `.orc/workflows/postgres-probe.yaml` — e2e probe workflow
- `test/integration/ecs_sidecar_test.go` — `TestECS_Sidecar`

## Verification

- `cd cdk && npm run build && npm test` — unit + synth assertions pass (91).
- `make unit-test && make vet` — Go side clean, incl. the `ecs.go` find-by-name
  fix + its unit tests.
- `make bootstrap-validate-test` — rendered CFN still ValidateTemplate-clean.
- `make e2e-up && make e2e-test && make e2e-down` — live on prepdesk:
  `TestECS_Sidecar` green proves worker↔sidecar localhost reachability on real
  Fargate (run `1aawbgjq8v70`). The find-by-name fix is proven by the unit tests
  across all three readers, not the e2e (ECS container order is not forceable).
