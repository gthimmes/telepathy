// Package api serves Telepathy's JSON API (everything the UI does is a public API call).
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gthimmes/telepathy/internal/query"
	"github.com/gthimmes/telepathy/internal/store"
)

type API struct {
	Store   *store.Store
	Version string
}

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/status", a.status)
	mux.HandleFunc("GET /api/v1/meta", a.meta)
	mux.HandleFunc("GET /api/v1/health", a.health)
	mux.HandleFunc("POST /api/v1/query", a.query)
	mux.HandleFunc("POST /api/v1/explain", a.explain)
	mux.HandleFunc("GET /api/v1/spans", a.spans)
	mux.HandleFunc("GET /api/v1/logs", a.logs)
	mux.HandleFunc("GET /api/v1/traces/{id}", a.trace)
	mux.HandleFunc("GET /api/v1/events", a.events)
	mux.HandleFunc("POST /api/v1/events", a.postEvent)
	mux.HandleFunc("GET /api/v1/dashboards", a.listDashboards)
	mux.HandleFunc("POST /api/v1/dashboards", a.putDashboard)
	mux.HandleFunc("GET /api/v1/dashboards/{id}", a.getDashboard)
	mux.HandleFunc("PUT /api/v1/dashboards/{id}", a.putDashboard)
	mux.HandleFunc("DELETE /api/v1/dashboards/{id}", a.deleteDashboard)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func rangeOf(r *http.Request, def time.Duration) (query.TimeRange, error) {
	q := r.URL.Query()
	from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	return query.ResolveRange(from, to, q.Get("range"), def)
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"name": "telepathy", "version": a.Version, "latestSpanMs": a.Store.LatestSpanNs() / 1e6})
}

func (a *API) meta(w http.ResponseWriter, r *http.Request) {
	m, err := query.GetMeta(a.Store.DB(), time.Now().Add(-24*time.Hour).UnixNano())
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, m)
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	window := 5 * time.Minute
	if s := r.URL.Query().Get("window"); s != "" {
		d, err := query.ParseRange(s)
		if err != nil || d < time.Minute || d > 6*time.Hour {
			fail(w, 400, errStr("window must be between 1m and 6h"))
			return
		}
		window = d
	}
	rep, err := query.Health(a.Store.DB(), window)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, rep)
}

type errStr string

func (e errStr) Error() string { return string(e) }

func (a *API) query(w http.ResponseWriter, r *http.Request) {
	var req query.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	res, err := query.Run(a.Store.DB(), req)
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, res)
}

func (a *API) explain(w http.ResponseWriter, r *http.Request) {
	var req query.ExplainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	res, err := query.Explain(a.Store.DB(), a.Store, req)
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, res)
}

func (a *API) spans(w http.ResponseWriter, r *http.Request) {
	tr, err := rangeOf(r, time.Hour)
	if err != nil {
		fail(w, 400, err)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	spans, err := query.SearchSpans(a.Store.DB(), q.Get("filter"), tr, q.Get("order"), limit)
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"spans": spans})
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	tr, err := rangeOf(r, time.Hour)
	if err != nil {
		fail(w, 400, err)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	logs, err := query.SearchLogs(a.Store.DB(), q.Get("filter"), tr, limit)
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"logs": logs})
}

func (a *API) trace(w http.ResponseWriter, r *http.Request) {
	t, err := query.GetTrace(a.Store.DB(), r.PathValue("id"))
	if err != nil {
		fail(w, 500, err)
		return
	}
	if len(t.Spans) == 0 {
		fail(w, 404, errStr("trace not found"))
		return
	}
	writeJSON(w, 200, t)
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	tr, err := rangeOf(r, time.Hour)
	if err != nil {
		fail(w, 400, err)
		return
	}
	evs, err := a.Store.Events(tr.FromNs, tr.ToNs)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"events": evs})
}

// postEvent records a change event. CI/CD pipelines call this on deploy:
//
//	curl -X POST :4318/api/v1/events -d '{"kind":"deploy","service":"payments","title":"payments 2.4.1"}'
func (a *API) postEvent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.Event
		TsMs int64 `json:"tsMs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, err)
		return
	}
	e := in.Event
	if e.Title == "" {
		fail(w, 400, errStr("title is required"))
		return
	}
	if e.Kind == "" {
		e.Kind = "change"
	}
	if in.TsMs > 0 {
		e.TsNs = in.TsMs * 1e6
	}
	if err := a.Store.InsertEvent(&e); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 201, e)
}

// ---- dashboards ----

func (a *API) listDashboards(w http.ResponseWriter, r *http.Request) {
	ds, err := a.Store.ListDashboards()
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"dashboards": ds})
}

func (a *API) getDashboard(w http.ResponseWriter, r *http.Request) {
	d, err := a.Store.GetDashboard(r.PathValue("id"))
	if err != nil {
		fail(w, 500, err)
		return
	}
	if d == nil {
		fail(w, 404, errStr("dashboard not found"))
		return
	}
	writeJSON(w, 200, d)
}

func (a *API) putDashboard(w http.ResponseWriter, r *http.Request) {
	var d store.Dashboard
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		fail(w, 400, err)
		return
	}
	if id := r.PathValue("id"); id != "" {
		d.ID = id
	}
	if d.ID == "" {
		b := make([]byte, 6)
		rand.Read(b)
		d.ID = hex.EncodeToString(b)
	}
	if d.Title == "" || !json.Valid(d.Spec) {
		fail(w, 400, errStr("title and a JSON spec are required"))
		return
	}
	d.Provisioned = false // editing in the UI detaches a dashboard from its file
	if err := a.Store.PutDashboard(&d); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, d)
}

func (a *API) deleteDashboard(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.DeleteDashboard(r.PathValue("id")); err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ProvisionDashboards loads dashboards-as-code: every *.json file in dir is
// upserted, with the file name as its id. A dashboard that was edited in the
// UI since (no longer marked provisioned) is left alone.
func ProvisionDashboards(st *store.Store, dir string) (int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return n, err
		}
		var doc struct {
			Title string          `json:"title"`
			Spec  json.RawMessage `json:"spec"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || doc.Title == "" || len(doc.Spec) == 0 {
			return n, errStr("invalid dashboard file " + f)
		}
		id := strings.TrimSuffix(filepath.Base(f), ".json")
		if cur, _ := st.GetDashboard(id); cur != nil && !cur.Provisioned {
			continue
		}
		if err := st.PutDashboard(&store.Dashboard{ID: id, Title: doc.Title, Spec: doc.Spec, Provisioned: true}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
