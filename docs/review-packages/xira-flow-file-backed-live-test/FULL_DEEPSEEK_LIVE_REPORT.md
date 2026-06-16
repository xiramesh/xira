# Full DeepSeek Live Review Report

Date: 2026-06-16

Scope: real DeepSeek API verification for Xira Flow + HITL, including long multi-agent Flow, tool-using Flow, file-backed artifact Flow, skill activation, and runtime-native human request behavior.

## Final Result

Final full live suite passed:

```bash
DEEPSEEK_API_KEY="$(tr -d "\r\n" < DEEPSEEK_API_KEY)" \
XIRA_DEEPSEEK_LIVE=1 \
XIRA_LIVE_ARTIFACT_ROOT=/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-170642 \
GOCACHE=$(pwd)/.cache/go-build \
go test -count=1 ./apps/xira/internal/runtime -run "TestRealDeepSeek" -v
```

Result:

```text
PASS
ok  	github.com/xiramesh/xira/internal/runtime	211.693s
```

Primary preserved evidence root:

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-170642
```

## Live Cases Covered

- `TestRealDeepSeekHITLHumanRequestTool`: real DeepSeek calls `human.request` and runtime enters `waiting_human`.
- `TestRealDeepSeekHITLRequireConfirmationSnapshot`: real DeepSeek calls a confirmation-gated `write_file`; action snapshot is preserved.
- `TestRealDeepSeekHITLRespondsAfterApprovedToolOutput`: approved tool output is replayed and the model completes without opening a new human request.
- `TestRealDeepSeekHITLDelegateCompleted`: parent delegates to child agent and receives a completed delegate output.
- `TestRealDeepSeekHITLDelegateChildWaiting`: delegated child can suspend on human input and parent surfaces waiting state.
- `TestRealDeepSeekFlowAgentStepCompletes`: Flow agent step completes through real DeepSeek.
- `TestRealDeepSeekFlowRoutesToHumanApproval`: Flow routes from agent step into human approval.
- `TestRealDeepSeekLongFlowFourAgentsWithHITL`: 10-step Flow, 4 agents, HITL approval gate, all real DeepSeek calls.
- `TestRealDeepSeekLongFlowFourAgentsWithToolsAndHITL`: 10-step Flow, 4 agents, real workspace tools, HITL approval gate.
- `TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill`: 10-step file-backed Flow with skill activation, multiple artifact files, jump-back reads, search/list/read/write contracts, and HITL gates.

## File-Backed Flow Evidence

The final full run preserved the file-backed Flow under:

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-170642
```

Important final-step evidence:

- Final agent run: `20260616-170958-flow-research`
- Final step recorded `tool_calls=5`
- Tool sequence: `list_dir`, `read_file`, `read_file`, `read_file`, `write_file`
- Required read paths:
  - `artifacts/flow-files/01-brief.md`
  - `artifacts/flow-files/04-plan.md`
  - `artifacts/flow-files/08-release.md`
- Final artifact:
  - `/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-170642/workspace/artifacts/flow-files/09-final-report.md`

This verifies the intended scenario: early steps create independent files, later steps jump back to non-adjacent earlier artifacts, and the final step combines a directory listing plus multiple reads before writing the final report.

## Issues Found By Real DeepSeek Replay

1. Negated plan wording was misclassified as a positive read/search/list claim.

   Earlier real DeepSeek outputs used phrases such as "do not call read_file" and "require no prior reads". The old assertion treated those as positive read claims and failed for the wrong reason. The classifier now handles explicit and nearby negation, with a regression test.

2. `human.request` without `kind` failed incorrectly.

   Real DeepSeek called `human_request` with only `question`. Runtime converted missing `kind` through `fmt.Sprint(nil)`, producing `"<nil>"`, then rejected it. Runtime now defaults missing kind to `freeform`, with a regression test.

3. Tool-using Flow steps needed explicit per-step tool/input contracts.

   The long tool Flow now gives each tool step an `executor.tools` and `tool_input_allowlist` contract so the runtime constrains both exposed tools and accepted inputs.

4. ADK concurrent tool execution could drop tool call records.

   Real DeepSeek triggered concurrent tool calls in the file-backed Flow final step. Logs showed all three `read_file` calls executed, but `tool_calls.jsonl` preserved only two because ADK tool callbacks appended to shared slices concurrently. Runtime now synchronizes tool call, event, audit, and LLM-call recording. The rerun preserved all five final-step tool calls.

## Supporting Evidence Roots

Failed and diagnostic runs were intentionally preserved:

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-163440
/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-20260616-164015
/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-20260616-164423
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-164742
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-165700
/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-20260616-170311
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-170642
```

Notable intermediate proof:

- `/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-20260616-170311` passed the file-backed Flow after the concurrent record fix.
- `/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-165700` exposed the missing final-step `read_file` record despite execution logs proving the tool ran.

## Local Verification

```bash
git diff --check
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime ./apps/xira/internal/flow
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/...
```

Results:

```text
git diff --check: passed
ok  	github.com/xiramesh/xira/internal/runtime	5.136s
ok  	github.com/xiramesh/xira/internal/flow	3.607s
./apps/xira/...: passed with local bind permission enabled
```

Targeted real DeepSeek rerun after the concurrency fix:

```bash
DEEPSEEK_API_KEY="$(tr -d "\r\n" < DEEPSEEK_API_KEY)" \
XIRA_DEEPSEEK_LIVE=1 \
XIRA_LIVE_ARTIFACT_ROOT=/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-20260616-170311 \
GOCACHE=$(pwd)/.cache/go-build \
go test -count=1 ./apps/xira/internal/runtime -run "TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill$" -v
```

Result:

```text
PASS
ok  	github.com/xiramesh/xira/internal/runtime	196.276s
```

## Review Notes

- The final report should be reviewed together with `tool_calls.jsonl`, `run.yaml`, `events.jsonl`, and generated workspace artifacts, not only final model text.
- The live suite is intentionally strict: if a real-business contract fails, the runtime or fixture should be fixed instead of weakening the assertion.
- Real DeepSeek can issue parallel tool calls. Reviewers should check that recorded tool calls match actual execution logs, especially when multiple reads/searches occur in one turn.
- The preserved failed roots are useful regression material; do not delete them while this review is open.
