package humanrequest

import (
	"context"
	"crypto/sha256"
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
	"gopkg.in/yaml.v3"
)

var (
	ErrValidation = errors.New("validation")
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
)

type Store struct {
	root string
	mu   sync.Mutex
}

func NewStore(root string) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = ".xira"
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
		ID:             id,
		WorkspaceID:    strings.TrimSpace(input.WorkspaceID),
		WorkspaceKey:   strings.TrimSpace(input.WorkspaceKey),
		RunID:          strings.TrimSpace(input.RunID),
		AgentID:        strings.TrimSpace(input.AgentID),
		SessionID:      strings.TrimSpace(input.SessionID),
		ToolCallID:     strings.TrimSpace(input.ToolCallID),
		Source:         strings.TrimSpace(input.Source),
		Kind:           input.Kind,
		Status:         StatusPending,
		Question:       strings.TrimSpace(input.Question),
		Options:        append([]HumanOption(nil), input.Options...),
		ActionSnapshot: cloneActionSnapshot(input.ActionSnapshot),
		DedupeKey:      strings.TrimSpace(input.DedupeKey),
		CreatedAt:      now,
		Metadata:       cloneStringMap(input.Metadata),
		Audit: []AuditRecord{{
			Time:     now,
			Action:   "human_request.created",
			ToStatus: StatusPending,
		}},
	}
	if req.ActionSnapshot != nil {
		req.Replay = &ReplayState{Status: ReplayPending, UpdatedAt: now}
	}
	if err := s.writeRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *Store) Resolve(ctx context.Context, input ResolveRequest) (*HumanRequest, error) {
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
	if req.Status != StatusPending {
		return nil, fmt.Errorf("%w: human request %s is already %s", ErrConflict, req.ID, req.Status)
	}
	now := input.ResolvedAt
	if now.IsZero() {
		now = time.Now()
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
	req.Status = StatusResolved
	req.ResolvedAt = &now
	req.Response = response
	if req.Replay != nil {
		switch input.Kind {
		case ResponseDeny:
			req.Replay.Status = ReplayDenied
			req.Replay.UpdatedAt = now
		case ResponseCancel:
			req.Replay.Status = ReplayCanceled
			req.Replay.UpdatedAt = now
		}
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

func (s *Store) BeginReplay(ctx context.Context, input ReplayLeaseRequest) (*HumanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceKey(input.WorkspaceKey); err != nil {
		return nil, err
	}
	if err := validatePathID(input.RequestID, "request id"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Owner) == "" {
		return nil, fmt.Errorf("%w: owner is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	req, err := s.loadRequest(input.WorkspaceKey, input.RequestID)
	if err != nil {
		return nil, err
	}
	if req.Replay == nil {
		return nil, fmt.Errorf("%w: request has no replay snapshot", ErrConflict)
	}
	now := input.Now
	if now.IsZero() {
		now = time.Now()
	}
	duration := input.LeaseDuration
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	switch req.Replay.Status {
	case ReplayPending, ReplayFailed:
	case ReplayRunning:
		if req.Replay.LeaseUntil != nil && now.Before(*req.Replay.LeaseUntil) {
			return nil, fmt.Errorf("%w: replay lease held by %s", ErrConflict, req.Replay.LeaseOwner)
		}
	case ReplayCompleted, ReplayDenied, ReplayCanceled:
		return nil, fmt.Errorf("%w: replay is %s", ErrConflict, req.Replay.Status)
	default:
		return nil, fmt.Errorf("%w: replay status %q cannot start", ErrConflict, req.Replay.Status)
	}
	leaseUntil := now.Add(duration)
	req.Replay.Status = ReplayRunning
	req.Replay.LeaseOwner = strings.TrimSpace(input.Owner)
	req.Replay.LeaseUntil = &leaseUntil
	req.Replay.UpdatedAt = now
	req.Audit = append(req.Audit, AuditRecord{
		Time:   now,
		Actor:  req.Replay.LeaseOwner,
		Action: "human_request.replay_started",
	})
	if err := s.writeRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *Store) CompleteReplay(ctx context.Context, input CompleteReplayRequest) (*HumanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceKey(input.WorkspaceKey); err != nil {
		return nil, err
	}
	if err := validatePathID(input.RequestID, "request id"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Owner) == "" {
		return nil, fmt.Errorf("%w: owner is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	req, err := s.loadRequest(input.WorkspaceKey, input.RequestID)
	if err != nil {
		return nil, err
	}
	now := input.CompletedAt
	if now.IsZero() {
		now = time.Now()
	}
	idem := strings.TrimSpace(input.IdempotencyKey)
	if req.Replay != nil && req.Replay.Status == ReplayCompleted {
		if idem != "" && req.Replay.IdempotencyKey == idem {
			return req, nil
		}
		return nil, fmt.Errorf("%w: replay already completed", ErrConflict)
	}
	if req.Replay == nil || req.Replay.Status != ReplayRunning {
		return nil, fmt.Errorf("%w: replay is not running", ErrConflict)
	}
	if req.Replay.LeaseOwner != strings.TrimSpace(input.Owner) {
		return nil, fmt.Errorf("%w: replay lease held by %s", ErrConflict, req.Replay.LeaseOwner)
	}
	req.Replay.Status = ReplayCompleted
	req.Replay.LeaseUntil = nil
	req.Replay.ResultDigest = strings.TrimSpace(input.ResultDigest)
	req.Replay.ResultReference = strings.TrimSpace(input.ResultReference)
	req.Replay.IdempotencyKey = idem
	req.Replay.UpdatedAt = now
	req.Audit = append(req.Audit, AuditRecord{
		Time:   now,
		Actor:  strings.TrimSpace(input.Owner),
		Action: "human_request.replay_completed",
	})
	if err := s.writeReplayResult(req.WorkspaceKey, req.ID, req.Replay); err != nil {
		return nil, err
	}
	if err := s.writeRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *Store) FailReplay(ctx context.Context, input FailReplayRequest) (*HumanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceKey(input.WorkspaceKey); err != nil {
		return nil, err
	}
	if err := validatePathID(input.RequestID, "request id"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Owner) == "" {
		return nil, fmt.Errorf("%w: owner is required", ErrValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	req, err := s.loadRequest(input.WorkspaceKey, input.RequestID)
	if err != nil {
		return nil, err
	}
	if req.Replay == nil || req.Replay.Status != ReplayRunning {
		return nil, fmt.Errorf("%w: replay is not running", ErrConflict)
	}
	if req.Replay.LeaseOwner != strings.TrimSpace(input.Owner) {
		return nil, fmt.Errorf("%w: replay lease held by %s", ErrConflict, req.Replay.LeaseOwner)
	}
	now := input.FailedAt
	if now.IsZero() {
		now = time.Now()
	}
	req.Replay.Status = ReplayFailed
	req.Replay.LeaseUntil = nil
	req.Replay.Error = strings.TrimSpace(input.Error)
	req.Replay.UpdatedAt = now
	req.Audit = append(req.Audit, AuditRecord{
		Time:    now,
		Actor:   strings.TrimSpace(input.Owner),
		Action:  "human_request.replay_failed",
		Message: req.Replay.Error,
	})
	if err := s.writeRequest(req); err != nil {
		return nil, err
	}
	return req, nil
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
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		req, err := readYAMLFile[HumanRequest](path)
		if err != nil {
			return nil, fmt.Errorf("load human request %s: %w", path, err)
		}
		if query.Status != "" && req.Status != query.Status {
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
	req, err := readYAMLFile[HumanRequest](path)
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
	return writeYAMLAtomic(s.requestPath(req.WorkspaceKey, req.ID), req, 0o600)
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
	return writeYAMLAtomic(filepath.Join(s.workspaceDir(workspaceKey), "human-responses", response.ID+".yaml"), response, 0o600)
}

func (s *Store) writeReplayResult(workspaceKey, requestID string, replay *ReplayState) error {
	if err := validateWorkspaceKey(workspaceKey); err != nil {
		return err
	}
	if err := validatePathID(requestID, "request id"); err != nil {
		return err
	}
	return writeYAMLAtomic(filepath.Join(s.workspaceDir(workspaceKey), "replay-results", requestID+".yaml"), replay, 0o600)
}

func (s *Store) workspaceDir(workspaceKey string) string {
	return filepath.Join(s.root, "workspaces", workspaceKey)
}

func (s *Store) requestsDir(workspaceKey string) string {
	return filepath.Join(s.workspaceDir(workspaceKey), "human-requests")
}

func (s *Store) requestPath(workspaceKey, requestID string) string {
	return filepath.Join(s.requestsDir(workspaceKey), strings.TrimSpace(requestID)+".yaml")
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

func readYAMLFile[T any](path string) (T, error) {
	var value T
	data, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	if err := yaml.Unmarshal(data, &value); err != nil {
		return value, err
	}
	return value, nil
}

func writeYAMLAtomic(path string, value any, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*.yaml")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func cloneActionSnapshot(in *ActionSnapshot) *ActionSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	if in.Arguments != nil {
		out.Arguments = map[string]any{}
		for key, value := range in.Arguments {
			out.Arguments[key] = value
		}
	}
	return &out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
