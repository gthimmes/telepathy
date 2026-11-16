package query

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gthimmes/telepathy/internal/store"
)

func TestParseFilter(t *testing.T) {
	terms, err := ParseFilter(`service:checkout -location:eu-* duration>=500ms http.route:"/a b" "card declined" status!=ok`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Term{
		{"service", ":", "checkout", false},
		{"location", ":", "eu-*", true},
		{"duration", ">=", "500ms", false},
		{"http.route", ":", "/a b", false},
		{"", ":", "card declined", false},
		{"status", "!=", "ok", false},
	}
	if len(terms) != len(want) {
		t.Fatalf("got %d terms: %+v", len(terms), terms)
	}
	for i := range want {
		if terms[i] != want[i] {
			t.Errorf("term %d: got %+v want %+v", i, terms[i], want[i])
		}
	}
	if _, err := ParseFilter(`bad"key:1`); err == nil {
		t.Error("expected error for invalid field name")
	}
}

func TestQuantile(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5}
	if q := Quantile(v, 0.5); q != 3 {
		t.Errorf("p50 = %v", q)
	}
	if q := Quantile(v, 1); q != 5 {
		t.Errorf("p100 = %v", q)
	}
	if q := Quantile(nil, 0.9); q != 0 {
		t.Errorf("empty = %v", q)
	}
}

func seed(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	var spans []store.Span
	// 13 slices of 5 minutes; "pay" in "eu" goes bad in the last one, only for provider=adyen.
	for i := 0; i < 65*60; i += 2 {
		ts := now.Add(-time.Duration(i) * time.Second).UnixNano()
		recent := i < 4*60
		for _, loc := range []string{"eu", "us"} {
			for j, prov := range []string{"adyen", "stripe"} {
				s := store.Span{TraceID: "t", SpanID: "s", Service: "pay", Location: loc, Name: "POST /charge", Kind: "server",
					TsNs: ts + int64(j), DurationNs: int64(100 * time.Millisecond), Status: store.StatusOK,
					Attrs: map[string]any{"payment.provider": prov, "http.response.status_code": 200}}
				if recent && loc == "eu" && prov == "adyen" {
					s.Status, s.DurationNs = store.StatusError, int64(3*time.Second)
					s.Attrs["http.response.status_code"] = 504
				}
				spans = append(spans, s)
			}
		}
	}
	if err := st.InsertSpans(spans); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(&store.Event{TsNs: now.Add(-5 * time.Minute).UnixNano(), Kind: "deploy", Service: "pay", Location: "eu", Title: "deploy"}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRunHealthExplain(t *testing.T) {
	st := seed(t)

	res, err := Run(st.DB(), Request{Signal: Spans, Agg: "error_rate", GroupBy: []string{"location"}, Range: "3m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 2 || res.Series[0].Group["location"] != "eu" || res.Series[0].Total != 50 || res.Series[1].Total != 0 {
		t.Fatalf("error_rate by location: %+v", res.Series)
	}
	res, err = Run(st.DB(), Request{Signal: Spans, Agg: "p95", Filter: "http.response.status_code:504 duration>1s", Range: "3m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 1 || res.Series[0].Total != 3000 {
		t.Fatalf("p95 of failing spans: %+v", res.Series)
	}
	if _, err := Run(st.DB(), Request{Signal: Spans, Agg: "nope"}); err == nil {
		t.Error("expected error for unknown aggregation")
	}

	rep, err := Health(st.DB(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range rep.Cells {
		got[c.Location] = c.Status
	}
	if got["eu"] != Critical || got["us"] != Healthy {
		t.Fatalf("health: %+v", rep.Cells)
	}

	ex, err := Explain(st.DB(), st, ExplainRequest{Scope: "service:pay", Range: "3m"})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range ex.Findings {
		found[f.Key+"="+f.Value] = true
	}
	for _, w := range []string{"location=eu", "payment.provider=adyen", "http.response.status_code=504"} {
		if !found[w] {
			t.Errorf("explain missing %s; findings: %+v", w, ex.Findings)
		}
	}
	if len(ex.Changes) != 1 || !ex.Changes[0].Related {
		t.Errorf("expected the deploy to be flagged as related: %+v", ex.Changes)
	}
}
