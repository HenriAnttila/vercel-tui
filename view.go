package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var (
	faint   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	bold    = lipgloss.NewStyle().Bold(true)
	active  = lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Bold(true)
	red     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	green   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellow  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	blue    = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	magenta = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	cyan    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	banner  = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("3")).Bold(true)
)

const (
	footerLines  = 3
	liveChrome   = 3 + footerLines
	detailChrome = 4 + footerLines
)

const (
	colCursor = 2
	colStar   = 2
	colState  = 11
	colEnv    = 8
	colBranch = 14
	colAuthor = 12
	colAge    = 5
	colBuild  = 7
)

func (m model) View() tea.View {
	var lines []string
	switch {
	case m.width == 0:
	case m.screen == detailScreen:
		lines = append(lines, m.detailHeader()...)
		if m.detailRuntime {
			lines = append(lines, m.rule(), m.deployRT.vp.View())
		} else {
			lines = append(lines, m.rule(), m.vp.View())
		}
	case m.screen == liveScreen:
		lines = append(lines, m.liveHeader()...)
		lines = append(lines, m.rule(), m.live.vp.View())
	default:
		lines = append(lines, m.header()...)
		lines = append(lines, m.rule(), m.columnHeader())
		lines = append(lines, m.rows()...)
	}
	body := strings.Join(lines, "\n")
	if gap := m.height - footerLines - lipgloss.Height(body); gap > 0 {
		body += strings.Repeat("\n", gap)
	}
	v := tea.NewView(body + "\n" + strings.Join(m.footer(), "\n"))
	v.AltScreen = true
	v.WindowTitle = "▲ " + m.projectName()
	return v
}

func (m model) projectName() string {
	if m.proj.Name != "" {
		return m.proj.Name
	}
	return m.link.ProjectID
}

func (m model) headerCount() int {
	n := 2
	if m.proj.ID != "" && !m.proj.autoAssign() {
		n++
	}
	if m.fetchErr != nil {
		n++
	}
	return n
}

func (m model) listHeight() int {
	return m.height - m.headerCount() - 2 - footerLines
}

func (m model) rule() string {
	return faint.Render(strings.Repeat("─", max(m.width, 0)))
}

func (m model) bar(left string, status []string) string {
	name := m.projectName()
	if m.scope != "" {
		name = m.scope + "/" + name
	}
	right := append(status, faint.Render("▲ "+name))
	return spread(left, strings.Join(right, faint.Render("  ·  "))+" ", m.width)
}

func (m model) tabRow(status []string) string {
	return m.bar(" "+tab(m.screen == listScreen, "Deployments")+"   "+tab(m.screen == liveScreen, "Live"), status)
}

func tab(on bool, label string) string {
	if on {
		return active.Render(label)
	}
	return faint.Render(label)
}

func (m model) header() []string {
	var status []string
	if f := filters[m.filter]; f != "" {
		status = append(status, cyan.Render("showing "+f))
	}
	switch {
	case m.loading:
		status = append(status, m.spin.View()+faint.Render(" refreshing"))
	case !m.fetched.IsZero():
		status = append(status, faint.Render("updated "+ago(m.fetched.UnixMilli(), time.Now())))
	}
	lines := []string{m.tabRow(status), " " + m.productionLine()}

	if m.proj.ID != "" && !m.proj.autoAssign() {
		branch := m.proj.Link.ProductionBranch
		if branch == "" {
			branch = "the production branch"
		}
		msg := fmt.Sprintf(" ROLLED BACK  Pushes to %s won't go live until you promote a deployment (P) ", branch)
		lines = append(lines, banner.Render(ansi.Truncate(msg, m.width, "…")))
	}
	if m.fetchErr != nil {
		msg := "Vercel API error: " + m.fetchErr.Error()
		var apiErr *apiError
		if errors.As(m.fetchErr, &apiErr) && apiErr.Status == 401 {
			msg += ". Run `vercel login` or set VERCEL_TOKEN"
		}
		lines = append(lines, red.Render(ansi.Truncate(msg, m.width, "…")))
	}
	return lines
}

func (m model) productionLine() string {
	id := m.proj.productionID()
	if id == "" {
		if m.proj.ID == "" {
			return faint.Render("loading project…")
		}
		return faint.Render("no production deployment yet")
	}
	line := magenta.Render("production") + "  "
	if domain := m.proj.productionDomain(); domain != "" {
		line += bold.Render(domain) + faint.Render(" → ")
	}
	if i := m.indexOf(id); i >= 0 {
		d := m.deps[i]
		line += d.short()
		if sha := d.sha(); sha != "" {
			line += " " + faint.Render(sha)
		}
		line += " " + d.message() + faint.Render(" · "+ago(d.Created, time.Now()))
	} else {
		line += deployment{UID: id}.short()
	}
	return ansi.Truncate(line, m.width, "…")
}

func (m model) layout() (commit int, showAuthor bool) {
	fixed := colCursor + colStar + colState + 1 + colEnv + 1 + colBranch + 1 + 1 + colAge + 1 + colBuild
	commit = m.width - fixed - colAuthor - 1
	if commit >= 24 {
		return commit, true
	}
	return max(8, m.width-fixed), false
}

func (m model) columnHeader() string {
	commitW, showAuthor := m.layout()
	cells := []string{
		strings.Repeat(" ", colCursor+colStar) + pad("STATE", colState),
		pad("ENV", colEnv),
		pad("BRANCH", colBranch),
		pad("COMMIT", commitW),
	}
	if showAuthor {
		cells = append(cells, pad("AUTHOR", colAuthor))
	}
	cells = append(cells, padLeft("AGE", colAge), padLeft("BUILD", colBuild))
	return faint.Render(strings.Join(cells, " "))
}

func (m model) rows() []string {
	if len(m.deps) == 0 {
		if m.loading {
			return []string{faint.Render("  Loading deployments…")}
		}
		return []string{faint.Render("  No deployments")}
	}
	h := max(1, m.listHeight())
	end := min(len(m.deps), m.offset+h)
	commitW, showAuthor := m.layout()
	now := time.Now()
	out := make([]string, 0, end-m.offset)
	for i := m.offset; i < end; i++ {
		out = append(out, m.row(m.deps[i], i == m.cursor, commitW, showAuthor, now))
	}
	return out
}

func (m model) row(d deployment, selected bool, commitW int, showAuthor bool, now time.Time) string {
	cursor := strings.Repeat(" ", colCursor)
	if selected {
		cursor = blue.Render("›") + " "
	}
	star := strings.Repeat(" ", colStar)
	if d.UID == m.proj.productionID() {
		star = yellow.Render("★") + " "
	}

	commit := d.message()
	if selected {
		commit = bold.Render(commit)
	}
	if sha := d.sha(); sha != "" {
		commit = faint.Render(sha) + " " + commit
	}
	branch := d.branch()
	if branch == "" {
		branch = "-"
	}

	cells := []string{
		cursor + star + pad(m.stateCell(d), colState),
		pad(envCell(d), colEnv),
		pad(branch, colBranch),
		pad(commit, commitW),
	}
	if showAuthor {
		cells = append(cells, pad(faint.Render(d.author()), colAuthor))
	}
	cells = append(cells,
		padLeft(faint.Render(ago(d.Created, now)), colAge),
		padLeft(faint.Render(buildTime(d, now)), colBuild),
	)
	return strings.Join(cells, " ")
}

func (m model) stateCell(d deployment) string {
	switch d.State {
	case "READY":
		return green.Render("● Ready")
	case "ERROR":
		return red.Render("● Error")
	case "BUILDING":
		return m.spin.View() + yellow.Render(" Building")
	case "INITIALIZING":
		return m.spin.View() + yellow.Render(" Starting")
	case "QUEUED":
		return faint.Render("◌ Queued")
	case "CANCELED":
		return faint.Render("○ Canceled")
	}
	return faint.Render("· " + strings.ToLower(d.State))
}

func envCell(d deployment) string {
	switch {
	case d.production() && d.ReadySubstate == "STAGED":
		return yellow.Render("staged")
	case d.production():
		return magenta.Render("prod")
	}
	return faint.Render("preview")
}

func (m model) deploymentSummary(d deployment, withID bool) string {
	parts := []string{m.stateCell(d), envCell(d)}
	if withID {
		parts = append(parts, bold.Render(d.short()))
	}
	if ref := d.branch(); ref != "" {
		if sha := d.sha(); sha != "" {
			ref += faint.Render("@" + sha)
		}
		parts = append(parts, ref)
	}
	parts = append(parts, d.message())
	if d.UID == m.proj.productionID() {
		parts = append(parts, yellow.Render("★ live"))
	}
	return strings.Join(parts, "  ")
}

func (m model) detailHeader() []string {
	d := m.logDep
	crumb := " " + faint.Render("Deployments ›") + " " + active.Render(d.short()) +
		"   " + tab(!m.detailRuntime, "Build") + "   " + tab(m.detailRuntime, "Runtime")
	var status []string
	if m.detailRuntime && m.deployRT.loading {
		status = append(status, m.spin.View()+faint.Render(" loading"))
	}

	var meta []string
	if m.detailRuntime {
		r := m.deployRT
		if s := r.stats(); s != "" {
			meta = append(meta, s)
		}
		if t := r.tags(); t != "" {
			meta = append(meta, t)
		}
	} else {
		now := time.Now()
		meta = append(meta, d.URL, "created "+ago(d.Created, now))
		if bt := buildTime(d, now); bt != "-" {
			meta = append(meta, "build "+bt)
		}
		if d.author() != "" {
			meta = append(meta, "by "+d.author())
		}
	}
	return []string{
		m.bar(crumb, status),
		ansi.Truncate(" "+m.deploymentSummary(d, false), m.width, "…"),
		ansi.Truncate(" "+faint.Render(strings.Join(meta, " · ")), m.width, "…"),
	}
}

func (m model) liveHeader() []string {
	r := m.live
	var status []string
	if t := r.tags(); t != "" {
		status = append(status, t)
	}
	switch {
	case r.loading:
		status = append(status, m.spin.View()+faint.Render(" loading"))
	case r.loaded:
		status = append(status, faint.Render("updated "+ago(r.until.UnixMilli(), time.Now())))
	}
	line := " " + faint.Render("following ")
	switch i := m.indexOf(r.deployID); {
	case r.deployID == "":
		line += faint.Render("the live deployment once the project loads…")
	case i >= 0:
		line += m.deploymentSummary(m.deps[i], true)
	default:
		line += yellow.Render("★ live") + "  " + bold.Render(deployment{UID: r.deployID}.short())
	}
	if s := r.stats(); s != "" {
		line += faint.Render("  ·  " + s)
	}
	return []string{m.tabRow(status), ansi.Truncate(line, m.width, "…")}
}

func (m *model) renderLogs(jumpToBottom bool) {
	follow := jumpToBottom || m.vp.AtBottom()
	var b strings.Builder
	for _, e := range m.events {
		text := strings.TrimRight(strings.ReplaceAll(e.Text, "\r", ""), "\n")
		switch e.Type {
		case "stderr":
			text = red.Render(text)
		case "command":
			text = cyan.Render("$ " + text)
		}
		b.WriteString(faint.Render(time.UnixMilli(e.Created).Format("15:04:05")))
		b.WriteString("  ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	switch {
	case m.logsErr != nil:
		b.WriteString(red.Render("Couldn't load logs: " + m.logsErr.Error()))
	case len(m.events) == 0 && m.logDep.active():
		b.WriteString(faint.Render("Waiting for build output…"))
	case len(m.events) == 0:
		b.WriteString(faint.Render("No build logs for this deployment"))
	case m.logDep.active():
		b.WriteString(m.spin.View() + faint.Render(" building…"))
	}
	m.vp.SetContent(strings.TrimRight(b.String(), "\n"))
	if follow {
		m.vp.GotoBottom()
	}
}

func (m model) footer() []string {
	if m.confirm != nil {
		bar := yellow.Render("▌ ")
		return []string{
			m.rule(),
			ansi.Truncate(bar+bold.Render(m.confirm.prompt)+faint.Render("   y confirm · any other key cancels"), m.width, "…"),
			ansi.Truncate(bar+faint.Render(m.confirm.detail), m.width, "…"),
		}
	}

	status := ""
	switch {
	case m.busy:
		status = m.spin.View() + " Working…"
	case m.flash != "" && m.flashBad:
		status = red.Render("✗ " + m.flash)
	case m.flash != "":
		status = green.Render("✓ ") + m.flash
	}

	return []string{m.rule(), ansi.Truncate(" "+m.help(), m.width, "…"), ansi.Truncate(" "+status, m.width, "…")}
}

func (m model) help() string {
	sep := faint.Render("   ")
	actions := keys("R", "redeploy", "P", "promote", "B", "rollback", "C", "cancel")
	runtimeKeys := keys("e", "problems only", "s", "static files")
	var full, compact string
	switch {
	case m.screen == liveScreen:
		full = keys("↑↓ pgup pgdn", "scroll", "g/G", "top/bottom") + sep + runtimeKeys + sep + keys("o", "open", "i", "dashboard", "r", "reload", "tab", "deployments", "q", "quit")
		compact = keys("↑↓", "scroll", "e", "problems", "s", "static", "tab", "deployments", "q", "quit")
	case m.screen == detailScreen && m.detailRuntime:
		full = keys("↑↓", "scroll", "g/G", "top/bottom") + sep + runtimeKeys + sep + keys("tab", "build logs") + sep + actions + sep + keys("esc", "back")
		compact = keys("↑↓", "scroll", "e", "problems", "s", "static", "tab", "build", "esc", "back")
	case m.screen == detailScreen:
		full = keys("↑↓ pgup pgdn", "scroll", "g/G", "top/bottom", "o", "open", "i", "dashboard", "tab", "runtime logs") + sep + actions + sep + keys("esc", "back")
		compact = keys("↑↓", "scroll", "tab", "runtime", "R/P/B/C", "actions", "esc", "back")
	default:
		full = keys("↑↓", "select", "enter", "details", "o", "open", "i", "dashboard", "f", "filter", "tab", "live") + sep + actions + sep + keys("q", "quit")
		compact = keys("⏎", "details", "tab", "live", "f", "filter", "R/P/B/C", "actions", "q", "quit")
	}
	if lipgloss.Width(full) < m.width {
		return full
	}
	return compact
}

func keys(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, bold.Render(pairs[i])+" "+faint.Render(pairs[i+1]))
	}
	return strings.Join(parts, "  ")
}

func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if n := lipgloss.Width(s); n < w {
		s += strings.Repeat(" ", w-n)
	}
	return s
}

func padLeft(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	if n := lipgloss.Width(s); n < w {
		s = strings.Repeat(" ", w-n) + s
	}
	return s
}

func truncate(s string, w int) string {
	return ansi.Truncate(s, w, "…")
}

func spread(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		return ansi.Truncate(left+"  "+right, width, "…")
	}
	return left + strings.Repeat(" ", gap) + right
}

func ago(ms int64, now time.Time) string {
	if ms == 0 {
		return "-"
	}
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < 5*time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func buildTime(d deployment, now time.Time) string {
	if d.BuildingAt == 0 {
		return "-"
	}
	end := d.Ready
	if end == 0 {
		if !d.active() {
			return "-"
		}
		end = now.UnixMilli()
	}
	dur := time.Duration(end-d.BuildingAt) * time.Millisecond
	if dur < time.Minute {
		return fmt.Sprintf("%ds", int(dur.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(dur.Minutes()), int(dur.Seconds())%60)
}
