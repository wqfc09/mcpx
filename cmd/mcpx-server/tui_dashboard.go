package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"mcpx/internal/config"
	runtimeinstance "mcpx/internal/instance"
	"mcpx/internal/workspace"
)

var (
	mcpxAccent      = lipgloss.Color("#89B4FA")
	mcpxAccentSoft  = lipgloss.Color("#313A55")
	mcpxText        = lipgloss.Color("#CDD6F4")
	mcpxMuted       = lipgloss.Color("#7F849C")
	mcpxSubtle      = lipgloss.Color("#585B70")
	mcpxSurface     = lipgloss.Color("#181825")
	mcpxSurfaceSoft = lipgloss.Color("#1E1E2E")
	mcpxGreen       = lipgloss.Color("#A6E3A1")
	mcpxYellow      = lipgloss.Color("#F9E2AF")
	mcpxRed         = lipgloss.Color("#F38BA8")
)

type mcpxTUITheme struct {
	brand, body, muted, dim, good, warn, bad, key lipgloss.Style
	color                                         bool
}

func newMCPXTUITheme(color bool) mcpxTUITheme {
	if !color {
		return mcpxTUITheme{
			brand: lipgloss.NewStyle(), body: lipgloss.NewStyle(), muted: lipgloss.NewStyle(),
			dim: lipgloss.NewStyle(), good: lipgloss.NewStyle(), warn: lipgloss.NewStyle(), bad: lipgloss.NewStyle(),
			key: lipgloss.NewStyle(), color: false,
		}
	}
	return mcpxTUITheme{
		brand: lipgloss.NewStyle().Foreground(mcpxAccent).Bold(true),
		body:  lipgloss.NewStyle().Foreground(mcpxText),
		muted: lipgloss.NewStyle().Foreground(mcpxMuted),
		dim:   lipgloss.NewStyle().Foreground(mcpxSubtle),
		good:  lipgloss.NewStyle().Foreground(mcpxGreen),
		warn:  lipgloss.NewStyle().Foreground(mcpxYellow),
		bad:   lipgloss.NewStyle().Foreground(mcpxRed),
		key:   lipgloss.NewStyle().Foreground(mcpxAccent).Background(mcpxAccentSoft).Bold(true).Padding(0, 1),
		color: true,
	}
}

func renderMCPXDashboard(state runtimeinstance.State, ws workspace.Workspace, items []pluginStatusItem, definitions map[string]config.MCPServer, width int, hosted bool, prefix string, color bool) string {
	if width < 1 {
		width = 1
	}
	if width < 50 {
		return renderMCPXCompact(state, ws, items, width, hosted, prefix)
	}
	theme := newMCPXTUITheme(color)
	lines := []string{renderMCPXHeader(theme, state, ws, width)}

	if width >= 92 {
		leftWidth := width/2 - 1
		rightWidth := width - leftWidth - 1
		left := renderMCPXRuntimePanel(theme, state, leftWidth)
		right := renderMCPXWorkspacePanel(theme, ws, rightWidth)
		lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right))
	} else {
		lines = append(lines, renderMCPXRuntimePanel(theme, state, width), renderMCPXWorkspacePanel(theme, ws, width))
	}
	lines = append(lines, renderMCPXPluginsPanel(theme, items, definitions, width), renderMCPXFooter(theme, hosted, prefix, width))
	return strings.Join(lines, "\n")
}

func renderMCPXHeader(theme mcpxTUITheme, state runtimeinstance.State, ws workspace.Workspace, width int) string {
	build := strings.TrimSpace(state.Build)
	if build == "" {
		build = "dev"
	}
	left := theme.brand.Render("MCPX") + "  " + theme.body.Render(tuiEmpty(ws.Name))
	right := theme.good.Render("● RUNTIME READY") + theme.dim.Render("  ") + theme.muted.Render("v"+build)
	line1 := tuiEdge(left, right, width)
	line2 := tuiEdge(theme.muted.Render(tuiFit(ws.Path, maxIntTUI(12, width/2))), theme.dim.Render(tuiFit(state.Endpoint, maxIntTUI(12, width/2))), width)
	if !theme.color {
		return line1 + "\n" + line2
	}
	bar := lipgloss.NewStyle().Background(mcpxSurface)
	return bar.Render(tuiPadANSI(line1, width)) + "\n" + bar.Render(tuiPadANSI(line2, width))
}

func renderMCPXRuntimePanel(theme mcpxTUITheme, state runtimeinstance.State, totalWidth int) string {
	rows := []string{
		tuiSection(theme, "RUNTIME", fmt.Sprintf("pid %d", state.PID), totalWidth-2),
		"",
		tuiField(theme, "Status", theme.good.Render("● LIVE"), totalWidth-2),
		tuiField(theme, "Instance", state.InstanceID, totalWidth-2),
		tuiField(theme, "Endpoint", state.Endpoint, totalWidth-2),
		tuiField(theme, "Build", tuiBuildLabel(state), totalWidth-2),
		tuiField(theme, "Started", state.StartedAt, totalWidth-2),
	}
	return tuiPanel(theme, rows, totalWidth)
}

func renderMCPXWorkspacePanel(theme mcpxTUITheme, ws workspace.Workspace, totalWidth int) string {
	statusStyle := theme.good
	status := strings.ToUpper(strings.TrimSpace(ws.Status))
	if status == "" {
		status = "OK"
	}
	if ws.Status != "" && ws.Status != workspace.StatusOK {
		statusStyle = theme.warn
	}
	rows := []string{
		tuiSection(theme, "WORKSPACE", statusStyle.Render(status), totalWidth-2),
		"",
		tuiField(theme, "Name", ws.Name, totalWidth-2),
		tuiField(theme, "ID", ws.ID, totalWidth-2),
		tuiField(theme, "Path", ws.Path, totalWidth-2),
		tuiField(theme, "Description", ws.Description, totalWidth-2),
		"",
	}
	return tuiPanel(theme, rows, totalWidth)
}

func renderMCPXPluginsPanel(theme mcpxTUITheme, items []pluginStatusItem, definitions map[string]config.MCPServer, totalWidth int) string {
	contentWidth := maxIntTUI(1, totalWidth-2)
	running := 0
	for _, item := range items {
		if item.RuntimeState == "running" {
			running++
		}
	}
	rows := []string{tuiSection(theme, "PLUGINS", fmt.Sprintf("%d installed · %d live", len(items), running), contentWidth), ""}
	if len(items) == 0 {
		rows = append(rows, theme.muted.Render("No plugins installed."))
		return tuiPanel(theme, rows, totalWidth)
	}
	if contentWidth >= 74 {
		nameWidth := minIntTUI(24, maxIntTUI(14, contentWidth/4))
		stateWidth := 11
		typeWidth := minIntTUI(24, maxIntTUI(17, contentWidth/4))
		header := tuiColumns(contentWidth, []tuiColumn{{"PLUGIN", nameWidth}, {"STATE", stateWidth}, {"RUNTIME", typeWidth}, {"DETAIL", contentWidth - nameWidth - stateWidth - typeWidth - 6}})
		rows = append(rows, theme.dim.Render(header))
		for _, item := range items {
			state, stateStyle := tuiPluginState(theme, item.RuntimeState)
			runtime := strings.Trim(strings.Join([]string{item.Runtime, item.Scope}, " / "), " /-")
			detail := ""
			definition := definitions[item.Name]
			if definition.Plugin != nil && definition.Plugin.TUI != nil {
				detail = "TUI"
			}
			if item.RuntimePID > 0 {
				if detail != "" {
					detail += " · "
				}
				detail += fmt.Sprintf("pid %d", item.RuntimePID)
			}
			plain := tuiColumns(contentWidth, []tuiColumn{{item.Name, nameWidth}, {state, stateWidth}, {runtime, typeWidth}, {detail, contentWidth - nameWidth - stateWidth - typeWidth - 6}})
			// Paint the status marker after the column geometry is fixed.
			plain = strings.Replace(plain, tuiFit(state, stateWidth), stateStyle.Render(tuiFit(state, stateWidth)), 1)
			rows = append(rows, plain)
		}
	} else {
		for _, item := range items {
			state, stateStyle := tuiPluginState(theme, item.RuntimeState)
			left := theme.body.Render(item.Name)
			right := stateStyle.Render(state)
			rows = append(rows, tuiEdge(left, right, contentWidth))
			detail := fmt.Sprintf("  %s / %s", item.Runtime, item.Scope)
			if item.RuntimePID > 0 {
				detail += fmt.Sprintf(" · pid %d", item.RuntimePID)
			}
			rows = append(rows, theme.muted.Render(tuiFit(detail, contentWidth)))
		}
	}
	return tuiPanel(theme, rows, totalWidth)
}

func renderMCPXFooter(theme mcpxTUITheme, hosted bool, prefix string, width int) string {
	var line string
	if hosted {
		left := theme.key.Render("Ctrl+C") + " " + theme.muted.Render("Dashboard")
		right := theme.key.Render(prefix) + " " + theme.muted.Render("Dashboard") + theme.dim.Render("   ") + theme.key.Render(prefix+" c") + " " + theme.muted.Render("Send Ctrl+C")
		line = tuiEdge(left, right, width)
	} else {
		line = theme.key.Render("Ctrl+C") + " " + theme.muted.Render("Exit MCPX page")
	}
	if !theme.color {
		return tuiPadANSI(line, width)
	}
	return lipgloss.NewStyle().Background(mcpxSurfaceSoft).Render(tuiPadANSI(line, width))
}

func renderMCPXCompact(state runtimeinstance.State, ws workspace.Workspace, items []pluginStatusItem, width int, hosted bool, prefix string) string {
	lines := []string{
		tuiFit("MCPX · "+tuiEmpty(ws.Name)+" · runtime live", width),
		tuiFit(fmt.Sprintf("%s · pid %d · plugins %d", state.Endpoint, state.PID, len(items)), width),
	}
	if hosted {
		lines = append(lines, tuiFit("Ctrl+C dashboard · "+prefix+" dashboard", width))
	} else {
		lines = append(lines, tuiFit("Ctrl+C exit page", width))
	}
	return strings.Join(lines, "\n")
}

func encodeMCPXFullscreenFrame(frame string) string {
	lines := strings.Split(frame, "\n")
	var output strings.Builder
	output.WriteString("\x1b[?7l\x1b[2J")
	for index, line := range lines {
		fmt.Fprintf(&output, "\x1b[%d;1H\x1b[2K%s", index+1, line)
	}
	output.WriteString("\x1b[?7h\x1b[1;1H")
	return output.String()
}

func tuiBuildLabel(state runtimeinstance.State) string {
	parts := []string{}
	if strings.TrimSpace(state.Build) != "" {
		parts = append(parts, state.Build)
	}
	if strings.TrimSpace(state.Commit) != "" {
		commit := state.Commit
		if len(commit) > 10 {
			commit = commit[:10]
		}
		parts = append(parts, commit)
	}
	if len(parts) == 0 {
		return "dev"
	}
	return strings.Join(parts, " · ")
}

func tuiPluginState(theme mcpxTUITheme, value string) (string, lipgloss.Style) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "running":
		return "● LIVE", theme.good
	case "not_running":
		return "◌ READY", theme.warn
	case "inactive":
		return "○ OFF", theme.muted
	case "stale":
		return "! STALE", theme.bad
	case "instance_stopped":
		return "○ STOPPED", theme.bad
	default:
		return "○ UNKNOWN", theme.muted
	}
}

type tuiColumn struct {
	value string
	width int
}

func tuiColumns(total int, columns []tuiColumn) string {
	parts := make([]string, 0, len(columns))
	for _, column := range columns {
		parts = append(parts, tuiPad(column.value, column.width))
	}
	return tuiFit(strings.Join(parts, "  "), total)
}

func tuiSection(theme mcpxTUITheme, left, right string, width int) string {
	return tuiEdge(theme.brand.Render(left), theme.dim.Render(right), width)
}

func tuiField(theme mcpxTUITheme, label, value string, width int) string {
	labelText := theme.muted.Render(tuiPad(label, 12))
	available := maxIntTUI(1, width-13)
	return labelText + " " + tuiFit(tuiEmpty(value), available)
}

func tuiPanel(theme mcpxTUITheme, rows []string, totalWidth int) string {
	contentWidth := maxIntTUI(1, totalWidth-2)
	for index := range rows {
		rows[index] = tuiPadANSI(rows[index], contentWidth)
	}
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
	if theme.color {
		style = style.BorderForeground(mcpxSubtle).Foreground(mcpxText)
	}
	return style.Render(strings.Join(rows, "\n"))
}

func tuiEdge(left, right string, width int) string {
	if width <= 0 {
		return ""
	}
	rightWidth := lipgloss.Width(right)
	if rightWidth >= width {
		return ansi.Truncate(right, width, "…")
	}
	left = ansi.Truncate(left, maxIntTUI(0, width-rightWidth-1), "…")
	gap := width - lipgloss.Width(left) - rightWidth
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func tuiPadANSI(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = ansi.Truncate(value, width, "…")
	if padding := width - lipgloss.Width(value); padding > 0 {
		value += strings.Repeat(" ", padding)
	}
	return value
}

func tuiPad(value string, width int) string {
	value = tuiFit(value, width)
	if padding := width - lipgloss.Width(value); padding > 0 {
		value += strings.Repeat(" ", padding)
	}
	return value
}

func tuiFit(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(strings.TrimSpace(value), width, "…")
}

func tuiEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}

func maxIntTUI(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minIntTUI(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func tuiBaseName(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return filepath.Base(value)
}
