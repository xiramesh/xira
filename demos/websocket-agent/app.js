(function attachXiraWebSocketDemo(root, factory) {
  const api = factory();
  if (typeof module === "object" && module.exports) {
    module.exports = api;
    return;
  }
  root.XiraWebSocketDemo = api;
  const boot = function boot() {
    api.mountDemo(root.document, root.XiraWebSocketProtocol);
  };
  if (root.document.readyState === "loading") {
    root.document.addEventListener("DOMContentLoaded", boot, { once: true });
  } else {
    boot();
  }
})(typeof globalThis !== "undefined" ? globalThis : this, function createDemoApp() {
  "use strict";

  function safeString(value, fallback) {
    const normalized = String(value == null ? "" : value).trim();
    return normalized || fallback || "";
  }

  function pretty(value) {
    try {
      return JSON.stringify(value, null, 2);
    } catch (_error) {
      return String(value);
    }
  }

  function frameToPresentation(frame) {
    const value = frame && typeof frame === "object" ? frame : {};
    const type = safeString(value.type, "unknown");
    const data = value.data && typeof value.data === "object" ? value.data : {};
    const base = {
      kind: "event",
      tone: "neutral",
      title: type,
      body: pretty(data),
      requestId: safeString(value.request_id),
      runId: safeString(value.run_id || data.run_id),
      humanRequests: [],
    };

    if (type === "response") {
      return {
        ...base,
        kind: "assistant",
        tone: data.status === "completed" ? "success" : "neutral",
        title: `${safeString(data.agent_id, "默认 Agent")} · ${safeString(data.status, "finished")}`,
        body: safeString(data.final_response, "Agent 已完成，但没有返回文本。"),
      };
    }
    if (type === "event") {
      const event = data.event && typeof data.event === "object" ? data.event : {};
      const payload = event.payload && typeof event.payload === "object" ? event.payload : {};
      return {
        ...base,
        title: safeString(event.kind, "runtime.event"),
        body: safeString(
          event.message || payload.message || payload.tool_name || payload.status,
          Object.keys(payload).length ? pretty(payload) : "Runtime event",
        ),
      };
    }
    if (type === "interrupt") {
      const requests = Array.isArray(data.human_requests) ? data.human_requests : [];
      return {
        ...base,
        kind: "interrupt",
        tone: "warning",
        title: `需要人工响应 · ${safeString(data.status, "waiting_human")}`,
        body: safeString(requests[0] && requests[0].question, data.reason || "Agent 正在等待操作。"),
        humanRequests: requests,
      };
    }
    if (type === "error") {
      return {
        ...base,
        kind: "error",
        tone: "danger",
        title: `${safeString(data.code, "websocket_error")}${data.retryable ? " · 可重试" : ""}`,
        body: safeString(data.message, "WebSocket 请求失败。"),
      };
    }
    if (type === "ack") {
      const status = safeString(data.status, "accepted");
      return {
        ...base,
        tone: status === "accepted" || status === "human_response_accepted" ? "success" : "neutral",
        title: `ACK · ${status}`,
        body: safeString(data.reason || data.message_id, "服务端已接收请求。"),
      };
    }
    if (type === "ready") {
      return {
        ...base,
        tone: "online",
        title: "CHANNEL READY",
        body: `entrypoint · ${safeString(data.entrypoint_id, "websocket-default")}`,
      };
    }
    if (type === "pong") {
      return { ...base, title: "PONG", body: "心跳响应", tone: "online" };
    }
    return base;
  }

  function stateToPresentation(state, detail) {
    const info = detail || {};
    switch (state) {
      case "connecting":
        return { label: "正在接入", detail: "等待 WebSocket 握手", tone: "working" };
      case "connected":
        return { label: "链路在线", detail: "默认 Agent 路由已就绪", tone: "online" };
      case "error":
        return {
          label: "链路异常",
          detail: "无法握手；确认 Xira 已在填写地址启动",
          tone: "danger",
        };
      case "disconnected":
        return {
          label: "链路断开",
          detail: `连接已关闭${info.code ? ` · ${info.code}` : ""}`,
          tone: "offline",
        };
      default:
        return { label: "链路离线", detail: "等待手工连接", tone: "offline" };
    }
  }

  function mountDemo(document, protocol) {
    if (!document || !protocol || !protocol.XiraSocketClient) {
      throw new Error("Xira WebSocket protocol module is unavailable");
    }

    const elements = {
      body: document.body,
      connectionForm: document.getElementById("connection-form"),
      endpoint: document.getElementById("endpoint"),
      entrypointId: document.getElementById("entrypoint-id"),
      chatId: document.getElementById("chat-id"),
      senderId: document.getElementById("sender-id"),
      connectButton: document.getElementById("connect-button"),
      disconnectButton: document.getElementById("disconnect-button"),
      connectionLabel: document.getElementById("connection-label"),
      connectionDetail: document.getElementById("connection-detail"),
      messageForm: document.getElementById("message-form"),
      messageInput: document.getElementById("message-input"),
      sendButton: document.getElementById("send-button"),
      transcript: document.getElementById("transcript"),
      eventLog: document.getElementById("event-log"),
      clearEvents: document.getElementById("clear-events"),
      requestCounter: document.getElementById("request-counter"),
      lastFrame: document.getElementById("last-frame"),
      humanRegion: document.getElementById("human-action-region"),
    };
    let requestCount = 0;
    let client = null;
    const requestMessages = new Map();

    function setControls(state) {
      const connecting = state === "connecting";
      const connected = state === "connected";
      elements.body.dataset.connectionState = state;
      for (const input of [elements.endpoint, elements.entrypointId, elements.chatId, elements.senderId]) {
        input.disabled = connecting || connected;
      }
      elements.connectButton.disabled = connecting || connected;
      elements.disconnectButton.disabled = !connecting && !connected;
      elements.messageInput.disabled = !connected;
      elements.sendButton.disabled = !connected;
      if (connected) elements.messageInput.focus();
    }

    function setConnectionState(state, detail) {
      const presentation = stateToPresentation(state, detail);
      elements.connectionLabel.textContent = presentation.label;
      elements.connectionDetail.textContent = presentation.detail;
      setControls(state);
      addEventEntry({
        kind: "event",
        tone: presentation.tone,
        title: `CONNECTION · ${state.toUpperCase()}`,
        body: presentation.detail,
        requestId: "",
        runId: "",
        humanRequests: [],
      });
    }

    function nowLabel() {
      return new Intl.DateTimeFormat("zh-CN", {
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
        hour12: false,
      }).format(new Date());
    }

    function clearEventPlaceholder() {
      const empty = elements.eventLog.querySelector(".event-empty");
      if (empty) empty.remove();
    }

    function addEventEntry(presentation) {
      clearEventPlaceholder();
      const entry = document.createElement("article");
      entry.className = "event-entry";
      entry.dataset.tone = presentation.tone;

      const meta = document.createElement("div");
      meta.className = "event-meta";
      const request = document.createElement("span");
      request.textContent = presentation.requestId ? `REQ ${presentation.requestId}` : "SYSTEM";
      const time = document.createElement("time");
      time.textContent = nowLabel();
      meta.append(request, time);

      const title = document.createElement("strong");
      title.textContent = presentation.title;
      const body = document.createElement("p");
      body.textContent = presentation.body;
      entry.append(meta, title, body);
      elements.eventLog.prepend(entry);
    }

    function addMessage(role, title, body, requestId, tone) {
      const article = document.createElement("article");
      article.className = `message message-${role}`;
      if (tone) article.dataset.tone = tone;
      if (requestId) article.dataset.requestId = requestId;

      const meta = document.createElement("div");
      meta.className = "message-meta";
      const author = document.createElement("span");
      author.textContent = title;
      const time = document.createElement("time");
      time.textContent = nowLabel();
      meta.append(author, time);

      const text = document.createElement("p");
      text.textContent = body;
      article.append(meta, text);
      elements.transcript.append(article);
      elements.transcript.scrollTop = elements.transcript.scrollHeight;
      return article;
    }

    function markRequest(requestId, status) {
      const article = requestMessages.get(requestId);
      if (!article) return;
      const meta = article.querySelector(".message-meta");
      let marker = article.querySelector(".message-request-state");
      if (!marker) {
        marker = document.createElement("span");
        marker.className = "message-request-state";
        meta.insertBefore(marker, meta.lastElementChild);
      }
      marker.textContent = status;
    }

    function renderHumanRequest(request) {
      elements.humanRegion.replaceChildren();
      elements.humanRegion.hidden = false;
      const heading = document.createElement("h3");
      heading.textContent = request.kind === "freeform" ? "Agent 需要你的回答" : "Agent 请求确认";
      const question = document.createElement("p");
      question.textContent = safeString(request.question, "请确认下一步操作。 ");
      const actions = document.createElement("div");
      actions.className = "human-actions";

      const answerInput = document.createElement("textarea");
      answerInput.className = "human-answer";
      answerInput.rows = 2;
      answerInput.placeholder = "输入回答…";
      if (request.kind === "freeform") elements.humanRegion.append(heading, question, answerInput);
      else elements.humanRegion.append(heading, question);

      const actionNames = request.kind === "freeform"
        ? [["answer", "提交回答", true], ["cancel", "取消", false]]
        : [["approve", "批准", true], ["deny", "拒绝", false], ["cancel", "取消", false]];
      for (const [action, label, primary] of actionNames) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = `human-button${primary ? " human-button-primary" : ""}`;
        button.textContent = label;
        button.addEventListener("click", () => {
          try {
            client.sendHumanResponse({
              requestId: request.id,
              correlationToken: request.correlation_token,
              action,
              answer: action === "answer" ? answerInput.value : "",
            });
            for (const child of actions.querySelectorAll("button")) child.disabled = true;
            if (request.kind === "freeform") answerInput.disabled = true;
            addEventEntry({
              kind: "event",
              tone: "working",
              title: `HUMAN RESPONSE · ${action}`,
              body: `request · ${request.id}`,
              requestId: request.id,
              runId: request.run_id || "",
              humanRequests: [],
            });
          } catch (error) {
            addMessage("error", "HUMAN RESPONSE ERROR", error.message, request.id, "danger");
          }
        });
        actions.append(button);
      }
      elements.humanRegion.append(actions);
    }

    function handleFrame(frame) {
      const presentation = frameToPresentation(frame);
      elements.lastFrame.textContent = `LAST FRAME — ${safeString(frame.type, "UNKNOWN").toUpperCase()} · ${nowLabel()}`;
      addEventEntry(presentation);

      if (frame.type === "ack") {
        markRequest(frame.request_id, safeString(frame.data && frame.data.status, "ack"));
      } else if (presentation.kind === "assistant") {
        markRequest(presentation.requestId, "completed");
        addMessage("assistant", presentation.title, presentation.body, presentation.requestId, presentation.tone);
        elements.humanRegion.hidden = true;
        elements.humanRegion.replaceChildren();
      } else if (presentation.kind === "interrupt") {
        markRequest(presentation.requestId, "waiting_human");
        addMessage("system", presentation.title, presentation.body, presentation.requestId, presentation.tone);
        if (presentation.humanRequests[0]) renderHumanRequest(presentation.humanRequests[0]);
      } else if (presentation.kind === "error") {
        markRequest(presentation.requestId, "error");
        addMessage("error", presentation.title, presentation.body, presentation.requestId, presentation.tone);
      }
    }

    function createClient() {
      return new protocol.XiraSocketClient({
        onState: setConnectionState,
        onFrame: handleFrame,
        onProtocolError: (error) => {
          addMessage("error", "PROTOCOL ERROR", error.message, "", "danger");
          addEventEntry({
            kind: "error",
            tone: "danger",
            title: "INVALID FRAME",
            body: error.message,
            requestId: "",
            runId: "",
            humanRequests: [],
          });
        },
      });
    }

    elements.connectionForm.addEventListener("submit", (event) => {
      event.preventDefault();
      try {
        client = createClient();
        client.connect({
          endpoint: elements.endpoint.value,
          entrypointId: elements.entrypointId.value,
          chatId: elements.chatId.value,
          senderId: elements.senderId.value,
        });
      } catch (error) {
        setConnectionState("error", {});
        addMessage("error", "CONNECTION ERROR", error.message, "", "danger");
      }
    });

    elements.disconnectButton.addEventListener("click", () => {
      if (client) client.disconnect();
    });

    elements.messageForm.addEventListener("submit", (event) => {
      event.preventDefault();
      const message = elements.messageInput.value.trim();
      if (!message || !client) return;
      try {
        const requestId = client.sendMessage(message);
        const article = addMessage("user", "YOU · DEFAULT ROUTE", message, requestId, "neutral");
        requestMessages.set(requestId, article);
        markRequest(requestId, "sent");
        requestCount += 1;
        elements.requestCounter.textContent = `${requestCount} REQUEST${requestCount === 1 ? "" : "S"}`;
        elements.messageInput.value = "";
        elements.messageInput.focus();
      } catch (error) {
        addMessage("error", "SEND ERROR", error.message, "", "danger");
      }
    });

    elements.messageInput.addEventListener("keydown", (event) => {
      if (event.key === "Enter" && !event.shiftKey) {
        event.preventDefault();
        elements.messageForm.requestSubmit();
      }
    });

    elements.clearEvents.addEventListener("click", () => {
      elements.eventLog.replaceChildren();
      const empty = document.createElement("div");
      empty.className = "event-empty";
      const glyph = document.createElement("span");
      glyph.setAttribute("aria-hidden", "true");
      glyph.textContent = "⌁";
      const label = document.createElement("p");
      label.textContent = "等待 WebSocket 帧";
      empty.append(glyph, label);
      elements.eventLog.append(empty);
    });

    if (typeof window !== "undefined") {
      window.addEventListener("beforeunload", () => {
        if (client) client.disconnect();
      });
    }
    setControls("disconnected");
  }

  return Object.freeze({ frameToPresentation, mountDemo, stateToPresentation });
});
