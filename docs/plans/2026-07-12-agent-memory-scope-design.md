# Sender and Agent Memory Scope Design

Issue: #159. Follow-up to #128 and part of #124.

## Problem

Xira currently has one persistent memory address space: the current sender. The runtime preserves the real sender even when a group message addresses the owner, then `update_memory` resolves its path from `ChatKey.SenderID`. This is correct for facts about the speaker, but it cannot express memories that belong to the Agent itself, such as an accepted follow-up or an operational lesson that must remain available across senders.

Addressing and memory ownership are independent facts:

- `addressed_to=agent|owner` tells the existing Agent Loop which role the message invokes;
- `scope=sender|agent` tells the memory tool what the remembered content is about.

The model chooses the memory scope from semantics and the Agent prompt. Runtime only seals the identity behind that scope.

## Decision

Keep one memory subsystem and add a sealed optional `scope` parameter to `update_memory` and `forget_memory`:

- `sender` (default): resolve the current real sender from ChatKey, preserving all #128 behavior and files;
- `agent`: resolve the current Agent from the runtime-created registry. The model never supplies an Agent ID.

Agent memory is scoped by the service workspace plus `agent_id`. `stateDir` already isolates the service/workspace, so the on-disk suffix only needs the normalized Agent ID. Sender and Agent files use distinct path prefixes and cannot collide.

Every normal Agent Turn injects two independently labelled, independently capped untrusted-data blocks when present:

1. `# Sender Memory` from the current sender;
2. `# Agent Memory` from the current profile Agent ID.

Owner memory is not loaded merely because the message addresses the owner. No Agent Notes, owner inbox, cron scheduler, second Agent Loop, or task state machine is introduced.

## Alternatives rejected

### Route all owner-addressed memory to the owner

Rejected because `addressed_to=owner` does not change who spoke. It would let any authorized third party poison the owner's sender memory merely by mentioning the owner.

### Add a separate Agent Notes domain

Rejected because notes, lessons, and accepted follow-ups are all Agent-owned memories. A new lifecycle and store would duplicate key/upsert/expiry/forget behavior already present in memory.

### Add separate tools for sender and Agent memory

Viable but unnecessarily expands the model's tool surface. A sealed `scope` parameter expresses the same semantic choice while reusing validation, persistence, expiry, and forget behavior.

### Start a second Agent Loop

Rejected because two subjects do not require two model invocations. One existing turn sees addressing facts plus both memory blocks and makes one coherent decision. Future cron is only another trigger into this same Loop.

## Failure and compatibility behavior

- Missing scope behaves exactly like current #128 and uses sender memory.
- Unknown or non-string scope fails closed; it never falls back to sender.
- Sender scope without a ChatKey sender fails as today.
- Agent scope without a configured Agent identity fails closed.
- A sender and Agent may use the same key without overwriting each other.
- Existing `stateDir/memories/sender_*/memory.jsonl` files are read in place and require no migration.
- Both memory blocks remain untrusted data with dynamic delimiters; Agent-owned does not mean trusted instruction.

## Cross-sender threat model

Agent memory is shared state. An authorized sender can try to prompt the model into storing adversarial text in `scope=agent`; that text would then be injected as untrusted memory in later turns for other senders. This expands the blast radius compared with sender memory, whose stored content is reinjected only for the same sender. The dynamic untrusted-data delimiter preserves instruction priority, but it does not prove that a model can never be confused by stored prompt injection.

This phase accepts that residual risk under three explicit boundaries:

- ingress authorization still controls who can trigger the Agent Loop;
- the model-facing tool contract says a sender message is input, not authority: the Agent writes or forgets shared memory only when it independently adopts or retracts the item as its own;
- Agent memory remains untrusted data, must not contain sensitive data, and does not become a system instruction merely because the Agent stored it.

Agent memory is owned by the Agent, not by the sender who happened to trigger the turn. Consequently, writer identity must not become an ownership ACL: doing so would recreate per-sender memory under another name and would break the intended third-party `@owner` / `@agent` flow. Provenance is still valuable for audit and incident analysis, and is deferred together with atomic persistence and multi-process coordination to hardening follow-up #161.

Deployments that do not trust all ingress-authorized participants to influence one shared assistant need a stricter policy layer (for example owner-approved skills or write approval). That is a different product policy, not something this storage primitive can infer from `sender_id`.

## Test contract

- Scope normalization covers missing, sender, agent, unknown, and non-string input.
- Path resolution covers sender/Agent separation and missing identities.
- Update and forget affect only the selected scope.
- Different senders share one Agent memory while retaining isolated sender memories.
- Different Agent IDs do not share Agent memory.
- Runtime instruction contains correctly labelled blocks and never substitutes owner memory for an owner-addressed third-party turn.
- Production ADK tests use real tool execution, not hand-built records.
- Live DeepSeek tests prove one sender-memory choice and one Agent-memory choice with no live-gated skip.
- Model-visible update/forget descriptions explicitly pin the cross-sender shared-state boundary and independent Agent judgment.
