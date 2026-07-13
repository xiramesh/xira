package humanrequest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrValidation = errors.New("validation")
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
	ErrExpired    = errors.New("expired")
)

type Store struct {
	root string
	mu   sync.Mutex
}

func NewStore(root string) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("human request store state dir is required")
	}
	return &Store{root: root}, nil
}

func WorkspaceKeyFor(workspaceID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(workspaceID)))
	return "ws_" + hex.EncodeToString(sum[:])[:16]
}

func (s *Store) Create(ctx context.Context, input CreateRequest) (*HumanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateCreate(input); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok, err := s.findPendingDuplicate(input); err != nil {
		return nil, err
	} else if ok {
		return existing, nil
	}

	now := input.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = "hrq_" + uuid.NewString()
	} else if err := validatePathID(id, "request id"); err != nil {
		return nil, err
	}
	req := &HumanRequest{
		ID:               id,
		WorkspaceID:      strings.TrimSpace(input.WorkspaceID),
		WorkspaceKey:     strings.TrimSpace(input.WorkspaceKey),
		RunID:            strings.TrimSpace(input.RunID),
		AgentID:          strings.TrimSpace(input.AgentID),
		SessionID:        strings.TrimSpace(input.SessionID),
		ToolCallID:       strings.TrimSpace(input.ToolCallID),
		Source:           strings.TrimSpace(input.Source),
		Kind:             input.Kind,
		Status:           StatusPending,
		Question:         strings.TrimSpace(input.Question),
		Options:          append([]HumanOption(nil), input.Options...),
		DedupeKey:        strings.TrimSpace(input.DedupeKey),
		Responder:        normalizeResponderPolicy(input.Responder),
		CorrelationToken: strings.TrimSpace(input.CorrelationToken),
		ChatKey:          strings.TrimSpace(input.ChatKey),
		CreatedAt:        now,
		ExpiresAt:        copyTime(input.ExpiresAt),
		Metadata:         cloneStringMap(input.Metadata),
		Audit: []AuditRecord{{
			Time:     now,
			Action:   "human_request.created",
			ToStatus: StatusPending,
		}},
	}
	if req.CorrelationToken == "" {
		req.CorrelationToken = uuid.NewString()
	}
	req.Delivery.Status = DeliveryNone
	if input.DeliveryRequired {
		req.Delivery.Status = DeliveryPending
	}
	req.Resume.Status = ResumeWaitingResponse
	if err := s.writeRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *Store) Resolve(ctx context.Context, input ResolveRequest) (*HumanRequest, error) {
	return s.resolve(ctx, input, nil)
}

// ResolveExact resolves one request only when the channel-supplied correlation
// and typed responder identity match the authority persisted at creation.
// coverage: contract (100% required)
func (s *Store) ResolveExact(ctx context.Context, input HumanResponseEnvelope) (*HumanRequest, error) {
	return s.resolve(ctx, ResolveRequest{
		WorkspaceKey:   input.WorkspaceKey,
		RequestID:      input.RequestID,
		Kind:           input.Kind,
		Actor:          input.SenderID,
		Message:        input.Message,
		IdempotencyKey: input.IdempotencyKey,
		ResolvedAt:     input.ResolvedAt,
	}, &input)
}

func (s *Store) resolve(ctx context.Context, input ResolveRequest, exact *HumanResponseEnvelope) (*HumanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceKey(input.WorkspaceKey); err != nil {
		return nil, err
	}
	if err := validatePathID(input.RequestID, "request id"); err != nil {
		return nil, err
	}
	if err := validateResponseKind(input.Kind); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	req, err := s.loadRequest(input.WorkspaceKey, input.RequestID)
	if err != nil {
		return nil, err
	}
	if input.Kind == ResponseAnswer && strings.TrimSpace(input.Message) == "" {
		return nil, fmt.Errorf("%w: answer message is required", ErrValidation)
	}
	now := input.ResolvedAt
	if now.IsZero() {
		now = time.Now()
	}
	if exact != nil {
		if err := validateExactResponse(req, *exact, now); err != nil {
			return nil, err
		}
	}
	if req.Status != StatusPending {
		if sameResponseRetry(req.Response, input, exact) {
			return req, nil
		}
		return nil, fmt.Errorf("%w: human request %s is already %s", ErrConflict, req.ID, req.Status)
	}
	response := &HumanResponse{
		ID:             "hrs_" + uuid.NewString(),
		RequestID:      req.ID,
		Kind:           input.Kind,
		Actor:          strings.TrimSpace(input.Actor),
		Message:        strings.TrimSpace(input.Message),
		IdempotencyKey: strings.TrimSpace(input.IdempotencyKey),
		CreatedAt:      now,
	}
	if exact != nil {
		response.ActorIDType = strings.ToLower(strings.TrimSpace(exact.SenderIDType))
		response.EntrypointID = strings.TrimSpace(exact.EntrypointID)
		response.DeliveryMessageID = strings.TrimSpace(exact.DeliveryMessageID)
	}
	req.Status = StatusResolved
	req.ResolvedAt = &now
	req.Response = response
	if req.Resume.Status == ResumeWaitingResponse {
		req.Resume.Status = ResumePending
		req.Resume.LastError = ""
	}
	req.Audit = append(req.Audit, AuditRecord{
		Time:       now,
		Actor:      response.Actor,
		Action:     "human_request.resolved",
		FromStatus: StatusPending,
		ToStatus:   StatusResolved,
		Signal:     input.Kind,
		Message:    response.Message,
	})
	if err := s.writeResponse(req.WorkspaceKey, response); err != nil {
		return nil, err
	}
	if err := s.writeRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// validateExactResponse checks every authority dimension before the request is
// mutated. Empty persisted optional dimensions are legacy-compatible; a
// non-empty persisted value must match exactly.
// coverage: contract (100% required)
func validateExactResponse(req *HumanRequest, input HumanResponseEnvelope, now time.Time) error {
	if req == nil {
		return fmt.Errorf("%w: human request is required", ErrValidation)
	}
	correlation := strings.TrimSpace(input.CorrelationToken)
	if correlation == "" || subtle.ConstantTimeCompare([]byte(correlation), []byte(req.CorrelationToken)) != 1 {
		return fmt.Errorf("%w: human response correlation does not match request", ErrConflict)
	}
	entrypointID := strings.TrimSpace(input.EntrypointID)
	senderID := strings.TrimSpace(input.SenderID)
	senderIDType := strings.ToLower(strings.TrimSpace(input.SenderIDType))
	if req.Responder.EntrypointID != "" && entrypointID != req.Responder.EntrypointID {
		return fmt.Errorf("%w: human response entrypoint does not match request", ErrConflict)
	}
	if req.Responder.SenderID != "" && senderID != req.Responder.SenderID {
		return fmt.Errorf("%w: human response sender does not match request", ErrConflict)
	}
	if req.Responder.SenderIDType != "" && senderIDType != req.Responder.SenderIDType {
		return fmt.Errorf("%w: human response sender id type does not match request", ErrConflict)
	}
	deliveryMessageID := strings.TrimSpace(input.DeliveryMessageID)
	if req.Delivery.MessageID != "" && deliveryMessageID != req.Delivery.MessageID {
		return fmt.Errorf("%w: human response delivery message does not match request", ErrConflict)
	}
	if req.ExpiresAt != nil && now.After(*req.ExpiresAt) {
		return fmt.Errorf("%w: human request %s expired at %s", ErrExpired, req.ID, req.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// sameResponseRetry recognizes an exact retry only when a non-empty stable
// idempotency key and every persisted response field match.
// coverage: contract (100% required)
func sameResponseRetry(existing *HumanResponse, input ResolveRequest, exact *HumanResponseEnvelope) bool {
	if existing == nil || strings.TrimSpace(input.IdempotencyKey) == "" || existing.IdempotencyKey != strings.TrimSpace(input.IdempotencyKey) {
		return false
	}
	if existing.Kind != input.Kind || existing.Actor != strings.TrimSpace(input.Actor) || existing.Message != strings.TrimSpace(input.Message) {
		return false
	}
	if exact == nil {
		return true
	}
	return existing.ActorIDType == strings.ToLower(strings.TrimSpace(exact.SenderIDType)) &&
		existing.EntrypointID == strings.TrimSpace(exact.EntrypointID) &&
		existing.DeliveryMessageID == strings.TrimSpace(exact.DeliveryMessageID)
}

func (s *Store) Get(ctx context.Context, workspaceKey, requestID string) (*HumanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceKey(workspaceKey); err != nil {
		return nil, err
	}
	if err := validatePathID(requestID, "request id"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadRequest(workspaceKey, requestID)
}

// FindByCorrelation returns the unique request carrying one full opaque
// correlation token across pending and resolved states. Resolved records must
// remain addressable so exact idempotent retries can reach sameResponseRetry.
// coverage: contract (100% required)
func (s *Store) FindByCorrelation(ctx context.Context, workspaceKey, correlation string) (*HumanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceKey(workspaceKey); err != nil {
		return nil, err
	}
	correlation = strings.TrimSpace(correlation)
	if correlation == "" {
		return nil, fmt.Errorf("%w: correlation token is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	requests, err := s.listLocked(ListQuery{WorkspaceKey: workspaceKey})
	if err != nil {
		return nil, err
	}
	var match *HumanRequest
	for i := range requests {
		candidate := strings.TrimSpace(requests[i].CorrelationToken)
		if candidate == "" || subtle.ConstantTimeCompare([]byte(candidate), []byte(correlation)) != 1 {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("%w: multiple human requests share one correlation token", ErrConflict)
		}
		copy := requests[i]
		match = &copy
	}
	if match == nil {
		return nil, fmt.Errorf("%w: human request correlation not found", ErrNotFound)
	}
	return match, nil
}

func (s *Store) List(ctx context.Context, query ListQuery) ([]HumanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceKey(query.WorkspaceKey); err != nil {
		return nil, err
	}
	if query.Status != "" && query.Status != StatusPending && query.Status != StatusResolved {
		return nil, fmt.Errorf("%w: invalid status %q", ErrValidation, query.Status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked(query)
}

// ListByChatKey returns the PENDING human requests originating from chatKey
// (runtime.ChatKey.String()). It's the most common query for #91-A/#92: "does
// this chat have a HITL waiting for an answer?"
//
// An empty chatKey is a CONTRACT VIOLATION for this entry point: callers asking
// "pending HITL for this chat" must supply the chat. An empty value (from a
// missing inbound field, a bug, or an unhandled zero value) would silently
// return ALL pending requests in the workspace — a cross-chat mismatch that
// breaks per-chat-key isolation. So we return a validation error rather than
// fall through to the底层 List's "empty = no filter" semantics (which exists
// for backward-compat with pre-ChatKey data, not for this dedicated query).
func (s *Store) ListByChatKey(ctx context.Context, workspaceKey, chatKey string) ([]HumanRequest, error) {
	chatKey = strings.TrimSpace(chatKey)
	if chatKey == "" {
		return nil, fmt.Errorf("%w: ListByChatKey requires a non-empty chatKey (empty would return all pending — cross-chat mismatch risk)", ErrValidation)
	}
	return s.List(ctx, ListQuery{
		WorkspaceKey: workspaceKey,
		Status:       StatusPending,
		ChatKey:      chatKey,
	})
}

func (s *Store) findPendingDuplicate(input CreateRequest) (*HumanRequest, bool, error) {
	if strings.TrimSpace(input.DedupeKey) == "" || strings.TrimSpace(input.RunID) == "" || strings.TrimSpace(input.ToolCallID) == "" {
		return nil, false, nil
	}
	list, err := s.listLocked(ListQuery{WorkspaceKey: input.WorkspaceKey, Status: StatusPending})
	if err != nil {
		return nil, false, err
	}
	for i := range list {
		req := list[i]
		if req.RunID == strings.TrimSpace(input.RunID) &&
			req.ToolCallID == strings.TrimSpace(input.ToolCallID) &&
			req.DedupeKey == strings.TrimSpace(input.DedupeKey) {
			return &req, true, nil
		}
	}
	return nil, false, nil
}

func (s *Store) listLocked(query ListQuery) ([]HumanRequest, error) {
	dir := s.requestsDir(query.WorkspaceKey)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]HumanRequest, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		req, err := readJSONFile[HumanRequest](path)
		if err != nil {
			return nil, fmt.Errorf("load human request %s: %w", path, err)
		}
		if query.Status != "" && req.Status != query.Status {
			continue
		}
		// ChatKey filter: empty = no filter (backward compatible, returns all).
		if query.ChatKey != "" && req.ChatKey != query.ChatKey {
			continue
		}
		out = append(out, req)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) loadRequest(workspaceKey, requestID string) (*HumanRequest, error) {
	if err := validateWorkspaceKey(workspaceKey); err != nil {
		return nil, err
	}
	if err := validatePathID(requestID, "request id"); err != nil {
		return nil, err
	}
	path := s.requestPath(workspaceKey, requestID)
	req, err := readJSONFile[HumanRequest](path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: human request %s", ErrNotFound, requestID)
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (s *Store) writeRequest(req *HumanRequest) error {
	if req == nil {
		return fmt.Errorf("%w: human request is required", ErrValidation)
	}
	if err := validateWorkspaceKey(req.WorkspaceKey); err != nil {
		return err
	}
	if err := validatePathID(req.ID, "request id"); err != nil {
		return err
	}
	return writeJSONAtomic(s.requestPath(req.WorkspaceKey, req.ID), req, 0o600)
}

func (s *Store) writeResponse(workspaceKey string, response *HumanResponse) error {
	if response == nil {
		return fmt.Errorf("%w: human response is required", ErrValidation)
	}
	if err := validateWorkspaceKey(workspaceKey); err != nil {
		return err
	}
	if err := validatePathID(response.ID, "response id"); err != nil {
		return err
	}
	if err := validatePathID(response.RequestID, "request id"); err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(s.workspaceDir(workspaceKey), "human-responses", response.ID+".json"), response, 0o600)
}

func (s *Store) workspaceDir(workspaceKey string) string {
	return filepath.Join(s.root, "workspaces", workspaceKey)
}

func (s *Store) requestsDir(workspaceKey string) string {
	return filepath.Join(s.workspaceDir(workspaceKey), "human-requests")
}

func (s *Store) requestPath(workspaceKey, requestID string) string {
	return filepath.Join(s.requestsDir(workspaceKey), strings.TrimSpace(requestID)+".json")
}

func validateCreate(input CreateRequest) error {
	if err := validateWorkspaceKey(input.WorkspaceKey); err != nil {
		return err
	}
	if strings.TrimSpace(input.ID) != "" {
		if err := validatePathID(input.ID, "request id"); err != nil {
			return err
		}
	}
	if strings.TrimSpace(input.WorkspaceID) == "" {
		return fmt.Errorf("%w: workspace id is required", ErrValidation)
	}
	if strings.TrimSpace(input.RunID) == "" {
		return fmt.Errorf("%w: run id is required", ErrValidation)
	}
	if strings.TrimSpace(input.AgentID) == "" {
		return fmt.Errorf("%w: agent id is required", ErrValidation)
	}
	if strings.TrimSpace(input.SessionID) == "" {
		return fmt.Errorf("%w: session id is required", ErrValidation)
	}
	if input.Kind != RequestFreeform && input.Kind != RequestApproval {
		return fmt.Errorf("%w: invalid request kind %q", ErrValidation, input.Kind)
	}
	if strings.TrimSpace(input.Question) == "" {
		return fmt.Errorf("%w: question is required", ErrValidation)
	}
	if err := validateResponderPolicy(normalizeResponderPolicy(input.Responder)); err != nil {
		return err
	}
	seenOptions := map[string]struct{}{}
	for _, opt := range input.Options {
		id := strings.TrimSpace(opt.ID)
		if id == "" {
			return fmt.Errorf("%w: option id is required", ErrValidation)
		}
		if _, exists := seenOptions[id]; exists {
			return fmt.Errorf("%w: duplicate option id %q", ErrValidation, id)
		}
		seenOptions[id] = struct{}{}
	}
	return nil
}

// normalizeResponderPolicy keeps persisted responder identity deterministic.
// A zero-value policy defaults to the human who triggered the conversation.
// coverage: contract (100% required)
func normalizeResponderPolicy(policy ResponderPolicy) ResponderPolicy {
	policy.Type = ResponderType(strings.ToLower(strings.TrimSpace(string(policy.Type))))
	if policy.Type == "" {
		policy.Type = ResponderCurrentSender
	}
	policy.EntrypointID = strings.TrimSpace(policy.EntrypointID)
	policy.SenderID = strings.TrimSpace(policy.SenderID)
	policy.SenderIDType = strings.ToLower(strings.TrimSpace(policy.SenderIDType))
	return policy
}

// validateResponderPolicy protects the authorization address persisted with a
// request. Owner requests must be fully bound before they are stored. Current
// sender fields may be absent for legacy CLI/Flow contexts.
// coverage: contract (100% required)
func validateResponderPolicy(policy ResponderPolicy) error {
	switch policy.Type {
	case ResponderCurrentSender:
		return nil
	case ResponderOwner:
		if policy.EntrypointID == "" {
			return fmt.Errorf("%w: owner responder entrypoint id is required", ErrValidation)
		}
		if policy.SenderID == "" {
			return fmt.Errorf("%w: owner responder sender id is required", ErrValidation)
		}
		if policy.SenderIDType == "" {
			return fmt.Errorf("%w: owner responder sender id type is required", ErrValidation)
		}
		return nil
	default:
		return fmt.Errorf("%w: invalid responder type %q", ErrValidation, policy.Type)
	}
}

func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := value.UTC()
	return &copied
}

func validateWorkspaceKey(key string) error {
	return validatePathID(key, "workspace key")
}

func validatePathID(id, label string) error {
	id = strings.TrimSpace(id)
	if label == "" {
		label = "path id"
	}
	if id == "" {
		return fmt.Errorf("%w: %s is required", ErrValidation, label)
	}
	if strings.HasPrefix(id, ".") || strings.Contains(id, "/") || strings.Contains(id, `\`) || strings.Contains(id, "..") || filepath.IsAbs(id) {
		return fmt.Errorf("%w: invalid %s %q", ErrValidation, label, id)
	}
	return nil
}

func validateResponseKind(kind ResponseKind) error {
	switch kind {
	case ResponseApprove, ResponseDeny, ResponseCancel, ResponseAnswer:
		return nil
	default:
		return fmt.Errorf("%w: invalid response kind %q", ErrValidation, kind)
	}
}
