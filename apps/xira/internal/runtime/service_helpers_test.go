package runtime

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/xiramesh/xira/internal/model/deepseek"
)

// TestCollectionLen covers all type arms of the collection-length helper.
func TestCollectionLen(t *testing.T) {
	if collectionLen([]map[string]any{{"a": 1}, {"b": 2}}) != 2 {
		t.Fatalf("[]map[string]any len mismatch")
	}
	if collectionLen([]any{1, 2, 3}) != 3 {
		t.Fatalf("[]any len mismatch")
	}
	if collectionLen("not-a-collection") != 0 {
		t.Fatalf("default arm should be 0")
	}
	if collectionLen(nil) != 0 {
		t.Fatalf("nil should be 0")
	}
}

// TestSortedAnyKeys covers key extraction + deterministic sorting.
func TestSortedAnyKeys(t *testing.T) {
	keys := sortedAnyKeys(map[string]any{"c": 1, "a": 2, "b": 3})
	if !sort.StringsAreSorted(keys) {
		t.Fatalf("keys not sorted: %v", keys)
	}
	want := []string{"a", "b", "c"}
	if len(keys) != len(want) {
		t.Fatalf("got %d keys, want %d", len(keys), len(want))
	}
	for i, k := range keys {
		if k != want[i] {
			t.Fatalf("keys[%d] = %q, want %q", i, k, want[i])
		}
	}
	if len(sortedAnyKeys(map[string]any{})) != 0 {
		t.Fatalf("empty map should yield empty keys")
	}
}

// TestMessageContent covers the text-extraction branches.
func TestMessageContent(t *testing.T) {
	// Plain string content.
	if got := messageContent(deepseek.Message{Content: "hi"}); got != "hi" {
		t.Fatalf("string content: got %q", got)
	}
	// Nil content -> "".
	if got := messageContent(deepseek.Message{Content: nil}); got != "" {
		t.Fatalf("nil content: got %q", got)
	}
	// Non-text content -> JSON-marshalled fallback.
	msg := deepseek.Message{Content: json.RawMessage(`{"x":1}`)}
	got := messageContent(msg)
	if got == "" {
		t.Fatalf("non-text content should fall back to JSON, got empty")
	}
}
