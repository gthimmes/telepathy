package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gthimmes/telepathy/internal/mcp"
	"github.com/gthimmes/telepathy/internal/query"
	"github.com/gthimmes/telepathy/internal/store"
)

func (a *API) registerM2(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/slos", a.listSLOs)
	mux.HandleFunc("POST /api/v1/slos", a.putSLO)
	mux.HandleFunc("PUT /api/v1/slos/{id}", a.putSLO)
	mux.HandleFunc("DELETE /api/v1/slos/{id}", a.deleteSLO)
	mux.HandleFunc("GET /api/v1/silences", a.listSilences)
	mux.HandleFunc("POST /api/v1/silences", a.createSilence)
	mux.HandleFunc("DELETE /api/v1/silences/{id}", a.deleteSilence)
	mux.Handle("/api/v1/mcp", &mcp.Server{Store: a.Store, Version: a.Version})
}

func (a *API) listSLOs(w http.ResponseWriter, r *http.Request) {
	reps, err := query.SLOs(a.Store.DB(), a.Store)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"slos": reps})
}

func (a *API) putSLO(w http.ResponseWriter, r *http.Request) {
	var x store.SLO
	if err := json.NewDecoder(r.Body).Decode(&x); err != nil {
		fail(w, 400, err)
		return
	}
	x.ID, x.CreatedMs = r.PathValue("id"), 0
	if err := query.ValidateSLO(&x); err != nil {
		fail(w, 400, err)
		return
	}
	if err := a.Store.PutSLO(&x); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, x)
}

func (a *API) deleteSLO(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.DeleteSLO(r.PathValue("id")); err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listSilences(w http.ResponseWriter, r *http.Request) {
	xs, err := a.Store.ListSilences(time.Now().UnixMilli())
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"silences": xs})
}

// createSilence accepts either endsMs or a duration such as "2h".
func (a *API) createSilence(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.Silence
		Duration string `json:"duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, err)
		return
	}
	x := in.Silence
	now := time.Now().UnixMilli()
	if x.StartsMs == 0 {
		x.StartsMs = now
	}
	if in.Duration != "" {
		d, err := query.ParseRange(in.Duration)
		if err != nil {
			fail(w, 400, err)
			return
		}
		x.EndsMs = x.StartsMs + d.Milliseconds()
	}
	x.Service, x.Location, x.Reason = strings.TrimSpace(x.Service), strings.TrimSpace(x.Location), strings.TrimSpace(x.Reason)
	switch {
	case x.EndsMs <= now || x.EndsMs <= x.StartsMs:
		fail(w, 400, errStr("a silence must end in the future: give a duration such as 2h"))
		return
	case x.EndsMs-x.StartsMs > 30*86400_000:
		fail(w, 400, errStr("a silence can last at most 30 days"))
		return
	case !store.ValidGlobs(x.Service) || !store.ValidGlobs(x.Location):
		fail(w, 400, errStr("invalid service or location pattern"))
		return
	}
	if err := a.Store.CreateSilence(&x); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 201, x)
}

func (a *API) deleteSilence(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.DeleteSilence(r.PathValue("id")); err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
