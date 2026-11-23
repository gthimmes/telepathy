# Architecture

This describes what is built today (M0, M1 and part of M2) and where each piece is meant to go.
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
| `internal/store` | Schema, batched inserts, request rollups, dashboards, events, tokens, monitors, channels, silences, SLOs, alerts, retention |
| `internal/query` | Filter parser/compiler, time-series engine, health, SLOs, Explain, service map, search |
| `internal/auth` | Admin sign-in, sessions, API tokens, per-route roles |
| `internal/alert` | Monitor evaluation, alert lifecycle, root-cause grouping, routing, silences, notifications |
| `internal/mcp` | Model Context Protocol server: read-only tools for AI agents |
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
| `rollups` | `step` (60 or 3600 s), `ts`, `service`, `location`, `n`, `errs`, `hist` (latency histogram) |
| `slos` | `id`, `spec` (JSON: service, location globs, kind, threshold, target, window) |
| `silences` | `service`, `location` (globs), `reason`, `starts_ms`, `ends_ms` |

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

### Request rollups

When spans are inserted, the same transaction updates one row per
service × location × minute, and one per hour, for every *request* (a server
or consumer span, or a root span): request count, error count and a latency
histogram. The histogram is log-scale with 4% buckets (DDSketch style), so
histograms merge by addition and any quantile read from a merge is within
about 2% of the true value.

Health, SLOs and burn rates read rollups instead of spans. Their cost depends
on services × locations × time buckets, not on traffic, and they keep working
after the raw spans have aged out: minute rollups are kept 4 days, hour
rollups 400 days, independent of `--retention`. A database written by an
earlier version is summarized once at startup (minutes, for a database of
several gigabytes).

### Health

For each (service, location), from the minute rollups:

1. Split the last 13 × *window* (default 5 min) into slices; the last is "now"
   and also includes the minute in progress.
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
| `slo` | An SLO's burn rate is ≥ 14.4 over 1 h and 5 min (critical), or ≥ 6 over 6 h and 30 min (degraded), with at least 20 requests in the last hour | each SLO |

A default health monitor and a default SLO monitor are created on first start,
so alerting needs no setup. An SLO alert for a service that already has
another firing alert is recorded as *the same incident* and does not notify.

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

**Routing and silences.** A channel may carry service globs, location globs
and a critical-only flag; an alert goes to the channels it matches, and a
channel with none of these receives everything. A silence (service and
location globs, a reason, a time window) suppresses notifications — firing and
resolved — for the alerts it covers; the alert is still recorded, with the
silence's reason.

**Notifications.** Every alert event (firing, escalated to critical, resolved)
goes to every matching channel: Slack incoming webhooks (text with trigger, findings
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
| `read` | every `GET /api/v1/*` except tokens and channels; `POST /api/v1/query`, `/explain`, `/mcp` |
| `admin` | everything (a signed-in person is an admin) |

Public without credentials: the web app shell (static files, no data),
`/api/v1/status`, `/api/v1/session`, `/api/v1/login`, `/api/v1/logout`.
Channel destinations are write-only: the API returns them masked.

There is one admin identity and no TLS termination in the binary; run it
behind a reverse proxy for TLS. Per-user accounts, OIDC and RBAC are M2.

## SLOs

An SLO names a service, optional location globs, an objective (requests
succeed, or requests finish within a threshold), a target percentage and a
rolling window of 1–90 days. From the rollups:

- **Measured** = good ÷ total requests in the window (hour rollups). For a
  latency objective, good is the histogram count at or below the threshold.
- **Error budget left** = 1 − bad ÷ ((1 − target) × total). It is negative
  once the budget is overspent.
- **Burn rate** over a period = that period's bad share ÷ (1 − target), from
  minute rollups: 1 spends the budget exactly over the window.
- **Per location**: the same figures for each location, each against its own
  traffic, sorted worst first.

## MCP

`POST /api/v1/mcp` speaks the Model Context Protocol's Streamable HTTP
transport with one JSON-RPC request per POST and a JSON response;
notifications are answered with 202. It implements `initialize`, `ping`,
`tools/list` and `tools/call`. Every tool is read-only and calls the same
query functions as the HTTP API. A failing tool call returns a result with
`isError`, so the model can read the message and correct itself. Requests
carrying an `Origin` header from another host are refused. With authentication
on, the endpoint needs the `read` role.

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
| `GET` / `POST /api/v1/slos`, `PUT` / `DELETE /api/v1/slos/{id}` | SLOs with their current standing |
| `GET` / `POST /api/v1/silences`, `DELETE /api/v1/silences/{id}` | Silences (`duration` such as `2h`, or `endsMs`) |
| `POST /api/v1/mcp` | Model Context Protocol endpoint (JSON-RPC) |
| `GET /api/v1/meta` · `/api/v1/status` | Known services, locations, metrics · version |

## Measured performance

`telepathy bench --history 2h --rate 20` on a developer laptop (Windows 11,
NVMe SSD), one run, numbers rounded. It sends simulated telemetry through the
real OTLP/HTTP path into a fresh database, then times queries over all of it.

| Ingest | |
|---|---|
| Volume | 505k spans, 11k logs, 35k metric points in 53 s |
| Throughput | about 10,400 records/s (9,500 spans/s), rollups included |
| Disk | 276 MB, about 500 bytes per record |

| Query over the full 2 h (505k spans) | Median of 5 |
|---|---|
| Health matrix (always the last 65 min, from rollups) | 0.01 s |
| Request rate by service | 1.0 s |
| p95 latency by location, one service | 0.10 s |
| Error rate split by a custom attribute | 0.31 s |
| Metric average by location | 0.03 s |
| Explain (errors, one service) | 0.71 s |
| Service map (edges from the most recent 100k calls) | 2.1 s |
| Span search by attribute, newest 100 | 0.01 s |
| Log search, newest 200 | < 0.01 s |

Read these as the shape of the embedded store, not as a capacity claim: one
machine, one run, synthetic data, a single writer. Ingest speed depends
heavily on the disk's sync latency (a slower drive on the same machine was
several times slower). Before rollups existed the same run ingested about
12,000 records/s and the health matrix took 0.53 s; with noise between runs,
rollups cost roughly a tenth of ingest throughput. On a separate demo
database holding 834k requests for a service, evaluating two 7- and 30-day
SLOs took 0.02 s. Ad-hoc aggregations still scan the rows in the query
window, so their latency grows linearly with spans × window — fine for a
team, the reason a columnar backend remains in M2.

## Known limits (deliberate)

- **Storage is SQLite.** Right for a laptop, a small team or an edge site.
  Health and SLOs read rollups; ad-hoc queries, Explain and the service map
  still scan rows in the query window (see the measurements above). The query
  layer only depends on the schema above, so a columnar implementation can
  replace it without changing ingest or API.
- **Rollups cover requests by service and location only.** A dashboard query
  with an arbitrary filter or grouping does not use them.
- **An SLO measures what is stored.** A new SLO over 30 days starts with
  however much history the rollups hold; a database upgraded from an earlier
  version has only as much as its raw-span retention kept.
- **One admin, no TLS in the binary.** See Authentication.
- **Routing is by service, location and severity.** No teams, schedules or
  escalation policies; monitors and SLOs are created through the UI or API,
  not from files.
- **MCP is tools only**, over plain JSON responses: no resources, prompts or
  server-initiated stream.
- **Root-cause grouping is a heuristic** over observed call edges in one
  location. A caller with an independent problem at the same time as its
  dependency would be recorded as downstream (it is still listed, with its
  own evidence, just not notified).
- **Metrics are stored as points.** Delta sums and gauges aggregate correctly;
  cumulative counters need a rate function, and histograms keep count/sum but
  not buckets.
- **Single node.**
