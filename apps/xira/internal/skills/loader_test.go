package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromWorkspaceReadsSkillMarkdownCaseInsensitively(t *testing.T) {
	workspace := t.TempDir()
	skillDir := filepath.Join(workspace, "skills", "local-research")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeSkillFile(t, filepath.Join(skillDir, "skill.md"), `---
schema_version: xira.skill.v0
id: local-research
name: Local Research
version: 0.1.0
description: Source-backed local research.
activation:
  mode: explicit
requires:
  tools:
    - search_file
    - read_file
  optional_tools:
    - command.run
context:
  includes:
    - references/
verification:
  default_checks:
    - final_response_non_empty
artifacts:
  output_dir: artifacts/skills/local-research
  retention: local
---
# Instructions

Use local evidence before summaries.
`)
	writeSkillFile(t, filepath.Join(skillDir, "references", "usage.md"), "Use references only when relevant.\n")

	manager, err := LoadFromWorkspace(workspace)
	if err != nil {
		t.Fatalf("LoadFromWorkspace() error = %v", err)
	}
	skill, ok := manager.Get("local-research")
	if !ok {
		t.Fatal("expected local-research skill")
	}
	if skill.SchemaVersion != SchemaVersionSkillV0 || skill.Activation.Mode != "explicit" {
		t.Fatalf("skill metadata = %+v", skill)
	}
	if got := strings.Join(skill.Requires.Tools, ","); got != "search_file,read_file" {
		t.Fatalf("required tools = %q", got)
	}
	if block := skill.InstructionBlock(); !strings.Contains(block, "Loaded Skill: local-research v0.1.0") || !strings.Contains(block, "Use local evidence before summaries.") {
		t.Fatalf("instruction block missing skill body:\n%s", block)
	}
}

func TestLoadSkillDirRejectsContextPathOutsideSkillDirectory(t *testing.T) {
	workspace := t.TempDir()
	skillDir := filepath.Join(workspace, "skills", "bad-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeSkillFile(t, filepath.Join(skillDir, "SKILL.md"), `---
schema_version: xira.skill.v0
id: bad-skill
name: Bad Skill
version: 0.1.0
description: Invalid skill.
context:
  includes:
    - ../secrets
---
Body.
`)

	_, err := LoadFromWorkspace(workspace)
	if err == nil {
		t.Fatal("expected invalid context path error")
	}
	if !strings.Contains(err.Error(), "must stay within skill directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func writeSkillFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}
