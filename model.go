package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

type screen int

const (
	listScreen screen = iota
	liveScreen
	detailScreen
)

type actionKind int

const (
	actRedeploy actionKind = iota
	actPromote
	actPromotePreview
	actRollback
	actCancel
)

type pending struct {
	kind   actionKind
	dep    deployment
	prompt string
	detail string
}

type (
	tickMsg    struct{ seq int }
	logTickMsg struct{ seq int }
	flashMsg   struct {
		text string
		bad  bool
	}
	flashExpireMsg struct{ seq int }
	dataMsg        struct {
		filter  int
		project project
		deps    []deployment
		err     error
	}
	scopeMsg  struct{ name string }
	eventsMsg struct {
		id     string
		events []event
		err    error
	}
	actionMsg struct {
		text string
		err  error
	}
	runtimeMsg struct {
		which  runtimeWhich
		gen    int
		until  time.Time
		window time.Duration
		reqs   []requestRecord
		logs   []logLine
		err    error
	}
	runtimePollMsg struct {
		which runtimeWhich
		seq   int
	}
)

var filters = []string{"", "production", "preview"}

const deploymentLimit = 40

type model struct {
	api  *client
	link projectLink

	scope    string
	proj     project
	deps     []deployment
	filter   int
	loading  bool
	fetched  time.Time
	fetchErr error
	tickSeq  int

	cursor int
	offset int
	width  int
	height int
	screen screen
	spin   spinner.Model

	detailRuntime bool
	vp            viewport.Model
	logDep        deployment
	events        []event
	logsErr       error
	logsSeen      bool
	logSeq        int

	live     runtimeView
	deployRT runtimeView

	flash    string
	flashBad bool
	flashSeq int
	confirm  *pending
	busy     bool
}

func newModel(api *client, link projectLink) model {
	vp := viewport.New()
	vp.SoftWrap = true
	return model{
		api:      api,
		link:     link,
		loading:  true,
		spin:     spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(yellow)),
		vp:       vp,
		live:     newRuntimeView(),
		deployRT: newRuntimeView(),
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.fetch(), m.fetchScope(), m.spin.Tick)
}

func (m model) fetch() tea.Cmd {
	api, projectID, filter := m.api, m.link.ProjectID, m.filter
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		p, err := api.project(ctx, projectID)
		if err != nil {
			return dataMsg{filter: filter, err: err}
		}
		deps, err := api.deployments(ctx, projectID, filters[filter], deploymentLimit)
		return dataMsg{filter: filter, project: p, deps: deps, err: err}
	}
}

func (m model) fetchScope() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		name, err := api.scopeName(ctx)
		if err != nil {
			return nil
		}
		return scopeMsg{name: name}
	}
}

func (m model) fetchEvents() tea.Cmd {
	api, id := m.api, m.logDep.UID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		evs, err := api.events(ctx, id)
		return eventsMsg{id: id, events: evs, err: err}
	}
}

func (m *model) rt(which runtimeWhich) *runtimeView {
	if which == liveRuntime {
		return &m.live
	}
	return &m.deployRT
}

func (m model) runtimeVisible(which runtimeWhich) bool {
	if which == liveRuntime {
		return m.screen == liveScreen
	}
	return m.screen == detailScreen && m.detailRuntime
}

func (m model) activeRuntime() (runtimeWhich, bool) {
	switch {
	case m.screen == liveScreen:
		return liveRuntime, true
	case m.screen == detailScreen && m.detailRuntime:
		return deployRuntime, true
	}
	return 0, false
}

func (m *model) fetchRuntime(which runtimeWhich) tea.Cmd {
	r := m.rt(which)
	if r.deployID == "" || r.loading {
		return nil
	}
	r.loading = true
	api, owner, projectID, deployID, gen := m.api, m.link.OrgID, m.link.ProjectID, r.deployID, r.gen
	initial, since, window := r.until.IsZero(), r.until.Add(-runtimeOverlap), r.window
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		until := time.Now()
		if !initial {
			reqs, logs, err := api.runtime(ctx, owner, projectID, deployID, since, until, 200)
			return runtimeMsg{which: which, gen: gen, until: until, window: window, reqs: reqs, logs: logs, err: err}
		}
		var err error
		for _, w := range runtimeWindows {
			var reqs []requestRecord
			var logs []logLine
			reqs, logs, err = api.runtime(ctx, owner, projectID, deployID, until.Add(-w), until, 500)
			if err == nil || !isRetentionError(err) {
				return runtimeMsg{which: which, gen: gen, until: until, window: w, reqs: reqs, logs: logs, err: err}
			}
		}
		return runtimeMsg{which: which, gen: gen, err: err}
	}
}

func (m *model) showRuntime(which runtimeWhich, deployID string) tea.Cmd {
	if deployID == "" {
		return nil
	}
	r := m.rt(which)
	if r.deployID != deployID {
		r.reset(deployID)
		r.render(m.width)
	}
	r.pollSeq++
	return m.fetchRuntime(which)
}

func (m *model) schedulePoll(which runtimeWhich) tea.Cmd {
	r := m.rt(which)
	r.pollSeq++
	seq := r.pollSeq
	return tea.Tick(runtimePoll, func(time.Time) tea.Msg { return runtimePollMsg{which: which, seq: seq} })
}

func (m *model) scheduleTick() tea.Cmd {
	m.tickSeq++
	seq := m.tickSeq
	every := 15 * time.Second
	for _, d := range m.deps {
		if d.active() {
			every = 3 * time.Second
			break
		}
	}
	return tea.Tick(every, func(time.Time) tea.Msg { return tickMsg{seq: seq} })
}

func (m *model) scheduleLogTick() tea.Cmd {
	m.logSeq++
	seq := m.logSeq
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return logTickMsg{seq: seq} })
}

func (m *model) setFlash(text string, bad bool) tea.Cmd {
	m.flash, m.flashBad = text, bad
	m.flashSeq++
	seq := m.flashSeq
	return tea.Tick(6*time.Second, func(time.Time) tea.Msg { return flashExpireMsg{seq: seq} })
}

func (m *model) refresh() tea.Cmd {
	if m.loading {
		return nil
	}
	m.loading = true
	return m.fetch()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		for _, vp := range []*viewport.Model{&m.vp, &m.live.vp, &m.deployRT.vp} {
			vp.SetWidth(m.width)
			vp.SetHeight(max(1, m.height-detailChrome))
		}
		m.live.vp.SetHeight(max(1, m.height-liveChrome))
		for _, r := range []*runtimeView{&m.live, &m.deployRT} {
			if r.deployID != "" {
				r.render(m.width)
			}
		}
		m.clampCursor()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case scopeMsg:
		m.scope = msg.name
		return m, nil

	case dataMsg:
		if msg.filter != m.filter {
			return m, nil
		}
		m.loading = false
		m.fetchErr = msg.err
		var cmds []tea.Cmd
		if msg.err == nil {
			selected := ""
			if d, ok := m.selected(); ok && m.screen == listScreen {
				selected = d.UID
			}
			m.proj, m.deps, m.fetched = msg.project, msg.deps, time.Now()
			if i := m.indexOf(selected); i >= 0 {
				m.cursor = i
			}
			m.clampCursor()
			if i := m.indexOf(m.logDep.UID); i >= 0 {
				wasActive := m.logDep.active()
				m.logDep = m.deps[i]
				if m.screen == detailScreen && wasActive && !m.logDep.active() {
					cmds = append(cmds, m.fetchEvents())
				}
			}
			cmds = append(cmds, m.followLive())
		}
		cmds = append(cmds, m.scheduleTick())
		return m, tea.Batch(cmds...)

	case tickMsg:
		if msg.seq != m.tickSeq {
			return m, nil
		}
		return m, m.refresh()

	case eventsMsg:
		if msg.id != m.logDep.UID || m.screen != detailScreen {
			return m, nil
		}
		m.logsErr = msg.err
		if msg.err == nil {
			m.events = msg.events
		}
		m.renderLogs(!m.logsSeen)
		m.logsSeen = true
		if m.logDep.active() && !m.detailRuntime {
			return m, m.scheduleLogTick()
		}
		return m, nil

	case logTickMsg:
		if msg.seq != m.logSeq || m.screen != detailScreen || m.detailRuntime {
			return m, nil
		}
		return m, m.fetchEvents()

	case runtimeMsg:
		r := m.rt(msg.which)
		if msg.gen != r.gen {
			return m, nil
		}
		r.loading = false
		r.err = msg.err
		if msg.err == nil {
			r.merge(msg.reqs, msg.logs)
			r.until, r.window, r.loaded = msg.until, msg.window, true
		}
		r.render(m.width)
		if m.runtimeVisible(msg.which) {
			return m, m.schedulePoll(msg.which)
		}
		return m, nil

	case runtimePollMsg:
		if msg.seq != m.rt(msg.which).pollSeq || !m.runtimeVisible(msg.which) {
			return m, nil
		}
		return m, m.fetchRuntime(msg.which)

	case actionMsg:
		m.busy = false
		var flash tea.Cmd
		if msg.err != nil {
			flash = m.setFlash(msg.err.Error(), true)
		} else {
			flash = m.setFlash(msg.text, false)
		}
		m.loading = true
		return m, tea.Batch(flash, m.fetch())

	case flashMsg:
		return m, m.setFlash(msg.text, msg.bad)

	case flashExpireMsg:
		if msg.seq == m.flashSeq {
			m.flash = ""
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) followLive() tea.Cmd {
	prod := m.proj.productionID()
	if prod == "" || prod == m.live.deployID {
		return nil
	}
	var flash tea.Cmd
	if m.live.deployID != "" {
		flash = m.setFlash("Live now follows the new production deployment "+deployment{UID: prod}.short(), false)
	}
	if m.screen == liveScreen {
		return tea.Batch(flash, m.showRuntime(liveRuntime, prod))
	}
	m.live.reset(prod)
	return flash
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if m.confirm != nil {
		p := *m.confirm
		m.confirm = nil
		if key == "y" || key == "Y" {
			m.busy = true
			return m, m.run(p)
		}
		return m, m.setFlash("Cancelled, nothing changed", false)
	}

	switch key {
	case "ctrl+c":
		return m, tea.Quit
	case "q":
		if m.screen == detailScreen {
			m.closeDetail()
			return m, nil
		}
		return m, tea.Quit
	case "esc", "backspace":
		if m.screen == detailScreen {
			m.closeDetail()
		}
		return m, nil
	case "tab", "shift+tab":
		return m, m.switchTab()
	case "o", "i":
		d, ok := m.selected()
		if !ok {
			return m, nil
		}
		target := "https://" + d.URL
		if key == "i" {
			target = d.InspectorURL
		}
		return m, openURL(target)
	case "R", "P", "B", "C":
		if m.screen == liveScreen {
			return m, m.setFlash("Run actions from the Deployments tab or a deployment's detail view", true)
		}
		d, ok := m.selected()
		if !ok {
			return m, nil
		}
		if m.busy {
			return m, m.setFlash("Another action is still running", true)
		}
		p, err := m.prepare(key, d)
		if err != nil {
			return m, m.setFlash(err.Error(), true)
		}
		m.confirm = &p
		return m, nil
	}

	if which, ok := m.activeRuntime(); ok {
		r := m.rt(which)
		switch key {
		case "e":
			r.problemsOnly = !r.problemsOnly
			r.render(m.width)
		case "s":
			r.showStatic = !r.showStatic
			r.render(m.width)
		case "r":
			id := r.deployID
			r.deployID = ""
			return m, m.showRuntime(which, id)
		case "g", "home":
			r.vp.GotoTop()
		case "G", "end":
			r.vp.GotoBottom()
		default:
			var cmd tea.Cmd
			r.vp, cmd = r.vp.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	if m.screen == listScreen {
		switch key {
		case "r":
			return m, m.refresh()
		case "up", "k":
			m.cursor--
		case "down", "j":
			m.cursor++
		case "pgup", "ctrl+u":
			m.cursor -= max(1, m.listHeight()/2)
		case "pgdown", "ctrl+d":
			m.cursor += max(1, m.listHeight()/2)
		case "g", "home":
			m.cursor = 0
		case "G", "end":
			m.cursor = len(m.deps) - 1
		case "f":
			m.filter = (m.filter + 1) % len(filters)
			m.deps, m.cursor, m.offset = nil, 0, 0
			m.loading = true
			return m, m.fetch()
		case "enter", "l", "right":
			if d, ok := m.selected(); ok {
				return m, m.openDetail(d)
			}
		}
		m.clampCursor()
		return m, nil
	}

	switch key {
	case "r":
		return m, m.fetchEvents()
	case "g", "home":
		m.vp.GotoTop()
	case "G", "end":
		m.vp.GotoBottom()
	default:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) switchTab() tea.Cmd {
	switch m.screen {
	case listScreen:
		m.screen = liveScreen
		return m.showRuntime(liveRuntime, m.proj.productionID())
	case liveScreen:
		m.screen = listScreen
		m.live.pollSeq++
		return nil
	}
	m.detailRuntime = !m.detailRuntime
	if m.detailRuntime {
		m.logSeq++
		return m.showRuntime(deployRuntime, m.logDep.UID)
	}
	m.deployRT.pollSeq++
	return m.fetchEvents()
}

func (m *model) openDetail(d deployment) tea.Cmd {
	m.screen = detailScreen
	m.detailRuntime = false
	m.logDep = d
	m.events, m.logsErr, m.logsSeen = nil, nil, false
	m.vp.SetContent(faint.Render("Loading build logs…"))
	m.logSeq++
	return m.fetchEvents()
}

func (m *model) closeDetail() {
	m.screen = listScreen
	m.detailRuntime = false
	m.logSeq++
	m.deployRT.pollSeq++
	if i := m.indexOf(m.logDep.UID); i >= 0 {
		m.cursor = i
		m.clampCursor()
	}
	m.logDep = deployment{}
}

func (m model) selected() (deployment, bool) {
	switch m.screen {
	case detailScreen:
		return m.logDep, m.logDep.UID != ""
	case liveScreen:
		if i := m.indexOf(m.live.deployID); i >= 0 {
			return m.deps[i], true
		}
		return deployment{}, false
	}
	if m.cursor < 0 || m.cursor >= len(m.deps) {
		return deployment{}, false
	}
	return m.deps[m.cursor], true
}

func (m model) indexOf(uid string) int {
	if uid == "" {
		return -1
	}
	for i, d := range m.deps {
		if d.UID == uid {
			return i
		}
	}
	return -1
}

func (m *model) clampCursor() {
	n := len(m.deps)
	m.cursor = max(0, min(m.cursor, n-1))
	h := max(1, m.listHeight())
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	m.offset = max(0, min(m.offset, n-h))
}

func (m model) prepare(key string, d deployment) (pending, error) {
	label := fmt.Sprintf("%s %q", d.short(), truncate(d.message(), 40))
	live := d.UID == m.proj.productionID()
	switch key {
	case "R":
		env := "preview"
		if d.production() {
			env = "production"
		}
		return pending{
			kind:   actRedeploy,
			dep:    d,
			prompt: fmt.Sprintf("Redeploy %s to %s?", label, env),
			detail: "Builds the same commit again as a new deployment.",
		}, nil
	case "P":
		if !d.production() {
			return pending{
				kind:   actPromotePreview,
				dep:    d,
				prompt: fmt.Sprintf("Promote preview %s to production?", label),
				detail: "Previews are built with preview env vars, so this rebuilds it as a new production deployment.",
			}, nil
		}
		if d.State != "READY" {
			return pending{}, errors.New("Only Ready deployments can be promoted")
		}
		if live && m.proj.autoAssign() {
			return pending{}, errors.New("This deployment is already live in production")
		}
		return pending{
			kind:   actPromote,
			dep:    d,
			prompt: fmt.Sprintf("Promote %s to production?", label),
			detail: "Points production domains at this build without rebuilding. Later pushes go live automatically again.",
		}, nil
	case "B":
		if !d.production() || !d.IsRollbackCandidate {
			return pending{}, errors.New("You can only roll back to a previous production deployment")
		}
		if live {
			return pending{}, errors.New("This deployment is already live in production")
		}
		branch := m.proj.Link.ProductionBranch
		if branch == "" {
			branch = "the production branch"
		}
		return pending{
			kind:   actRollback,
			dep:    d,
			prompt: fmt.Sprintf("Roll production back to %s?", label),
			detail: fmt.Sprintf("Pushes to %s stop going live until you promote a deployment (P).", branch),
		}, nil
	case "C":
		if !d.active() {
			return pending{}, errors.New("Only queued or building deployments can be cancelled")
		}
		return pending{
			kind:   actCancel,
			dep:    d,
			prompt: fmt.Sprintf("Cancel %s?", label),
			detail: "Stops the build. It can't be resumed, only redeployed.",
		}, nil
	}
	return pending{}, errors.New("unknown action")
}

func (m model) run(p pending) tea.Cmd {
	api, proj := m.api, m.proj
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		d := p.dep
		switch p.kind {
		case actRedeploy:
			url, err := api.redeploy(ctx, proj, d.UID, d.Target)
			return actionMsg{text: "Redeploy started: " + url, err: err}
		case actPromotePreview:
			url, err := api.redeploy(ctx, proj, d.UID, "production")
			return actionMsg{text: "Production build started: " + url, err: err}
		case actPromote:
			accepted, err := api.promote(ctx, proj.ID, d.UID)
			text := "Promoted " + d.short() + " to production"
			if accepted {
				text = "Promoting " + d.short() + " to production; domains switch over shortly"
			}
			return actionMsg{text: text, err: err}
		case actRollback:
			err := api.rollback(ctx, proj.ID, d.UID)
			return actionMsg{text: "Rolled production back to " + d.short(), err: err}
		case actCancel:
			err := api.cancel(ctx, d.UID)
			return actionMsg{text: "Cancelled " + d.short(), err: err}
		}
		return nil
	}
}

func openURL(target string) tea.Cmd {
	return func() tea.Msg {
		if target == "" || target == "https://" {
			return flashMsg{text: "No URL for this deployment", bad: true}
		}
		name := "xdg-open"
		if runtime.GOOS == "darwin" {
			name = "open"
		}
		if err := exec.Command(name, target).Start(); err != nil {
			return flashMsg{text: err.Error(), bad: true}
		}
		return flashMsg{text: "Opened " + target}
	}
}
