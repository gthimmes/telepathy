package query

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Request is the single query shape used by the explorer, dashboards widgets
// and (later) monitors. One shape for every signal is a product decision:
// a widget, an alert and an ad-hoc question are the same query.
type Request struct {
	Signal  string   `json:"signal"`  // spans | logs | metrics
	Metric  string   `json:"metric"`  // metric name (metrics only)
	Filter  string   `json:"filter"`  // filter expression
	Agg     string   `json:"agg"`     // count|rate|error_rate|avg|sum|min|max|p50|p90|p95|p99
	Field   string   `json:"field"`   // numeric field for value aggs; default duration (spans) / value (metrics)
	GroupBy []string `json:"groupBy"` // fields to split series by
	From    int64    `json:"from"`    // unix ms (optional)
	To      int64    `json:"to"`      // unix ms (optional)
	Range   string   `json:"range"`   // e.g. "15m" when from/to omitted
	Step    int      `json:"step"`    // seconds; 0 = auto
	Limit   int      `json:"limit"`   // max series (top N by total)
}

type Series struct {
	Group  map[string]string `json:"group"`
	Values []*float64        `json:"values"` // aligned with Result.Timestamps; null = no data
	Total  float64           `json:"total"`  // the aggregate over the whole window
}

type Result struct {
	From       int64    `json:"from"` // unix ms
	To         int64    `json:"to"`
	Step       int      `json:"step"` // seconds
	Unit       string   `json:"unit"`
	Timestamps []int64  `json:"timestamps"`
	Series     []Series `json:"series"`
}

var niceSteps = []int{1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 21600, 43200, 86400}

func autoStep(d time.Duration) int {
	want := d.Seconds() / 60
	for _, s := range niceSteps {
		if float64(s) >= want {
			return s
		}
	}
	return 86400
}

var quantiles = map[string]float64{"p50": 0.5, "p75": 0.75, "p90": 0.9, "p95": 0.95, "p99": 0.99}

// Quantile of a sorted slice (nearest-rank with linear interpolation).
func Quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

type acc struct {
	count, errs   float64
	sum, min, max float64
	vals          []float64
}

func (a *acc) add(count, errs, sum, mn, mx float64) {
	if a.count == 0 {
		a.min, a.max = mn, mx
	} else {
		a.min, a.max = math.Min(a.min, mn), math.Max(a.max, mx)
	}
	a.count += count
	a.errs += errs
	a.sum += sum
}

func (a *acc) value(agg string, seconds float64, metric bool) float64 {
	if q, ok := quantiles[agg]; ok {
		sort.Float64s(a.vals)
		return Quantile(a.vals, q)
	}
	switch agg {
	case "count":
		return a.count
	case "rate":
		if metric {
			return a.sum / seconds
		}
		return a.count / seconds
	case "error_rate":
		if a.count == 0 {
			return 0
		}
		return a.errs / a.count * 100
	case "avg":
		if a.count == 0 {
			return 0
		}
		return a.sum / a.count
	case "sum":
		return a.sum
	case "min":
		return a.min
	case "max":
		return a.max
	}
	return 0
}

// Run executes a time-series query.
func Run(db *sql.DB, req Request) (*Result, error) {
	si, ok := signals[req.Signal]
	if !ok {
		return nil, fmt.Errorf("unknown signal %q", req.Signal)
	}
	agg := req.Agg
	if agg == "" {
		agg = "count"
	}
	_, isQ := quantiles[agg]
	switch agg {
	case "count", "rate", "avg", "sum", "min", "max":
	case "error_rate":
		if req.Signal != Spans {
			return nil, fmt.Errorf("error_rate is only defined for spans")
		}
	default:
		if !isQ {
			return nil, fmt.Errorf("unknown aggregation %q", agg)
		}
	}
	tr, err := ResolveRange(req.From, req.To, req.Range, time.Hour)
	if err != nil {
		return nil, err
	}
	step := req.Step
	if step <= 0 {
		step = autoStep(time.Duration(tr.ToNs - tr.FromNs))
	}
	stepNs := int64(step) * 1e9
	if (tr.ToNs-tr.FromNs)/stepNs > 2000 {
		return nil, fmt.Errorf("step too small for range")
	}

	where, args, err := Compile(req.Signal, req.Filter)
	if err != nil {
		return nil, err
	}

	// Value expression + unit.
	valExpr, unit := "1", ""
	var valArgs []any
	switch req.Signal {
	case Spans:
		if req.Field == "" || req.Field == "duration" {
			valExpr, unit = "duration_ns/1e6", "ms"
		} else {
			e, a, _, err := fieldExpr(Spans, req.Field)
			if err != nil {
				return nil, err
			}
			valExpr, valArgs = "CAST("+e+" AS REAL)", a
		}
	case Metrics:
		if req.Metric == "" {
			return nil, fmt.Errorf("metric name is required")
		}
		valExpr = "value"
		where += " AND name = ?"
		args = append(args, req.Metric)
	case Logs:
		if agg != "count" && agg != "rate" {
			return nil, fmt.Errorf("logs support count and rate")
		}
	}
	switch agg {
	case "count":
		unit = ""
	case "rate":
		unit = "/s"
	case "error_rate":
		unit = "%"
	}

	// Group expressions.
	var gExprs []string
	var gArgs []any
	for _, g := range req.GroupBy {
		e, a, _, err := fieldExpr(req.Signal, g)
		if err != nil {
			return nil, err
		}
		gExprs = append(gExprs, "COALESCE(CAST("+e+" AS TEXT),'')")
		gArgs = append(gArgs, a...)
	}
	gSel := ""
	for _, e := range gExprs {
		gSel += ", " + e
	}

	errExpr := "0"
	if req.Signal == Spans {
		errExpr = "CASE WHEN status=2 THEN 1 ELSE 0 END"
	}

	var q string
	var qArgs []any
	qArgs = append(qArgs, stepNs)
	qArgs = append(qArgs, gArgs...)
	if isQ {
		q = "SELECT ts_ns/? AS b" + gSel + ", " + valExpr + " FROM " + si.table +
			" WHERE ts_ns>=? AND ts_ns<? AND " + where + " LIMIT 2000000"
		qArgs = append(qArgs, valArgs...)
	} else {
		q = "SELECT ts_ns/? AS b" + gSel + ", COUNT(*), SUM(" + errExpr + "), SUM(" + valExpr + "), MIN(" + valExpr + "), MAX(" + valExpr + ") FROM " + si.table +
			" WHERE ts_ns>=? AND ts_ns<? AND " + where + " GROUP BY 1"
		for i := range gExprs {
			q += fmt.Sprintf(",%d", i+2)
		}
		qArgs = append(qArgs, valArgs...)
		qArgs = append(qArgs, valArgs...)
		qArgs = append(qArgs, valArgs...)
	}
	qArgs = append(qArgs, tr.FromNs, tr.ToNs)
	qArgs = append(qArgs, args...)

	rows, err := db.Query(q, qArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type gdata struct {
		group   []string
		buckets map[int64]*acc
		total   acc
	}
	groups := map[string]*gdata{}
	ng := len(gExprs)
	for rows.Next() {
		var b int64
		gv := make([]string, ng)
		dest := []any{&b}
		for i := range gv {
			dest = append(dest, &gv[i])
		}
		var cnt, errs float64
		var sum, mn, mx, v sql.NullFloat64
		if isQ {
			dest = append(dest, &v)
		} else {
			dest = append(dest, &cnt, &errs, &sum, &mn, &mx)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		key := strings.Join(gv, "\x00")
		g := groups[key]
		if g == nil {
			g = &gdata{group: gv, buckets: map[int64]*acc{}}
			groups[key] = g
		}
		a := g.buckets[b]
		if a == nil {
			a = &acc{}
			g.buckets[b] = a
		}
		if isQ {
			if v.Valid {
				a.vals = append(a.vals, v.Float64)
				g.total.vals = append(g.total.vals, v.Float64)
			}
		} else {
			a.add(cnt, errs, sum.Float64, mn.Float64, mx.Float64)
			g.total.add(cnt, errs, sum.Float64, mn.Float64, mx.Float64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	res := &Result{From: tr.FromNs / 1e6, To: tr.ToNs / 1e6, Step: step, Unit: unit, Series: []Series{}}
	b0, b1 := tr.FromNs/stepNs, (tr.ToNs-1)/stepNs
	for b := b0; b <= b1; b++ {
		res.Timestamps = append(res.Timestamps, b*stepNs/1e6)
	}
	isMetric := req.Signal == Metrics
	rangeSec := float64(tr.ToNs-tr.FromNs) / 1e9
	// count/rate are zero (not missing) when a bucket has no rows.
	zeroFill := agg == "count" || agg == "rate"
	for _, g := range groups {
		s := Series{Group: map[string]string{}, Total: g.total.value(agg, rangeSec, isMetric)}
		for i, name := range req.GroupBy {
			s.Group[name] = g.group[i]
		}
		for b := b0; b <= b1; b++ {
			if a := g.buckets[b]; a != nil {
				v := a.value(agg, float64(step), isMetric)
				s.Values = append(s.Values, &v)
			} else if zeroFill {
				z := 0.0
				s.Values = append(s.Values, &z)
			} else {
				s.Values = append(s.Values, nil)
			}
		}
		res.Series = append(res.Series, s)
	}
	sort.Slice(res.Series, func(i, j int) bool {
		if res.Series[i].Total != res.Series[j].Total {
			return res.Series[i].Total > res.Series[j].Total
		}
		return fmt.Sprint(res.Series[i].Group) < fmt.Sprint(res.Series[j].Group)
	})
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	if len(res.Series) > limit {
		res.Series = res.Series[:limit]
	}
	return res, nil
}
