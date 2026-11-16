package query

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/gthimmes/telepathy/internal/store"
)

// ExplainRequest asks: "what is different about the spans I care about?"
//
// Scope narrows the population (e.g. "service:payments"). Within the scope,
// the selection is either errors, the slowest 5%, or a custom filter; the
// rest of the scope is the baseline. Every dimension of every span is then
// compared between the two sets.
type ExplainRequest struct {
	Scope     string `json:"scope"`
	Mode      string `json:"mode"`      // errors (default) | slow | custom
	Selection string `json:"selection"` // filter, for mode=custom
	From      int64  `json:"from"`
	To        int64  `json:"to"`
	Range     string `json:"range"`
}

type Finding struct {
	Key       string  `json:"key"`
	Value     string  `json:"value"`
	SelPct    float64 `json:"selPct"`  // % of selected spans with key=value
	BasePct   float64 `json:"basePct"` // % of baseline spans with key=value
	SelCount  int     `json:"selCount"`
	BaseCount int     `json:"baseCount"`
	Score     float64 `json:"score"` // selPct - basePct, in points
}

type ChangeRef struct {
	store.Event
	Related bool `json:"related"` // matches a dimension implicated by the findings
}

type ExplainResult struct {
	Selection      string      `json:"selection"` // the effective selection filter
	SelectionCount int         `json:"selectionCount"`
	BaselineCount  int         `json:"baselineCount"`
	Findings       []Finding   `json:"findings"`
	Changes        []ChangeRef `json:"changes"`
}

const explainSample = 20000

// Attributes that are unique per span carry no explanatory power.
var explainIgnore = map[string]bool{"trace_id": true, "span_id": true, "parent_id": true}

type kv struct{ k, v string }

func sampleDims(db *sql.DB, where string, args []any) (int, map[kv]int, error) {
	rows, err := db.Query(`SELECT service, location, env, name, kind, attrs FROM spans WHERE `+where+
		` ORDER BY ts_ns DESC LIMIT ?`, append(args, explainSample)...)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	counts := map[kv]int{}
	n := 0
	for rows.Next() {
		var svc, loc, env, name, kind, attrs string
		if err := rows.Scan(&svc, &loc, &env, &name, &kind, &attrs); err != nil {
			return 0, nil, err
		}
		n++
		counts[kv{"service", svc}]++
		counts[kv{"location", loc}]++
		if env != "" {
			counts[kv{"env", env}]++
		}
		counts[kv{"name", name}]++
		counts[kv{"kind", kind}]++
		var m map[string]any
		if json.Unmarshal([]byte(attrs), &m) == nil {
			for k, v := range m {
				switch t := v.(type) {
				case string:
					counts[kv{k, t}]++
				case float64, bool:
					counts[kv{k, fmt.Sprint(t)}]++
				}
			}
		}
	}
	return n, counts, rows.Err()
}

// Explain runs the differential analysis.
func Explain(db *sql.DB, st *store.Store, req ExplainRequest) (*ExplainResult, error) {
	tr, err := ResolveRange(req.From, req.To, req.Range, 15*time.Minute)
	if err != nil {
		return nil, err
	}
	scopeW, scopeA, err := Compile(Spans, req.Scope)
	if err != nil {
		return nil, err
	}
	timeW := "ts_ns>=? AND ts_ns<? AND "
	timeA := []any{tr.FromNs, tr.ToNs}

	sel := req.Selection
	switch req.Mode {
	case "", "errors":
		sel = "status:error"
	case "slow":
		// Threshold = p95 duration of the scope.
		rows, err := db.Query(`SELECT duration_ns FROM spans WHERE `+timeW+scopeW+
			` ORDER BY ts_ns DESC LIMIT ?`, append(append(append([]any{}, timeA...), scopeA...), explainSample*2)...)
		if err != nil {
			return nil, err
		}
		var durs []float64
		for rows.Next() {
			var d float64
			rows.Scan(&d)
			durs = append(durs, d/1e6)
		}
		rows.Close()
		sort.Float64s(durs)
		sel = fmt.Sprintf("duration>%.3f", Quantile(durs, 0.95))
	case "custom":
		if sel == "" {
			return nil, fmt.Errorf("selection filter is required for mode=custom")
		}
	default:
		return nil, fmt.Errorf("unknown mode %q", req.Mode)
	}
	selW, selA, err := Compile(Spans, sel)
	if err != nil {
		return nil, err
	}

	base := append(append([]any{}, timeA...), scopeA...)
	selN, selC, err := sampleDims(db, timeW+scopeW+" AND ("+selW+")", append(append([]any{}, base...), selA...))
	if err != nil {
		return nil, err
	}
	baseN, baseC, err := sampleDims(db, timeW+scopeW+" AND NOT ("+selW+")", append(append([]any{}, base...), selA...))
	if err != nil {
		return nil, err
	}

	res := &ExplainResult{Selection: sel, SelectionCount: selN, BaselineCount: baseN,
		Findings: []Finding{}, Changes: []ChangeRef{}}
	if selN > 0 {
		// Cardinality per key in the selection: drop keys that are nearly unique.
		card := map[string]int{}
		for k := range selC {
			card[k.k]++
		}
		var all []Finding
		for k, n := range selC {
			if explainIgnore[k.k] || (card[k.k] > 50 && card[k.k]*2 > selN) {
				continue
			}
			f := Finding{Key: k.k, Value: k.v, SelCount: n, BaseCount: baseC[k],
				SelPct: float64(n) / float64(selN) * 100}
			if baseN > 0 {
				f.BasePct = float64(baseC[k]) / float64(baseN) * 100
			}
			f.Score = f.SelPct - f.BasePct
			if f.Score >= 5 {
				all = append(all, f)
			}
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].Score != all[j].Score {
				return all[i].Score > all[j].Score
			}
			return all[i].Key+all[i].Value < all[j].Key+all[j].Value
		})
		perKey := map[string]int{}
		for _, f := range all {
			if perKey[f.Key] >= 2 || len(res.Findings) >= 12 {
				continue
			}
			perKey[f.Key]++
			res.Findings = append(res.Findings, f)
		}
	}

	// Change events shortly before and during the window; flag the ones that
	// touch a service/location the findings point at.
	evs, err := st.Events(tr.FromNs-int64(30*time.Minute), tr.ToNs)
	if err != nil {
		return nil, err
	}
	// A service/location is implicated when it dominates the selection, whether
	// or not it is also over-represented (a scoped query is 100% one service).
	implicated := map[kv]bool{}
	for k, n := range selC {
		if (k.k == "service" || k.k == "location") && float64(n) >= 0.4*float64(selN) {
			implicated[k] = true
		}
	}
	for _, e := range evs {
		rel := implicated[kv{"service", e.Service}] && (e.Location == "" || implicated[kv{"location", e.Location}])
		res.Changes = append(res.Changes, ChangeRef{Event: e, Related: rel})
	}
	sort.SliceStable(res.Changes, func(i, j int) bool { return res.Changes[i].Related && !res.Changes[j].Related })
	return res, nil
}
