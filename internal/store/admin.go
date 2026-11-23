package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Configuration and alert state: API tokens, monitors, notification channels
// and alerts. These tables are small and are not subject to retention.
const adminSchema = `
CREATE TABLE IF NOT EXISTS tokens (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  role TEXT NOT NULL,
  hash TEXT NOT NULL UNIQUE,
  prefix TEXT NOT NULL,
  created_ms INTEGER NOT NULL,
  last_used_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS monitors (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  spec TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  updated_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS channels (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  url TEXT NOT NULL,
  created_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS alerts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  monitor_id TEXT NOT NULL,
  monitor_name TEXT NOT NULL,
  key TEXT NOT NULL,
  status TEXT NOT NULL,
  severity TEXT NOT NULL,
  title TEXT NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  service TEXT NOT NULL DEFAULT '',
  location TEXT NOT NULL DEFAULT '',
  started_ms INTEGER NOT NULL,
  updated_ms INTEGER NOT NULL,
  resolved_ms INTEGER NOT NULL DEFAULT 0,
  details TEXT NOT NULL DEFAULT '{}',
  notify_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS alerts_status ON alerts(status, started_ms);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

// NewID returns a short random identifier.
func NewID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- settings ----

func (s *Store) Setting(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (s *Store) SetSetting(key, value string) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO settings (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
		return err
	})
}

// ---- API tokens ----

// Token is an API credential. Only a hash of the secret is stored.
type Token struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"` // ingest | read | admin
	Prefix     string `json:"prefix"`
	CreatedMs  int64  `json:"createdMs"`
	LastUsedMs int64  `json:"lastUsedMs"`
}

func (s *Store) CreateToken(t *Token, hash string) error {
	t.ID, t.CreatedMs = NewID(), time.Now().UnixMilli()
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO tokens (id,name,role,hash,prefix,created_ms) VALUES (?,?,?,?,?,?)`,
			t.ID, t.Name, t.Role, hash, t.Prefix, t.CreatedMs)
		return err
	})
}

func (s *Store) ListTokens() ([]Token, error) {
	rows, err := s.db.Query(`SELECT id,name,role,prefix,created_ms,last_used_ms FROM tokens ORDER BY created_ms`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.Role, &t.Prefix, &t.CreatedMs, &t.LastUsedMs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenByHash returns the token with this secret hash, or nil.
func (s *Store) TokenByHash(hash string) *Token {
	var t Token
	err := s.db.QueryRow(`SELECT id,name,role,prefix,created_ms,last_used_ms FROM tokens WHERE hash=?`, hash).
		Scan(&t.ID, &t.Name, &t.Role, &t.Prefix, &t.CreatedMs, &t.LastUsedMs)
	if err != nil {
		return nil
	}
	return &t
}

func (s *Store) TouchToken(id string, nowMs int64) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE tokens SET last_used_ms=? WHERE id=?`, nowMs, id)
		return err
	})
}

func (s *Store) DeleteToken(id string) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM tokens WHERE id=?`, id)
		return err
	})
}

// ---- monitors ----

type Monitor struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"` // health | query
	Spec      json.RawMessage `json:"spec"`
	Enabled   bool            `json:"enabled"`
	UpdatedMs int64           `json:"updatedMs"`
}

func (s *Store) ListMonitors() ([]Monitor, error) {
	rows, err := s.db.Query(`SELECT id,name,kind,spec,enabled,updated_ms FROM monitors ORDER BY kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Monitor{}
	for rows.Next() {
		var m Monitor
		var spec string
		if err := rows.Scan(&m.ID, &m.Name, &m.Kind, &spec, &m.Enabled, &m.UpdatedMs); err != nil {
			return nil, err
		}
		m.Spec = json.RawMessage(spec)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) PutMonitor(m *Monitor) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	m.UpdatedMs = time.Now().UnixMilli()
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO monitors (id,name,kind,spec,enabled,updated_ms) VALUES (?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET name=excluded.name, kind=excluded.kind, spec=excluded.spec,
			enabled=excluded.enabled, updated_ms=excluded.updated_ms`,
			m.ID, m.Name, m.Kind, string(m.Spec), m.Enabled, m.UpdatedMs)
		return err
	})
}

func (s *Store) DeleteMonitor(id string) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM monitors WHERE id=?`, id)
		return err
	})
}

// ---- notification channels ----

type Channel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // webhook | slack | pagerduty
	URL       string `json:"url"`  // webhook URL, or the PagerDuty routing key
	CreatedMs int64  `json:"createdMs"`
}

func (s *Store) ListChannels() ([]Channel, error) {
	rows, err := s.db.Query(`SELECT id,name,kind,url,created_ms FROM channels ORDER BY created_ms`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.URL, &c.CreatedMs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CreateChannel(c *Channel) error {
	c.ID, c.CreatedMs = NewID(), time.Now().UnixMilli()
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO channels (id,name,kind,url,created_ms) VALUES (?,?,?,?,?)`, c.ID, c.Name, c.Kind, c.URL, c.CreatedMs)
		return err
	})
}

func (s *Store) DeleteChannel(id string) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM channels WHERE id=?`, id)
		return err
	})
}

// ---- alerts ----

const (
	AlertFiring   = "firing"
	AlertResolved = "resolved"
)

// Alert is one episode of a monitor's condition being true for one subject
// (Key): it opens as firing and is later resolved.
type Alert struct {
	ID          int64           `json:"id"`
	MonitorID   string          `json:"monitorId"`
	MonitorName string          `json:"monitorName"`
	Key         string          `json:"key"`
	Status      string          `json:"status"`
	Severity    string          `json:"severity"` // critical | degraded | silent
	Title       string          `json:"title"`
	Summary     string          `json:"summary"`
	Service     string          `json:"service"`
	Location    string          `json:"location"`
	StartedMs   int64           `json:"startedMs"`
	UpdatedMs   int64           `json:"updatedMs"`
	ResolvedMs  int64           `json:"resolvedMs"`
	Details     json.RawMessage `json:"details"`
	NotifyError string          `json:"notifyError,omitempty"`
}

const alertCols = `id,monitor_id,monitor_name,key,status,severity,title,summary,service,location,started_ms,updated_ms,resolved_ms,details,notify_error`

func scanAlerts(rows *sql.Rows) ([]Alert, error) {
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		var details string
		if err := rows.Scan(&a.ID, &a.MonitorID, &a.MonitorName, &a.Key, &a.Status, &a.Severity, &a.Title, &a.Summary,
			&a.Service, &a.Location, &a.StartedMs, &a.UpdatedMs, &a.ResolvedMs, &details, &a.NotifyError); err != nil {
			return nil, err
		}
		a.Details = json.RawMessage(details)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) FiringAlerts() ([]Alert, error) {
	rows, err := s.db.Query(`SELECT `+alertCols+` FROM alerts WHERE status=? ORDER BY started_ms DESC`, AlertFiring)
	if err != nil {
		return nil, err
	}
	return scanAlerts(rows)
}

// RecentAlerts returns resolved alerts, newest first.
func (s *Store) RecentAlerts(limit int) ([]Alert, error) {
	rows, err := s.db.Query(`SELECT `+alertCols+` FROM alerts WHERE status=? ORDER BY resolved_ms DESC LIMIT ?`, AlertResolved, limit)
	if err != nil {
		return nil, err
	}
	return scanAlerts(rows)
}

// SaveAlert inserts a new alert (ID 0) or updates an existing one.
func (s *Store) SaveAlert(a *Alert) error {
	if len(a.Details) == 0 {
		a.Details = json.RawMessage("{}")
	}
	return s.write(func(tx *sql.Tx) error {
		if a.ID == 0 {
			res, err := tx.Exec(`INSERT INTO alerts (monitor_id,monitor_name,key,status,severity,title,summary,service,location,started_ms,updated_ms,resolved_ms,details,notify_error)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.MonitorID, a.MonitorName, a.Key, a.Status, a.Severity, a.Title, a.Summary,
				a.Service, a.Location, a.StartedMs, a.UpdatedMs, a.ResolvedMs, string(a.Details), a.NotifyError)
			if err != nil {
				return err
			}
			a.ID, _ = res.LastInsertId()
			return nil
		}
		_, err := tx.Exec(`UPDATE alerts SET status=?, severity=?, title=?, summary=?, updated_ms=?, resolved_ms=?, details=?, notify_error=? WHERE id=?`,
			a.Status, a.Severity, a.Title, a.Summary, a.UpdatedMs, a.ResolvedMs, string(a.Details), a.NotifyError, a.ID)
		return err
	})
}

// PruneAlerts deletes resolved alerts older than the cutoff.
func (s *Store) PruneAlerts(cutoffMs int64) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM alerts WHERE status=? AND resolved_ms < ?`, AlertResolved, cutoffMs)
		return err
	})
}
