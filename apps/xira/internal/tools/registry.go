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
	Risk string
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

// SandboxRoots carries the out-of-workspace roots an agent is authorized to
// reach. Values must already be expanded to absolute paths (see ExpandRoots).
// AllowRoots are read/write; ReadonlyRoots are read-only.
type SandboxRoots struct {
	AllowRoots    []string
	ReadonlyRoots []string
}

func NewBuiltinRegistry(workspaceRoot string, allowed []string, roots SandboxRoots, stateDir string) *Registry {
	ws := cleanWorkspace(workspaceRoot)
	readRoots := mergeRoots([]string{ws}, roots.AllowRoots, roots.ReadonlyRoots)
	writeRoots := mergeRoots([]string{ws}, roots.AllowRoots)
	all := map[string]Tool{
		"command.run":      NewCommandRunTool(ws, writeRoots),
		"shell.run":        NewShellRunTool(ws, writeRoots),
		"tool_output.read": NewToolOutputReadTool(),
		"read_file":        NewReadFileTool(ws, readRoots, writeRoots),
		"search_file":      NewSearchFileTool(ws, readRoots, writeRoots),
		"write_file":       NewWriteFileTool(ws, readRoots, writeRoots),
		"list_dir":         NewListDirTool(ws, readRoots, writeRoots),
		"edit_file":        NewEditFileTool(ws, readRoots, writeRoots),
		"update_profile":   NewUpdateProfileTool(stateDir), // #127: user.md 在 stateDir（非 workspace），通用工具不可达
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

func SchemaFromMap(value map[string]any) *jsonschema.Schema {
	return schemaFromMap(value)
}
