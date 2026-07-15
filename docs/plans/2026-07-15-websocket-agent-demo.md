# WebSocket Default Agent Demo Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build a dependency-free static browser demo that exercises Xira's WebSocket channel through an entrypoint's default Agent.

**Architecture:** Put a directly-openable HTML/CSS/JavaScript demo under `demos/websocket-agent/`. Keep protocol construction and WebSocket lifecycle in a browser/CommonJS-compatible module so Node's built-in test runner can verify behavior without adding packages; keep DOM rendering in a thin UI adapter.

**Tech Stack:** HTML5, CSS, browser WebSocket API, ES5-compatible UMD module, Node `node:test`, Playwright for visual verification.

---

### Task 1: Pin the WebSocket protocol client contract

**Files:**
- Create: `demos/websocket-agent/protocol.test.cjs`
- Create: `demos/websocket-agent/protocol.js`

**Steps:**

1. Write failing tests for endpoint normalization, `hello`, `message`, and `ping` frames.
2. Require the `message` frame to omit `agent_id`, carry stable chat/sender/message context, and use its frame ID as correlation.
3. Add a fake WebSocket and failing tests for open/message/error/close callbacks plus heartbeat cleanup.
4. Run `node --test demos/websocket-agent/protocol.test.cjs` and confirm RED because `protocol.js` does not exist.
5. Implement the smallest UMD-compatible protocol client and rerun the tests to GREEN.

### Task 2: Build the directly-openable page

**Files:**
- Create: `demos/websocket-agent/index.html`
- Create: `demos/websocket-agent/styles.css`
- Create: `demos/websocket-agent/console.css`
- Create: `demos/websocket-agent/app.js`

**Steps:**

1. Build the semantic page shell with connection form, transcript, composer, event rail, status live region, and HITL action region.
2. Bind form state to the protocol client; omit any Agent selector and display that routing uses the entrypoint default.
3. Render inbound frames by type using DOM nodes and `textContent` only.
4. Disable invalid actions according to connection state and expose manual reconnect after close/error.
5. Add responsive layout for laptop and narrow browser widths.

### Task 3: Document setup and expected protocol

**Files:**
- Create: `demos/websocket-agent/README.md`

**Steps:**

1. Document the required enabled `websocket-default` entrypoint and default Agent configuration.
2. Document `xira serve`, direct `index.html` opening, frame handling, and common errors.
3. State that the demo is for local/trusted-network testing and does not add WebSocket authentication.

### Task 4: Verify behavior and appearance

**Files:**
- Modify only files above if verification finds defects.

**Steps:**

1. Run `node --test demos/websocket-agent/protocol.test.cjs` and require all tests to pass.
2. Run `git diff --check`.
3. Run `go build ./...` and `go test ./... -count=1` from `apps/xira` to prove the demo does not regress Go code.
4. Open `demos/websocket-agent/index.html` in Chromium using the Playwright skill; inspect desktop and narrow layouts plus the disconnected/error state.
5. Fix every visual or interaction defect found, rerun tests, and capture the verified final page.
6. Commit the completed demo and verify `git show --stat HEAD` includes every intended file.
