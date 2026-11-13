package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
)

const usageHistoryWindowDays = 30

type usageTabModel struct {
	client   *Client
	viewport viewport.Model
	content  string
	err      error
	width    int
	height   int
	ready    bool
	now      func() time.Time

	lastSummary usagehistory.Summary
	lastEvents  usagehistory.EventsPage
}

type usageDataMsg struct {
	summary usagehistory.Summary
	events  usagehistory.EventsPage
	err     error
}

func newUsageTabModel(client *Client) usageTabModel {
	return usageTabModel{client: client, now: time.Now}
}

func (m usageTabModel) Init() tea.Cmd {
	return m.fetchData
}

func (m usageTabModel) fetchData() tea.Msg {
	if m.client == nil {
		return usageDataMsg{err: fmt.Errorf("management client unavailable")}
	}
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	to := now().UTC()
	from := to.AddDate(0, 0, -(usageHistoryWindowDays - 1))
	summary, errSummary := m.client.GetUsageSummary(from.Format(time.DateOnly), to.Format(time.DateOnly))
	if errSummary != nil {
		return usageDataMsg{err: errSummary}
	}
	events, errEvents := m.client.GetUsageEvents(20, 0)
	return usageDataMsg{summary: summary, events: events, err: errEvents}
}

func (m usageTabModel) Update(msg tea.Msg) (usageTabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case localeChangedMsg:
		m.content = m.renderContent()
		m.viewport.SetContent(m.content)
		return m, m.fetchData
	case usageDataMsg:
		m.err = msg.err
		if msg.err == nil {
			m.lastSummary = msg.summary
			m.lastEvents = msg.events
		}
		m.content = m.renderContent()
		m.viewport.SetContent(m.content)
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "r" {
			return m, m.fetchData
		}
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m *usageTabModel) SetSize(width, height int) {
	m.width = width
	m.height = height
	if !m.ready {
		m.viewport = viewport.New(width, height)
		m.content = m.renderContent()
		m.viewport.SetContent(m.content)
		m.ready = true
		return
	}
	m.viewport.Width = width
	m.viewport.Height = height
}

func (m usageTabModel) View() string {
	if !m.ready {
		return T("loading")
	}
	return m.viewport.View()
}

func (m usageTabModel) renderContent() string {
	var output strings.Builder
	output.WriteString(titleStyle.Render(T("usage_title")))
	output.WriteString("\n")
	output.WriteString(helpStyle.Render(T("usage_help")))
	output.WriteString("\n\n")
	if m.err != nil {
		output.WriteString(errorStyle.Render(T("error_prefix") + m.err.Error()))
		return output.String()
	}

	totals := m.lastSummary.Totals
	if m.lastSummary.From == "" {
		output.WriteString(helpStyle.Render(T("usage_no_data")))
		return output.String()
	}

	cardWidth := 22
	if m.width > 0 {
		cardWidth = (m.width - 5) / 4
		if cardWidth < 16 {
			cardWidth = 16
		}
	}
	cardStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorBorder).
		Padding(0, 1).
		Width(cardWidth).
		Height(2)
	cards := []string{
		usageCard(cardStyle, T("usage_total_reqs"), formatLargeNumber(totals.Requests), colorInfo),
		usageCard(cardStyle, T("usage_success"), formatLargeNumber(totals.Success), colorSuccess),
		usageCard(cardStyle, T("usage_failure"), formatLargeNumber(totals.Failed), colorError),
		usageCard(cardStyle, T("usage_total_tokens"), formatLargeNumber(totals.TotalTokens), colorHighlight),
	}
	output.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, cards...))
	output.WriteString("\n\n")
	output.WriteString(fmt.Sprintf("  %s %s — %s (%s)\n", labelStyle.Render(T("usage_time")+":"), m.lastSummary.From, m.lastSummary.To, m.lastSummary.Timezone))
	output.WriteString(fmt.Sprintf("  %s %s  %s %s  %s %s  %s %s\n",
		labelStyle.Render(T("usage_input")+":"), formatLargeNumber(totals.InputTokens),
		labelStyle.Render(T("usage_output")+":"), formatLargeNumber(totals.OutputTokens),
		labelStyle.Render(T("usage_cached")+":"), formatLargeNumber(totals.CacheReadTokens),
		labelStyle.Render(T("usage_reasoning")+":"), formatLargeNumber(totals.ReasoningTokens)))
	output.WriteString(fmt.Sprintf("  %s %s  %s %s  %s %s\n\n",
		labelStyle.Render(T("usage_reported_cost")+":"), formatUSDMicros(totals.ReportedCostUSDMicros),
		labelStyle.Render(T("usage_estimated_cost")+":"), formatUSDMicros(totals.EstimatedCostUSDMicros),
		labelStyle.Render(T("usage_effective_cost")+":"), formatUSDMicros(totals.EffectiveCostUSDMicros)))

	output.WriteString(tableHeaderStyle.Render(T("usage_req_by_day")))
	output.WriteString("\n")
	output.WriteString(fmt.Sprintf("  %-12s %10s %14s\n", T("usage_time"), T("requests"), T("tokens")))
	start := 0
	if len(m.lastSummary.Days) > 14 {
		start = len(m.lastSummary.Days) - 14
	}
	for index := len(m.lastSummary.Days) - 1; index >= start; index-- {
		day := m.lastSummary.Days[index]
		output.WriteString(fmt.Sprintf("  %-12s %10s %14s\n", day.Date, formatLargeNumber(day.Requests), formatLargeNumber(day.TotalTokens)))
	}

	output.WriteString("\n")
	output.WriteString(tableHeaderStyle.Render(T("usage_recent_events")))
	output.WriteString("\n")
	output.WriteString(fmt.Sprintf("  %-20s %-12s %-24s %8s %10s %12s %-9s %8s\n",
		T("usage_time"), T("usage_provider"), T("model"), T("usage_status"), T("tokens"), T("usage_cost"), T("usage_cost_source"), T("usage_latency")))
	if len(m.lastEvents.Items) == 0 {
		output.WriteString(helpStyle.Render(T("usage_no_data")))
		output.WriteString("\n")
		return output.String()
	}
	for _, event := range m.lastEvents.Items {
		status := T("usage_success")
		if event.Failed {
			status = T("usage_failure")
		}
		modelWidth := 24
		output.WriteString(fmt.Sprintf("  %-20s %-12s %-24s %8s %10s %12s %-9s %8s\n",
			event.Timestamp.UTC().Format("2006-01-02 15:04:05"),
			truncate(event.Provider, 12),
			truncate(event.Model, modelWidth),
			status,
			formatLargeNumber(event.TotalTokens),
			formatUSDMicros(event.EffectiveCostUSDMicros),
			truncate(event.CostSource, 9),
			fmt.Sprintf("%dms", event.LatencyMs)))
	}
	return output.String()
}

func formatUSDMicros(value int64) string {
	if value < 0 {
		value = 0
	}
	return fmt.Sprintf("$%d.%06d", value/1_000_000, value%1_000_000)
}

func usageCard(style lipgloss.Style, label, value string, color lipgloss.Color) string {
	return style.Render(fmt.Sprintf("%s\n%s",
		lipgloss.NewStyle().Bold(true).Foreground(color).Render(value),
		lipgloss.NewStyle().Foreground(colorMuted).Render(label)))
}
