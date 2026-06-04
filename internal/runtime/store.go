package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type RunStore struct {
	root string
	mu   sync.RWMutex
}

func NewRunStore(root string) *RunStore {
	if strings.TrimSpace(root) == "" {
		root = ".xira/runs"
	}
	return &RunStore{root: root}
}

func (s *RunStore) Root() string {
	return s.root
}

func (s *RunStore) RunDir(runID string) string {
	return filepath.Join(s.root, runID)
}

func (s *RunStore) InitRun(runID string) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("run id is required")
	}
	return os.MkdirAll(filepath.Join(s.RunDir(runID), "artifacts"), 0o755)
}

func (s *RunStore) SaveRun(resp TurnResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.InitRun(resp.RunID); err != nil {
		return err
	}
	runPath := filepath.Join(s.RunDir(resp.RunID), "run.yaml")
	data, err := yaml.Marshal(resp)
	if err != nil {
		return err
	}
	if err := os.WriteFile(runPath, data, 0o644); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(s.RunDir(resp.RunID), "events.jsonl"), resp.Events); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(s.RunDir(resp.RunID), "audit.jsonl"), resp.AuditEvents); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(s.RunDir(resp.RunID), "tool_calls.jsonl"), resp.ToolCalls); err != nil {
		return err
	}
	verification, err := json.MarshalIndent(resp.VerificationResult, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.RunDir(resp.RunID), "verification.json"), verification, 0o644); err != nil {
		return err
	}
	if resp.EvolutionCandidate != nil {
		candDir := filepath.Join(s.RunDir(resp.RunID), "evolution", "candidates")
		if err := os.MkdirAll(candDir, 0o755); err != nil {
			return err
		}
		cand, err := yaml.Marshal(resp.EvolutionCandidate)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(candDir, resp.EvolutionCandidate.ID+".yaml"), cand, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (s *RunStore) List() ([]TurnResponse, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]TurnResponse, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		resp, err := s.Load(entry.Name())
		if err == nil {
			out = append(out, resp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

func (s *RunStore) Load(runID string) (TurnResponse, error) {
	var resp TurnResponse
	data, err := os.ReadFile(filepath.Join(s.RunDir(runID), "run.yaml"))
	if err != nil {
		return resp, err
	}
	if err := yaml.Unmarshal(data, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

func NewRunID(agentID string, now time.Time) string {
	safeAgent := strings.NewReplacer("/", "-", " ", "-").Replace(agentID)
	return fmt.Sprintf("%s-%s", now.Format("20060102-150405"), safeAgent)
}

func writeJSONL[T any](path string, values []T) error {
	if len(values) == 0 {
		return os.WriteFile(path, nil, 0o644)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return err
		}
	}
	return nil
}
