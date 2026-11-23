package store

import (
	"database/sql"
	"encoding/json"
	"path"
	"strings"
	"time"
)

// migrate applies schema changes that CREATE TABLE IF NOT EXISTS cannot.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(m2Schema); err != nil {
		return err
	}
	for _, col := range []string{"services", "locations", "severity"} {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('channels') WHERE name=?`, col).Scan(&n)
		if n == 0 {
			if _, err := s.db.Exec(`ALTER TABLE channels ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	return nil
}

const m2Schema = `
CREATE TABLE IF NOT EXISTS silences (
  id TEXT PRIMARY KEY,
  service TEXT NOT NULL DEFAULT '',
  location TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  starts_ms INTEGER NOT NULL,
  ends_ms INTEGER NOT NULL,
  created_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS slos (
  id TEXT PRIMARY KEY,
  spec TEXT NOT NULL,
  created_ms INTEGER NOT NULL
);
`

// MatchGlobs reports whether s matches any of the comma-separated glob
// patterns. An empty pattern list matches everything.
func MatchGlobs(patterns, s string) bool {
	if strings.TrimSpace(patterns) == "" {
		return true
	}
	for _, p := range strings.Split(patterns, ",") {
		if ok, _ := path.Match(strings.TrimSpace(p), s); ok {
			return true
		}
	}
	return false
}

// ValidGlobs reports whether every comma-separated pattern is well formed.
func ValidGlobs(patterns string) bool {
	for _, p := range strings.Split(patterns, ",") {
		if _, err := path.Match(strings.TrimSpace(p), ""); err != nil {
			return false
		}
	}
	return true
}

// Routes reports whether an alert with these attributes goes to the channel.
// A channel with no routing fields receives everything. An alert that has no
// service or location (some query monitors) only matches channels that do not
// filter on that dimension.
func (c Channel) Routes(service, location, severity string) bool {
	if c.Severity == "critical" && severity != "critical" {
		return false
	}
	return MatchGlobs(c.Services, service) && MatchGlobs(c.Locations, location)
}

// ---- silences ----

// Silence suppresses notifications (not alerts) for matching services and
// locations during a time window: planned maintenance, a known incident.
type Silence struct {
	ID        string `json:"id"`
	Service   string `json:"service"`  // comma-separated globs; empty = all
	Location  string `json:"location"` // comma-separated globs; empty = all
	Reason    string `json:"reason"`
	StartsMs  int64  `json:"startsMs"`
	EndsMs    int64  `json:"endsMs"`
	CreatedMs int64  `json:"createdMs"`
}

func (s Silence) Covers(service, location string, nowMs int64) bool {
	return nowMs >= s.StartsMs && nowMs < s.EndsMs && MatchGlobs(s.Service, service) && MatchGlobs(s.Location, location)
}

// ListSilences returns silences that have not ended yet.
func (s *Store) ListSilences(nowMs int64) ([]Silence, error) {
	rows, err := s.db.Query(`SELECT id,service,location,reason,starts_ms,ends_ms,created_ms FROM silences WHERE ends_ms>? ORDER BY ends_ms`, nowMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Silence{}
	for rows.Next() {
		var x Silence
		if err := rows.Scan(&x.ID, &x.Service, &x.Location, &x.Reason, &x.StartsMs, &x.EndsMs, &x.CreatedMs); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) CreateSilence(x *Silence) error {
	x.ID, x.CreatedMs = NewID(), time.Now().UnixMilli()
	return s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM silences WHERE ends_ms<?`, x.CreatedMs-7*86400_000); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO silences (id,service,location,reason,starts_ms,ends_ms,created_ms) VALUES (?,?,?,?,?,?,?)`,
			x.ID, x.Service, x.Location, x.Reason, x.StartsMs, x.EndsMs, x.CreatedMs)
		return err
	})
}

func (s *Store) DeleteSilence(id string) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM silences WHERE id=?`, id)
		return err
	})
}

// ---- service level objectives ----

// SLO is a reliability target for one service: a share of requests that must
// succeed (availability) or finish within a threshold (latency), measured
// over a rolling window.
type SLO struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Service     string  `json:"service"`
	Location    string  `json:"location"`    // comma-separated globs; empty = every location
	Kind        string  `json:"kind"`        // availability | latency
	ThresholdMs float64 `json:"thresholdMs"` // latency only
	Target      float64 `json:"target"`      // percent, e.g. 99.9
	WindowDays  int     `json:"windowDays"`  // rolling window
	CreatedMs   int64   `json:"createdMs"`
}

func (s *Store) ListSLOs() ([]SLO, error) {
	rows, err := s.db.Query(`SELECT id,spec,created_ms FROM slos ORDER BY created_ms`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SLO{}
	for rows.Next() {
		var x SLO
		var id, spec string
		var created int64
		if err := rows.Scan(&id, &spec, &created); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(spec), &x)
		x.ID, x.CreatedMs = id, created
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) PutSLO(x *SLO) error {
	if x.ID == "" {
		x.ID = NewID()
	}
	if x.CreatedMs == 0 {
		x.CreatedMs = time.Now().UnixMilli()
	}
	spec, _ := json.Marshal(x)
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO slos (id,spec,created_ms) VALUES (?,?,?) ON CONFLICT(id) DO UPDATE SET spec=excluded.spec`, x.ID, string(spec), x.CreatedMs)
		return err
	})
}

func (s *Store) DeleteSLO(id string) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM slos WHERE id=?`, id)
		return err
	})
}
