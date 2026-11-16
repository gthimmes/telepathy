// Package store is Telepathy's embedded storage engine (SQLite, pure Go).
//
// It is deliberately small: four append-only signal tables that share the same
// first-class dimensions (service, location, env) plus a JSON attribute bag.
// The query layer only depends on this schema, so the engine can be swapped
// for a columnar store (ClickHouse) without touching ingest or the API.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Span status codes (mirror OTLP).
const (
	StatusUnset = 0
	StatusOK    = 1
	StatusError = 2
)

type Span struct {
	TraceID    string         `json:"traceId"`
	SpanID     string         `json:"spanId"`
	ParentID   string         `json:"parentId"`
	Service    string         `json:"service"`
	Location   string         `json:"location"`
	Env        string         `json:"env"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	TsNs       int64          `json:"tsNs"`
	DurationNs int64          `json:"durationNs"`
	Status     int            `json:"status"`
	StatusMsg  string         `json:"statusMsg,omitempty"`
	Attrs      map[string]any `json:"attrs"`
}

type Log struct {
	TsNs        int64          `json:"tsNs"`
	Service     string         `json:"service"`
	Location    string         `json:"location"`
	Env         string         `json:"env"`
	SeverityNum int            `json:"severityNum"`
	Severity    string         `json:"severity"`
	Body        string         `json:"body"`
	TraceID     string         `json:"traceId,omitempty"`
	SpanID      string         `json:"spanId,omitempty"`
	Attrs       map[string]any `json:"attrs"`
}

type MetricPoint struct {
	TsNs     int64
	Name     string
	Service  string
	Location string
	Env      string
	Kind     string // gauge | sum | histogram | summary
	Unit     string
	Value    float64 // gauge/sum value; mean for histograms
	Count    int64   // histogram sample count
	Sum      float64 // histogram sum
	Attrs    map[string]any
}

// Event is a change event: a deploy, config change, feature flag flip, scale
// operation. Changes are a first-class signal because most incidents start
// with one.
type Event struct {
	ID       int64          `json:"id"`
	TsNs     int64          `json:"tsNs"`
	Kind     string         `json:"kind"`
	Service  string         `json:"service"`
	Location string         `json:"location"`
	Env      string         `json:"env"`
	Title    string         `json:"title"`
	Attrs    map[string]any `json:"attrs"`
}

type Dashboard struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Spec        json.RawMessage `json:"spec"`
	UpdatedMs   int64           `json:"updatedMs"`
	Provisioned bool            `json:"provisioned"`
}

type Store struct {
	db *sql.DB
	wm sync.Mutex // SQLite has a single writer; serialize to avoid busy retries
}

const schema = `
CREATE TABLE IF NOT EXISTS spans (
  ts_ns INTEGER NOT NULL,
  trace_id TEXT NOT NULL,
  span_id TEXT NOT NULL,
  parent_id TEXT NOT NULL DEFAULT '',
  service TEXT NOT NULL,
  location TEXT NOT NULL,
  env TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  duration_ns INTEGER NOT NULL,
  status INTEGER NOT NULL DEFAULT 0,
  status_msg TEXT NOT NULL DEFAULT '',
  attrs TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS spans_ts ON spans(ts_ns);
CREATE INDEX IF NOT EXISTS spans_svc ON spans(service, location, ts_ns);
CREATE INDEX IF NOT EXISTS spans_trace ON spans(trace_id);

CREATE TABLE IF NOT EXISTS logs (
  ts_ns INTEGER NOT NULL,
  service TEXT NOT NULL,
  location TEXT NOT NULL,
  env TEXT NOT NULL DEFAULT '',
  severity_num INTEGER NOT NULL DEFAULT 0,
  severity TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '',
  trace_id TEXT NOT NULL DEFAULT '',
  span_id TEXT NOT NULL DEFAULT '',
  attrs TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS logs_ts ON logs(ts_ns);
CREATE INDEX IF NOT EXISTS logs_svc ON logs(service, location, ts_ns);
CREATE INDEX IF NOT EXISTS logs_trace ON logs(trace_id);

CREATE TABLE IF NOT EXISTS metrics (
  ts_ns INTEGER NOT NULL,
  name TEXT NOT NULL,
  service TEXT NOT NULL,
  location TEXT NOT NULL,
  env TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  unit TEXT NOT NULL DEFAULT '',
  value REAL NOT NULL,
  count INTEGER NOT NULL DEFAULT 0,
  sum REAL NOT NULL DEFAULT 0,
  attrs TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS metrics_name ON metrics(name, ts_ns);

CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts_ns INTEGER NOT NULL,
  kind TEXT NOT NULL,
  service TEXT NOT NULL DEFAULT '',
  location TEXT NOT NULL DEFAULT '',
  env TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL,
  attrs TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts_ns);

CREATE TABLE IF NOT EXISTS dashboards (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  spec TEXT NOT NULL,
  updated_ms INTEGER NOT NULL,
  provisioned INTEGER NOT NULL DEFAULT 0
);
`

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + strings.ReplaceAll(path, `\`, "/") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for the read-only query layer.
func (s *Store) DB() *sql.DB { return s.db }

func attrsJSON(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (s *Store) write(fn func(tx *sql.Tx) error) error {
	s.wm.Lock()
	defer s.wm.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) InsertSpans(spans []Span) error {
	if len(spans) == 0 {
		return nil
	}
	return s.write(func(tx *sql.Tx) error {
		st, err := tx.Prepare(`INSERT INTO spans
			(ts_ns,trace_id,span_id,parent_id,service,location,env,name,kind,duration_ns,status,status_msg,attrs)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for i := range spans {
			p := &spans[i]
			if _, err := st.Exec(p.TsNs, p.TraceID, p.SpanID, p.ParentID, p.Service, p.Location, p.Env,
				p.Name, p.Kind, p.DurationNs, p.Status, p.StatusMsg, attrsJSON(p.Attrs)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) InsertLogs(logs []Log) error {
	if len(logs) == 0 {
		return nil
	}
	return s.write(func(tx *sql.Tx) error {
		st, err := tx.Prepare(`INSERT INTO logs
			(ts_ns,service,location,env,severity_num,severity,body,trace_id,span_id,attrs)
			VALUES (?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for i := range logs {
			l := &logs[i]
			if _, err := st.Exec(l.TsNs, l.Service, l.Location, l.Env, l.SeverityNum, l.Severity,
				l.Body, l.TraceID, l.SpanID, attrsJSON(l.Attrs)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) InsertMetrics(pts []MetricPoint) error {
	if len(pts) == 0 {
		return nil
	}
	return s.write(func(tx *sql.Tx) error {
		st, err := tx.Prepare(`INSERT INTO metrics
			(ts_ns,name,service,location,env,kind,unit,value,count,sum,attrs)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for i := range pts {
			m := &pts[i]
			if _, err := st.Exec(m.TsNs, m.Name, m.Service, m.Location, m.Env, m.Kind, m.Unit,
				m.Value, m.Count, m.Sum, attrsJSON(m.Attrs)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) InsertEvent(e *Event) error {
	if e.TsNs == 0 {
		e.TsNs = time.Now().UnixNano()
	}
	return s.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO events (ts_ns,kind,service,location,env,title,attrs) VALUES (?,?,?,?,?,?,?)`,
			e.TsNs, e.Kind, e.Service, e.Location, e.Env, e.Title, attrsJSON(e.Attrs))
		if err != nil {
			return err
		}
		e.ID, _ = res.LastInsertId()
		return nil
	})
}

func (s *Store) Events(fromNs, toNs int64) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id,ts_ns,kind,service,location,env,title,attrs FROM events
		WHERE ts_ns>=? AND ts_ns<? ORDER BY ts_ns DESC LIMIT 500`, fromNs, toNs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var attrs string
		if err := rows.Scan(&e.ID, &e.TsNs, &e.Kind, &e.Service, &e.Location, &e.Env, &e.Title, &attrs); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(attrs), &e.Attrs)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestSpanNs returns the newest span timestamp, or 0 when empty.
func (s *Store) LatestSpanNs() int64 {
	var v sql.NullInt64
	s.db.QueryRow(`SELECT MAX(ts_ns) FROM spans`).Scan(&v)
	return v.Int64
}

// Prune deletes signal data older than the cutoff.
func (s *Store) Prune(cutoffNs int64) error {
	return s.write(func(tx *sql.Tx) error {
		for _, t := range []string{"spans", "logs", "metrics", "events"} {
			if _, err := tx.Exec(`DELETE FROM `+t+` WHERE ts_ns < ?`, cutoffNs); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- dashboards ----

func (s *Store) ListDashboards() ([]Dashboard, error) {
	rows, err := s.db.Query(`SELECT id,title,spec,updated_ms,provisioned FROM dashboards ORDER BY title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Dashboard{}
	for rows.Next() {
		var d Dashboard
		var spec string
		if err := rows.Scan(&d.ID, &d.Title, &spec, &d.UpdatedMs, &d.Provisioned); err != nil {
			return nil, err
		}
		d.Spec = json.RawMessage(spec)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetDashboard(id string) (*Dashboard, error) {
	var d Dashboard
	var spec string
	err := s.db.QueryRow(`SELECT id,title,spec,updated_ms,provisioned FROM dashboards WHERE id=?`, id).
		Scan(&d.ID, &d.Title, &spec, &d.UpdatedMs, &d.Provisioned)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.Spec = json.RawMessage(spec)
	return &d, nil
}

func (s *Store) PutDashboard(d *Dashboard) error {
	d.UpdatedMs = time.Now().UnixMilli()
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO dashboards (id,title,spec,updated_ms,provisioned) VALUES (?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET title=excluded.title, spec=excluded.spec,
			updated_ms=excluded.updated_ms, provisioned=excluded.provisioned`,
			d.ID, d.Title, string(d.Spec), d.UpdatedMs, d.Provisioned)
		return err
	})
}

func (s *Store) DeleteDashboard(id string) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM dashboards WHERE id=?`, id)
		return err
	})
}
