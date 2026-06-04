package feishu

import (
	"encoding/json"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestExtractContentText(t *testing.T) {
	got := extractContent(larkim.MsgTypeText, `{"text":"hello"}`)
	if got != "hello" {
		t.Fatalf("content = %q", got)
	}
}

func TestStripMentionPlaceholders(t *testing.T) {
	key := "@_user_1"
	got := stripMentionPlaceholders("hello @_user_1 world", []*larkim.MentionEvent{{Key: &key}})
	if got != "hello  world" {
		t.Fatalf("content = %q", got)
	}
}

func TestBuildMarkdownCard(t *testing.T) {
	card, err := buildMarkdownCard("hello")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(card), &parsed); err != nil {
		t.Fatalf("card is not json: %v", err)
	}
	if parsed["schema"] != "2.0" {
		t.Fatalf("schema = %v", parsed["schema"])
	}
}
