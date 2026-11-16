// Package demo simulates a small e-commerce system running in four locations
// and ships its telemetry to Telepathy over real OTLP/HTTP. It includes a
// recurring incident — a bad payments deploy in one location that only breaks
// one payment provider — so there is something worth troubleshooting.
package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

type Options struct {
	Endpoint string        // e.g. http://localhost:4318
	Backfill time.Duration // history to generate before going live
	Rate     int           // traces per second
	APIKey   string
	Logf     func(format string, args ...any)
}

const (
	incidentPeriod = 90 * time.Minute
	incidentLength = 25 * time.Minute
	incidentLoc    = "eu-central"
	goodVersion    = "2.4.0"
	badVersion     = "2.4.1"
)

var (
	locations = []struct {
		name   string
		weight float64
	}{{"us-east", 0.35}, {"us-west", 0.25}, {"eu-central", 0.25}, {"ap-south", 0.15}}
	services  = []string{"web-gateway", "checkout", "payments", "inventory", "search"}
	versions  = map[string]string{"web-gateway": "5.12.0", "checkout": "3.7.2", "payments": goodVersion, "inventory": "1.18.4", "search": "1.9.0"}
	providers = []string{"stripe", "adyen", "paypal"}
	platforms = []string{"web", "web", "ios", "android"}
	tiers     = []string{"free", "free", "free", "pro", "pro", "enterprise"}
)

type resKey struct{ svc, loc, ver string }

type gen struct {
	opts   Options
	rng    *rand.Rand
	client *http.Client
	anchor time.Time // incident phase origin

	spans   map[resKey][]*tracepb.Span
	logs    map[resKey][]*logspb.LogRecord
	metrics map[resKey][]*metricspb.Metric
	nSpans  int

	orders  map[string]float64 // per location since last metrics tick
	revenue map[string]float64
	wasBad  bool
}

// Run backfills history and then emits live telemetry until ctx is done.
func Run(ctx context.Context, opts Options) error {
	if opts.Rate <= 0 {
		opts.Rate = 6
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	opts.Endpoint = strings.TrimRight(opts.Endpoint, "/")
	now := time.Now()
	g := &gen{opts: opts, rng: rand.New(rand.NewSource(now.UnixNano())), client: &http.Client{Timeout: 30 * time.Second},
		anchor: now.Add(-8 * time.Minute), orders: map[string]float64{}, revenue: map[string]float64{}}
	g.reset()

	start := now.Add(-opts.Backfill).Truncate(time.Second)
	g.wasBad = g.incident(start)
	if opts.Backfill > 0 {
		opts.Logf("demo: backfilling %s of telemetry…", opts.Backfill)
		g.oneOffEvents(now)
		for t := start; t.Before(now.Add(-time.Second)); t = t.Add(time.Second) {
			if ctx.Err() != nil {
				return nil
			}
			g.tick(t)
			if g.nSpans >= 3000 {
				if err := g.flush(); err != nil {
					return err
				}
			}
		}
		if err := g.flush(); err != nil {
			return err
		}
		opts.Logf("demo: backfill complete, now live (%d traces/s)", opts.Rate)
	}
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case t := <-tk.C:
			g.tick(t.Add(-time.Second).Truncate(time.Second))
			if err := g.flush(); err != nil {
				opts.Logf("demo: send failed: %v", err)
				g.reset()
			}
		}
	}
}

func (g *gen) reset() {
	g.spans, g.logs, g.metrics, g.nSpans = map[resKey][]*tracepb.Span{}, map[resKey][]*logspb.LogRecord{}, map[resKey][]*metricspb.Metric{}, 0
}

func (g *gen) incident(t time.Time) bool {
	phase := t.Sub(g.anchor) % incidentPeriod
	if phase < 0 {
		phase += incidentPeriod
	}
	return phase < incidentLength
}

func (g *gen) version(svc, loc string, bad bool) string {
	if svc == "payments" && loc == incidentLoc && bad {
		return badVersion
	}
	return versions[svc]
}

// tick simulates one second of the system.
func (g *gen) tick(t time.Time) {
	bad := g.incident(t)
	if bad != g.wasBad {
		g.wasBad = bad
		if bad {
			g.event(t.Add(-45*time.Second), "deploy", "payments", incidentLoc, "Deploy payments "+badVersion,
				map[string]any{"version": badVersion, "previous": goodVersion, "author": "release-bot"})
		} else {
			g.event(t, "deploy", "payments", incidentLoc, "Rollback payments to "+goodVersion,
				map[string]any{"version": goodVersion, "previous": badVersion, "author": "oncall"})
		}
	}
	for i := 0; i < g.opts.Rate; i++ {
		g.trace(t.Add(time.Duration(g.rng.Intn(1000))*time.Millisecond), bad)
	}
	if t.Unix()%10 == 0 {
		g.emitMetrics(t, bad)
	}
}

func (g *gen) oneOffEvents(now time.Time) {
	if g.opts.Backfill >= 45*time.Minute {
		g.event(now.Add(-40*time.Minute), "deploy", "search", "us-west", "Deploy search 1.9.0", map[string]any{"version": "1.9.0"})
	}
	if g.opts.Backfill >= 55*time.Minute {
		g.event(now.Add(-52*time.Minute), "flag", "search", "", "Flag new-ranking enabled for 10%", map[string]any{"flag": "new-ranking", "rollout": 10})
	}
}

func (g *gen) pickLoc() string {
	r := g.rng.Float64()
	for _, l := range locations {
		if r < l.weight {
			return l.name
		}
		r -= l.weight
	}
	return locations[0].name
}

func (g *gen) pick(s []string) string { return s[g.rng.Intn(len(s))] }

// lat samples a log-normal latency around base milliseconds.
func (g *gen) lat(baseMs float64) time.Duration {
	return time.Duration(baseMs * math.Exp(g.rng.NormFloat64()*0.35) * float64(time.Millisecond))
}

func (g *gen) id(n int) []byte {
	b := make([]byte, n)
	g.rng.Read(b)
	return b
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func num(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

type spanIn struct {
	svc, loc, name string
	kind           tracepb.Span_SpanKind
	trace, parent  []byte
	start          time.Time
	dur            time.Duration
	errMsg         string
	attrs          []*commonpb.KeyValue
	bad            bool
}

func (g *gen) span(in spanIn) []byte {
	id := g.id(8)
	sp := &tracepb.Span{TraceId: in.trace, SpanId: id, ParentSpanId: in.parent, Name: in.name, Kind: in.kind,
		StartTimeUnixNano: uint64(in.start.UnixNano()), EndTimeUnixNano: uint64(in.start.Add(in.dur).UnixNano()),
		Attributes: append(in.attrs, str("host.name", fmt.Sprintf("%s-%s-%d", in.svc, in.loc, 1+g.rng.Intn(3)))),
		Status:     &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}}
	if in.errMsg != "" {
		sp.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: in.errMsg}
	}
	k := resKey{in.svc, in.loc, g.version(in.svc, in.loc, in.bad)}
	g.spans[k] = append(g.spans[k], sp)
	g.nSpans++
	return id
}

func (g *gen) log(svc, loc string, bad bool, t time.Time, sev logspb.SeverityNumber, sevText, body string, trace, span []byte, attrs ...*commonpb.KeyValue) {
	k := resKey{svc, loc, g.version(svc, loc, bad)}
	g.logs[k] = append(g.logs[k], &logspb.LogRecord{TimeUnixNano: uint64(t.UnixNano()), SeverityNumber: sev, SeverityText: sevText,
		Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}}, TraceId: trace, SpanId: span, Attributes: attrs})
}

func httpAttrs(method, route string, status int64) []*commonpb.KeyValue {
	return []*commonpb.KeyValue{str("http.request.method", method), str("http.route", route), num("http.response.status_code", status)}
}

// trace generates one end-to-end request.
func (g *gen) trace(t time.Time, bad bool) {
	loc := g.pickLoc()
	tid := g.id(16)
	platform, tier := g.pick(platforms), g.pick(tiers)
	edge := []*commonpb.KeyValue{str("client.platform", platform), str("customer.tier", tier)}
	gwOverhead := g.lat(4)

	// A small background error rate everywhere.
	flake := g.rng.Float64() < 0.004

	r := g.rng.Float64()
	switch {
	case r < 0.40: // search
		d := g.lat(45)
		status, msg := int64(200), ""
		if flake {
			status, msg = 500, "index shard unavailable"
		}
		gw := g.span(spanIn{svc: "web-gateway", loc: loc, name: "GET /api/search", kind: tracepb.Span_SPAN_KIND_SERVER, trace: tid, start: t,
			dur: d + gwOverhead, errMsg: msg, attrs: append(httpAttrs("GET", "/api/search", status), edge...), bad: bad})
		sattrs := httpAttrs("GET", "/query", status)
		if flake {
			sattrs = append(sattrs, str("exception.type", "ShardUnavailableError"))
		}
		g.span(spanIn{svc: "search", loc: loc, name: "GET /query", kind: tracepb.Span_SPAN_KIND_SERVER, trace: tid, parent: gw,
			start: t.Add(gwOverhead / 2), dur: d, errMsg: msg, attrs: sattrs, bad: bad})

	case r < 0.70: // product page → inventory
		db := g.lat(6)
		d := db + g.lat(8)
		gw := g.span(spanIn{svc: "web-gateway", loc: loc, name: "GET /api/products/{id}", kind: tracepb.Span_SPAN_KIND_SERVER, trace: tid, start: t,
			dur: d + gwOverhead, attrs: append(httpAttrs("GET", "/api/products/{id}", 200), edge...), bad: bad})
		inv := g.span(spanIn{svc: "inventory", loc: loc, name: "GET /stock/{sku}", kind: tracepb.Span_SPAN_KIND_SERVER, trace: tid, parent: gw,
			start: t.Add(gwOverhead / 2), dur: d, attrs: httpAttrs("GET", "/stock/{sku}", 200), bad: bad})
		g.span(spanIn{svc: "inventory", loc: loc, name: "SELECT stock", kind: tracepb.Span_SPAN_KIND_CLIENT, trace: tid, parent: inv,
			start: t.Add(gwOverhead/2 + 2*time.Millisecond), dur: db, attrs: []*commonpb.KeyValue{str("db.system.name", "postgresql"), str("db.operation.name", "SELECT")}, bad: bad})
		if g.rng.Float64() < 0.01 {
			g.log("inventory", loc, bad, t, logspb.SeverityNumber_SEVERITY_NUMBER_WARN, "WARN", "stock cache miss, falling back to primary", tid, inv)
		}

	default: // checkout → inventory + payments
		provider := g.pick(providers)
		amount := 20 + g.rng.Float64()*180
		invDur := g.lat(18)
		payDur := g.lat(120)
		payStatus, payMsg, exc := int64(200), "", ""
		hit := bad && loc == incidentLoc && provider == "adyen"
		switch {
		case hit && g.rng.Float64() < 0.7:
			payDur = 3*time.Second + g.lat(40)
			payStatus, payMsg, exc = 504, "provider request timed out", "ProviderTimeoutError"
		case hit:
			payDur += g.lat(900)
		case flake:
			payStatus, payMsg, exc = 500, "card tokenization failed", "TokenizationError"
		}
		coOverhead := g.lat(10)
		coDur := invDur + payDur + coOverhead
		status, msg := int64(200), ""
		if payMsg != "" {
			status, msg = 502, "payment failed"
		}
		gw := g.span(spanIn{svc: "web-gateway", loc: loc, name: "POST /api/checkout", kind: tracepb.Span_SPAN_KIND_SERVER, trace: tid, start: t,
			dur: coDur + gwOverhead, errMsg: msg, attrs: append(httpAttrs("POST", "/api/checkout", status), edge...), bad: bad})
		t1 := t.Add(gwOverhead / 2)
		co := g.span(spanIn{svc: "checkout", loc: loc, name: "POST /orders", kind: tracepb.Span_SPAN_KIND_SERVER, trace: tid, parent: gw,
			start: t1, dur: coDur, errMsg: msg, attrs: append(httpAttrs("POST", "/orders", status), str("customer.tier", tier)), bad: bad})
		t2 := t1.Add(coOverhead / 2)
		inv := g.span(spanIn{svc: "inventory", loc: loc, name: "POST /reserve", kind: tracepb.Span_SPAN_KIND_SERVER, trace: tid, parent: co,
			start: t2, dur: invDur, attrs: httpAttrs("POST", "/reserve", 200), bad: bad})
		g.span(spanIn{svc: "inventory", loc: loc, name: "UPDATE stock", kind: tracepb.Span_SPAN_KIND_CLIENT, trace: tid, parent: inv,
			start: t2.Add(2 * time.Millisecond), dur: invDur * 7 / 10, attrs: []*commonpb.KeyValue{str("db.system.name", "postgresql"), str("db.operation.name", "UPDATE")}, bad: bad})
		t3 := t2.Add(invDur)
		currency := "USD"
		if c, ok := map[string]string{"eu-central": "EUR", "ap-south": "INR"}[loc]; ok {
			currency = c
		}
		pattrs := append(httpAttrs("POST", "/charge", payStatus), str("payment.provider", provider), str("payment.currency", currency))
		if exc != "" {
			pattrs = append(pattrs, str("exception.type", exc))
		}
		pay := g.span(spanIn{svc: "payments", loc: loc, name: "POST /charge", kind: tracepb.Span_SPAN_KIND_SERVER, trace: tid, parent: co,
			start: t3, dur: payDur, errMsg: payMsg, attrs: pattrs, bad: bad})
		g.span(spanIn{svc: "payments", loc: loc, name: "POST " + provider + "/v1/charges", kind: tracepb.Span_SPAN_KIND_CLIENT, trace: tid, parent: pay,
			start: t3.Add(3 * time.Millisecond), dur: payDur - 6*time.Millisecond, errMsg: payMsg,
			attrs: []*commonpb.KeyValue{str("payment.provider", provider), str("server.address", "api."+provider+".com")}, bad: bad})
		end := t3.Add(payDur)
		if payMsg != "" {
			g.log("payments", loc, bad, end, logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "ERROR",
				fmt.Sprintf("charge failed: %s (provider=%s, after %dms)", payMsg, provider, payDur.Milliseconds()), tid, pay,
				str("payment.provider", provider), str("exception.type", exc))
			g.log("checkout", loc, bad, end, logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "ERROR", "order aborted: payment failed", tid, co)
		} else {
			g.orders[loc]++
			g.revenue[loc] += amount
			if g.rng.Float64() < 0.2 {
				g.log("checkout", loc, bad, end, logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO",
					fmt.Sprintf("order placed total=%.2f provider=%s", amount, provider), tid, co, str("payment.provider", provider))
			}
		}
	}
}

func gauge(name, unit string, t time.Time, v float64) *metricspb.Metric {
	return &metricspb.Metric{Name: name, Unit: unit, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{
		{TimeUnixNano: uint64(t.UnixNano()), Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: v}}}}}}
}

func delta(name, unit string, t time.Time, v float64) *metricspb.Metric {
	return &metricspb.Metric{Name: name, Unit: unit, Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
		AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, IsMonotonic: true,
		DataPoints: []*metricspb.NumberDataPoint{{TimeUnixNano: uint64(t.UnixNano()), Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: v}}}}}}
}

func (g *gen) emitMetrics(t time.Time, bad bool) {
	for _, l := range locations {
		for _, svc := range services {
			cpu := 0.22 + l.weight*0.6 + g.rng.NormFloat64()*0.03
			if bad && svc == "payments" && l.name == incidentLoc {
				cpu = 0.88 + g.rng.NormFloat64()*0.03 // threads pile up behind the timing-out provider
			}
			k := resKey{svc, l.name, g.version(svc, l.name, bad)}
			g.metrics[k] = append(g.metrics[k],
				gauge("system.cpu.utilization", "1", t, math.Max(0.02, math.Min(1, cpu))),
				gauge("system.memory.utilization", "1", t, 0.55+g.rng.NormFloat64()*0.02))
		}
		k := resKey{"checkout", l.name, versions["checkout"]}
		g.metrics[k] = append(g.metrics[k],
			delta("orders.completed", "{order}", t, g.orders[l.name]),
			delta("orders.revenue", "USD", t, g.revenue[l.name]))
		g.orders[l.name], g.revenue[l.name] = 0, 0
	}
}

func resourceOf(k resKey) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		str("service.name", k.svc), str("service.version", k.ver),
		str("telepathy.location", k.loc), str("deployment.environment.name", "production")}}
}

func (g *gen) flush() error {
	defer g.reset()
	if len(g.spans) > 0 {
		req := &coltrace.ExportTraceServiceRequest{}
		for k, s := range g.spans {
			req.ResourceSpans = append(req.ResourceSpans, &tracepb.ResourceSpans{Resource: resourceOf(k), ScopeSpans: []*tracepb.ScopeSpans{{Spans: s}}})
		}
		if err := g.post("/v1/traces", req); err != nil {
			return err
		}
	}
	if len(g.logs) > 0 {
		req := &collogs.ExportLogsServiceRequest{}
		for k, l := range g.logs {
			req.ResourceLogs = append(req.ResourceLogs, &logspb.ResourceLogs{Resource: resourceOf(k), ScopeLogs: []*logspb.ScopeLogs{{LogRecords: l}}})
		}
		if err := g.post("/v1/logs", req); err != nil {
			return err
		}
	}
	if len(g.metrics) > 0 {
		req := &colmetrics.ExportMetricsServiceRequest{}
		for k, m := range g.metrics {
			req.ResourceMetrics = append(req.ResourceMetrics, &metricspb.ResourceMetrics{Resource: resourceOf(k), ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: m}}})
		}
		if err := g.post("/v1/metrics", req); err != nil {
			return err
		}
	}
	return nil
}

func (g *gen) send(path, contentType string, body []byte) error {
	req, err := http.NewRequest("POST", g.opts.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if g.opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.opts.APIKey)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func (g *gen) post(path string, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	return g.send(path, "application/x-protobuf", b)
}

func (g *gen) event(t time.Time, kind, svc, loc, title string, attrs map[string]any) {
	b, _ := json.Marshal(map[string]any{"tsMs": t.UnixMilli(), "kind": kind, "service": svc, "location": loc,
		"env": "production", "title": title, "attrs": attrs})
	if err := g.send("/api/v1/events", "application/json", b); err != nil {
		g.opts.Logf("demo: event failed: %v", err)
	}
}
