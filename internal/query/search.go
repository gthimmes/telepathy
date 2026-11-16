package query

import (
	"database/sql"
	"encoding/json"

	"github.com/gthimmes/telepathy/internal/store"
)

func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

const spanCols = `ts_ns,trace_id,span_id,parent_id,service,location,env,name,kind,duration_ns,status,status_msg,attrs`

func scanSpans(rows *sql.Rows) ([]store.Span, error) {
	defer rows.Close()
	out := []store.Span{}
	for rows.Next() {
		var s store.Span
		var attrs string
		if err := rows.Scan(&s.TsNs, &s.TraceID, &s.SpanID, &s.ParentID, &s.Service, &s.Location, &s.Env,
			&s.Name, &s.Kind, &s.DurationNs, &s.Status, &s.StatusMsg, &attrs); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(attrs), &s.Attrs)
		out = append(out, s)
	}
	return out, rows.Err()
}

// SearchSpans returns matching spans. order is "recent" (default) or "slow".
func SearchSpans(db *sql.DB, filter string, tr TimeRange, order string, limit int) ([]store.Span, error) {
	where, args, err := Compile(Spans, filter)
	if err != nil {
		return nil, err
	}
	by := "ts_ns DESC"
	if order == "slow" {
		by = "duration_ns DESC"
	}
	q := `SELECT ` + spanCols + ` FROM spans WHERE ts_ns>=? AND ts_ns<? AND ` + where + ` ORDER BY ` + by + ` LIMIT ?`
	rows, err := db.Query(q, append(append([]any{tr.FromNs, tr.ToNs}, args...), clampLimit(limit, 100, 1000))...)
	if err != nil {
		return nil, err
	}
	return scanSpans(rows)
}

func scanLogs(rows *sql.Rows) ([]store.Log, error) {
	defer rows.Close()
	out := []store.Log{}
	for rows.Next() {
		var l store.Log
		var attrs string
		if err := rows.Scan(&l.TsNs, &l.Service, &l.Location, &l.Env, &l.SeverityNum, &l.Severity,
			&l.Body, &l.TraceID, &l.SpanID, &attrs); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(attrs), &l.Attrs)
		out = append(out, l)
	}
	return out, rows.Err()
}

const logCols = `ts_ns,service,location,env,severity_num,severity,body,trace_id,span_id,attrs`

func SearchLogs(db *sql.DB, filter string, tr TimeRange, limit int) ([]store.Log, error) {
	where, args, err := Compile(Logs, filter)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + logCols + ` FROM logs WHERE ts_ns>=? AND ts_ns<? AND ` + where + ` ORDER BY ts_ns DESC LIMIT ?`
	rows, err := db.Query(q, append(append([]any{tr.FromNs, tr.ToNs}, args...), clampLimit(limit, 200, 1000))...)
	if err != nil {
		return nil, err
	}
	return scanLogs(rows)
}

// Trace is every span of a trace plus the logs emitted inside it.
type Trace struct {
	TraceID string       `json:"traceId"`
	Spans   []store.Span `json:"spans"`
	Logs    []store.Log  `json:"logs"`
}

func GetTrace(db *sql.DB, id string) (*Trace, error) {
	rows, err := db.Query(`SELECT `+spanCols+` FROM spans WHERE trace_id=? ORDER BY ts_ns LIMIT 5000`, id)
	if err != nil {
		return nil, err
	}
	spans, err := scanSpans(rows)
	if err != nil {
		return nil, err
	}
	lrows, err := db.Query(`SELECT `+logCols+` FROM logs WHERE trace_id=? ORDER BY ts_ns LIMIT 2000`, id)
	if err != nil {
		return nil, err
	}
	logs, err := scanLogs(lrows)
	if err != nil {
		return nil, err
	}
	return &Trace{TraceID: id, Spans: spans, Logs: logs}, nil
}

// Meta lists the known dimension values, for pickers and autocompletion.
type Meta struct {
	Services  []string `json:"services"`
	Locations []string `json:"locations"`
	Metrics   []string `json:"metrics"`
}

func GetMeta(db *sql.DB, sinceNs int64) (*Meta, error) {
	m := &Meta{}
	for _, q := range []struct {
		dst *[]string
		sql string
	}{
		{&m.Services, `SELECT DISTINCT service FROM spans WHERE ts_ns>=? UNION SELECT DISTINCT service FROM metrics WHERE ts_ns>=? ORDER BY 1`},
		{&m.Locations, `SELECT DISTINCT location FROM spans WHERE ts_ns>=? UNION SELECT DISTINCT location FROM metrics WHERE ts_ns>=? ORDER BY 1`},
		{&m.Metrics, `SELECT DISTINCT name FROM metrics WHERE ts_ns>=? AND ts_ns>=? ORDER BY 1`},
	} {
		*q.dst = []string{}
		rows, err := db.Query(q.sql, sinceNs, sinceNs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var s string
			rows.Scan(&s)
			*q.dst = append(*q.dst, s)
		}
		rows.Close()
	}
	return m, nil
}
