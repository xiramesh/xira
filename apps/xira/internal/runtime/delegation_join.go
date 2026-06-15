package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xiramesh/xira/internal/humanrequest"
)

const delegationJoinSchemaVersion = "xira.delegation_join.v0"

type DelegationJoinState struct {
	SchemaVersion     string               `json:"schema_version" yaml:"schema_version"`
	ID                string               `json:"id" yaml:"id"`
	Workspace         string               `json:"workspace" yaml:"workspace"`
	ParentRunID       string               `json:"parent_run_id" yaml:"parent_run_id"`
	ParentAgentID     string               `json:"parent_agent_id" yaml:"parent_agent_id"`
	ToolBatchID       string               `json:"tool_batch_id,omitempty" yaml:"tool_batch_id,omitempty"`
	JoinPolicy        string               `json:"join_policy" yaml:"join_policy"`
	Status            string               `json:"status" yaml:"status"`
	SuspendedToolCall SuspendedToolCall    `json:"suspended_tool_call" yaml:"suspended_tool_call"`
	Calls             []DelegationJoinCall `json:"calls" yaml:"calls"`
	CreatedAt         time.Time            `json:"created_at" yaml:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at" yaml:"updated_at"`
}

type DelegationJoinCall struct {
	ParentToolCallID    string `json:"parent_tool_call_id" yaml:"parent_tool_call_id"`
	ChildRunID          string `json:"child_run_id" yaml:"child_run_id"`
	ChildAgentID        string `json:"child_agent_id" yaml:"child_agent_id"`
	Status              string `json:"status" yaml:"status"`
	ChildHumanRequestID string `json:"child_human_request_id,omitempty" yaml:"child_human_request_id,omitempty"`
	OutputRef           string `json:"output_ref,omitempty" yaml:"output_ref,omitempty"`
	Error               string `json:"error,omitempty" yaml:"error,omitempty"`
}

func (s *Service) createWaitingDelegationJoinState(parentRunID, parentAgentID, parentToolCallID, childRunID, childAgentID string, childRequests []humanrequest.HumanRequest) (*DelegationJoinState, error) {
	now := time.Now()
	join := &DelegationJoinState{
		SchemaVersion: delegationJoinSchemaVersion,
		ID:            "djoin_" + shortID(),
		Workspace:     s.workspace,
		ParentRunID:   strings.TrimSpace(parentRunID),
		ParentAgentID: strings.TrimSpace(parentAgentID),
		ToolBatchID:   strings.TrimSpace(parentToolCallID),
		JoinPolicy:    "all",
		Status:        StatusWaitingHuman,
		SuspendedToolCall: SuspendedToolCall{
			ID:     strings.TrimSpace(parentToolCallID),
			RunID:  strings.TrimSpace(childRunID),
			Name:   delegateAgentToolName,
			Status: StatusWaitingHuman,
		},
		Calls: []DelegationJoinCall{{
			ParentToolCallID:    strings.TrimSpace(parentToolCallID),
			ChildRunID:          strings.TrimSpace(childRunID),
			ChildAgentID:        strings.TrimSpace(childAgentID),
			Status:              StatusWaitingHuman,
			ChildHumanRequestID: firstHumanRequestID(childRequests),
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.saveDelegationJoinState(join); err != nil {
		return nil, err
	}
	return join, nil
}

func (s *Service) saveDelegationJoinState(join *DelegationJoinState) error {
	if join == nil {
		return fmt.Errorf("delegation join state is nil")
	}
	if err := validateDelegationJoinPathID(join.ParentRunID, "parent run id"); err != nil {
		return err
	}
	if err := validateDelegationJoinPathID(join.ID, "delegation join id"); err != nil {
		return err
	}
	path := s.delegationJoinPath(join.ParentRunID, join.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(join)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func (s *Service) loadDelegationJoinState(parentRunID, joinID string) (*DelegationJoinState, error) {
	if err := validateDelegationJoinPathID(parentRunID, "parent run id"); err != nil {
		return nil, err
	}
	if err := validateDelegationJoinPathID(joinID, "delegation join id"); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.delegationJoinPath(parentRunID, joinID))
	if err != nil {
		return nil, err
	}
	var join DelegationJoinState
	if err := yaml.Unmarshal(data, &join); err != nil {
		return nil, err
	}
	return &join, nil
}

func (s *Service) loadDelegationJoinStates(parentRunID string) ([]DelegationJoinState, error) {
	if err := validateDelegationJoinPathID(parentRunID, "parent run id"); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.runs.RunDir(strings.TrimSpace(parentRunID)), "delegations")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]DelegationJoinState, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var join DelegationJoinState
		if err := yaml.Unmarshal(data, &join); err != nil {
			return nil, err
		}
		out = append(out, join)
	}
	return out, nil
}

func (s *Service) findDelegationJoinByHumanRequest(requestID string) (*DelegationJoinState, int, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, -1, nil
	}
	entries, err := os.ReadDir(s.runs.Root())
	if os.IsNotExist(err) {
		return nil, -1, nil
	}
	if err != nil {
		return nil, -1, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		joins, err := s.loadDelegationJoinStates(entry.Name())
		if err != nil {
			return nil, -1, err
		}
		for i := range joins {
			for callIndex := range joins[i].Calls {
				if joins[i].Calls[callIndex].ChildHumanRequestID == requestID {
					join := joins[i]
					return &join, callIndex, nil
				}
			}
		}
	}
	return nil, -1, nil
}

func (s *Service) outstandingChildCount(parentRunID string) (int, error) {
	count := s.activeChildCount(parentRunID)
	joins, err := s.loadDelegationJoinStates(parentRunID)
	if err != nil {
		return count, err
	}
	for _, join := range joins {
		for _, call := range join.Calls {
			if !isTerminalDelegateCallStatus(call.Status) {
				count++
			}
		}
	}
	return count, nil
}

func (s *Service) delegationJoinPath(parentRunID, joinID string) string {
	return filepath.Join(s.runs.RunDir(strings.TrimSpace(parentRunID)), "delegations", strings.TrimSpace(joinID)+".yaml")
}

func validateDelegationJoinPathID(value, label string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", label)
	}
	if strings.Contains(value, "/") || strings.Contains(value, `\`) || strings.Contains(value, "..") || strings.HasPrefix(value, ".") || filepath.IsAbs(value) {
		return fmt.Errorf("invalid %s %q", label, value)
	}
	return nil
}

func firstHumanRequestID(requests []humanrequest.HumanRequest) string {
	for _, req := range requests {
		if id := strings.TrimSpace(req.ID); id != "" {
			return id
		}
	}
	return ""
}

func isTerminalDelegateCallStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "completed", "failed", "timeout", "canceled":
		return true
	default:
		return false
	}
}
