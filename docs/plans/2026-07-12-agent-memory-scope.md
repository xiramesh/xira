# Agent Memory Scope Implementation Plan

## Step 1: Pin the sealed scope and path contract

- Output: failing tools tests for default sender scope, explicit Agent scope, invalid scope, isolated paths, and missing identities.
- Test: `go test ./apps/xira/internal/tools -run 'MemoryScope|AgentMemory|UpdateMemoryTool|ForgetMemoryTool' -count=1` fails for the new cases before implementation.

## Step 2: Implement scoped memory tools

- Output: `MemoryScope`, `AgentMemoryPath`, scope-aware update/forget tools, and runtime registry wiring that binds the current Agent ID without exposing IDs to the model.
- Test: focused tools tests pass; existing sender-memory tests remain unchanged and green.

## Step 3: Inject both memory blocks

- Output: runtime instruction loads `# Sender Memory` and `# Agent Memory` independently with stable ordering and separate caps.
- Test: runtime tests prove sender-only, agent-only, both, different-agent isolation, and owner-addressed third-party behavior.

## Step 4: Exercise the production ADK path

- Output: fake-HTTP production-path tests demonstrate tool calls with each scope write the intended file; live test prompts cover both semantic choices.
- Test: focused runtime tests and `task live-test` pass with no live-gated skip.

## Step 5: Full verification and delivery

- Output: coverage report, commit, pushed branch, PR linked to #159, and updated issue comment.
- Test: `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race ./...`, coverage gates, `git diff --check`, and CI all pass.
