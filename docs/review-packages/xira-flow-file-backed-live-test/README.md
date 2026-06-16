# Xira Flow File-Backed Live Test Review Package

This package is for reviewing the live DeepSeek Flow + HITL acceptance test added during the Flow hardening work.

The goal is not to prove that a test can pass. The goal is to verify that Flow behaves correctly under a realistic multi-agent, file-backed, human-in-the-loop workflow with a real model.

## What To Review First

1. Test entrypoint:
   - `apps/xira/internal/runtime/deepseek_hitl_live_test.go`
   - Test: `TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill`

2. Runtime boundaries:
   - `apps/xira/internal/runtime/human_request_resume.go`
   - `apps/xira/internal/runtime/service.go`
   - `apps/xira/internal/runtime/service_adk.go`
   - Key behavior: approved tool-output replay disables runtime-native tools as well as profile tools and skills.

3. Flow tool contracts:
   - `apps/xira/internal/flow/types.go`
   - `apps/xira/internal/flow/executor.go`
   - `apps/xira/internal/runtime/flow_bridge.go`
   - Key behavior: Flow steps can define `executor.tools` and `executor.tool_input_allowlist`.

4. Successful replay evidence:
   - `.xira/live-tests/file-flow-skill-20260616-161541/ANALYSIS.md`
   - `docs/review-packages/xira-flow-file-backed-live-test/LATEST_REPLAY_ANALYSIS.md`
   - Replay root: `.xira/live-tests/file-flow-skill-20260616-161541`

5. Review checklist and evidence:
   - `docs/review-packages/xira-flow-file-backed-live-test/REVIEW_CHECKLIST.md`
   - `docs/review-packages/xira-flow-file-backed-live-test/EVIDENCE.md`

## Why This Test Matters

This is a live acceptance test for Flow as a product abstraction.

It verifies that a real DeepSeek-backed Flow can:

- Run a 10-step workflow.
- Use 4 different agents.
- Activate a workspace skill.
- Write multiple durable files.
- Read non-adjacent earlier files in later steps.
- Search previous artifacts.
- Route through a Flow-level human approval gate.
- Route every `write_file` through runtime tool-gate HITL.
- Persist and verify `tool_calls.jsonl`.
- Enforce exact per-step tool names and tool input values.
- Enforce the same per-step allowlist on runtime-native tools.
- Reject generated references to unknown markdown artifact filenames.
- Verify that artifact text does not contradict the actual tool contract.

Deterministic tests validate state transitions and local contracts. They cannot expose live model behavior such as tool reuse after resume, invented filenames, unauthorized extra reads, or prompt drift. This test intentionally exercises those boundaries with a real model.

## Business Scenario

The test models a realistic file-backed Flow:

1. A writer creates an initial brief.
2. A research agent creates source evidence.
3. A reviewer creates constraints requiring approval.
4. An architect reads non-adjacent earlier files and writes a plan.
5. A human approves the plan.
6. The architect writes an implementation slice from the approved plan.
7. A reviewer reads earlier research and implementation output to produce risk review.
8. A reviewer searches and reads prior artifacts to produce a test plan.
9. A writer combines constraints and test plan into release notes.
10. A research agent lists and reads selected prior files to write the final report.

This is intentionally longer than a smoke test. It checks whether Flow can preserve state, artifacts, tool evidence, and human decisions across a multi-agent chain.

## Test Shape

Flow id:

```text
live-deepseek-flow-file-artifacts-skill
```

Steps:

```text
write_brief
write_research
write_constraints
synthesize_plan
approve_plan
implementation_slice
risk_review
test_plan
release_notes
final_report
```

Agents:

```text
flow-writer
flow-research
flow-architect
flow-reviewer
```

Expected files:

```text
artifacts/flow-files/01-brief.md
artifacts/flow-files/02-research.md
artifacts/flow-files/03-constraints.md
artifacts/flow-files/04-plan.md
artifacts/flow-files/05-implementation.md
artifacts/flow-files/06-risk.md
artifacts/flow-files/07-test-plan.md
artifacts/flow-files/08-release.md
artifacts/flow-files/09-final-report.md
```

Required tool contract:

```text
write_brief          -> write 01-brief.md only
write_research       -> write 02-research.md only
write_constraints    -> write 03-constraints.md only
synthesize_plan      -> read 01-brief.md, 03-constraints.md; write 04-plan.md
implementation_slice -> read 04-plan.md; write 05-implementation.md
risk_review          -> read 02-research.md, 05-implementation.md; write 06-risk.md
test_plan            -> search BRIEF-MARKER-101 under artifacts/flow-files; read 06-risk.md; write 07-test-plan.md
release_notes        -> read 03-constraints.md, 07-test-plan.md; write 08-release.md
final_report         -> list artifacts/flow-files; read 01-brief.md, 04-plan.md, 08-release.md; write 09-final-report.md
```

## Respecting The Test

Do not weaken this test just because a live model run fails.

First ask:

```text
If this happened in a real user Flow, would the workflow be wrong, stuck, unverifiable, or misleading?
```

If the answer is yes, treat the failure as a Flow/runtime/prompt-design problem. Fix the system, runtime boundary, skill contract, or missing assertion. Do not make the test pass by accepting invalid business behavior.

Examples from the replay work:

- The model created another `write_file` request after an approved `write_file` output. That was a runtime resume bug, not a flaky test.
- Approved replay still exposed runtime-native tools. That was a runtime boundary bug, so replay mode now disables those too.
- Ordinary Flow steps still exposed runtime-native tools even when `executor.tools` declared only workspace/profile tools. Native tools now share the same step allowlist and must be explicit.
- The model attempted extra tools or extra file paths. That forced step-level tool and input allowlists.
- The model referenced non-artifact markdown filenames. That was invalid business output, so the test now rejects unknown markdown references by basename allowlist.
- The model wrote a plan with incorrect dependency claims. The plan artifact now has a tested tool contract table.
- Action snapshot hashing changed after YAML persistence normalized leading newline content. Snapshot arguments are now canonicalized before hashing and replay.

## How To Re-Run

The DeepSeek API key is stored locally in the file `DEEPSEEK_API_KEY`. Do not commit or print it.

Run the live test with an explicit artifact root:

```bash
DEEPSEEK_API_KEY="$(tr -d '\r\n' < DEEPSEEK_API_KEY)" \
XIRA_DEEPSEEK_LIVE=1 \
XIRA_LIVE_ARTIFACT_ROOT=/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-YYYYMMDD-HHMMSS \
GOCACHE=$(pwd)/.cache/go-build \
go test -count=1 ./apps/xira/internal/runtime -run 'TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill' -v
```

Run full regression:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/...
```

In the managed sandbox, tests using `httptest` may fail with local bind permission errors. Re-run outside the sandbox if the only failure is `listen tcp 127.0.0.1:0: bind: operation not permitted` or `[::1]:0: bind: operation not permitted`.

## Latest Known Good Replay

Replay root:

```text
.xira/live-tests/file-flow-skill-20260616-161541
```

Flow run:

```text
fr_20260616_f64b77302a9a
```

Result:

```text
completed
```

Tool counts:

```text
write_file: 9
read_file: 11
search_file: 1
list_dir: 1
```

Human requests:

```text
10 total
9 runtime tool-gate approvals for write_file
1 Flow-level approval at approve_plan
```

## Remaining Risks

These are not hidden by the test result:

- Completed Flow step state still preserves historical `interrupt` blocks, which is confusing in `flow_run.yaml`.
- Completed agent steps still list broad `artifacts/` references even though the meaningful files are under `artifacts/flow-files/`.
- The live DeepSeek replay is expensive and remains env-gated.
