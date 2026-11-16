package query

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Health statuses, ordered by severity.
const (
	Healthy  = "healthy"
	Degraded = "degraded"
	Critical = "critical"
	Silent   = "silent" // had traffic in the baseline, none now
)

var sevRank = map[string]int{Healthy: 0, Silent: 1, Degraded: 2, Critical: 3}

// Cell is the health of one application at one location.
type Cell struct {
	Service   string   `json:"service"`
	Location  string   `json:"location"`
	Status    string   `json:"status"`
	Requests  int64    `json:"requests"`
	Rate      float64  `json:"rate"`      // req/s in window
	ErrorRate float64  `json:"errorRate"` // percent
	P95       float64  `json:"p95"`       // ms
	BaseRate  float64  `json:"baseRate"`
	BaseError float64  `json:"baseErrorRate"`
	BaseP95   float64  `json:"baseP95"`
	Reasons   []string `json:"reasons"`
}

type Rollup struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type HealthReport struct {
	GeneratedMs int64          `json:"generatedMs"`
	WindowSec   int            `json:"windowSec"`
	Services    []Rollup       `json:"services"`
	Locations   []Rollup       `json:"locations"`
	Cells       []Cell         `json:"cells"`
	Summary     map[string]int `json:"summary"`
}

const baselineSlices = 12

type slice struct {
	n, errs int
	durs    []float64
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	return Quantile(v, 0.5)
}

// Health computes the application × location health matrix.
//
// There are no monitors to configure: every cell is compared against its own
// recent baseline (the median of the preceding window-sized slices, which
// keeps an earlier incident from poisoning the baseline) and against absolute
// error-rate guardrails. A "request" is a server/consumer span or a root span.
func Health(db *sql.DB, window time.Duration) (*HealthReport, error) {
	now := time.Now().UnixNano()
	w := int64(window)
	from := now - w*(baselineSlices+1)
	rows, err := db.Query(`SELECT service, location, ts_ns, duration_ns, status FROM spans
		WHERE ts_ns>=? AND ts_ns<? AND (kind IN ('server','consumer') OR parent_id='')`, from, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type key struct{ svc, loc string }
	data := map[key]*[baselineSlices + 1]slice{}
	for rows.Next() {
		var k key
		var ts, dur int64
		var status int
		if err := rows.Scan(&k.svc, &k.loc, &ts, &dur, &status); err != nil {
			return nil, err
		}
		idx := int((ts - from) / w) // last index = current window
		if idx < 0 || idx > baselineSlices {
			continue
		}
		d := data[k]
		if d == nil {
			d = &[baselineSlices + 1]slice{}
			data[k] = d
		}
		d[idx].n++
		if status == 2 {
			d[idx].errs++
		}
		d[idx].durs = append(d[idx].durs, float64(dur)/1e6)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rep := &HealthReport{GeneratedMs: now / 1e6, WindowSec: int(window.Seconds()), Cells: []Cell{},
		Summary: map[string]int{Healthy: 0, Degraded: 0, Critical: 0, Silent: 0}}
	svcWorst, locWorst := map[string]string{}, map[string]string{}
	secs := window.Seconds()
	for k, d := range data {
		cur := &d[baselineSlices]
		c := Cell{Service: k.svc, Location: k.loc, Status: Healthy, Requests: int64(cur.n), Reasons: []string{}}
		var bRates, bErrs, bP95s []float64
		for i := 0; i < baselineSlices; i++ {
			s := &d[i]
			if s.n == 0 {
				continue
			}
			sort.Float64s(s.durs)
			bRates = append(bRates, float64(s.n)/secs)
			bErrs = append(bErrs, float64(s.errs)/float64(s.n)*100)
			bP95s = append(bP95s, Quantile(s.durs, 0.95))
		}
		hasBase := len(bRates) >= 2
		c.BaseRate, c.BaseError, c.BaseP95 = median(bRates), median(bErrs), median(bP95s)

		bump := func(status, reason string) {
			if sevRank[status] > sevRank[c.Status] {
				c.Status = status
			}
			c.Reasons = append(c.Reasons, reason)
		}
		if cur.n == 0 {
			if !hasBase {
				continue // long gone; not part of the current picture
			}
			bump(Silent, fmt.Sprintf("no traffic in the last %s (baseline %.1f req/s)", window, c.BaseRate))
		} else {
			sort.Float64s(cur.durs)
			c.Rate = float64(cur.n) / secs
			c.ErrorRate = float64(cur.errs) / float64(cur.n) * 100
			c.P95 = Quantile(cur.durs, 0.95)
			enough := cur.n >= 10
			errEnough := enough && cur.errs >= 5 // a couple of stray failures is not a signal

			switch {
			case errEnough && c.ErrorRate >= 5 && (!hasBase || c.ErrorRate > 2*c.BaseError):
				bump(Critical, fmt.Sprintf("error rate %.1f%% (baseline %.1f%%)", c.ErrorRate, c.BaseError))
			case errEnough && c.ErrorRate >= 5:
				bump(Degraded, fmt.Sprintf("error rate chronically high at %.1f%%", c.ErrorRate))
			case errEnough && c.ErrorRate >= 1 && (!hasBase || c.ErrorRate > 2*c.BaseError+0.5):
				bump(Degraded, fmt.Sprintf("error rate %.1f%% (baseline %.1f%%)", c.ErrorRate, c.BaseError))
			}
			if enough && hasBase && c.BaseP95 > 0 && c.P95 > 50 {
				ratio := c.P95 / c.BaseP95
				if ratio >= 4 {
					bump(Critical, fmt.Sprintf("p95 latency %.0fms is %.1f× baseline (%.0fms)", c.P95, ratio, c.BaseP95))
				} else if ratio >= 2 {
					bump(Degraded, fmt.Sprintf("p95 latency %.0fms is %.1f× baseline (%.0fms)", c.P95, ratio, c.BaseP95))
				}
			}
			if hasBase && c.BaseRate*secs >= 20 && c.Rate < 0.3*c.BaseRate {
				bump(Degraded, fmt.Sprintf("traffic down %.0f%% vs baseline", (1-c.Rate/c.BaseRate)*100))
			}
		}
		rep.Cells = append(rep.Cells, c)
		rep.Summary[c.Status]++
		if sevRank[c.Status] >= sevRank[svcWorst[k.svc]] {
			svcWorst[k.svc] = c.Status
		}
		if sevRank[c.Status] >= sevRank[locWorst[k.loc]] {
			locWorst[k.loc] = c.Status
		}
	}
	sort.Slice(rep.Cells, func(i, j int) bool {
		a, b := rep.Cells[i], rep.Cells[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Location < b.Location
	})
	rep.Services, rep.Locations = rollups(svcWorst), rollups(locWorst)
	return rep, nil
}

func rollups(m map[string]string) []Rollup {
	out := []Rollup{}
	for n, s := range m {
		out = append(out, Rollup{n, s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
