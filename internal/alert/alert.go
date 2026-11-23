// Package alert turns health and queries into alerts and notifications.
//
// Two kinds of monitor exist:
//
//   - health: zero-config. It watches the application × location health matrix
//     and alerts when a cell stops being healthy. One is created by default.
//   - query: the same query object used by the explorer and dashboards, plus a
//     comparison ("p95 of checkout > 800 for 5m"). It alerts per series.
//
// When an alert opens, Telepathy runs Explain on its scope and puts the answer
// — what is different, and which change probably triggered it — in the
// notification, so the page arrives with a hypothesis attached.
package alert

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/gthimmes/telepathy/internal/query"
	"github.com/gthimmes/telepathy/internal/store"
)

// HealthSpec configures a health monitor.
type HealthSpec struct {
	Statuses []string `json:"statuses"` // which statuses alert; default critical, degraded, silent
	Service  string   `json:"service"`  // glob; empty = all
	Location string   `json:"location"` // glob; empty = all
	ForEvals int      `json:"forEvals"` // consecutive evaluations before firing/resolving; default 2
}

// QuerySpec configures a query monitor.
type QuerySpec struct {
	Query     query.Request `json:"query"`
	Op        string        `json:"op"` // > | >= | < | <=
	Threshold float64       `json:"threshold"`
	Window    string        `json:"window"`   // evaluation window, default 5m
	Severity  string        `json:"severity"` // critical (default) | degraded
	ForEvals  int           `json:"forEvals"`
}

// Details is the evidence attached to an alert.
type Details struct {
	Scope    string          `json:"scope"`              // filter that reproduces the alert's subject
	Reasons  []string        `json:"reasons,omitempty"`  // why health flagged it
	Value    *float64        `json:"value,omitempty"`    // query monitors: the measured value
	Unit     string          `json:"unit,omitempty"`     //
	Findings []query.Finding `json:"findings,omitempty"` // Explain: what is over-represented
	Triggers []string        `json:"triggers,omitempty"` // change events that likely caused it
	CausedBy string          `json:"causedBy,omitempty"` // an unhealthy dependency this alert is downstream of
	Impacted []string        `json:"impacted,omitempty"` // callers that are unhealthy because of this one
}

// ValidateMonitor checks a monitor's spec (query monitors are test-run).
func ValidateMonitor(st *store.Store, m *store.Monitor) error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("name is required")
	}
	switch m.Kind {
	case "health":
		var s HealthSpec
		if err := json.Unmarshal(m.Spec, &s); err != nil {
			return fmt.Errorf("invalid spec: %w", err)
		}
		for _, st := range s.Statuses {
			if st != query.Critical && st != query.Degraded && st != query.Silent {
				return fmt.Errorf("unknown status %q", st)
			}
		}
		for _, g := range []string{s.Service, s.Location} {
			if _, err := path.Match(g, ""); err != nil {
				return fmt.Errorf("invalid pattern %q", g)
			}
		}
	case "query":
		var s QuerySpec
		if err := json.Unmarshal(m.Spec, &s); err != nil {
			return fmt.Errorf("invalid spec: %w", err)
		}
		switch s.Op {
		case ">", ">=", "<", "<=":
		default:
			return fmt.Errorf("op must be one of >, >=, <, <=")
		}
		if s.Severity != "" && s.Severity != query.Critical && s.Severity != query.Degraded {
			return fmt.Errorf("severity must be critical or degraded")
		}
		req := s.Query
		req.From, req.To, req.Range = 0, 0, s.window()
		if _, err := query.Run(st.DB(), req); err != nil {
			return err
		}
	default:
		return fmt.Errorf("kind must be health or query")
	}
	return nil
}

func (s QuerySpec) window() string {
	if s.Window == "" {
		return "5m"
	}
	return s.Window
}

func forEvals(n int) int {
	if n <= 0 {
		return 2
	}
	return n
}

// DefaultHealthMonitor is installed on first start so alerting works with no setup.
const DefaultHealthMonitor = "default-health"

// EnsureDefaults creates the built-in health monitor once.
func EnsureDefaults(st *store.Store) error {
	if st.Setting("alert.defaults") != "" {
		return nil
	}
	spec, _ := json.Marshal(HealthSpec{Statuses: []string{query.Critical, query.Degraded, query.Silent}, ForEvals: 2})
	if err := st.PutMonitor(&store.Monitor{ID: DefaultHealthMonitor, Name: "Application health, everywhere", Kind: "health", Spec: spec, Enabled: true}); err != nil {
		return err
	}
	return st.SetSetting("alert.defaults", "1")
}

// cond is a monitor condition that currently holds for one subject.
type cond struct {
	key, severity, title, summary string
	service, location             string
	details                       Details
	explainMode                   string // "" = no Explain
}

func globMatch(pattern, s string) bool {
	if pattern == "" {
		return true
	}
	ok, _ := path.Match(pattern, s)
	return ok
}

func healthConds(spec HealthSpec, rep *query.HealthReport) []cond {
	want := map[string]bool{}
	for _, s := range spec.Statuses {
		want[s] = true
	}
	if len(want) == 0 {
		want = map[string]bool{query.Critical: true, query.Degraded: true, query.Silent: true}
	}
	var out []cond
	for _, c := range rep.Cells {
		if !want[c.Status] || !globMatch(spec.Service, c.Service) || !globMatch(spec.Location, c.Location) {
			continue
		}
		cd := cond{
			key: c.Service + "|" + c.Location, severity: c.Status, service: c.Service, location: c.Location,
			title:   fmt.Sprintf("%s in %s is %s", c.Service, c.Location, c.Status),
			summary: strings.Join(c.Reasons, "; "),
			details: Details{Scope: scopeFilter(c.Service, c.Location), Reasons: c.Reasons},
		}
		if c.Status != query.Silent {
			cd.explainMode = "errors"
			if !strings.Contains(cd.summary, "error rate") && strings.Contains(cd.summary, "latency") {
				cd.explainMode = "slow"
			}
		}
		out = append(out, cd)
	}
	return out
}

// attribute finds the root of a failure that spans several services. When an
// unhealthy service calls another unhealthy service in the same location and
// those calls are failing or slow, the caller is a symptom: its alert is still
// recorded, but it is marked as downstream and only the root notifies.
func (e *Engine) attribute(conds []cond, rep *query.HealthReport) {
	type key struct{ svc, loc string }
	sick := map[key]query.Cell{}
	for _, c := range rep.Cells {
		if c.Status == query.Critical || c.Status == query.Degraded {
			sick[key{c.Service, c.Location}] = c
		}
	}
	alerting := map[key]int{}
	perLoc := map[string]int{}
	for i, c := range conds {
		if _, ok := sick[key{c.service, c.location}]; ok {
			alerting[key{c.service, c.location}] = i
			perLoc[c.location]++
		}
	}
	now := time.Now()
	tr := query.TimeRange{FromNs: now.Add(-10 * time.Minute).UnixNano(), ToNs: now.UnixNano()}
	for loc, n := range perLoc {
		if n < 2 {
			continue
		}
		edges, _, err := query.Edges(e.Store.DB(), tr, loc)
		if err != nil {
			continue
		}
		// blames[x] = unhealthy, alerting dependencies that x's own calls implicate.
		blames := map[string][]string{}
		for _, ed := range edges {
			callee, ok := sick[key{ed.To, loc}]
			if _, alerts := alerting[key{ed.To, loc}]; !ok || !alerts || ed.From == ed.To {
				continue
			}
			slow := strings.Contains(strings.Join(callee.Reasons, " "), "latency")
			if ed.ErrorRate >= 1 || slow {
				blames[ed.From] = append(blames[ed.From], ed.To)
			}
		}
		for k, i := range alerting {
			if k.loc != loc {
				continue
			}
			// Follow the blame chain to its end; stop on a cycle.
			root, seen := k.svc, map[string]bool{k.svc: true}
			for {
				next := ""
				for _, cand := range blames[root] {
					if !seen[cand] {
						next = cand
						break
					}
				}
				if next == "" {
					break
				}
				seen[next] = true
				root = next
			}
			if root == k.svc {
				continue
			}
			conds[i].details.CausedBy = root + " in " + loc
			ri := alerting[key{root, loc}]
			conds[ri].details.Impacted = append(conds[ri].details.Impacted, k.svc)
		}
	}
	for i := range conds {
		sort.Strings(conds[i].details.Impacted)
	}
}

func quoteValue(v string) string {
	if strings.ContainsAny(v, " \t\"") {
		return `"` + strings.ReplaceAll(v, `"`, "") + `"`
	}
	return v
}

func scopeFilter(service, location string) string {
	var parts []string
	if service != "" {
		parts = append(parts, "service:"+quoteValue(service))
	}
	if location != "" {
		parts = append(parts, "location:"+quoteValue(location))
	}
	return strings.Join(parts, " ")
}

func compare(v float64, op string, threshold float64) bool {
	switch op {
	case ">":
		return v > threshold
	case ">=":
		return v >= threshold
	case "<":
		return v < threshold
	case "<=":
		return v <= threshold
	}
	return false
}

func fmtNum(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

func (e *Engine) queryConds(m store.Monitor, spec QuerySpec) ([]cond, error) {
	req := spec.Query
	req.From, req.To, req.Range, req.Limit = 0, 0, spec.window(), 50
	res, err := query.Run(e.Store.DB(), req)
	if err != nil {
		return nil, err
	}
	sev := spec.Severity
	if sev == "" {
		sev = query.Critical
	}
	var out []cond
	for _, s := range res.Series {
		hasData := false
		for _, v := range s.Values {
			if v != nil {
				hasData = true
				break
			}
		}
		if !hasData || !compare(s.Total, spec.Op, spec.Threshold) {
			continue
		}
		keys := make([]string, 0, len(s.Group))
		for k := range s.Group {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var label, scope []string
		for _, k := range keys {
			label = append(label, k+"="+s.Group[k])
			scope = append(scope, k+":"+quoteValue(s.Group[k]))
		}
		subject := strings.Join(label, ", ")
		title := m.Name
		if subject != "" {
			title += " — " + subject
		}
		unit := res.Unit
		if unit == "1" {
			unit = ""
		}
		v := s.Total
		cd := cond{
			key: subject, severity: sev, title: title,
			service: s.Group["service"], location: s.Group["location"],
			summary: fmt.Sprintf("%s is %s%s over the last %s (threshold %s %s)", describe(req), fmtNum(v), unit, spec.window(), spec.Op, fmtNum(spec.Threshold)),
			details: Details{Scope: strings.TrimSpace(req.Filter + " " + strings.Join(scope, " ")), Value: &v, Unit: unit},
		}
		if req.Signal == query.Spans || req.Signal == "" {
			cd.explainMode = "errors"
			if req.Agg != "error_rate" && req.Agg != "count" && req.Agg != "rate" {
				cd.explainMode = "slow"
			}
		}
		out = append(out, cd)
	}
	return out, nil
}

func describe(r query.Request) string {
	what := r.Signal
	if r.Signal == query.Metrics {
		what = r.Metric
	}
	if r.Filter != "" {
		what += " [" + r.Filter + "]"
	}
	return r.Agg + " of " + what
}

// Engine evaluates monitors on an interval and sends notifications.
type Engine struct {
	Store     *store.Store
	PublicURL string        // base URL used for links in notifications
	Interval  time.Duration // default 30s
	Logf      func(format string, args ...any)

	notifier
	pending map[string]int // consecutive evaluations a condition has held, before firing
	clear   map[string]int // consecutive evaluations a firing alert's condition was absent
}

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// Run evaluates until done is closed.
func (e *Engine) Run(done <-chan struct{}) {
	iv := e.Interval
	if iv <= 0 {
		iv = 30 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := e.Evaluate(); err != nil {
				e.logf("alerting: %v", err)
			}
		}
	}
}

// Evaluate runs every enabled monitor once and opens, updates or resolves alerts.
func (e *Engine) Evaluate() error {
	if e.pending == nil {
		e.pending, e.clear = map[string]int{}, map[string]int{}
	}
	monitors, err := e.Store.ListMonitors()
	if err != nil {
		return err
	}
	firingList, err := e.Store.FiringAlerts()
	if err != nil {
		return err
	}
	firing := map[string]*store.Alert{}
	for i := range firingList {
		a := &firingList[i]
		firing[a.MonitorID+"\x00"+a.Key] = a
	}

	var health *query.HealthReport
	now := time.Now().UnixMilli()
	live := map[string]bool{} // state keys whose condition holds now
	active := map[string]bool{}

	for _, m := range monitors {
		if !m.Enabled {
			continue
		}
		active[m.ID] = true
		var conds []cond
		need := 2
		switch m.Kind {
		case "health":
			var spec HealthSpec
			json.Unmarshal(m.Spec, &spec)
			need = forEvals(spec.ForEvals)
			if health == nil {
				if health, err = query.Health(e.Store.DB(), 5*time.Minute); err != nil {
					return err
				}
			}
			conds = healthConds(spec, health)
			e.attribute(conds, health)
		case "query":
			var spec QuerySpec
			json.Unmarshal(m.Spec, &spec)
			need = forEvals(spec.ForEvals)
			if conds, err = e.queryConds(m, spec); err != nil {
				e.logf("alerting: monitor %q: %v", m.Name, err)
				// Keep existing alerts as they are; a broken query is not a recovery.
				for sk := range firing {
					if strings.HasPrefix(sk, m.ID+"\x00") {
						live[sk] = true
					}
				}
				continue
			}
		}
		for _, c := range conds {
			sk := m.ID + "\x00" + c.key
			live[sk] = true
			delete(e.clear, sk)
			if a := firing[sk]; a != nil {
				escalated := c.severity == query.Critical && a.Severity != query.Critical
				a.Severity, a.Title, a.Summary, a.UpdatedMs = c.severity, c.title, c.summary, now
				if escalated {
					a.Details = e.evidence(c)
					if c.details.CausedBy == "" {
						a.NotifyError = e.notify("escalated", a)
					}
				}
				if err := e.Store.SaveAlert(a); err != nil {
					return err
				}
				continue
			}
			e.pending[sk]++
			if e.pending[sk] < need {
				continue
			}
			delete(e.pending, sk)
			a := &store.Alert{MonitorID: m.ID, MonitorName: m.Name, Key: c.key, Status: store.AlertFiring, Severity: c.severity,
				Title: c.title, Summary: c.summary, Service: c.service, Location: c.location, StartedMs: now, UpdatedMs: now,
				Details: e.evidence(c)}
			if err := e.Store.SaveAlert(a); err != nil {
				return err
			}
			if c.details.CausedBy != "" {
				// A symptom of another alert: recorded, but nobody is paged twice.
				e.logf("alert firing: %s (downstream of %s, not notified)", a.Title, c.details.CausedBy)
				continue
			}
			a.NotifyError = e.notify("firing", a)
			if a.NotifyError != "" {
				e.Store.SaveAlert(a)
			}
			e.logf("alert firing: %s", a.Title)
		}
		// Resolve this monitor's alerts whose condition has stayed clear.
		for sk, a := range firing {
			if a.MonitorID != m.ID || live[sk] {
				continue
			}
			e.clear[sk]++
			if e.clear[sk] < need {
				live[sk] = true // not resolved yet
				continue
			}
			delete(e.clear, sk)
			if err := e.resolve(a, now, true); err != nil {
				return err
			}
		}
	}
	// Alerts whose monitor was deleted or disabled end quietly.
	for _, a := range firing {
		if !active[a.MonitorID] {
			if err := e.resolve(a, now, false); err != nil {
				return err
			}
		}
	}
	for sk := range e.pending {
		if !live[sk] {
			delete(e.pending, sk)
		}
	}
	return nil
}

func (e *Engine) resolve(a *store.Alert, now int64, tell bool) error {
	a.Status, a.ResolvedMs, a.UpdatedMs = store.AlertResolved, now, now
	var d Details
	json.Unmarshal(a.Details, &d)
	if tell && d.CausedBy == "" {
		a.NotifyError = e.notify("resolved", a)
		e.logf("alert resolved: %s", a.Title)
	}
	return e.Store.SaveAlert(a)
}

// evidence runs Explain for the alert's scope and collects the likely triggers.
func (e *Engine) evidence(c cond) json.RawMessage {
	d := c.details
	if c.explainMode != "" {
		res, err := query.Explain(e.Store.DB(), e.Store, query.ExplainRequest{Scope: d.Scope, Mode: c.explainMode, Range: "10m"})
		if err == nil {
			for _, f := range res.Findings {
				if len(d.Findings) == 5 {
					break
				}
				// The scope's own dimensions are not news.
				if (f.Key == "service" && f.Value == c.service) || (f.Key == "location" && f.Value == c.location) {
					continue
				}
				d.Findings = append(d.Findings, f)
			}
			for _, ch := range res.Changes {
				if ch.Related {
					d.Triggers = append(d.Triggers, changeTitle(ch.Event))
				}
			}
		}
	} else if c.service != "" {
		// Nothing to explain (e.g. silence), but a recent change is still the best lead.
		now := time.Now()
		evs, _ := e.Store.Events(now.Add(-30*time.Minute).UnixNano(), now.UnixNano())
		for _, ev := range evs {
			if ev.Service == c.service && (ev.Location == "" || ev.Location == c.location) {
				d.Triggers = append(d.Triggers, changeTitle(ev))
			}
		}
	}
	b, _ := json.Marshal(d)
	return b
}

func changeTitle(ev store.Event) string {
	t := ev.Title
	if ev.Location != "" {
		t += " in " + ev.Location
	}
	return t + " at " + time.Unix(0, ev.TsNs).UTC().Format("15:04 UTC")
}
