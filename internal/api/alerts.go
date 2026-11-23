package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gthimmes/telepathy/internal/alert"
	"github.com/gthimmes/telepathy/internal/query"
	"github.com/gthimmes/telepathy/internal/store"
)

func (a *API) registerM1(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/servicemap", a.serviceMap)
	mux.HandleFunc("GET /api/v1/alerts", a.alerts)
	mux.HandleFunc("GET /api/v1/monitors", a.listMonitors)
	mux.HandleFunc("POST /api/v1/monitors", a.putMonitor)
	mux.HandleFunc("PUT /api/v1/monitors/{id}", a.putMonitor)
	mux.HandleFunc("DELETE /api/v1/monitors/{id}", a.deleteMonitor)
	mux.HandleFunc("GET /api/v1/channels", a.listChannels)
	mux.HandleFunc("POST /api/v1/channels", a.createChannel)
	mux.HandleFunc("DELETE /api/v1/channels/{id}", a.deleteChannel)
	mux.HandleFunc("POST /api/v1/channels/{id}/test", a.testChannel)
}

func (a *API) serviceMap(w http.ResponseWriter, r *http.Request) {
	tr, err := rangeOf(r, time.Hour)
	if err != nil {
		fail(w, 400, err)
		return
	}
	res, err := query.ServiceMap(a.Store.DB(), tr, r.URL.Query().Get("location"))
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, res)
}

func (a *API) alerts(w http.ResponseWriter, r *http.Request) {
	firing, err := a.Store.FiringAlerts()
	if err != nil {
		fail(w, 500, err)
		return
	}
	recent, err := a.Store.RecentAlerts(50)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"firing": firing, "recent": recent})
}

func (a *API) listMonitors(w http.ResponseWriter, r *http.Request) {
	ms, err := a.Store.ListMonitors()
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"monitors": ms})
}

func (a *API) putMonitor(w http.ResponseWriter, r *http.Request) {
	var m store.Monitor
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		fail(w, 400, err)
		return
	}
	m.ID = r.PathValue("id") // empty on POST → generated
	if err := alert.ValidateMonitor(a.Store, &m); err != nil {
		fail(w, 400, err)
		return
	}
	if err := a.Store.PutMonitor(&m); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, m)
}

func (a *API) deleteMonitor(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.DeleteMonitor(r.PathValue("id")); err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maskChannel hides the secret part of a channel's destination.
func maskChannel(c store.Channel) store.Channel {
	if c.Kind == "pagerduty" {
		if n := len(c.URL); n > 4 {
			c.URL = "…" + c.URL[n-4:]
		} else {
			c.URL = "…"
		}
		return c
	}
	if u, err := url.Parse(c.URL); err == nil && u.Host != "" {
		c.URL = u.Scheme + "://" + u.Host + "/…"
	} else {
		c.URL = "…"
	}
	return c
}

func (a *API) listChannels(w http.ResponseWriter, r *http.Request) {
	cs, err := a.Store.ListChannels()
	if err != nil {
		fail(w, 500, err)
		return
	}
	for i := range cs {
		cs[i] = maskChannel(cs[i])
	}
	writeJSON(w, 200, map[string]any{"channels": cs})
}

func (a *API) createChannel(w http.ResponseWriter, r *http.Request) {
	var c store.Channel
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		fail(w, 400, err)
		return
	}
	c.Name, c.URL = strings.TrimSpace(c.Name), strings.TrimSpace(c.URL)
	switch c.Kind {
	case "webhook", "slack":
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fail(w, 400, errStr("an http(s) URL is required"))
			return
		}
	case "pagerduty":
		if len(c.URL) < 16 || strings.ContainsAny(c.URL, " /:") {
			fail(w, 400, errStr("a PagerDuty Events API v2 routing (integration) key is required"))
			return
		}
	default:
		fail(w, 400, errStr("kind must be webhook, slack or pagerduty"))
		return
	}
	c.Services, c.Locations = strings.TrimSpace(c.Services), strings.TrimSpace(c.Locations)
	if !store.ValidGlobs(c.Services) || !store.ValidGlobs(c.Locations) {
		fail(w, 400, errStr("invalid service or location pattern"))
		return
	}
	if c.Severity != "" && c.Severity != "critical" {
		fail(w, 400, errStr("severity must be empty (all alerts) or critical"))
		return
	}
	if c.Name == "" {
		c.Name = c.Kind
	}
	if err := a.Store.CreateChannel(&c); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 201, maskChannel(c))
}

func (a *API) deleteChannel(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.DeleteChannel(r.PathValue("id")); err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testChannel sends a clearly-labelled sample alert so a channel can be verified.
func (a *API) testChannel(w http.ResponseWriter, r *http.Request) {
	if a.Alerts == nil {
		fail(w, 503, errStr("alerting is not running"))
		return
	}
	cs, err := a.Store.ListChannels()
	if err != nil {
		fail(w, 500, err)
		return
	}
	for _, c := range cs {
		if c.ID != r.PathValue("id") {
			continue
		}
		now := time.Now().UnixMilli()
		details, _ := json.Marshal(alert.Details{Scope: "service:example", Reasons: []string{"this is a test notification"}})
		sample := &store.Alert{ID: now, MonitorName: "Test", Key: "test", Status: store.AlertFiring, Severity: query.Degraded,
			Title: "Test notification from Telepathy", Summary: "If you can read this, the channel works.",
			Service: "example", StartedMs: now, UpdatedMs: now, Details: details}
		if err := a.Alerts.Send(c, "firing", sample); err != nil {
			fail(w, 502, err)
			return
		}
		if c.Kind == "pagerduty" { // do not leave a test incident open
			sample.ResolvedMs = now
			a.Alerts.Send(c, "resolved", sample)
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	fail(w, 404, errStr("channel not found"))
}
