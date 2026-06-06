package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/xiramesh/xira/internal/channelcontrol"
	frt "github.com/xiramesh/xira/internal/runtime"
)

type Server struct {
	runtime         *frt.Service
	channelControls ChannelControls
	server          *http.Server
	addr            string
}

type ChannelControls interface {
	CreatePairing(context.Context, string) (channelcontrol.PairingSnapshot, error)
	GetPairing(string, string) (channelcontrol.PairingSnapshot, error)
	ListAccounts(string) ([]channelcontrol.AccountSnapshot, error)
	DeleteAccount(context.Context, string, string) error
}

const xiragardenChannel = "xiragarden"

func NewServer(rt *frt.Service, addr string, controls ...ChannelControls) *Server {
	if strings.TrimSpace(addr) == "" {
		addr = "127.0.0.1:0"
	}
	s := &Server{runtime: rt, addr: addr}
	if len(controls) > 0 {
		s.channelControls = controls[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", s.status)
	mux.HandleFunc("/api/v1/agents", s.agents)
	mux.HandleFunc("/api/v1/agent-registry", s.agentRegistry)
	mux.HandleFunc("/api/v1/agent-runs", s.agentRuns)
	mux.HandleFunc("/api/v1/events", s.events)
	mux.HandleFunc("/api/v1/channels/xiragarden/messages", s.xiragardenMessages)
	mux.HandleFunc("/api/v1/channels/xiragarden/events", s.xiragardenEvents)
	mux.HandleFunc("/api/v1/entrypoints/", s.entrypointControls)
	mux.HandleFunc("/api/v1/runs", s.runs)
	mux.HandleFunc("/api/v1/runs/", s.runByID)
	s.server = &http.Server{Addr: addr, Handler: withCORS(mux)}
	return s
}

func (s *Server) Addr() string {
	return s.addr
}

func (s *Server) URL() string {
	return "http://" + s.addr
}

func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.addr = ln.Addr().String()
	return s.serve(ctx, ln)
}

func (s *Server) StartAsync(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.addr = ln.Addr().String()
	go func() {
		_ = s.serve(ctx, ln)
	}()
	return nil
}

func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()
	err := s.server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.runtime.Status())
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.runtime.Agents())
}

func (s *Server) agentRegistry(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.runtime.AgentRegistry())
}

func (s *Server) agentRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req frt.TurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := s.runtime.RunAgent(r.Context(), req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		writeJSON(w, map[string]any{"error": err.Error(), "run": resp})
		return
	}
	writeJSON(w, resp)
}

func (s *Server) xiragardenMessages(w http.ResponseWriter, r *http.Request) {
	s.channelMessages(w, r, xiragardenChannel)
}

func (s *Server) channelMessages(w http.ResponseWriter, r *http.Request, channelName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req frt.TurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}
	if req.Channel != "" && normalizeChannel(req.Channel) != channelName {
		http.Error(w, fmt.Sprintf("request channel must be %q", channelName), http.StatusBadRequest)
		return
	}
	req.Channel = channelName
	resp, err := s.runtime.RunAgent(r.Context(), req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		writeJSON(w, map[string]any{"error": err.Error(), "run": resp})
		return
	}
	writeJSON(w, resp)
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list, err := s.runtime.RunStore().List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

func (s *Server) runByID(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimPrefix(r.URL.Path, "/api/v1/runs/")
	if runID == "" {
		http.NotFound(w, r)
		return
	}
	resp, err := s.runtime.RunStore().Load(runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	events := s.runtime.EventBus().Subscribe(r.Context())
	for evt := range events {
		if err := conn.WriteJSON(evt); err != nil {
			return
		}
	}
}

func (s *Server) xiragardenEvents(w http.ResponseWriter, r *http.Request) {
	s.channelEvents(w, r, xiragardenChannel)
}

func (s *Server) entrypointControls(w http.ResponseWriter, r *http.Request) {
	if s.channelControls == nil {
		http.Error(w, "channel controls are not available", http.StatusNotFound)
		return
	}
	entrypointID, resource, resourceID, ok := parseEntrypointControlPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch resource {
	case "pairings":
		s.entrypointPairings(w, r, entrypointID, resourceID)
	case "accounts":
		s.entrypointAccounts(w, r, entrypointID, resourceID)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) entrypointPairings(w http.ResponseWriter, r *http.Request, entrypointID, pairingID string) {
	switch {
	case r.Method == http.MethodPost && pairingID == "":
		snapshot, err := s.channelControls.CreatePairing(r.Context(), entrypointID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, snapshot)
	case r.Method == http.MethodGet && pairingID != "":
		snapshot, err := s.channelControls.GetPairing(entrypointID, pairingID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, snapshot)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) entrypointAccounts(w http.ResponseWriter, r *http.Request, entrypointID, accountID string) {
	switch {
	case r.Method == http.MethodGet && accountID == "":
		accounts, err := s.channelControls.ListAccounts(entrypointID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"accounts": accounts})
	case r.Method == http.MethodDelete && accountID != "":
		if err := s.channelControls.DeleteAccount(r.Context(), entrypointID, accountID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) channelEvents(w http.ResponseWriter, r *http.Request, channelName string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	runIDs := map[string]struct{}{}
	events := s.runtime.EventBus().Subscribe(r.Context())
	for evt := range events {
		if !eventBelongsToChannel(evt, channelName, runIDs) {
			continue
		}
		if err := conn.WriteJSON(evt); err != nil {
			return
		}
	}
}

func eventBelongsToChannel(evt frt.RuntimeEvent, channelName string, runIDs map[string]struct{}) bool {
	if evt.RunID != "" {
		if _, ok := runIDs[evt.RunID]; ok {
			return true
		}
	}
	if evt.Correlation != nil {
		if _, ok := runIDs[evt.Correlation.ParentRunID]; ok {
			if evt.RunID != "" {
				runIDs[evt.RunID] = struct{}{}
			}
			if evt.Correlation.ChildRunID != "" {
				runIDs[evt.Correlation.ChildRunID] = struct{}{}
			}
			return true
		}
		if _, ok := runIDs[evt.Correlation.ChildRunID]; ok {
			if evt.RunID != "" {
				runIDs[evt.RunID] = struct{}{}
			}
			return true
		}
	}
	var eventChannel string
	if evt.Scope != nil {
		eventChannel = evt.Scope.Channel
	}
	if eventChannel == "" {
		eventChannel = payloadString(evt.Payload, "channel")
	}
	if normalizeChannel(eventChannel) != channelName {
		return false
	}
	if evt.RunID != "" {
		runIDs[evt.RunID] = struct{}{}
	}
	if evt.Correlation != nil && evt.Correlation.ChildRunID != "" {
		runIDs[evt.Correlation.ChildRunID] = struct{}{}
	}
	return true
}

func payloadString(payload map[string]any, key string) string {
	if len(payload) == 0 {
		return ""
	}
	switch value := payload[key].(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return ""
	}
}

func normalizeChannel(channelName string) string {
	return strings.ToLower(strings.TrimSpace(channelName))
}

func parseEntrypointControlPath(path string) (entrypointID, resource, resourceID string, ok bool) {
	rest := strings.TrimPrefix(path, "/api/v1/entrypoints/")
	if rest == path {
		return "", "", "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 && len(parts) != 3 {
		return "", "", "", false
	}
	if parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	entrypointID = parts[0]
	resource = parts[1]
	if len(parts) == 3 {
		resourceID = parts[2]
	}
	return entrypointID, resource, resourceID, true
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, fmt.Sprintf("encode json: %v", err), http.StatusInternalServerError)
	}
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
