# Latest Replay Analysis

## Verdict

PASS.

This replay is the current acceptance evidence for the file-backed Flow + HITL scenario. It used the real DeepSeek client, four agents, a workspace skill, ten Flow steps, nine persisted markdown artifacts, one Flow-level human approval, and nine runtime tool-gate approvals for `write_file`. It was rerun after runtime-native tools were brought under the same step allowlist as profile/workspace tools.

## Replay Identity

- Replay root: `.xira/live-tests/file-flow-skill-20260616-161541`
- Flow run: `fr_20260616_f64b77302a9a`
- Flow id: `live-deepseek-flow-file-artifacts-skill`
- Result: `completed`

Command:

```bash
DEEPSEEK_API_KEY="$(tr -d '\r\n' < DEEPSEEK_API_KEY)" \
XIRA_DEEPSEEK_LIVE=1 \
XIRA_LIVE_ARTIFACT_ROOT=/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-20260616-161541 \
GOCACHE=$(pwd)/.cache/go-build \
go test -count=1 ./apps/xira/internal/runtime -run "TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill" -v
```

Result:

```text
--- PASS: TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill (149.51s)
PASS
ok  	github.com/xiramesh/xira/internal/runtime	149.967s
```

## Flow Contract

| Step | Agent | Contract |
| --- | --- | --- |
| `write_brief` | `flow-writer` | write `01-brief.md`; no read/search/list |
| `write_research` | `flow-research` | write `02-research.md`; no read/search/list |
| `write_constraints` | `flow-reviewer` | write `03-constraints.md`; no read/search/list |
| `synthesize_plan` | `flow-architect` | read `01-brief.md` and `03-constraints.md`; write `04-plan.md` |
| `approve_plan` | human approval | approve the plan before implementation |
| `implementation_slice` | `flow-architect` | read `04-plan.md`; write `05-implementation.md` |
| `risk_review` | `flow-reviewer` | read `02-research.md` and `05-implementation.md`; write `06-risk.md` |
| `test_plan` | `flow-reviewer` | search `BRIEF-MARKER-101` under `artifacts/flow-files`; read `06-risk.md`; write `07-test-plan.md` |
| `release_notes` | `flow-writer` | read `03-constraints.md` and `07-test-plan.md`; write `08-release.md` |
| `final_report` | `flow-research` | list `artifacts/flow-files`; read `01-brief.md`, `04-plan.md`, and `08-release.md`; write `09-final-report.md` |

## Tool Evidence

Exact aggregate tool counts from `runs/*/tool_calls.jsonl`:

```text
write_file: 9
read_file: 11
search_file: 1
list_dir: 1
```

The test asserts exact per-step tool names and exact per-tool input values. It also constrains runtime tool schemas per Flow step using `executor.tools` and `executor.tool_input_allowlist`. Native tools such as `human.request`, `emit_status`, and `delegate_agent` must be explicit in the same allowlist.

## HITL Evidence

Human request files are preserved under:

```text
.xira/live-tests/file-flow-skill-20260616-161541/state/workspaces/ws_b9355e01f3851fa1/human-requests/
```

There are 10 human requests:

- 9 runtime tool-gate requests for `write_file`
- 1 Flow-level `approve_plan` request

## Failure History Preserved For Review

| Replay | Useful failure |
| --- | --- |
| `file-flow-skill-20260616-141013` | Unknown markdown reference guard caught generated skill-source filename references. |
| `file-flow-skill-20260616-141402` | Model attempted an extra `list_dir`, exposing the need for step-level tool-name allowlists. |
| `file-flow-skill-20260616-141838` | Generated artifact text repeated invalid markdown filename literals from the prompt. |
| `file-flow-skill-20260616-142213` | Plan artifact falsely claimed initial steps read prior files. |
| `file-flow-skill-20260616-142456` | Approved tool replay failed because action snapshot hashing was sensitive to YAML normalization of leading newline content. |
| `file-flow-skill-20260616-143157` | Model attempted an extra `read_file` path, exposing the need for step-level tool-input allowlists. |
| `file-flow-skill-20260616-143813` | Plan artifact had incorrect dependency claims for later steps; final test now requires a concrete tool contract table. |

## Remaining Product Notes

- Completed Flow steps still preserve historical `interrupt` blocks in `flow_run.yaml`.
- Step artifacts are still broad `artifacts/` references even though the meaningful files are under `artifacts/flow-files/`.
- The live DeepSeek replay is intentionally expensive and remains env-gated by `XIRA_DEEPSEEK_LIVE=1`.
