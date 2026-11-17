package ingest

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/gthimmes/telepathy/internal/store"
)

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func TestGRPCExport(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := &Handler{Store: st}
	srv := h.GRPCServer(func(cred string) bool { return cred == "secret" })
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := coltrace.NewTraceServiceClient(conn)

	now := uint64(time.Now().UnixNano())
	req := &coltrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "checkout"), kv("cloud.region", "eu-west-1")}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, SpanId: []byte{1, 2, 3, 4, 5, 6, 7, 8},
			Name: "POST /orders", Kind: tracepb.Span_SPAN_KIND_SERVER, StartTimeUnixNano: now - 5e6, EndTimeUnixNano: now,
			Attributes: []*commonpb.KeyValue{kv("http.route", "/orders")},
		}}}},
	}}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Export(ctx, req); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("export without a credential: got %v, want Unauthenticated", err)
	}
	authed := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer secret")
	if _, err := client.Export(authed, req, grpc.UseCompressor(gzip.Name)); err != nil {
		t.Fatalf("export: %v", err)
	}

	var service, location, traceID, route string
	err = st.DB().QueryRow(`SELECT service, location, trace_id, json_extract(attrs, '$."http.route"') FROM spans`).
		Scan(&service, &location, &traceID, &route)
	if err != nil {
		t.Fatal(err)
	}
	if service != "checkout" || location != "eu-west-1" || traceID != "0102030405060708090a0b0c0d0e0f10" || route != "/orders" {
		t.Errorf("stored span: service=%q location=%q trace=%q route=%q", service, location, traceID, route)
	}
}
