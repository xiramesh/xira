package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/xiramesh/xira/internal/fileutil"
)

const (
	SkillFileName        = "SKILL.md"
	SchemaVersionSkillV0 = "xira.skill.v0"
)

type Skill struct {
	SchemaVersion string             `json:"schema_version" yaml:"schema_version"`
	ID            string             `json:"id" yaml:"id"`
	Name          string             `json:"name" yaml:"name"`
	Version       string             `json:"version" yaml:"version"`
	Description   string             `json:"description" yaml:"description"`
	Activation    ActivationPolicy   `json:"activation,omitempty" yaml:"activation,omitempty"`
	Requires      Requirements       `json:"requires,omitempty" yaml:"requires,omitempty"`
	Context       ContextPolicy      `json:"context,omitempty" yaml:"context,omitempty"`
	Verification  VerificationPolicy `json:"verification,omitempty" yaml:"verification,omitempty"`
	Artifacts     ArtifactPolicy     `json:"artifacts,omitempty" yaml:"artifacts,omitempty"`
	Instructions  string             `json:"instructions,omitempty" yaml:"-"`
	SourcePath    string             `json:"source_path,omitempty" yaml:"-"`
}

type ActivationPolicy struct {
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`
}

type Requirements struct {
	Tools         []string `json:"tools,omitempty" yaml:"tools,omitempty"`
	OptionalTools []string `json:"optional_tools,omitempty" yaml:"optional_tools,omitempty"`
	Secrets       []string `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	MCPServers    []string `json:"mcp_servers,omitempty" yaml:"mcp_servers,omitempty"`
}

type ContextPolicy struct {
	Includes  []string `json:"includes,omitempty" yaml:"includes,omitempty"`
	Forbidden []string `json:"forbidden,omitempty" yaml:"forbidden,omitempty"`
}

type VerificationPolicy struct {
	DefaultChecks []string `json:"default_checks,omitempty" yaml:"default_checks,omitempty"`
}

type ArtifactPolicy struct {
	OutputDir string `json:"output_dir,omitempty" yaml:"output_dir,omitempty"`
	Retention string `json:"retention,omitempty" yaml:"retention,omitempty"`
}

type Manager struct {
	skills map[string]Skill
}

func LoadFromWorkspace(workspaceRoot string) (*Manager, error) {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		return nil, fmt.Errorf("workspace root is required")
	}
	skillsRoot := filepath.Join(workspaceRoot, "skills")
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return NewManager(nil)
		}
		return nil, fmt.Errorf("read skills directory %s: %w", skillsRoot, err)
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(skillsRoot, entry.Name()))
		}
	}
	sort.Strings(dirs)

	loaded := make([]Skill, 0, len(dirs))
	for _, dir := range dirs {
		_, err := fileutil.FindFileEqualFold(dir, SkillFileName)
		if err != nil {
			if fileutil.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		skill, err := LoadSkillDir(dir)
		if err != nil {
			return nil, err
		}
		loaded = append(loaded, skill)
	}
	return NewManager(loaded)
}

func LoadSkillDir(skillDir string) (Skill, error) {
	skillPath, err := fileutil.FindFileEqualFold(skillDir, SkillFileName)
	if err != nil {
		return Skill{}, err
	}
	content, err := os.ReadFile(skillPath)
	if err != nil {
		return Skill{}, fmt.Errorf("read %s: %w", skillPath, err)
	}
	frontmatter, body, err := parseSkillMarkdown(string(content))
	if err != nil {
		return Skill{}, fmt.Errorf("parse %s: %w", skillPath, err)
	}
	dirID := filepath.Base(skillDir)
	if frontmatter.ID != dirID {
		return Skill{}, fmt.Errorf("%s id %q must match skill directory %q", SkillFileName, frontmatter.ID, dirID)
	}
	frontmatter.SourcePath = skillPath
	frontmatter.Instructions = strings.TrimSpace(body)
	frontmatter.normalize()
	if err := frontmatter.Validate(skillDir); err != nil {
		return Skill{}, fmt.Errorf("invalid skill %q: %w", frontmatter.ID, err)
	}
	return frontmatter, nil
}

func NewManager(skills []Skill) (*Manager, error) {
	m := &Manager{skills: map[string]Skill{}}
	for _, skill := range skills {
		if err := skill.Validate(filepath.Dir(skill.SourcePath)); err != nil {
			return nil, err
		}
		if _, exists := m.skills[skill.ID]; exists {
			return nil, fmt.Errorf("duplicate skill %q", skill.ID)
		}
		m.skills[skill.ID] = skill
	}
	return m, nil
}

func (m *Manager) Get(id string) (Skill, bool) {
	if m == nil {
		return Skill{}, false
	}
	skill, ok := m.skills[strings.TrimSpace(id)]
	return skill, ok
}

func (m *Manager) List() []Skill {
	if m == nil {
		return nil
	}
	out := make([]Skill, 0, len(m.skills))
	for _, skill := range m.skills {
		out = append(out, skill)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s Skill) InstructionBlock() string {
	return strings.TrimSpace(fmt.Sprintf(`# Loaded Skill: %s v%s

This skill is subordinate to the current agent profile. Use it only when relevant to the user task.

%s`, s.ID, s.Version, strings.TrimSpace(s.Instructions)))
}

func (s *Skill) normalize() {
	s.SchemaVersion = strings.TrimSpace(s.SchemaVersion)
	s.ID = strings.TrimSpace(s.ID)
	s.Name = strings.TrimSpace(s.Name)
	s.Version = strings.TrimSpace(s.Version)
	s.Description = strings.TrimSpace(s.Description)
	s.Activation.Mode = strings.TrimSpace(s.Activation.Mode)
	if s.Activation.Mode == "" {
		s.Activation.Mode = "explicit"
	}
	s.Requires.Tools = compactStrings(s.Requires.Tools)
	s.Requires.OptionalTools = compactStrings(s.Requires.OptionalTools)
	s.Requires.Secrets = compactStrings(s.Requires.Secrets)
	s.Requires.MCPServers = compactStrings(s.Requires.MCPServers)
	s.Context.Includes = compactStrings(s.Context.Includes)
	s.Context.Forbidden = compactStrings(s.Context.Forbidden)
	s.Verification.DefaultChecks = compactStrings(s.Verification.DefaultChecks)
	s.Artifacts.OutputDir = strings.TrimSpace(s.Artifacts.OutputDir)
	s.Artifacts.Retention = strings.TrimSpace(s.Artifacts.Retention)
	s.Instructions = strings.TrimSpace(s.Instructions)
}

func (s Skill) Validate(skillDir string) error {
	var errs []string
	if s.SchemaVersion != SchemaVersionSkillV0 {
		errs = append(errs, fmt.Sprintf("schema_version must be %s", SchemaVersionSkillV0))
	}
	if s.ID == "" {
		errs = append(errs, "id is required")
	}
	if s.Name == "" {
		errs = append(errs, "name is required")
	}
	if s.Version == "" {
		errs = append(errs, "version is required")
	}
	if s.Description == "" {
		errs = append(errs, "description is required")
	}
	if s.Instructions == "" {
		errs = append(errs, "instructions body is required")
	}
	if s.Activation.Mode != "explicit" {
		errs = append(errs, "activation.mode must be explicit in v0")
	}
	for _, path := range append(append([]string{}, s.Context.Includes...), s.Context.Forbidden...) {
		if err := validateSkillRelativePath(skillDir, path); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for _, path := range s.Context.Includes {
		if err := requireExistingPath(skillDir, path); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func parseSkillMarkdown(content string) (Skill, string, error) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return Skill{}, "", fmt.Errorf("YAML frontmatter is required")
	}
	rest := normalized[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return Skill{}, "", fmt.Errorf("YAML frontmatter closing delimiter is required")
	}
	var frontmatter Skill
	if err := yaml.Unmarshal([]byte(rest[:end]), &frontmatter); err != nil {
		return Skill{}, "", fmt.Errorf("YAML frontmatter is invalid: %w", err)
	}
	body := rest[end+len("\n---"):]
	if strings.HasPrefix(body, "\n") {
		body = body[1:]
	}
	return frontmatter, body, nil
}

func validateSkillRelativePath(skillDir, rawPath string) error {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return fmt.Errorf("skill context path is required")
	}
	if filepath.IsAbs(rawPath) {
		return fmt.Errorf("skill context path %q must be relative", rawPath)
	}
	cleanPath := filepath.Clean(rawPath)
	if cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return fmt.Errorf("skill context path %q must stay within skill directory", rawPath)
	}
	absRoot, err := filepath.Abs(skillDir)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(filepath.Join(skillDir, cleanPath))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("skill context path %q must stay within skill directory", rawPath)
	}
	return nil
}

func requireExistingPath(skillDir, rawPath string) error {
	path := filepath.Join(skillDir, filepath.Clean(rawPath))
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("skill context include %q is not readable: %w", rawPath, err)
	}
	return nil
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
