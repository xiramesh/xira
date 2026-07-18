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
		{name: "byte order mark", final: "\ufeff{\"reply\":\"ok\"}", wantErr: true},
		{name: "non breaking space", final: "{\"reply\":\"ok\"}\u00a0", wantErr: true},
		{name: "zero width space", final: "{\"reply\":\"ok\"}\u200b", wantErr: true},
		{name: "ideographic space", final: "{\"reply\":\"ok\"}\u3000", wantErr: true},
		{name: "markdown fence", final: "```json\n{\"reply\":\"ok\"}\n```", wantErr: true},
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

func TestModelPolicySnapshotPreservesAuthoredFormat(t *testing.T) {
	profile := agents.BuiltinXiraAssistant()
	profile.ModelPolicy.Format = ""
	if got := modelPolicySnapshot(profile, "builtin").Format; got != "" {
		t.Fatalf("omitted format snapshot = %q, want omission preserved", got)
	}

	profile.ModelPolicy.Format = " JSON "
	if got := modelPolicySnapshot(profile, "workspace").Format; got != "json" {
		t.Fatalf("authored JSON format snapshot = %q, want normalized json", got)
	}
}
