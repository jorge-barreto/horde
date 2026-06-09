# ECS E2E Coverage Extension Plan (2026-06-09)

Extend the real-AWS `TestECS*` sweep to cover features with no e2e coverage:
the #36 queue/event/spend backbone plus recently-landed PRs (token telemetry,
labels+filtering, per-launch env, JSON contract depth, identity override).

Target stack: the **throwaway** CDK e2e stack (`horde-jorge-barreto-horde-cdke2e`),
NEVER the persistent `PrepDesk-Horde`. Profile: `prepdesk` (acct 919303601549, us-east-1).

## Verified facts
- `HordeConfig` already has `EventBusName`/`MaxSpendPerWindow`/`SpendWindow` (ssm.go:137-148). No Go config change.
- Direct launch emits `run.started` (main.go:482, best-effort after running-update). Task 6 asserts both started+terminal.
- `--env` → `ContainerOverride.Environment` KeyValuePair on horde-worker (ecs.go:207-230).
- Existing branch-override precedent: `resumeWorkflowBranch()` (ecs_resume_test.go:15), `HORDE_E2E_SIDECAR_BRANCH` (ecs_sidecar_test.go:44).
- `quick-success`/`quick-fail`/`slow`/`postgres-probe`/`resume-marker` are on origin/main. New fixtures (env-echo, token-probe) are NOT — reach via `queueE2EBranch()` defaulting to `worktree-run-lifecycle-event-backbone` (pushed, PR #55).

## Decisions (locked)
- Token test: real-agent `token-probe` workflow (one haiku turn), assert tokens.input>0 + cost>0.
- Spend cap: add generous maxSpendPerWindow:1000 + spendWindow to e2e app; assert SSM+drain-env plumbing (NOT a forced exceed).
- Events: transient SQS capture (temp queue + EB rule on bus → poll → assert Detail shape). Bus name from hc.EventBusName.

## Tasks (TDD acceptance tests; all named TestECS* → auto-picked by e2e-test sweep)
1. cdk/e2e/app.ts: maxSpendPerWindow:1000 + spendWindow:24h + EventBusNameOut CfnOutput. cdk_e2e_test.go: cdkE2EState.EventBusName + readCDKOutputs. REQUIRES npm run build + e2e-up redeploy.
2. New .orc fixtures: env-echo.yaml, token-probe.yaml, prompts/token-probe.md. Push to branch.
3. Harness infra: ecsDriver attr-readers (StorePriority/StoreEnqueuedAt/StoreLabels/StoreTokens/StoreCostUSD, ConsistentRead:true), stash hc+awsCfg on driver; harness Enqueue/EnqueueJSON/QueueListJSON/QueuePrioritize/QueueCancel/ListJSON.
4. harness_events_test.go: eventCapture (SQS+EB rule create/poll/teardown). New deps: sqs, eventbridge.
5. ecs_spendcap_test.go: TestECSSpendCapConfigured — SSM has max_spend_per_window/spend_window/event_bus_name; drain Lambda env has MAX_SPEND_PER_WINDOW/SPEND_WINDOW_SECONDS/MAX_CONCURRENT (lambda.GetFunctionConfiguration). New dep: lambda.
6. ecs_events_test.go: TestECSRunLifecycleEvents — run.started + run.terminal Detail shapes (version=1, repo!="", status, exit_code).
7. ecs_env_test.go: TestECSPerLaunchEnv — --env injected, echoed in logs (env-echo workflow, branch override).
8. ecs_tokens_test.go: TestECSTokenTelemetry — tokens.input>0 + cost>0 (token-probe workflow, branch override, 15m timeout).
9. ecs_queue_test.go: TestECSQueueDrainCycle (non-parallel), TestECSQueuePrioritize, TestECSQueueCancel.
10. ecs_labels_test.go: TestECSLabelsAndFilters — --label stored+filtered; --status/--workflow/--ticket filters; GSI retry loop.
11. ecs_jsoncontract_test.go: TestECSStatusJSONFullContract — full StatusV1 field set populated.
12. (light) TestECSRepoOverridePartitioning — --repo doesn't re-partition; HORDE_SSM_PATH covered by construction.

## Run order
1. Commit+push new .orc fixtures to branch.
2. cd cdk && npm ci && npm run build.
3. make build.
4. make e2e-up (REDEPLOY — spend cap + bus output).
5. make e2e-test.
6. make e2e-down (ALWAYS).

## Risks
- maxConcurrent:20 vs parallel peak — queue-drain test non-parallel.
- Drain race: ClaimNextQueued atomic; assert transition not which drainer.
- Event latency: setup capture + ~5s before launch; SQS long-poll 20s, 3m budget; SQS queue policy granting events.amazonaws.com:SendMessage (silent-drop trap).
- GSI eventual consistency: per-run GetItem ConsistentRead:true; list/queue-list assertions retry ~30s.
- Real-agent cost: one haiku turn, assert input>0/cost>0 only.
