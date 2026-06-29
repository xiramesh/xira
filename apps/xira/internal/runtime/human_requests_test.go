package runtime

import (
	"context"
	"testing"
)

// TestChatKeyStringFromContext verifies the helper that fills
// CreateRequest.ChatKey from ctx. This is the link between the chatKey injected
// at service.go:491 (WithChatKey) and the persisted HumanRequest.ChatKey (#91-A).
func TestChatKeyStringFromContext(t *testing.T) {
	// No chatKey in ctx → "".
	if got := chatKeyStringFromContext(context.Background()); got != "" {
		t.Errorf("empty ctx → %q, want empty", got)
	}
	// With chatKey → its String() form.
	ctx := WithChatKey(context.Background(), ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"})
	got := chatKeyStringFromContext(ctx)
	want := ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"}.String()
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
