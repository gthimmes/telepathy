# Architecture

This describes what is built today (M0 + M1) and where each piece is meant to go.
See [PRODUCT_SPEC.md](PRODUCT_SPEC.md) for the why.

## Overview

```
  OTel SDKs / Collectors            CI/CD                Browser / agents
     │ OTLP/HTTP :4318                │ POST /api/v1/events      │
     │ OTLP/gRPC :4317                │                          │
     ▼                                ▼                          ▼
 ┌────────────────────────────────────────────────────────────────────┐
 │ telepathy serve                                                     │
 │                internal/auth  (sessions, API tokens, roles)         │
 │                                                                    │
 │  internal/ingest ──normalize──▶ internal/store ◀── internal/query  │
 │  OTLP → Span/Log/MetricPoint     SQLite (WAL)      filter language  │
 │  service · location · env             ▲            time series      │
 │                                       │            health · explain │
 │  internal/alert ── every 30 s ────────┘            service map      │
 │  monitors → alerts → Slack / PagerDuty / webhook        ▲           │
 │                                   internal/api ─────────┘           │
 │                                   internal/ui  (embedded React app) │
 └────────────────────────────────────────────────────────────────────┘
```

| Package | Responsibility |
|---|---|
| `cmd/telepathy` | CLI: `serve`, `demo`, `bench`, `version`; flags, retention loop, shutdown |
| `internal/ingest` | OTLP/HTTP (protobuf + JSON, gzip) and OTLP/gRPC, resource → dimensions, write |
| `internal/store` | Schema, batched inserts, dashboards, events, tokens, monitors, channels, alerts, retention |
| `internal/query` | Filter parser/compiler, time-series engine, health, Explain, service map, search |
| `internal/auth` | Admin sign-in, sessions, API tokens, per-route roles |
| `internal/alert` | Monitor evaluation, alert lifecycle, root-cause grouping, notifications |
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
| `monitors` | `id`, `name`, `kind` (health \| query), `spec` (JSON), `enabled` |
| `alerts` | `monitor_id`, `key`, `status` (firing \| resolved), `severity`, `started_ms`, `resolved_ms`, `details` (JSON evidence) |
| `channels` | `name`, `kind` (slack \| pagerduty \| webhook), `url` |
| `tokens` | `name`, `role`, `hash` (SHA-256 of the secret), `prefix` |

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

One shape for the explorer, dashboard widgets and monitors:

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

### Service map

Nothing is declared. An edge A → B exists wherever a server or consumer span
of B has a parent span that belongs to A (a self-join on `trace_id`,
`parent_id`). Each edge carries call rate, error rate and p95 as seen by the
callee; each node carries its entry-span rate, errors, p95 and its worst
health status. A location filter restricts both to calls handled there. Edge
figures are computed from at most the 100,000 most recent calls in the window.

## Alerting

The engine evaluates every enabled monitor each 30 seconds.

| Monitor | Condition | Subject (one alert each) |
|---|---|---|
| `health` | A cell of the health matrix has one of the configured statuses (default: critical, degraded, silent); optional service/location globs | application × location |
| `query` | A query object's aggregate over `window` compares true against `threshold` | each series of the query |

A default health monitor is created on first start, so alerting needs no setup.

**Lifecycle.** A condition must hold for `forEvals` consecutive evaluations
(default 2) before an alert fires, and be absent for as many before it
resolves — one noisy window neither pages nor un-pages anyone. A query that
errors leaves existing alerts untouched rather than resolving them. Alerts of
a deleted or disabled monitor end without notification.

**Evidence.** When an alert opens, the engine runs Explain on its scope and
stores the top findings and the related change events with the alert. For a
silent cell (nothing to compare) it attaches recent changes to that service.

**Root-cause grouping.** Within a location, if an alerting service's calls to
another alerting service are failing (edge error rate ≥ 1%) or that callee is
slow, the caller is following the callee down. The engine walks that chain to
its end, marks every caller as *downstream of* the root, lists them on the
root alert as *also affected*, and notifies only for the root.

**Notifications.** Every alert event (firing, escalated to critical, resolved)
goes to every channel: Slack incoming webhooks (text with trigger, findings
and a link), PagerDuty Events API v2 (trigger/resolve with a stable dedup key)
and a generic JSON webhook. Delivery is attempted twice; a failure is recorded
on the alert and shown in the UI, never by echoing the channel URL.

## Authentication

Off until `TELEPATHY_ADMIN_PASSWORD` is set. With it on:

- **People** sign in with the admin password and receive a session cookie
  (`HttpOnly`, `SameSite=Strict`, 7 days, held in memory — a restart signs
  everyone out). Failed sign-ins are throttled globally. Cookie-authenticated
  writes whose `Origin` does not match the host are refused.
- **Programs** present an API token (`tp_…`) as `Authorization: Bearer`, over
  HTTP or as gRPC metadata. Only the SHA-256 of a token is stored; the secret
  is shown once.

| Role | Routes |
|---|---|
| `ingest` | `POST /v1/*`, OTLP/gRPC, `POST /api/v1/events` |
| `read` | every `GET /api/v1/*` except tokens and channels; `POST /api/v1/query`, `/explain` |
| `admin` | everything (a signed-in person is an admin) |

Public without credentials: the web app shell (static files, no data),
`/api/v1/status`, `/api/v1/session`, `/api/v1/login`, `/api/v1/logout`.
Channel destinations are write-only: the API returns them masked.

There is one admin identity and no TLS termination in the binary; run it
behind a reverse proxy for TLS. Per-user accounts, OIDC and RBAC are M2.

## HTTP API

| Method & path | Purpose |
|---|---|
| `POST /v1/traces` · `/v1/logs` · `/v1/metrics` | OTLP/HTTP ingest (OTLP/gRPC is on its own port) |
| `GET /api/v1/health?window=5m` | Health matrix |
| `POST /api/v1/query` | Time-series query |
| `POST /api/v1/explain` | Differential analysis |
| `GET /api/v1/spans?filter=&range=&order=&limit=` | Span search |
| `GET /api/v1/logs?filter=&range=&limit=` | Log search |
| `GET /api/v1/traces/{id}` | Spans + logs of a trace |
| `GET` / `POST /api/v1/events` | Change events |
| `GET` / `POST /api/v1/dashboards`, `GET` / `PUT` / `DELETE /api/v1/dashboards/{id}` | Dashboards |
| `GET /api/v1/servicemap?range=&location=` | Service dependency graph |
| `GET /api/v1/alerts` | Firing and recently resolved alerts, with evidence |
| `GET` / `POST /api/v1/monitors`, `PUT` / `DELETE /api/v1/monitors/{id}` | Monitors |
| `GET` / `POST /api/v1/channels`, `DELETE /api/v1/channels/{id}`, `POST /api/v1/channels/{id}/test` | Notification channels |
| `GET /api/v1/session`, `POST /api/v1/login`, `POST /api/v1/logout` | Sign-in |
| `GET` / `POST /api/v1/tokens`, `DELETE /api/v1/tokens/{id}` | API tokens |
| `GET /api/v1/meta` · `/api/v1/status` | Known services, locations, metrics · version |

## Measured performance

`telepathy bench --history 2h --rate 20` on a developer laptop (Windows 11,
NVMe SSD), one run, numbers rounded. It sends simulated telemetry through the
real OTLP/HTTP path into a fresh database, then times queries over all of it.

| Ingest | |
|---|---|
| Volume | 504k spans, 10k logs, 35k metric points in 45 s |
| Throughput | about 12,000 records/s (11,000 spans/s) |
| Disk | 275 MB, about 500 bytes per record |

| Query over the full 2 h (504k spans) | Median of 5 |
|---|---|
| Health matrix (always the last 65 min) | 0.53 s |
| Request rate by service | 1.0 s |
| p95 latency by location, one service | 0.10 s |
| Error rate split by a custom attribute | 0.31 s |
| Metric average by location | 0.03 s |
| Explain (errors, one service) | 0.65 s |
| Service map (edges from the most recent 100k calls) | 2.5 s |
| Span search by attribute, newest 100 | 0.01 s |
| Log search, newest 200 | < 0.01 s |

Read these as the shape of the embedded store, not as a capacity claim: one
machine, one run, synthetic data, a single writer. Ingest speed depends
heavily on the disk's sync latency (a slower drive on the same machine was
several times slower). Aggregations scan the rows in the query window, so
latency grows linearly with spans × window — fine for a team, the reason a
columnar backend is the first item of M2.

## Known limits (deliberate)

- **Storage is SQLite.** Right for a laptop, a small team or an edge site;
  percentiles, Explain and the service map scan rows in the query window (see
  the measurements above). The query layer only depends on the schema above,
  so M2 adds a columnar implementation (quantile sketches, materialized RED
  rollups) without changing ingest or API.
- **One admin, no TLS in the binary.** See Authentication.
- **Every alert goes to every channel.** No routing, silences or escalation
  policies yet; monitors are created through the UI or API, not from files.
- **Root-cause grouping is a heuristic** over observed call edges in one
  location. A caller with an independent problem at the same time as its
  dependency would be recorded as downstream (it is still listed, with its
  own evidence, just not notified).
- **Metrics are stored as points.** Delta sums and gauges aggregate correctly;
  cumulative counters need a rate function, and histograms keep count/sum but
  not buckets.
- **Single node.**
