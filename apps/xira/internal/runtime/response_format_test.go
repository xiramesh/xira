package runtime

import (
	"testing"

	"github.com/xiramesh/xira/internal/agents"
)

func TestGenerateContentConfigHonorsModelFormat(t *testing.T) {
	jsonProfile := agents.BuiltinXiraAssistant()
	jsonProfile.ModelPolicy.Temp = nil
	jsonProfile.ModelPolicy.Format = "json"
	jsonConfig := generateContentConfig(jsonProfile)
	if jsonConfig == nil || jsonConfig.ResponseMIMEType != "application/json" {
		t.Fatalf("JSON config = %+v, want application/json", jsonConfig)
	}

	textProfile := agents.BuiltinXiraAssistant()
	textProfile.ModelPolicy.Temp = nil
	textProfile.ModelPolicy.Format = ""
	if got := generateContentConfig(textProfile); got != nil {
		t.Fatalf("default text config = %+v, want nil", got)
	}

	explicitTextProfile := agents.BuiltinXiraAssistant()
	explicitTextProfile.ModelPolicy.Format = "text"
	textConfig := generateContentConfig(explicitTextProfile)
	if textConfig == nil || textConfig.ResponseMIMEType != "" || textConfig.Temperature == nil {
		t.Fatalf("explicit text config = %+v, want temperature only", textConfig)
	}
}

func TestDeepSeekResponseFormatHonorsModelFormat(t *testing.T) {
	if got := deepSeekResponseFormat(agents.ModelPolicy{}); got != nil {
		t.Fatalf("default response format = %+v, want nil", got)
	}
	got := deepSeekResponseFormat(agents.ModelPolicy{Format: "JSON"})
	if got == nil || got.Type != "json_object" {
		t.Fatalf("JSON response format = %+v, want json_object", got)
	}
}

func TestValidateJSONFinalResponse(t *testing.T) {
	tests := []struct {
		name    string
		final   string
		wantErr bool
	}{
		{name: "object", final: `{"reply":"ok"}`},
		{name: "object with whitespace", final: " \n {\"reply\":\"ok\"} \t"},
		{name: "prose wrapped", final: `好的：{"reply":"ok"}`, wantErr: true},
		{name: "array", final: `[{"reply":"ok"}]`, wantErr: true},
		{name: "null", final: `null`, wantErr: true},
		{name: "empty", final: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateJSONFinalResponse(tt.final)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateJSONFinalResponse(%q) error = %v, wantErr %v", tt.final, err, tt.wantErr)
			}
		})
	}
}
