package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gthimmes/telepathy/internal/store"
)

func server(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var spans []store.Span
	for i := 0; i < 600; i++ {
		s := store.Span{TraceID: "trace-1", SpanID: "s", Service: "pay", Location: "eu", Name: "POST /charge", Kind: "server",
			TsNs: now.Add(-time.Duration(i) * time.Second).UnixNano(), DurationNs: 1e8, Status: store.StatusOK,
			Attrs: map[string]any{"payment.provider": "adyen"}}
		if i%3 == 0 {
			s.Status = store.StatusError
		}
		spans = append(spans, s)
	}
	if err := st.InsertSpans(spans); err != nil {
		t.Fatal(err)
	}
	st.PutSLO(&store.SLO{Name: "Pay availability", Service: "pay", Kind: "availability", Target: 99, WindowDays: 7})
	srv := httptest.NewServer(&Server{Store: st, Version: "test"})
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv
}

func rpc(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// call runs a tool and returns its decoded JSON payload.
func call(t *testing.T, url, name, args string) (map[string]any, bool) {
	t.Helper()
	_, m := rpc(t, url, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no result: %v", name, m)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	var out map[string]any
	json.Unmarshal([]byte(text), &out)
	if out == nil {
		out = map[string]any{"text": text}
	}
	return out, res["isError"].(bool)
}

func TestProtocol(t *testing.T) {
	srv := server(t)

	code, m := rpc(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	res := m["result"].(map[string]any)
	if code != 200 || res["protocolVersion"] != "2025-03-26" || res["serverInfo"].(map[string]any)["name"] != "telepathy" {
		t.Fatalf("initialize: %d %v", code, m)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("tools capability missing")
	}
	// An unknown revision gets one the server does speak.
	_, m = rpc(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if v := m["result"].(map[string]any)["protocolVersion"]; v != protocols[0] {
		t.Errorf("fallback version = %v", v)
	}

	// Notifications are acknowledged without a body.
	resp, _ := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification: status %d", resp.StatusCode)
	}

	_, m = rpc(t, srv.URL, `{"jsonrpc":"2.0","id":"a","method":"tools/list"}`)
	list := m["result"].(map[string]any)["tools"].([]any)
	if len(list) != len(tools) || m["id"] != "a" {
		t.Fatalf("tools/list returned %d tools, id %v", len(list), m["id"])
	}
	for _, x := range list {
		tl := x.(map[string]any)
		if tl["description"] == "" || tl["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %v lacks a description or object schema", tl["name"])
		}
	}

	_, m = rpc(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"nope"}`)
	if m["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Errorf("unknown method: %v", m)
	}
	_, m = rpc(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"drop_database"}}`)
	if m["error"] == nil {
		t.Errorf("unknown tool should be a protocol error: %v", m)
	}
	if code, _ := rpc(t, srv.URL, `[]`); code != 400 {
		t.Errorf("batch: status %d", code)
	}

	if resp, _ := http.Get(srv.URL); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin: status %d", resp.StatusCode)
	}
}

func TestTools(t *testing.T) {
	srv := server(t)

	out, isErr := call(t, srv.URL, "get_health", `{"include_healthy":true}`)
	if isErr || len(out["cells"].([]any)) != 1 {
		t.Fatalf("get_health: %v", out)
	}
	out, _ = call(t, srv.URL, "query_timeseries", `{"agg":"error_rate","filter":"service:pay","group_by":["location"],"range":"10m","points":5}`)
	series := out["series"].([]any)
	if len(series) != 1 || len(out["timestamps"].([]any)) > 7 {
		t.Fatalf("query_timeseries: %v", out)
	}
	if total := series[0].(map[string]any)["total"].(float64); total < 30 || total > 37 {
		t.Errorf("error rate %.1f, want about 33", total)
	}
	out, _ = call(t, srv.URL, "search_spans", `{"filter":"status:error payment.provider:adyen","limit":3}`)
	spans := out["spans"].([]any)
	if len(spans) != 3 {
		t.Fatalf("search_spans returned %d spans", len(spans))
	}
	out, _ = call(t, srv.URL, "get_trace", `{"trace_id":"trace-1"}`)
	if len(out["spans"].([]any)) == 0 {
		t.Error("get_trace returned no spans")
	}
	out, _ = call(t, srv.URL, "list_slos", `{}`)
	slo := out["slos"].([]any)[0].(map[string]any)
	if slo["name"] != "Pay availability" || slo["state"] != "exhausted" || slo["budget"] != nil {
		t.Errorf("list_slos: %v", slo)
	}
	out, _ = call(t, srv.URL, "get_metadata", `{}`)
	if out["services"].([]any)[0] != "pay" {
		t.Errorf("get_metadata: %v", out)
	}
	for _, name := range []string{"list_alerts", "explain", "search_logs", "get_service_map", "list_changes"} {
		args := `{}`
		if name == "explain" {
			args = `{"scope":"service:pay"}`
		}
		if out, isErr := call(t, srv.URL, name, args); isErr {
			t.Errorf("%s failed: %v", name, out)
		}
	}

	// A bad argument is a tool error the model can read, not a protocol error.
	out, isErr = call(t, srv.URL, "search_spans", `{"range":"soon"}`)
	if !isErr || !strings.Contains(out["text"].(string), "range") {
		t.Errorf("bad range: isError=%v %v", isErr, out)
	}
	if _, isErr = call(t, srv.URL, "get_trace", `{"trace_id":"missing"}`); !isErr {
		t.Error("missing trace should be a tool error")
	}
}
