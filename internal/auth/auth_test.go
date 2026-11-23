package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gthimmes/telepathy/internal/store"
)

func newServer(t *testing.T, a *Auth) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a.Store = st
	mux := http.NewServeMux()
	a.Register(mux)
	ok := func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }
	mux.HandleFunc("/", ok)
	srv := httptest.NewServer(a.Middleware(mux))
	t.Cleanup(srv.Close)
	return srv
}

type req struct {
	method, path, bearer, cookie, origin, body string
}

func do(t *testing.T, srv *httptest.Server, r req) *http.Response {
	t.Helper()
	hr, _ := http.NewRequest(r.method, srv.URL+r.path, strings.NewReader(r.body))
	if r.bearer != "" {
		hr.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.cookie != "" {
		hr.Header.Set("Cookie", cookieName+"="+r.cookie)
	}
	if r.origin != "" {
		hr.Header.Set("Origin", r.origin)
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestOpenModeEnforcesOnlyIngestKey(t *testing.T) {
	srv := newServer(t, &Auth{IngestKey: "k1"})
	for _, c := range []struct {
		r    req
		want int
	}{
		{req{method: "GET", path: "/api/v1/health"}, 200},
		{req{method: "PUT", path: "/api/v1/dashboards/x"}, 200},
		{req{method: "POST", path: "/v1/traces"}, 401},
		{req{method: "POST", path: "/v1/traces", bearer: "nope"}, 401},
		{req{method: "POST", path: "/v1/traces", bearer: "k1"}, 200},
	} {
		if got := do(t, srv, c.r).StatusCode; got != c.want {
			t.Errorf("%s %s: got %d want %d", c.r.method, c.r.path, got, c.want)
		}
	}
}

func TestSessionsTokensAndRoles(t *testing.T) {
	a := &Auth{AdminPassword: "correct horse"}
	srv := newServer(t, a)

	// Nothing but the app shell and the public endpoints without a credential.
	for path, want := range map[string]int{"/": 200, "/assets/app.js": 200, "/api/v1/status": 200, "/api/v1/health": 401, "/api/v1/tokens": 401} {
		if got := do(t, srv, req{method: "GET", path: path}).StatusCode; got != want {
			t.Errorf("anonymous GET %s: got %d want %d", path, got, want)
		}
	}
	if got := do(t, srv, req{method: "POST", path: "/v1/traces"}).StatusCode; got != 401 {
		t.Errorf("anonymous ingest: got %d", got)
	}

	if got := do(t, srv, req{method: "POST", path: "/api/v1/login", body: `{"password":"wrong"}`}).StatusCode; got != 401 {
		t.Fatalf("wrong password: got %d", got)
	}
	resp := do(t, srv, req{method: "POST", path: "/api/v1/login", body: `{"password":"correct horse"}`})
	if resp.StatusCode != 200 || len(resp.Cookies()) != 1 {
		t.Fatalf("login: %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
	ck := resp.Cookies()[0]
	if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie must be HttpOnly and SameSite=Strict: %+v", ck)
	}
	session := ck.Value

	if got := do(t, srv, req{method: "GET", path: "/api/v1/health", cookie: session}).StatusCode; got != 200 {
		t.Errorf("session read: got %d", got)
	}
	// A cross-site request riding on the cookie is refused; same-origin is fine.
	if got := do(t, srv, req{method: "PUT", path: "/api/v1/dashboards/x", cookie: session, origin: "https://evil.example"}).StatusCode; got != 403 {
		t.Errorf("cross-origin write: got %d want 403", got)
	}
	if got := do(t, srv, req{method: "PUT", path: "/api/v1/dashboards/x", cookie: session, origin: srv.URL}).StatusCode; got != 200 {
		t.Errorf("same-origin write: got %d", got)
	}

	mint := func(role string) string {
		resp := do(t, srv, req{method: "POST", path: "/api/v1/tokens", cookie: session, body: `{"name":"t","role":"` + role + `"}`})
		var out struct {
			Secret string      `json:"secret"`
			Token  store.Token `json:"token"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != 201 || !strings.HasPrefix(out.Secret, tokenPrefix) {
			t.Fatalf("create %s token: %d %+v", role, resp.StatusCode, out)
		}
		return out.Secret
	}
	ingest, read, admin := mint(RoleIngest), mint(RoleRead), mint(RoleAdmin)

	for _, c := range []struct {
		name string
		r    req
		want int
	}{
		{"ingest token sends telemetry", req{method: "POST", path: "/v1/traces", bearer: ingest}, 200},
		{"ingest token posts change events", req{method: "POST", path: "/api/v1/events", bearer: ingest}, 200},
		{"ingest token cannot read", req{method: "GET", path: "/api/v1/health", bearer: ingest}, 403},
		{"read token reads", req{method: "GET", path: "/api/v1/health", bearer: read}, 200},
		{"read token queries", req{method: "POST", path: "/api/v1/query", bearer: read}, 200},
		{"read token cannot ingest", req{method: "POST", path: "/v1/traces", bearer: read}, 403},
		{"read token cannot write", req{method: "PUT", path: "/api/v1/dashboards/x", bearer: read}, 403},
		{"read token cannot list tokens", req{method: "GET", path: "/api/v1/tokens", bearer: read}, 403},
		{"read token cannot see channels", req{method: "GET", path: "/api/v1/channels", bearer: read}, 403},
		{"admin token writes", req{method: "PUT", path: "/api/v1/dashboards/x", bearer: admin}, 200},
		{"admin token ingests", req{method: "POST", path: "/v1/traces", bearer: admin}, 200},
		{"unknown token", req{method: "GET", path: "/api/v1/health", bearer: "tp_0000"}, 401},
	} {
		if got := do(t, srv, c.r).StatusCode; got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
	if !a.AllowIngest(ingest) || a.AllowIngest(read) || a.AllowIngest("") {
		t.Error("AllowIngest (gRPC authorizer) disagrees with the HTTP rules")
	}

	// Secrets are stored hashed, and a deleted token stops working.
	var raw int
	a.Store.DB().QueryRow(`SELECT COUNT(*) FROM tokens WHERE hash LIKE 'tp_%' OR hash = ?`, read).Scan(&raw)
	if raw != 0 {
		t.Error("token secret stored in the clear")
	}
	toks, _ := a.Store.ListTokens()
	for _, tk := range toks {
		if tk.Role == RoleRead {
			do(t, srv, req{method: "DELETE", path: "/api/v1/tokens/" + tk.ID, cookie: session})
		}
	}
	if got := do(t, srv, req{method: "GET", path: "/api/v1/health", bearer: read}).StatusCode; got != 401 {
		t.Errorf("deleted token still works: %d", got)
	}

	do(t, srv, req{method: "POST", path: "/api/v1/logout", cookie: session})
	if got := do(t, srv, req{method: "GET", path: "/api/v1/health", cookie: session}).StatusCode; got != 401 {
		t.Errorf("session survives logout: %d", got)
	}
}

func TestLoginThrottle(t *testing.T) {
	srv := newServer(t, &Auth{AdminPassword: "pw"})
	for i := 0; i < maxFails; i++ {
		do(t, srv, req{method: "POST", path: "/api/v1/login", body: `{"password":"x"}`})
	}
	if got := do(t, srv, req{method: "POST", path: "/api/v1/login", body: `{"password":"pw"}`}).StatusCode; got != 429 {
		t.Errorf("expected throttling after %d failures, got %d", maxFails, got)
	}
}
