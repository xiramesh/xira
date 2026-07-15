const test = require("node:test");
const assert = require("node:assert/strict");

const { mountDemo } = require("./app.js");

class FakeElement {
  constructor(tagName = "div") {
    this.tagName = tagName.toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.listeners = new Map();
    this.attributes = new Map();
    this.dataset = {};
    this.className = "";
    this.textContent = "";
    this.value = "";
    this.disabled = false;
    this.hidden = false;
    this.focused = false;
    this.scrollTop = 0;
    this.scrollHeight = 100;
  }

  append(...nodes) {
    for (const node of nodes) {
      node.parentNode = this;
      this.children.push(node);
    }
  }

  prepend(node) {
    node.parentNode = this;
    this.children.unshift(node);
  }

  replaceChildren(...nodes) {
    for (const child of this.children) child.parentNode = null;
    this.children = [];
    this.append(...nodes);
  }

  remove() {
    if (!this.parentNode) return;
    this.parentNode.children = this.parentNode.children.filter((child) => child !== this);
    this.parentNode = null;
  }

  insertBefore(node, reference) {
    const index = this.children.indexOf(reference);
    node.parentNode = this;
    if (index < 0) this.children.push(node);
    else this.children.splice(index, 0, node);
  }

  get lastElementChild() {
    return this.children.at(-1) || null;
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  dispatch(type, detail = {}) {
    const event = {
      type,
      defaultPrevented: false,
      preventDefault() {
        this.defaultPrevented = true;
      },
      ...detail,
    };
    for (const listener of this.listeners.get(type) || []) listener(event);
    return event;
  }

  click() {
    return this.dispatch("click");
  }

  requestSubmit() {
    return this.dispatch("submit");
  }

  focus() {
    this.focused = true;
  }

  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }

  querySelectorAll(selector) {
    const matches = [];
    const visit = (node) => {
      for (const child of node.children) {
        const classes = child.className.split(/\s+/).filter(Boolean);
        if (
          (selector.startsWith(".") && classes.includes(selector.slice(1))) ||
          (!selector.startsWith(".") && child.tagName.toLowerCase() === selector.toLowerCase())
        ) {
          matches.push(child);
        }
        visit(child);
      }
    };
    visit(this);
    return matches;
  }
}

class FakeDocument {
  constructor() {
    this.body = new FakeElement("body");
    this.elements = new Map();
  }

  add(id, tagName = "div") {
    const element = new FakeElement(tagName);
    this.elements.set(id, element);
    return element;
  }

  getElementById(id) {
    return this.elements.get(id) || null;
  }

  createElement(tagName) {
    return new FakeElement(tagName);
  }
}

class FakeSocketClient {
  static instances = [];
  static connectError = null;
  static sendError = null;
  static humanError = null;

  constructor(callbacks) {
    this.callbacks = callbacks;
    this.connectCalls = [];
    this.sentMessages = [];
    this.humanResponses = [];
    this.disconnectCalls = 0;
    FakeSocketClient.instances.push(this);
  }

  connect(options) {
    if (FakeSocketClient.connectError) throw FakeSocketClient.connectError;
    this.connectCalls.push(options);
  }

  sendMessage(message) {
    if (FakeSocketClient.sendError) throw FakeSocketClient.sendError;
    this.sentMessages.push(message);
    return `msg-${this.sentMessages.length}`;
  }

  sendHumanResponse(response) {
    if (FakeSocketClient.humanError) throw FakeSocketClient.humanError;
    this.humanResponses.push(response);
  }

  disconnect() {
    this.disconnectCalls += 1;
  }
}

function createHarness() {
  FakeSocketClient.instances = [];
  FakeSocketClient.connectError = null;
  FakeSocketClient.sendError = null;
  FakeSocketClient.humanError = null;

  const document = new FakeDocument();
  const elements = {
    connectionForm: document.add("connection-form", "form"),
    endpoint: document.add("endpoint", "input"),
    entrypointId: document.add("entrypoint-id", "input"),
    chatId: document.add("chat-id", "input"),
    senderId: document.add("sender-id", "input"),
    connectButton: document.add("connect-button", "button"),
    disconnectButton: document.add("disconnect-button", "button"),
    connectionLabel: document.add("connection-label"),
    connectionDetail: document.add("connection-detail"),
    messageForm: document.add("message-form", "form"),
    messageInput: document.add("message-input", "textarea"),
    sendButton: document.add("send-button", "button"),
    transcript: document.add("transcript"),
    eventLog: document.add("event-log"),
    clearEvents: document.add("clear-events", "button"),
    requestCounter: document.add("request-counter"),
    lastFrame: document.add("last-frame"),
    humanRegion: document.add("human-action-region"),
  };
  elements.endpoint.value = "ws://localhost:8089/socket";
  elements.entrypointId.value = "websocket-default";
  elements.chatId.value = "demo-chat";
  elements.senderId.value = "browser-user";
  const placeholder = new FakeElement("div");
  placeholder.className = "event-empty";
  elements.eventLog.append(placeholder);

  const windowListeners = new Map();
  global.window = {
    addEventListener(type, listener) {
      windowListeners.set(type, listener);
    },
  };
  mountDemo(document, { XiraSocketClient: FakeSocketClient });
  return { document, elements, windowListeners };
}

test("mountDemo drives connection, message, frame, HITL, and cleanup interactions", (t) => {
  t.after(() => delete global.window);
  const { elements, windowListeners } = createHarness();

  assert.equal(elements.disconnectButton.disabled, true);
  assert.equal(elements.messageInput.disabled, true);
  elements.connectionForm.requestSubmit();
  const client = FakeSocketClient.instances[0];
  assert.deepEqual(client.connectCalls[0], {
    endpoint: "ws://localhost:8089/socket",
    entrypointId: "websocket-default",
    chatId: "demo-chat",
    senderId: "browser-user",
  });

  client.callbacks.onState("connecting", {});
  assert.equal(elements.connectButton.disabled, true);
  client.callbacks.onState("connected", {});
  assert.equal(elements.connectionLabel.textContent, "链路在线");
  assert.equal(elements.messageInput.focused, true);
  assert.equal(elements.eventLog.querySelector(".event-empty"), null);

  elements.messageInput.value = "你好";
  elements.messageForm.requestSubmit();
  assert.deepEqual(client.sentMessages, ["你好"]);
  assert.equal(elements.requestCounter.textContent, "1 REQUEST");
  assert.equal(elements.transcript.querySelectorAll(".message-user").length, 1);
  assert.equal(
    elements.transcript.querySelector(".message-request-state").textContent,
    "sent",
  );

  client.callbacks.onFrame({
    type: "ack",
    request_id: "msg-1",
    data: { status: "accepted" },
  });
  assert.equal(
    elements.transcript.querySelector(".message-request-state").textContent,
    "accepted",
  );

  client.callbacks.onFrame({
    type: "interrupt",
    request_id: "msg-1",
    run_id: "run-1",
    data: {
      status: "waiting_human",
      human_requests: [{
        id: "hr-1",
        kind: "freeform",
        question: "什么时候执行？",
        correlation_token: "token-1",
        run_id: "run-1",
      }],
    },
  });
  const answer = elements.humanRegion.querySelector(".human-answer");
  answer.value = "明天";
  elements.humanRegion.querySelectorAll("button")[0].click();
  assert.deepEqual(client.humanResponses[0], {
    requestId: "hr-1",
    correlationToken: "token-1",
    action: "answer",
    answer: "明天",
  });
  assert.equal(answer.disabled, true);

  client.callbacks.onFrame({
    type: "interrupt",
    request_id: "msg-1",
    data: {
      human_requests: [{
        id: "hr-2",
        kind: "approval",
        question: "允许吗？",
        correlation_token: "token-2",
      }],
    },
  });
  elements.humanRegion.querySelectorAll("button")[0].click();
  assert.equal(client.humanResponses[1].action, "approve");

  client.callbacks.onFrame({
    type: "response",
    request_id: "msg-1",
    data: { status: "completed", agent_id: "xira-assistant", final_response: "完成" },
  });
  assert.equal(elements.transcript.querySelectorAll(".message-assistant").length, 1);
  assert.equal(elements.humanRegion.hidden, true);

  client.callbacks.onFrame({
    type: "error",
    request_id: "msg-1",
    data: { code: "failed", message: "失败" },
  });
  client.callbacks.onProtocolError(new Error("bad json"));
  assert.equal(elements.transcript.querySelectorAll(".message-error").length, 2);
  assert.match(elements.lastFrame.textContent, /LAST FRAME — ERROR/);

  elements.messageInput.value = "第二条";
  elements.messageInput.dispatch("keydown", { key: "Enter", shiftKey: false });
  assert.equal(elements.requestCounter.textContent, "2 REQUESTS");
  elements.clearEvents.click();
  assert.equal(elements.eventLog.querySelector(".event-empty").textContent, "");
  elements.disconnectButton.click();
  windowListeners.get("beforeunload")();
  assert.equal(client.disconnectCalls, 2);
});

test("mountDemo keeps connection and send failures visible", (t) => {
  t.after(() => delete global.window);
  const { elements } = createHarness();

  FakeSocketClient.connectError = new Error("bad endpoint");
  elements.connectionForm.requestSubmit();
  assert.equal(elements.connectionLabel.textContent, "链路异常");
  assert.equal(elements.transcript.querySelectorAll(".message-error").length, 1);

  FakeSocketClient.connectError = null;
  elements.connectionForm.requestSubmit();
  const client = FakeSocketClient.instances.at(-1);
  client.callbacks.onState("connected", {});
  FakeSocketClient.sendError = new Error("not writable");
  elements.messageInput.value = "发送失败";
  elements.messageForm.requestSubmit();
  assert.equal(elements.transcript.querySelectorAll(".message-error").length, 2);

  FakeSocketClient.sendError = null;
  client.callbacks.onFrame({
    type: "interrupt",
    request_id: "msg-x",
    data: {
      human_requests: [{
        id: "hr-x",
        kind: "approval",
        correlation_token: "token-x",
      }],
    },
  });
  FakeSocketClient.humanError = new Error("stale request");
  elements.humanRegion.querySelectorAll("button")[0].click();
  assert.equal(elements.transcript.querySelectorAll(".message-error").length, 3);

  elements.messageInput.value = "保留换行";
  const shifted = elements.messageInput.dispatch("keydown", { key: "Enter", shiftKey: true });
  assert.equal(shifted.defaultPrevented, false);
});

test("mountDemo rejects a missing protocol", () => {
  assert.throws(() => mountDemo(null, null), /protocol module is unavailable/);
});
