package query

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gthimmes/telepathy/internal/store"
)

// Burn-rate thresholds (multiwindow, as in the Google SRE Workbook): a fast
// burn spends 2% of a 30-day budget in one hour, a slow burn 5% in six hours.
// The short window confirms the burn is still happening.
const (
	FastBurn = 14.4
	SlowBurn = 6.0
)

// SLO states.
const (
	SLOOk        = "ok"
	SLOSlowBurn  = "slow-burn"
	SLOFastBurn  = "fast-burn"
	SLOExhausted = "exhausted"
	SLONoData    = "no-data"
)

type SLOLocation struct {
	Location   string   `json:"location"`
	Good       int64    `json:"good"`
	Total      int64    `json:"total"`
	SLI        *float64 `json:"sli"`        // percent
	BudgetLeft *float64 `json:"budgetLeft"` // percent of this location's own error budget
	Burn1h     float64  `json:"burn1h"`
}

// SLOReport is an SLO with its current standing.
type SLOReport struct {
	store.SLO
	State      string        `json:"state"`
	Good       int64         `json:"good"`
	Total      int64         `json:"total"`
	SLI        *float64      `json:"sli"`        // percent of good requests in the window
	BudgetLeft *float64      `json:"budgetLeft"` // percent of the error budget remaining; negative = overspent
	Burn5m     float64       `json:"burn5m"`     // 1 = spending exactly the budget
	Burn30m    float64       `json:"burn30m"`
	Burn1h     float64       `json:"burn1h"`
	Burn6h     float64       `json:"burn6h"`
	Recent     int64         `json:"recentRequests"` // requests in the last hour
	Locations  []SLOLocation `json:"locations"`
	Timestamps []int64       `json:"timestamps"` // unix ms, hourly
	Budget     []float64     `json:"budget"`     // percent of budget remaining after each hour
}

// ValidateSLO checks a definition.
func ValidateSLO(x *store.SLO) error {
	x.Name, x.Service = strings.TrimSpace(x.Name), strings.TrimSpace(x.Service)
	switch {
	case x.Name == "":
		return fmt.Errorf("name is required")
	case x.Service == "":
		return fmt.Errorf("service is required")
	case x.Kind != "availability" && x.Kind != "latency":
		return fmt.Errorf("kind must be availability or latency")
	case x.Kind == "latency" && x.ThresholdMs <= 0:
		return fmt.Errorf("a latency objective needs a threshold in milliseconds")
	case x.Target < 50 || x.Target >= 100:
		return fmt.Errorf("target must be at least 50 and below 100 percent")
	case x.WindowDays < 1 || x.WindowDays > 90:
		return fmt.Errorf("window must be between 1 and 90 days")
	case !store.ValidGlobs(x.Location):
		return fmt.Errorf("invalid location pattern")
	}
	if x.Kind == "availability" {
		x.ThresholdMs = 0
	}
	return nil
}

func sloGood(x *store.SLO, r *store.Rollup) int64 {
	if x.Kind == "latency" {
		return r.Hist.CountLE(x.ThresholdMs)
	}
	return r.N - r.Errs
}

func pct(good, total int64) *float64 {
	if total == 0 {
		return nil
	}
	v := float64(good) / float64(total) * 100
	return &v
}

// budgetLeft is the share of the error budget still unspent, in percent.
func budgetLeft(good, total int64, target float64) *float64 {
	if total == 0 {
		return nil
	}
	allowed := float64(total) * (1 - target/100)
	v := (1 - float64(total-good)/allowed) * 100
	return &v
}

func burn(good, total int64, target float64) float64 {
	if total == 0 {
		return 0
	}
	return float64(total-good) / float64(total) / (1 - target/100)
}

// EvalSLO computes an SLO's standing from the request rollups.
func EvalSLO(db *sql.DB, x store.SLO, now time.Time) (*SLOReport, error) {
	rep := &SLOReport{SLO: x, State: SLONoData, Locations: []SLOLocation{}, Timestamps: []int64{}, Budget: []float64{}}
	nowSec := now.Unix()
	from := nowSec - int64(x.WindowDays)*86400
	from -= from % store.StepHour

	hours, err := store.ReadRollups(db, store.StepHour, from, nowSec+1, x.Service, "")
	if err != nil {
		return nil, err
	}
	type tot struct{ good, total int64 }
	perLoc := map[string]*tot{}
	perHour := map[int64]*tot{}
	for i := range hours {
		r := &hours[i]
		if !store.MatchGlobs(x.Location, r.Location) {
			continue
		}
		g := sloGood(&x, r)
		rep.Good += g
		rep.Total += r.N
		l := perLoc[r.Location]
		if l == nil {
			l = &tot{}
			perLoc[r.Location] = l
		}
		l.good += g
		l.total += r.N
		h := perHour[r.Ts]
		if h == nil {
			h = &tot{}
			perHour[r.Ts] = h
		}
		h.good += g
		h.total += r.N
	}
	rep.SLI = pct(rep.Good, rep.Total)
	rep.BudgetLeft = budgetLeft(rep.Good, rep.Total, x.Target)

	// Burn rates over the recent past, from minute rollups.
	mins, err := store.ReadRollups(db, store.StepMinute, nowSec-6*3600, nowSec+1, x.Service, "")
	if err != nil {
		return nil, err
	}
	var w [4]tot // 5m, 30m, 1h, 6h
	spans := [4]int64{300, 1800, 3600, 6 * 3600}
	loc1h := map[string]*tot{}
	for i := range mins {
		r := &mins[i]
		if !store.MatchGlobs(x.Location, r.Location) {
			continue
		}
		g := sloGood(&x, r)
		for j, s := range spans {
			if r.Ts >= nowSec-s {
				w[j].good += g
				w[j].total += r.N
			}
		}
		if r.Ts >= nowSec-3600 {
			l := loc1h[r.Location]
			if l == nil {
				l = &tot{}
				loc1h[r.Location] = l
			}
			l.good += g
			l.total += r.N
		}
	}
	rep.Burn5m, rep.Burn30m = burn(w[0].good, w[0].total, x.Target), burn(w[1].good, w[1].total, x.Target)
	rep.Burn1h, rep.Burn6h = burn(w[2].good, w[2].total, x.Target), burn(w[3].good, w[3].total, x.Target)
	rep.Recent = w[2].total

	for name, l := range perLoc {
		sl := SLOLocation{Location: name, Good: l.good, Total: l.total, SLI: pct(l.good, l.total), BudgetLeft: budgetLeft(l.good, l.total, x.Target)}
		if r := loc1h[name]; r != nil {
			sl.Burn1h = burn(r.good, r.total, x.Target)
		}
		rep.Locations = append(rep.Locations, sl)
	}
	// Worst first: the location spending the most budget leads.
	sort.Slice(rep.Locations, func(i, j int) bool {
		a, b := rep.Locations[i], rep.Locations[j]
		if *a.BudgetLeft != *b.BudgetLeft {
			return *a.BudgetLeft < *b.BudgetLeft
		}
		return a.Location < b.Location
	})

	// Budget remaining over time: cumulative bad requests against the budget
	// the whole window's traffic allows.
	if rep.Total > 0 {
		allowed := float64(rep.Total) * (1 - x.Target/100)
		var bad float64
		first := nowSec
		for ts := range perHour {
			if ts < first {
				first = ts
			}
		}
		for ts := first; ts <= nowSec; ts += store.StepHour {
			if h := perHour[ts]; h != nil {
				bad += float64(h.total - h.good)
			}
			rep.Timestamps = append(rep.Timestamps, ts*1000)
			rep.Budget = append(rep.Budget, (1-bad/allowed)*100)
		}
		switch {
		case *rep.BudgetLeft <= 0:
			rep.State = SLOExhausted
		case rep.Burn1h >= FastBurn && rep.Burn5m >= FastBurn:
			rep.State = SLOFastBurn
		case rep.Burn6h >= SlowBurn && rep.Burn30m >= SlowBurn:
			rep.State = SLOSlowBurn
		default:
			rep.State = SLOOk
		}
	}
	return rep, nil
}

// SLOs evaluates every defined SLO.
func SLOs(db *sql.DB, st *store.Store) ([]SLOReport, error) {
	defs, err := st.ListSLOs()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := []SLOReport{}
	for _, x := range defs {
		rep, err := EvalSLO(db, x, now)
		if err != nil {
			return nil, err
		}
		out = append(out, *rep)
	}
	return out, nil
}
