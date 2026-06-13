# DevRun Task Spec Template

DevRun turns one development request, bug report, or selected issue into a verified code change.

The flow must keep scope explicit, record artifacts for each stage, and pause at human approval gates when policy requires it.

## Non-Goals

- Do not merge without an explicit approval signal.
- Do not treat a CLI agent invocation as an untracked side effect.
- Do not store large stdout or stderr directly in the flow run state; reference command artifacts instead.

## Acceptance

- The flow can start from ad hoc request, bug report, or issue selection.
- Each completed step records required output slots.
- Failed verification routes to fix.
- Blocking review findings route to fix.
- Merge requires approval when `runtime.policy.require_merge_approval` is true.
