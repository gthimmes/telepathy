package alert

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gthimmes/telepathy/internal/store"
)

// When "pay" breaks and takes its caller "web" down with it, both are recorded
// but only the root notifies.
func TestRootCauseGrouping(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	var spans []store.Span
	for i := 0; i < 65*60; i += 2 {
		ts := now.Add(-time.Duration(i) * time.Second).UnixNano()
		for _, loc := range []string{"eu", "us"} {
			status := store.StatusOK
			if i < 4*60 && loc == "eu" {
				status = store.StatusError
			}
			tid := fmt.Sprintf("%s-%d", loc, i)
			spans = append(spans,
				store.Span{TraceID: tid, SpanID: "a", Service: "web", Location: loc, Name: "POST /buy", Kind: "server", TsNs: ts, DurationNs: 3e8, Status: status},
				store.Span{TraceID: tid, SpanID: "b", ParentID: "a", Service: "web", Location: loc, Name: "call pay", Kind: "client", TsNs: ts, DurationNs: 2e8, Status: status},
				store.Span{TraceID: tid, SpanID: "c", ParentID: "b", Service: "pay", Location: loc, Name: "POST /charge", Kind: "server", TsNs: ts, DurationNs: 1e8, Status: status},
			)
		}
	}
	if err := st.InsertSpans(spans); err != nil {
		t.Fatal(err)
	}
	EnsureDefaults(st)
	hook := newInbox(t)
	st.CreateChannel(&store.Channel{Name: "hook", Kind: "webhook", URL: hook.srv.URL})
	e := &Engine{Store: st}
	e.Evaluate()
	e.Evaluate()

	firing, _ := st.FiringAlerts()
	if len(firing) != 2 {
		t.Fatalf("want alerts for web and pay in eu, got %+v", firing)
	}
	for _, a := range firing {
		var d Details
		json.Unmarshal(a.Details, &d)
		switch a.Service {
		case "web":
			if d.CausedBy != "pay in eu" {
				t.Errorf("web should be marked downstream of pay: %+v", d)
			}
		case "pay":
			if d.CausedBy != "" || len(d.Impacted) != 1 || d.Impacted[0] != "web" {
				t.Errorf("pay should be the root, with web impacted: %+v", d)
			}
		}
	}
	got := hook.take()
	if len(got) != 1 || got[0]["alert"].(map[string]any)["service"] != "pay" {
		t.Fatalf("exactly one notification, for the root, expected: %+v", got)
	}

	// Recovery: again only the root says so.
	st.DB().Exec(`UPDATE spans SET status=1`)
	e.Evaluate()
	e.Evaluate()
	if firing, _ := st.FiringAlerts(); len(firing) != 0 {
		t.Fatalf("alerts did not resolve: %+v", firing)
	}
	if got := hook.take(); len(got) != 1 || got[0]["event"] != "resolved" {
		t.Errorf("want one resolve notification, got %+v", got)
	}
}
