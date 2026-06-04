package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ai-daming/flowdeck/internal/agents"
	"github.com/ai-daming/flowdeck/internal/entrypoints"
	"github.com/ai-daming/flowdeck/internal/routing"
)

const defaultConfigPath = "flowdeck.yaml"

type runtimeConfigFile struct {
	Workspace      string `yaml:"workspace"`
	DefaultAgentID string `yaml:"default_agent"`
	RunRoot        string `yaml:"run_root"`
	StateRoot      string `yaml:"state_root"`
	Routes         string `yaml:"routes"`
	Entrypoints    string `yaml:"entrypoints"`
}

type routesConfigFile struct {
	DefaultAgentID string       `yaml:"default_agent"`
	Routes         []routeEntry `yaml:"routes"`
	Rules          []routeEntry `yaml:"rules"`
}

type entrypointsConfigFile struct {
	Entrypoints []entrypoints.Definition `yaml:"entrypoints"`
}

type routeEntry struct {
	Channel       string                `yaml:"channel"`
	Agent         string                `yaml:"agent"`
	AgentID       string                `yaml:"agent_id"`
	Match         routeMatch            `yaml:"match"`
	SessionPolicy routing.SessionPolicy `yaml:"session"`
}

type routeMatch struct {
	Channel string `yaml:"channel"`
}

type resolvedRuntimeConfig struct {
	ConfigPath        string
	ConfigLoaded      bool
	WorkspaceExplicit bool
	WorkspaceRoot     string
	DefaultAgentID    string
	RunRoot           string
	Routes            []routing.Rule
	Entrypoints       []entrypoints.Definition
}

func resolveRuntimeConfig(cfg Config) (resolvedRuntimeConfig, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return resolvedRuntimeConfig{}, err
	}

	configInput := strings.TrimSpace(cfg.ConfigPath)
	if configInput == "" {
		configInput = defaultConfigPath
	}
	configPath := resolveRelativePath(cwd, configInput)
	configFile, configLoaded, err := readRuntimeConfigFile(configPath, isDefaultConfigInput(cfg.ConfigPath))
	if err != nil {
		return resolvedRuntimeConfig{}, err
	}

	baseDir := cwd
	if configLoaded {
		baseDir = filepath.Dir(configPath)
	}

	workspaceExplicit := strings.TrimSpace(cfg.WorkspaceRoot) != ""
	workspace := strings.TrimSpace(cfg.WorkspaceRoot)
	if workspace == "" {
		workspace = strings.TrimSpace(configFile.Workspace)
	}
	if workspace == "" {
		if configLoaded {
			workspace = "workspace"
		} else {
			workspace = cwd
		}
	}
	workspace = resolveRelativePath(baseDir, workspace)

	runRoot := strings.TrimSpace(cfg.RunRoot)
	if runRoot == "" {
		runRoot = strings.TrimSpace(configFile.RunRoot)
	}
	if runRoot == "" && strings.TrimSpace(configFile.StateRoot) != "" {
		runRoot = filepath.Join(strings.TrimSpace(configFile.StateRoot), "runs")
	}
	if runRoot == "" {
		runRoot = ".flowdeck/runs"
	}
	runRoot = resolveRelativePath(baseDir, runRoot)

	routesPath := strings.TrimSpace(configFile.Routes)
	routesRequired := false
	if routesPath != "" {
		routesPath = resolveRelativePath(baseDir, routesPath)
		routesRequired = true
	} else if configLoaded {
		routesPath = filepath.Join(workspace, "routes.yaml")
	}
	routes, routeDefaultAgentID, err := readRoutesFile(routesPath, routesRequired)
	if err != nil {
		return resolvedRuntimeConfig{}, err
	}
	entrypointsPath := strings.TrimSpace(configFile.Entrypoints)
	entrypointsRequired := false
	if entrypointsPath != "" {
		entrypointsPath = resolveRelativePath(baseDir, entrypointsPath)
		entrypointsRequired = true
	} else if configLoaded {
		entrypointsPath = filepath.Join(workspace, "entrypoints.yaml")
	}
	entrypointDefs, err := readEntrypointsFile(entrypointsPath, entrypointsRequired)
	if err != nil {
		return resolvedRuntimeConfig{}, err
	}

	defaultAgentID := strings.TrimSpace(cfg.DefaultAgentID)
	if defaultAgentID == "" {
		defaultAgentID = strings.TrimSpace(configFile.DefaultAgentID)
	}
	if defaultAgentID == "" {
		defaultAgentID = routeDefaultAgentID
	}
	if defaultAgentID == "" {
		defaultAgentID = agents.DefaultAgentID
	}

	return resolvedRuntimeConfig{
		ConfigPath:        configPath,
		ConfigLoaded:      configLoaded,
		WorkspaceExplicit: workspaceExplicit,
		WorkspaceRoot:     workspace,
		DefaultAgentID:    defaultAgentID,
		RunRoot:           runRoot,
		Routes:            routes,
		Entrypoints:       entrypointDefs,
	}, nil
}

func readRuntimeConfigFile(path string, optional bool) (runtimeConfigFile, bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return runtimeConfigFile{}, false, nil
		}
		return runtimeConfigFile{}, false, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg runtimeConfigFile
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return runtimeConfigFile{}, false, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, true, nil
}

func readRoutesFile(path string, required bool) ([]routing.Rule, string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, "", nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("read routes %s: %w", path, err)
	}
	var cfg routesConfigFile
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return nil, "", fmt.Errorf("parse routes %s: %w", path, err)
	}

	entries := append([]routeEntry{}, cfg.Routes...)
	entries = append(entries, cfg.Rules...)
	rules := make([]routing.Rule, 0, len(entries))
	for _, entry := range entries {
		channel := strings.TrimSpace(entry.Channel)
		if channel == "" {
			channel = strings.TrimSpace(entry.Match.Channel)
		}
		agentID := strings.TrimSpace(entry.AgentID)
		if agentID == "" {
			agentID = strings.TrimSpace(entry.Agent)
		}
		if channel == "" || agentID == "" {
			continue
		}
		rules = append(rules, routing.Rule{
			Channel:       channel,
			AgentID:       agentID,
			SessionPolicy: entry.SessionPolicy,
		})
	}
	return rules, strings.TrimSpace(cfg.DefaultAgentID), nil
}

func readEntrypointsFile(path string, required bool) ([]entrypoints.Definition, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read entrypoints %s: %w", path, err)
	}
	var cfg entrypointsConfigFile
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return nil, fmt.Errorf("parse entrypoints %s: %w", path, err)
	}
	return cfg.Entrypoints, nil
}

func loadAgentManager(resolved resolvedRuntimeConfig) (*agents.Manager, string, error) {
	if resolved.ConfigLoaded || resolved.WorkspaceExplicit {
		manager, err := agents.LoadFromWorkspace(resolved.WorkspaceRoot)
		if err != nil {
			return nil, "", err
		}
		return manager, "workspace", nil
	}
	manager, err := agents.NewBuiltinManager()
	if err != nil {
		return nil, "", err
	}
	return manager, "builtin", nil
}

func sessionPolicyForProfile(profile agents.Profile, fallback routing.SessionPolicy) routing.SessionPolicy {
	if len(profile.Session.Dimensions) == 0 && len(profile.Session.IdentityLinks) == 0 {
		return routing.NormalizeSessionPolicy(fallback)
	}
	return routing.NormalizeSessionPolicy(routing.SessionPolicy{
		Dimensions:    profile.Session.Dimensions,
		IdentityLinks: profile.Session.IdentityLinks,
	})
}

func resolveRelativePath(baseDir, path string) string {
	path = strings.TrimSpace(path)
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	abs, err := filepath.Abs(filepath.Join(baseDir, path))
	if err != nil {
		return filepath.Clean(filepath.Join(baseDir, path))
	}
	return abs
}

func isDefaultConfigInput(path string) bool {
	path = strings.TrimSpace(path)
	return path == "" || filepath.Clean(path) == defaultConfigPath
}
