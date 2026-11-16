// Package query implements Telepathy's read path: the filter language,
// time-series aggregation, health scoring and differential analysis ("Explain").
package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Signals.
const (
	Spans   = "spans"
	Logs    = "logs"
	Metrics = "metrics"
)

// Term is one clause of a filter expression. Clauses are ANDed.
//
//	service:checkout location:eu-* status:error duration>500ms -http.route:/healthz "card declined"
type Term struct {
	Key   string // empty for a bare word (free-text match)
	Op    string // ":", "!=", ">", ">=", "<", "<="
	Value string
	Neg   bool
}

var keyRe = regexp.MustCompile(`^[A-Za-z0-9_.\-/]+$`)

// ParseFilter tokenizes a filter string.
func ParseFilter(s string) ([]Term, error) {
	var terms []Term
	for _, tok := range tokenize(s) {
		t := Term{}
		if strings.HasPrefix(tok, "-") && len(tok) > 1 {
			t.Neg = true
			tok = tok[1:]
		}
		i := strings.IndexAny(tok, ":<>!")
		if i <= 0 || strings.HasPrefix(tok, `"`) {
			t.Op, t.Value = ":", unquote(tok)
			terms = append(terms, t)
			continue
		}
		t.Key = tok[:i]
		if !keyRe.MatchString(t.Key) {
			return nil, fmt.Errorf("invalid field name %q", t.Key)
		}
		rest := tok[i:]
		switch {
		case strings.HasPrefix(rest, ">="), strings.HasPrefix(rest, "<="), strings.HasPrefix(rest, "!="):
			t.Op, t.Value = rest[:2], rest[2:]
		case rest[0] == '!':
			return nil, fmt.Errorf("invalid operator in %q", tok)
		default:
			t.Op, t.Value = rest[:1], rest[1:]
		}
		t.Value = unquote(t.Value)
		terms = append(terms, t)
	}
	return terms, nil
}

func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			cur.WriteRune(r)
		case (r == ' ' || r == '\t' || r == '\n') && !inQ:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

type sigInfo struct {
	table string
	cols  map[string]string // filter key -> column
	text  string            // column used for bare-word search
}

var signals = map[string]sigInfo{
	Spans: {"spans", map[string]string{
		"service": "service", "location": "location", "env": "env", "name": "name", "kind": "kind",
		"trace_id": "trace_id", "span_id": "span_id", "parent_id": "parent_id", "status_msg": "status_msg",
	}, "name"},
	Logs: {"logs", map[string]string{
		"service": "service", "location": "location", "env": "env", "severity": "severity", "level": "severity",
		"body": "body", "trace_id": "trace_id", "span_id": "span_id",
	}, "body"},
	Metrics: {"metrics", map[string]string{
		"service": "service", "location": "location", "env": "env", "name": "name", "metric": "name",
		"unit": "unit", "kind": "kind",
	}, "name"},
}

// fieldExpr resolves a field name to a SQL text expression. Anything that is
// not a first-class column is looked up in the JSON attribute bag; the JSON
// path is always bound as a parameter.
func fieldExpr(signal, key string) (expr string, args []any, isAttr bool, err error) {
	si, ok := signals[signal]
	if !ok {
		return "", nil, false, fmt.Errorf("unknown signal %q", signal)
	}
	if col, ok := si.cols[key]; ok {
		return col, nil, false, nil
	}
	if !keyRe.MatchString(key) {
		return "", nil, false, fmt.Errorf("invalid field name %q", key)
	}
	return "json_extract(attrs, ?)", []any{`$."` + key + `"`}, true, nil
}

// parseDurationMs accepts "250", "250ms", "1.5s", "2m" and returns milliseconds.
func parseDurationMs(v string) (float64, error) {
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", v)
	}
	return float64(d) / float64(time.Millisecond), nil
}

// Compile turns a filter string into a SQL predicate for the signal's table.
func Compile(signal, filter string) (string, []any, error) {
	terms, err := ParseFilter(filter)
	if err != nil {
		return "", nil, err
	}
	si, ok := signals[signal]
	if !ok {
		return "", nil, fmt.Errorf("unknown signal %q", signal)
	}
	where := []string{"1=1"}
	var args []any
	for _, t := range terms {
		clause, cargs, err := compileTerm(signal, si, t)
		if err != nil {
			return "", nil, err
		}
		if t.Neg {
			clause = "NOT (" + clause + ")"
		}
		where = append(where, clause)
		args = append(args, cargs...)
	}
	return strings.Join(where, " AND "), args, nil
}

func compileTerm(signal string, si sigInfo, t Term) (string, []any, error) {
	if t.Key == "" {
		return si.text + " LIKE ?", []any{"%" + t.Value + "%"}, nil
	}
	numeric := t.Op == ">" || t.Op == ">=" || t.Op == "<" || t.Op == "<="

	// Special span fields.
	if signal == Spans {
		switch t.Key {
		case "status":
			code := map[string]int{"unset": 0, "ok": 1, "error": 2}
			c, ok := code[strings.ToLower(t.Value)]
			if !ok || numeric {
				return "", nil, fmt.Errorf("status must be ok, error or unset")
			}
			if t.Op == "!=" {
				return "status != ?", []any{c}, nil
			}
			return "status = ?", []any{c}, nil
		case "duration":
			ms, err := parseDurationMs(t.Value)
			if err != nil {
				return "", nil, err
			}
			op := t.Op
			if op == ":" {
				op = "="
			}
			return "duration_ns " + op + " ?", []any{int64(ms * 1e6)}, nil
		}
	}
	if signal == Metrics && t.Key == "value" {
		f, err := strconv.ParseFloat(t.Value, 64)
		if err != nil {
			return "", nil, fmt.Errorf("value comparison needs a number")
		}
		op := t.Op
		if op == ":" {
			op = "="
		}
		return "value " + op + " ?", []any{f}, nil
	}

	expr, args, isAttr, err := fieldExpr(signal, t.Key)
	if err != nil {
		return "", nil, err
	}
	if numeric {
		f, err := strconv.ParseFloat(t.Value, 64)
		if err != nil {
			return "", nil, fmt.Errorf("%s%s needs a number", t.Key, t.Op)
		}
		return "CAST(" + expr + " AS REAL) " + t.Op + " ?", append(args, f), nil
	}
	if isAttr {
		expr = "COALESCE(CAST(" + expr + " AS TEXT),'')"
	}
	val := t.Value
	if signal == Logs && (t.Key == "severity" || t.Key == "level") {
		val = strings.ToUpper(val)
	}
	switch {
	case t.Op == "!=":
		return expr + " != ?", append(args, val), nil
	case strings.Contains(val, "*"):
		return expr + " LIKE ?", append(args, strings.ReplaceAll(val, "*", "%")), nil
	default:
		return expr + " = ?", append(args, val), nil
	}
}

// TimeRange is a resolved [From, To) window in nanoseconds.
type TimeRange struct{ FromNs, ToNs int64 }

// ResolveRange turns (fromMs, toMs, "15m") into a window ending now by default.
func ResolveRange(fromMs, toMs int64, rng string, def time.Duration) (TimeRange, error) {
	if fromMs > 0 && toMs > fromMs {
		return TimeRange{fromMs * 1e6, toMs * 1e6}, nil
	}
	d := def
	if rng != "" {
		var err error
		if d, err = ParseRange(rng); err != nil {
			return TimeRange{}, err
		}
	}
	to := time.Now().UnixNano()
	return TimeRange{to - int64(d), to}, nil
}

// ParseRange parses "15m", "6h", "2d".
func ParseRange(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid range %q", s)
		}
		return time.Duration(n * 24 * float64(time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid range %q", s)
	}
	return d, nil
}
