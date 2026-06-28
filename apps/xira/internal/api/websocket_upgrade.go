package api

import (
	"context"
	"net/http"
	"strings"

	coderws "github.com/coder/websocket"

	wschannel "github.com/xiramesh/xira/internal/channelrunner/websocket"
)

// websocket_upgrade.go: the HTTP→websocket upgrade entry. WebSocket
// connections begin life as an HTTP request (RFC 6455 — there is no "pure WS
// listen"), so Xira's HTTP server (api.Server, which owns the mux/listener)
// registers /api/v1/channels/websocket/messages and upgrades it here. The
// upgraded connection is then handed to the websocket channel Runner
// (channelrunner/websocket) for the read loop + turn dispatch.
//
// This file holds ONLY the server-side upgrade handler — NOT the websocket
// channel implementation. The channel logic (frame types, turn dispatch,
// RunAgent, event framing, dedupe, etc.) lives in channelrunner/websocket/,
// registered with Manager alongside ilink/feishu. The split reflects the
// connection-direction asymmetry: ilink/feishu are outbound (Xira connects to
// the platform in Runner.Start), websocket is inbound (clients connect to
// Xira), so it needs this HTTP entry to accept the upgrade.

const websocketDefaultEntrypoint = "websocket-default"

// wsRunnerProvider is an optional capability of the ChannelControls passed to
// NewServer. The channelrunner.Manager implements it to expose its websocket
// Runner. api.Server uses it to delegate upgraded connections. Decoupled via
// interface so api doesn't import channelrunner (avoids an import cycle:
// channelrunner → progress → runtime, and api → runtime, but api must NOT
// depend on channelrunner).
type wsRunnerProvider interface {
	WSRunner() *wschannel.Runner
}

// websocketMessages is the HTTP handler for the websocket upgrade endpoint
// (registered at /api/v1/channels/websocket/messages). It performs the WS
// handshake and then hands the connection to the websocket Runner's
// HandleConnection for the lifetime of the connection.
func (s *Server) websocketMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runner := s.wsRunner()
	if runner == nil {
		http.Error(w, "websocket channel runner is not configured", http.StatusServiceUnavailable)
		return
	}
	conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defaultEntrypointID := firstNonEmpty(r.URL.Query().Get("entrypoint_id"), websocketDefaultEntrypoint)
	runner.HandleConnection(ctx, conn, defaultEntrypointID)
}

// wsRunner resolves the websocket Runner from the Server's ChannelControls
// (channelrunner.Manager), if the controls implement wsRunnerProvider.
func (s *Server) wsRunner() *wschannel.Runner {
	if s == nil || s.channelControls == nil {
		return nil
	}
	if provider, ok := s.channelControls.(wsRunnerProvider); ok {
		return provider.WSRunner()
	}
	return nil
}

// firstNonEmpty returns the first non-blank trimmed string. Own copy (the
// channelrunner/websocket package has its own); kept here because the upgrade
// handler uses it and api must not import channelrunner.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		v := strings.TrimSpace(value)
		if v != "" {
			return v
		}
	}
	return ""
}
