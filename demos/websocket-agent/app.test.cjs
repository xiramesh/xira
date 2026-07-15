const test = require("node:test");
const assert = require("node:assert/strict");

const { frameToPresentation, stateToPresentation } = require("./app.js");

test("response frames become correlated assistant messages", () => {
  assert.deepEqual(
    frameToPresentation({
      type: "response",
      request_id: "msg-1",
      run_id: "run-1",
      data: {
        status: "completed",
        agent_id: "xira-assistant",
        final_response: "这是默认 Agent 的回复。",
      },
    }),
    {
      kind: "assistant",
      tone: "success",
      title: "xira-assistant · completed",
      body: "这是默认 Agent 的回复。",
      requestId: "msg-1",
      runId: "run-1",
      humanRequests: [],
    },
  );
});

test("runtime events retain their kind and useful message", () => {
  const presentation = frameToPresentation({
    type: "event",
    request_id: "msg-1",
    run_id: "run-1",
    data: {
      event: {
        kind: "tool.called",
        message: "search_file",
        payload: { tool_name: "search_file" },
      },
    },
  });

  assert.equal(presentation.kind, "event");
  assert.equal(presentation.title, "tool.called");
  assert.equal(presentation.body, "search_file");
  assert.equal(presentation.requestId, "msg-1");
});

test("interrupt frames expose HumanRequests for structured actions", () => {
  const request = {
    id: "hr-1",
    kind: "approval",
    question: "允许执行吗？",
    correlation_token: "opaque-token",
  };
  const presentation = frameToPresentation({
    type: "interrupt",
    request_id: "msg-1",
    run_id: "run-1",
    data: { status: "waiting_human", human_requests: [request] },
  });

  assert.equal(presentation.kind, "interrupt");
  assert.equal(presentation.tone, "warning");
  assert.equal(presentation.body, "允许执行吗？");
  assert.deepEqual(presentation.humanRequests, [request]);
});

test("error and unknown frames stay visible instead of disappearing", () => {
  const error = frameToPresentation({
    type: "error",
    request_id: "msg-1",
    data: { code: "run_failed", message: "model unavailable", retryable: true },
  });
  assert.equal(error.kind, "error");
  assert.equal(error.title, "run_failed · 可重试");
  assert.equal(error.body, "model unavailable");

  const unknown = frameToPresentation({ type: "future_frame", data: { value: 1 } });
  assert.equal(unknown.kind, "event");
  assert.equal(unknown.title, "future_frame");
  assert.match(unknown.body, /"value": 1/);
});

test("connection states map to clear operator-facing labels", () => {
  assert.deepEqual(stateToPresentation("connecting", {}), {
    label: "正在接入",
    detail: "等待 WebSocket 握手",
    tone: "working",
  });
  assert.deepEqual(stateToPresentation("connected", {}), {
    label: "链路在线",
    detail: "默认 Agent 路由已就绪",
    tone: "online",
  });
  assert.deepEqual(stateToPresentation("disconnected", { code: 1006 }), {
    label: "链路断开",
    detail: "连接已关闭 · 1006",
    tone: "offline",
  });
});
