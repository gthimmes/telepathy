package ingest

import (
	"context"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // accept gzip-compressed exports
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type traceSvc struct {
	coltrace.UnimplementedTraceServiceServer
	h *Handler
}

func (s traceSvc) Export(_ context.Context, req *coltrace.ExportTraceServiceRequest) (*coltrace.ExportTraceServiceResponse, error) {
	return &coltrace.ExportTraceServiceResponse{}, storeErr(s.h.ConsumeTrace(req))
}

type logsSvc struct {
	collogs.UnimplementedLogsServiceServer
	h *Handler
}

func (s logsSvc) Export(_ context.Context, req *collogs.ExportLogsServiceRequest) (*collogs.ExportLogsServiceResponse, error) {
	return &collogs.ExportLogsServiceResponse{}, storeErr(s.h.ConsumeLogs(req))
}

type metricsSvc struct {
	colmetrics.UnimplementedMetricsServiceServer
	h *Handler
}

func (s metricsSvc) Export(_ context.Context, req *colmetrics.ExportMetricsServiceRequest) (*colmetrics.ExportMetricsServiceResponse, error) {
	return &colmetrics.ExportMetricsServiceResponse{}, storeErr(s.h.ConsumeMetrics(req))
}

func storeErr(err error) error {
	if err != nil {
		return status.Error(codes.Unavailable, err.Error()) // retryable for OTLP clients
	}
	return nil
}

// GRPCServer returns an OTLP/gRPC server for all three signals. authorize,
// when non-nil, is given the request's credential (the bearer token from the
// "authorization" metadata, or "x-telepathy-key") and decides whether the
// export is allowed.
func (h *Handler) GRPCServer(authorize func(credential string) bool) *grpc.Server {
	opts := []grpc.ServerOption{grpc.MaxRecvMsgSize(maxBody)}
	if authorize != nil {
		opts = append(opts, grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			cred := ""
			if v := md.Get("authorization"); len(v) > 0 {
				cred = BearerToken(v[0])
			}
			if v := md.Get("x-telepathy-key"); len(v) > 0 {
				cred = v[0]
			}
			if !authorize(cred) {
				return nil, status.Error(codes.Unauthenticated, "invalid or missing ingest credential")
			}
			return next(ctx, req)
		}))
	}
	srv := grpc.NewServer(opts...)
	coltrace.RegisterTraceServiceServer(srv, traceSvc{h: h})
	collogs.RegisterLogsServiceServer(srv, logsSvc{h: h})
	colmetrics.RegisterMetricsServiceServer(srv, metricsSvc{h: h})
	return srv
}

// BearerToken extracts the token from an Authorization header value.
func BearerToken(v string) string {
	if len(v) > 7 && (v[:7] == "Bearer " || v[:7] == "bearer ") {
		return v[7:]
	}
	return ""
}
