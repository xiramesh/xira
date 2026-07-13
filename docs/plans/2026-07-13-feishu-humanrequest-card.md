# Feishu HumanRequest Card Implementation Plan

**Goal:** Complete #165 with native Feishu HumanRequest cards for Agent Run and
Flow, current sender and owner, plus exact text fallback.

### Task 1: Freeze the platform contract with tests

- Add card JSON tests for approval, option, freeform-text, and terminal states.
- Add callback tests for exact fields, real Feishu identity shape, wrong input,
  duplicate input, and no card replacement on rejection.
- Add delivery tests for real message ID and card-to-text fallback.
- Run focused tests and retain the expected RED result before implementation.

### Task 2: Add async exact runtime resolution

- Add a narrow async resolver interface for native channel callbacks.
- Refactor exact validation from synchronous resume without changing existing
  callers.
- Accept a trusted typed identity set and select only the persisted responder
  type.
- Test atomic rejection, idempotency, and eventual durable resume state.

### Task 3: Implement the Feishu adapter

- Register `card.action.trigger` on the existing dispatcher.
- Implement card render, route validation, receipt-returning send, text
  fallback, callback parsing, safe toast/card response, and async resume.
- Inject exact and text resolvers from the channel manager.
- Remove implicit first-pending Feishu answer handling.

### Task 4: Verify and deliver

- Run focused coverage and require contract branches at 100% and package
  coverage at least 85%.
- Run gofmt, diff check, all-workspace build/test/race, and `task live-test`
  with no live-gated skips.
- Commit in reviewable stages, push, open a PR closing #165, and update #155.
