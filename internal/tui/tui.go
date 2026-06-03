package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	frt "github.com/ai-daming/flowdeck/internal/runtime"
)

type model struct {
	runtime     *frt.Service
	input       textinput.Model
	status      map[string]any
	agents      []map[string]any
	activeAgent string
	transcript  []transcriptEntry
	runs        []frt.TurnResponse
	output      string
	err         error
	loading     bool
	loadingMode string
}

type transcriptEntry struct {
	Role    string
	Content string
}

type initMsg struct {
	status map[string]any
	agents []map[string]any
	err    error
}

type runMsg struct {
	resp frt.TurnResponse
	err  error
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	labelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

func Run(ctx context.Context, runtime *frt.Service, initialAgentID string) error {
	input := textinput.New()
	input.Placeholder = "Talk to FlowDeck, or use /agent <id> <message>"
	input.Focus()
	input.CharLimit = 2000
	input.Width = 96
	m := model{runtime: runtime, input: input, activeAgent: strings.TrimSpace(initialAgentID)}
	_, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	return err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.load)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" || m.loading {
				return m, nil
			}
			if handled, cmd := m.applyCommand(text); handled {
				m.input.SetValue("")
				return m, cmd
			}
			if strings.TrimSpace(m.activeAgent) == "" {
				m.loading = true
				m.loadingMode = "default"
				m.output = ""
				m.err = nil
				m.addTranscript("You", text)
				return m, m.runAgent("", text)
			}
			m.loading = true
			m.loadingMode = "agent"
			m.output = ""
			m.err = nil
			m.addTranscript("You -> "+m.activeAgent, text)
			return m, m.runAgent(m.activeAgent, text)
		}
	case initMsg:
		m.status = msg.status
		m.agents = msg.agents
		m.err = msg.err
		if msg.err == nil && strings.TrimSpace(m.activeAgent) != "" && !hasAgent(m.agents, m.activeAgent) {
			m.err = fmt.Errorf("agent profile %q not found", m.activeAgent)
			m.activeAgent = ""
		}
	case runMsg:
		m.loading = false
		m.loadingMode = ""
		m.err = msg.err
		m.input.SetValue("")
		if msg.err != nil {
			m.output = msg.err.Error()
			m.addTranscript("Error", msg.err.Error())
		} else {
			m.runs = append([]frt.TurnResponse{msg.resp}, m.runs...)
			m.output = msg.resp.FinalResponse
			m.addTranscript(msg.resp.AgentID, msg.resp.FinalResponse)
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("FlowDeck TUI"))
	b.WriteString("\n")
	if m.status != nil {
		b.WriteString(labelStyle.Render("Model: "))
		b.WriteString(modelStatusLabel(m.status))
		b.WriteString("\n")
	}
	b.WriteString(labelStyle.Render("Mode: "))
	if strings.TrimSpace(m.activeAgent) == "" {
		b.WriteString("default agent")
		if id := defaultAgentID(m.status); id != "" {
			b.WriteString(" ")
			b.WriteString(id)
		}
	} else {
		b.WriteString("agent ")
		b.WriteString(agentSelectionLabel(m.agents, m.activeAgent))
	}
	b.WriteString("\n\n")
	if len(m.transcript) > 0 {
		b.WriteString(renderTranscript(m.transcript, 12))
		b.WriteString("\n\n")
	}
	b.WriteString(m.input.View())
	b.WriteString("\n")
	if m.loading {
		if m.loadingMode == "default" {
			b.WriteString("Thinking...\n")
		} else {
			b.WriteString("Running agent...\n")
		}
	}
	if len(m.runs) > 0 {
		b.WriteString("\n")
		b.WriteString(labelStyle.Render("Last runs"))
		b.WriteString("\n")
		for i, run := range m.runs {
			if i >= 5 {
				break
			}
			b.WriteString(fmt.Sprintf("- %s %s %s\n", run.RunID, run.Status, run.VerificationResult.Status))
		}
	}
	b.WriteString("\n")
	b.WriteString(labelStyle.Render("Enter: send  /agents  /agent <id> <message>  /use <id>  /exit-agent  Esc/Ctrl-C: quit"))
	return b.String()
}

func (m model) load() tea.Msg {
	if m.runtime == nil {
		return initMsg{err: fmt.Errorf("runtime is required")}
	}
	status := m.runtime.Status()
	profiles := m.runtime.Agents()
	agents := make([]map[string]any, 0, len(profiles))
	for _, profile := range profiles {
		agents = append(agents, map[string]any{
			"id":          profile.ID,
			"name":        profile.Name,
			"description": profile.Description,
			"tools":       append([]string(nil), profile.Permissions.Tools...),
		})
	}
	return initMsg{status: status, agents: agents}
}

func (m *model) applyCommand(text string) (bool, tea.Cmd) {
	switch {
	case text == "/agents":
		m.err = nil
		m.output = agentListText(m.agents)
		m.addTranscript("FlowDeck", m.output)
		return true, nil
	case text == "/help":
		m.err = nil
		m.output = helpText()
		m.addTranscript("FlowDeck", m.output)
		return true, nil
	case strings.HasPrefix(text, "/agent "):
		return true, m.applyAgentCommand(strings.TrimSpace(strings.TrimPrefix(text, "/agent ")))
	case text == "/agent":
		m.err = nil
		m.output = "Usage: /agent <id> <message>\n\n" + agentListText(m.agents)
		m.addTranscript("FlowDeck", m.output)
		return true, nil
	case strings.HasPrefix(text, "/use "):
		return true, m.applyUseCommand(strings.TrimSpace(strings.TrimPrefix(text, "/use ")))
	case text == "/use":
		m.err = nil
		m.output = "Usage: /use <agent-id>\n\n" + agentListText(m.agents)
		m.addTranscript("FlowDeck", m.output)
		return true, nil
	case text == "/exit-agent":
		m.activeAgent = ""
		m.err = nil
		m.output = "Mode: default agent"
		m.addTranscript("FlowDeck", m.output)
		return true, nil
	case text == "/flows" || text == "/flow":
		m.err = nil
		m.output = "Flow entrypoints are not enabled in Phase 1. Use /agents or /agent <id> <message>."
		m.addTranscript("FlowDeck", m.output)
		return true, nil
	default:
		return false, nil
	}
}

func (m *model) applyAgentCommand(args string) tea.Cmd {
	id, message := splitCommandArgs(args)
	if id == "" {
		m.err = nil
		m.output = "Usage: /agent <id> <message>\n\n" + agentListText(m.agents)
		m.addTranscript("FlowDeck", m.output)
		return nil
	}
	if !hasAgent(m.agents, id) {
		m.err = fmt.Errorf("agent profile %q not found", id)
		m.output = m.err.Error() + "\n\n" + agentListText(m.agents)
		m.addTranscript("Error", m.output)
		return nil
	}
	if message == "" {
		m.activeAgent = id
		m.err = nil
		m.output = "Mode: agent " + agentSelectionLabel(m.agents, id)
		m.addTranscript("FlowDeck", m.output)
		return nil
	}
	m.loading = true
	m.loadingMode = "agent"
	m.err = nil
	m.output = ""
	m.addTranscript("You -> "+id, message)
	return m.runAgent(id, message)
}

func (m *model) applyUseCommand(id string) tea.Cmd {
	if id == "" {
		m.err = nil
		m.output = "Usage: /use <agent-id>\n\n" + agentListText(m.agents)
		m.addTranscript("FlowDeck", m.output)
		return nil
	}
	if !hasAgent(m.agents, id) {
		m.err = fmt.Errorf("agent profile %q not found", id)
		m.output = m.err.Error() + "\n\n" + agentListText(m.agents)
		m.addTranscript("Error", m.output)
		return nil
	}
	m.activeAgent = id
	m.err = nil
	m.output = "Mode: agent " + agentSelectionLabel(m.agents, id)
	m.addTranscript("FlowDeck", m.output)
	return nil
}

func (m *model) addTranscript(role, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	m.transcript = append(m.transcript, transcriptEntry{Role: strings.TrimSpace(role), Content: content})
}

func renderTranscript(entries []transcriptEntry, max int) string {
	if max > 0 && len(entries) > max {
		entries = entries[len(entries)-max:]
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		role := strings.TrimSpace(entry.Role)
		if role == "" {
			role = "Unknown"
		}
		content := strings.TrimSpace(entry.Content)
		if content == "" {
			continue
		}
		parts := strings.Split(content, "\n")
		lines = append(lines, fmt.Sprintf("%s: %s", role, parts[0]))
		for _, part := range parts[1:] {
			lines = append(lines, "  "+part)
		}
	}
	return strings.Join(lines, "\n")
}

func (m model) runAgent(agentID, message string) tea.Cmd {
	return func() tea.Msg {
		if m.runtime == nil {
			return runMsg{err: fmt.Errorf("runtime is required")}
		}
		resp, err := m.runtime.RunAgent(context.Background(), frt.TurnRequest{
			AgentID: agentID,
			Message: message,
			Channel: "tui",
			UserID:  "local-tui",
		})
		return runMsg{resp: resp, err: err}
	}
}

func hasAgent(agents []map[string]any, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	for _, agent := range agents {
		if agentID(agent) == id {
			return true
		}
	}
	return false
}

func agentSelectionLabel(agents []map[string]any, id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "(none)"
	}
	for _, agent := range agents {
		if agentID(agent) == id {
			if name := agentString(agent, "name"); name != "" {
				return fmt.Sprintf("%s (%s)", id, name)
			}
			return id
		}
	}
	return id + " (not loaded)"
}

func agentListText(agents []map[string]any) string {
	if len(agents) == 0 {
		return "No agent profiles returned by runtime."
	}
	lines := []string{"Available agents:"}
	for _, agent := range agents {
		id := agentID(agent)
		if id == "" {
			continue
		}
		line := "- " + id
		if name := agentString(agent, "name"); name != "" {
			line += " (" + name + ")"
		}
		if desc := agentString(agent, "description"); desc != "" {
			line += ": " + desc
		}
		if tools := agentStringList(agent, "tools"); len(tools) > 0 {
			line += " [tools: " + strings.Join(tools, ", ") + "]"
		}
		lines = append(lines, line)
	}
	if len(lines) == 1 {
		return "No usable agent profiles returned by runtime."
	}
	return strings.Join(lines, "\n")
}

func splitCommandArgs(args string) (string, string) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", ""
	}
	id := fields[0]
	message := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), id))
	return id, message
}

func helpText() string {
	return strings.Join([]string{
		"Commands:",
		"/agents - list runtime agent profiles",
		"/agent <id> <message> - call an agent once",
		"/agent <id> - enter agent mode",
		"/use <id> - enter agent mode",
		"/exit-agent - return to the default agent",
		"/flows - list flow entrypoints when enabled",
	}, "\n")
}

func agentID(agent map[string]any) string {
	return agentString(agent, "id")
}

func agentString(agent map[string]any, key string) string {
	if agent == nil {
		return ""
	}
	value, ok := agent[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func agentStringList(agent map[string]any, key string) []string {
	if agent == nil {
		return nil
	}
	value, ok := agent[key]
	if !ok {
		return nil
	}
	switch v := value.(type) {
	case []string:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if trimmed := strings.TrimSpace(fmt.Sprint(item)); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	default:
		return nil
	}
}

func modelStatusLabel(status map[string]any) string {
	if statusBool(status, "mock_model") {
		return "mock"
	}
	return "DeepSeek"
}

func defaultAgentID(status map[string]any) string {
	value, ok := status["default_agent"]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func statusBool(status map[string]any, key string) bool {
	value, ok := status[key]
	if !ok {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	default:
		return false
	}
}
