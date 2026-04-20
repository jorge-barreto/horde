# horde CDK consumer example

Minimal CDK app that provisions a horde worker cluster using
[`@horde.io/cdk`](https://www.npmjs.com/package/@horde.io/cdk).

Clone this directory into your own repo, edit `app.ts` to set `SLUG` to
match your git remote, and follow the steps below.

## 1. Install

```bash
npm install
```

## 2. Synth (sanity check)

```bash
npm run synth
```

Produces the CloudFormation template under `cdk.out/`. No AWS calls.

## 3. Deploy

```bash
# First time only: bootstrap CDK in your account/region (idempotent).
npx cdk bootstrap

# Deploy the stack. ~5 min on a cold account.
npm run deploy
```

The stack outputs include:

- `SsmConfigPath` — the SSM parameter horde reads to discover the cluster
- `EcrRepoUri` — where `horde push` uploads the worker image
- `CliUserManagedPolicyArn` — attach this to the IAM user/role that runs `horde launch`

## 4. Populate secrets

```bash
aws secretsmanager put-secret-value \
  --secret-id horde-<slug>-claude-code-oauth-token \
  --secret-string "$CLAUDE_CODE_OAUTH_TOKEN"

aws secretsmanager put-secret-value \
  --secret-id horde-<slug>-git-token \
  --secret-string "$GIT_TOKEN"
```

## 5. Push the worker image

From your project directory (the one with the git remote matching `SLUG`):

```bash
horde push
```

`horde push` builds `horde-worker-base:latest` (from files embedded in
the horde binary), extends it with your `worker/Dockerfile` if present,
then pushes to the ECR repo this stack owns.

## 6. Launch a ticket

```bash
horde launch DEMO-123 --workflow demo
horde status DEMO-123
horde results DEMO-123
```

## Teardown

```bash
npm run destroy
```

The bucket, ECR repo, and secrets are set to `REMOVE`/`emptyOnDelete` for
easy teardown. For a real deployment you'll want `RETAIN` — see `app.ts`.

## What this proves

`npm run synth` succeeds → the construct's public API works with a fresh
`npm install @horde.io/cdk`. That's the CI-relevant signal; `cdk deploy`
is reserved for your own AWS account.
