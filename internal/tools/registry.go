package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any
	Execute(context.Context, map[string]any) (map[string]any, error)
}

type Registry struct {
	tools map[string]Tool
}

type Definition struct {
	Name        string
	Description string
	Parameters  map[string]any
}

func NewRegistry(tools []Tool) *Registry {
	registry := &Registry{tools: map[string]Tool{}}
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		name := strings.TrimSpace(tool.Name())
		if name == "" {
			continue
		}
		registry.tools[name] = tool
	}
	return registry
}

func NewBuiltinRegistry(workspaceRoot string, allowed []string) *Registry {
	all := map[string]Tool{
		"exec":       NewExecTool(workspaceRoot),
		"read_file":  NewReadFileTool(workspaceRoot),
		"write_file": NewWriteFileTool(workspaceRoot),
		"list_dir":   NewListDirTool(workspaceRoot),
		"edit_file":  NewEditFileTool(workspaceRoot),
	}
	tools := make([]Tool, 0, len(allowed))
	for _, name := range allowed {
		name = strings.TrimSpace(name)
		if tool, ok := all[name]; ok {
			tools = append(tools, tool)
		}
	}
	return NewRegistry(tools)
}

func (r *Registry) Has(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.tools[strings.TrimSpace(name)]
	return ok
}

func (r *Registry) Get(name string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	tool, ok := r.tools[strings.TrimSpace(name)]
	return tool, ok
}

func (r *Registry) List() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) Definitions() []Definition {
	names := r.List()
	out := make([]Definition, 0, len(names))
	for _, name := range names {
		tool := r.tools[name]
		out = append(out, Definition{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  tool.Parameters(),
		})
	}
	return out
}

func (r *Registry) Execute(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	tool, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("tool %q not found", name)
	}
	if args == nil {
		args = map[string]any{}
	}
	return tool.Execute(ctx, args)
}
