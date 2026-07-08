package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type fileTool struct {
	workspaceRoot string
	// readRoots is the set of absolute roots readable via read_file/list_dir/
	// search_file: the workspace plus any allow_roots and readonly_roots.
	readRoots []string
	// writeRoots is the set writable via write_file/edit_file: the workspace
	// plus any allow_roots. readonly_roots are intentionally excluded.
	writeRoots []string
}

type ReadFileTool struct{ fileTool }
type WriteFileTool struct{ fileTool }
type ListDirTool struct{ fileTool }
type EditFileTool struct{ fileTool }

func newFileTool(workspaceRoot string, readRoots, writeRoots []string) fileTool {
	return fileTool{
		workspaceRoot: cleanWorkspace(workspaceRoot),
		readRoots:     readRoots,
		writeRoots:    writeRoots,
	}
}

func NewReadFileTool(workspaceRoot string, readRoots, writeRoots []string) *ReadFileTool {
	return &ReadFileTool{newFileTool(workspaceRoot, readRoots, writeRoots)}
}

func NewWriteFileTool(workspaceRoot string, readRoots, writeRoots []string) *WriteFileTool {
	return &WriteFileTool{newFileTool(workspaceRoot, readRoots, writeRoots)}
}

func NewListDirTool(workspaceRoot string, readRoots, writeRoots []string) *ListDirTool {
	return &ListDirTool{newFileTool(workspaceRoot, readRoots, writeRoots)}
}

func NewEditFileTool(workspaceRoot string, readRoots, writeRoots []string) *EditFileTool {
	return &EditFileTool{newFileTool(workspaceRoot, readRoots, writeRoots)}
}

func (t *ReadFileTool) Name() string { return "read_file" }
func (t *ReadFileTool) Description() string {
	return "Read a UTF-8 text file from the Xira workspace or configured sandbox roots."
}
func (t *ReadFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Path within the workspace or configured sandbox roots. Defaults to the workspace for relative paths."},
		},
		"required": []string{"path"},
	}
}
func (t *ReadFileTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	path, err := t.resolveReadArgPathCtx(ctx, args)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path":    path,
		"content": string(data),
		"bytes":   len(data),
	}, nil
}

func (t *WriteFileTool) Name() string { return "write_file" }
func (t *WriteFileTool) Description() string {
	return "Create or overwrite a UTF-8 text file in the Xira workspace or configured sandbox roots."
}
func (t *WriteFileTool) Policy() ToolPolicy {
	// #110: write_file no longer gates on RequireConfirmation. allow_roots is a
	// hard boundary — in-bound writes execute directly (Codex workspace-write /
	// Aider model), out-of-bound writes are rejected by the sandbox
	// (resolveWriteArgPath → pathWithinRoots, "path must stay within allowed
	// roots"). gate was redundant double-protection that only triggered inside
	// the boundary (where writes should pass) and deadlocked IM channels
	// (runtime_tool_gate has no IM resolve path). Risk stays "high" for audit.
	return ToolPolicy{Risk: "high"}
}
func (t *WriteFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Path within the workspace or configured sandbox roots. Defaults to the workspace for relative paths."},
			"content": map[string]any{"type": "string", "description": "File content to write."},
		},
		"required": []string{"path", "content"},
	}
}
func (t *WriteFileTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	path, err := t.resolveWriteArgPathCtx(ctx, args)
	if err != nil {
		return nil, err
	}
	content, ok := args["content"].(string)
	if !ok {
		return nil, fmt.Errorf("content is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "bytes": len(content)}, nil
}

func (t *ListDirTool) Name() string { return "list_dir" }
func (t *ListDirTool) Description() string {
	return "List files and directories in the Xira workspace or configured sandbox roots."
}
func (t *ListDirTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory path within the workspace or configured sandbox roots. Defaults to workspace root."},
		},
	}
}
func (t *ListDirTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	rawPath, _ := args["path"].(string)
	if strings.TrimSpace(rawPath) == "" {
		rawPath = "."
	}
	path, err := t.resolveReadPathCtx(ctx, rawPath)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		kind := "file"
		if entry.IsDir() {
			kind = "dir"
		}
		items = append(items, map[string]any{
			"name": entry.Name(),
			"type": kind,
			"size": info.Size(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		left := fmt.Sprint(items[i]["type"]) + ":" + fmt.Sprint(items[i]["name"])
		right := fmt.Sprint(items[j]["type"]) + ":" + fmt.Sprint(items[j]["name"])
		return left < right
	})
	return map[string]any{"path": path, "entries": items}, nil
}

func (t *EditFileTool) Name() string { return "edit_file" }
func (t *EditFileTool) Description() string {
	return "Replace one exact text occurrence in an existing file within the workspace or configured sandbox roots."
}
func (t *EditFileTool) Policy() ToolPolicy {
	// #110: see WriteFileTool.Policy — edit_file no longer gates. allow_roots
	// boundary is the protection; gate was redundant and deadlocked IM.
	return ToolPolicy{Risk: "high"}
}
func (t *EditFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":     map[string]any{"type": "string", "description": "Path within the workspace or configured sandbox roots. Defaults to the workspace for relative paths."},
			"old_text": map[string]any{"type": "string", "description": "Exact existing text to replace."},
			"new_text": map[string]any{"type": "string", "description": "Replacement text."},
		},
		"required": []string{"path", "old_text", "new_text"},
	}
}
func (t *EditFileTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	path, err := t.resolveWriteArgPathCtx(ctx, args)
	if err != nil {
		return nil, err
	}
	oldText, ok := args["old_text"].(string)
	if !ok || oldText == "" {
		return nil, fmt.Errorf("old_text is required")
	}
	newText, ok := args["new_text"].(string)
	if !ok {
		return nil, fmt.Errorf("new_text is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(data)
	count := strings.Count(content, oldText)
	if count == 0 {
		return nil, fmt.Errorf("old_text not found")
	}
	if count > 1 {
		return nil, fmt.Errorf("old_text occurs %d times; provide a unique edit", count)
	}
	updated := strings.Replace(content, oldText, newText, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return nil, err
	}
	return map[string]any{
		"path":         path,
		"replacements": 1,
		"bytes":        len(updated),
	}, nil
}

// resolveReadArgPathCtx 是 #126 overlay 版：从 ctx 取 sender，走 overlay 解析。
func (t fileTool) resolveReadArgPathCtx(ctx context.Context, args map[string]any) (string, error) {
	rawPath, ok := args["path"].(string)
	if !ok || strings.TrimSpace(rawPath) == "" {
		return "", fmt.Errorf("path is required")
	}
	return resolveRead(rawPath, t.workspaceRoot, senderIDFromCtx(ctx), t.readRoots)
}

// resolveReadPathCtx 是 #126 overlay 版（直接传 rawPath，供 list_dir 用）。
func (t fileTool) resolveReadPathCtx(ctx context.Context, rawPath string) (string, error) {
	return resolveRead(rawPath, t.workspaceRoot, senderIDFromCtx(ctx), t.readRoots)
}

// resolveWriteArgPathCtx 是 #126 overlay 版：从 ctx 取 sender，走 overlay 解析。
func (t fileTool) resolveWriteArgPathCtx(ctx context.Context, args map[string]any) (string, error) {
	rawPath, ok := args["path"].(string)
	if !ok || strings.TrimSpace(rawPath) == "" {
		return "", fmt.Errorf("path is required")
	}
	return resolveWrite(rawPath, t.workspaceRoot, senderIDFromCtx(ctx), t.writeRoots)
}

func cleanWorkspace(workspaceRoot string) string {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		workspaceRoot = "."
	}
	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return filepath.Clean(workspaceRoot)
	}
	return abs
}
