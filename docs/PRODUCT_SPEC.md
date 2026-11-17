# Telepathy — Product Specification

**Status:** v0.2 (living document) · **Last updated:** 2026-11-17

Telepathy is an observability platform: it collects telemetry from production
applications, shows the health of every application at every location, helps
people find out why something broke, and lets teams build their own dashboards.

The name is the product thesis: *you should know what your systems are
experiencing without having to interrogate them.*

---

## 1. Problem

Tools like Datadog, New Relic and Grafana solved "collect everything and chart
it." The problems that remain are the ones teams actually complain about:

| # | Pain today | What it costs |
|---|---|---|
| 1 | **Three products in a trench coat.** Metrics, logs and traces live in separate stores with separate query languages. Correlating them is a manual, tab-juggling exercise. | Minutes-to-hours of MTTR spent navigating, not diagnosing. |
| 2 | **Alerting is hand-authored.** Somebody must guess a threshold for every metric on every service, then maintain it. | Alert fatigue, and silence exactly where nobody thought to add a monitor. |
| 3 | **The tool shows *that* something is wrong, never *why*.** The human does the differential diagnosis by slicing charts by one tag at a time. | The slowest, most expertise-dependent step of every incident. |
| 4 | **"Where" is an afterthought.** Tools model hosts and services. Businesses reason about regions, stores, clinics, plants, branches — *locations*. | Operations teams can't answer "is the Denver site OK?" without a custom dashboard. |
| 5 | **Changes are invisible.** Most incidents are caused by a deploy, flag flip or config change, yet change data lives in CI/CD, not next to the telemetry. | "What changed?" is asked in Slack instead of answered by the tool. |
| 6 | **Proprietary agents and pricing nobody can predict.** Per-host, per-custom-metric, per-GB-indexed billing punishes instrumentation. | Teams under-instrument to control cost; lock-in blocks migration. |
| 7 | **Built for humans clicking, not for agents.** AI assistants are bolted onto UIs rather than given a real API. | Automation can't do the first 10 minutes of triage. |

## 2. Vision and principles

**Vision:** the first observability system where the default experience is an
*answer* ("payments is failing in eu-central since the 2.4.1 deploy, only for
Adyen") rather than a wall of charts.

Principles that decide trade-offs:

1. **OpenTelemetry in, nothing proprietary.** OTLP is the only ingest protocol.
   No Telepathy agent, no Telepathy SDK. Anything that can export OTLP works on
   day one, and leaving is as easy as changing an endpoint.
2. **One event model, one query.** Traces, logs, metrics and change events share
   the same first-class dimensions (`service`, `location`, `env`) and the same
   filter language. A dashboard widget, an alert and an ad-hoc question are the
   same query object.
3. **Location is a first-class dimension**, equal to service. Health is a
   matrix of *application × location*, and "location" means whatever unit the
   business runs on.
4. **Baselines, not thresholds.** Every signal is judged against its own recent
   behaviour. Zero configuration should produce a useful health view.
5. **Explain, don't just display.** Every anomaly comes with an automatic
   differential analysis and the changes that preceded it.
6. **Everything is an API; the UI is just the first client.** Dashboards are
   code. Agents are first-class users.
7. **One binary to start, a cluster when you need it.** Time-to-first-insight
   under five minutes with no infrastructure; the same product scales out.

## 3. Users

| Persona | Job to be done | Primary surfaces |
|---|---|---|
| **On-call engineer** | "I was paged. What is broken, where, why, and since when?" | Health → Explain → Trace |
| **Service owner / developer** | "Is my service healthy after this deploy? Why is this endpoint slow?" | Explore, traces, dashboards |
| **Operations / site lead** | "Are all my locations OK right now?" | Health matrix, location view |
| **Engineering leader** | "Are we meeting our reliability goals? Where is the risk?" | SLOs, dashboards, reports |
| **AI agent / automation** | "Triage this alert and propose a cause before a human wakes up." | API, MCP server |

## 4. Product areas

Legend — **Now**: implemented in this repo (M0–M1) · **Next**: M2 · **Later**: M3+

### 4.1 Collect — getting data out of production

| Capability | Detail | When |
|---|---|---|
| OTLP/HTTP ingest | `/v1/traces`, `/v1/logs`, `/v1/metrics`; protobuf and JSON; gzip | **Now** |
| Location resolution | Configurable chain of resource attributes (`telepathy.location` → `site.id` → `cloud.region` → …) promoted to a first-class column | **Now** |
| Change events API | `POST /api/v1/events` for deploys, flags, config, scaling — one `curl` from CI | **Now** |
| OTLP/gRPC | Port 4317, gzip, same normalization as HTTP | **Now** |
| Ingest credentials | API tokens with an `ingest` role (or one static key) on HTTP and gRPC | **Now** |
| Collector recipes | Tested OTel Collector configs for hosts, Kubernetes, common databases and queues | Next |
| Edge buffering | Store-and-forward guidance and collector config for sites with unreliable links, with late-arriving data handled correctly | Next |
| Tail-based sampling at ingest | Keep 100% of errors and slow traces, sample the boring majority; exact RED metrics computed before sampling | Next |
| Browser & mobile RUM, synthetic checks | Via OTel web/mobile SDKs; scripted probes per location | Later |
| Continuous profiling | OTel profiles signal | Later |

### 4.2 Understand — health of applications and locations

| Capability | Detail | When |
|---|---|---|
| **Health matrix** | Application × location grid; each cell shows status, request rate, error rate, p95 | **Now** |
| **Zero-config health scoring** | Each cell compared against the median of its own preceding windows plus absolute guardrails; statuses *healthy / degraded / critical / silent* with plain-language reasons | **Now** |
| **Silence detection** | A cell that had traffic and now has none is flagged, not hidden | **Now** |
| Change timeline | Recent deploys/flags beside health, and marked on every chart | **Now** |
| Service map | Dependency graph derived from trace parent/child edges, with health, traffic, errors and latency on nodes and edges; filter by location | **Now** |
| Location hierarchy & map | Region → site → device rollups; geographic view | Next |
| SLOs | Declared objectives, error budgets, burn-rate alerts | Next |
| Seasonality-aware baselines | Hour-of-week baselines so Monday 9am is compared to Mondays at 9am | Next |
| Infrastructure views | Hosts, containers, Kubernetes objects linked to the services they run | Later |

### 4.3 Troubleshoot — when things go wrong

| Capability | Detail | When |
|---|---|---|
| **Explain** | One click from any scope: compares failing (or slowest-5%) spans against everything else across *every* attribute and ranks what is over-represented | **Now** |
| **Change correlation** | Explain surfaces the deploys/flags that preceded the problem and touch the implicated service and location as the "likely trigger" | **Now** |
| Unified filter language | `service:checkout location:eu-* status:error duration>500ms -http.route:/healthz "free text"` across spans, logs and metrics | **Now** |
| Golden-signal explorer | Rate / errors / p95 for any filter, split by any attribute | **Now** |
| Trace waterfall | Spans, errors and the logs emitted inside the trace in one view | **Now** |
| Log search | Filter by any attribute; jump from a log line to its trace | **Now** |
| **Zero-config alerting** | A health monitor exists from first start: alerts on any cell becoming critical, degraded or silent, with hysteresis | **Now** |
| Query monitors | The explorer's query object plus a comparison, alerting per series | **Now** |
| **Alerts with a hypothesis** | Every alert stores the Explain result and the change that likely triggered it; Slack, PagerDuty and webhook notifications carry them | **Now** |
| **Root-cause grouping** | When a failure cascades along the service map, callers are recorded as downstream symptoms and only the root service notifies | **Now** |
| Alert routing & silences | Per-team routing, maintenance windows, escalation policies, monitors as code | Next |
| Incident timeline | Auto-assembled record: first anomaly, changes, alerts, who looked at what | Next |
| Saved investigations | Shareable, replayable query notebooks | Later |

### 4.4 Build — custom dashboards

| Capability | Detail | When |
|---|---|---|
| Dashboard builder | Time series, single number and top-list widgets over any signal; any filter, any split | **Now** |
| **Dashboards as code** | JSON files in `dashboards/` are provisioned at startup; the editor exports the same format; editing in the UI detaches from the file rather than being silently overwritten | **Now** |
| Shared time range & live refresh | Global range and a pause-able 10s refresh | **Now** |
| Dashboard variables | `service` / `location` pickers on every dashboard, applied to each widget that does not already pin that dimension | **Now** |
| More widgets | Heatmaps, health-matrix widget, tables, markdown, SLO widgets | Next |
| Sharing & permissions | Teams, folders, read-only links, scheduled snapshots | Later |

### 4.5 Predict — the future pillar

The store, query model and baselines are being designed so prediction is an
extension, not a rewrite. Planned in order of confidence:

1. **Forecasting with bands** on any time series (capacity, disk, queue depth) and "time until threshold" alerts.
2. **Anomaly detection** as a query function, so anomalies can be charted and alerted on like any other value.
3. **Deploy risk scoring** — compare a canary's telemetry fingerprint with the previous version automatically at deploy events.
4. **Incident similarity** — "this looks like the incident on March 3" using Explain fingerprints.
5. **Leading-indicator discovery** — which signals historically moved before this SLO burned.

### 4.6 Platform

| Capability | When |
|---|---|
| Single static binary, embedded store and UI, Docker image | **Now** |
| Retention policy | **Now** |
| Authentication: admin sign-in and API tokens with `ingest` / `read` / `admin` roles | **Now** |
| Load test (`telepathy bench`) for ingest throughput and query latency | **Now** |
| Single sign-on (OIDC), per-user accounts, RBAC, multi-tenancy | Next |
| **MCP server** exposing health, query, explain and trace tools to AI agents | Next |
| Natural-language → query, grounded in the actual attribute catalog | Next |
| Columnar storage backend (ClickHouse) + object-storage cold tier | Next |
| Cost & cardinality explorer — see what each service's telemetry costs, drop or aggregate at ingest | Later |

## 5. What makes Telepathy different

1. **Health without configuration.** Install it, point OTLP at it, and the
   matrix is meaningful — no monitors authored, no thresholds guessed.
2. **Application × location as the home page.** Not hosts, not a service list.
3. **Alerts arrive with a hypothesis, and only the root cause pages.** The
   notification already says what is different and which deploy preceded it;
   services that are merely downstream are recorded, not paged.
4. **Explain is built in and one click away**, not a premium add-on behind a
   query-builder. It runs on every attribute you send, including the custom
   ones we've never heard of.
5. **Changes are telemetry.** Deploys and flags sit on every chart and are
   offered as the probable cause.
6. **One query object** for exploration, dashboards and monitors, so there
   is exactly one thing to learn and one thing for an agent to generate.
7. **OTel-native with no lock-in**, in one binary you can run on a laptop.

## 6. Non-goals (for now)

- A proprietary agent or SDK. Ever.
- Being a SIEM / security analytics product.
- A general BI tool; dashboards are for operational telemetry.
- Competing on integrations count — we lean on the OTel Collector ecosystem.

## 7. Success metrics

| Metric | Target |
|---|---|
| Time from install to first meaningful health view | < 5 minutes |
| Share of incidents where Explain's top-3 findings include the true cause | > 70% |
| Median time from "open Telepathy" to "root cause hypothesis" | < 3 minutes |
| Hand-authored monitors needed for a new service | 0 |
| Health false-positive rate (cells flagged with no user impact) | < 5% of flagged cells |
| Notifications per incident (root-cause grouping) | 1 |

## 8. Roadmap

| Milestone | Theme | Contents |
|---|---|---|
| **M0 — Foundation** *(done)* | A working vertical slice | OTLP/HTTP ingest for all three signals, embedded store, unified filter + query API, health matrix with baselines, Explain with change correlation, trace & log views, dashboard builder + dashboards-as-code, demo generator |
| **M1 — Trustworthy** *(done)* | Make it safe to depend on | Zero-config alerting, query monitors, Slack/PagerDuty/webhook notifications with Explain attached, root-cause grouping, authentication + API tokens, OTLP/gRPC, service map, dashboard variables, load test |
| **M2 — Team scale** | Many teams, many locations, much more data | Columnar storage backend (moved from M1: the load test shows where the embedded store runs out, and the engine choice is open question 3), SLOs & error budgets, location hierarchy and map, OIDC + RBAC + multi-tenancy, alert routing and silences, tail sampling, seasonality baselines, MCP server + NL query |
| **M3 — Predict** | From reactive to proactive | Forecasting, anomaly functions, deploy risk scoring, incident similarity |
| **M4 — Breadth** | Remaining parity | RUM, synthetics, profiling, infrastructure views, cost explorer |

## 9. Open questions

1. **Deployment model** — self-hosted open core, managed cloud, or both? Drives auth, tenancy and pricing work in M1–M2.
2. **Location semantics** — is a flat location enough for early users, or is hierarchy (region → site → device) needed in M1?
3. **Storage** — commit to ClickHouse for scale-out, or evaluate an embedded columnar engine to preserve the single-binary story longer?
4. **Explain at scale** — sampling strategy and pre-aggregated attribute sketches once span volume makes full scans impractical.
5. **Alert routing** — every alert currently goes to every channel. What is the smallest routing model (by service owner? by location?) that early users need?

**Decided:** the repository is licensed under Apache 2.0 — permissive, with a patent grant, and the same license as OpenTelemetry itself, so it can be embedded and contributed to without friction. If a managed offering (question 1) later needs protection from resale, that can be handled with separately licensed enterprise components rather than by relicensing the core.
