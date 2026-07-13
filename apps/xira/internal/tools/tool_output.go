package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultToolOutputReadLimit = 20_000
const maxToolOutputReadLimit = 65_536

type runDirContextKey struct{}

type ToolOutputReadTool struct{}

func NewToolOutputReadTool() *ToolOutputReadTool {
	return &ToolOutputReadTool{}
}

func WithRunDir(ctx context.Context, runDir string) context.Context {
	runDir = strings.TrimSpace(runDir)
	if runDir == "" {
		return ctx
	}
	abs, err := filepath.Abs(runDir)
	if err != nil {
		abs = filepath.Clean(runDir)
	}
	return context.WithValue(ctx, runDirContextKey{}, abs)
}

func runDirFromContext(ctx context.Context) string {
	runDir, _ := ctx.Value(runDirContextKey{}).(string)
	return strings.TrimSpace(runDir)
}

func (t *ToolOutputReadTool) Name() string { return "tool_output.read" }
func (t *ToolOutputReadTool) Description() string {
	return "Read a bounded slice of a raw stdout/stderr artifact produced during the current Xira run."
}
func (t *ToolOutputReadTool) Guidance() string {
	return "Use this when a prior command result says its stdout or stderr preview was truncated and the missing content matters to the task. " +
		"For failures, inspect the relevant error tail before diagnosing or claiming a cause; do not guess from an incomplete preview."
}
func (t *ToolOutputReadTool) Parameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"raw_output_path": map[string]any{"type": "string", "description": "Relative raw output artifact path returned by an earlier command tool call, for example artifacts/tool-outputs/<call>.json."},
			"stream":          map[string]any{"type": "string", "enum": []string{"stdout", "stderr"}, "description": "Which stream to read."},
			"offset_bytes":    map[string]any{"type": "integer", "description": "Byte offset for slice reads. Ignored when tail_lines is set."},
			"limit_bytes":     map[string]any{"type": "integer", "description": "Maximum bytes to return. Defaults to 20000 and is capped at 65536."},
			"tail_lines":      map[string]any{"type": "integer", "description": "Return the tail of the selected stream by line count. Useful for test and compiler failures."},
		},
		"required": []string{"raw_output_path", "stream"},
	}
}
func (t *ToolOutputReadTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	runDir := runDirFromContext(ctx)
	if runDir == "" {
		return nil, fmt.Errorf("tool_output.read requires an active run context")
	}
	rawPath := mapStringArg(args, "raw_output_path")
	absPath, relPath, err := resolveRawOutputArtifact(runDir, rawPath)
	if err != nil {
		return nil, err
	}
	stream := strings.TrimSpace(strings.ToLower(mapStringArg(args, "stream")))
	if stream != "stdout" && stream != "stderr" {
		return nil, fmt.Errorf("stream must be stdout or stderr")
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode raw output artifact: %w", err)
	}
	text, _ := raw[stream].(string)
	limit := boundedToolOutputLimit(args)
	tailLines := 0
	if rawTail, ok := numberArg(args, "tail_lines"); ok && rawTail > 0 {
		tailLines = rawTail
	}
	offset := 0
	if rawOffset, ok := numberArg(args, "offset_bytes"); ok && rawOffset > 0 {
		offset = rawOffset
	}
	content, start, next, truncated, mode := selectToolOutputContent(text, offset, limit, tailLines)
	out := map[string]any{
		"status":               "ok",
		"raw_output_path":      relPath,
		"tool":                 raw["tool"],
		"stream":               stream,
		"mode":                 mode,
		"content":              content,
		"content_offset_bytes": start,
		"stream_bytes":         len([]byte(text)),
		"returned_bytes":       len([]byte(content)),
		"limit_bytes":          limit,
		"truncated":            truncated,
		"stdout_bytes":         raw["stdout_bytes"],
		"stderr_bytes":         raw["stderr_bytes"],
		"exit_code":            raw["exit_code"],
		"duration_ms":          raw["duration_ms"],
	}
	if tailLines > 0 {
		out["tail_lines"] = tailLines
	}
	if truncated {
		out["next_offset_bytes"] = next
	}
	if value := raw["program"]; value != nil {
		out["program"] = value
	}
	if value := raw["command"]; value != nil {
		out["command"] = value
	}
	if value := raw["error"]; value != nil {
		out["error"] = value
	}
	return out, nil
}

func resolveRawOutputArtifact(runDir, rawPath string) (string, string, error) {
	runDir = filepath.Clean(runDir)
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return "", "", fmt.Errorf("raw_output_path is required")
	}
	if filepath.IsAbs(rawPath) {
		return "", "", fmt.Errorf("raw_output_path must be relative to the current run")
	}
	cleanRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rawPath)))
	if cleanRel == "." || strings.HasPrefix(cleanRel, "../") || cleanRel == ".." {
		return "", "", fmt.Errorf("raw_output_path must stay within the current run")
	}
	if cleanRel != "artifacts/tool-outputs" && !strings.HasPrefix(cleanRel, "artifacts/tool-outputs/") {
		return "", "", fmt.Errorf("raw_output_path must point to artifacts/tool-outputs")
	}
	if filepath.Ext(cleanRel) != ".json" {
		return "", "", fmt.Errorf("raw_output_path must point to a json artifact")
	}
	abs := filepath.Clean(filepath.Join(runDir, filepath.FromSlash(cleanRel)))
	rel, err := filepath.Rel(runDir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("raw_output_path must stay within the current run")
	}
	return abs, filepath.ToSlash(rel), nil
}

func boundedToolOutputLimit(args map[string]any) int {
	limit := defaultToolOutputReadLimit
	if raw, ok := numberArg(args, "limit_bytes"); ok && raw > 0 {
		limit = raw
	}
	if limit > maxToolOutputReadLimit {
		return maxToolOutputReadLimit
	}
	return limit
}

func selectToolOutputContent(text string, offset, limit, tailLines int) (string, int, int, bool, string) {
	data := []byte(text)
	if tailLines > 0 {
		tail := tailTextLines(text, tailLines)
		tailData := []byte(tail)
		if len(tailData) > limit {
			start := len(tailData) - limit
			tail = string(tailData[start:])
			tailData = []byte(tail)
		}
		contentStart := len(data) - len(tailData)
		return tail, contentStart, len(data), contentStart > 0, "tail"
	}
	if offset > len(data) {
		offset = len(data)
	}
	end := offset + limit
	if end > len(data) {
		end = len(data)
	}
	return string(data[offset:end]), offset, end, end < len(data), "slice"
}

func tailTextLines(text string, lines int) string {
	if lines <= 0 || text == "" {
		return ""
	}
	parts := strings.SplitAfter(text, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if lines >= len(parts) {
		return text
	}
	return strings.Join(parts[len(parts)-lines:], "")
}
