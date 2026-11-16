# Architecture

This describes what is built today (M0) and where each piece is meant to go.
See [PRODUCT_SPEC.md](PRODUCT_SPEC.md) for the why.

## Overview

```
  OTel SDKs / Collectors            CI/CD                Browser / agents
          │ OTLP/HTTP                 │ POST /api/v1/events      │
          ▼                           ▼                          ▼
 ┌────────────────────────────────────────────────────────────────────┐
 │ telepathy serve  (one process, one port: 4318)                      │
 │                                                                    │
 │  internal/ingest ──normalize──▶ internal/store ◀── internal/query  │
 │  OTLP → Span/Log/MetricPoint     SQLite (WAL)      filter language  │
 │  service · location · env                          time series      │
 │                                                    health           │
 │                                                    explain          │
 │                                   ▲                     ▲           │
 │                                   └──── internal/api ───┘           │
 │                                         JSON API                    │
 │                                   internal/ui  (embedded React app) │
 └────────────────────────────────────────────────────────────────────┘
```

| Package | Responsibility |
|---|---|
| `cmd/telepathy` | CLI: `serve`, `demo`, `version`; flags, retention loop, shutdown |
| `internal/ingest` | OTLP/HTTP decode (protobuf + JSON, gzip), resource → dimensions, write |
| `internal/store` | Schema, batched inserts, dashboards, events, retention |
| `internal/query` | Filter parser/compiler, time-series engine, health, Explain, search |
| `internal/api` | HTTP JSON API, dashboard provisioning |
| `internal/ui` | `go:embed` of the built web app |
| `internal/demo` | Simulated system that emits real OTLP |
| `web/` | React + TypeScript + Vite single-page app |

## Data model

Every signal carries the same three first-class dimensions — `service`,
`location`, `env` — and a JSON `attrs` bag holding the merged resource and
record attributes. Promoting the dimensions at ingest is what makes the
application × location matrix a cheap query instead of a join.

| Table | Key columns |
|---|---|
| `spans` | `ts_ns`, `trace_id`, `span_id`, `parent_id`, `name`, `kind`, `duration_ns`, `status` |
| `logs` | `ts_ns`, `severity`, `body`, `trace_id`, `span_id` |
| `metrics` | `ts_ns`, `name`, `kind`, `unit`, `value`, `count`, `sum` |
| `events` | `ts_ns`, `kind`, `title` — deploys, flags, config changes |
| `dashboards` | `id`, `title`, `spec` (JSON), `provisioned` |

**Location resolution.** The first resource attribute present from
`telepathy.location`, `location`, `site.id`, `cloud.region`,
`cloud.availability_zone`, `k8s.cluster.name` becomes the location (override
with `--location-keys`). Nothing matched → `default`.

**Normalization details.** `service.name` → `service`;
`deployment.environment(.name)` → `env`; boolean attributes are stored as
`"true"`/`"false"`; `exception.*` attributes on span events are promoted onto
the span so they are filterable and visible to Explain; OTLP/JSON hex ids are
recovered after protobuf-JSON base64 decoding.

## Query

### Filter language

Space-separated clauses, ANDed:

```
service:checkout          equality on a column or any attribute
location:eu-*             wildcard
-http.route:/healthz      negation
status:error              span status (ok | error | unset)
duration>500ms            numeric comparison (>, >=, <, <=); durations take ms/s/m
http.response.status_code>=500
level:error               log severity
"card declined"           free text (span name / log body / metric name)
```

Unknown keys resolve to `json_extract(attrs, '$."key"')`; the path is always a
bound parameter and keys are validated against `[A-Za-z0-9_.\-/]+`.

### The query object

One shape for the explorer, dashboard widgets and (next) monitors:

```json
{ "signal": "spans", "agg": "p95", "filter": "service:checkout kind:server",
  "groupBy": ["location"], "range": "1h" }
```

`signal`: `spans | logs | metrics` · `agg`: `count | rate | error_rate | avg |
sum | min | max | p50 | p90 | p95 | p99` · optional `metric`, `field`, `from`,
`to`, `step`, `limit`. The response has one aligned `timestamps` array, and per
series its `group`, `values` (null = no data) and `total` over the window.

### Health

For each (service, location), over server/consumer/root spans:

1. Split the last 13 × *window* (default 5 min) into slices; the last is "now".
2. Baseline = **median** of the per-slice rate, error rate and p95 over the 12
   preceding slices. The median keeps an earlier incident from normalizing a
   current one.
3. Status:
   - **critical** — error rate ≥ 5% and > 2× baseline, or p95 ≥ 4× baseline
   - **degraded** — error rate ≥ 1% and > 2× baseline (+0.5 pt), chronically ≥ 5%, p95 ≥ 2× baseline, or traffic < 30% of baseline
   - **silent** — traffic in the baseline, none now
   - minimum-volume guards (≥ 10 requests, ≥ 5 errors) suppress noise

### Explain

Input: a scope filter, a mode (`errors`, `slow` = above the scope's p95, or a
custom selection filter) and a window.

1. Sample up to 20k spans matching *scope ∧ selection* and 20k matching *scope ∧ ¬selection*.
2. Count every `(attribute, value)` pair in both sets, including first-class dimensions.
3. Score = share in selection − share in baseline (percentage points). Drop
   near-unique keys; keep the top values per key.
4. Attach change events from 30 minutes before the window onward; mark as
   *related* those whose service (and location, if set) dominate the selection.

## HTTP API

| Method & path | Purpose |
|---|---|
| `POST /v1/traces` · `/v1/logs` · `/v1/metrics` | OTLP/HTTP ingest |
| `GET /api/v1/health?window=5m` | Health matrix |
| `POST /api/v1/query` | Time-series query |
| `POST /api/v1/explain` | Differential analysis |
| `GET /api/v1/spans?filter=&range=&order=&limit=` | Span search |
| `GET /api/v1/logs?filter=&range=&limit=` | Log search |
| `GET /api/v1/traces/{id}` | Spans + logs of a trace |
| `GET` / `POST /api/v1/events` | Change events |
| `GET` / `POST /api/v1/dashboards`, `GET` / `PUT` / `DELETE /api/v1/dashboards/{id}` | Dashboards |
| `GET /api/v1/meta` · `/api/v1/status` | Known services, locations, metrics · version |

## Known limits of M0 (deliberate)

- **Storage is SQLite.** Right for a laptop, a small team or an edge site;
  percentiles and Explain scan rows in the query window. The query layer only
  depends on the schema above, so M1 adds a ClickHouse implementation
  (quantile sketches, materialized RED rollups) without changing ingest or API.
- **No authentication on the API/UI**, only an optional ingest key. Do not
  expose it to the internet yet.
- **Metrics are stored as points.** Delta sums and gauges aggregate correctly;
  cumulative counters need a rate function, and histograms keep count/sum but
  not buckets.
- **No OTLP/gRPC, no alerting, single node.**
