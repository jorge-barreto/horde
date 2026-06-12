# `networkMode`: public-subnet default with a private lever

**Date:** 2026-06-12
**Scope:** CDK construct only (`@horde.io/cdk`). No Go, SSM-contract, or bootstrap-CloudFormation changes.

## Problem

The `HordeWorker` CDK construct always provisions a NAT Gateway and runs worker
tasks in `PRIVATE_WITH_EGRESS` subnets with no public IP. A NAT Gateway costs
~$32/mo (us-east-1: $0.045/hr) plus data-processing charges — a recurring cost
that, for horde's threat model, buys almost no security. The construct offers no
lever to opt out, so teams who want the cheaper public-subnet posture must fork
the construct or use a CloudFormation escape hatch.

## Security rationale (why public-by-default is safe here)

The danger of a public subnet is **inbound** reachability, and that is governed
by the security group, not the subnet. horde's worker security group is created
with `allowAllOutbound: false` and a **single egress rule** (443 outbound) and
**zero ingress rules** (`horde-worker.ts:257-266`). A security group is
default-deny inbound, so with no ingress rule nothing on the internet can open a
connection to the task — the public IP is purely an **egress** enabler.

A NAT Gateway does **not** restrict egress; it only translates it. A compromised
worker (these run AI agents executing arbitrary repo code) can exfiltrate to any
HTTPS endpoint with or without a NAT. So the NAT buys **zero** egress security
over a public IP. The two postures have an identical egress threat model.

What `private` still genuinely offers, and why we keep it as a lever:

1. **Blast-radius containment for future mistakes.** With a public IP, the day
   someone adds an ingress rule ("open 8080 to debug") they expose it to the
   whole internet. In a private subnet the same mistake only exposes it inside
   the VPC.
2. **Reaching private VPC resources** (RDS, internal services, PrivateLink)
   without an internet path, with a simpler "nothing is internet-facing" audit
   story.

Neither is a reason to make `private` the *default*; both are reasons to keep it
*available*.

## Design

### New prop

Add to `HordeWorkerProps` (`cdk/src/horde-worker-props.ts`):

```ts
export type HordeNetworkMode = 'public' | 'private';

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
 * type.
 *
 * @default 'public'
 */
readonly networkMode?: HordeNetworkMode;
```

### Construct behavior (`cdk/src/horde-worker.ts`)

Resolve once near the top of the constructor:

```ts
const networkMode = props.networkMode ?? 'public';
```

**Managed VPC** (the `else` branch, currently ~line 184) branches on mode:

- `'public'`: `subnetConfiguration: [{ name: 'public', subnetType: PUBLIC, cidrMask: 24 }]`, `natGateways: 0`.
- `'private'`: today's config — `public` + `PRIVATE_WITH_EGRESS`, `natGateways: 1`.

**Subnet selection** (currently hardcoded `PRIVATE_WITH_EGRESS`, ~line 382)
becomes mode-driven and is the *same path* for managed and BYO VPCs:

```ts
const subnetType =
  networkMode === 'public'
    ? ec2.SubnetType.PUBLIC
    : ec2.SubnetType.PRIVATE_WITH_EGRESS;
const selectedSubnetIds = this.vpc.selectSubnets({ subnetType }).subnetIds;
if (selectedSubnetIds.length === 0) {
  throw new Error(
    `HordeWorker networkMode '${networkMode}' requires ${subnetType} subnets ` +
    `in the VPC, but none were found.`,
  );
}
```

The empty-selection guard turns a BYO VPC that lacks the chosen subnet type into
a clear **synth-time** error instead of a runtime `RunTask` failure.

**`assign_public_ip`** derives from the same value:

```ts
const assignPublicIp = networkMode === 'public' ? 'ENABLED' : 'DISABLED';
```

threaded into **both** the SSM config JSON (replacing the hardcoded
`"assign_public_ip":"DISABLED"`, ~line 422) and the drain Lambda env (replacing
`ASSIGN_PUBLIC_IP: "DISABLED"`, ~line 572). Both derive from the one resolved
value, so they cannot drift.

Three derived values (subnet config, subnet selection, `assign_public_ip`) all
flow from the single `networkMode` — the coupling the enum exists to enforce.

### BYO-VPC rule (uniform, no context-dependent default)

`networkMode` selects subnet type identically for managed and BYO VPCs. The
default is `'public'` in **all** cases — supplying a VPC does **not** flip the
default to `'private'`. A typical enterprise private VPC therefore needs one
explicit line, `networkMode: 'private'`, and gets a clear synth error if omitted
(no PUBLIC subnets found). Rationale: uniform mental model + default-to-lowest-
cost everywhere; the construct never silently steers toward a NAT.

### What does NOT change

The entire runtime path already supports public networking:

- `internal/config/ssm.go` — `AssignPublicIp` field + `ENABLED`/`DISABLED` validation.
- `internal/provider/ecs.go` — maps the string to the AWS enum, sets it on the task's `NetworkConfiguration`.
- `cdk/src/drain-lambda/index.ts` — already reads `ASSIGN_PUBLIC_IP` from env.

The Python bootstrap CloudFormation template keeps its NAT, consistent with the
CFN-is-being-retired / CDK-leads convention.

## Testing

In `cdk/src/horde-worker.test.ts` (and any affected unit tests):

- **`'public'` (default, prop omitted):** template has **no** `AWS::EC2::NatGateway`; SSM config JSON contains `"assign_public_ip":"ENABLED"`; drain Lambda env `ASSIGN_PUBLIC_IP: "ENABLED"`; task subnets are the public ones.
- **`'private'` (explicit):** NAT Gateway present; `"assign_public_ip":"DISABLED"`; private subnets selected — today's behavior still reachable.
- **BYO VPC + `'public'` with no public subnets** → synth throws the clear error.
- **BYO VPC + `'private'` with no private-egress subnets** → synth throws the clear error.
- Existing assertions/snapshots that currently expect a NAT or `DISABLED` in the default case are updated to the new default.

CI's `cdk-test` job (`cd cdk && npm ci && npm run build && npm test`) must stay green.

## Migration / release note (behavior change)

Existing CDK consumers who re-synth without setting `networkMode` move from
private+NAT to public+no-NAT — a real infrastructure change (NAT destroyed,
tasks get public IPs). This is the intended default flip but **must be flagged
prominently in the release notes**: a `cdk deploy` after upgrade will tear down
the NAT and reassign task networking. Teams who want the prior posture set
`networkMode: 'private'`.

## Out of scope

- No change to the Go CLI, SSM JSON contract, or `internal/bootstrap` CFN template.
- No third network mode (e.g. `'isolated'` / PrivateLink-only). The enum leaves
  room for it as a non-breaking future addition, but it is not built here.
