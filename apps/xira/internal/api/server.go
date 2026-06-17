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
	"github.com/xiramesh/xira/internal/humanrequest"
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
	mux.HandleFunc("/api/v1/human-requests", s.humanRequests)
	mux.HandleFunc("/api/v1/human-requests/", s.humanRequestByID)
	mux.HandleFunc("/api/v1/channels/xiragarden/messages", s.xiragardenMessages)
	mux.HandleFunc("/api/v1/channels/xiragarden/events", s.xiragardenEvents)
	mux.HandleFunc("/api/v1/entrypoints/", s.entrypointControls)
	mux.HandleFunc("/api/v1/runs", s.runs)
	mux.HandleFunc("/api/v1/runs/", s.runByID)
	mux.HandleFunc("/api/v1/flows/runs", s.flowRuns)
	mux.HandleFunc("/api/v1/flows/runs/", s.flowRunByID)
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
	if req.Context.Channel != "" && normalizeChannel(req.Context.Channel) != channelName {
		http.Error(w, fmt.Sprintf("request channel must be %q", channelName), http.StatusBadRequest)
		return
	}
	req.Context.Channel = channelName
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

func (s *Server) humanRequestByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/human-requests/")
	requestID, resource, ok := parseHumanRequestPath(path)
	if !ok || requestID == "" {
		http.NotFound(w, r)
		return
	}
	if resource == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		req, err := s.runtime.GetHumanRequest(r.Context(), requestID)
		if err != nil {
			if errors.Is(err, humanrequest.ErrNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, req)
		return
	}
	if resource != "responses" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Kind           humanrequest.ResponseKind `json:"kind"`
		Actor          string                    `json:"actor"`
		Message        string                    `json:"message"`
		IdempotencyKey string                    `json:"idempotency_key"`
		WorkspaceKey   string                    `json:"workspace_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.WorkspaceKey) != "" {
		http.Error(w, "workspace_key override is not allowed", http.StatusBadRequest)
		return
	}
	resolved, err := s.runtime.ResolveHumanRequest(r.Context(), requestID, humanrequest.ResolveRequest{
		Kind:           body.Kind,
		Actor:          body.Actor,
		Message:        body.Message,
		IdempotencyKey: body.IdempotencyKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, humanrequest.ErrNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, humanrequest.ErrConflict):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, humanrequest.ErrValidation):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, resolved)
}

func parseHumanRequestPath(path string) (requestID, resource string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 1 && strings.TrimSpace(parts[0]) != "" {
		return parts[0], "", true
	}
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *Server) humanRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status := humanrequest.RequestStatus(strings.TrimSpace(r.URL.Query().Get("status")))
	list, err := s.runtime.ListHumanRequests(r.Context(), status)
	if err != nil {
		if errors.Is(err, humanrequest.ErrValidation) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
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

func (s *Server) flowRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		FlowPath     string            `json:"flow_path"`
		FlowID       string            `json:"flow_id"`
		EntrypointID string            `json:"entrypoint_id"`
		Input        map[string]string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.FlowPath) == "" && strings.TrimSpace(body.FlowID) == "" {
		http.Error(w, "flow_path or flow_id is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.EntrypointID) == "" {
		http.Error(w, "entrypoint_id is required", http.StatusBadRequest)
		return
	}
	run, err := s.runtime.StartFlow(r.Context(), frt.FlowStartRequest{
		FlowPath:     body.FlowPath,
		FlowID:       body.FlowID,
		EntrypointID: body.EntrypointID,
		Input:        body.Input,
	})
	if err != nil {
		writeFlowError(w, err)
		return
	}
	writeJSON(w, run)
}

func (s *Server) flowRunByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/flows/runs/")
	flowRunID, resource, ok := parseFlowRunPath(path)
	if !ok || flowRunID == "" {
		http.NotFound(w, r)
		return
	}
	switch resource {
	case "":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		run, err := s.runtime.GetFlowRun(r.Context(), flowRunID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, run)
	case "advance":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		run, err := s.runtime.AdvanceFlow(r.Context(), flowRunID)
		if err != nil {
			writeFlowError(w, err)
			return
		}
		writeJSON(w, run)
	case "resume":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			HumanRequestID string `json:"human_request_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.HumanRequestID) == "" {
			http.Error(w, "human_request_id is required", http.StatusBadRequest)
			return
		}
		run, err := s.runtime.ResumeFlow(r.Context(), flowRunID, body.HumanRequestID)
		if err != nil {
			writeFlowError(w, err)
			return
		}
		writeJSON(w, run)
	default:
		http.NotFound(w, r)
	}
}

func parseFlowRunPath(path string) (flowRunID, resource string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 1 && strings.TrimSpace(parts[0]) != "" {
		return parts[0], "", true
	}
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func writeFlowError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	msg := err.Error()
	switch {
	case strings.Contains(msg, "flow run not found"), strings.Contains(msg, "not found: human request"), strings.Contains(msg, "human request ") && strings.Contains(msg, "not found"):
		status = http.StatusNotFound
	case strings.Contains(msg, "still pending"), strings.Contains(msg, "already resolved"), strings.Contains(msg, "already completed"):
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if encErr := json.NewEncoder(w).Encode(map[string]any{"error": msg}); encErr != nil {
		http.Error(w, fmt.Sprintf("encode json: %v", encErr), http.StatusInternalServerError)
	}
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
