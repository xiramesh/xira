(function attachXiraWebSocketProtocol(root, factory) {
  const api = factory();
  if (typeof module === "object" && module.exports) {
    module.exports = api;
    return;
  }
  root.XiraWebSocketProtocol = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function createProtocol() {
  "use strict";

  const CHANNEL_PATH = "/api/v1/channels/websocket/messages";
  const DEFAULT_ENDPOINT = `ws://127.0.0.1:8089${CHANNEL_PATH}`;

  function required(value, label) {
    const normalized = String(value == null ? "" : value).trim();
    if (!normalized) {
      throw new Error(`${label} is required`);
    }
    return normalized;
  }

  function normalizeEndpoint(value) {
    const input = String(value == null ? "" : value).trim();
    if (!input) return DEFAULT_ENDPOINT;

    let parsed;
    try {
      parsed = new URL(input);
    } catch (_error) {
      throw new Error("WebSocket URL is invalid");
    }

    if (parsed.protocol === "http:") parsed.protocol = "ws:";
    if (parsed.protocol === "https:") parsed.protocol = "wss:";
    if (parsed.protocol !== "ws:" && parsed.protocol !== "wss:") {
      throw new Error("WebSocket URL must use ws, wss, http, or https");
    }
    if (!parsed.pathname || parsed.pathname === "/") {
      parsed.pathname = CHANNEL_PATH;
    }
    return parsed.toString().replace(/\/$/, "");
  }

  function buildHelloFrame(id, entrypointId) {
    return {
      type: "hello",
      id: required(id, "hello id"),
      data: {
        client_id: "xira-websocket-agent-demo",
        entrypoint_id: required(entrypointId, "entrypoint id"),
      },
    };
  }

  function buildMessageFrame(options) {
    const id = required(options && options.id, "message id");
    return {
      type: "message",
      id,
      data: {
        entrypoint_id: required(options.entrypointId, "entrypoint id"),
        message: required(options.message, "message"),
        context: {
          channel: "websocket",
          entrypoint_id: required(options.entrypointId, "entrypoint id"),
          chat_id: required(options.chatId, "chat id"),
          chat_type: "direct",
          sender_id: required(options.senderId, "sender id"),
          message_id: id,
          mentioned: true,
          raw: { client: "websocket-agent-demo" },
        },
      },
    };
  }

  function buildPingFrame(id) {
    return { type: "ping", id: required(id, "ping id") };
  }

  function buildHumanResponseFrame(options) {
    const action = required(options && options.action, "human response action");
    const frame = {
      type: "human_response",
      id: required(options.id, "human response id"),
      request_id: required(options.requestId, "human request id"),
      correlation_token: required(options.correlationToken, "correlation token"),
      action,
    };
    if (action === "answer") {
      frame.answer = required(options.answer, "answer");
    }
    return frame;
  }

  function defaultID(prefix) {
    if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
      return `${prefix}_${crypto.randomUUID()}`;
    }
    return `${prefix}_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 10)}`;
  }

  class XiraSocketClient {
    constructor(options) {
      const settings = options || {};
      this.WebSocketImpl = settings.WebSocketImpl || rootWebSocket();
      this.makeId = settings.makeId || defaultID;
      this.heartbeatMs = settings.heartbeatMs || 20_000;
      const setIntervalFunction = settings.setIntervalImpl || setInterval;
      const clearIntervalFunction = settings.clearIntervalImpl || clearInterval;
      this.setIntervalImpl = (callback, delay) => setIntervalFunction(callback, delay);
      this.clearIntervalImpl = (timer) => clearIntervalFunction(timer);
      this.onState = settings.onState || function noop() {};
      this.onFrame = settings.onFrame || function noop() {};
      this.onProtocolError = settings.onProtocolError || function noop() {};
      this.socket = null;
      this.heartbeatTimer = null;
      this.connection = null;
    }

    connect(options) {
      if (this.socket && (this.socket.readyState === 0 || this.socket.readyState === 1)) {
        throw new Error("WebSocket is already connected or connecting");
      }
      this.stopHeartbeat();
      this.connection = {
        endpoint: normalizeEndpoint(options && options.endpoint),
        entrypointId: required(options && options.entrypointId, "entrypoint id"),
        chatId: required(options && options.chatId, "chat id"),
        senderId: required(options && options.senderId, "sender id"),
      };

      const socket = new this.WebSocketImpl(this.connection.endpoint);
      this.socket = socket;
      this.onState("connecting", { endpoint: this.connection.endpoint });

      socket.onopen = () => {
        if (this.socket !== socket) return;
        this.sendFrame(buildHelloFrame(this.makeId("hello"), this.connection.entrypointId));
        this.startHeartbeat();
        this.onState("connected", { endpoint: this.connection.endpoint });
      };
      socket.onmessage = (event) => {
        if (this.socket !== socket) return;
        try {
          if (typeof event.data !== "string") {
            throw new Error("WebSocket frame is not JSON text");
          }
          this.onFrame(JSON.parse(event.data));
        } catch (error) {
          const detail = error instanceof Error ? error.message : String(error);
          this.onProtocolError(new Error(`Invalid JSON frame: ${detail}`));
        }
      };
      socket.onerror = (event) => {
        if (this.socket !== socket) return;
        this.onState("error", { event });
      };
      socket.onclose = (event) => {
        if (this.socket !== socket) return;
        this.stopHeartbeat();
        this.onState("disconnected", {
          code: event.code,
          reason: event.reason || "connection closed",
          wasClean: Boolean(event.wasClean),
        });
      };
      return socket;
    }

    isConnected() {
      return Boolean(this.socket && this.socket.readyState === 1);
    }

    sendFrame(frame) {
      if (!this.isConnected()) {
        throw new Error("WebSocket is not connected");
      }
      this.socket.send(JSON.stringify(frame));
      return frame.id;
    }

    sendMessage(message) {
      if (!this.connection) throw new Error("WebSocket is not connected");
      const id = this.makeId("msg");
      this.sendFrame(
        buildMessageFrame({
          id,
          entrypointId: this.connection.entrypointId,
          chatId: this.connection.chatId,
          senderId: this.connection.senderId,
          message,
        }),
      );
      return id;
    }

    sendHumanResponse(options) {
      const frame = buildHumanResponseFrame({
        id: this.makeId("human"),
        requestId: options && options.requestId,
        correlationToken: options && options.correlationToken,
        action: options && options.action,
        answer: options && options.answer,
      });
      this.sendFrame(frame);
      return frame.id;
    }

    startHeartbeat() {
      this.stopHeartbeat();
      this.heartbeatTimer = this.setIntervalImpl(() => {
        if (!this.isConnected()) return;
        this.sendFrame(buildPingFrame(this.makeId("ping")));
      }, this.heartbeatMs);
    }

    stopHeartbeat() {
      if (this.heartbeatTimer == null) return;
      this.clearIntervalImpl(this.heartbeatTimer);
      this.heartbeatTimer = null;
    }

    disconnect() {
      this.stopHeartbeat();
      if (!this.socket || this.socket.readyState >= 2) return;
      this.socket.close(1000, "demo disconnect");
    }
  }

  function rootWebSocket() {
    if (typeof WebSocket === "undefined") {
      throw new Error("WebSocket API is unavailable");
    }
    return WebSocket;
  }

  return Object.freeze({
    CHANNEL_PATH,
    DEFAULT_ENDPOINT,
    XiraSocketClient,
    buildHelloFrame,
    buildHumanResponseFrame,
    buildMessageFrame,
    buildPingFrame,
    normalizeEndpoint,
  });
});
