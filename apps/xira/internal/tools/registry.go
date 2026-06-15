package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
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
	Name         string
	Description  string
	Parameters   map[string]any
	InputSchema  *jsonschema.Schema
	OutputSchema *jsonschema.Schema
	Policy       ToolPolicy
}

type ToolPolicy struct {
	Risk                string
	RequireConfirmation bool
}

type PolicyProvider interface {
	Policy() ToolPolicy
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
		"command.run":      NewCommandRunTool(workspaceRoot),
		"shell.run":        NewShellRunTool(workspaceRoot),
		"tool_output.read": NewToolOutputReadTool(),
		"read_file":        NewReadFileTool(workspaceRoot),
		"search_file":      NewSearchFileTool(workspaceRoot),
		"write_file":       NewWriteFileTool(workspaceRoot),
		"list_dir":         NewListDirTool(workspaceRoot),
		"edit_file":        NewEditFileTool(workspaceRoot),
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
		parameters := tool.Parameters()
		policy := ToolPolicy{}
		if provider, ok := tool.(PolicyProvider); ok {
			policy = provider.Policy()
		}
		out = append(out, Definition{
			Name:         tool.Name(),
			Description:  tool.Description(),
			Parameters:   parameters,
			InputSchema:  schemaFromMap(parameters),
			OutputSchema: &jsonschema.Schema{Type: "object"},
			Policy:       policy,
		})
	}
	return out
}

func (r *Registry) GetDefinition(name string) (Definition, bool) {
	tool, ok := r.Get(name)
	if !ok {
		return Definition{}, false
	}
	parameters := tool.Parameters()
	policy := ToolPolicy{}
	if provider, ok := tool.(PolicyProvider); ok {
		policy = provider.Policy()
	}
	return Definition{
		Name:         tool.Name(),
		Description:  tool.Description(),
		Parameters:   parameters,
		InputSchema:  schemaFromMap(parameters),
		OutputSchema: &jsonschema.Schema{Type: "object"},
		Policy:       policy,
	}, true
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

func schemaFromMap(value map[string]any) *jsonschema.Schema {
	if len(value) == 0 {
		return &jsonschema.Schema{Type: "object"}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return &jsonschema.Schema{Type: "object"}
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		return &jsonschema.Schema{Type: "object"}
	}
	return &schema
}
