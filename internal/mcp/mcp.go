// Package mcp exposes Telepathy to AI agents over the Model Context Protocol
// (Streamable HTTP transport, JSON responses, no server-initiated stream).
//
// Every tool is read-only and maps onto the same query layer the UI uses, so
// an agent can do what an on-call engineer does: look at health and alerts,
// ask Explain what is different, then drill into time series, spans and logs.
package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gthimmes/telepathy/internal/query"
	"github.com/gthimmes/telepathy/internal/store"
)

// Protocol revisions this server speaks; the first is offered to unknown clients.
var protocols = []string{"2025-06-18", "2025-11-25", "2025-03-26", "2024-11-05"}

type Server struct {
	Store   *store.Store
	Version string
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const instructions = `Telepathy is an observability backend (traces, logs, metrics, change events) organized by two first-class dimensions: service and location.

Suggested investigation order: get_health or list_alerts to see what is wrong -> explain to learn what is different about the failing or slow requests and which deploy preceded it -> query_timeseries, search_spans, search_logs and get_trace to confirm.

Filter language (clauses are ANDed): service:checkout location:eu-* status:error duration>500ms kind:server -http.route:/healthz "free text". Any other key is looked up in span/log attributes, e.g. payment.provider:adyen. Values may use * wildcards. Time ranges are strings such as 15m, 1h, 6h, 24h.`

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
	run         func(s *Server, args json.RawMessage) (any, error)
}

func schema(required []string, props map[string]any) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

const (
	rangeDesc  = "How far back to look, e.g. 15m, 1h, 6h, 24h. Default 1h."
	filterDesc = "Filter expression, e.g. service:checkout location:eu-central status:error. Empty = everything."
)

func timeRange(rng string) (query.TimeRange, error) {
	return query.ResolveRange(0, 0, rng, time.Hour)
}

var tools = []tool{
	{
		Name:        "get_health",
		Description: "Health of every service at every location, each compared with its own recent baseline. Returns the cells that are not healthy with the reasons, plus counts. Start here.",
		InputSchema: schema(nil, map[string]any{"include_healthy": map[string]any{"type": "boolean", "description": "Also return healthy cells with their rate, error rate and p95. Default false."}}),
		run: func(s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				IncludeHealthy bool `json:"include_healthy"`
			}
			json.Unmarshal(raw, &a)
			rep, err := query.Health(s.Store.DB(), 5*time.Minute)
			if err != nil {
				return nil, err
			}
			cells := []query.Cell{}
			for _, c := range rep.Cells {
				if a.IncludeHealthy || c.Status != query.Healthy {
					cells = append(cells, c)
				}
			}
			return map[string]any{"summary": rep.Summary, "windowSec": rep.WindowSec, "services": rep.Services, "locations": rep.Locations, "cells": cells}, nil
		},
	},
	{
		Name:        "list_alerts",
		Description: "Alerts that are firing now and those recently resolved. Each carries its evidence: what is over-represented among the affected requests, the change that likely triggered it, and whether it is downstream of another service.",
		InputSchema: schema(nil, map[string]any{"include_resolved": map[string]any{"type": "boolean", "description": "Include the 20 most recently resolved alerts. Default false."}}),
		run: func(s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				IncludeResolved bool `json:"include_resolved"`
			}
			json.Unmarshal(raw, &a)
			firing, err := s.Store.FiringAlerts()
			if err != nil {
				return nil, err
			}
			out := map[string]any{"firing": firing}
			if a.IncludeResolved {
				recent, err := s.Store.RecentAlerts(20)
				if err != nil {
					return nil, err
				}
				out["recentlyResolved"] = recent
			}
			return out, nil
		},
	},
	{
		Name:        "explain",
		Description: "Differential analysis: compares failing (mode=errors) or slowest (mode=slow) requests in a scope with the rest and returns the attribute values that are over-represented (e.g. one location, one version, one payment provider), plus recent deploys and other changes that match. Use this to find the cause of a problem.",
		InputSchema: schema([]string{"scope"}, map[string]any{
			"scope": str("Filter selecting the population to analyze, e.g. service:payments location:eu-central."),
			"mode":  map[string]any{"type": "string", "enum": []string{"errors", "slow"}, "description": "errors (default): what distinguishes failed requests. slow: what distinguishes the slowest."},
			"range": str("How far back to look. Default 15m."),
		}),
		run: func(s *Server, raw json.RawMessage) (any, error) {
			var a struct{ Scope, Mode, Range string }
			json.Unmarshal(raw, &a)
			if a.Range == "" {
				a.Range = "15m"
			}
			return query.Explain(s.Store.DB(), s.Store, query.ExplainRequest{Scope: a.Scope, Mode: a.Mode, Range: a.Range})
		},
	},
	{
		Name:        "query_timeseries",
		Description: "Aggregate spans, logs or metrics over time, optionally split into series. Examples: p95 latency of checkout by location; error_rate by service; avg of a metric by location. Durations are in milliseconds, error rates in percent.",
		InputSchema: schema([]string{"agg"}, map[string]any{
			"signal":   map[string]any{"type": "string", "enum": []string{"spans", "logs", "metrics"}, "description": "Default spans."},
			"metric":   str("Metric name; required when signal is metrics (see get_metadata)."),
			"agg":      map[string]any{"type": "string", "enum": []string{"count", "rate", "error_rate", "avg", "sum", "min", "max", "p50", "p90", "p95", "p99"}},
			"filter":   str(filterDesc),
			"group_by": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Fields to split by, e.g. [\"location\"] or [\"service\",\"location\"] or an attribute key."},
			"range":    str(rangeDesc),
			"points":   num("Approximate number of time buckets to return (5-120). Default 30."),
		}),
		run: func(s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Signal, Metric, Agg, Filter, Range string
				GroupBy                            []string `json:"group_by"`
				Points                             int
			}
			json.Unmarshal(raw, &a)
			if a.Signal == "" {
				a.Signal = query.Spans
			}
			if a.Range == "" {
				a.Range = "1h"
			}
			if a.Points < 5 || a.Points > 120 {
				a.Points = 30
			}
			d, err := query.ParseRange(a.Range)
			if err != nil {
				return nil, err
			}
			step := int(d.Seconds()) / a.Points
			if step < 1 {
				step = 1
			}
			return query.Run(s.Store.DB(), query.Request{Signal: a.Signal, Metric: a.Metric, Agg: a.Agg, Filter: a.Filter, GroupBy: a.GroupBy, Range: a.Range, Step: step, Limit: 20})
		},
	},
	{
		Name:        "search_spans",
		Description: "Find individual spans (requests and the operations inside them) matching a filter. Returns newest first, or slowest first with order=slowest. Use a returned traceId with get_trace.",
		InputSchema: schema(nil, map[string]any{
			"filter": str(filterDesc),
			"range":  str(rangeDesc),
			"order":  map[string]any{"type": "string", "enum": []string{"newest", "slowest"}},
			"limit":  num("Maximum spans to return (1-100). Default 20."),
		}),
		run: func(s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Filter, Range, Order string
				Limit                int
			}
			json.Unmarshal(raw, &a)
			tr, err := timeRange(a.Range)
			if err != nil {
				return nil, err
			}
			if a.Limit < 1 || a.Limit > 100 {
				a.Limit = 20
			}
			order := ""
			if a.Order == "slowest" {
				order = "slow"
			}
			spans, err := query.SearchSpans(s.Store.DB(), a.Filter, tr, order, a.Limit)
			if err != nil {
				return nil, err
			}
			return map[string]any{"spans": spans}, nil
		},
	},
	{
		Name:        "search_logs",
		Description: "Find log records matching a filter, newest first. Bare words match the log body; severity:error selects by level.",
		InputSchema: schema(nil, map[string]any{
			"filter": str("Filter expression, e.g. service:payments severity:error \"declined\"."),
			"range":  str(rangeDesc),
			"limit":  num("Maximum records to return (1-200). Default 50."),
		}),
		run: func(s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Filter, Range string
				Limit         int
			}
			json.Unmarshal(raw, &a)
			tr, err := timeRange(a.Range)
			if err != nil {
				return nil, err
			}
			if a.Limit < 1 || a.Limit > 200 {
				a.Limit = 50
			}
			logs, err := query.SearchLogs(s.Store.DB(), a.Filter, tr, a.Limit)
			if err != nil {
				return nil, err
			}
			return map[string]any{"logs": logs}, nil
		},
	},
	{
		Name:        "get_trace",
		Description: "Every span of one trace, with the logs emitted inside it.",
		InputSchema: schema([]string{"trace_id"}, map[string]any{"trace_id": str("Trace ID, from search_spans or a log record.")}),
		run: func(s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				TraceID string `json:"trace_id"`
			}
			json.Unmarshal(raw, &a)
			t, err := query.GetTrace(s.Store.DB(), a.TraceID)
			if err != nil {
				return nil, err
			}
			if len(t.Spans) == 0 {
				return nil, fmt.Errorf("trace %q not found", a.TraceID)
			}
			return t, nil
		},
	},
	{
		Name:        "get_service_map",
		Description: "Which services call which, derived from traces, with call rate, error rate and p95 latency on every service and every call edge. Use it to tell a root cause from its downstream symptoms.",
		InputSchema: schema(nil, map[string]any{"range": str(rangeDesc), "location": str("Restrict to calls handled in one location.")}),
		run: func(s *Server, raw json.RawMessage) (any, error) {
			var a struct{ Range, Location string }
			json.Unmarshal(raw, &a)
			tr, err := timeRange(a.Range)
			if err != nil {
				return nil, err
			}
			return query.ServiceMap(s.Store.DB(), tr, a.Location)
		},
	},
	{
		Name:        "list_changes",
		Description: "Change events (deploys, config changes, feature flags, scaling) in a time range, newest first. Most incidents start with one.",
		InputSchema: schema(nil, map[string]any{"range": str(rangeDesc)}),
		run: func(s *Server, raw json.RawMessage) (any, error) {
			var a struct{ Range string }
			json.Unmarshal(raw, &a)
			tr, err := timeRange(a.Range)
			if err != nil {
				return nil, err
			}
			evs, err := s.Store.Events(tr.FromNs, tr.ToNs)
			if err != nil {
				return nil, err
			}
			return map[string]any{"changes": evs}, nil
		},
	},
	{
		Name:        "list_slos",
		Description: "Service level objectives with their current standing: the measured percentage, how much error budget is left, burn rates (1 = spending exactly the budget) and a per-location breakdown, worst first.",
		InputSchema: schema(nil, map[string]any{}),
		run: func(s *Server, raw json.RawMessage) (any, error) {
			reps, err := query.SLOs(s.Store.DB(), s.Store)
			if err != nil {
				return nil, err
			}
			for i := range reps { // the hourly history is for charts
				reps[i].Timestamps, reps[i].Budget = nil, nil
			}
			return map[string]any{"slos": reps}, nil
		},
	},
	{
		Name:        "get_metadata",
		Description: "The service names, location names and metric names seen in the last 24 hours. Use it to learn valid values for filters.",
		InputSchema: schema(nil, map[string]any{}),
		run: func(s *Server, raw json.RawMessage) (any, error) {
			return query.GetMeta(s.Store.DB(), time.Now().Add(-24*time.Hour).UnixNano())
		},
	},
}

func init() {
	for i := range tools {
		tools[i].Annotations = map[string]any{"readOnlyHint": true, "openWorldHint": false}
	}
}

func (s *Server) dispatch(req *rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := protocols[0]
		for _, v := range protocols {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "telepathy", "title": "Telepathy", "version": s.Version},
			"instructions":    instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		for _, t := range tools {
			if t.Name != p.Name {
				continue
			}
			if len(p.Arguments) == 0 {
				p.Arguments = json.RawMessage("{}")
			}
			// Tool failures are results the model can read and correct, not protocol errors.
			res, err := t.run(s, p.Arguments)
			if err != nil {
				return toolText(err.Error(), true), nil
			}
			b, err := json.Marshal(res)
			if err != nil {
				return toolText(err.Error(), true), nil
			}
			return toolText(string(b), false), nil
		}
		return nil, &rpcError{-32602, "unknown tool: " + p.Name}
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

func toolText(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

// ServeHTTP handles one JSON-RPC message per POST.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Browsers must not reach a local server from another site (DNS rebinding).
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || u.Host != r.Host {
			http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
			return
		}
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "this endpoint accepts JSON-RPC over POST; it offers no event stream", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, "expected a single JSON-RPC 2.0 request"}})
		return
	}
	if len(req.ID) == 0 { // a notification: nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	res, rerr := s.dispatch(&req)
	json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: res, Error: rerr})
}
