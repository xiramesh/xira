package flow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestApprovalSignalMappingCoversOptionsAndFallbacks(t *testing.T) {
	withReject := Step{Executor: Executor{Options: []string{"approve", "reject", "cancel"}}}
	cases := []struct {
		name string
		kind string
		step Step
		want string
	}{
		{name: "declared option preserves option", kind: "ReViSe", step: Step{Executor: Executor{Options: []string{"revise"}}}, want: "revise"},
		{name: "deny normalizes to reject when present", kind: "deny", step: withReject, want: "reject"},
		{name: "deny stays deny without reject", kind: "deny", step: Step{}, want: "deny"},
		{name: "cancel", kind: "cancel", step: Step{}, want: "cancel"},
		{name: "answer means approve", kind: "answer", step: Step{}, want: "approve"},
		{name: "unknown trimmed lower", kind: "  HOLD  ", step: Step{}, want: "hold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapApprovalResponseToSignal(tc.kind, tc.step); got != tc.want {
				t.Fatalf("mapApprovalResponseToSignal(%q) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
	if !hasOption(withReject, "REJECT") {
		t.Fatal("hasOption should match case-insensitively")
	}
}

func TestExpressionHelpersCoverComparisonEdges(t *testing.T) {
	run := &Run{Steps: map[string]StepState{
		"score": {Outputs: map[string]OutputRef{
			"n":        {Value: int64(42)},
			"flag":     {Value: true},
			"nested":   {Value: map[string]any{"count": float64(3)}},
			"summary":  {Summary: "done"},
			"artifact": {Artifact: "artifacts/report.md"},
			"nil":      {},
		}},
	}}

	tests := []struct {
		expr string
		want bool
	}{
		{expr: "${outputs.score.n >= 42}", want: true},
		{expr: "${outputs.score.n < 42}", want: false},
		{expr: "${outputs.score.flag == true}", want: true},
		{expr: "${outputs.score.nested.count > 2}", want: true},
		{expr: "${outputs.score.summary == 'done'}", want: true},
		{expr: "${outputs.score.artifact == 'artifacts/report.md'}", want: true},
		{expr: "${outputs.score.nil == ''}", want: true},
		{expr: "${runtime.policy.requires_approval != true}", want: true},
	}
	k := &Kernel{}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			got, err := k.evalWhen(context.Background(), run, &Definition{}, StepState{}, tt.expr)
			if err != nil {
				t.Fatalf("evalWhen(%q): %v", tt.expr, err)
			}
			if got != tt.want {
				t.Fatalf("evalWhen(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}

	if _, err := resolveOutputPath(run, "score.nested.missing"); err == nil {
		t.Fatal("expected missing nested key error")
	}
	if _, err := resolveOutputPath(run, "score.n.missing"); err == nil {
		t.Fatal("expected non-map nested navigation error")
	}
	if _, err := compareNumeric("not-a-number", ">", "1"); err == nil {
		t.Fatal("expected non-numeric left side error")
	}
	if _, err := compareNumeric(1, ">", "not-a-number"); err == nil {
		t.Fatal("expected non-numeric right side error")
	}
	if _, err := compare(1, "~=", "1"); err == nil {
		t.Fatal("expected unsupported operator error")
	}
	equalCases := []struct {
		actual any
		right  string
		want   bool
	}{
		{actual: int(7), right: "7", want: true},
		{actual: float64(1.5), right: "1.5", want: true},
		{actual: nil, right: "null", want: true},
		{actual: struct{ Name string }{Name: "x"}, right: "{x}", want: true},
		{actual: true, right: "not-bool", want: false},
		{actual: int64(9), right: "not-int", want: false},
	}
	for _, tc := range equalCases {
		if got := equalsValue(tc.actual, tc.right); got != tc.want {
			t.Fatalf("equalsValue(%v, %q) = %v, want %v", tc.actual, tc.right, got, tc.want)
		}
	}
	floatCases := []any{int(1), int64(2), float64(3), true, false, "4.5"}
	for _, v := range floatCases {
		if _, err := toFloat(v); err != nil {
			t.Fatalf("toFloat(%v): %v", v, err)
		}
	}
}

func TestStoreDefinitionAndErrorEdges(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	run, err := store.CreateRun(ctx, CreateRunRequest{
		ID:            "fr_edges_001",
		FlowID:        "devrun",
		FlowVersion:   "0.1.0",
		EntrypointID:  "ad_hoc",
		CurrentStepID: "intake",
		Input:         map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if root := store.Root(); root == "" {
		t.Fatal("Root returned empty string")
	}
	if dir := store.ArtifactsDir(run.ID); !strings.HasSuffix(dir, filepath.Join("flow-runs", run.ID, "artifacts")) {
		t.Fatalf("ArtifactsDir = %q", dir)
	}

	def := &Definition{
		SchemaVersion: SchemaVersionDefinition,
		ID:            "devrun",
		Version:       "0.1.0",
		Entrypoints:   []Entrypoint{{ID: "ad_hoc", StartStep: "intake"}},
		Steps:         []Step{{ID: "intake", Objective: "collect", Executor: Executor{Agent: "dev"}}},
	}
	if err := store.SaveDefinition(run.ID, def); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	loaded, err := store.LoadDefinitionForRun(run.ID)
	if err != nil {
		t.Fatalf("LoadDefinitionForRun: %v", err)
	}
	if loaded.ID != "devrun" || loaded.Steps[0].ID != "intake" {
		t.Fatalf("loaded definition = %+v", loaded)
	}
	if err := store.SaveDefinition(run.ID, nil); err == nil {
		t.Fatal("expected nil definition error")
	}
	if _, err := store.LoadDefinitionForRun("fr_missing_definition"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("LoadDefinitionForRun missing err = %v, want ErrRunNotFound", err)
	}
	if _, err := store.UpdateRun(ctx, run.ID, nil); err == nil {
		t.Fatal("expected nil update function error")
	}
	wantErr := errors.New("stop")
	if _, err := store.UpdateRun(ctx, run.ID, func(r *Run) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("UpdateRun fn err = %v, want %v", err, wantErr)
	}
	if err := store.AppendEvents(ctx, run.ID, nil); err != nil {
		t.Fatalf("AppendEvents nil: %v", err)
	}
	if got, err := store.ReadEvents(ctx, "fr_no_events"); err != nil || got != nil {
		t.Fatalf("ReadEvents missing = %+v, %v; want nil, nil", got, err)
	}
}

func TestStoreReadEventsRejectsMalformedJSONL(t *testing.T) {
	store := newTestStore(t)
	runID := "fr_bad_events"
	if err := os.MkdirAll(store.RunDir(runID), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.RunDir(runID), "events.jsonl"), []byte("{bad json\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := store.ReadEvents(context.Background(), runID); err == nil {
		t.Fatal("expected malformed json error")
	}
}

func TestUnmarshalYAMLEdges(t *testing.T) {
	var slot OutputSlot
	if err := slot.UnmarshalYAML(mustYAMLNode(t, "build_log")); err != nil {
		t.Fatalf("OutputSlot scalar unmarshal: %v", err)
	}
	if slot.ID != "build_log" {
		t.Fatalf("slot id = %q, want build_log", slot.ID)
	}
	var rich OutputSlot
	if err := rich.UnmarshalYAML(mustYAMLNode(t, "{id: report, formats: [md]}")); err != nil {
		t.Fatalf("OutputSlot map unmarshal: %v", err)
	}
	if rich.ID != "report" || len(rich.Formats) != 1 || rich.Formats[0] != "md" {
		t.Fatalf("rich slot = %+v", rich)
	}
	var executor Executor
	if err := executor.UnmarshalYAML(mustYAMLNode(t, "{agent: dev}")); err != nil {
		t.Fatalf("Executor unmarshal: %v", err)
	}
	if executor.toolsConfigured {
		t.Fatal("toolsConfigured should be false when tools key is absent")
	}
	if yamlMappingHasKey(nil, "tools") {
		t.Fatal("nil yaml node should not have key")
	}
	if yamlMappingHasKey(mustYAMLNode(t, "[]"), "tools") {
		t.Fatal("sequence yaml node should not have mapping key")
	}
}

func TestStoreValidationEdges(t *testing.T) {
	if err := validateFlowRunID(strings.Repeat("a", 129)); err == nil {
		t.Fatal("expected too-long flow run id error")
	}
	if err := validateFlowID("../bad"); err == nil {
		t.Fatal("expected invalid flow id error")
	}
	if err := validateArtifactPath(" "); err == nil {
		t.Fatal("expected empty artifact path error")
	}
	if got := cloneStringMap(nil); got != nil {
		t.Fatalf("cloneStringMap(nil) = %+v, want nil", got)
	}
	if got := cloneInboundContext(nil); got != nil {
		t.Fatalf("cloneInboundContext(nil) = %+v, want nil", got)
	}
}

func TestNowUsesInjectedClock(t *testing.T) {
	want := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	k := &Kernel{Clock: func() time.Time { return want }}
	if got := k.now(); !got.Equal(want) {
		t.Fatalf("now = %v, want %v", got, want)
	}
}

func TestDefinitionValidationEdges(t *testing.T) {
	valid := func() *Definition {
		return &Definition{
			SchemaVersion: SchemaVersionDefinition,
			ID:            "devrun",
			Name:          "Dev Run",
			Version:       "0.1.0",
			Entrypoints:   []Entrypoint{{ID: "ad_hoc", StartStep: "intake"}},
			Steps:         []Step{{ID: "intake", Objective: "collect", Executor: Executor{Agent: "dev"}}},
		}
	}
	mutations := map[string]func(*Definition){
		"nil":                  nil,
		"schema":               func(d *Definition) { d.SchemaVersion = "old" },
		"id":                   func(d *Definition) { d.ID = " " },
		"name":                 func(d *Definition) { d.Name = " " },
		"version":              func(d *Definition) { d.Version = " " },
		"no steps":             func(d *Definition) { d.Steps = nil },
		"empty step id":        func(d *Definition) { d.Steps[0].ID = " " },
		"empty objective":      func(d *Definition) { d.Steps[0].Objective = " " },
		"no executor":          func(d *Definition) { d.Steps[0].Executor = Executor{} },
		"shell executor":       func(d *Definition) { d.Steps[0].Executor = Executor{Type: "shell"} },
		"missing on failure":   func(d *Definition) { d.Steps[0].Transitions.OnFailure = "missing" },
		"empty branch when":    func(d *Definition) { d.Steps[0].Transitions.Branches = []Branch{{When: " ", Next: "intake"}} },
		"empty branch next":    func(d *Definition) { d.Steps[0].Transitions.Branches = []Branch{{When: "${outputs.intake.x == 'y'}"}} },
		"missing retry target": func(d *Definition) { d.Steps[0].Retry = &RetryPolicy{OnExhausted: "missing"} },
		"no entrypoints":       func(d *Definition) { d.Entrypoints = nil },
		"empty entrypoint id":  func(d *Definition) { d.Entrypoints[0].ID = " " },
		"duplicate entrypoint": func(d *Definition) { d.Entrypoints = append(d.Entrypoints, d.Entrypoints[0]) },
		"empty start step":     func(d *Definition) { d.Entrypoints[0].StartStep = " " },
		"missing start step":   func(d *Definition) { d.Entrypoints[0].StartStep = "missing" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var def *Definition
			if mutate != nil {
				def = valid()
				mutate(def)
			}
			if err := ValidateDefinition(def); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}

	def := valid()
	if step, ok := def.StepByID("missing"); ok || step.ID != "" {
		t.Fatalf("missing StepByID = %+v, %v", step, ok)
	}
	if _, _, err := (*Definition)(nil).ResolveEntrypoint("ad_hoc"); err == nil {
		t.Fatal("expected nil definition entrypoint error")
	}
	def.Entrypoints[0].StartStep = "missing"
	if _, _, err := def.ResolveEntrypoint("ad_hoc"); err == nil {
		t.Fatal("expected missing entrypoint start step error")
	}
}

func TestKernelResolveDefinitionByIDFallsBackToPersistedDefinition(t *testing.T) {
	store := newTestStore(t)
	def := &Definition{
		SchemaVersion: SchemaVersionDefinition,
		ID:            "devrun",
		Name:          "Dev Run",
		Version:       "0.1.0",
		Entrypoints:   []Entrypoint{{ID: "ad_hoc", StartStep: "intake"}},
		Steps:         []Step{{ID: "intake", Objective: "collect", Executor: Executor{Agent: "dev"}}},
	}
	run, err := store.CreateRun(context.Background(), CreateRunRequest{ID: "fr_def_fallback", FlowID: "devrun"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := store.SaveDefinition(run.ID, def); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	k := &Kernel{Store: store}
	got, err := k.resolveDefinitionByID(context.Background(), run)
	if err != nil {
		t.Fatalf("resolveDefinitionByID: %v", err)
	}
	if got.ID != "devrun" {
		t.Fatalf("definition id = %q, want devrun", got.ID)
	}
	if _, ok := k.lookupDefinition("devrun"); !ok {
		t.Fatal("persisted definition should be cached after load")
	}
	if _, err := k.resolveDefinitionByID(context.Background(), &Run{ID: "fr_missing", FlowID: "missing"}); err == nil {
		t.Fatal("expected missing definition error")
	}
}

func TestCloneToolInputAllowlistDeepCopies(t *testing.T) {
	in := map[string]map[string][]string{
		"write_file": {"path": {"artifacts/report.md"}},
		"read_file":  {},
	}
	got := cloneToolInputAllowlist(in)
	if got["write_file"]["path"][0] != "artifacts/report.md" {
		t.Fatalf("copied allowlist = %+v", got)
	}
	in["write_file"]["path"][0] = "mutated"
	if got["write_file"]["path"][0] != "artifacts/report.md" {
		t.Fatal("cloneToolInputAllowlist did not deep-copy values")
	}
	if got["read_file"] != nil {
		t.Fatalf("empty nested allowlist = %+v, want nil", got["read_file"])
	}
	if cloneToolInputAllowlist(nil) != nil {
		t.Fatal("nil allowlist should clone to nil")
	}
}

func mustYAMLNode(t *testing.T, src string) *yaml.Node {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(src), &node); err != nil {
		t.Fatalf("yaml.Unmarshal(%q): %v", src, err)
	}
	if len(node.Content) == 0 {
		t.Fatalf("yaml.Unmarshal(%q) produced empty node", src)
	}
	return node.Content[0]
}
