package flow

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xiramesh/xira/internal/fileutil"
)

// flowDefinitionFileName is the canonical name of a flow definition file within
// a flow directory. Matching is case-insensitive via fileutil.FindFileEqualFold
// to mirror PROFILE.md discovery, so it does not depend on the host
// filesystem's case-sensitivity rules.
const flowDefinitionFileName = "flow.yaml"

// FlowRef is a lightweight, serializable reference to a discovered flow. It is
// safe to expose via JSON to the CLI and HTTP API. The Description and Name
// fields are read directly from the flow definition file — it is the single
// source of truth; there is no config-file override.
type FlowRef struct {
	ID          string `json:"id" yaml:"id"`
	Path        string `json:"path" yaml:"path"`
	Name        string `json:"name,omitempty" yaml:"name,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// FlowRegistry implements DefinitionSource by discovering flow files under
// <workspace>/flows/<id>/flow.yaml. It mirrors agents.LoadFromWorkspace so that
// flows and agents share the same workspace-discovery model: no registration,
// directory name is the id, and the definition file name is matched
// case-insensitively.
type FlowRegistry struct {
	refs []FlowRef
	byID map[string]FlowRef
	defs map[string]*Definition
}

// LoadFromWorkspace discovers flows under <workspaceRoot>/flows/. A missing or
// empty flows directory returns an empty registry (NOT an error), because not
// every workspace has flows. Each <id>/ subdirectory may contain a flow.yaml
// (case-insensitive) whose Definition.ID must equal the directory name, exactly
// like agents PROFILE.md. Directories without a flow file are skipped; a flow
// file with a non-matching id, an ambiguous case variant, or an invalid
// definition is a hard error.
func LoadFromWorkspace(workspaceRoot string) (*FlowRegistry, error) {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		return nil, fmt.Errorf("workspace root is required")
	}
	flowsRoot := filepath.Join(workspaceRoot, "flows")
	entries, err := os.ReadDir(flowsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return newEmptyFlowRegistry(), nil
		}
		return nil, fmt.Errorf("read flows directory %s: %w", flowsRoot, err)
	}

	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(flowsRoot, entry.Name()))
		}
	}
	sort.Strings(dirs)

	reg := &FlowRegistry{
		byID: map[string]FlowRef{},
		defs: map[string]*Definition{},
	}
	for _, dir := range dirs {
		path, err := fileutil.FindFileEqualFold(dir, flowDefinitionFileName)
		if err != nil {
			if fileutil.IsNotExist(err) {
				continue // no flow file in this dir; skip (mirrors agents loader)
			}
			return nil, err // ambiguous case variants and read errors propagate
		}
		def, err := LoadDefinition(path)
		if err != nil {
			return nil, fmt.Errorf("load flow definition %s: %w", path, err)
		}
		dirID := filepath.Base(dir)
		if def.ID != dirID {
			return nil, fmt.Errorf("flow %s id %q must match directory %q", flowDefinitionFileName, def.ID, dirID)
		}
		ref := FlowRef{
			ID:          def.ID,
			Path:        path,
			Name:        def.Name,
			Description: def.Description,
		}
		reg.refs = append(reg.refs, ref)
		reg.byID[def.ID] = ref
		reg.defs[def.ID] = def
	}
	return reg, nil
}

func newEmptyFlowRegistry() *FlowRegistry {
	return &FlowRegistry{byID: map[string]FlowRef{}, defs: map[string]*Definition{}}
}

// Definition implements DefinitionSource so the flow kernel can resolve a
// flow by id via Kernel.Definitions without an explicit file path.
func (r *FlowRegistry) Definition(flowID string) (*Definition, error) {
	if r == nil {
		return nil, fmt.Errorf("flow %q not found", flowID)
	}
	def, ok := r.defs[strings.TrimSpace(flowID)]
	if !ok {
		return nil, fmt.Errorf("flow %q not found", flowID)
	}
	return def, nil
}

// Find returns the FlowRef for id and whether it was present.
func (r *FlowRegistry) Find(id string) (FlowRef, bool) {
	if r == nil {
		return FlowRef{}, false
	}
	ref, ok := r.byID[strings.TrimSpace(id)]
	return ref, ok
}

// List returns a stable, sorted copy of all discovered flow refs.
func (r *FlowRegistry) List() []FlowRef {
	if r == nil {
		return nil
	}
	out := make([]FlowRef, len(r.refs))
	copy(out, r.refs)
	return out
}
