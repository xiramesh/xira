# Explicit Intentional Silence Design

Issue: #158. Part of #124. Follow-up to #156 and #160.

## Problem

An Agent Turn currently needs either a non-empty final response or a successful `notify_owner` delivery. That makes private notification the only way to finish without a public reply. It cannot represent the ordinary case where the Agent deliberately records memory, prepares internal state, or decides no action is needed and wants no outbound message at all.

An empty model response cannot itself mean success: it is also produced by truncation, provider errors, broken tool loops, and accidental omissions. Runtime needs a sealed, explicit outcome.

## Decision

Add a runtime-owned ADK tool named `finish_silent` with an empty, closed input schema. Calling it declares that the Agent deliberately wants this turn to complete without any outbound response. It does not send, schedule, or mutate external state.

The tool records a normal `ToolCallRecord`, a non-sensitive runtime event, and an audit entry. The model can choose the outcome, but it cannot provide a recipient, status, reason text, or identity. Runtime supplies the fixed reason `finish_silent`.

After the ADK loop ends with an empty final, runtime accepts silence only when:

1. at least one `finish_silent` call completed successfully; and
2. every recorded tool call in the turn completed without an error or failed/rejected status.

A successful `notify_owner` remains a separate intentional-silence reason with its existing retry semantics. A non-empty final remains a normal public response even if `finish_silent` was called earlier. Parent turns, spawned child turns, and HITL resume turns use the same verification helper, so a tool that is advertised in those paths cannot silently acquire contradictory completion semantics.

## Alternatives rejected

- Treat any empty final as silence: hides provider and loop failures.
- Use a magic final string such as `[SILENT]`: leaks protocol text and relies on parsing natural language.
- Infer silence after memory writes: turns one tool type into a hidden business rule and cannot represent pure no-op.
- Require `notify_owner`: creates an unwanted notification merely to satisfy runtime validation.
- Add a second Agent Loop or lightweight classifier: duplicates the model decision already made in the current turn.

## Failure and observability contract

- `finish_silent + empty final + no tool failures` → completed/passed, no outbound.
- sender or Agent memory update followed by successful `finish_silent` → completed/passed.
- any failed/rejected tool plus `finish_silent + empty final` → failed; silence cannot erase failure evidence.
- empty final without explicit silence or successful owner notification → failed as today.
- successful owner notification plus empty final → unchanged.
- event/audit payloads contain only fixed reason, tool call ID, Agent/run identifiers already present in runtime context, and status. They contain no model-written explanation or memory/message body.

`assistant.final` is still emitted only for a non-empty successful final. `run.finished` remains unconditional and carries the completed/failed status.

## Test contract

- The silence evaluator is contract code and every branch is covered.
- The ADK tool has no model-controlled fields and is absent when runtime-native tools are disabled or excluded by an explicit allowlist.
- Production-path fake-model tests cover pure silence, sender memory + silence, Agent memory + silence, failed tool + silence, and accidental empty final.
- Existing `notify_owner` retry/silence tests remain green.
- A real DeepSeek live test calls `finish_silent`, returns no public final, and completes without a live-gated skip.
