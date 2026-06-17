# Xira Flow Registry And Session History Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Flow usable as a workspace-level feature with multiple named Flow configuration files, and make Flow chat/session history part of the durable evidence chain.

**Architecture:** Keep v0's existing path-based Flow execution, but add a thin registry layer that discovers declared Flow files from `xira.yaml` / workspace defaults and resolves a `flow_id` to a concrete `flow.yaml`. Preserve explicit `flow_path` as the lowest-level escape hatch. For session history, do not invent a new transcript store; reuse the existing runtime session manager and make Flow tests/docs assert where `messages.jsonl` is written.

**Tech Stack:** Go, Cobra CLI, net/http API handlers, YAML via `gopkg.in/yaml.v3`, existing `internal/flow`, `internal/runtime`, and `internal/session` packages.

---

## Current State

Flow currently works by explicit file path:

- CLI: `xira flow run <flow-file> --entrypoint <entrypoint-id> --input key=value`
- API: `POST /api/v1/flows/runs` with `flow_path`
- Runtime: `flow.Kernel.resolveDefinition` loads `StartRequest.FlowPath` first, then falls back to in-memory definitions by `FlowID`

This is technically usable, but not product-grade for a workspace with many flows. A user has to know exact paths.

Session history already exists for normal agent runs:

- Runtime appends messages in `apps/xira/internal/runtime/service.go`
- Store path is managed by `apps/xira/internal/session`
- Messages land under `.xira/sessions/<channel>/<entrypoint>/<conversation-dir>/agents/<agent-id>/messages.jsonl`

Flow uses runtime agent turns through `apps/xira/internal/runtime/flow_bridge.go`, but Flow tests and docs mostly focus on run records, events, tool calls, human requests, and artifacts. They do not strongly assert Flow chat transcript persistence.

## Desired User Experience

Workspace layout:

```text
workspace/
  agents/
  skills/
  flows/
    devrun/
      flow.yaml
      context/
      verification/
    release-review/
      flow.yaml
    incident-debug/
      flow.yaml
```

Runtime config:

```yaml
workspace: workspace
state_root: .xira/state
run_root: .xira/runs
session_root: .xira/sessions
flows:
  - id: devrun
    path: flows/devrun/flow.yaml
  - id: release-review
    path: flows/release-review/flow.yaml
```

CLI:

```bash
xira flow run devrun --entrypoint ad_hoc --input repo=/Users/yinwm/work/flowdeck --input request="fix this"
xira flow run --path workspace/flows/devrun/flow.yaml --entrypoint ad_hoc --input repo=/Users/yinwm/work/flowdeck --input request="fix this"
xira flow list
xira flow inspect devrun
```

API:

```json
{
  "flow_id": "devrun",
  "entrypoint_id": "ad_hoc",
  "input": {
    "repo": "/Users/yinwm/work/flowdeck",
    "request": "fix this"
  }
}
```

Durable evidence should include both:

```text
.xira/state/flow-runs/<flow-run-id>/flow_run.yaml
.xira/sessions/flow/<entrypoint-id>/<conversation-dir>/agents/<agent-id>/messages.jsonl
```

## File Structure

Modify:

- `apps/xira/internal/runtime/config.go`
  - Parse optional `flows:` list from `xira.yaml`.
  - Resolve flow paths relative to the runtime config base directory and workspace.

- `apps/xira/internal/runtime/types.go`
  - Add runtime config / status structs or fields for flow registry, if needed.

- `apps/xira/internal/runtime/service.go`
  - Own a Flow registry on `Service`.
  - Expose registry data to `StartFlow`, status, and API/CLI callers.

- `apps/xira/internal/runtime/flow_bridge.go`
  - Keep existing runtime bridge behavior.
  - Add no new transcript mechanism; this file should only be touched if the current Flow `TurnRequest` does not reliably set `Channel`, `EntrypointID`, `SessionID`, or metadata needed for durable session paths.

- `apps/xira/internal/flow/kernel.go`
  - Keep path loading as-is.
  - Add or use a `DefinitionSource` implementation for registry lookup by `flow_id`.

- `apps/xira/internal/api/server.go`
  - Accept `flow_id` in `POST /api/v1/flows/runs`.
  - Keep `flow_path`.
  - Add optional list/inspect endpoints only if they can be small and fully tested.

- `apps/xira/cmd/xira/main.go`
  - Let `xira flow run` accept a flow id as positional arg.
  - Add `--path` for explicit path mode.
  - Add `xira flow list` and `xira flow inspect <flow-id>` if registry exists.

- `docs/guide/xira-flow-v0-usage.zh.md`
  - Document workspace `flows/` layout.
  - Explain `repo` as business input, not runtime input.
  - Document chat/session history location.

Create:

- `apps/xira/internal/runtime/flow_registry.go`
  - Small registry type for declared flows and default workspace discovery.

- `apps/xira/internal/runtime/flow_registry_test.go`
  - Unit tests for config parsing, relative path resolution, duplicate ids, missing files, and default discovery.

Test:

- `apps/xira/internal/api/flow_test.go`
  - API start by `flow_id`.

- `apps/xira/cmd/xira/main_flow_test.go`
  - CLI start by id and explicit `--path`.

- `apps/xira/internal/runtime/service_test.go`
  - Flow agent step persists `messages.jsonl` under the expected session path.

- `apps/xira/internal/runtime/deepseek_hitl_live_test.go`
  - Add assertions to the existing real DeepSeek Flow live test that session history files exist for relevant agent runs.

---

### Task 1: Add Flow Registry Data Model And Config Parsing

**Files:**
- Create: `apps/xira/internal/runtime/flow_registry.go`
- Create: `apps/xira/internal/runtime/flow_registry_test.go`
- Modify: `apps/xira/internal/runtime/config.go`

- [ ] **Step 1: Write failing tests for configured flows**

Add this test to `apps/xira/internal/runtime/flow_registry_test.go`:

```go
package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRuntimeConfigLoadsConfiguredFlows(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	mustWriteFlowFile(t, filepath.Join(workspace, "flows", "devrun", "flow.yaml"), "devrun")
	mustWriteFlowFile(t, filepath.Join(workspace, "flows", "release-review", "flow.yaml"), "release-review")
	mustWriteFile(t, filepath.Join(root, "xira.yaml"), `
workspace: workspace
state_root: .xira/state
flows:
  - id: devrun
    path: flows/devrun/flow.yaml
  - id: release-review
    path: flows/release-review/flow.yaml
`)

	t.Chdir(root)
	resolved, err := resolveRuntimeConfig(Config{})
	if err != nil {
		t.Fatalf("resolveRuntimeConfig: %v", err)
	}
	if len(resolved.Flows) != 2 {
		t.Fatalf("flows len = %d, want 2: %+v", len(resolved.Flows), resolved.Flows)
	}
	if resolved.Flows[0].ID != "devrun" {
		t.Fatalf("first flow id = %q", resolved.Flows[0].ID)
	}
	if resolved.Flows[0].Path != filepath.Join(workspace, "flows", "devrun", "flow.yaml") {
		t.Fatalf("first flow path = %q", resolved.Flows[0].Path)
	}
}

func TestResolveRuntimeConfigRejectsDuplicateFlowIDs(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	mustWriteFlowFile(t, filepath.Join(workspace, "flows", "devrun", "flow.yaml"), "devrun")
	mustWriteFile(t, filepath.Join(root, "xira.yaml"), `
workspace: workspace
flows:
  - id: devrun
    path: flows/devrun/flow.yaml
  - id: devrun
    path: flows/devrun/flow.yaml
`)

	t.Chdir(root)
	_, err := resolveRuntimeConfig(Config{})
	if err == nil {
		t.Fatal("expected duplicate flow id error")
	}
}

func mustWriteFlowFile(t *testing.T, path, id string) {
	t.Helper()
	mustWriteFile(t, path, "schema_version: xira.flow.v0\nid: "+id+"\nentrypoints:\n  - id: ad_hoc\n    start_step: answer\nsteps:\n  - id: answer\n    objective: Answer.\n    executor:\n      agent: xira-assistant\n")
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestResolveRuntimeConfig.*Flow' -v
```

Expected: compile failure because `resolved.Flows` and config `flows` are not implemented.

- [ ] **Step 3: Add registry types**

Create `apps/xira/internal/runtime/flow_registry.go`:

```go
package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type FlowRef struct {
	ID          string `json:"id" yaml:"id"`
	Path        string `json:"path" yaml:"path"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

type FlowRegistry struct {
	flows []FlowRef
	byID  map[string]FlowRef
}

func newFlowRegistry(flows []FlowRef) (*FlowRegistry, error) {
	reg := &FlowRegistry{byID: map[string]FlowRef{}}
	for _, flow := range flows {
		flow.ID = strings.TrimSpace(flow.ID)
		flow.Path = strings.TrimSpace(flow.Path)
		flow.Description = strings.TrimSpace(flow.Description)
		if flow.ID == "" {
			return nil, fmt.Errorf("flow id is required")
		}
		if flow.Path == "" {
			return nil, fmt.Errorf("flow %q path is required", flow.ID)
		}
		if _, ok := reg.byID[flow.ID]; ok {
			return nil, fmt.Errorf("duplicate flow id %q", flow.ID)
		}
		if _, err := os.Stat(flow.Path); err != nil {
			return nil, fmt.Errorf("flow %q path %s: %w", flow.ID, flow.Path, err)
		}
		reg.flows = append(reg.flows, flow)
		reg.byID[flow.ID] = flow
	}
	return reg, nil
}

func discoverWorkspaceFlows(workspace string) []FlowRef {
	root := filepath.Join(strings.TrimSpace(workspace), "flows")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var flows []FlowRef
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		path := filepath.Join(root, id, "flow.yaml")
		if _, err := os.Stat(path); err == nil {
			flows = append(flows, FlowRef{ID: id, Path: path})
		}
	}
	return flows
}

func (r *FlowRegistry) List() []FlowRef {
	if r == nil {
		return nil
	}
	out := make([]FlowRef, len(r.flows))
	copy(out, r.flows)
	return out
}

func (r *FlowRegistry) Find(id string) (FlowRef, bool) {
	if r == nil {
		return FlowRef{}, false
	}
	flow, ok := r.byID[strings.TrimSpace(id)]
	return flow, ok
}
```

- [ ] **Step 4: Parse `flows:` in runtime config**

Modify `apps/xira/internal/runtime/config.go`:

```go
type runtimeConfigFile struct {
	Workspace      string       `yaml:"workspace"`
	DefaultAgentID string       `yaml:"default_agent"`
	RunRoot        string       `yaml:"run_root"`
	SessionRoot    string       `yaml:"session_root"`
	StateRoot      string       `yaml:"state_root"`
	Entrypoints    string       `yaml:"entrypoints"`
	Pricing        UsagePricing `yaml:"pricing"`
	Flows          []FlowRef    `yaml:"flows"`
}

type resolvedRuntimeConfig struct {
	ConfigPath        string
	ConfigLoaded      bool
	WorkspaceExplicit bool
	WorkspaceRoot     string
	DefaultAgentID    string
	RunRoot           string
	SessionRoot       string
	StateRoot         string
	Pricing           UsagePricing
	Entrypoints       []entrypoints.Definition
	Flows             []FlowRef
}
```

Inside `resolveRuntimeConfig`, after `workspace` is resolved:

```go
flows := resolveConfiguredFlows(baseDir, workspace, configFile.Flows)
if len(flows) == 0 && configLoaded {
	flows = discoverWorkspaceFlows(workspace)
}
if _, err := newFlowRegistry(flows); err != nil {
	return resolvedRuntimeConfig{}, err
}
```

Return `Flows: flows`.

Add helper:

```go
func resolveConfiguredFlows(baseDir, workspace string, flows []FlowRef) []FlowRef {
	out := make([]FlowRef, 0, len(flows))
	for _, flow := range flows {
		flow.ID = strings.TrimSpace(flow.ID)
		flow.Path = strings.TrimSpace(flow.Path)
		flow.Description = strings.TrimSpace(flow.Description)
		if flow.Path != "" && !filepath.IsAbs(flow.Path) {
			candidate := filepath.Join(workspace, flow.Path)
			if _, err := os.Stat(candidate); err == nil {
				flow.Path = candidate
			} else {
				flow.Path = resolveRelativePath(baseDir, flow.Path)
			}
		}
		out = append(out, flow)
	}
	return out
}
```

- [ ] **Step 5: Run tests and commit**

Run:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestResolveRuntimeConfig.*Flow' -v
```

Expected: PASS.

Commit:

```bash
git add apps/xira/internal/runtime/config.go apps/xira/internal/runtime/flow_registry.go apps/xira/internal/runtime/flow_registry_test.go
git commit -m "feat: load flow registry from config"
```

---

### Task 2: Wire Registry Into Runtime Flow Start

**Files:**
- Modify: `apps/xira/internal/runtime/service.go`
- Modify: `apps/xira/internal/runtime/types.go`
- Modify: `apps/xira/internal/runtime/flow_bridge.go` only if existing `StartFlow` cannot resolve `flow_id`
- Test: `apps/xira/internal/runtime/service_test.go`

- [ ] **Step 1: Write failing runtime test for `flow_id` start**

Add to `apps/xira/internal/runtime/service_test.go`:

```go
func TestStartFlowResolvesConfiguredFlowID(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	mustWriteRuntimeFlowFile(t, filepath.Join(workspace, "flows", "hello", "flow.yaml"), "hello")
	mustWriteFile(t, filepath.Join(root, "xira.yaml"), `
workspace: workspace
state_root: .xira/state
run_root: .xira/runs
session_root: .xira/sessions
flows:
  - id: hello
    path: flows/hello/flow.yaml
`)
	t.Chdir(root)

	rt, err := NewService(Config{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer rt.Close()

	run, err := rt.StartFlow(context.Background(), FlowStartRequest{
		FlowID:       "hello",
		EntrypointID: "ad_hoc",
		Input:        map[string]string{"request": "hello"},
	})
	if err != nil {
		t.Fatalf("StartFlow: %v", err)
	}
	if run.FlowID != "hello" {
		t.Fatalf("flow id = %q, want hello", run.FlowID)
	}
}

func mustWriteRuntimeFlowFile(t *testing.T, path, id string) {
	t.Helper()
	mustWriteFile(t, path, "schema_version: xira.flow.v0\nid: "+id+"\nentrypoints:\n  - id: ad_hoc\n    start_step: answer\n    required_inputs:\n      - request\nsteps:\n  - id: answer\n    objective: Answer ${input.request}.\n    executor:\n      agent: xira-assistant\n")
}
```

If helper name collides with another test file, keep the body but rename to `mustWriteServiceFlowFile`.

- [ ] **Step 2: Run test and verify failure**

Run:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run TestStartFlowResolvesConfiguredFlowID -v
```

Expected: FAIL because runtime has parsed flows but `StartFlow` does not resolve `FlowID` through registry yet.

- [ ] **Step 3: Add registry to service**

Find `Service` struct in `apps/xira/internal/runtime/service.go` and add:

```go
flowRegistry *FlowRegistry
```

In `NewService`, after config resolution:

```go
flowRegistry, err := newFlowRegistry(resolved.Flows)
if err != nil {
	return nil, err
}
```

Assign it:

```go
flowRegistry: flowRegistry,
```

Add methods:

```go
func (s *Service) FlowRegistry() *FlowRegistry {
	if s == nil {
		return nil
	}
	return s.flowRegistry
}

func (s *Service) FlowRefs() []FlowRef {
	if s == nil || s.flowRegistry == nil {
		return nil
	}
	return s.flowRegistry.List()
}
```

- [ ] **Step 4: Resolve `FlowID` to `FlowPath` in `StartFlow`**

Find `StartFlow` in `apps/xira/internal/runtime/service.go`. Before calling kernel start, add:

```go
flowPath := strings.TrimSpace(req.FlowPath)
flowID := strings.TrimSpace(req.FlowID)
if flowPath == "" && flowID != "" {
	if ref, ok := s.flowRegistry.Find(flowID); ok {
		flowPath = ref.Path
	}
}
run, err := s.FlowKernel().Start(ctx, flow.StartRequest{
	FlowID:       flowID,
	FlowPath:     flowPath,
	EntrypointID: req.EntrypointID,
	Input:        req.Input,
})
```

Keep explicit `FlowPath` precedence: if both `flow_path` and `flow_id` are supplied, use `flow_path`.

- [ ] **Step 5: Run tests and commit**

Run:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestStartFlowResolvesConfiguredFlowID|TestResolveRuntimeConfig.*Flow' -v
```

Expected: PASS.

Commit:

```bash
git add apps/xira/internal/runtime/service.go apps/xira/internal/runtime/types.go apps/xira/internal/runtime/service_test.go
git commit -m "feat: start flows by registered id"
```

---

### Task 3: Add CLI Flow List, Inspect, And Path Mode

**Files:**
- Modify: `apps/xira/cmd/xira/main.go`
- Modify: `apps/xira/cmd/xira/main_flow_test.go`

- [ ] **Step 1: Write failing CLI tests**

Add tests to `apps/xira/cmd/xira/main_flow_test.go`:

```go
func TestFlowRunAcceptsRegisteredFlowID(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	writeFlowTestFileAt(t, filepath.Join(workspace, "flows", "hello", "flow.yaml"), "hello")
	mustWriteFile(t, filepath.Join(root, "xira.yaml"), `
workspace: workspace
state_root: .xira/state
run_root: .xira/runs
session_root: .xira/sessions
flows:
  - id: hello
    path: flows/hello/flow.yaml
`)
	t.Chdir(root)

	out := runXiraCommand(t, "flow", "run", "hello", "--entrypoint", "ad_hoc", "--input", "request=hi")
	if !strings.Contains(out, `"flow_id": "hello"`) {
		t.Fatalf("output missing flow id: %s", out)
	}
}

func TestFlowListPrintsRegisteredFlows(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	writeFlowTestFileAt(t, filepath.Join(workspace, "flows", "hello", "flow.yaml"), "hello")
	mustWriteFile(t, filepath.Join(root, "xira.yaml"), `
workspace: workspace
flows:
  - id: hello
    path: flows/hello/flow.yaml
`)
	t.Chdir(root)

	out := runXiraCommand(t, "flow", "list")
	if !strings.Contains(out, `"id": "hello"`) {
		t.Fatalf("flow list output = %s", out)
	}
}
```

Adapt helper names to the existing test harness in `main_flow_test.go`; do not create a second incompatible CLI runner.

- [ ] **Step 2: Run CLI tests and verify failure**

Run:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/cmd/xira -run 'TestFlow(RunAcceptsRegisteredFlowID|ListPrintsRegisteredFlows)' -v
```

Expected: FAIL because `flow run` currently treats the positional arg as a file path and `flow list` does not exist.

- [ ] **Step 3: Update CLI command shape**

In `apps/xira/cmd/xira/main.go`, change `flowRunCommand`:

```go
var entrypoint string
var flowPath string
var inputs []string
cmd := &cobra.Command{
	Use:   "run <flow-id>",
	Short: "Start a new flow run from a registered flow id or explicit flow path",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		rt, err := newRuntime()
		if err != nil {
			return err
		}
		defer rt.Close()
		inputMap, err := parseStringSliceFlag(inputs)
		if err != nil {
			return err
		}
		flowID := args[0]
		if flowPath == "" && strings.HasSuffix(args[0], ".yaml") {
			flowPath = args[0]
			flowID = ""
		}
		run, err := rt.StartFlow(cmd.Context(), runtime.FlowStartRequest{
			FlowID:       flowID,
			FlowPath:     flowPath,
			EntrypointID: entrypoint,
			Input:        inputMap,
		})
		if err != nil {
			return err
		}
		return printJSON(cmd, run)
	},
}
cmd.Flags().StringVar(&flowPath, "path", "", "Explicit Flow definition file path")
```

This preserves backward compatibility with `xira flow run docs/examples/flows/devrun/flow.yaml`.

- [ ] **Step 4: Add `flow list` and `flow inspect`**

In `flowCommand`, add:

```go
cmd.AddCommand(flowListCommand(newRuntime))
cmd.AddCommand(flowInspectCommand(newRuntime))
```

Add functions:

```go
func flowListCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered flows",
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			return printJSON(cmd, rt.FlowRefs())
		},
	}
}

func flowInspectCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <flow-id>",
		Short: "Show a registered flow",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			ref, ok := rt.FlowRegistry().Find(args[0])
			if !ok {
				return fmt.Errorf("flow %q not found", args[0])
			}
			return printJSON(cmd, ref)
		},
	}
}
```

- [ ] **Step 5: Run tests and commit**

Run:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/cmd/xira -run 'TestFlow' -v
```

Expected: PASS.

Commit:

```bash
git add apps/xira/cmd/xira/main.go apps/xira/cmd/xira/main_flow_test.go
git commit -m "feat: add flow registry cli"
```

---

### Task 4: Add API Support For `flow_id` And Flow Listing

**Files:**
- Modify: `apps/xira/internal/api/server.go`
- Modify: `apps/xira/internal/api/flow_test.go`

- [ ] **Step 1: Write failing API tests**

Add to `apps/xira/internal/api/flow_test.go`:

```go
func TestPostFlowRunAcceptsFlowID(t *testing.T) {
	server, root := newFlowAPITestServerWithConfig(t)
	workspace := filepath.Join(root, "workspace")
	writeFlowAPIFileAt(t, filepath.Join(workspace, "flows", "hello", "flow.yaml"), "hello")
	mustWriteFile(t, filepath.Join(root, "xira.yaml"), `
workspace: workspace
flows:
  - id: hello
    path: flows/hello/flow.yaml
`)

	resp := serveJSON(t, server, http.MethodPost, "/api/v1/flows/runs", map[string]any{
		"flow_id":       "hello",
		"entrypoint_id": "ad_hoc",
		"input": map[string]string{
			"request": "hello",
		},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"flow_id":"hello"`) {
		t.Fatalf("body missing flow id: %s", resp.Body.String())
	}
}

func TestGetFlowsListsRegisteredFlows(t *testing.T) {
	server, root := newFlowAPITestServerWithConfig(t)
	workspace := filepath.Join(root, "workspace")
	writeFlowAPIFileAt(t, filepath.Join(workspace, "flows", "hello", "flow.yaml"), "hello")
	mustWriteFile(t, filepath.Join(root, "xira.yaml"), `
workspace: workspace
flows:
  - id: hello
    path: flows/hello/flow.yaml
`)

	resp := serveJSON(t, server, http.MethodGet, "/api/v1/flows", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"id":"hello"`) {
		t.Fatalf("body missing flow id: %s", resp.Body.String())
	}
}
```

Use existing API test helpers where possible. If `newFlowAPITestServerWithConfig` does not exist, create it in the same test file and make it call the existing server constructor after `t.Chdir(root)`.

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/api -run 'Test(PostFlowRunAcceptsFlowID|GetFlowsListsRegisteredFlows)' -v
```

Expected: FAIL because `flow_id` or `/api/v1/flows` is not wired.

- [ ] **Step 3: Add request field and handler**

In `apps/xira/internal/api/server.go`, update flow start request struct:

```go
type flowRunRequest struct {
	FlowID       string            `json:"flow_id"`
	FlowPath     string            `json:"flow_path"`
	EntrypointID string            `json:"entrypoint_id"`
	Input        map[string]string `json:"input"`
}
```

Pass both fields to runtime:

```go
run, err := s.runtime.StartFlow(r.Context(), runtime.FlowStartRequest{
	FlowID:       req.FlowID,
	FlowPath:     req.FlowPath,
	EntrypointID: req.EntrypointID,
	Input:        req.Input,
})
```

Add route:

```go
mux.HandleFunc("/api/v1/flows", s.flows)
```

Add handler:

```go
func (s *Server) flows(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flows": s.runtime.FlowRefs()})
}
```

- [ ] **Step 4: Run tests and commit**

Run:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/api -run 'Test(PostFlow|GetFlow)' -v
```

Expected: PASS.

Commit:

```bash
git add apps/xira/internal/api/server.go apps/xira/internal/api/flow_test.go
git commit -m "feat: expose registered flows api"
```

---

### Task 5: Make Flow Session History A Tested Evidence Chain

**Files:**
- Modify: `apps/xira/internal/runtime/service_test.go`
- Modify: `apps/xira/internal/runtime/deepseek_hitl_live_test.go`
- Modify: `docs/review-packages/xira-flow-file-backed-live-test/FULL_DEEPSEEK_LIVE_REPORT.zh.md`

- [ ] **Step 1: Write non-live Flow session persistence test**

Add to `apps/xira/internal/runtime/service_test.go`:

```go
func TestFlowAgentStepPersistsSessionMessages(t *testing.T) {
	stateRoot := t.TempDir()
	flowPath := filepath.Join(stateRoot, "workspace", "flows", "hello", "flow.yaml")
	mustWriteRuntimeFlowFile(t, flowPath, "hello")

	rt := newTestService(t, Config{
		RunRoot:     filepath.Join(stateRoot, "runs"),
		StateRoot:   filepath.Join(stateRoot, "state"),
		SessionRoot: filepath.Join(stateRoot, "sessions"),
	})
	run, err := rt.StartFlow(context.Background(), FlowStartRequest{
		FlowPath:     flowPath,
		EntrypointID: "ad_hoc",
		Input:        map[string]string{"request": "persist flow chat"},
	})
	if err != nil {
		t.Fatalf("StartFlow: %v", err)
	}
	run, err = rt.AdvanceFlow(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("AdvanceFlow: %v", err)
	}
	var agentRunID string
	for _, step := range run.Steps {
		if step.AgentRunID != "" {
			agentRunID = step.AgentRunID
			break
		}
	}
	if agentRunID == "" {
		t.Fatalf("no agent run id in flow run: %+v", run.Steps)
	}
	agentRun, err := rt.RunStore().Load(agentRunID)
	if err != nil {
		t.Fatalf("load agent run: %v", err)
	}
	messagesPath := rt.SessionManager().AgentMessagesPath(session.AgentTurnInput{
		SessionID:      agentRun.SessionID,
		AgentID:        agentRun.AgentID,
		AgentSessionID: agentRun.AgentSessionID,
		RunID:          agentRun.RunID,
		Context: channel.InboundContext{
			Channel:      agentRun.Channel,
			EntrypointID: agentRun.EntrypointID,
			SenderID:     agentRun.UserID,
		},
		Scope: &agentRun.SessionScope,
	})
	if _, err := os.Stat(messagesPath); err != nil {
		t.Fatalf("expected flow session messages at %s: %v", messagesPath, err)
	}
	history := rt.SessionManager().AgentHistory(agentRun.SessionID, agentRun.AgentID)
	if len(history) < 2 {
		t.Fatalf("history len = %d, want user+assistant: %+v", len(history), history)
	}
}
```

Add imports if needed:

```go
import (
	"github.com/xiramesh/xira/internal/channel"
	session "github.com/xiramesh/xira/internal/session"
)
```

If this fails because `TurnResponse` does not include `AgentSessionID` or `SessionScope`, inspect `apps/xira/internal/runtime/types.go` and either use fields available in `TurnResponse`, or add the missing durable fields to `TurnResponse` with matching YAML/JSON tags.

- [ ] **Step 2: Run test and verify behavior**

Run:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run TestFlowAgentStepPersistsSessionMessages -v
```

Expected:

- If PASS: runtime already persists Flow session history; keep test as regression.
- If FAIL due missing persistence: fix `flow_bridge.go` or `service.go` so Flow turns pass stable `Channel`, `EntrypointID`, `SessionID`, and metadata into `RunAgent`.
- If FAIL only due missing exported fields: add the minimal fields to `TurnResponse` and persisted run YAML.

- [ ] **Step 3: Add live evidence assertion**

In `apps/xira/internal/runtime/deepseek_hitl_live_test.go`, add helper:

```go
func assertPersistedSessionMessagesForRun(t *testing.T, rt *Service, runID string) {
	t.Helper()
	run, err := rt.RunStore().Load(runID)
	if err != nil {
		t.Fatalf("load run %s: %v", runID, err)
	}
	history := rt.SessionManager().AgentHistory(run.SessionID, run.AgentID)
	if len(history) == 0 {
		t.Fatalf("run %s session history is empty for session=%s agent=%s", runID, run.SessionID, run.AgentID)
	}
}
```

Call it in the file-backed Flow live test for every completed `agent_run_id`:

```go
for _, step := range flowRun.Steps {
	if step.AgentRunID != "" {
		assertPersistedSessionMessagesForRun(t, rt, step.AgentRunID)
	}
}
```

Do not require a final assistant message for runs that are still `waiting_human` before approval. Assert after the full Flow has completed.

- [ ] **Step 4: Run non-live and live-targeted tests**

Run non-live:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestFlowAgentStepPersistsSessionMessages|TestRunAgentPersistsSessionFilesAndReloadsHistory' -v
```

Expected: PASS.

Run live targeted only when DeepSeek key is available:

```bash
DEEPSEEK_API_KEY="$(tr -d '\r\n' < DEEPSEEK_API_KEY)" \
XIRA_DEEPSEEK_LIVE=1 \
XIRA_LIVE_ARTIFACT_ROOT=/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-session-$(date +%Y%m%d-%H%M%S) \
GOCACHE=$(pwd)/.cache/go-build \
go test -count=1 ./apps/xira/internal/runtime -run 'TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill$' -v
```

Expected: PASS and preserved evidence contains `sessions/**/messages.jsonl`.

- [ ] **Step 5: Commit**

```bash
git add apps/xira/internal/runtime/service_test.go apps/xira/internal/runtime/deepseek_hitl_live_test.go docs/review-packages/xira-flow-file-backed-live-test/FULL_DEEPSEEK_LIVE_REPORT.zh.md
git commit -m "test: assert flow session history persistence"
```

---

### Task 6: Update Flow Usage Guide For Real Operators

**Files:**
- Modify: `docs/guide/xira-flow-v0-usage.zh.md`
- Modify: `docs/examples/flows/devrun/flow.yaml` only if wording around `repo` is unclear

- [ ] **Step 1: Add documentation section for multiple Flow files**

In `docs/guide/xira-flow-v0-usage.zh.md`, after the minimal Flow section, add:

```markdown
## 多个 Flow 文件如何组织

推荐把 Flow 当成 workspace 资源，而不是散落在任意目录：

```text
workspace/
  agents/
  skills/
  flows/
    devrun/
      flow.yaml
      context/
      verification/
    release-review/
      flow.yaml
    incident-debug/
      flow.yaml
```

每个业务 Flow 一个目录，入口文件统一叫 `flow.yaml`。Flow 相关的上下文、验收样例、说明文件放在同级子目录里。

`xira.yaml` 可以声明可用 Flow：

```yaml
workspace: workspace
flows:
  - id: devrun
    path: flows/devrun/flow.yaml
  - id: release-review
    path: flows/release-review/flow.yaml
```

启动注册过的 Flow：

```bash
xira flow run devrun --entrypoint ad_hoc --input repo=/Users/yinwm/work/flowdeck --input request="fix this"
```

显式按路径启动仍然可用：

```bash
xira flow run --path workspace/flows/devrun/flow.yaml --entrypoint ad_hoc --input repo=/Users/yinwm/work/flowdeck --input request="fix this"
```
```

- [ ] **Step 2: Explain `repo` clearly**

Add:

```markdown
### `repo` 是什么

`repo` 不是 Flow runtime 的固定字段。它只是 `devrun` 这个开发类 Flow 声明的业务输入，意思是“这次要操作哪个代码仓库/工作目录”。

例如：

```bash
--input repo=/Users/yinwm/work/flowdeck
```

表示后续 agent step 要围绕 `/Users/yinwm/work/flowdeck` 做 intake、实现、测试和 review。

其他 Flow 可以完全不用 `repo`。客服 Flow 可以用 `ticket_id`，周报 Flow 可以用 `week`，发布 Flow 可以用 `release_id`。
```

- [ ] **Step 3: Add session history evidence section**

Add:

```markdown
## Flow 的聊天记录在哪里

Flow 每个 agent step 都会通过 runtime `RunAgent` 执行。成功完成后，runtime 会把该 agent 的会话消息写入：

```text
.xira/sessions/<channel>/<entrypoint>/<conversation-dir>/agents/<agent-id>/messages.jsonl
```

Flow 默认 channel 通常是 `flow`，所以常见路径是：

```text
.xira/sessions/flow/<entrypoint-id>/<conversation-dir>/agents/<agent-id>/messages.jsonl
```

这些文件和下面的 Flow evidence 配合使用：

```text
.xira/state/flow-runs/<flow-run-id>/flow_run.yaml
.xira/runs/<agent-run-id>/tool_calls.jsonl
.xira/runs/<agent-run-id>/events.jsonl
.xira/state/workspaces/*/human-requests/*.yaml
```

注意：如果某个 step 正在 `waiting_human`，它可能还没有最终 assistant message；完整聊天记录应在 human request resolve 并 resume 完成后检查。
```

- [ ] **Step 4: Run doc-adjacent tests**

Run:

```bash
git diff --check
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime ./apps/xira/internal/flow ./apps/xira/cmd/xira ./apps/xira/internal/api
```

Expected: PASS except API bind restrictions if the local sandbox blocks `httptest` bind. If bind fails only in sandbox, rerun outside sandbox or document the exact failure.

- [ ] **Step 5: Commit**

```bash
git add docs/guide/xira-flow-v0-usage.zh.md docs/examples/flows/devrun/flow.yaml
git commit -m "docs: explain flow registry and session history"
```

---

### Task 7: Full Verification And Review Package Refresh

**Files:**
- Modify: `docs/review-packages/xira-flow-file-backed-live-test/FULL_DEEPSEEK_LIVE_REPORT.zh.md`
- Modify: `docs/review-packages/xira-flow-file-backed-live-test/README.md`
- Modify: `docs/review-packages/xira-flow-file-backed-live-test/REVIEW_CHECKLIST.md`

- [ ] **Step 1: Run full local tests**

Run:

```bash
git diff --check
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime ./apps/xira/internal/flow ./apps/xira/cmd/xira
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/api -run 'Test(PostFlow|GetFlow)' -v
GOCACHE=$(pwd)/.cache/go-build go test -race -count=1 ./apps/xira/internal/runtime -run 'TestRuntimeRecordersPreserveConcurrentAppends'
```

Expected: PASS.

- [ ] **Step 2: Run real DeepSeek targeted Flow**

Run:

```bash
DEEPSEEK_API_KEY="$(tr -d '\r\n' < DEEPSEEK_API_KEY)" \
XIRA_DEEPSEEK_LIVE=1 \
XIRA_LIVE_ARTIFACT_ROOT=/Users/yinwm/work/flowdeck/.xira/live-tests/flow-registry-session-$(date +%Y%m%d-%H%M%S) \
GOCACHE=$(pwd)/.cache/go-build \
go test -count=1 ./apps/xira/internal/runtime -run 'TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill$' -v
```

Expected:

- PASS.
- Evidence root contains `runs/**/tool_calls.jsonl`.
- Evidence root contains `state/flow-runs/**/flow_run.yaml`.
- Evidence root contains `sessions/**/messages.jsonl`.

- [ ] **Step 3: Update review package**

Update `FULL_DEEPSEEK_LIVE_REPORT.zh.md` with:

```markdown
### Flow registry 和多配置文件

本轮新增 workspace-level Flow registry。生产使用时推荐通过 `xira.yaml` 声明多个 Flow，然后用 `flow_id` 启动；显式 `flow_path` 仍保留给临时和调试场景。

### 聊天记录证据链

Flow agent step 的聊天记录现在纳入 review checklist。每个完成的 agent step 都应能从 `flow_run.yaml` 的 `agent_run_id` 回查到 runtime run，再通过 `session_id` / `agent_id` 找到 `sessions/**/messages.jsonl`。
```

Update `REVIEW_CHECKLIST.md` with two checks:

```markdown
- [ ] Multiple Flow files can be declared in `xira.yaml` and started by `flow_id`.
- [ ] Completed Flow agent steps have persisted `sessions/**/messages.jsonl` evidence.
```

- [ ] **Step 4: Commit final verification docs**

```bash
git add docs/review-packages/xira-flow-file-backed-live-test/FULL_DEEPSEEK_LIVE_REPORT.zh.md docs/review-packages/xira-flow-file-backed-live-test/README.md docs/review-packages/xira-flow-file-backed-live-test/REVIEW_CHECKLIST.md
git commit -m "docs: refresh flow registry review evidence"
```

---

## Acceptance Criteria

- `xira.yaml` supports multiple declared flows.
- `workspace/flows/<flow-id>/flow.yaml` is discovered by default when no explicit `flows:` list is configured.
- CLI can start a registered flow by id.
- CLI can still start a flow by explicit path.
- API can start a flow by `flow_id`.
- API can still start a flow by `flow_path`.
- `xira flow list` returns registered flows.
- `xira flow inspect <flow-id>` returns one registered flow.
- Flow session history is asserted in non-live tests.
- Real DeepSeek Flow live test asserts session history files exist after completion.
- Guide explains `repo` as a business input, not a runtime concept.
- Guide explains where multiple Flow configs live.
- Guide explains where Flow chat history lives.

## Self-Review Notes

Spec coverage:

- Multiple Flow configuration files: covered by Tasks 1, 2, 3, 4, 6.
- Where configs should live: covered by Task 6.
- How to use them: covered by CLI/API tasks and docs.
- Meaning of `repo`: covered by Task 6.
- Missing chat records in tests: covered by Task 5 and Task 7.
- Real review evidence: covered by Task 7.

No intentional placeholders remain. If implementation discovers existing helper names differ from snippets, adapt names locally but keep the test intent and assertions unchanged.
