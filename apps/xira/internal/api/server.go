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

	frt "github.com/ai-daming/xira/internal/runtime"
)

type Server struct {
	runtime *frt.Service
	server  *http.Server
	addr    string
}

const xiragardenChannel = "xiragarden"

func NewServer(rt *frt.Service, addr string) *Server {
	if strings.TrimSpace(addr) == "" {
		addr = "127.0.0.1:0"
	}
	s := &Server{runtime: rt, addr: addr}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", s.status)
	mux.HandleFunc("/api/v1/agents", s.agents)
	mux.HandleFunc("/api/v1/agent-runs", s.agentRuns)
	mux.HandleFunc("/api/v1/events", s.events)
	mux.HandleFunc("/api/v1/channels/xiragarden/messages", s.xiragardenMessages)
	mux.HandleFunc("/api/v1/channels/xiragarden/events", s.xiragardenEvents)
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
	if normalizeChannel(payloadString(evt.Payload, "channel")) != channelName {
		return false
	}
	if evt.RunID != "" {
		runIDs[evt.RunID] = struct{}{}
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
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
