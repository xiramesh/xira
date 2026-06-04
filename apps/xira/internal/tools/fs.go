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
}

type ReadFileTool struct{ fileTool }
type WriteFileTool struct{ fileTool }
type ListDirTool struct{ fileTool }
type EditFileTool struct{ fileTool }

func NewReadFileTool(workspaceRoot string) *ReadFileTool {
	return &ReadFileTool{fileTool{workspaceRoot: cleanWorkspace(workspaceRoot)}}
}

func NewWriteFileTool(workspaceRoot string) *WriteFileTool {
	return &WriteFileTool{fileTool{workspaceRoot: cleanWorkspace(workspaceRoot)}}
}

func NewListDirTool(workspaceRoot string) *ListDirTool {
	return &ListDirTool{fileTool{workspaceRoot: cleanWorkspace(workspaceRoot)}}
}

func NewEditFileTool(workspaceRoot string) *EditFileTool {
	return &EditFileTool{fileTool{workspaceRoot: cleanWorkspace(workspaceRoot)}}
}

func (t *ReadFileTool) Name() string { return "read_file" }
func (t *ReadFileTool) Description() string {
	return "Read a UTF-8 text file from the Xira workspace."
}
func (t *ReadFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Path to read, relative to the workspace unless absolute."},
		},
		"required": []string{"path"},
	}
}
func (t *ReadFileTool) Execute(_ context.Context, args map[string]any) (map[string]any, error) {
	path, err := t.resolveArgPath(args)
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
	return "Create or overwrite a UTF-8 text file in the Xira workspace."
}
func (t *WriteFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Path to write, relative to the workspace unless absolute."},
			"content": map[string]any{"type": "string", "description": "File content to write."},
		},
		"required": []string{"path", "content"},
	}
}
func (t *WriteFileTool) Execute(_ context.Context, args map[string]any) (map[string]any, error) {
	path, err := t.resolveArgPath(args)
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
	return "List files and directories in the Xira workspace."
}
func (t *ListDirTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory path, relative to the workspace unless absolute. Defaults to workspace root."},
		},
	}
}
func (t *ListDirTool) Execute(_ context.Context, args map[string]any) (map[string]any, error) {
	rawPath, _ := args["path"].(string)
	if strings.TrimSpace(rawPath) == "" {
		rawPath = "."
	}
	path, err := t.resolvePath(rawPath)
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
	return "Replace one exact text occurrence in an existing workspace file."
}
func (t *EditFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":     map[string]any{"type": "string", "description": "Path to edit, relative to the workspace unless absolute."},
			"old_text": map[string]any{"type": "string", "description": "Exact existing text to replace."},
			"new_text": map[string]any{"type": "string", "description": "Replacement text."},
		},
		"required": []string{"path", "old_text", "new_text"},
	}
}
func (t *EditFileTool) Execute(_ context.Context, args map[string]any) (map[string]any, error) {
	path, err := t.resolveArgPath(args)
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

func (t fileTool) resolveArgPath(args map[string]any) (string, error) {
	rawPath, ok := args["path"].(string)
	if !ok || strings.TrimSpace(rawPath) == "" {
		return "", fmt.Errorf("path is required")
	}
	return t.resolvePath(rawPath)
}

func (t fileTool) resolvePath(rawPath string) (string, error) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(rawPath) {
		return filepath.Clean(rawPath), nil
	}
	return filepath.Clean(filepath.Join(t.workspaceRoot, rawPath)), nil
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
