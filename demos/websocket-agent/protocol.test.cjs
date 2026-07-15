const test = require("node:test");
const assert = require("node:assert/strict");

const {
  DEFAULT_ENDPOINT,
  XiraSocketClient,
  buildHelloFrame,
  buildHumanResponseFrame,
  buildMessageFrame,
  buildPingFrame,
  normalizeEndpoint,
} = require("./protocol.js");

test("normalizeEndpoint supplies the channel path without changing custom paths", () => {
  assert.equal(normalizeEndpoint(""), DEFAULT_ENDPOINT);
  assert.equal(
    normalizeEndpoint("http://localhost:8089"),
    "ws://localhost:8089/api/v1/channels/websocket/messages",
  );
  assert.equal(
    normalizeEndpoint("https://xira.example.test/"),
    "wss://xira.example.test/api/v1/channels/websocket/messages",
  );
  assert.equal(
    normalizeEndpoint("wss://xira.example.test/custom/socket"),
    "wss://xira.example.test/custom/socket",
  );
  assert.throws(() => normalizeEndpoint("not a url"), /invalid/i);
  assert.throws(() => normalizeEndpoint("ftp://xira.example.test"), /must use ws/i);
});

test("frame builders preserve correlation and leave agent routing to the entrypoint", () => {
  assert.deepEqual(buildHelloFrame("hello-1", "websocket-default"), {
    type: "hello",
    id: "hello-1",
    data: {
      client_id: "xira-websocket-agent-demo",
      entrypoint_id: "websocket-default",
    },
  });

  const message = buildMessageFrame({
    id: "msg-1",
    entrypointId: "websocket-default",
    chatId: "demo-chat",
    senderId: "browser-user",
    message: "你好",
  });
  assert.equal(message.type, "message");
  assert.equal(message.id, "msg-1");
  assert.equal(message.data.message, "你好");
  assert.equal(message.data.entrypoint_id, "websocket-default");
  assert.equal(message.data.context.channel, "websocket");
  assert.equal(message.data.context.chat_id, "demo-chat");
  assert.equal(message.data.context.sender_id, "browser-user");
  assert.equal(message.data.context.message_id, "msg-1");
  assert.equal(Object.hasOwn(message.data, "agent_id"), false);

  assert.deepEqual(buildPingFrame("ping-1"), { type: "ping", id: "ping-1" });
  assert.throws(() => buildHelloFrame("", "websocket-default"), /hello id is required/);
});

test("human response frames carry only request correlation and the selected action", () => {
  assert.deepEqual(
    buildHumanResponseFrame({
      id: "human-1",
      requestId: "hr-1",
      correlationToken: "opaque-token",
      action: "approve",
    }),
    {
      type: "human_response",
      id: "human-1",
      request_id: "hr-1",
      correlation_token: "opaque-token",
      action: "approve",
    },
  );

  const answer = buildHumanResponseFrame({
    id: "human-2",
    requestId: "hr-2",
    correlationToken: "opaque-token-2",
    action: "answer",
    answer: "周五下午",
  });
  assert.equal(answer.answer, "周五下午");
  assert.equal(Object.hasOwn(answer, "sender_id"), false);
});

class FakeWebSocket {
  static instances = [];

  constructor(url) {
    this.url = url;
    this.readyState = 0;
    this.sent = [];
    this.closeCalls = [];
    FakeWebSocket.instances.push(this);
  }

  send(payload) {
    this.sent.push(JSON.parse(payload));
  }

  close(code, reason) {
    this.closeCalls.push({ code, reason });
    this.readyState = 3;
    if (this.onclose) this.onclose({ code, reason, wasClean: true });
  }

  open() {
    this.readyState = 1;
    if (this.onopen) this.onopen({});
  }

  receive(frame) {
    if (this.onmessage) {
      this.onmessage({ data: typeof frame === "string" ? frame : JSON.stringify(frame) });
    }
  }

  fail() {
    if (this.onerror) this.onerror({ type: "error" });
  }

  remoteClose(code = 1006, reason = "network lost") {
    this.readyState = 3;
    if (this.onclose) this.onclose({ code, reason, wasClean: false });
  }
}

function createHarness() {
  FakeWebSocket.instances = [];
  const states = [];
  const frames = [];
  const protocolErrors = [];
  let intervalCallback;
  let intervalDelay;
  let clearedTimer;
  let sequence = 0;

  const client = new XiraSocketClient({
    WebSocketImpl: FakeWebSocket,
    makeId: (prefix) => `${prefix}-${++sequence}`,
    heartbeatMs: 20_000,
    setIntervalImpl: (callback, delay) => {
      intervalCallback = callback;
      intervalDelay = delay;
      return 77;
    },
    clearIntervalImpl: (timer) => {
      clearedTimer = timer;
    },
    onState: (state, detail) => states.push({ state, detail }),
    onFrame: (frame) => frames.push(frame),
    onProtocolError: (error) => protocolErrors.push(error),
  });

  return {
    client,
    frames,
    protocolErrors,
    states,
    interval: () => ({ callback: intervalCallback, delay: intervalDelay }),
    clearedTimer: () => clearedTimer,
  };
}

test("XiraSocketClient opens with hello, sends default-agent messages, and correlates frames", () => {
  const harness = createHarness();
  harness.client.connect({
    endpoint: "http://localhost:8089",
    entrypointId: "websocket-default",
    chatId: "demo-chat",
    senderId: "browser-user",
  });

  const socket = FakeWebSocket.instances[0];
  assert.equal(
    socket.url,
    "ws://localhost:8089/api/v1/channels/websocket/messages",
  );
  assert.equal(harness.states[0].state, "connecting");

  socket.open();
  assert.equal(harness.states.at(-1).state, "connected");
  assert.equal(socket.sent[0].type, "hello");
  assert.equal(socket.sent[0].data.entrypoint_id, "websocket-default");
  assert.equal(harness.interval().delay, 20_000);

  const requestID = harness.client.sendMessage("测试默认 Agent");
  assert.equal(requestID, "msg-2");
  assert.equal(socket.sent[1].id, requestID);
  assert.equal(socket.sent[1].data.message, "测试默认 Agent");
  assert.equal(Object.hasOwn(socket.sent[1].data, "agent_id"), false);

  const humanResponseID = harness.client.sendHumanResponse({
    requestId: "hr-1",
    correlationToken: "token-1",
    action: "approve",
  });
  assert.equal(humanResponseID, "human-3");
  assert.deepEqual(socket.sent[2], {
    type: "human_response",
    id: "human-3",
    request_id: "hr-1",
    correlation_token: "token-1",
    action: "approve",
  });

  socket.receive({
    type: "response",
    request_id: requestID,
    data: { final_response: "收到" },
  });
  assert.equal(harness.frames[0].request_id, requestID);
  assert.equal(harness.frames[0].data.final_response, "收到");

  harness.interval().callback();
  assert.equal(socket.sent[3].type, "ping");
  assert.match(socket.sent[3].id, /^ping-/);
});

test("XiraSocketClient reports invalid frames and clears heartbeat when the connection closes", () => {
  const harness = createHarness();
  harness.client.connect({
    endpoint: DEFAULT_ENDPOINT,
    entrypointId: "websocket-default",
    chatId: "demo-chat",
    senderId: "browser-user",
  });
  const socket = FakeWebSocket.instances[0];
  socket.open();

  socket.receive("not-json");
  assert.equal(harness.protocolErrors.length, 1);
  assert.match(harness.protocolErrors[0].message, /JSON/);
  socket.onmessage({ data: new Uint8Array([1, 2, 3]) });
  assert.equal(harness.protocolErrors.length, 2);
  assert.match(harness.protocolErrors[1].message, /not JSON text/);

  socket.fail();
  assert.equal(harness.states.at(-1).state, "error");

  socket.remoteClose();
  assert.equal(harness.states.at(-1).state, "disconnected");
  assert.equal(harness.clearedTimer(), 77);
});

test("XiraSocketClient refuses sends before open and closes explicitly", () => {
  const harness = createHarness();
  harness.client.connect({
    endpoint: DEFAULT_ENDPOINT,
    entrypointId: "websocket-default",
    chatId: "demo-chat",
    senderId: "browser-user",
  });
  const socket = FakeWebSocket.instances[0];

  assert.throws(
    () => harness.client.connect({
      endpoint: DEFAULT_ENDPOINT,
      entrypointId: "websocket-default",
      chatId: "demo-chat",
      senderId: "browser-user",
    }),
    /already connected or connecting/i,
  );
  assert.throws(() => harness.client.sendMessage("too soon"), /not connected/i);
  socket.open();
  harness.client.disconnect();
  assert.deepEqual(socket.closeCalls, [{ code: 1000, reason: "demo disconnect" }]);
  assert.equal(harness.clearedTimer(), 77);
});

test("XiraSocketClient invokes browser timer functions without rebinding this", () => {
  FakeWebSocket.instances = [];
  let intervalReceiver = "not-called";
  let clearReceiver = "not-called";
  function browserLikeSetInterval() {
    "use strict";
    intervalReceiver = this;
    return 91;
  }
  function browserLikeClearInterval() {
    "use strict";
    clearReceiver = this;
  }
  const client = new XiraSocketClient({
    WebSocketImpl: FakeWebSocket,
    makeId: (prefix) => `${prefix}-timer`,
    setIntervalImpl: browserLikeSetInterval,
    clearIntervalImpl: browserLikeClearInterval,
  });

  client.connect({
    endpoint: DEFAULT_ENDPOINT,
    entrypointId: "websocket-default",
    chatId: "demo-chat",
    senderId: "browser-user",
  });
  const socket = FakeWebSocket.instances[0];
  socket.open();
  client.disconnect();

  assert.equal(intervalReceiver, undefined);
  assert.equal(clearReceiver, undefined);
});

test("XiraSocketClient can use browser WebSocket and ID defaults", (t) => {
  const originalWebSocket = global.WebSocket;
  global.WebSocket = FakeWebSocket;
  t.after(() => {
    global.WebSocket = originalWebSocket;
  });
  FakeWebSocket.instances = [];

  const client = new XiraSocketClient({
    setIntervalImpl: () => 1,
    clearIntervalImpl: () => {},
  });
  client.connect({
    endpoint: DEFAULT_ENDPOINT,
    entrypointId: "websocket-default",
    chatId: "demo-chat",
    senderId: "browser-user",
  });
  const socket = FakeWebSocket.instances[0];
  socket.open();

  assert.match(socket.sent[0].id, /^hello_/);
  client.disconnect();
});
