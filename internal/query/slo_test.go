package query

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/gthimmes/telepathy/internal/store"
)

// 65 minutes of 1 req/s per location; in the last 4 minutes half of the eu
// requests fail and take 3 s. Totals: 7800 requests, 120 bad, all in eu.
func sloStore(t *testing.T) *store.Store {
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
			for j := 0; j < 2; j++ {
				s := store.Span{TraceID: "t", SpanID: "s", Service: "pay", Location: loc, Kind: "server", TsNs: ts + int64(j), DurationNs: 1e8, Status: store.StatusOK}
				if i < 4*60 && loc == "eu" && j == 0 {
					s.Status, s.DurationNs = store.StatusError, 3e9
				}
				spans = append(spans, s)
			}
		}
	}
	if err := st.InsertSpans(spans); err != nil {
		t.Fatal(err)
	}
	return st
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestEvalSLO(t *testing.T) {
	st := sloStore(t)
	now := time.Now()

	avail, err := EvalSLO(st.DB(), store.SLO{Service: "pay", Kind: "availability", Target: 99.9, WindowDays: 30}, now)
	if err != nil {
		t.Fatal(err)
	}
	if avail.Total != 7800 || avail.Good != 7680 {
		t.Fatalf("good/total = %d/%d, want 7680/7800", avail.Good, avail.Total)
	}
	// Budget: 0.1% of 7800 = 7.8 allowed failures, 120 happened.
	if !near(*avail.SLI, 98.46, 0.01) || !near(*avail.BudgetLeft, (1-120/7.8)*100, 0.5) || avail.State != SLOExhausted {
		t.Errorf("sli=%.2f budgetLeft=%.1f state=%s", *avail.SLI, *avail.BudgetLeft, avail.State)
	}
	// Last hour: 120 bad of ~7200 = 1.67% against 0.1% allowed.
	if avail.Burn1h < FastBurn || avail.Burn5m < FastBurn || avail.Burn5m < avail.Burn1h {
		t.Errorf("burn 1h=%.1f 5m=%.1f, want a fast burn that is hotter over 5m", avail.Burn1h, avail.Burn5m)
	}
	if len(avail.Locations) != 2 || avail.Locations[0].Location != "eu" || avail.Locations[1].Burn1h != 0 || *avail.Locations[1].BudgetLeft != 100 {
		t.Errorf("locations should list eu first (worst) and us untouched: %+v", avail.Locations)
	}
	if n := len(avail.Budget); n == 0 || !near(avail.Budget[n-1], *avail.BudgetLeft, 0.01) || len(avail.Timestamps) != n {
		t.Errorf("budget history should end at the current budget: %v", avail.Budget)
	}

	// Latency: the 120 failures are also the only slow requests.
	lat, _ := EvalSLO(st.DB(), store.SLO{Service: "pay", Kind: "latency", ThresholdMs: 500, Target: 95, WindowDays: 7}, now)
	if lat.Good != 7680 || lat.State != SLOOk || !near(*lat.BudgetLeft, (1-120.0/390)*100, 0.1) {
		t.Errorf("latency: good=%d state=%s budgetLeft=%.1f", lat.Good, lat.State, *lat.BudgetLeft)
	}

	// Scoped to the healthy location, nothing is spent.
	us, _ := EvalSLO(st.DB(), store.SLO{Service: "pay", Location: "us", Kind: "availability", Target: 99.9, WindowDays: 30}, now)
	if us.Total != 3900 || *us.BudgetLeft != 100 || us.State != SLOOk {
		t.Errorf("us only: total=%d budgetLeft=%.1f state=%s", us.Total, *us.BudgetLeft, us.State)
	}

	none, _ := EvalSLO(st.DB(), store.SLO{Service: "nope", Kind: "availability", Target: 99, WindowDays: 7}, now)
	if none.State != SLONoData || none.SLI != nil {
		t.Errorf("unknown service: %+v", none)
	}
}

func TestValidateSLO(t *testing.T) {
	ok := store.SLO{Name: "x", Service: "pay", Kind: "latency", ThresholdMs: 300, Target: 99, WindowDays: 30}
	if err := ValidateSLO(&ok); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]store.SLO{
		"no name":      {Service: "pay", Kind: "availability", Target: 99, WindowDays: 30},
		"target 100":   {Name: "x", Service: "pay", Kind: "availability", Target: 100, WindowDays: 30},
		"no threshold": {Name: "x", Service: "pay", Kind: "latency", Target: 99, WindowDays: 30},
		"window":       {Name: "x", Service: "pay", Kind: "availability", Target: 99, WindowDays: 365},
		"glob":         {Name: "x", Service: "pay", Location: "[", Kind: "availability", Target: 99, WindowDays: 30},
	} {
		if err := ValidateSLO(&bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
