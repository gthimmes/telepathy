// Package ingest accepts OpenTelemetry (OTLP/HTTP) traces, logs and metrics
// in protobuf or JSON and normalizes them into Telepathy's storage model.
package ingest

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/gthimmes/telepathy/internal/store"
)

const maxBody = 32 << 20

// DefaultLocationKeys is the resolution order for a resource's location.
// "Location" is whatever unit the business reasons about — a region, a
// datacenter, a store, a clinic, an edge site — so it is configurable.
var DefaultLocationKeys = []string{"telepathy.location", "location", "site.id", "cloud.region", "cloud.availability_zone", "k8s.cluster.name"}

type Handler struct {
	Store        *store.Store
	LocationKeys []string
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/traces", h.wrap(h.traces))
	mux.HandleFunc("POST /v1/logs", h.wrap(h.logs))
	mux.HandleFunc("POST /v1/metrics", h.wrap(h.metrics))
}

type decodeFn func(body []byte, isJSON bool) (resp proto.Message, err error)

func (h *Handler) wrap(fn decodeFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var rd io.Reader = http.MaxBytesReader(w, r.Body, maxBody)
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(rd)
			if err != nil {
				http.Error(w, "bad gzip body", http.StatusBadRequest)
				return
			}
			defer gz.Close()
			rd = io.LimitReader(gz, maxBody)
		}
		body, err := io.ReadAll(rd)
		if err != nil {
			http.Error(w, "cannot read body", http.StatusBadRequest)
			return
		}
		isJSON := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
		resp, err := fn(body, isJSON)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if isJSON {
			w.Header().Set("Content-Type", "application/json")
			b, _ := protojson.Marshal(resp)
			w.Write(b)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		b, _ := proto.Marshal(resp)
		w.Write(b)
	}
}

func unmarshal(body []byte, isJSON bool, m proto.Message) error {
	if isJSON {
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, m)
	}
	return proto.Unmarshal(body, m)
}

// idString renders a trace/span id as lowercase hex. OTLP/JSON encodes ids as
// hex, but protojson decodes bytes fields as base64 — so a 32-char hex trace
// id arrives as 24 "bytes". Re-encoding those as base64 recovers the hex.
func idString(b []byte) string {
	switch len(b) {
	case 0:
		return ""
	case 24, 12:
		return strings.ToLower(base64.StdEncoding.EncodeToString(b))
	}
	allZero := true
	for _, c := range b {
		if c != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return ""
	}
	return hex.EncodeToString(b)
}

func anyValue(v *commonpb.AnyValue) any {
	switch t := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return t.StringValue
	case *commonpb.AnyValue_BoolValue:
		if t.BoolValue {
			return "true"
		}
		return "false"
	case *commonpb.AnyValue_IntValue:
		return t.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return t.DoubleValue
	case *commonpb.AnyValue_ArrayValue:
		out := make([]any, 0, len(t.ArrayValue.GetValues()))
		for _, e := range t.ArrayValue.GetValues() {
			out = append(out, anyValue(e))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		out := map[string]any{}
		for _, kv := range t.KvlistValue.GetValues() {
			out[kv.GetKey()] = anyValue(kv.GetValue())
		}
		return out
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(t.BytesValue)
	}
	return nil
}

func addAttrs(dst map[string]any, kvs []*commonpb.KeyValue) {
	for _, kv := range kvs {
		if v := anyValue(kv.GetValue()); v != nil {
			dst[kv.GetKey()] = v
		}
	}
}

// resource holds the first-class dimensions extracted from an OTel resource.
type resource struct {
	service, location, env string
	attrs                  map[string]any
}

func (h *Handler) resource(r *resourcepb.Resource) resource {
	attrs := map[string]any{}
	addAttrs(attrs, r.GetAttributes())
	str := func(k string) string {
		if s, ok := attrs[k].(string); ok {
			return s
		}
		return ""
	}
	res := resource{service: str("service.name"), location: "default", attrs: attrs}
	if res.service == "" {
		res.service = "unknown_service"
	}
	keys := h.LocationKeys
	if len(keys) == 0 {
		keys = DefaultLocationKeys
	}
	for _, k := range keys {
		if v := str(k); v != "" {
			res.location = v
			delete(attrs, k) // promoted to a first-class dimension
			break
		}
	}
	if res.env = str("deployment.environment.name"); res.env == "" {
		res.env = str("deployment.environment")
	}
	delete(attrs, "service.name")
	return res
}

func merged(base map[string]any, kvs []*commonpb.KeyValue) map[string]any {
	out := make(map[string]any, len(base)+len(kvs))
	for k, v := range base {
		out[k] = v
	}
	addAttrs(out, kvs)
	return out
}

var spanKinds = map[tracepb.Span_SpanKind]string{
	tracepb.Span_SPAN_KIND_SERVER:   "server",
	tracepb.Span_SPAN_KIND_CLIENT:   "client",
	tracepb.Span_SPAN_KIND_PRODUCER: "producer",
	tracepb.Span_SPAN_KIND_CONSUMER: "consumer",
}

func (h *Handler) traces(body []byte, isJSON bool) (proto.Message, error) {
	var req coltrace.ExportTraceServiceRequest
	if err := unmarshal(body, isJSON, &req); err != nil {
		return nil, fmt.Errorf("decode traces: %w", err)
	}
	return &coltrace.ExportTraceServiceResponse{}, h.ConsumeTrace(&req)
}

// ConsumeTrace normalizes and stores a decoded OTLP request (shared by HTTP and gRPC).
func (h *Handler) ConsumeTrace(req *coltrace.ExportTraceServiceRequest) error {
	var out []store.Span
	for _, rs := range req.GetResourceSpans() {
		res := h.resource(rs.GetResource())
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				s := store.Span{
					TraceID: idString(sp.GetTraceId()), SpanID: idString(sp.GetSpanId()), ParentID: idString(sp.GetParentSpanId()),
					Service: res.service, Location: res.location, Env: res.env,
					Name: sp.GetName(), Kind: "internal",
					TsNs:       int64(sp.GetStartTimeUnixNano()),
					DurationNs: int64(sp.GetEndTimeUnixNano()) - int64(sp.GetStartTimeUnixNano()),
					Status:     int(sp.GetStatus().GetCode()), StatusMsg: sp.GetStatus().GetMessage(),
					Attrs: merged(res.attrs, sp.GetAttributes()),
				}
				if k, ok := spanKinds[sp.GetKind()]; ok {
					s.Kind = k
				}
				if s.DurationNs < 0 {
					s.DurationNs = 0
				}
				// Promote exception events onto the span so they are queryable
				// (and visible to Explain) like any other attribute.
				for _, ev := range sp.GetEvents() {
					if ev.GetName() != "exception" {
						continue
					}
					for _, kv := range ev.GetAttributes() {
						if _, exists := s.Attrs[kv.GetKey()]; !exists && strings.HasPrefix(kv.GetKey(), "exception.") {
							if kv.GetKey() == "exception.stacktrace" {
								continue
							}
							s.Attrs[kv.GetKey()] = anyValue(kv.GetValue())
						}
					}
				}
				out = append(out, s)
			}
		}
	}
	return h.Store.InsertSpans(out)
}

func severityText(n int) string {
	switch {
	case n >= 21:
		return "FATAL"
	case n >= 17:
		return "ERROR"
	case n >= 13:
		return "WARN"
	case n >= 9:
		return "INFO"
	case n >= 5:
		return "DEBUG"
	case n >= 1:
		return "TRACE"
	}
	return "INFO"
}

func (h *Handler) logs(body []byte, isJSON bool) (proto.Message, error) {
	var req collogs.ExportLogsServiceRequest
	if err := unmarshal(body, isJSON, &req); err != nil {
		return nil, fmt.Errorf("decode logs: %w", err)
	}
	return &collogs.ExportLogsServiceResponse{}, h.ConsumeLogs(&req)
}

// ConsumeLogs normalizes and stores a decoded OTLP request (shared by HTTP and gRPC).
func (h *Handler) ConsumeLogs(req *collogs.ExportLogsServiceRequest) error {
	now := time.Now().UnixNano()
	var out []store.Log
	for _, rl := range req.GetResourceLogs() {
		res := h.resource(rl.GetResource())
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				l := store.Log{
					TsNs: int64(lr.GetTimeUnixNano()), Service: res.service, Location: res.location, Env: res.env,
					SeverityNum: int(lr.GetSeverityNumber()), Severity: strings.ToUpper(lr.GetSeverityText()),
					TraceID: idString(lr.GetTraceId()), SpanID: idString(lr.GetSpanId()),
					Attrs: merged(res.attrs, lr.GetAttributes()),
				}
				if l.TsNs == 0 {
					if l.TsNs = int64(lr.GetObservedTimeUnixNano()); l.TsNs == 0 {
						l.TsNs = now
					}
				}
				if l.Severity == "" {
					l.Severity = severityText(l.SeverityNum)
				}
				switch b := anyValue(lr.GetBody()).(type) {
				case string:
					l.Body = b
				case nil:
				default:
					j, _ := json.Marshal(b)
					l.Body = string(j)
				}
				out = append(out, l)
			}
		}
	}
	return h.Store.InsertLogs(out)
}

func numberValue(dp *metricspb.NumberDataPoint) float64 {
	if v, ok := dp.GetValue().(*metricspb.NumberDataPoint_AsInt); ok {
		return float64(v.AsInt)
	}
	return dp.GetAsDouble()
}

func (h *Handler) metrics(body []byte, isJSON bool) (proto.Message, error) {
	var req colmetrics.ExportMetricsServiceRequest
	if err := unmarshal(body, isJSON, &req); err != nil {
		return nil, fmt.Errorf("decode metrics: %w", err)
	}
	return &colmetrics.ExportMetricsServiceResponse{}, h.ConsumeMetrics(&req)
}

// ConsumeMetrics normalizes and stores a decoded OTLP request (shared by HTTP and gRPC).
func (h *Handler) ConsumeMetrics(req *colmetrics.ExportMetricsServiceRequest) error {
	var out []store.MetricPoint
	for _, rm := range req.GetResourceMetrics() {
		res := h.resource(rm.GetResource())
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				base := store.MetricPoint{Name: m.GetName(), Unit: m.GetUnit(),
					Service: res.service, Location: res.location, Env: res.env}
				number := func(kind string, dps []*metricspb.NumberDataPoint) {
					for _, dp := range dps {
						p := base
						p.Kind, p.TsNs, p.Value = kind, int64(dp.GetTimeUnixNano()), numberValue(dp)
						p.Attrs = merged(res.attrs, dp.GetAttributes())
						out = append(out, p)
					}
				}
				dist := func(kind string, ts uint64, count uint64, sum float64, attrs []*commonpb.KeyValue) {
					p := base
					p.Kind, p.TsNs, p.Count, p.Sum = kind, int64(ts), int64(count), sum
					if count > 0 {
						p.Value = sum / float64(count)
					}
					p.Attrs = merged(res.attrs, attrs)
					out = append(out, p)
				}
				switch d := m.GetData().(type) {
				case *metricspb.Metric_Gauge:
					number("gauge", d.Gauge.GetDataPoints())
				case *metricspb.Metric_Sum:
					number("sum", d.Sum.GetDataPoints())
				case *metricspb.Metric_Histogram:
					for _, dp := range d.Histogram.GetDataPoints() {
						dist("histogram", dp.GetTimeUnixNano(), dp.GetCount(), dp.GetSum(), dp.GetAttributes())
					}
				case *metricspb.Metric_ExponentialHistogram:
					for _, dp := range d.ExponentialHistogram.GetDataPoints() {
						dist("histogram", dp.GetTimeUnixNano(), dp.GetCount(), dp.GetSum(), dp.GetAttributes())
					}
				case *metricspb.Metric_Summary:
					for _, dp := range d.Summary.GetDataPoints() {
						dist("summary", dp.GetTimeUnixNano(), dp.GetCount(), dp.GetSum(), dp.GetAttributes())
					}
				}
			}
		}
	}
	return h.Store.InsertMetrics(out)
}
