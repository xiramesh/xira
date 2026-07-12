# Explicit Intentional Silence Implementation Plan

## Step 1: Pin the outcome contract

- Output: failing tests for the sealed ADK tool, silence evaluator, success paths, failure masking, and accidental empty final.
- Test: focused runtime tests fail before implementation.

## Step 2: Implement the runtime-owned tool

- Output: `finish_silent` ADK tool, closed schema, fixed output, event/audit recording, and strict evaluator.
- Test: tool/evaluator contract tests reach 100% branch coverage.

## Step 3: Integrate run completion

- Output: ADK empty-final handling and Service verification accept explicit silence while preserving `notify_owner` and ordinary failure behavior.
- Test: production-path fake-model scenarios pass, including sender/Agent memory and failed tools.

## Step 4: Verify live model behavior

- Output: real DeepSeek smoke test for explicit silence with no outbound final.
- Test: live test passes with both gates enabled and no live-gated skip.

## Step 5: Deliver

- Output: full coverage/build/test/race/vet results, commit, pushed branch, PR linked to #158, and updated #124 roadmap.
- Test: CI passes on the exact pushed head.
