package alert

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gthimmes/telepathy/internal/query"
	"github.com/gthimmes/telepathy/internal/store"
)

func addSLO(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.PutSLO(&store.SLO{Name: "Pay availability", Service: "pay", Kind: "availability", Target: 99.9, WindowDays: 30}); err != nil {
		t.Fatal(err)
	}
}

func TestSLOBurnAlert(t *testing.T) {
	st := seed(t)
	addSLO(t, st)
	// Only the SLO monitor: the burn alert must stand on its own.
	st.PutMonitor(&store.Monitor{ID: DefaultSLOMonitor, Name: "SLO burn rate", Kind: "slo", Spec: json.RawMessage("{}"), Enabled: true})
	hook := newInbox(t)
	st.CreateChannel(&store.Channel{Name: "hook", Kind: "webhook", URL: hook.srv.URL})
	e := &Engine{Store: st}
	e.Evaluate()
	if f, _ := st.FiringAlerts(); len(f) != 0 {
		t.Fatalf("fired after one evaluation: %+v", f)
	}
	e.Evaluate()
	firing, _ := st.FiringAlerts()
	if len(firing) != 1 {
		t.Fatalf("want one SLO alert, got %d", len(firing))
	}
	a := firing[0]
	if a.Severity != query.Critical || !strings.Contains(a.Title, "Pay availability") || !strings.Contains(a.Summary, "worst location eu") {
		t.Errorf("unexpected alert: %s / %s / %s", a.Severity, a.Title, a.Summary)
	}
	var d Details
	json.Unmarshal(a.Details, &d)
	found := false
	for _, f := range d.Findings {
		if f.Key == "payment.provider" && f.Value == "adyen" {
			found = true
		}
	}
	if !found || len(d.Triggers) == 0 {
		t.Errorf("SLO alert should carry Explain evidence and the deploy: %+v", d)
	}
	if got := hook.take(); len(got) != 1 {
		t.Fatalf("want one notification, got %d", len(got))
	}

	// The failures stop: the alert resolves.
	st.DB().Exec("DELETE FROM spans WHERE status=2")
	st.RebuildRollups()
	e.Evaluate()
	e.Evaluate()
	if f, _ := st.FiringAlerts(); len(f) != 0 {
		t.Errorf("SLO alert did not resolve: %+v", f)
	}
}

func TestSLOAlertDefersToHealthAlert(t *testing.T) {
	st := seed(t)
	addSLO(t, st)
	EnsureDefaults(st)
	hook := newInbox(t)
	st.CreateChannel(&store.Channel{Name: "hook", Kind: "webhook", URL: hook.srv.URL})
	e := &Engine{Store: st}
	e.Evaluate()
	e.Evaluate()
	firing, _ := st.FiringAlerts()
	if len(firing) != 2 {
		t.Fatalf("want a health alert and an SLO alert, got %d", len(firing))
	}
	for _, a := range firing {
		var d Details
		json.Unmarshal(a.Details, &d)
		if a.MonitorID == DefaultSLOMonitor && d.CoveredBy == "" {
			t.Errorf("SLO alert should defer to the health alert: %+v", d)
		}
	}
	got := hook.take()
	if len(got) != 1 || got[0]["alert"].(map[string]any)["monitorId"] != DefaultHealthMonitor {
		t.Fatalf("one incident should notify once, from the health monitor: %d notifications", len(got))
	}
}

func TestRoutingAndSilences(t *testing.T) {
	st := seed(t)
	EnsureDefaults(st)
	other, eu, crit := newInbox(t), newInbox(t), newInbox(t)
	st.CreateChannel(&store.Channel{Name: "search team", Kind: "webhook", URL: other.srv.URL, Services: "search*"})
	st.CreateChannel(&store.Channel{Name: "eu on-call", Kind: "webhook", URL: eu.srv.URL, Locations: "eu*"})
	st.CreateChannel(&store.Channel{Name: "pager", Kind: "webhook", URL: crit.srv.URL, Severity: "critical"})
	e := &Engine{Store: st}
	e.Evaluate()
	e.Evaluate()
	if n := len(other.take()); n != 0 {
		t.Errorf("channel routed to search* received %d notifications for pay", n)
	}
	if n := len(eu.take()); n != 1 {
		t.Errorf("channel routed to eu* received %d notifications, want 1", n)
	}
	if n := len(crit.take()); n != 1 {
		t.Errorf("critical-only channel received %d notifications for a critical alert, want 1", n)
	}

	// A silence stops the resolve notification too, and says why on the alert.
	now := time.Now().UnixMilli()
	st.CreateSilence(&store.Silence{Service: "pay", Reason: "planned failover", StartsMs: now - 1000, EndsMs: now + 3600_000})
	st.DB().Exec("DELETE FROM spans WHERE status=2")
	st.RebuildRollups()
	e.Evaluate()
	e.Evaluate()
	if f, _ := st.FiringAlerts(); len(f) != 0 {
		t.Fatalf("alert did not resolve: %+v", f)
	}
	if n := len(eu.take()) + len(crit.take()); n != 0 {
		t.Errorf("silenced alert sent %d notifications", n)
	}
	recent, _ := st.RecentAlerts(5)
	var d Details
	json.Unmarshal(recent[0].Details, &d)
	if d.Silenced != "planned failover" {
		t.Errorf("alert should record the silence: %+v", d)
	}
}
