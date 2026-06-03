package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	frt "github.com/ai-daming/flowdeck/internal/runtime"
)

type model struct {
	runtime     *frt.Service
	input       textinput.Model
	width       int
	height      int
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
	titleStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	labelStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	mutedStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	errorStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	successStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	userStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117"))
	assistantStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("150"))
	panelBorderStyle = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("238")).Padding(1, 2)
	activePanelStyle = panelBorderStyle.BorderForeground(lipgloss.Color("81"))
	headerStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255")).Background(lipgloss.Color("235")).Padding(0, 1)
	footerStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Background(lipgloss.Color("235")).Padding(0, 1)
	pillStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("238")).Padding(0, 1)
)

func Run(ctx context.Context, runtime *frt.Service, initialAgentID string) error {
	input := textinput.New()
	input.Placeholder = "Talk to FlowDeck, or use /agent <id> <message>"
	input.Focus()
	input.CharLimit = 2000
	input.Width = 96
	m := model{runtime: runtime, input: input, width: 110, height: 32, activeAgent: strings.TrimSpace(initialAgentID)}
	_, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	return err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.load)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = inputWidth(msg.Width)
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
	width := m.width
	if width < 72 {
		width = 72
	}
	height := m.height
	if height < 20 {
		height = 20
	}

	sidebarWidth := clamp(width/4, 24, 34)
	mainWidth := width - sidebarWidth - 1
	bodyHeight := height - 3
	if bodyHeight < 16 {
		bodyHeight = 16
	}
	m.input.Width = maxInt(24, mainWidth-8)

	header := renderHeader(m, width)
	sidebar := renderSidebar(m, sidebarWidth, bodyHeight)
	main := renderConversation(m, mainWidth, bodyHeight)
	footer := renderFooter(m, width)
	return strings.TrimRight(strings.Join([]string{
		header,
		lipgloss.JoinHorizontal(lipgloss.Top, sidebar, main),
		footer,
	}, "\n"), "\n")
}

func renderHeader(m model, width int) string {
	mode := modeLabel(m)
	left := titleStyle.Render("FlowDeck TUI")
	right := strings.Join([]string{
		"Model: " + modelStatusLabel(m.status),
		"Mode: " + mode,
	}, "  ")
	gap := maxInt(1, width-lipgloss.Width(left)-lipgloss.Width(right)-2)
	return headerStyle.Width(width).Render(left + strings.Repeat(" ", gap) + mutedStyle.Render(right))
}

func renderSidebar(m model, width, height int) string {
	selected := selectedAgentID(m)
	lines := []string{titleStyle.Render("Agents")}
	if len(m.agents) == 0 {
		lines = append(lines, mutedStyle.Render("No agents loaded"))
	} else {
		for _, agent := range m.agents {
			id := agentID(agent)
			if id == "" {
				continue
			}
			marker := " "
			rowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
			if id == selected {
				marker = ">"
				rowStyle = successStyle.Bold(true)
			}
			lines = append(lines, rowStyle.Render(fmt.Sprintf("%s %s", marker, truncate(id, width-8))))
			if name := agentString(agent, "name"); name != "" && name != id {
				lines = append(lines, mutedStyle.Render("  "+truncate(name, width-8)))
			}
			if tools := agentStringList(agent, "tools"); len(tools) > 0 {
				lines = append(lines, mutedStyle.Render(fmt.Sprintf("  tools %d", len(tools))))
			}
		}
	}

	lines = append(lines, "", titleStyle.Render("Runs"))
	if len(m.runs) == 0 {
		lines = append(lines, mutedStyle.Render("No runs yet"))
	} else {
		for i, run := range m.runs {
			if i >= 4 {
				break
			}
			status := run.Status
			if run.VerificationResult.Status != "" {
				status += "/" + run.VerificationResult.Status
			}
			lines = append(lines, truncate(run.AgentID, width-8))
			lines = append(lines, mutedStyle.Render("  "+truncate(status, width-8)))
		}
	}
	return panelBorderStyle.Width(width - 5).Height(height - 4).Render(strings.Join(lines, "\n"))
}

func renderConversation(m model, width, height int) string {
	var sections []string
	title := titleStyle.Render("Conversation")
	if m.loading {
		title += " " + pillStyle.Render(loadingLabel(m.loadingMode))
	}
	sections = append(sections, title)

	bodyWidth := maxInt(28, width-8)
	if len(m.transcript) == 0 {
		sections = append(sections, mutedStyle.Render("Start with a message, or use /agents and /use <id>."))
	} else {
		sections = append(sections, renderTranscriptBlocks(m.transcript, 10, bodyWidth))
	}
	if trace := renderRunTrace(lastRun(m), bodyWidth); trace != "" {
		sections = append(sections, "", trace)
	} else if m.loading {
		sections = append(sections, "", titleStyle.Render("Trace"), mutedStyle.Render("Waiting for tool callbacks..."))
	}

	if m.err != nil {
		sections = append(sections, errorStyle.Render(m.err.Error()))
	}
	sections = append(sections, "", labelStyle.Render("Message"), m.input.View())
	return activePanelStyle.Width(width - 5).Height(height - 4).Render(strings.Join(sections, "\n"))
}

func renderFooter(m model, width int) string {
	status := "Enter send  /agents list  /use <id> switch  /exit-agent default  Trace shows tools/audit  Esc quit"
	if m.err != nil {
		status = "Error: " + m.err.Error()
	}
	return footerStyle.Width(width).Render(truncate(status, width-2))
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

func renderTranscriptBlocks(entries []transcriptEntry, max, width int) string {
	if max > 0 && len(entries) > max {
		entries = entries[len(entries)-max:]
	}
	blocks := make([]string, 0, len(entries))
	for _, entry := range entries {
		role := strings.TrimSpace(entry.Role)
		if role == "" {
			role = "Unknown"
		}
		content := strings.TrimSpace(entry.Content)
		if content == "" {
			continue
		}
		header := userStyle.Render(role)
		lower := strings.ToLower(role)
		switch {
		case strings.Contains(lower, "error"):
			header = errorStyle.Bold(true).Render(role)
		case strings.Contains(lower, "flowdeck") || strings.Contains(lower, "assistant"):
			header = assistantStyle.Render(role)
		}
		blocks = append(blocks, header+"\n"+mutedStyle.Render(indent(wrapText(content, width), "  ")))
	}
	return strings.Join(blocks, "\n\n")
}

func renderRunTrace(run *frt.TurnResponse, width int) string {
	if run == nil {
		return ""
	}
	lines := []string{
		titleStyle.Render("Trace"),
		mutedStyle.Render(truncate(fmt.Sprintf("run %s  route %s", run.RunID, emptyDash(run.RouteMatchedBy)), width)),
	}
	if len(run.ToolCalls) == 0 {
		lines = append(lines, mutedStyle.Render("tools: none"))
	} else {
		lines = append(lines, labelStyle.Render("tools"))
		for i, call := range run.ToolCalls {
			if i >= 4 {
				lines = append(lines, mutedStyle.Render(fmt.Sprintf("  ... %d more", len(run.ToolCalls)-i)))
				break
			}
			lines = append(lines, renderToolCallLine(call, width))
		}
	}
	if len(run.AuditEvents) > 0 {
		lines = append(lines, labelStyle.Render("audit"))
		for i, event := range run.AuditEvents {
			if i >= 3 {
				lines = append(lines, mutedStyle.Render(fmt.Sprintf("  ... %d more", len(run.AuditEvents)-i)))
				break
			}
			allowed := "deny"
			style := errorStyle
			if event.Allowed {
				allowed = "allow"
				style = successStyle
			}
			text := fmt.Sprintf("  %s %s -> %s", allowed, emptyDash(event.Action), emptyDash(event.Target))
			if event.Reason != "" {
				text += " (" + event.Reason + ")"
			}
			lines = append(lines, style.Render(truncate(text, width)))
		}
	}
	if len(run.Events) > 0 {
		lines = append(lines, labelStyle.Render("events"))
		for _, event := range compactEvents(run.Events, 4) {
			text := fmt.Sprintf("  %s %s", emptyDash(event.Kind), emptyDash(event.Source))
			if event.Message != "" {
				text += " - " + event.Message
			}
			lines = append(lines, mutedStyle.Render(truncate(text, width)))
		}
	}
	return strings.Join(lines, "\n")
}

func renderToolCallLine(call frt.ToolCallRecord, width int) string {
	status := successStyle.Render("ok")
	if call.Error != "" {
		status = errorStyle.Render("err")
	}
	input := summarizeMap(call.Input, []string{"path", "command", "cwd", "action", "old_text", "new_text"})
	output := summarizeMap(call.Output, []string{"path", "bytes", "entries", "exit_code", "stdout", "stderr", "content"})
	if output == "" && call.Error != "" {
		output = call.Error
	}
	line := fmt.Sprintf("  %s %s", status, call.Name)
	if input != "" {
		line += " in{" + input + "}"
	}
	if output != "" {
		line += " out{" + output + "}"
	}
	return truncate(line, width)
}

func summarizeMap(values map[string]any, preferred []string) string {
	if len(values) == 0 {
		return ""
	}
	seen := map[string]bool{}
	parts := make([]string, 0, minLen(len(values), 4))
	for _, key := range preferred {
		if value, ok := values[key]; ok {
			parts = append(parts, key+"="+summarizeValue(key, value))
			seen[key] = true
		}
		if len(parts) >= 4 {
			return strings.Join(parts, ", ")
		}
	}
	var keys []string
	for key := range values {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, key+"="+summarizeValue(key, values[key]))
		if len(parts) >= 4 {
			break
		}
	}
	return strings.Join(parts, ", ")
}

func summarizeValue(key string, value any) string {
	switch v := value.(type) {
	case string:
		if key == "content" {
			return fmt.Sprintf("<%d chars>", utf8.RuneCountInString(v))
		}
		return quoteSummary(v, 48)
	case []map[string]any:
		return fmt.Sprintf("%d items", len(v))
	case []any:
		return fmt.Sprintf("%d items", len(v))
	case nil:
		return "nil"
	default:
		return quoteSummary(fmt.Sprint(v), 48)
	}
}

func quoteSummary(text string, max int) string {
	text = strings.ReplaceAll(strings.TrimSpace(text), "\n", "\\n")
	return `"` + truncate(text, max) + `"`
}

func compactEvents(events []frt.RuntimeEvent, max int) []frt.RuntimeEvent {
	if len(events) <= max {
		return events
	}
	if max <= 0 {
		return nil
	}
	if max == 1 {
		return events[len(events)-1:]
	}
	out := make([]frt.RuntimeEvent, 0, max)
	out = append(out, events[0])
	out = append(out, events[len(events)-(max-1):]...)
	return out
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

func modeLabel(m model) string {
	if strings.TrimSpace(m.activeAgent) == "" {
		if id := defaultAgentID(m.status); id != "" {
			return "default agent " + id
		}
		return "default agent"
	}
	return "agent " + agentSelectionLabel(m.agents, m.activeAgent)
}

func selectedAgentID(m model) string {
	if id := strings.TrimSpace(m.activeAgent); id != "" {
		return id
	}
	return defaultAgentID(m.status)
}

func lastRun(m model) *frt.TurnResponse {
	if len(m.runs) == 0 {
		return nil
	}
	return &m.runs[0]
}

func loadingLabel(mode string) string {
	if mode == "default" {
		return "thinking"
	}
	return "running"
}

func emptyDash(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "-"
	}
	return text
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

func inputWidth(width int) int {
	return maxInt(24, width-42)
}

func wrapText(text string, width int) string {
	width = maxInt(12, width)
	var out []string
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		for lipgloss.Width(line) > width {
			head, tail := splitDisplayWidth(line, width)
			if head == "" {
				break
			}
			out = append(out, strings.TrimSpace(head))
			line = strings.TrimSpace(tail)
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func indent(text, prefix string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func truncate(text string, width int) string {
	text = strings.TrimSpace(text)
	if width <= 0 || lipgloss.Width(text) <= width {
		return text
	}
	if width <= 3 {
		head, _ := splitDisplayWidth(text, width)
		return head
	}
	head, _ := splitDisplayWidth(text, width-3)
	return strings.TrimSpace(head) + "..."
}

func splitDisplayWidth(text string, width int) (string, string) {
	if width <= 0 || text == "" {
		return "", text
	}
	var b strings.Builder
	used := 0
	for index, r := range text {
		w := lipgloss.Width(string(r))
		if used+w > width {
			return b.String(), text[index:]
		}
		b.WriteRune(r)
		used += w
	}
	return b.String(), ""
}

func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
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
