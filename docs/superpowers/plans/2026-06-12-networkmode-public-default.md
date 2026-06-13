# networkMode public-subnet default Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the `HordeWorker` CDK construct default to public-subnet + public-IP networking (no NAT Gateway, ~$32/mo saved), with a `networkMode: 'private'` lever that restores the NAT-bearing private posture.

**Architecture:** CDK-only change. A single `networkMode?: 'public' | 'private'` prop (default `'public'`) drives three derived values inside the construct: the managed VPC's subnet configuration (NAT or no NAT), the subnet type selected for the task/drain network config, and the `assign_public_ip` string written to SSM and the drain Lambda env. The Go CLI, SSM JSON contract, drain Lambda code, and bootstrap CloudFormation template are all untouched — they already support `ENABLED`/public subnets.

**Tech Stack:** TypeScript, AWS CDK (`aws-cdk-lib`), Jest with `aws-cdk-lib/assertions` (`Template`/`Match`) + snapshot tests. All commands run from the `cdk/` directory.

**Working directory:** All paths below are relative to the repo root. All `npm` commands run from `cdk/`. The session's shell cwd is already `cdk/`.

---

## File Structure

- **Modify** `cdk/src/horde-worker-props.ts` — add the `HordeNetworkMode` type alias and `networkMode?` prop with JSDoc.
- **Modify** `cdk/src/index.ts` — re-export the `HordeNetworkMode` type.
- **Modify** `cdk/src/horde-worker.ts` — resolve `networkMode`; branch the managed-VPC subnet config; make subnet selection + `assign_public_ip` mode-driven; thread the value into the SSM JSON and drain env; add the empty-subnet synth guard.
- **Modify** `cdk/test/horde-worker.test.ts` — update the SSM-JSON assertion from `DISABLED` to `ENABLED`; add default-public assertions (no NAT, ENABLED) and explicit-private assertions (NAT present, DISABLED) and the two BYO-VPC error cases.
- **Regenerate** `cdk/test/__snapshots__/horde-worker-matrix.test.ts.snap` — the default-networking change alters the matrix snapshots; regenerate with `jest -u`.
- **Modify** `cdk/src/horde-worker-props.ts` JSDoc also updates the `vpc?` `@default` note (subnet layout now depends on mode).

---

### Task 1: Add the `networkMode` prop and type

**Files:**
- Modify: `cdk/src/horde-worker-props.ts`
- Modify: `cdk/src/index.ts`
- Test: `cdk/test/horde-worker-props.test.ts`

- [ ] **Step 1: Write a failing compile-time test for the new type export**

Append to `cdk/test/horde-worker-props.test.ts`, inside the existing `describe("HordeWorkerProps", ...)` block (before its closing `});`):

```ts
  it("accepts networkMode 'public' and 'private'", () => {
    const a: HordeNetworkMode = "public";
    const b: HordeNetworkMode = "private";
    expect([a, b]).toEqual(["public", "private"]);
  });
```

And change the top import line to also import the type:

```ts
import type { HordeNetworkMode, HordeWorkerProps } from "../src";
```

- [ ] **Step 2: Run the build to verify it fails**

Run: `npm run build`
Expected: FAIL — `tsc` error `Module '"../src"' has no exported member 'HordeNetworkMode'` (and the test references an undefined type).

- [ ] **Step 3: Add the type alias and prop**

In `cdk/src/horde-worker-props.ts`, add this exported type alias near the top of the file (after the existing imports, before the `HordeWorkerSecrets`/`HordeWorkerProps` declarations):

```ts
/** Network posture for worker tasks. See `HordeWorkerProps.networkMode`. */
export type HordeNetworkMode = "public" | "private";
```

Then, in the `HordeWorkerProps` interface, immediately after the existing `vpc?` prop, add:

```ts
  /**
   * Network posture for the worker tasks.
   *
   * - `'public'` (default): tasks run in PUBLIC subnets with a public IPv4
   *   address and reach the internet directly via the Internet Gateway — no
   *   NAT Gateway is created (~$32/mo saved). The worker security group has no
   *   ingress rules, so the public IP enables egress only; nothing on the
   *   internet can open a connection to the task.
   * - `'private'`: tasks run in PRIVATE_WITH_EGRESS subnets with no public IP,
   *   reaching the internet through a NAT Gateway the construct creates. Choose
   *   this to limit the blast radius of a future ingress rule, or when workers
   *   must reach private VPC resources without an internet path.
   *
   * With a bring-your-own `vpc`, this selects which of that VPC's subnets to
   * use; a synth-time error is raised if the VPC has no subnets of the chosen
   * type. The default is `'public'` in all cases — supplying a VPC does not
   * change it.
   *
   * @default 'public'
   */
  readonly networkMode?: HordeNetworkMode;
```

Also update the existing `vpc?` prop's `@default` line so it reflects the mode-dependent layout. Change:

```ts
   * @default — a new VPC with 2 private + 2 public subnets is created.
```

to:

```ts
   * @default — a new VPC. In `networkMode: 'public'` (the default) it has only
   *   public subnets and no NAT Gateway; in `'private'` it adds
   *   PRIVATE_WITH_EGRESS subnets and one NAT Gateway.
```

- [ ] **Step 4: Re-export the type from the package index**

In `cdk/src/index.ts`, change the props type re-export line:

```ts
export type { HordeWorkerProps, HordeWorkerSecrets } from "./horde-worker-props";
```

to:

```ts
export type { HordeNetworkMode, HordeWorkerProps, HordeWorkerSecrets } from "./horde-worker-props";
```

- [ ] **Step 5: Run the build to verify it passes**

Run: `npm run build`
Expected: PASS — `tsc` clean, both lambda bundles emitted.

- [ ] **Step 6: Run the props test**

Run: `npx jest test/horde-worker-props.test.ts`
Expected: PASS (3 tests).

- [ ] **Step 7: Commit**

```bash
git add cdk/src/horde-worker-props.ts cdk/src/index.ts cdk/test/horde-worker-props.test.ts
git commit -m "feat(cdk): add networkMode prop and HordeNetworkMode type"
```

---

### Task 2: Make the managed VPC, subnet selection, and assign_public_ip mode-driven

**Files:**
- Modify: `cdk/src/horde-worker.ts:182-200` (managed VPC block)
- Modify: `cdk/src/horde-worker.ts:382-384` (subnet selection)
- Modify: `cdk/src/horde-worker.ts:408-433` (SSM config JSON — `assign_public_ip` + subnets var name)
- Modify: `cdk/src/horde-worker.ts:564-574` (drain Lambda env)

This task is wiring, validated by Task 3's tests. Implement it, then build.

- [ ] **Step 1: Resolve `networkMode` once at the top of the constructor**

In `cdk/src/horde-worker.ts`, find the early constructor lines (right after `const retention = toRetention(...)`, near line 178) and add:

```ts
    const networkMode = props.networkMode ?? "public";
```

- [ ] **Step 2: Branch the managed VPC subnet config on the mode**

Replace the managed-VPC `else` block (lines 184-200):

```ts
    } else {
      const vpc = new ec2.Vpc(this, "Vpc", {
        maxAzs: 2,
        natGateways: 1,
        vpcName: `horde-${slug}-vpc`,
        subnetConfiguration: [
          { name: "public", subnetType: ec2.SubnetType.PUBLIC, cidrMask: 24 },
          {
            name: "private",
            subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS,
            cidrMask: 24,
          },
        ],
      });
      cdk.Tags.of(vpc).add("Name", `horde-${slug}-vpc`);
      this.vpc = vpc;
    }
```

with:

```ts
    } else {
      const vpc = new ec2.Vpc(this, "Vpc", {
        maxAzs: 2,
        // 'public': no NAT, tasks egress directly via the IGW. 'private': one
        // NAT Gateway fronting PRIVATE_WITH_EGRESS subnets.
        natGateways: networkMode === "public" ? 0 : 1,
        vpcName: `horde-${slug}-vpc`,
        subnetConfiguration:
          networkMode === "public"
            ? [{ name: "public", subnetType: ec2.SubnetType.PUBLIC, cidrMask: 24 }]
            : [
                { name: "public", subnetType: ec2.SubnetType.PUBLIC, cidrMask: 24 },
                {
                  name: "private",
                  subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS,
                  cidrMask: 24,
                },
              ],
      });
      cdk.Tags.of(vpc).add("Name", `horde-${slug}-vpc`);
      this.vpc = vpc;
    }
```

- [ ] **Step 3: Make subnet selection mode-driven with an empty-selection guard**

Replace the subnet-selection block (lines 382-384):

```ts
    const privateSubnetIds = this.vpc.selectSubnets({
      subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS,
    }).subnetIds;
```

with:

```ts
    const subnetType =
      networkMode === "public"
        ? ec2.SubnetType.PUBLIC
        : ec2.SubnetType.PRIVATE_WITH_EGRESS;
    const selectedSubnetIds = this.vpc.selectSubnets({ subnetType }).subnetIds;
    if (selectedSubnetIds.length === 0) {
      throw new Error(
        `HordeWorker networkMode '${networkMode}' requires ${subnetType} subnets ` +
          `in the VPC, but none were found.`,
      );
    }
    const assignPublicIp = networkMode === "public" ? "ENABLED" : "DISABLED";
```

- [ ] **Step 4: Use the resolved subnets + assign_public_ip in the SSM config JSON**

In the SSM config block (lines 408-433), the `subnetsJson` builder currently references `privateSubnetIds`. Change line ~410:

```ts
      cdk.Fn.join(",", privateSubnetIds.map((id) => cdk.Fn.join("", ['"', id, '"']))),
```

to:

```ts
      cdk.Fn.join(",", selectedSubnetIds.map((id) => cdk.Fn.join("", ['"', id, '"']))),
```

Then replace the hardcoded `assign_public_ip` literal segment (line ~422). Change:

```ts
      '","assign_public_ip":"DISABLED","log_group":"',
```

to (split so the resolved value is interpolated as its own segment):

```ts
      '","assign_public_ip":"',
      assignPublicIp,
      '","log_group":"',
```

- [ ] **Step 5: Use the resolved subnets + assign_public_ip in the drain Lambda env**

In the `drainEnv` block (lines 564-574), change:

```ts
      SUBNETS: cdk.Fn.join(",", privateSubnetIds),
      SECURITY_GROUP: this.workerSecurityGroup.securityGroupId,
      ASSIGN_PUBLIC_IP: "DISABLED", // matches the SSM config above
```

to:

```ts
      SUBNETS: cdk.Fn.join(",", selectedSubnetIds),
      SECURITY_GROUP: this.workerSecurityGroup.securityGroupId,
      ASSIGN_PUBLIC_IP: assignPublicIp, // matches the SSM config above
```

- [ ] **Step 6: Build to verify there are no remaining references to `privateSubnetIds`**

Run: `npm run build`
Expected: PASS — `tsc` clean. If it fails with `Cannot find name 'privateSubnetIds'`, find the stray reference and switch it to `selectedSubnetIds`.

- [ ] **Step 7: Commit**

```bash
git add cdk/src/horde-worker.ts
git commit -m "feat(cdk): drive subnets/NAT/assign_public_ip from networkMode"
```

---

### Task 3: Update and add construct tests

**Files:**
- Modify: `cdk/test/horde-worker.test.ts:345` (the `DISABLED` assertion)
- Modify: `cdk/test/horde-worker.test.ts` (add a new `describe` block for networkMode)

- [ ] **Step 1: Fix the now-wrong default assertion**

In `cdk/test/horde-worker.test.ts`, the test `"SSM config JSON references every required HordeConfig field"` asserts the old default at line 345:

```ts
    expect(valueStr).toContain('\\"DISABLED\\"');
```

Change it to the new default:

```ts
    expect(valueStr).toContain('\\"ENABLED\\"');
```

- [ ] **Step 2: Run that test to verify the change is correct (it should now pass once Task 2 is in)**

Run: `npx jest test/horde-worker.test.ts -t "references every required HordeConfig field"`
Expected: PASS. (Before Task 2 it would have failed; this confirms Task 2 wired `ENABLED` into the default.)

- [ ] **Step 3: Add the networkMode behavior tests**

Append a new top-level `describe` block at the end of `cdk/test/horde-worker.test.ts` (after the final closing `});` of the existing describe). It reuses the imports already at the top of the file (`App`, `Stack`, `Match`, `Template`, `ecr`, `ecs`, `secretsmanager`) and adds `ec2`:

```ts
import * as ec2 from "aws-cdk-lib/aws-ec2";

function synthWithProps(extra: Record<string, unknown>): Template {
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
    ...extra,
  });
  return Template.fromStack(stack);
}

describe("HordeWorker networkMode", () => {
  it("defaults to public: no NAT Gateway and assign_public_ip ENABLED", () => {
    const t = synthWithProps({});
    t.resourceCountIs("AWS::EC2::NatGateway", 0);
    const params = t.findResources("AWS::SSM::Parameter");
    const cfg = Object.values(params).find((p) => p.Properties.Name === "/horde/test/config");
    if (!cfg) throw new Error("expected /horde/test/config parameter");
    expect(JSON.stringify(cfg.Properties.Value)).toContain('\\"assign_public_ip\\":\\"');
    expect(JSON.stringify(cfg.Properties.Value)).toContain('\\"ENABLED\\"');
  });

  it("public mode sets the drain Lambda ASSIGN_PUBLIC_IP env to ENABLED", () => {
    const t = synthWithProps({});
    t.hasResourceProperties("AWS::Lambda::Function", {
      Environment: { Variables: Match.objectLike({ ASSIGN_PUBLIC_IP: "ENABLED" }) },
    });
  });

  it("private mode creates one NAT Gateway and sets assign_public_ip DISABLED", () => {
    const t = synthWithProps({ networkMode: "private" });
    t.resourceCountIs("AWS::EC2::NatGateway", 1);
    const params = t.findResources("AWS::SSM::Parameter");
    const cfg = Object.values(params).find((p) => p.Properties.Name === "/horde/test/config");
    if (!cfg) throw new Error("expected /horde/test/config parameter");
    expect(JSON.stringify(cfg.Properties.Value)).toContain('\\"DISABLED\\"');
  });

  it("private mode sets the drain Lambda ASSIGN_PUBLIC_IP env to DISABLED", () => {
    const t = synthWithProps({ networkMode: "private" });
    t.hasResourceProperties("AWS::Lambda::Function", {
      Environment: { Variables: Match.objectLike({ ASSIGN_PUBLIC_IP: "DISABLED" }) },
    });
  });

  it("throws when a BYO VPC has no public subnets in public mode", () => {
    const app = new App();
    const stack = new Stack(app, "S", { env: { account: "111111111111", region: "us-east-1" } });
    const repo = ecr.Repository.fromRepositoryName(stack, "Repo", "horde-test");
    const vpc = new ec2.Vpc(stack, "PrivateOnlyVpc", {
      maxAzs: 1,
      natGateways: 0,
      subnetConfiguration: [
        { name: "isolated", subnetType: ec2.SubnetType.PRIVATE_ISOLATED, cidrMask: 24 },
      ],
    });
    expect(() =>
      new HordeWorker(stack, "Horde", {
        projectSlug: "test",
        repo: "github.com/example/test",
        workerImage: ecs.ContainerImage.fromRegistry("public.ecr.aws/horde/test:latest"),
        ecrRepository: repo,
        vpc,
        secrets: {
          CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "C", "c"),
          GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "G", "g"),
        },
      }),
    ).toThrow(/requires .* subnets in the VPC, but none were found/);
  });

  it("throws when a BYO VPC has no private-egress subnets in private mode", () => {
    const app = new App();
    const stack = new Stack(app, "S", { env: { account: "111111111111", region: "us-east-1" } });
    const repo = ecr.Repository.fromRepositoryName(stack, "Repo", "horde-test");
    const vpc = new ec2.Vpc(stack, "PublicOnlyVpc", {
      maxAzs: 1,
      natGateways: 0,
      subnetConfiguration: [
        { name: "public", subnetType: ec2.SubnetType.PUBLIC, cidrMask: 24 },
      ],
    });
    expect(() =>
      new HordeWorker(stack, "Horde", {
        projectSlug: "test",
        repo: "github.com/example/test",
        workerImage: ecs.ContainerImage.fromRegistry("public.ecr.aws/horde/test:latest"),
        ecrRepository: repo,
        vpc,
        networkMode: "private",
        secrets: {
          CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "C", "c"),
          GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "G", "g"),
        },
      }),
    ).toThrow(/requires .* subnets in the VPC, but none were found/);
  });
});
```

Note: place the new `import * as ec2 ...` line with the other imports at the top of the file, not inside the describe block (shown above only to indicate it is needed). If `ec2` is already imported at the top, do not duplicate it.

- [ ] **Step 4: Run the full networkMode + SSM test file**

Run: `npx jest test/horde-worker.test.ts`
Expected: PASS — all existing tests plus the 6 new networkMode tests.

- [ ] **Step 5: Commit**

```bash
git add cdk/test/horde-worker.test.ts
git commit -m "test(cdk): cover networkMode public default and private lever"
```

---

### Task 4: Regenerate matrix snapshots and run the full suite

The matrix test (`cdk/test/horde-worker-matrix.test.ts`) uses `toMatchSnapshot()`. The default-networking change alters the synthesized template for the construct-owned-VPC cells, so the saved snapshots must be regenerated. The BYO-VPC matrix cells use CDK's default VPC (which has public subnets), so they still synth under the default `'public'` mode — only the snapshot content changes, no error.

- [ ] **Step 1: Run the suite to see the snapshot mismatch (confirms the change is captured)**

Run: `npx jest test/horde-worker-matrix.test.ts`
Expected: FAIL — snapshot mismatches for the cells using the construct's own VPC (NAT Gateway removed, subnets changed). This is expected; do not "fix" by editing code.

- [ ] **Step 2: Inspect what changed before blessing it**

Run: `npx jest test/horde-worker-matrix.test.ts 2>&1 | grep -E "NatGateway|AssignPublicIp|SubnetType|- |\\+ " | head -40`
Expected: the diff shows NAT Gateway / EIP resources removed and public-subnet routing in the default cells — i.e. exactly the intended change, nothing unrelated.

- [ ] **Step 3: Regenerate the snapshots**

Run: `npx jest test/horde-worker-matrix.test.ts -u`
Expected: PASS — snapshots written/updated.

- [ ] **Step 4: Run the ENTIRE CDK suite the way CI does**

Run: `npm run build && npm test`
Expected: PASS — all test files green (`horde-worker`, `horde-worker-matrix`, `horde-worker-events`, `horde-worker-props`, `horde-worker-sidecars`, `drain-lambda`, `status-lambda`). This is exactly what the CI `cdk-test` job runs.

- [ ] **Step 5: Commit**

```bash
git add cdk/test/__snapshots__/horde-worker-matrix.test.ts.snap
git commit -m "test(cdk): regenerate matrix snapshots for public-default networking"
```

---

## Self-Review

**Spec coverage:**

- New prop `networkMode?: 'public' | 'private'`, default `'public'` → Task 1.
- `HordeNetworkMode` exported type → Task 1.
- Managed VPC public-only (no NAT) in public mode; public+private+NAT in private → Task 2 Step 2.
- Mode-driven subnet selection, same path for managed/BYO → Task 2 Step 3.
- Empty-selection synth guard → Task 2 Step 3 + Task 3 Steps (two BYO error tests).
- `assign_public_ip` derived and threaded into SSM JSON + drain env → Task 2 Steps 4-5.
- BYO uniform rule, default public everywhere → Task 1 JSDoc + Task 3 error tests.
- "What does not change" (Go/SSM/drain-code/CFN) → no tasks touch them; verified by exploration. ✓
- Tests: default no-NAT/ENABLED, explicit private NAT/DISABLED, two BYO errors → Task 3. Snapshot regen → Task 4.
- Migration/release note → documentation concern; surfaced to the user at PR time, not a code task. Flagged in the PR-body checklist below.

**Placeholder scan:** No TBD/TODO/"handle edge cases". Every code step shows complete code. ✓

**Type/name consistency:** `networkMode`, `HordeNetworkMode`, `selectedSubnetIds`, `subnetType`, `assignPublicIp` are used identically across Tasks 1-3. The old `privateSubnetIds` name is fully removed in Task 2 (Steps 3-5) and Step 6 builds to catch any stray reference. ✓

**At PR time:** the PR body MUST call out the behavior change — existing consumers who re-synth without `networkMode` move from private+NAT to public+no-NAT; `cdk deploy` will destroy the NAT and reassign task networking. Anyone wanting the old posture sets `networkMode: 'private'`. This belongs in the release notes / changelog for the next `@horde.io/cdk` version bump.
