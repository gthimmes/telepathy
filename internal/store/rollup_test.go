package store

import (
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestHistAccuracyAndEncoding(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h := Hist{}
	var vals []float64
	for i := 0; i < 20000; i++ {
		v := math.Exp(rng.NormFloat64()*1.2 + 4) // log-normal around 55 ms
		vals = append(vals, v)
		h.Add(v)
	}
	sort.Float64s(vals)
	for _, q := range []float64{0.5, 0.95, 0.99} {
		want := vals[int(q*float64(len(vals)-1))]
		got := h.Quantile(q)
		if math.Abs(got-want)/want > 0.03 {
			t.Errorf("q%.2f: got %.2f, want %.2f (within 3%%)", q, got, want)
		}
	}
	le := h.CountLE(100)
	exact := int64(sort.SearchFloat64s(vals, 100))
	if math.Abs(float64(le-exact))/float64(exact) > 0.03 {
		t.Errorf("CountLE(100) = %d, exact %d", le, exact)
	}
	if got := decodeHist(h.encode()); !reflect.DeepEqual(got, h) {
		t.Error("histogram did not survive encoding")
	}
	if n := len(decodeHist(nil)); n != 0 {
		t.Errorf("empty blob decoded to %d buckets", n)
	}
}

func TestRollupsFollowSpans(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := time.Now().Add(-10 * time.Minute).Truncate(time.Minute)
	var a, b []Span
	for i := 0; i < 120; i++ {
		ts := base.Add(time.Duration(i) * time.Second).UnixNano()
		status := StatusOK
		if i%10 == 0 {
			status = StatusError
		}
		s := []Span{
			{TraceID: "t", SpanID: "a", Service: "web", Location: "eu", Kind: "server", TsNs: ts, DurationNs: 5e7, Status: status},
			{TraceID: "t", SpanID: "b", ParentID: "a", Service: "web", Location: "eu", Kind: "client", TsNs: ts, DurationNs: 1e7}, // not a request
		}
		if i < 60 {
			a = append(a, s...)
		} else {
			b = append(b, s...)
		}
	}
	// Two batches that touch the same hour bucket must merge, not overwrite.
	if err := st.InsertSpans(a); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertSpans(b); err != nil {
		t.Fatal(err)
	}
	check := func(label string) {
		t.Helper()
		for _, step := range []int64{StepMinute, StepHour} {
			rs, err := ReadRollups(st.DB(), step, 0, time.Now().Unix()+1, "web", "eu")
			if err != nil {
				t.Fatal(err)
			}
			var n, errs, hn int64
			for _, r := range rs {
				n += r.N
				errs += r.Errs
				hn += r.Hist.Count()
			}
			if n != 120 || errs != 12 || hn != 120 {
				t.Errorf("%s, step %d: n=%d errs=%d hist=%d, want 120/12/120", label, step, n, errs, hn)
			}
			if step == StepMinute && len(rs) != 2 {
				t.Errorf("%s: want 2 minute buckets, got %d", label, len(rs))
			}
		}
	}
	check("incremental")
	if err := st.RebuildRollups(); err != nil {
		t.Fatal(err)
	}
	check("rebuilt")

	if err := st.PruneRollups(time.Now().Unix()+60, 0); err != nil {
		t.Fatal(err)
	}
	if rs, _ := ReadRollups(st.DB(), StepMinute, 0, time.Now().Unix()+1, "", ""); len(rs) != 0 {
		t.Errorf("minute rollups survived pruning: %d", len(rs))
	}
	if rs, _ := ReadRollups(st.DB(), StepHour, 0, time.Now().Unix()+1, "", ""); len(rs) == 0 {
		t.Error("hour rollups should outlive minute rollups")
	}
}

func TestRoutingAndSilenceMatching(t *testing.T) {
	all := Channel{}
	if !all.Routes("pay", "eu", "degraded") || !all.Routes("", "", "critical") {
		t.Error("a channel without routing receives everything")
	}
	c := Channel{Services: "pay*, checkout", Locations: "eu-*", Severity: "critical"}
	for _, tc := range []struct {
		svc, loc, sev string
		want          bool
	}{
		{"payments", "eu-central", "critical", true},
		{"checkout", "eu-west", "critical", true},
		{"payments", "eu-central", "degraded", false},
		{"search", "eu-central", "critical", false},
		{"payments", "us-east", "critical", false},
		{"", "", "critical", false},
	} {
		if got := c.Routes(tc.svc, tc.loc, tc.sev); got != tc.want {
			t.Errorf("Routes(%q,%q,%q) = %v", tc.svc, tc.loc, tc.sev, got)
		}
	}
	s := Silence{Service: "pay*", StartsMs: 100, EndsMs: 200}
	if !s.Covers("payments", "anywhere", 150) || s.Covers("payments", "x", 200) || s.Covers("search", "x", 150) || s.Covers("payments", "x", 50) {
		t.Error("silence window or matcher is wrong")
	}
	if ValidGlobs("a,[") || !ValidGlobs("") || !ValidGlobs("eu-*, us-?") {
		t.Error("ValidGlobs")
	}
}
