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

	frt "github.com/ai-daming/xira/internal/runtime"
)

type model struct {
	runtime      *frt.Service
	input        textinput.Model
	width        int
	height       int
	status       map[string]any
	agents       []map[string]any
	activeAgent  string
	transcript   []transcriptEntry
	runs         []frt.TurnResponse
	output       string
	err          error
	loading      bool
	loadingMode  string
	runningAgent string
	showTrace    bool
	traceEvents  []frt.RuntimeEvent
	traceSub     <-chan frt.RuntimeEvent
	traceCancel  context.CancelFunc
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

type traceEventMsg struct {
	event frt.RuntimeEvent
}

type traceClosedMsg struct{}

var (
	titleStyle         = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	labelStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("249"))
	mutedStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	errorStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	successStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	userStyle          = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("159"))
	assistantStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("194"))
	userTextStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("255"))
	assistantTextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	codeStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("229")).Background(lipgloss.Color("236")).Padding(0, 1)
	quoteStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Italic(true)
	panelBorderStyle   = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("238")).Padding(1, 2)
	activePanelStyle   = panelBorderStyle.BorderForeground(lipgloss.Color("81"))
	headerStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255")).Background(lipgloss.Color("235")).Padding(0, 1)
	runbarStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("232")).Padding(0, 1)
	footerStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Background(lipgloss.Color("235")).Padding(0, 1)
	pillStyle          = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("63")).Padding(0, 1)
)

func Run(ctx context.Context, runtime *frt.Service, initialAgentID string) error {
	input := textinput.New()
	input.Placeholder = "Talk to Xira, or use /agent <id> <message>"
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
				m.runningAgent = defaultAgentID(m.status)
				m.output = ""
				m.err = nil
				m.addTranscript("You", text)
				m.input.SetValue("")
				traceCmd := m.beginTrace()
				return m, tea.Batch(traceCmd, m.runAgent("", text))
			}
			m.loading = true
			m.loadingMode = "agent"
			m.runningAgent = strings.TrimSpace(m.activeAgent)
			m.output = ""
			m.err = nil
			m.addTranscript("You -> "+m.activeAgent, text)
			m.input.SetValue("")
			traceCmd := m.beginTrace()
			return m, tea.Batch(traceCmd, m.runAgent(m.activeAgent, text))
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
		m.runningAgent = ""
		m.stopTrace()
		m.err = msg.err
		m.input.SetValue("")
		if msg.resp.RunID != "" {
			m.runs = append([]frt.TurnResponse{msg.resp}, m.runs...)
		}
		if msg.err != nil {
			m.output = msg.err.Error()
			m.addTranscript("Error", msg.err.Error())
		} else {
			m.output = msg.resp.FinalResponse
			m.addTranscript(msg.resp.AgentID, msg.resp.FinalResponse)
		}
	case traceEventMsg:
		m.traceEvents = appendTraceEvent(m.traceEvents, msg.event, 80)
		if m.loading {
			return m, m.watchTrace()
		}
	case traceClosedMsg:
		m.traceSub = nil
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
	m.input.Width = inputWidth(width)

	header := renderHeader(m, width)
	runbar := renderRunStatus(m, width)
	main := renderConversation(m, width)
	composer := renderComposer(m, width)
	footer := renderFooter(m, width)
	return strings.TrimRight(strings.Join([]string{
		header,
		runbar,
		main,
		composer,
		footer,
	}, "\n"), "\n")
}

func renderHeader(m model, width int) string {
	mode := modeLabel(m)
	left := titleStyle.Render("Xira TUI")
	right := strings.Join([]string{
		"Model: " + modelStatusLabel(m.status),
		"Mode: " + mode,
	}, "  ")
	gap := maxInt(1, width-lipgloss.Width(left)-lipgloss.Width(right)-2)
	return headerStyle.Width(width).Render(left + strings.Repeat(" ", gap) + mutedStyle.Render(right))
}

func renderRunStatus(m model, width int) string {
	text := "Current run  "
	style := runbarStyle
	switch {
	case m.loading:
		agent := runningAgentID(m)
		text += fmt.Sprintf("RUNNING %s · %s · steps %d · audit persisted to .xira/runs", loadingLabel(m.loadingMode), agent, len(liveActivitySteps(m.traceEvents, agent)))
		style = style.Foreground(lipgloss.Color("230")).Background(lipgloss.Color("63"))
	case lastRun(m) != nil:
		run := lastRun(m)
		status := emptyDash(run.Status)
		if run.VerificationResult.Status != "" {
			status += "/" + run.VerificationResult.Status
		}
		text += fmt.Sprintf("%s · %s · steps %d  artifacts %d · audit persisted to .xira/runs", status, emptyDash(run.AgentID), len(runActivitySteps(run)), len(run.Artifacts))
		if run.Status == "failed" {
			style = style.Foreground(lipgloss.Color("203"))
		}
	default:
		text += fmt.Sprintf("idle · %s · audit will persist to .xira/runs", modeLabel(m))
	}
	return style.Width(width).Render(truncate(text, width-2))
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
	lines = append(lines, "", titleStyle.Render("Activity"))
	lines = append(lines, renderActivity(m, width-8)...)
	return panelBorderStyle.Width(width - 5).Height(height - 4).Render(strings.Join(lines, "\n"))
}

func renderConversation(m model, width int) string {
	var sections []string
	sections = append(sections, titleStyle.Render("Conversation"))

	bodyWidth := maxInt(28, width-4)
	if len(m.transcript) == 0 {
		sections = append(sections, mutedStyle.Render("Start with a message, or use /agents and /use <id>."))
	} else {
		sections = append(sections, renderTranscriptBlocks(m.transcript, 0, bodyWidth))
	}
	if m.loading {
		sections = append(sections, "", renderInlineLiveActivity(m, bodyWidth))
	} else if summary := renderInlineRunSummary(lastRun(m), bodyWidth); summary != "" {
		sections = append(sections, "", summary)
	}
	if m.showTrace {
		if m.loading {
			sections = append(sections, "", renderLiveTrace(m.traceEvents, bodyWidth))
		} else if trace := renderRunTrace(lastRun(m), bodyWidth); trace != "" {
			sections = append(sections, "", trace)
		} else if len(m.traceEvents) > 0 {
			sections = append(sections, "", renderLiveTrace(m.traceEvents, bodyWidth))
		}
	}

	if m.err != nil {
		sections = append(sections, errorStyle.Render(m.err.Error()))
	}
	if m.loading {
		sections = append(sections, "", renderActiveStatus(m, bodyWidth))
	}
	return strings.Join(sections, "\n")
}

func renderComposer(m model, width int) string {
	return strings.Join([]string{
		labelStyle.Render(composerLabel(m)),
		m.input.View(),
	}, "\n")
}

func renderFooter(m model, width int) string {
	status := "Enter send  /trace inspect raw events  /agents list  /use <id> switch  Esc quit"
	if m.loading {
		status = "RUNNING: " + loadingLabel(m.loadingMode) + "  activity shows steps  /trace opens raw inspector"
	}
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
		m.addTranscript("Xira", m.output)
		return true, nil
	case text == "/help":
		m.err = nil
		m.output = helpText()
		m.addTranscript("Xira", m.output)
		return true, nil
	case text == "/trace":
		m.showTrace = !m.showTrace
		m.err = nil
		m.output = traceModeText(m.showTrace)
		return true, nil
	case text == "/trace on":
		m.showTrace = true
		m.err = nil
		m.output = traceModeText(m.showTrace)
		return true, nil
	case text == "/trace off":
		m.showTrace = false
		m.err = nil
		m.output = traceModeText(m.showTrace)
		return true, nil
	case strings.HasPrefix(text, "/agent "):
		return true, m.applyAgentCommand(strings.TrimSpace(strings.TrimPrefix(text, "/agent ")))
	case text == "/agent":
		m.err = nil
		m.output = "Usage: /agent <id> <message>\n\n" + agentListText(m.agents)
		m.addTranscript("Xira", m.output)
		return true, nil
	case strings.HasPrefix(text, "/use "):
		return true, m.applyUseCommand(strings.TrimSpace(strings.TrimPrefix(text, "/use ")))
	case text == "/use":
		m.err = nil
		m.output = "Usage: /use <agent-id>\n\n" + agentListText(m.agents)
		m.addTranscript("Xira", m.output)
		return true, nil
	case text == "/exit-agent":
		m.activeAgent = ""
		m.err = nil
		m.output = "Mode: default agent"
		m.addTranscript("Xira", m.output)
		return true, nil
	case text == "/flows" || text == "/flow":
		m.err = nil
		m.output = "Flow entrypoints are not enabled in Phase 1. Use /agents or /agent <id> <message>."
		m.addTranscript("Xira", m.output)
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
		m.addTranscript("Xira", m.output)
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
		m.addTranscript("Xira", m.output)
		return nil
	}
	m.loading = true
	m.loadingMode = "agent"
	m.runningAgent = id
	m.err = nil
	m.output = ""
	m.addTranscript("You -> "+id, message)
	traceCmd := m.beginTrace()
	return tea.Batch(traceCmd, m.runAgent(id, message))
}

func (m *model) applyUseCommand(id string) tea.Cmd {
	if id == "" {
		m.err = nil
		m.output = "Usage: /use <agent-id>\n\n" + agentListText(m.agents)
		m.addTranscript("Xira", m.output)
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
	m.addTranscript("Xira", m.output)
	return nil
}

func (m *model) addTranscript(role, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	m.transcript = append(m.transcript, transcriptEntry{Role: strings.TrimSpace(role), Content: content})
}

func (m *model) beginTrace() tea.Cmd {
	m.stopTrace()
	m.traceEvents = nil
	if m.runtime == nil || m.runtime.EventBus() == nil {
		m.traceSub = nil
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.traceCancel = cancel
	m.traceSub = m.runtime.EventBus().Subscribe(ctx)
	return m.watchTrace()
}

func (m *model) stopTrace() {
	if m.traceCancel != nil {
		m.traceCancel()
		m.traceCancel = nil
	}
}

func (m model) watchTrace() tea.Cmd {
	ch := m.traceSub
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		event, ok := <-ch
		if !ok {
			return traceClosedMsg{}
		}
		return traceEventMsg{event: event}
	}
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
		case strings.Contains(lower, "xira") || strings.Contains(lower, "assistant"):
			header = assistantStyle.Render(role)
		}
		bodyStyle := userTextStyle
		if strings.Contains(lower, "xira") || strings.Contains(lower, "assistant") {
			bodyStyle = assistantTextStyle
		}
		blocks = append(blocks, header+"\n"+renderMarkdown(content, width, bodyStyle))
	}
	return strings.Join(blocks, "\n\n")
}

func renderActivity(m model, width int) []string {
	width = maxInt(12, width)
	if m.loading {
		lines := []string{pillStyle.Render(loadingLabel(m.loadingMode))}
		events := compactEvents(m.traceEvents, 4)
		if len(events) == 0 {
			return append(lines, mutedStyle.Render("waiting for events"))
		}
		for _, event := range events {
			lines = append(lines, compactEventLine(event, width))
		}
		return lines
	}
	if run := lastRun(m); run != nil {
		status := emptyDash(run.Status)
		if run.VerificationResult.Status != "" {
			status += "/" + run.VerificationResult.Status
		}
		lines := []string{
			labelStyle.Render(truncate(status, width)),
			mutedStyle.Render(truncate(fmt.Sprintf("tools %d  events %d", len(run.ToolCalls), len(run.Events)), width)),
		}
		if len(run.Events) > 0 {
			lines = append(lines, compactEventLine(run.Events[len(run.Events)-1], width))
		}
		return lines
	}
	if len(m.traceEvents) > 0 {
		return []string{compactEventLine(m.traceEvents[len(m.traceEvents)-1], width)}
	}
	return []string{mutedStyle.Render("idle")}
}

type activityStep struct {
	Status string
	Agent  string
	Action string
	Detail string
}

func renderInlineLiveActivity(m model, width int) string {
	width = maxInt(24, width)
	steps := liveActivitySteps(m.traceEvents, runningAgentID(m))
	header := renderActivityHeader("Activity live", true, steps, 0, "")
	lines := []string{header}
	if len(steps) == 0 {
		lines = append(lines, mutedStyle.Render("  waiting for activity..."))
		return strings.Join(lines, "\n")
	}
	for _, step := range steps {
		lines = append(lines, renderActivityStep(step, width))
	}
	return strings.Join(lines, "\n")
}

func renderInlineRunSummary(run *frt.TurnResponse, width int) string {
	if run == nil {
		return ""
	}
	width = maxInt(24, width)
	steps := runActivitySteps(run)
	lines := []string{
		renderActivityHeader("Activity summary", false, steps, len(run.Artifacts), runDurationLabel(run)),
		mutedStyle.Render(truncate("  audit ref: .xira/runs", width)),
	}
	if len(steps) == 0 {
		lines = append(lines, mutedStyle.Render("  no visible activity steps"))
		return strings.Join(lines, "\n")
	}
	for _, step := range steps {
		lines = append(lines, renderActivityStep(step, width))
	}
	return strings.Join(lines, "\n")
}

func renderActivityHeader(label string, live bool, steps []activityStep, artifacts int, duration string) string {
	agentCount := countActivityAgents(steps)
	if agentCount == 0 && len(steps) > 0 {
		agentCount = 1
	}
	parts := []string{pluralCount(agentCount, "agent"), pluralCount(len(steps), "step")}
	if artifacts > 0 {
		parts = append(parts, pluralCount(artifacts, "artifact"))
	}
	if duration != "" {
		parts = append(parts, duration)
	}
	header := titleStyle.Render(label)
	if live {
		header += " " + pillStyle.Render("running")
	}
	return header + " " + mutedStyle.Render(strings.Join(parts, " · "))
}

func renderActivityStep(step activityStep, width int) string {
	status := strings.TrimSpace(step.Status)
	if status == "" {
		status = "ok"
	}
	statusStyle := successStyle
	switch status {
	case "running":
		statusStyle = labelStyle
	case "err", "failed":
		statusStyle = errorStyle
	}
	agent := emptyDash(step.Agent)
	action := emptyDash(step.Action)
	detail := strings.TrimSpace(step.Detail)
	prefix := statusStyle.Render(padRight(status, 7)) + " " + assistantStyle.Render(truncate(agent, 22)) + " " + labelStyle.Render(truncate(action, 16))
	if detail == "" {
		return prefix
	}
	remaining := maxInt(16, width-lipgloss.Width(prefix)-2)
	return prefix + " " + mutedStyle.Render(truncate(detail, remaining))
}

func liveActivitySteps(events []frt.RuntimeEvent, fallbackAgent string) []activityStep {
	agent := emptyDash(fallbackAgent)
	var steps []activityStep
	modelStep := -1
	for _, event := range events {
		switch event.Kind {
		case "tool.started":
			steps = append(steps, activityStep{
				Status: "running",
				Agent:  agent,
				Action: toolEventName(event),
				Detail: toolEventDetail(event.Payload),
			})
		case "tool.finished":
			updateLastMatchingStep(steps, toolEventName(event), "ok", toolEventDetail(event.Payload))
		case "tool.failed":
			if !updateLastMatchingStep(steps, toolEventName(event), "err", toolEventDetail(event.Payload)) {
				steps = append(steps, activityStep{Status: "err", Agent: agent, Action: toolEventName(event), Detail: toolEventDetail(event.Payload)})
			}
		case "adk.event":
			detail := modelEventDetail(event.Payload)
			if detail == "" {
				continue
			}
			if modelStep >= 0 {
				steps[modelStep].Detail = detail
				continue
			}
			steps = append(steps, activityStep{Status: "running", Agent: agent, Action: "model", Detail: detail})
			modelStep = len(steps) - 1
		case "run.failed":
			steps = append(steps, activityStep{Status: "err", Agent: agent, Action: "runtime", Detail: event.Message})
		}
	}
	return steps
}

func runActivitySteps(run *frt.TurnResponse) []activityStep {
	agent := emptyDash(run.AgentID)
	var steps []activityStep
	for _, call := range run.ToolCalls {
		status := "ok"
		if call.Error != "" {
			status = "err"
		}
		detail := toolCallDetail(call)
		steps = append(steps, activityStep{Status: status, Agent: agent, Action: call.Name, Detail: detail})
	}
	for _, artifact := range run.Artifacts {
		steps = append(steps, activityStep{Status: "ok", Agent: agent, Action: "artifact", Detail: artifact})
	}
	if strings.TrimSpace(run.FinalResponse) != "" {
		steps = append(steps, activityStep{Status: "ok", Agent: agent, Action: "answer", Detail: "synthesized final response"})
	}
	if len(steps) == 0 {
		steps = liveActivitySteps(run.Events, agent)
		for i := range steps {
			if steps[i].Status == "running" {
				steps[i].Status = "ok"
			}
		}
	}
	return steps
}

func updateLastMatchingStep(steps []activityStep, action, status, detail string) bool {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Action != action {
			continue
		}
		steps[i].Status = status
		if detail != "" {
			steps[i].Detail = detail
		}
		return true
	}
	return false
}

func toolEventName(event frt.RuntimeEvent) string {
	if event.Source != "" {
		return event.Source
	}
	if tool := payloadString(event.Payload, "tool", "name"); tool != "" {
		return tool
	}
	return "tool"
}

func toolEventDetail(payload map[string]any) string {
	return firstNonEmpty(
		payloadString(payload, "command"),
		payloadString(payload, "path"),
		payloadString(payload, "action"),
		payloadString(payload, "status"),
	)
}

func toolCallDetail(call frt.ToolCallRecord) string {
	input := firstNonEmpty(
		payloadString(call.Input, "command"),
		payloadString(call.Input, "path"),
		payloadString(call.Input, "action"),
	)
	output := firstNonEmpty(
		payloadString(call.Output, "path"),
		payloadString(call.Output, "bytes"),
		payloadString(call.Output, "entries"),
		payloadString(call.Output, "exit_code"),
	)
	if call.Error != "" {
		if input == "" {
			return call.Error
		}
		return input + " -> " + call.Error
	}
	if input != "" && output != "" && input != output {
		return input + " -> " + output
	}
	return firstNonEmpty(input, output)
}

func modelEventDetail(payload map[string]any) string {
	if payloadString(payload, "final") == "true" {
		return "final response"
	}
	if reason := payloadString(payload, "finish_reason"); reason != "" {
		return "finish reason: " + reason
	}
	if chars := payloadString(payload, "content_chars"); chars != "" && chars != "0" {
		return "streaming response"
	}
	return ""
}

func payloadString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := payload[key]
		if !ok {
			continue
		}
		text := strings.TrimSpace(fmt.Sprint(value))
		if text != "" {
			return text
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func countActivityAgents(steps []activityStep) int {
	seen := map[string]bool{}
	for _, step := range steps {
		agent := strings.TrimSpace(step.Agent)
		if agent != "" && agent != "-" {
			seen[agent] = true
		}
	}
	return len(seen)
}

func pluralCount(count int, singular string) string {
	word := singular
	if count != 1 {
		word += "s"
	}
	return fmt.Sprintf("%d %s", count, word)
}

func runDurationLabel(run *frt.TurnResponse) string {
	if run == nil || run.StartedAt.IsZero() || run.EndedAt.IsZero() {
		return ""
	}
	duration := run.EndedAt.Sub(run.StartedAt)
	if duration <= 0 {
		return ""
	}
	return duration.Round(100 * 1000 * 1000).String()
}

func padRight(text string, width int) string {
	for lipgloss.Width(text) < width {
		text += " "
	}
	return text
}

func compactEventLine(event frt.RuntimeEvent, width int) string {
	text := emptyDash(event.Kind)
	if event.Source != "" {
		text += " " + event.Source
	}
	if payload := summarizeMap(event.Payload, []string{"tool", "path", "command", "status", "content_chars"}); payload != "" {
		text += " {" + payload + "}"
	}
	style := mutedStyle
	if strings.Contains(event.Kind, "tool.") {
		style = labelStyle
	}
	if strings.Contains(event.Kind, "failed") || strings.Contains(event.Kind, "empty") {
		style = errorStyle
	}
	return style.Render(truncate(text, width))
}

func renderMarkdown(content string, width int, bodyStyle lipgloss.Style) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	width = maxInt(12, width)
	var out []string
	inCode := false
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			if inCode {
				label := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				if label == "" {
					label = "code"
				}
				out = append(out, codeStyle.Render(" "+label+" "))
			}
			continue
		}
		if inCode {
			out = append(out, codeStyle.Render(truncate(line, width-2)))
			continue
		}
		if trimmed == "" {
			out = append(out, "")
			continue
		}
		if heading, ok := markdownHeading(trimmed); ok {
			out = append(out, titleStyle.Render(wrapText(stripInlineMarkdown(heading), width)))
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			quote := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
			out = append(out, renderWrappedMarkdownLine("> ", quote, width, quoteStyle))
			continue
		}
		if prefix, text, ok := markdownBullet(trimmed); ok {
			out = append(out, renderWrappedMarkdownLine(prefix, text, width, bodyStyle))
			continue
		}
		out = append(out, renderWrappedMarkdownLine("  ", trimmed, width, bodyStyle))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

func renderWrappedMarkdownLine(prefix, text string, width int, style lipgloss.Style) string {
	text = stripInlineMarkdown(strings.TrimSpace(text))
	if text == "" {
		return style.Render(strings.TrimRight(prefix, " "))
	}
	bodyWidth := maxInt(12, width-lipgloss.Width(prefix))
	wrapped := wrapText(text, bodyWidth)
	lines := strings.Split(wrapped, "\n")
	continuation := strings.Repeat(" ", lipgloss.Width(prefix))
	for i := range lines {
		if i == 0 {
			lines[i] = prefix + lines[i]
			continue
		}
		lines[i] = continuation + lines[i]
	}
	return style.Render(strings.Join(lines, "\n"))
}

func markdownHeading(line string) (string, bool) {
	count := 0
	for count < len(line) && count < 6 && line[count] == '#' {
		count++
	}
	if count == 0 || count >= len(line) {
		return "", false
	}
	if line[count] != ' ' && line[count] != '\t' {
		return "", false
	}
	return strings.TrimSpace(line[count:]), true
}

func markdownBullet(line string) (string, string, bool) {
	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, marker) {
			return "- ", strings.TrimSpace(strings.TrimPrefix(line, marker)), true
		}
	}
	dot := strings.Index(line, ". ")
	if dot <= 0 || dot > 4 {
		return "", "", false
	}
	for _, r := range line[:dot] {
		if r < '0' || r > '9' {
			return "", "", false
		}
	}
	return line[:dot+2], strings.TrimSpace(line[dot+2:]), true
}

func stripInlineMarkdown(text string) string {
	replacer := strings.NewReplacer("**", "", "__", "", "`", "")
	return replacer.Replace(text)
}

func renderActiveStatus(m model, width int) string {
	message := truncate(loadingLabel(m.loadingMode)+" - activity is streaming in this turn", maxInt(12, width-10))
	return pillStyle.Render("RUNNING") + " " + assistantTextStyle.Render(message)
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

func renderLiveTrace(events []frt.RuntimeEvent, width int) string {
	lines := []string{titleStyle.Render("Trace ") + pillStyle.Render("live")}
	if len(events) == 0 {
		lines = append(lines, mutedStyle.Render("  waiting for run.started / model / tool events..."))
		return strings.Join(lines, "\n")
	}
	for _, event := range compactEvents(events, 8) {
		line := fmt.Sprintf("  %s", emptyDash(event.Kind))
		if event.Source != "" {
			line += " " + event.Source
		}
		if event.Message != "" {
			line += " - " + event.Message
		}
		if payload := summarizeMap(event.Payload, []string{"tool", "agent_id", "channel", "matched_by", "command", "path", "status"}); payload != "" {
			line += " {" + payload + "}"
		}
		style := mutedStyle
		if strings.Contains(event.Kind, "tool.") {
			style = labelStyle
		}
		if strings.Contains(event.Kind, "failed") {
			style = errorStyle
		}
		lines = append(lines, style.Render(truncate(line, width)))
	}
	return strings.Join(lines, "\n")
}

func renderToolCallLine(call frt.ToolCallRecord, width int) string {
	status := "ok"
	style := successStyle
	if call.Error != "" {
		status = "err"
		style = errorStyle
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
	return style.Render(truncate(line, width))
}

func appendTraceEvent(events []frt.RuntimeEvent, event frt.RuntimeEvent, max int) []frt.RuntimeEvent {
	events = append(events, event)
	if max > 0 && len(events) > max {
		return events[len(events)-max:]
	}
	return events
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
		"/trace - toggle trace inspector",
		"/trace on|off - show or hide trace inspector",
		"/exit-agent - return to the default agent",
		"/flows - list flow entrypoints when enabled",
	}, "\n")
}

func traceModeText(enabled bool) string {
	if enabled {
		return "Trace inspector: on"
	}
	return "Trace inspector: off"
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

func composerLabel(m model) string {
	if id := strings.TrimSpace(m.activeAgent); id != "" {
		return "You -> " + id
	}
	return "You"
}

func runningAgentID(m model) string {
	if agent := strings.TrimSpace(m.runningAgent); agent != "" {
		return agent
	}
	if agent := selectedAgentID(m); agent != "" {
		return agent
	}
	return "default agent"
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
	return maxInt(24, width-4)
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
