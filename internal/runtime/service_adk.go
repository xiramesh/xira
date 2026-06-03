package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/ai-daming/flowdeck/internal/agents"
	"github.com/ai-daming/flowdeck/internal/model/deepseek"
)

type adkExecArgs struct {
	Action         string `json:"action,omitempty"`
	Command        string `json:"command"`
	CWD            string `json:"cwd,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type adkReadFileArgs struct {
	Path string `json:"path"`
}

type adkWriteFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type adkListDirArgs struct {
	Path string `json:"path,omitempty"`
}

type adkEditFileArgs struct {
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

func (s *Service) generateADK(
	ctx context.Context,
	profile agents.Profile,
	req TurnRequest,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) (string, []ToolCallRecord, error) {
	adkModel, err := deepseek.NewADKModel(profile.ModelPolicy.Model, s.deepseek)
	if err != nil {
		return "", nil, err
	}
	var toolRecords []ToolCallRecord
	tools, err := s.adkTools(ctx, profile, recordEvent, recordAudit, func(rec ToolCallRecord) {
		toolRecords = append(toolRecords, rec)
	})
	if err != nil {
		return "", nil, err
	}
	agent, err := llmagent.New(llmagent.Config{
		Name:        profile.ID,
		Description: profile.Description,
		Model:       adkModel,
		Instruction: s.instructionText(profile),
		Tools:       tools,
	})
	if err != nil {
		return "", nil, err
	}
	run, err := runner.New(runner.Config{
		AppName:           "flowdeck",
		Agent:             agent,
		SessionService:    s.adkSessions,
		AutoCreateSession: true,
	})
	if err != nil {
		return "", nil, err
	}
	var final string
	for evt, err := range run.Run(ctx, req.UserID, req.SessionID, genai.NewContentFromText(req.Message, genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			return final, nil, err
		}
		if evt == nil {
			continue
		}
		recordEvent("adk.event", "adk.runner", evt.Author, map[string]any{
			"event_id":      evt.ID,
			"invocation_id": evt.InvocationID,
			"partial":       evt.Partial,
			"final":         evt.IsFinalResponse(),
		})
		if evt.IsFinalResponse() {
			final = contentText(evt.Content)
		}
	}
	if strings.TrimSpace(final) == "" {
		return final, nil, fmt.Errorf("ADK runner produced empty final response")
	}
	recordAudit("adk.runner", profile.ID, true, "ADK runner completed", nil)
	return final, toolRecords, nil
}

func (s *Service) adkTools(
	ctx context.Context,
	profile agents.Profile,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
	recordTool func(ToolCallRecord),
) ([]adktool.Tool, error) {
	var out []adktool.Tool
	registry := s.toolRegistry(profile)
	description := func(name string) string {
		tool, ok := registry.Get(name)
		if !ok {
			return ""
		}
		return tool.Description()
	}
	run := func(name string, args any) (map[string]any, error) {
		input := mapFromStruct(args)
		call := deepseek.ToolCall{
			Type: "function",
			Function: deepseek.ToolCallFunction{
				Name:      name,
				Arguments: mustJSON(input),
			},
		}
		rec := s.executeToolCall(ctx, profile, call, recordEvent, recordAudit)
		recordTool(rec)
		if rec.Error != "" {
			return rec.Output, fmt.Errorf("%s", rec.Error)
		}
		return rec.Output, nil
	}
	if registry.Has("exec") {
		t, err := functiontool.New(functiontool.Config{
			Name:        "exec",
			Description: description("exec"),
		}, func(_ adktool.Context, args adkExecArgs) (map[string]any, error) {
			return run("exec", args)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if registry.Has("read_file") {
		t, err := functiontool.New(functiontool.Config{
			Name:        "read_file",
			Description: description("read_file"),
		}, func(_ adktool.Context, args adkReadFileArgs) (map[string]any, error) {
			return run("read_file", args)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if registry.Has("write_file") {
		t, err := functiontool.New(functiontool.Config{
			Name:        "write_file",
			Description: description("write_file"),
		}, func(_ adktool.Context, args adkWriteFileArgs) (map[string]any, error) {
			return run("write_file", args)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if registry.Has("list_dir") {
		t, err := functiontool.New(functiontool.Config{
			Name:        "list_dir",
			Description: description("list_dir"),
		}, func(_ adktool.Context, args adkListDirArgs) (map[string]any, error) {
			return run("list_dir", args)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if registry.Has("edit_file") {
		t, err := functiontool.New(functiontool.Config{
			Name:        "edit_file",
			Description: description("edit_file"),
		}, func(_ adktool.Context, args adkEditFileArgs) (map[string]any, error) {
			return run("edit_file", args)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func mapFromStruct(value any) map[string]any {
	data, err := json.Marshal(value)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func contentText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var parts []string
	for _, part := range content.Parts {
		if part != nil && part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "")
}
