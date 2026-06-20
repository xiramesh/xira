package runtime

import (
	"testing"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// TestCleanChildArtifactPath covers the valid / absolute / traversal / empty arms.
func TestCleanChildArtifactPath(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
		want   string
	}{
		{"artifacts/tool-outputs/x.json", true, "artifacts/tool-outputs/x.json"},
		{"  artifacts/a  ", true, "artifacts/a"},
		{"", false, ""},
		{"  ", false, ""},
		{"/abs/path", false, ""},
		{"..", false, ""},
		{"../escape", false, ""},
		{".", false, ""},
	}
	for _, tc := range cases {
		got, ok := cleanChildArtifactPath(tc.in)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("cleanChildArtifactPath(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestChildToolArtifactEvidenceRef covers nil-service, missing path, wrong dir,
// and the happy path (artifact exists on disk).
func TestChildToolArtifactEvidenceRef(t *testing.T) {
	svc := newTestService(t, Config{})
	const runID = "run-art-1"
	if err := svc.runs.InitRun(runID); err != nil {
		t.Fatal(err)
	}
	// nil service -> "".
	var nilSvc *Service
	if got := nilSvc.childToolArtifactEvidenceRef(runID, nil); got != "" {
		t.Fatalf("nil service should return empty, got %q", got)
	}
	// No raw_output_path -> "".
	if got := svc.childToolArtifactEvidenceRef(runID, map[string]any{}); got != "" {
		t.Fatalf("missing path should return empty, got %q", got)
	}
	// Path outside artifacts/tool-outputs -> "".
	if got := svc.childToolArtifactEvidenceRef(runID, map[string]any{"raw_output_path": "other/x"}); got != "" {
		t.Fatalf("wrong dir should return empty, got %q", got)
	}
	// Happy path: create the artifact, reference should resolve.
	rel := "artifacts/tool-outputs/out.txt"
	abs := svc.runs.RunDir(runID) + "/" + rel
	writeFile(t, abs, "hello")
	got := svc.childToolArtifactEvidenceRef(runID, map[string]any{"raw_output_path": rel})
	want := "artifact://" + runID + "/" + rel
	if got != want {
		t.Fatalf("evidence ref = %q, want %q", got, want)
	}
}

// TestHumanRequestMetadata covers the source/options branches + base fields.
func TestHumanRequestMetadata(t *testing.T) {
	// Minimal: no source, no options.
	meta := humanRequestMetadata(humanrequest.HumanRequest{ID: "r1", Kind: humanrequest.RequestApproval})
	if meta["human_request_id"] != "r1" || meta["kind"] != "approval" || meta["request_kind"] != "approval" {
		t.Fatalf("base meta wrong: %v", meta)
	}
	if _, ok := meta["source"]; ok {
		t.Fatalf("source should be absent when empty")
	}
	// With source + options.
	meta = humanRequestMetadata(humanrequest.HumanRequest{
		ID:      "r2",
		Kind:    humanrequest.RequestFreeform,
		Source:  "tool",
		Options: []humanrequest.HumanOption{{Label: "Yes"}, {Label: "No"}},
	})
	if meta["source"] != "tool" {
		t.Fatalf("source should be set")
	}
	opts, ok := meta["options"].([]string)
	if !ok || len(opts) != 2 || opts[0] != "Yes" || opts[1] != "No" {
		t.Fatalf("options wrong: %v", meta["options"])
	}
}

// TestUsageStoreRootNilSafe covers the nil-receiver guard.
func TestUsageStoreRootNilSafe(t *testing.T) {
	var nilStore *UsageStore
	if nilStore.Root() != "" {
		t.Fatalf("nil UsageStore Root should be empty")
	}
}

// TestUsageStoreAppendCallsNilSafe: nil store and empty calls are no-ops; a real
// store appends and persists.
func TestUsageStoreAppendCalls(t *testing.T) {
	var nilStore *UsageStore
	if err := nilStore.AppendCalls(nil); err != nil {
		t.Fatalf("nil AppendCalls should be no-op, got %v", err)
	}
	if err := nilStore.AppendCalls([]LLMCallRecord{{Model: "m"}}); err != nil {
		t.Fatalf("nil AppendCalls with data should be no-op, got %v", err)
	}

	store := NewUsageStore(t.TempDir())
	// Empty calls slice -> no-op (early return).
	if err := store.AppendCalls(nil); err != nil {
		t.Fatalf("empty AppendCalls should be no-op: %v", err)
	}
	if err := store.AppendCalls([]LLMCallRecord{{Model: "deepseek", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}); err != nil {
		t.Fatalf("AppendCalls failed: %v", err)
	}
}

// TestSummarizeUsage covers aggregation: call counts, token sums, failed vs
// completed, cost rollup, missing-usage tracking.
func TestSummarizeUsage(t *testing.T) {
	cost := 1.5
	resp := TurnResponse{
		RunID:    "run-sum-1",
		AgentID:  "agent-1",
		LLMCalls: []LLMCallRecord{
			{Model: "deepseek", Status: "completed", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, UsageSource: "provider", Cost: &cost, Currency: "usd"},
			{Model: "deepseek", Status: "failed", PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0, UsageSource: "missing", RequestIndex: 7},
		},
	}
	summary := summarizeUsage(resp)
	if summary.RunID != "run-sum-1" || summary.AgentID != "agent-1" {
		t.Fatalf("identity fields wrong: %+v", summary)
	}
	if summary.CallCount != 2 || summary.CompletedCalls != 1 || summary.FailedCalls != 1 {
		t.Fatalf("call counts wrong: %+v", summary)
	}
	if summary.PromptTokens != 10 || summary.CompletionTokens != 5 || summary.TotalTokens != 15 {
		t.Fatalf("token sums wrong: %+v", summary)
	}
	if summary.UsageSources["provider"] != 1 || summary.UsageSources["missing"] != 1 {
		t.Fatalf("usage sources wrong: %+v", summary.UsageSources)
	}
	if len(summary.MissingUsageRequests) != 1 || summary.MissingUsageRequests[0] != 7 {
		t.Fatalf("missing usage tracking wrong: %v", summary.MissingUsageRequests)
	}
	if summary.Currency != "usd" {
		t.Fatalf("currency wrong: %q", summary.Currency)
	}
	// Per-model aggregation.
	m, ok := summary.Models["deepseek"]
	if !ok || m.CallCount != 2 {
		t.Fatalf("model aggregation wrong: %+v", summary.Models)
	}
}

// TestStringArg covers nil map, missing key, nil value, present value.
func TestStringArg(t *testing.T) {
	if got := stringArg(nil, "k"); got != "" {
		t.Fatalf("nil map should be empty, got %q", got)
	}
	if got := stringArg(map[string]any{}, "k"); got != "" {
		t.Fatalf("missing key should be empty, got %q", got)
	}
	if got := stringArg(map[string]any{"k": nil}, "k"); got != "" {
		t.Fatalf("nil value should be empty, got %q", got)
	}
	if got := stringArg(map[string]any{"k": "  hi  "}, "k"); got != "hi" {
		t.Fatalf("present value: got %q", got)
	}
	if got := stringArg(map[string]any{"k": 42}, "k"); got != "42" {
		t.Fatalf("non-string value: got %q", got)
	}
}

// TestStringSliceFromAny covers each type arm + sorting + blank filtering.
func TestStringSliceFromAny(t *testing.T) {
	// []string arm: copied + sorted.
	got := stringSliceFromAny([]string{"b", "a", "c"})
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("[]string arm wrong: %v", got)
	}
	// []any arm: blank filtered, sorted.
	got = stringSliceFromAny([]any{"b", "  ", "a"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("[]any arm wrong: %v", got)
	}
	// string arm.
	got = stringSliceFromAny("single")
	if len(got) != 1 || got[0] != "single" {
		t.Fatalf("string arm wrong: %v", got)
	}
	// blank string -> nil.
	if stringSliceFromAny("   ") != nil {
		t.Fatalf("blank string should be nil")
	}
	// default arm.
	if stringSliceFromAny(123) != nil {
		t.Fatalf("default arm should be nil")
	}
}
