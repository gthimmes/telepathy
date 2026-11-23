package store

import (
	"database/sql"
	"encoding/binary"
	"math"
	"sort"
	"time"
)

// Rollups are pre-aggregated request statistics: for every service × location
// and every minute (and hour), the request count, the error count and a
// latency histogram. They are written in the same transaction as the spans
// they summarize.
//
// A "request" is a server or consumer span, or a root span — the same
// definition the health matrix uses. Health, SLOs and burn rates read rollups
// instead of scanning spans, so their cost depends on the number of
// services × locations × minutes, not on traffic volume, and they keep working
// after the raw spans have aged out.
const rollupSchema = `
CREATE TABLE IF NOT EXISTS rollups (
  step INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  service TEXT NOT NULL,
  location TEXT NOT NULL,
  n INTEGER NOT NULL,
  errs INTEGER NOT NULL,
  hist BLOB NOT NULL,
  PRIMARY KEY (step, ts, service, location)
) WITHOUT ROWID;
`

// Rollup resolutions, in seconds.
const (
	StepMinute = 60
	StepHour   = 3600
)

// histGamma sets the histogram's relative accuracy: bucket boundaries grow by
// 4%, so any quantile read from it is within about 2% of the true value.
const histGamma = 1.04

var logGamma = math.Log(histGamma)

// Hist is a sparse log-scale latency histogram (bucket index → count), in the
// style of DDSketch. Histograms of the same shape merge by adding counts.
type Hist map[int32]int64

func histIndex(ms float64) int32 {
	if ms < 0.001 {
		ms = 0.001
	}
	return int32(math.Ceil(math.Log(ms) / logGamma))
}

// Add records one duration in milliseconds.
func (h Hist) Add(ms float64) { h[histIndex(ms)]++ }

// Merge adds another histogram's counts.
func (h Hist) Merge(o Hist) {
	for i, c := range o {
		h[i] += c
	}
}

func (h Hist) Count() int64 {
	var n int64
	for _, c := range h {
		n += c
	}
	return n
}

func (h Hist) sorted() []int32 {
	idx := make([]int32, 0, len(h))
	for i := range h {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
	return idx
}

// Quantile returns the estimated q-quantile in milliseconds (0 when empty).
func (h Hist) Quantile(q float64) float64 {
	total := h.Count()
	if total == 0 {
		return 0
	}
	rank := int64(math.Ceil(q * float64(total)))
	if rank < 1 {
		rank = 1
	}
	var seen int64
	for _, i := range h.sorted() {
		seen += h[i]
		if seen >= rank {
			// Midpoint of the bucket (γ^(i-1), γ^i].
			return 2 * math.Pow(histGamma, float64(i)) / (histGamma + 1)
		}
	}
	return 0
}

// CountLE returns how many recorded durations were at most ms (to bucket accuracy).
func (h Hist) CountLE(ms float64) int64 {
	limit := histIndex(ms)
	var n int64
	for i, c := range h {
		if i <= limit {
			n += c
		}
	}
	return n
}

func (h Hist) encode() []byte {
	idx := h.sorted()
	buf := make([]byte, 0, len(idx)*3+2)
	buf = binary.AppendUvarint(buf, uint64(len(idx)))
	prev := int32(0)
	for _, i := range idx {
		buf = binary.AppendVarint(buf, int64(i-prev))
		buf = binary.AppendUvarint(buf, uint64(h[i]))
		prev = i
	}
	return buf
}

func decodeHist(b []byte) Hist {
	h := Hist{}
	n, k := binary.Uvarint(b)
	if k <= 0 {
		return h
	}
	b = b[k:]
	prev := int32(0)
	for ; n > 0; n-- {
		d, k1 := binary.Varint(b)
		if k1 <= 0 {
			break
		}
		c, k2 := binary.Uvarint(b[k1:])
		if k2 <= 0 {
			break
		}
		b = b[k1+k2:]
		prev += int32(d)
		h[prev] = int64(c)
	}
	return h
}

// Rollup is one service × location bucket.
type Rollup struct {
	Ts       int64 // bucket start, unix seconds
	Service  string
	Location string
	N, Errs  int64
	Hist     Hist
}

type rollupKey struct {
	step, ts int64
	svc, loc string
}

type rollupBatch map[rollupKey]*Rollup

func (b rollupBatch) add(tsNs int64, svc, loc string, durNs int64, isErr bool) {
	sec := tsNs / 1e9
	for _, step := range [2]int64{StepMinute, StepHour} {
		k := rollupKey{step, sec - sec%step, svc, loc}
		r := b[k]
		if r == nil {
			r = &Rollup{Hist: Hist{}}
			b[k] = r
		}
		r.N++
		if isErr {
			r.Errs++
		}
		r.Hist.Add(float64(durNs) / 1e6)
	}
}

func isRequest(kind, parentID string) bool {
	return kind == "server" || kind == "consumer" || parentID == ""
}

func (b rollupBatch) flush(tx *sql.Tx) error {
	if len(b) == 0 {
		return nil
	}
	get, err := tx.Prepare(`SELECT n, errs, hist FROM rollups WHERE step=? AND ts=? AND service=? AND location=?`)
	if err != nil {
		return err
	}
	defer get.Close()
	put, err := tx.Prepare(`INSERT OR REPLACE INTO rollups (step,ts,service,location,n,errs,hist) VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer put.Close()
	for k, r := range b {
		var n, errs int64
		var blob []byte
		switch err := get.QueryRow(k.step, k.ts, k.svc, k.loc).Scan(&n, &errs, &blob); err {
		case nil:
			r.N += n
			r.Errs += errs
			r.Hist.Merge(decodeHist(blob))
		case sql.ErrNoRows:
		default:
			return err
		}
		if _, err := put.Exec(k.step, k.ts, k.svc, k.loc, r.N, r.Errs, r.Hist.encode()); err != nil {
			return err
		}
	}
	return nil
}

// ReadRollups returns the buckets of one resolution in [fromSec, toSec).
// Empty service or location means all of them.
func ReadRollups(db *sql.DB, step, fromSec, toSec int64, service, location string) ([]Rollup, error) {
	q := `SELECT ts, service, location, n, errs, hist FROM rollups WHERE step=? AND ts>=? AND ts<?`
	args := []any{step, fromSec, toSec}
	if service != "" {
		q += ` AND service=?`
		args = append(args, service)
	}
	if location != "" {
		q += ` AND location=?`
		args = append(args, location)
	}
	rows, err := db.Query(q+` ORDER BY ts`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rollup
	for rows.Next() {
		var r Rollup
		var blob []byte
		if err := rows.Scan(&r.Ts, &r.Service, &r.Location, &r.N, &r.Errs, &blob); err != nil {
			return nil, err
		}
		r.Hist = decodeHist(blob)
		out = append(out, r)
	}
	return out, rows.Err()
}

// EnsureRollups builds rollups for spans stored by a version that did not
// keep them. It runs once per database, before the server accepts traffic,
// and can take minutes on a database of several gigabytes; logf reports
// progress.
func (s *Store) EnsureRollups(logf func(format string, args ...any)) error {
	if s.Setting("rollups.v1") != "" {
		return nil
	}
	var any int
	s.db.QueryRow(`SELECT 1 FROM spans LIMIT 1`).Scan(&any)
	if any == 1 {
		logf("upgrading database: summarizing existing spans into rollups (one time)…")
		if err := s.rebuildRollups(logf); err != nil {
			return err
		}
		logf("upgrading database: done")
	}
	return s.SetSetting("rollups.v1", "1")
}

// RebuildRollups recomputes every rollup from the stored spans. It must not
// run while spans are being ingested.
func (s *Store) RebuildRollups() error {
	return s.rebuildRollups(func(string, ...any) {})
}

func (s *Store) rebuildRollups(logf func(format string, args ...any)) error {
	if err := s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM rollups`)
		return err
	}); err != nil {
		return err
	}
	const chunk = int64(6 * 3600 * 1e9)
	var lo, hi sql.NullInt64
	if err := s.db.QueryRow(`SELECT MIN(ts_ns), MAX(ts_ns) FROM spans`).Scan(&lo, &hi); err != nil || !lo.Valid {
		return err
	}
	start := lo.Int64 - lo.Int64%chunk
	total := (hi.Int64-start)/chunk + 1
	last := time.Now()
	for from := start; from <= hi.Int64; from += chunk {
		if time.Since(last) > 10*time.Second {
			last = time.Now()
			logf("upgrading database: %d%%", (from-start)/chunk*100/total)
		}
		batch := rollupBatch{}
		rows, err := s.db.Query(`SELECT ts_ns, service, location, duration_ns, status FROM spans
			WHERE ts_ns>=? AND ts_ns<? AND (kind IN ('server','consumer') OR parent_id='')`, from, from+chunk)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ts, dur int64
			var svc, loc string
			var status int
			if err := rows.Scan(&ts, &svc, &loc, &dur, &status); err != nil {
				rows.Close()
				return err
			}
			batch.add(ts, svc, loc, dur, status == StatusError)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := s.write(batch.flush); err != nil {
			return err
		}
	}
	return nil
}

// PruneRollups deletes minute buckets older than minuteCutoff and hour
// buckets older than hourCutoff (unix seconds).
func (s *Store) PruneRollups(minuteCutoff, hourCutoff int64) error {
	return s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM rollups WHERE step=? AND ts<?`, StepMinute, minuteCutoff); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM rollups WHERE step=? AND ts<?`, StepHour, hourCutoff)
		return err
	})
}
