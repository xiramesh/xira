# Evidence Manifest

This file lists the concrete evidence a reviewer should inspect. It intentionally points to original artifacts instead of copying them into this package.

## Code Evidence

| Area | Path | What To Check |
| --- | --- | --- |
| Live Flow test | `apps/xira/internal/runtime/deepseek_hitl_live_test.go` | `TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill` defines the 10-step live DeepSeek Flow. |
| Flow replay driver | `apps/xira/internal/runtime/deepseek_hitl_live_test.go` | `approveAndDrainLiveFlow` approves runtime tool-gate requests and Flow-level approvals until completion or timeout. |
| Step tool contract assertions | `apps/xira/internal/runtime/deepseek_hitl_live_test.go` | `assertStepToolContract` checks exact tool names and exact tool input values per step. |
| Artifact filename guard | `apps/xira/internal/runtime/deepseek_hitl_live_test.go` | `assertArtifactReferencesKnownFiles` scans markdown references by basename allowlist, not only `NN-*.md`. |
| Artifact semantic guard | `apps/xira/internal/runtime/deepseek_hitl_live_test.go` | `assertInitialStepsDoNotClaimReads` and `assertPlanToolContractTable` reject misleading plan claims. |
| Runtime resume boundary | `apps/xira/internal/runtime/human_request_resume.go` | Approved tool-output replay runs with profile tools and skills removed. |
| Runtime native tool allowlist | `apps/xira/internal/runtime/service.go`, `apps/xira/internal/runtime/service_adk.go` | Approved replay disables native tools; ordinary Flow step allowlists also filter native tools such as human request/status/delegate. |
| Flow tool allowlist schema | `apps/xira/internal/flow/types.go`, `apps/xira/internal/runtime/types.go` | Flow can carry allowed tool names and allowed input values into runtime turns. |
| Snapshot canonicalization | `apps/xira/internal/runtime/human_requests.go` | Tool-gate action snapshot arguments are canonicalized before hashing and replay. |

## Replay Evidence

Latest successful replay root:

```text
.xira/live-tests/file-flow-skill-20260616-161541
```

Summary analysis:

```text
.xira/live-tests/file-flow-skill-20260616-161541/ANALYSIS.md
docs/review-packages/xira-flow-file-backed-live-test/LATEST_REPLAY_ANALYSIS.md
```

Flow run state:

```text
.xira/live-tests/file-flow-skill-20260616-161541/state/flow-runs/fr_20260616_f64b77302a9a/flow_run.yaml
.xira/live-tests/file-flow-skill-20260616-161541/state/flow-runs/fr_20260616_f64b77302a9a/definition.yaml
.xira/live-tests/file-flow-skill-20260616-161541/state/flow-runs/fr_20260616_f64b77302a9a/events.jsonl
```

Per-agent run evidence is under:

```text
.xira/live-tests/file-flow-skill-20260616-161541/runs/
```

Each run directory should contain:

```text
audit.jsonl
events.jsonl
llm_calls.jsonl
run.yaml
tool_calls.jsonl
usage.json
verification.json
```

Usage ledger:

```text
.xira/live-tests/file-flow-skill-20260616-161541/state/usage-ledger.jsonl
```

Human requests and responses:

```text
.xira/live-tests/file-flow-skill-20260616-161541/state/workspaces/ws_b9355e01f3851fa1/human-requests/
.xira/live-tests/file-flow-skill-20260616-161541/state/workspaces/ws_b9355e01f3851fa1/human-responses/
.xira/live-tests/file-flow-skill-20260616-161541/state/workspaces/ws_b9355e01f3851fa1/replay-results/
```

## Replay Runs

The latest successful replay contains these run directories:

```text
runs/20260616-161542-flow-writer
runs/20260616-161547-flow-research
runs/20260616-161602-flow-reviewer
runs/20260616-161614-flow-architect
runs/20260616-161632-flow-architect
runs/20260616-161646-flow-reviewer
runs/20260616-161706-flow-reviewer
runs/20260616-161730-flow-writer
runs/20260616-161746-flow-research
```

There are 9 agent runs because `approve_plan` is a Flow-level human approval step, not an agent run.

## Expected Artifact Files

The test expects these workspace files to exist in the replay workspace:

```text
workspace/artifacts/flow-files/01-brief.md
workspace/artifacts/flow-files/02-research.md
workspace/artifacts/flow-files/03-constraints.md
workspace/artifacts/flow-files/04-plan.md
workspace/artifacts/flow-files/05-implementation.md
workspace/artifacts/flow-files/06-risk.md
workspace/artifacts/flow-files/07-test-plan.md
workspace/artifacts/flow-files/08-release.md
workspace/artifacts/flow-files/09-final-report.md
```

The test asserts marker propagation:

| File | Required Markers |
| --- | --- |
| `01-brief.md` | `BRIEF-MARKER-101` |
| `02-research.md` | `RESEARCH-MARKER-202` |
| `03-constraints.md` | `CONSTRAINT-MARKER-303` |
| `04-plan.md` | `PLAN-MARKER-404`, `BRIEF-MARKER-101`, `CONSTRAINT-MARKER-303` |
| `05-implementation.md` | `IMPL-MARKER-505`, `PLAN-MARKER-404` |
| `06-risk.md` | `RISK-MARKER-606`, `RESEARCH-MARKER-202`, `IMPL-MARKER-505` |
| `07-test-plan.md` | `TEST-MARKER-707`, `BRIEF-MARKER-101`, `RISK-MARKER-606` |
| `08-release.md` | `RELEASE-MARKER-808`, `CONSTRAINT-MARKER-303`, `TEST-MARKER-707` |
| `09-final-report.md` | `FINAL-MARKER-909`, `BRIEF-MARKER-101`, `PLAN-MARKER-404`, `RELEASE-MARKER-808` |

## Commands Already Run

Targeted non-live compile/runtime tests:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestRuntimeToolAllowlistFiltersProfileTools|TestApprovedToolReplayDisablesRuntimeNativeTools|TestNativeApprovedToolReplayRejectsHumanRequestToolCall|TestApproveReplaysSnapshotWithLeadingNewlineContent|TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill'
```

Targeted Flow tests:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run 'TestAgentExecutorPassesStepAllowedTools|TestAgentExecutorBuildsTurnRequestFromStep'
```

Live DeepSeek replay:

```bash
DEEPSEEK_API_KEY="$(tr -d '\r\n' < DEEPSEEK_API_KEY)" \
XIRA_DEEPSEEK_LIVE=1 \
XIRA_LIVE_ARTIFACT_ROOT=/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-20260616-161541 \
GOCACHE=$(pwd)/.cache/go-build \
go test -count=1 ./apps/xira/internal/runtime -run 'TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill' -v
```

Result:

```text
--- PASS: TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill (149.51s)
PASS
ok  	github.com/xiramesh/xira/internal/runtime	149.967s
```

## Useful Failed Replays

These failures should be treated as design feedback, not as test flakiness:

| Replay | Meaning | Resulting Change |
| --- | --- | --- |
| `file-flow-skill-20260616-141013` | Generated artifact referenced skill-source markdown. | Broadened unknown markdown reference guard. |
| `file-flow-skill-20260616-141402` | Model attempted an extra `list_dir`. | Added exact per-step tool-name allowlist. |
| `file-flow-skill-20260616-141838` | Prompt literals leaked into artifact content. | Removed invalid markdown filename literals from prompts. |
| `file-flow-skill-20260616-142213` | Plan claimed initial steps read prior artifacts. | Added semantic guard for initial-step read claims. |
| `file-flow-skill-20260616-142456` | Approved replay failed due snapshot hash drift after YAML normalization. | Canonicalized action snapshot arguments before hashing. |
| `file-flow-skill-20260616-143157` | Model attempted an extra allowed-tool read path. | Added exact per-step tool-input allowlist. |
| `file-flow-skill-20260616-143813` | Plan claimed later dependencies that did not match actual tool contract. | Required and asserted a concrete tool contract table. |
| `file-flow-skill-20260616-161541` | Successful replay after native tools were brought under the same step allowlist. | Current known-good replay. |
