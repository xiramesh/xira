package humanrequest

import (
	"errors"
	"strings"
	"testing"
)

const textProtocolCorrelation = "550e8400-e29b-41d4-a716-446655440000"
const textProtocolReference = "HR-550E8400E29B41D4A716446655440000"

func TestTextReferenceRequiresFullUUIDEntropy(t *testing.T) {
	got, err := TextReference(textProtocolCorrelation)
	if err != nil {
		t.Fatalf("TextReference() error = %v", err)
	}
	if got != textProtocolReference {
		t.Fatalf("TextReference() = %q, want %q", got, textProtocolReference)
	}

	for _, token := range []string{"", "7K2P", "550e8400-e29b-41d4-a716", "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"} {
		if _, err := TextReference(token); !errors.Is(err, ErrValidation) {
			t.Fatalf("TextReference(%q) error = %v, want validation", token, err)
		}
	}
}

func TestParseTextResponseSeparatesProtocolFromOrdinaryChat(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		recognized bool
		wantToken  string
		wantAnswer string
		wantErr    bool
	}{
		{name: "ordinary chat", content: "please answer this", recognized: false},
		{name: "valid action", content: "/answer " + textProtocolReference + " approve", recognized: true, wantToken: textProtocolCorrelation, wantAnswer: "approve"},
		{name: "case and whitespace", content: "  /ANSWER  " + strings.ToLower(textProtocolReference) + "   Tuesday after 3pm  ", recognized: true, wantToken: textProtocolCorrelation, wantAnswer: "Tuesday after 3pm"},
		{name: "multiline freeform", content: "/answer " + textProtocolReference + " first line\nsecond line", recognized: true, wantToken: textProtocolCorrelation, wantAnswer: "first line\nsecond line"},
		{name: "missing reference", content: "/answer", recognized: true, wantErr: true},
		{name: "missing answer", content: "/answer " + textProtocolReference, recognized: true, wantErr: true},
		{name: "short reference", content: "/answer HR-7K2P approve", recognized: true, wantErr: true},
		{name: "invalid full reference", content: "/answer HR-ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ approve", recognized: true, wantErr: true},
		{name: "lookalike command", content: "/answering " + textProtocolReference + " approve", recognized: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, recognized, err := ParseTextResponse(tt.content)
			if recognized != tt.recognized {
				t.Fatalf("recognized = %v, want %v", recognized, tt.recognized)
			}
			if tt.wantErr {
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("error = %v, want validation", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTextResponse() error = %v", err)
			}
			if got.CorrelationToken != tt.wantToken || got.Answer != tt.wantAnswer {
				t.Fatalf("command = %+v, want token=%q answer=%q", got, tt.wantToken, tt.wantAnswer)
			}
		})
	}
}

func TestRenderTextRequestCarriesExactReferenceAndOptions(t *testing.T) {
	req := HumanRequest{
		Kind:             RequestApproval,
		Question:         "是否批准合同？",
		CorrelationToken: textProtocolCorrelation,
		Options: []HumanOption{
			{ID: "approve", Label: "批准"},
			{ID: "revise", Label: "修改后再审"},
			{ID: "deny", Label: "DENY"},
			{},
		},
	}
	freeform, err := RenderTextRequest(HumanRequest{Question: "哪一天？", CorrelationToken: textProtocolCorrelation})
	if err != nil || !strings.Contains(freeform, "/answer "+textProtocolReference+" <回答>") {
		t.Fatalf("freeform render = %q, %v", freeform, err)
	}
	if _, err := RenderTextRequest(HumanRequest{Question: "missing token"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing correlation render error = %v, want validation", err)
	}
	if _, err := RenderTextRequest(HumanRequest{CorrelationToken: textProtocolCorrelation}); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing question render error = %v, want validation", err)
	}
	text, err := RenderTextRequest(req)
	if err != nil {
		t.Fatalf("RenderTextRequest() error = %v", err)
	}
	for _, want := range []string{
		"[请求 " + textProtocolReference + "]",
		"是否批准合同？",
		"/answer " + textProtocolReference + " approve",
		"/answer " + textProtocolReference + " revise",
		"/answer " + textProtocolReference + " deny",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered text missing %q:\n%s", want, text)
		}
	}
}

func TestNormalizeTextAnswerUsesDeclaredOptionsExactly(t *testing.T) {
	req := HumanRequest{Options: []HumanOption{
		{ID: "approve", Label: "批准"},
		{ID: "revise", Label: "修改后再审"},
		{Label: "稍后决定"},
	}}
	for input, want := range map[string]string{
		" APPROVE ": "approve",
		"批准":        "approve",
		"修改后再审":     "revise",
		"稍后决定":      "稍后决定",
	} {
		got, err := NormalizeTextAnswer(req, input)
		if err != nil || got != want {
			t.Fatalf("NormalizeTextAnswer(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := NormalizeTextAnswer(req, "approved"); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown option error = %v, want validation", err)
	}

	freeform, err := NormalizeTextAnswer(HumanRequest{}, "  Tuesday after 3pm  ")
	if err != nil || freeform != "Tuesday after 3pm" {
		t.Fatalf("freeform = %q, %v", freeform, err)
	}
	if _, err := NormalizeTextAnswer(HumanRequest{}, "  "); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty freeform error = %v, want validation", err)
	}
}
