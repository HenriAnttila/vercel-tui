package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type runtimeWhich int

const (
	liveRuntime runtimeWhich = iota
	deployRuntime
)

const (
	runtimePoll    = 5 * time.Second
	runtimeOverlap = 2 * time.Minute
	runtimeMaxRows = 2000
)

var runtimeWindows = []time.Duration{23 * time.Hour, 55 * time.Minute}

type runtimeEntry struct {
	key    string
	at     time.Time
	req    requestRecord
	hasReq bool
	method string
	path   string
	lines  []logLine
}

func (e *runtimeEntry) problem() bool {
	if e.hasReq && e.req.HTTPStatus >= 400 {
		return true
	}
	for _, l := range e.lines {
		switch l.Level {
		case "warning", "error", "fatal":
			return true
		}
	}
	return false
}

type runtimeView struct {
	deployID     string
	gen          int
	entries      []*runtimeEntry
	index        map[string]*runtimeEntry
	seenLogs     map[string]bool
	requests     int
	until        time.Time
	window       time.Duration
	loaded       bool
	loading      bool
	err          error
	problemsOnly bool
	showStatic   bool
	pollSeq      int
	vp           viewport.Model
	rendered     bool
}

func newRuntimeView() runtimeView {
	vp := viewport.New()
	vp.SoftWrap = true
	return runtimeView{vp: vp}
}

func (r *runtimeView) reset(deployID string) {
	r.deployID = deployID
	r.gen++
	r.pollSeq++
	r.entries, r.index, r.seenLogs, r.requests = nil, map[string]*runtimeEntry{}, map[string]bool{}, 0
	r.until, r.window = time.Time{}, 0
	r.loaded, r.loading, r.err, r.rendered = false, false, nil, false
}

func (r *runtimeView) entry(key string) *runtimeEntry {
	if e, ok := r.index[key]; ok {
		return e
	}
	e := &runtimeEntry{key: key}
	r.index[key] = e
	r.entries = append(r.entries, e)
	return e
}

func (r *runtimeView) merge(reqs []requestRecord, logs []logLine) {
	for _, q := range reqs {
		key := q.RequestID
		if key == "" {
			key = q.Timestamp + q.RequestPath
		}
		e := r.entry(key)
		if !e.hasReq {
			r.requests++
		}
		e.req, e.hasReq = q, true
		e.at = parseTime(q.Timestamp)
		e.method, e.path = q.RequestMethod, q.RequestPath
	}
	for _, l := range logs {
		id := l.Timestamp + "|" + l.RequestID + "|" + l.Message
		if r.seenLogs[id] {
			continue
		}
		r.seenLogs[id] = true
		l.at = parseTime(l.Timestamp)
		key := l.RequestID
		if key == "" {
			key = "log|" + id
		}
		e := r.entry(key)
		if !e.hasReq {
			if e.path == "" {
				e.method, e.path = l.RequestMethod, l.RequestPath
			}
			if e.at.IsZero() || (!l.at.IsZero() && l.at.Before(e.at)) {
				e.at = l.at
			}
		}
		e.lines = append(e.lines, l)
		sort.SliceStable(e.lines, func(i, j int) bool { return e.lines[i].at.Before(e.lines[j].at) })
	}
	sort.SliceStable(r.entries, func(i, j int) bool { return r.entries[i].at.Before(r.entries[j].at) })
	if extra := len(r.entries) - runtimeMaxRows; extra > 0 {
		for _, e := range r.entries[:extra] {
			delete(r.index, e.key)
			if e.hasReq {
				r.requests--
			}
		}
		r.entries = r.entries[extra:]
	}
}

func (r *runtimeView) visible(e *runtimeEntry) bool {
	if !r.showStatic && e.hasReq && e.req.PathType == "static" {
		return false
	}
	return !r.problemsOnly || e.problem()
}

func (r *runtimeView) stats() string {
	if !r.loaded {
		return ""
	}
	span := "last " + windowLabel(r.window)
	if len(r.entries) > 0 && !r.entries[0].at.IsZero() {
		span = "since " + r.entries[0].at.Local().Format("Mon 15:04")
	}
	return fmt.Sprintf("%s · %d requests · %d log lines", span, r.requests, len(r.seenLogs))
}

func (r *runtimeView) tags() string {
	var tags []string
	if r.problemsOnly {
		tags = append(tags, "problems only")
	}
	if r.showStatic {
		tags = append(tags, "+static files")
	}
	if len(tags) == 0 {
		return ""
	}
	return cyan.Render(strings.Join(tags, " · "))
}

func (r *runtimeView) render(width int) {
	follow := !r.rendered || r.vp.AtBottom()
	var b strings.Builder
	shown, lastDay := 0, ""
	for _, e := range r.entries {
		if !r.visible(e) {
			continue
		}
		if day := e.at.Local().Format("Mon 2 Jan"); day != lastDay {
			b.WriteString(faint.Render("── "+day+" ──") + "\n")
			lastDay = day
		}
		b.WriteString(requestLine(e, width) + "\n")
		for _, l := range e.lines {
			b.WriteString(logLineText(l))
		}
		shown++
	}
	switch {
	case shown > 0:
	case !r.loaded && r.err == nil:
		b.WriteString(faint.Render("Loading runtime logs…"))
	case !r.loaded:
	case r.problemsOnly && len(r.entries) > 0:
		b.WriteString(faint.Render(fmt.Sprintf("No problems in the last %s: no 4xx/5xx responses, warnings or errors", windowLabel(r.window))))
	case len(r.entries) > 0:
		b.WriteString(faint.Render("Only static file requests so far (s shows them)"))
	default:
		b.WriteString(faint.Render(fmt.Sprintf("No requests to this deployment in the last %s", windowLabel(r.window))))
	}
	if r.err != nil {
		b.WriteString("\n" + red.Render("Couldn't load runtime logs: "+r.err.Error()))
	}
	r.vp.SetContent(strings.Trim(b.String(), "\n"))
	if follow {
		r.vp.GotoBottom()
	}
	r.rendered = true
}

func requestLine(e *runtimeEntry, width int) string {
	ts := faint.Render(e.at.Local().Format("15:04:05"))
	status := faint.Render("  —")
	if e.hasReq && e.req.HTTPStatus > 0 {
		status = statusStyle(e.req.HTTPStatus).Render(strconv.Itoa(e.req.HTTPStatus))
	}
	method := e.method
	if method == "" {
		method = "-"
	}
	tail := ""
	if e.hasReq {
		tail = faint.Render(strings.TrimSpace(pathTypeLabel(e.req.PathType) + " " + strings.ToLower(e.req.CacheResult)))
	}
	pathW := min(60, max(10, width-8-2-3-2-7-2-lipgloss.Width(tail)))
	line := ts + "  " + status + "  " + pad(method, 7) + pad(e.path, pathW) + "  " + tail
	return ansi.Truncate(line, width, "…")
}

func logLineText(l logLine) string {
	style := lipgloss.NewStyle()
	badge := faint.Render(pad(l.Level, 5))
	switch l.Level {
	case "error", "fatal":
		style, badge = red, red.Bold(true).Render(pad(l.Level, 5))
	case "warning":
		style, badge = yellow, yellow.Bold(true).Render("warn ")
	case "debug", "trace":
		style = faint
	}
	msg := strings.TrimRight(strings.ReplaceAll(l.Message, "\r", ""), "\n")
	if l.Truncated {
		msg += faint.Render(" (truncated)")
	}
	gutter := faint.Render("          │ ")
	var b strings.Builder
	for i, line := range strings.Split(msg, "\n") {
		lead := badge
		if i > 0 {
			lead = "     "
		}
		b.WriteString(gutter + lead + " " + style.Render(line) + "\n")
	}
	return b.String()
}

func statusStyle(code int) lipgloss.Style {
	switch {
	case code >= 500:
		return red.Bold(true)
	case code >= 400:
		return yellow
	case code >= 300:
		return cyan
	}
	return green
}

func pathTypeLabel(pathType string) string {
	if strings.HasSuffix(pathType, "func") {
		return "function"
	}
	return pathType
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func windowLabel(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
