package flow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/xiramesh/xira/internal/channel"
)

// Store is a file-backed store for flow runs. Each run lives at
// <root>/flow-runs/<flow_run_id>/flow_run.yaml, with an events.jsonl and
// artifacts/ subdirectory alongside. Writes are atomic via temp file + rename.
type Store struct {
	root string
	mu   sync.Mutex
}

// NewStore returns a store rooted at root. If root is empty, defaults to .xira
// under the current directory.
func NewStore(root string) *Store {
	root = strings.TrimSpace(root)
	if root == "" {
		root = ".xira"
	}
	return &Store{root: root}
}

// Root returns the store root directory.
func (s *Store) Root() string {
	return s.root
}

// CreateRunRequest creates a new flow run. If ID is empty a deterministic id is
// generated. If a run with the same ID already exists and the FlowID matches,
// the existing run is returned (idempotent); a different FlowID is a conflict.
type CreateRunRequest struct {
	ID            string
	FlowID        string
	FlowVersion   string
	EntrypointID  string
	CurrentStepID string
	Input         map[string]string
	// Context is the persisted trigger identity for this run (see Run.Context).
	Context *channel.InboundContext
}

// CreateRun persists a new flow run.
func (s *Store) CreateRun(ctx context.Context, req CreateRunRequest) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.FlowID) == "" {
		return nil, fmt.Errorf("flow id is required")
	}
	if err := validateFlowID(req.FlowID); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(req.ID)
	if id != "" {
		if err := validateFlowRunID(id); err != nil {
			return nil, err
		}
	} else {
		id = generateFlowRunID(req.FlowID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Idempotency: existing run with same id + flow id returns it.
	if existing, err := s.readRunLocked(id); err == nil {
		if existing.FlowID != req.FlowID {
			return nil, fmt.Errorf("flow run %q already exists for flow %q", id, existing.FlowID)
		}
		return existing, nil
	} else if !errors.Is(err, ErrRunNotFound) {
		return nil, err
	}

	now := time.Now().UTC()
	run := &Run{
		SchemaVersion: SchemaVersionRun,
		ID:            id,
		FlowID:        req.FlowID,
		FlowVersion:   req.FlowVersion,
		Status:        RunPending,
		CurrentStepID: req.CurrentStepID,
		EntrypointID:  req.EntrypointID,
		Input:         cloneStringMap(req.Input),
		Context:       cloneInboundContext(req.Context),
		Steps:         map[string]StepState{},
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.writeRunLocked(run); err != nil {
		return nil, err
	}
	return run, nil
}

// GetRun loads a flow run by id.
func (s *Store) GetRun(ctx context.Context, flowRunID string) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateFlowRunID(flowRunID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readRunLocked(flowRunID)
}

// UpdateRun loads a run, applies fn, and persists it. fn must mutate the Run
// in place; the returned Run is the persisted state. The mutex is held for the
// whole read-modify-write, so fn can safely advance multi-field state.
func (s *Store) UpdateRun(ctx context.Context, flowRunID string, fn func(*Run) error) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateFlowRunID(flowRunID); err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, fmt.Errorf("update fn is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	run, err := s.readRunLocked(flowRunID)
	if err != nil {
		return nil, err
	}
	if err := fn(run); err != nil {
		return nil, err
	}
	if err := validateRunArtifacts(run); err != nil {
		return nil, err
	}
	run.UpdatedAt = time.Now().UTC()
	if err := s.writeRunLocked(run); err != nil {
		return nil, err
	}
	return run, nil
}

// AppendEvents appends events to events.jsonl under the run dir. It creates
// the run dir if missing so events can be recorded before the run file is
// written (used for create-time events).
func (s *Store) AppendEvents(ctx context.Context, flowRunID string, events []Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateFlowRunID(flowRunID); err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.runDir(flowRunID), "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for i := range events {
		evt := events[i]
		if evt.ID == "" {
			evt.ID = "fevt_" + uuid.NewString()
		}
		if evt.Time.IsZero() {
			evt.Time = time.Now().UTC()
		}
		if evt.FlowRunID == "" {
			evt.FlowRunID = flowRunID
		}
		if err := enc.Encode(evt); err != nil {
			return err
		}
	}
	return nil
}

// ReadEvents reads all events from events.jsonl for the run.
func (s *Store) ReadEvents(ctx context.Context, flowRunID string) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateFlowRunID(flowRunID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.runDir(flowRunID), "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Event
	dec := json.NewDecoder(strings.NewReader(string(data)))
	for dec.More() {
		var evt Event
		if err := dec.Decode(&evt); err != nil {
			return nil, fmt.Errorf("decode event: %w", err)
		}
		out = append(out, evt)
	}
	return out, nil
}

// RunDir returns the on-disk directory for a run.
func (s *Store) RunDir(flowRunID string) string {
	return s.runDir(flowRunID)
}

// SaveDefinition persists the flow definition alongside the run so later
// Advance/Resume calls (possibly in a fresh process) can reload it without
// the caller re-passing the flow path.
func (s *Store) SaveDefinition(flowRunID string, def *Definition) error {
	if def == nil {
		return fmt.Errorf("definition is required")
	}
	if err := validateFlowRunID(flowRunID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.runDir(flowRunID), "definition.yaml")
	data, err := yaml.Marshal(def)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, data, 0o644)
}

// LoadDefinitionForRun reads the persisted flow definition for a run.
func (s *Store) LoadDefinitionForRun(flowRunID string) (*Definition, error) {
	if err := validateFlowRunID(flowRunID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.runDir(flowRunID), "definition.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: definition for flow run %s", ErrRunNotFound, flowRunID)
		}
		return nil, err
	}
	var def Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("parse definition for run %s: %w", flowRunID, err)
	}
	return &def, nil
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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

// ArtifactsDir returns the artifacts subdirectory path for a run.
func (s *Store) ArtifactsDir(flowRunID string) string {
	return filepath.Join(s.runDir(flowRunID), "artifacts")
}

func (s *Store) runDir(flowRunID string) string {
	return filepath.Join(s.root, "flow-runs", flowRunID)
}

func (s *Store) readRunLocked(flowRunID string) (*Run, error) {
	path := filepath.Join(s.runDir(flowRunID), "flow_run.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: flow run %s", ErrRunNotFound, flowRunID)
		}
		return nil, err
	}
	var run Run
	if err := yaml.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("parse flow run %s: %w", flowRunID, err)
	}
	if run.Steps == nil {
		run.Steps = map[string]StepState{}
	}
	return &run, nil
}

func (s *Store) writeRunLocked(run *Run) error {
	if run == nil {
		return fmt.Errorf("run is required")
	}
	if err := validateFlowRunID(run.ID); err != nil {
		return err
	}
	path := filepath.Join(s.runDir(run.ID), "flow_run.yaml")
	data, err := yaml.Marshal(run)
	if err != nil {
		return err
	}
	if err := writeAtomic(path, data, 0o644); err != nil {
		return err
	}
	// Ensure artifacts dir exists for a newly created run.
	_ = os.MkdirAll(filepath.Join(s.runDir(run.ID), "artifacts"), 0o755)
	return nil
}

// validateRunArtifacts rejects artifact refs that escape the run dir.
func validateRunArtifacts(run *Run) error {
	if run == nil {
		return nil
	}
	for _, step := range run.Steps {
		for _, ref := range step.Artifacts {
			if err := validateArtifactPath(ref.Path); err != nil {
				return fmt.Errorf("step artifact: %w", err)
			}
		}
	}
	for _, ref := range run.Artifacts {
		if err := validateArtifactPath(ref.Path); err != nil {
			return fmt.Errorf("run artifact: %w", err)
		}
	}
	return nil
}

func validateArtifactPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("artifact path is required")
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("artifact path must be relative, got %q", path)
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("artifact path must not contain parent traversal, got %q", path)
	}
	return nil
}

func validateFlowRunID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("flow run id is required")
	}
	if len(id) > 128 {
		return fmt.Errorf("flow run id too long")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-':
		default:
			return fmt.Errorf("flow run id %q contains invalid character %q", id, r)
		}
	}
	return nil
}

func validateFlowID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("flow id is required")
	}
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return fmt.Errorf("invalid flow id %q", id)
	}
	return nil
}

func generateFlowRunID(flowID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(flowID) + time.Now().UTC().Format(time.RFC3339Nano) + uuid.NewString()))
	return "fr_" + time.Now().UTC().Format("20060102") + "_" + hex.EncodeToString(sum[:])[:12]
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// cloneInboundContext returns a defensive copy of the trigger context, or nil if
// the input is nil. Used when persisting a run so later mutations of the caller's
// context cannot bleed into the stored run.
func cloneInboundContext(in *channel.InboundContext) *channel.InboundContext {
	if in == nil {
		return nil
	}
	cp := *in
	if in.Raw != nil {
		cp.Raw = cloneStringMap(in.Raw)
	}
	return &cp
}

// Errors returned by the store.
var (
	// ErrRunNotFound is returned when a flow run does not exist.
	ErrRunNotFound = errors.New("flow run not found")
	// ErrStepAlreadyCompleted is returned when attempting to re-execute a
	// completed step without explicitly opting into retry.
	ErrStepAlreadyCompleted = errors.New("step already completed")
)

// MarkStepRunning transitions a step to running, rejecting re-execution of a
// completed step unless MarkStepRunningWithRetry is used.
func MarkStepRunning(run *Run, stepID string) error {
	return markStepRunning(run, stepID, false)
}

// MarkStepRunningWithRetry transitions a step to running, allowing re-execution
// of a previously completed step and bumping its attempts counter.
func MarkStepRunningWithRetry(run *Run, stepID string) error {
	return markStepRunning(run, stepID, true)
}

func markStepRunning(run *Run, stepID string, allowRetry bool) error {
	if run == nil {
		return fmt.Errorf("run is required")
	}
	if strings.TrimSpace(stepID) == "" {
		return fmt.Errorf("step id is required")
	}
	if run.Steps == nil {
		run.Steps = map[string]StepState{}
	}
	step, ok := run.Steps[stepID]
	if ok && step.Status == StepCompleted && !allowRetry {
		return fmt.Errorf("%w: step %q", ErrStepAlreadyCompleted, stepID)
	}
	now := time.Now().UTC()
	if step.Status == StepCompleted && allowRetry {
		step.Attempts++
		step.CompletedAt = nil
		step.Outputs = nil
		step.StartedAt = nil
		step.Error = ""
	} else if step.StartedAt == nil {
		if step.Attempts == 0 {
			step.Attempts = 1
		} else {
			step.Attempts++
		}
	}
	step.Status = StepRunning
	step.StartedAt = &now
	run.Steps[stepID] = step
	return nil
}
