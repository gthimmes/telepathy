package query

import (
	"database/sql"
	"sort"
	"time"
)

// MapNode is one application on the service map.
type MapNode struct {
	Service   string  `json:"service"`
	Status    string  `json:"status"` // worst health across the locations in view
	Rate      float64 `json:"rate"`   // inbound req/s
	ErrorRate float64 `json:"errorRate"`
	P95       float64 `json:"p95"` // ms
}

// MapEdge is observed traffic from one application to another.
type MapEdge struct {
	From      string  `json:"from"`
	To        string  `json:"to"`
	Calls     int     `json:"calls"`
	Rate      float64 `json:"rate"`
	ErrorRate float64 `json:"errorRate"`
	P95       float64 `json:"p95"` // ms, as seen by the callee
}

type ServiceMapResult struct {
	Nodes   []MapNode `json:"nodes"`
	Edges   []MapEdge `json:"edges"`
	Sampled bool      `json:"sampled"` // true when edges cover only the most recent calls in the window
}

const mapEdgeLimit = 100000

type stat struct {
	n, errs int
	durs    []float64
}

func (s *stat) add(durNs int64, status int) {
	s.n++
	if status == 2 {
		s.errs++
	}
	s.durs = append(s.durs, float64(durNs)/1e6)
}

func (s *stat) summarize(secs float64) (rate, errRate, p95 float64) {
	if s.n == 0 {
		return 0, 0, 0
	}
	sort.Float64s(s.durs)
	return float64(s.n) / secs, float64(s.errs) / float64(s.n) * 100, Quantile(s.durs, 0.95)
}

// ServiceMap derives the dependency graph from traces — nothing is declared.
// An edge exists wherever a span's parent belongs to a different service.
// location, when set, restricts the view to calls handled in that location.
func ServiceMap(db *sql.DB, tr TimeRange, location string) (*ServiceMapResult, error) {
	secs := float64(tr.ToNs-tr.FromNs) / 1e9
	if secs <= 0 {
		secs = 1
	}
	res := &ServiceMapResult{Nodes: []MapNode{}}
	var err error
	if res.Edges, res.Sampled, err = Edges(db, tr, location); err != nil {
		return nil, err
	}

	// Node stats: requests each service handled (entry spans), like Health.
	loc, args := "", []any{tr.FromNs, tr.ToNs}
	if location != "" {
		loc = " AND location=?"
		args = append(args, location)
	}
	rows, err := db.Query(`SELECT service, duration_ns, status FROM spans
		WHERE ts_ns>=? AND ts_ns<? AND (kind IN ('server','consumer') OR parent_id='')`+loc, args...)
	if err != nil {
		return nil, err
	}
	nodes := map[string]*stat{}
	for rows.Next() {
		var svc string
		var dur int64
		var status int
		if err := rows.Scan(&svc, &dur, &status); err != nil {
			rows.Close()
			return nil, err
		}
		n := nodes[svc]
		if n == nil {
			n = &stat{}
			nodes[svc] = n
		}
		n.add(dur, status)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	health, err := Health(db, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	worst := map[string]string{}
	for _, c := range health.Cells {
		if location != "" && c.Location != location {
			continue
		}
		if cur, ok := worst[c.Service]; !ok || sevRank[c.Status] > sevRank[cur] {
			worst[c.Service] = c.Status
		}
	}
	for _, e := range res.Edges {
		// Make sure both ends exist as nodes even if they handled no entry spans.
		for _, s := range []string{e.From, e.To} {
			if nodes[s] == nil {
				nodes[s] = &stat{}
			}
		}
	}
	for svc, n := range nodes {
		mn := MapNode{Service: svc, Status: worst[svc]}
		if mn.Status == "" {
			mn.Status = Healthy
		}
		mn.Rate, mn.ErrorRate, mn.P95 = n.summarize(secs)
		res.Nodes = append(res.Nodes, mn)
	}
	sort.Slice(res.Nodes, func(i, j int) bool { return res.Nodes[i].Service < res.Nodes[j].Service })
	return res, nil
}

// Edges returns the service-to-service calls observed in a window: one edge
// wherever a server or consumer span's parent belongs to a different service.
// At most mapEdgeLimit calls are read, newest first; when that limit is hit,
// sampled is true and rates are computed over the time those calls span.
func Edges(db *sql.DB, tr TimeRange, location string) (out []MapEdge, sampled bool, err error) {
	secs := float64(tr.ToNs-tr.FromNs) / 1e9
	if secs <= 0 {
		secs = 1
	}
	loc, args := "", []any{tr.FromNs, tr.ToNs}
	if location != "" {
		loc = " AND c.location=?"
		args = append(args, location)
	}
	args = append(args, mapEdgeLimit)
	rows, err := db.Query(`SELECT p.service, c.service, c.duration_ns, c.status, c.ts_ns
		FROM spans c JOIN spans p ON p.trace_id=c.trace_id AND p.span_id=c.parent_id
		WHERE c.ts_ns>=? AND c.ts_ns<? AND c.kind IN ('server','consumer') AND c.parent_id<>'' AND c.service<>p.service`+loc+` ORDER BY c.ts_ns DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	type pair struct{ from, to string }
	edges := map[pair]*stat{}
	scanned, oldest := 0, tr.ToNs
	for rows.Next() {
		var k pair
		var dur, ts int64
		var status int
		if err := rows.Scan(&k.from, &k.to, &dur, &status, &ts); err != nil {
			rows.Close()
			return nil, false, err
		}
		scanned++
		oldest = ts
		e := edges[k]
		if e == nil {
			e = &stat{}
			edges[k] = e
		}
		e.add(dur, status)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	sampled = scanned >= mapEdgeLimit
	if sampled && tr.ToNs > oldest {
		secs = float64(tr.ToNs-oldest) / 1e9
	}
	out = []MapEdge{}
	for k, e := range edges {
		me := MapEdge{From: k.from, To: k.to, Calls: e.n}
		me.Rate, me.ErrorRate, me.P95 = e.summarize(secs)
		out = append(out, me)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out, sampled, nil
}
