import { App, Stack } from "aws-cdk-lib";
import { Match, Template } from "aws-cdk-lib/assertions";
import * as ecr from "aws-cdk-lib/aws-ecr";
import * as ecs from "aws-cdk-lib/aws-ecs";
import * as secretsmanager from "aws-cdk-lib/aws-secretsmanager";
import { HordeWorker } from "../src";

function synth() {
  const app = new App();
  const stack = new Stack(app, "TestStack", {
    env: { account: "111111111111", region: "us-east-1" },
  });
  const repo = ecr.Repository.fromRepositoryName(stack, "Repo", "horde-test");
  new HordeWorker(stack, "Horde", {
    projectSlug: "test",
    workerImage: ecs.ContainerImage.fromRegistry("public.ecr.aws/horde/test:latest"),
    ecrRepository: repo,
    secrets: {
      CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "Claude", "horde/claude"),
      GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "Git", "horde/git"),
    },
  });
  return Template.fromStack(stack);
}

describe("HordeWorker (5fh.3 skeleton)", () => {
  it("creates a VPC tagged horde-<slug>-vpc", () => {
    const t = synth();
    t.hasResourceProperties("AWS::EC2::VPC", {
      Tags: Match.arrayWith([{ Key: "Name", Value: "horde-test-vpc" }]),
    });
  });

  it("creates an ECS cluster named horde-<slug>", () => {
    synth().hasResourceProperties("AWS::ECS::Cluster", { ClusterName: "horde-test" });
  });

  it("creates a log group with the expected name and 30-day retention", () => {
    synth().hasResourceProperties("AWS::Logs::LogGroup", {
      LogGroupName: "/ecs/horde-worker-test",
      RetentionInDays: 30,
    });
  });

  it("creates a Fargate task definition with the worker container", () => {
    synth().hasResourceProperties("AWS::ECS::TaskDefinition", {
      Family: "horde-test",
      Cpu: "1024",
      Memory: "4096",
      RequiresCompatibilities: ["FARGATE"],
      NetworkMode: "awsvpc",
      ContainerDefinitions: Match.arrayWith([
        Match.objectLike({
          Name: "horde-worker",
          Image: "public.ecr.aws/horde/test:latest",
          Essential: true,
          LogConfiguration: Match.objectLike({
            LogDriver: "awslogs",
            Options: Match.objectLike({ "awslogs-stream-prefix": "ecs" }),
          }),
        }),
      ]),
    });
  });

  it("injects CLAUDE_CODE_OAUTH_TOKEN and GIT_TOKEN as ECS secrets via valueFrom", () => {
    const t = synth();
    t.hasResourceProperties("AWS::ECS::TaskDefinition", {
      ContainerDefinitions: Match.arrayWith([
        Match.objectLike({
          Name: "horde-worker",
          Secrets: Match.arrayWith([
            Match.objectLike({ Name: "CLAUDE_CODE_OAUTH_TOKEN", ValueFrom: Match.anyValue() }),
            Match.objectLike({ Name: "GIT_TOKEN", ValueFrom: Match.anyValue() }),
          ]),
        }),
      ]),
    });
  });

  it("does NOT pass secrets as plain Environment vars", () => {
    const t = synth();
    const tds = t.findResources("AWS::ECS::TaskDefinition");
    for (const td of Object.values(tds)) {
      const containers = td.Properties.ContainerDefinitions ?? [];
      for (const c of containers) {
        const env = c.Environment ?? [];
        for (const e of env) {
          expect(e.Name).not.toBe("CLAUDE_CODE_OAUTH_TOKEN");
          expect(e.Name).not.toBe("GIT_TOKEN");
        }
      }
    }
  });

  it("creates an S3 artifacts bucket with public access blocked and SSE-S3", () => {
    const t = synth();
    t.hasResourceProperties("AWS::S3::Bucket", {
      BucketEncryption: {
        ServerSideEncryptionConfiguration: [
          { ServerSideEncryptionByDefault: { SSEAlgorithm: "AES256" } },
        ],
      },
      PublicAccessBlockConfiguration: {
        BlockPublicAcls: true,
        BlockPublicPolicy: true,
        IgnorePublicAcls: true,
        RestrictPublicBuckets: true,
      },
    });
  });

  it("denies non-TLS requests on the artifacts bucket", () => {
    const t = synth();
    t.hasResourceProperties("AWS::S3::BucketPolicy", {
      PolicyDocument: Match.objectLike({
        Statement: Match.arrayWith([
          Match.objectLike({
            Effect: "Deny",
            Action: "s3:*",
            Condition: { Bool: { "aws:SecureTransport": "false" } },
          }),
        ]),
      }),
    });
  });

  it("scopes the task role to PutObject/AbortMultipartUpload on horde-runs/* only", () => {
    const t = synth();
    t.hasResourceProperties("AWS::IAM::Policy", {
      PolicyDocument: Match.objectLike({
        Statement: Match.arrayWith([
          Match.objectLike({
            Sid: "ArtifactsWrite",
            Effect: "Allow",
            Action: ["s3:PutObject", "s3:AbortMultipartUpload"],
            Resource: Match.objectLike({
              "Fn::Join": Match.arrayWith([
                Match.arrayWith([Match.stringLikeRegexp(".*horde-runs/\\*")]),
              ]),
            }),
          }),
        ]),
      }),
      Roles: Match.arrayWith([{ Ref: Match.stringLikeRegexp("HordeTaskRole.*") }]),
    });
  });

  it("does NOT grant the task role secretsmanager access", () => {
    const t = synth();
    const policies = t.findResources("AWS::IAM::Policy");
    for (const [_id, policy] of Object.entries(policies)) {
      const roles = policy.Properties.Roles ?? [];
      const isTaskRolePolicy = roles.some((r: { Ref?: string }) =>
        typeof r === "object" && r.Ref?.includes("TaskRole") && !r.Ref?.includes("ExecutionRole"),
      );
      if (!isTaskRolePolicy) continue;
      const stmts = policy.Properties.PolicyDocument.Statement ?? [];
      for (const s of stmts) {
        const actions = Array.isArray(s.Action) ? s.Action : [s.Action];
        for (const a of actions) {
          expect(typeof a === "string" ? a : "").not.toMatch(/^secretsmanager:/);
        }
      }
    }
  });

  it("execution role has the AWS managed ECS task execution policy", () => {
    const t = synth();
    t.hasResourceProperties("AWS::IAM::Role", {
      AssumeRolePolicyDocument: Match.objectLike({
        Statement: Match.arrayWith([
          Match.objectLike({
            Principal: { Service: "ecs-tasks.amazonaws.com" },
          }),
        ]),
      }),
      ManagedPolicyArns: Match.arrayWith([
        Match.objectLike({
          "Fn::Join": Match.arrayWith([
            Match.arrayWith([
              Match.stringLikeRegexp(".*service-role/AmazonECSTaskExecutionRolePolicy.*"),
            ]),
          ]),
        }),
      ]),
    });
  });

  it("execution role can resolve the two named secrets at container start", () => {
    const t = synth();
    const policies = t.findResources("AWS::IAM::Policy");
    const execPolicies = Object.entries(policies).filter(([_id, p]) =>
      (p.Properties.Roles ?? []).some(
        (r: { Ref?: string }) => typeof r === "object" && r.Ref?.includes("ExecutionRole"),
      ),
    );
    expect(execPolicies.length).toBeGreaterThan(0);

    const allActions = execPolicies
      .flatMap(([_id, p]) => p.Properties.PolicyDocument.Statement ?? [])
      .flatMap((s: { Action: string | string[] }) =>
        Array.isArray(s.Action) ? s.Action : [s.Action],
      );
    expect(allActions).toContain("secretsmanager:GetSecretValue");

    // Each statement that grants secretsmanager:* must scope to a specific
    // secret ARN (Fn::Join over the secret name) — never "*".
    for (const [_id, p] of execPolicies) {
      for (const s of p.Properties.PolicyDocument.Statement ?? []) {
        const actions = Array.isArray(s.Action) ? s.Action : [s.Action];
        if (actions.some((a: string) => a?.startsWith?.("secretsmanager:"))) {
          const resources = Array.isArray(s.Resource) ? s.Resource : [s.Resource];
          for (const r of resources) {
            expect(r).not.toBe("*");
          }
        }
      }
    }
  });

  it("creates the horde-runs DynamoDB table with PAY_PER_REQUEST billing", () => {
    const t = synth();
    t.hasResourceProperties("AWS::DynamoDB::Table", {
      TableName: "horde-runs-test",
      BillingMode: "PAY_PER_REQUEST",
      KeySchema: [{ AttributeName: "id", KeyType: "HASH" }],
    });
  });

  it("creates 4 GSIs on the runs table (by-repo, by-ticket, by-status, by-instance)", () => {
    const t = synth();
    const tables = t.findResources("AWS::DynamoDB::Table");
    const runsTables = Object.values(tables).filter(
      (tbl) => tbl.Properties.TableName === "horde-runs-test",
    );
    expect(runsTables).toHaveLength(1);
    const gsis = runsTables[0].Properties.GlobalSecondaryIndexes;
    expect(gsis).toBeDefined();
    const names = gsis.map((g: { IndexName: string }) => g.IndexName).sort();
    expect(names).toEqual(["by-instance", "by-repo", "by-status", "by-ticket"]);

    const byInstance = gsis.find((g: { IndexName: string }) => g.IndexName === "by-instance");
    expect(byInstance.KeySchema).toEqual([{ AttributeName: "instance_id", KeyType: "HASH" }]);
    expect(byInstance.Projection.ProjectionType).toBe("ALL");

    for (const name of ["by-repo", "by-ticket", "by-status"]) {
      const idx = gsis.find((g: { IndexName: string }) => g.IndexName === name);
      const pkName = name === "by-repo" ? "repo" : name === "by-ticket" ? "ticket" : "status";
      expect(idx.KeySchema).toEqual([
        { AttributeName: pkName, KeyType: "HASH" },
        { AttributeName: "started_at", KeyType: "RANGE" },
      ]);
      expect(idx.Projection.ProjectionType).toBe("ALL");
    }
  });

  it("creates a worker security group with no ingress and only 443 egress", () => {
    const t = synth();
    const sgs = t.findResources("AWS::EC2::SecurityGroup");
    const workerSgs = Object.values(sgs).filter((sg) =>
      JSON.stringify(sg.Properties.Tags ?? []).includes("horde-test-worker-sg"),
    );
    expect(workerSgs).toHaveLength(1);
    const sg = workerSgs[0];

    // No ingress rules at all.
    expect(sg.Properties.SecurityGroupIngress ?? []).toEqual([]);

    // Exactly one egress: 443 over TCP to 0.0.0.0/0.
    const egress = sg.Properties.SecurityGroupEgress ?? [];
    expect(egress).toHaveLength(1);
    expect(egress[0].IpProtocol).toBe("tcp");
    expect(egress[0].FromPort).toBe(443);
    expect(egress[0].ToPort).toBe(443);
    expect(egress[0].CidrIp).toBe("0.0.0.0/0");
  });

  it("writes an SSM String parameter at /horde/<slug>/config by default", () => {
    const t = synth();
    t.hasResourceProperties("AWS::SSM::Parameter", {
      Name: "/horde/test/config",
      Type: "String",
    });
  });

  it("respects a caller-provided ssmParameterPath", () => {
    const app = new App();
    const stack = new Stack(app, "S2", { env: { account: "111111111111", region: "us-east-1" } });
    const repo = ecr.Repository.fromRepositoryName(stack, "Repo", "horde-test");
    new HordeWorker(stack, "Horde", {
      projectSlug: "test",
      workerImage: ecs.ContainerImage.fromRegistry("public.ecr.aws/horde/test:latest"),
      ecrRepository: repo,
      secrets: {
        CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "C", "c"),
        GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "G", "g"),
      },
      ssmParameterPath: "/custom/horde/cfg",
    });
    Template.fromStack(stack).hasResourceProperties("AWS::SSM::Parameter", {
      Name: "/custom/horde/cfg",
    });
  });

  it("SSM config JSON references every required HordeConfig field", () => {
    const t = synth();
    const params = t.findResources("AWS::SSM::Parameter");
    const cfg = Object.values(params).find((p) => p.Properties.Name === "/horde/test/config");
    if (!cfg) throw new Error("expected /horde/test/config parameter");
    // The Value is an Fn::Join over alternating literal and token segments.
    // Stringify everything and check each key appears in the literal pieces.
    const valueStr = JSON.stringify(cfg.Properties.Value);
    // The template's literal string fragments are JSON-escaped (\" becomes \\\")
    // when passed through JSON.stringify, so each key reads as \"cluster_arn\".
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
    expect(valueStr).toContain('\\"DISABLED\\"');
    expect(valueStr).toContain('\\"ecs\\"');
    expect(valueStr).toContain('\\"max_concurrent\\":5');
    expect(valueStr).toContain('\\"default_timeout_minutes\\":1440');
  });

  it("creates a CLI user managed policy with all six required Sids", () => {
    const t = synth();
    const policies = t.findResources("AWS::IAM::ManagedPolicy");
    const cliPolicies = Object.values(policies).filter((p) =>
      p.Properties.ManagedPolicyName?.["Fn::Join"]?.[1]?.some?.(
        (seg: string) => typeof seg === "string" && seg.includes("horde-test-cli-"),
      ) ||
      (typeof p.Properties.ManagedPolicyName === "string" &&
        p.Properties.ManagedPolicyName.includes("horde-test-cli-")),
    );
    expect(cliPolicies).toHaveLength(1);
    const stmts = cliPolicies[0].Properties.PolicyDocument.Statement as Array<{
      Sid: string;
    }>;
    const sids = stmts.map((s) => s.Sid).sort();
    expect(sids).toEqual([
      "ArtifactsRead",
      "DynamoRunsTable",
      "EcsPassRole",
      "EcsRun",
      "LogsRead",
      "SsmRead",
    ]);
  });

  it("EcsRun is scoped to the cluster ARN via condition", () => {
    const t = synth();
    const policies = t.findResources("AWS::IAM::ManagedPolicy");
    const cliPolicy = Object.values(policies)[0];
    const ecsRun = cliPolicy.Properties.PolicyDocument.Statement.find(
      (s: { Sid: string }) => s.Sid === "EcsRun",
    );
    expect(ecsRun.Condition.ArnEquals).toBeDefined();
    expect(ecsRun.Condition.ArnEquals["ecs:cluster"]).toBeDefined();
  });

  it("emits a CfnOutput named CliUserManagedPolicyArn", () => {
    const t = synth();
    const outputs = t.findOutputs("*");
    const matchingKeys = Object.keys(outputs).filter((k) =>
      k.includes("CliUserManagedPolicyArn"),
    );
    expect(matchingKeys.length).toBeGreaterThan(0);
  });

  it("matches the saved snapshot", () => {
    expect(synth().toJSON()).toMatchSnapshot();
  });

  it("rejects an unsupported logRetentionDays value", () => {
    const app = new App();
    const stack = new Stack(app, "BadStack");
    const repo = ecr.Repository.fromRepositoryName(stack, "Repo", "horde-test");
    expect(
      () =>
        new HordeWorker(stack, "Horde", {
          projectSlug: "test",
          workerImage: ecs.ContainerImage.fromRegistry("public.ecr.aws/horde/test:latest"),
          ecrRepository: repo,
          secrets: {
            CLAUDE_CODE_OAUTH_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "C", "c"),
            GIT_TOKEN: secretsmanager.Secret.fromSecretNameV2(stack, "G", "g"),
          },
          logRetentionDays: 42,
        }),
    ).toThrow(/logRetentionDays=42/);
  });
});

describe("HordeWorker status-sync (5fh.12/13/14)", () => {
  it("creates a Node.js 20 status lambda with the expected env vars", () => {
    const t = synth();
    t.hasResourceProperties("AWS::Lambda::Function", {
      FunctionName: "horde-test-status-updater",
      Runtime: "nodejs20.x",
      Handler: "bundle.handler",
      Timeout: 30,
      MemorySize: 256,
      Environment: {
        Variables: Match.objectLike({
          RUNS_TABLE: Match.anyValue(),
          ARTIFACTS_BUCKET: Match.anyValue(),
        }),
      },
    });
  });

  it("creates an EventBridge rule matching STOPPED ECS tasks on this cluster", () => {
    const t = synth();
    const rules = t.findResources("AWS::Events::Rule");
    const statusRules = Object.values(rules).filter(
      (r) => r.Properties.Name === "horde-test-status",
    );
    expect(statusRules).toHaveLength(1);
    const rule = statusRules[0];
    expect(rule.Properties.State).toBe("ENABLED");
    expect(rule.Properties.EventPattern.source).toEqual(["aws.ecs"]);
    expect(rule.Properties.EventPattern["detail-type"]).toEqual(["ECS Task State Change"]);
    expect(rule.Properties.EventPattern.detail.lastStatus).toEqual(["STOPPED"]);
    const clusterArn = rule.Properties.EventPattern.detail.clusterArn;
    expect(Array.isArray(clusterArn)).toBe(true);
    expect(clusterArn.length).toBeGreaterThan(0);
    const targets = rule.Properties.Targets ?? [];
    expect(targets.length).toBeGreaterThan(0);
    expect(targets[0].Arn).toBeDefined();
  });

  it("grants EventBridge lambda:InvokeFunction on the status lambda", () => {
    synth().hasResourceProperties("AWS::Lambda::Permission", {
      Action: "lambda:InvokeFunction",
      Principal: "events.amazonaws.com",
    });
  });

  it("grants the status lambda exactly Query+GetItem+UpdateItem on the table + by-instance GSI", () => {
    const t = synth();
    const policies = t.findResources("AWS::IAM::Policy");
    const lambdaPolicies = Object.values(policies).filter((p) =>
      (p.Properties.Roles ?? []).some(
        (r: { Ref?: string }) => typeof r === "object" && r.Ref?.includes("StatusLambdaServiceRole"),
      ),
    );
    expect(lambdaPolicies).toHaveLength(1);

    const stmts = lambdaPolicies[0].Properties.PolicyDocument.Statement as Array<{
      Sid?: string;
      Action: string | string[];
      Resource: unknown;
    }>;
    const dynamo = stmts.find((s) => s.Sid === "DynamoRunsTableRW");
    expect(dynamo).toBeDefined();
    const actions = Array.isArray(dynamo!.Action) ? dynamo!.Action : [dynamo!.Action];
    expect(actions.sort()).toEqual([
      "dynamodb:GetItem",
      "dynamodb:Query",
      "dynamodb:UpdateItem",
    ]);
    const resources = Array.isArray(dynamo!.Resource) ? dynamo!.Resource : [dynamo!.Resource];
    const resourceStr = JSON.stringify(resources);
    expect(resourceStr).toContain("index/by-instance");
    expect(resourceStr).not.toContain("index/*");
  });

  it("grants the status lambda s3:GetObject (and ListBucket) on the artifacts bucket", () => {
    const t = synth();
    const policies = t.findResources("AWS::IAM::Policy");
    const lambdaPolicies = Object.values(policies).filter((p) =>
      (p.Properties.Roles ?? []).some(
        (r: { Ref?: string }) => typeof r === "object" && r.Ref?.includes("StatusLambdaServiceRole"),
      ),
    );
    const stmts = lambdaPolicies[0].Properties.PolicyDocument.Statement as Array<{
      Sid?: string;
      Action: string | string[];
    }>;
    const s3Stmt = stmts.find((s) => s.Sid === "ArtifactsRead");
    expect(s3Stmt).toBeDefined();
    const actions = Array.isArray(s3Stmt!.Action) ? s3Stmt!.Action : [s3Stmt!.Action];
    expect(actions).toEqual(expect.arrayContaining(["s3:GetObject", "s3:ListBucket"]));
    for (const a of actions) {
      expect(a).not.toMatch(/^s3:Put/);
      expect(a).not.toMatch(/^s3:Delete/);
    }
  });

  it("does NOT grant the status lambda ecs/ssm/secrets permissions", () => {
    const t = synth();
    const policies = t.findResources("AWS::IAM::Policy");
    const lambdaPolicies = Object.values(policies).filter((p) =>
      (p.Properties.Roles ?? []).some(
        (r: { Ref?: string }) => typeof r === "object" && r.Ref?.includes("StatusLambdaServiceRole"),
      ),
    );
    const allActions = lambdaPolicies
      .flatMap((p) => p.Properties.PolicyDocument.Statement ?? [])
      .flatMap((s: { Action: string | string[] }) =>
        Array.isArray(s.Action) ? s.Action : [s.Action],
      );
    for (const a of allActions) {
      expect(a).not.toMatch(/^ecs:/);
      expect(a).not.toMatch(/^ssm:/);
      expect(a).not.toMatch(/^secretsmanager:/);
    }
  });
});
