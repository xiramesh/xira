# Review Checklist

Use this checklist when reviewing the Flow file-backed live test and the runtime changes it forced.

## Test Intent

- [ ] The test validates a real business Flow, not a synthetic happy path.
- [ ] The test requires a real DeepSeek call path when `XIRA_DEEPSEEK_LIVE=1`.
- [ ] The test covers both Flow-level HITL and runtime tool-gate HITL.
- [ ] The test proves that later steps read persisted files, not only `${outputs...}` or model context.
- [ ] The test verifies that every non-human step records persisted tool calls.
- [ ] The test rejects unknown markdown artifact filename references.
- [ ] The test rejects artifact text that contradicts actual tool usage.

## Flow Shape

- [ ] There are 10 Flow steps.
- [ ] There are 4 distinct agents.
- [ ] `approve_plan` is a human approval step.
- [ ] All other steps are agent-backed.
- [ ] The step chain cannot skip directly to final success without producing the required files.

## File-Backed Behavior

- [ ] `write_brief`, `write_research`, and `write_constraints` write initial files without reading/searching/listing.
- [ ] `synthesize_plan` reads `01-brief.md` and `03-constraints.md`.
- [ ] `implementation_slice` reads `04-plan.md`.
- [ ] `risk_review` reads `02-research.md` and `05-implementation.md`.
- [ ] `test_plan` searches for `BRIEF-MARKER-101` under `artifacts/flow-files` and reads `06-risk.md`.
- [ ] `release_notes` reads `03-constraints.md` and `07-test-plan.md`.
- [ ] `final_report` lists `artifacts/flow-files` and reads `01-brief.md`, `04-plan.md`, and `08-release.md`.

## Runtime Tool Contracts

- [ ] `executor.tools` is carried from Flow definition into runtime `TurnRequest`.
- [ ] Runtime native and ADK tool definitions filter profile tools by the step allowlist.
- [ ] Runtime native and ADK tool definitions filter runtime-native tools by the same step allowlist.
- [ ] Runtime rejects non-allowlisted tool calls before execution.
- [ ] `executor.tool_input_allowlist` is carried into runtime `TurnRequest`.
- [ ] Runtime native and ADK tool schemas constrain allowed input values when possible.
- [ ] Runtime rejects non-allowlisted tool input values before execution or approval.
- [ ] The live test asserts exact tool names and exact input values for every agent step.

## HITL Behavior

- [ ] Every `write_file` creates a runtime human request.
- [ ] The live test driver approves runtime tool-gate requests.
- [ ] `approve_plan` creates a Flow-level human request.
- [ ] The live test driver approves the Flow-level request with `approval_signal=approve`.
- [ ] After tool approval, the agent run resumes and completes instead of opening another tool request for the same approved action.

## Runtime Resume Boundary

- [ ] `resumeRunAfterApprovedToolOutput` does not expose profile tools during the approved-output resume turn.
- [ ] `resumeRunAfterApprovedToolOutput` does not expose skills during the approved-output resume turn.
- [ ] `resumeRunAfterApprovedToolOutput` disables runtime-native tools such as human request/status/delegate.
- [ ] The resume prompt tells the model that the approved tool call already executed.
- [ ] The resumed run records a final answer from the approved tool output.
- [ ] The change does not break ordinary HITL resume tests.
- [ ] There is deterministic coverage for an approved replay where the model attempts `human_request`.

## Snapshot Replay

- [ ] Runtime tool-gate action snapshot arguments are canonicalized before hashing.
- [ ] Approved replay verifies the canonical snapshot instead of failing on YAML representation drift.
- [ ] There is deterministic coverage for leading-newline content in a `write_file` approval snapshot.

## Evidence Review

- [ ] Inspect `.xira/live-tests/file-flow-skill-20260616-161541/ANALYSIS.md`.
- [ ] Inspect `.xira/live-tests/file-flow-skill-20260616-161541/runs/*/tool_calls.jsonl` for exact write/read/search/list evidence.
- [ ] Inspect `.xira/live-tests/file-flow-skill-20260616-161541/runs/*/run.yaml` for agent id, model policy, tool calls, and final status.
- [ ] Inspect `.xira/live-tests/file-flow-skill-20260616-161541/state/usage-ledger.jsonl` for live model usage.
- [ ] Inspect `.xira/live-tests/file-flow-skill-20260616-161541/state/workspaces/ws_b9355e01f3851fa1/human-requests/` for HITL evidence.
- [ ] Confirm the replay root is not deleted or overwritten during review.

## Things Not To Do

- [ ] Do not remove marker assertions just because DeepSeek output varies.
- [ ] Do not accept invented artifact filenames.
- [ ] Do not accept extra tool calls or extra file paths when the step contract is meaningful.
- [ ] Do not hide repeated tool-gate requests by loosening the driver loop.
- [ ] Do not treat a model violating a meaningful business instruction as harmless if the same behavior would break a real Flow.
- [ ] Do not commit or print the DeepSeek key.

## Acceptable Future Tightening

- [ ] Store first-class file artifact refs instead of broad `artifacts/` refs.
- [ ] Remove or archive completed-step `interrupt` state to reduce replay confusion.
- [ ] Consider extracting the file-backed Flow fixture from Go string literals if it grows further.
