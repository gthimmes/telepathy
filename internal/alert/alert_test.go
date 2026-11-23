package alert

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gthimmes/telepathy/internal/query"
	"github.com/gthimmes/telepathy/internal/store"
)

// seed writes 65 minutes of traffic for "pay" in "eu" and "us"; in the last
// four minutes eu fails, only for provider=adyen, after a deploy.
func seed(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	var spans []store.Span
	for i := 0; i < 65*60; i += 2 {
		ts := now.Add(-time.Duration(i) * time.Second).UnixNano()
		for _, loc := range []string{"eu", "us"} {
			for j, prov := range []string{"adyen", "stripe"} {
				s := store.Span{TraceID: "t", SpanID: "s", Service: "pay", Location: loc, Name: "POST /charge", Kind: "server",
					TsNs: ts + int64(j), DurationNs: int64(100 * time.Millisecond), Status: store.StatusOK,
					Attrs: map[string]any{"payment.provider": prov}}
				if i < 4*60 && loc == "eu" && prov == "adyen" {
					s.Status, s.DurationNs = store.StatusError, int64(3*time.Second)
				}
				spans = append(spans, s)
			}
		}
	}
	if err := st.InsertSpans(spans); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(&store.Event{TsNs: now.Add(-5 * time.Minute).UnixNano(), Kind: "deploy", Service: "pay", Location: "eu", Title: "Deploy pay 2.0"}); err != nil {
		t.Fatal(err)
	}
	return st
}

type inbox struct {
	mu   sync.Mutex
	got  []map[string]any
	srv  *httptest.Server
	fail bool
}

func newInbox(t *testing.T) *inbox {
	in := &inbox{}
	in.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in.mu.Lock()
		defer in.mu.Unlock()
		if in.fail {
			w.WriteHeader(500)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		in.got = append(in.got, m)
	}))
	t.Cleanup(in.srv.Close)
	return in
}

func (in *inbox) take() []map[string]any {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := in.got
	in.got = nil
	return out
}

func TestHealthMonitorLifecycle(t *testing.T) {
	st := seed(t)
	if err := EnsureDefaults(st); err != nil {
		t.Fatal(err)
	}
	hook, slack, pd := newInbox(t), newInbox(t), newInbox(t)
	st.CreateChannel(&store.Channel{Name: "hook", Kind: "webhook", URL: hook.srv.URL})
	st.CreateChannel(&store.Channel{Name: "slack", Kind: "slack", URL: slack.srv.URL})
	st.CreateChannel(&store.Channel{Name: "pd", Kind: "pagerduty", URL: "0123456789abcdef0123456789abcdef"})
	e := &Engine{Store: st, PublicURL: "https://telepathy.example"}
	e.PagerDutyURL = pd.srv.URL

	// One evaluation is not enough: a condition must persist before anyone is paged.
	if err := e.Evaluate(); err != nil {
		t.Fatal(err)
	}
	if firing, _ := st.FiringAlerts(); len(firing) != 0 || len(hook.take()) != 0 {
		t.Fatalf("alert fired on the first evaluation: %+v", firing)
	}
	if err := e.Evaluate(); err != nil {
		t.Fatal(err)
	}
	firing, _ := st.FiringAlerts()
	if len(firing) != 1 {
		t.Fatalf("want 1 firing alert, got %+v", firing)
	}
	a := firing[0]
	if a.Service != "pay" || a.Location != "eu" || a.Severity != query.Critical || !strings.Contains(a.Summary, "error rate") {
		t.Errorf("alert: %+v", a)
	}
	var d Details
	json.Unmarshal(a.Details, &d)
	if len(d.Triggers) != 1 || !strings.Contains(d.Triggers[0], "Deploy pay 2.0") {
		t.Errorf("likely trigger missing: %+v", d.Triggers)
	}
	if len(d.Findings) == 0 || d.Findings[0].Key != "payment.provider" || d.Findings[0].Value != "adyen" {
		t.Errorf("explain findings should lead with payment.provider=adyen: %+v", d.Findings)
	}

	// Each channel got exactly one notification, in its own format.
	if got := hook.take(); len(got) != 1 || got[0]["event"] != "firing" ||
		!strings.HasPrefix(got[0]["url"].(string), "https://telepathy.example/#/explore?") {
		t.Errorf("webhook: %+v", got)
	}
	if got := slack.take(); len(got) != 1 || !strings.Contains(got[0]["text"].(string), "pay in eu is critical") ||
		!strings.Contains(got[0]["text"].(string), "Likely trigger") || !strings.Contains(got[0]["text"].(string), "payment.provider=adyen") {
		t.Errorf("slack: %+v", got)
	}
	pdGot := pd.take()
	if len(pdGot) != 1 || pdGot[0]["event_action"] != "trigger" || pdGot[0]["routing_key"] != "0123456789abcdef0123456789abcdef" {
		t.Errorf("pagerduty: %+v", pdGot)
	}

	// Still failing: no duplicate alert, no repeat notification.
	if err := e.Evaluate(); err != nil {
		t.Fatal(err)
	}
	if firing, _ := st.FiringAlerts(); len(firing) != 1 || len(hook.take()) != 0 {
		t.Errorf("re-evaluation duplicated the alert or re-notified")
	}

	// Recovery (the failing spans are gone) resolves after it has held twice.
	if _, err := st.DB().Exec(`DELETE FROM spans WHERE status=2`); err != nil {
		t.Fatal(err)
	}
	e.Evaluate()
	if firing, _ := st.FiringAlerts(); len(firing) != 1 {
		t.Fatal("alert resolved on the first clear evaluation")
	}
	e.Evaluate()
	if firing, _ := st.FiringAlerts(); len(firing) != 0 {
		t.Fatalf("alert did not resolve: %+v", firing)
	}
	recent, _ := st.RecentAlerts(10)
	if len(recent) != 1 || recent[0].ResolvedMs == 0 {
		t.Errorf("resolved alert not recorded: %+v", recent)
	}
	if got := hook.take(); len(got) != 1 || got[0]["event"] != "resolved" {
		t.Errorf("resolve webhook: %+v", got)
	}
	if got := pd.take(); len(got) != 1 || got[0]["event_action"] != "resolve" || got[0]["dedup_key"] != pdGot[0]["dedup_key"] {
		t.Errorf("pagerduty resolve must reuse the dedup key: %+v", got)
	}
}

func TestQueryMonitor(t *testing.T) {
	st := seed(t)
	hook := newInbox(t)
	hook.fail = true // a broken channel must not stop the alert from being recorded
	st.CreateChannel(&store.Channel{Name: "hook", Kind: "webhook", URL: hook.srv.URL})

	spec, _ := json.Marshal(QuerySpec{
		Query: query.Request{Signal: query.Spans, Agg: "p95", Filter: "service:pay", GroupBy: []string{"location"}},
		Op:    ">", Threshold: 1000, Window: "3m", ForEvals: 1,
	})
	m := &store.Monitor{Name: "Pay p95 latency", Kind: "query", Spec: spec, Enabled: true}
	if err := ValidateMonitor(st, m); err != nil {
		t.Fatal(err)
	}
	if err := st.PutMonitor(m); err != nil {
		t.Fatal(err)
	}
	bad := &store.Monitor{Name: "x", Kind: "query", Spec: json.RawMessage(`{"query":{"signal":"spans","agg":"nope"},"op":">"}`)}
	if err := ValidateMonitor(st, bad); err == nil {
		t.Error("expected an invalid query to be rejected")
	}

	e := &Engine{Store: st}
	if err := e.Evaluate(); err != nil {
		t.Fatal(err)
	}
	firing, _ := st.FiringAlerts()
	if len(firing) != 1 || firing[0].Location != "eu" || firing[0].Key != "location=eu" {
		t.Fatalf("want one alert for location=eu, got %+v", firing)
	}
	if !strings.Contains(firing[0].Summary, "3000ms") || firing[0].NotifyError == "" {
		t.Errorf("summary %q, notifyError %q", firing[0].Summary, firing[0].NotifyError)
	}
	if strings.Contains(firing[0].NotifyError, hook.srv.URL) {
		t.Errorf("notification error leaks the channel URL: %q", firing[0].NotifyError)
	}

	// Disabling the monitor ends its alerts.
	m.Enabled = false
	st.PutMonitor(m)
	e.Evaluate()
	if firing, _ := st.FiringAlerts(); len(firing) != 0 {
		t.Errorf("alert outlived its disabled monitor: %+v", firing)
	}
}
