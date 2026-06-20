package runtime

import (
	"encoding/json"
	"math"
	"testing"
)

// TestNormalizeUsagePricing covers the pricing-normalization branches: empty
// models collapse to nil, whitespace currency trimmed, blank model keys dropped.
func TestNormalizeUsagePricing(t *testing.T) {
	// Empty models -> nil (the early-return branch).
	got := normalizeUsagePricing(UsagePricing{Currency: " usd "})
	if got.Currency != "usd" {
		t.Fatalf("currency = %q, want usd", got.Currency)
	}
	if got.Models != nil {
		t.Fatalf("empty models should collapse to nil, got %v", got.Models)
	}

	// Models with a blank key dropped, real key kept.
	got = normalizeUsagePricing(UsagePricing{
		Currency: "cny",
		Models: map[string]ModelUsagePricing{
			"  ":       {PromptPerMillion: 1},
			"deepseek": {PromptPerMillion: 2, CompletionPerMillion: 3},
		},
	})
	if _, ok := got.Models["deepseek"]; !ok {
		t.Fatalf("real model key should be kept")
	}
	if _, ok := got.Models[""]; ok {
		t.Fatalf("blank model key should be dropped")
	}

	// All keys blank -> models collapse to nil.
	got = normalizeUsagePricing(UsagePricing{
		Models: map[string]ModelUsagePricing{"   ": {}},
	})
	if got.Models != nil {
		t.Fatalf("all-blank models should collapse to nil, got %v", got.Models)
	}
}

// TestUsageCost covers the cost-calculation branches.
func TestUsageCost(t *testing.T) {
	// No models -> no cost.
	cost, cur := usageCost(UsagePricing{}, "m", 100, 100)
	if cost != nil || cur != "" {
		t.Fatalf("no models: got cost=%v cur=%q, want nil/empty", cost, cur)
	}
	// Model not present -> no cost.
	cost, cur = usageCost(UsagePricing{Currency: "usd", Models: map[string]ModelUsagePricing{"other": {}}}, "m", 100, 100)
	if cost != nil {
		t.Fatalf("unknown model: got cost=%v, want nil", cost)
	}
	// Zero pricing -> no cost (the `== 0 && == 0` guard).
	cost, cur = usageCost(UsagePricing{Models: map[string]ModelUsagePricing{"m": {}}}, "m", 100, 100)
	if cost != nil {
		t.Fatalf("zero pricing: got cost=%v, want nil", cost)
	}
	// Real pricing -> cost computed, currency trimmed.
	cost, cur = usageCost(UsagePricing{Currency: "  usd  ", Models: map[string]ModelUsagePricing{
		"m": {PromptPerMillion: 2, CompletionPerMillion: 8},
	}}, "m", 500_000, 250_000)
	if cost == nil {
		t.Fatalf("expected a cost, got nil")
	}
	// (500000/1e6)*2 + (250000/1e6)*8 = 1 + 2 = 3
	if math.Abs(*cost-3.0) > 1e-9 {
		t.Fatalf("cost = %v, want 3.0", *cost)
	}
	if cur != "usd" {
		t.Fatalf("currency = %q, want usd", cur)
	}
}

// TestUsageInt covers each type arm of the int extraction.
func TestUsageInt(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want int64
	}{
		{"missing key", map[string]any{}, 0},
		{"int", map[string]any{"prompt_tokens": 42}, 42},
		{"int64", map[string]any{"prompt_tokens": int64(42)}, 42},
		{"float64", map[string]any{"prompt_tokens": float64(42)}, 42},
		{"float64 NaN", map[string]any{"prompt_tokens": math.NaN()}, 0},
		{"float64 Inf", map[string]any{"prompt_tokens": math.Inf(1)}, 0},
		{"json.Number", map[string]any{"prompt_tokens": json.Number("42")}, 42},
		{"json.Number invalid", map[string]any{"prompt_tokens": json.Number("x")}, 0},
		{"string valid", map[string]any{"prompt_tokens": " 42 "}, 42},
		{"string invalid", map[string]any{"prompt_tokens": "abc"}, 0},
		{"first key wins", map[string]any{"a": 1, "prompt_tokens": 99}, 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageInt(tc.in, "prompt_tokens", "a"); got != tc.want {
				t.Fatalf("usageInt(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestAnyChars covers each type arm of the char counter.
func TestAnyChars(t *testing.T) {
	if anyChars(nil) != 0 {
		t.Fatalf("nil should be 0")
	}
	if anyChars("hello") != 5 {
		t.Fatalf("string hello should be 5")
	}
	if anyChars([]byte("你好")) != 2 {
		t.Fatalf("utf8 bytes 你好 should be 2 runes")
	}
	// Default: marshal to JSON, count runes.
	if anyChars(map[string]any{"a": 1}) == 0 {
		t.Fatalf("marshalled map should have nonzero chars")
	}
}
