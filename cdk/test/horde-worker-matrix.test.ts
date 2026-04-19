// Feature-matrix snapshot + structural tests for HordeWorker (bead 5fh.15).
// Each cell exercises a different combination of optional props (vpc,
// artifactsBucket) and asserts the construct adapts: skipping resource
// creation when the caller provides one, plus IAM scoping that wires the
// caller-provided resources correctly.
import { App, Stack } from "aws-cdk-lib";
import { Template } from "aws-cdk-lib/assertions";
import * as ec2 from "aws-cdk-lib/aws-ec2";
import * as ecr from "aws-cdk-lib/aws-ecr";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as s3 from "aws-cdk-lib/aws-s3";
import * as secretsmanager from "aws-cdk-lib/aws-secretsmanager";
import { HordeWorker, type HordeWorkerProps } from "../src";

const ENV = { account: "111111111111", region: "us-east-1" };

interface MatrixOpts {
  readonly withVpc: boolean;
  readonly withBucket: boolean;
}

function buildStack(opts: MatrixOpts): { stack: Stack; template: Template } {
  const app = new App();
  const stack = new Stack(app, "TestStack", { env: ENV });
  const repo = ecr.Repository.fromRepositoryName(stack, "Repo", "horde-test");
  const props: HordeWorkerProps = {
    projectSlug: "test",
    workerImage: ecs.ContainerImage.fromRegistry("public.ecr.aws/horde/test:latest"),
    ecrRepository: repo,
    secrets: {
      CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "Claude", "horde/claude"),
      GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "Git", "horde/git"),
    },
    ...(opts.withVpc
      ? { vpc: new ec2.Vpc(stack, "ProvidedVpc", { maxAzs: 2, natGateways: 1 }) }
      : {}),
    ...(opts.withBucket
      ? {
          artifactsBucket: new s3.Bucket(stack, "ProvidedBucket", {
            bucketName: "provided-bucket",
          }),
        }
      : {}),
  };
  new HordeWorker(stack, "Horde", props);
  return { stack, template: Template.fromStack(stack) };
}

const cells: ReadonlyArray<{ name: string; opts: MatrixOpts }> = [
  { name: "default (no vpc, no bucket)", opts: { withVpc: false, withBucket: false } },
  { name: "provided vpc", opts: { withVpc: true, withBucket: false } },
  { name: "provided bucket", opts: { withVpc: false, withBucket: true } },
  { name: "provided vpc and bucket", opts: { withVpc: true, withBucket: true } },
];

describe.each(cells)("HordeWorker matrix — $name", ({ opts }) => {
  it("matches the saved snapshot", () => {
    const { template } = buildStack(opts);
    expect(template.toJSON()).toMatchSnapshot();
  });

  it("creates exactly one Vpc inside the construct iff vpc was not provided", () => {
    const { template } = buildStack(opts);
    const vpcs = template.findResources("AWS::EC2::VPC");
    // Either way exactly 1 VPC ends up in the synthesized template
    // (caller-provided or HordeWorker-created), but the construct must NOT
    // create its own when the caller passes one.
    expect(Object.keys(vpcs)).toHaveLength(1);
    const vpcLogicalId = Object.keys(vpcs)[0];
    if (opts.withVpc) {
      expect(vpcLogicalId).toMatch(/^ProvidedVpc/);
    } else {
      expect(vpcLogicalId).toMatch(/^HordeVpc/);
    }
  });

  it("creates the artifacts bucket inside the construct iff one was not provided", () => {
    const { template } = buildStack(opts);
    const buckets = template.findResources("AWS::S3::Bucket");
    // Caller may also create extras; just check our HordeArtifactsBucket
    // exists or doesn't.
    const ourBucket = Object.keys(buckets).filter((id) => id.startsWith("HordeArtifactsBucket"));
    if (opts.withBucket) {
      expect(ourBucket).toHaveLength(0);
    } else {
      expect(ourBucket).toHaveLength(1);
    }
  });

  it("IAM policies never use wildcard resources for s3/ssm/dynamodb", () => {
    const { template } = buildStack(opts);
    const policies = {
      ...template.findResources("AWS::IAM::Policy"),
      ...template.findResources("AWS::IAM::ManagedPolicy"),
    };
    for (const policy of Object.values(policies)) {
      const stmts = policy.Properties.PolicyDocument.Statement ?? [];
      for (const s of stmts) {
        const actions = Array.isArray(s.Action) ? s.Action : [s.Action];
        const isScopedService = actions.some(
          (a: string) =>
            typeof a === "string" &&
            (a.startsWith("s3:") || a.startsWith("ssm:") || a.startsWith("dynamodb:")),
        );
        if (!isScopedService) continue;
        const resources = Array.isArray(s.Resource) ? s.Resource : [s.Resource];
        for (const r of resources) {
          // Allow "*" only when accompanied by a Condition that scopes
          // (e.g. ecs:RunTask uses "*" + ArnEquals on cluster). Pure scoped
          // services (s3/ssm/dynamodb) should never be unconstrained "*".
          expect(r).not.toBe("*");
        }
      }
    }
  });

  it("SSM JSON parameter is always written and parses to the HordeConfig key set", () => {
    const { template } = buildStack(opts);
    const params = template.findResources("AWS::SSM::Parameter");
    const cfg = Object.values(params).find((p) => p.Properties.Name === "/horde/test/config");
    if (!cfg) throw new Error("expected /horde/test/config parameter");
    const valueStr = JSON.stringify(cfg.Properties.Value);
    for (const key of [
      "cluster_arn",
      "task_definition_arn",
      "subnets",
      "security_group",
      "assign_public_ip",
      "log_group",
      "log_stream_prefix",
      "artifacts_bucket",
      "runs_table",
      "ecr_repo_uri",
      "max_concurrent",
      "default_timeout_minutes",
    ]) {
      expect(valueStr).toContain(`\\"${key}\\"`);
    }
  });

  it("status Lambda is always present regardless of optional props", () => {
    const { template } = buildStack(opts);
    template.hasResourceProperties("AWS::Lambda::Function", {
      FunctionName: "horde-test-status-updater",
      Runtime: "nodejs20.x",
    });
  });

  it("EventBridge rule clusterArn pattern always references the construct cluster", () => {
    const { template } = buildStack(opts);
    const rules = template.findResources("AWS::Events::Rule");
    const statusRules = Object.values(rules).filter(
      (r) => r.Properties.Name === "horde-test-status",
    );
    expect(statusRules).toHaveLength(1);
    const pattern = statusRules[0].Properties.EventPattern;
    expect(pattern.detail.clusterArn).toBeDefined();
    expect(pattern.detail.lastStatus).toEqual(["STOPPED"]);
  });

  it("CLI managed policy always emits a CfnOutput for its ARN", () => {
    const { template } = buildStack(opts);
    const outputs = template.findOutputs("*");
    expect(
      Object.keys(outputs).some((k) => k.includes("CliUserManagedPolicyArn")),
    ).toBe(true);
  });
});
