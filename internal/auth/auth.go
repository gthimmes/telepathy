// Package auth protects the API, UI data and ingest endpoints.
//
// Authentication is off until an admin password is configured, so the
// five-minute quick start stays zero-config. Once it is on:
//
//   - people sign in with the admin password and get a session cookie;
//   - programs use API tokens ("tp_…") with one of three roles:
//     ingest (send telemetry and change events), read (query), admin (everything).
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gthimmes/telepathy/internal/store"
)

const (
	RoleIngest = "ingest"
	RoleRead   = "read"
	RoleAdmin  = "admin"

	cookieName  = "telepathy_session"
	sessionTTL  = 7 * 24 * time.Hour
	tokenPrefix = "tp_"
	maxFails    = 10 // failed sign-ins per minute before we start refusing
)

type Auth struct {
	Store         *store.Store
	AdminPassword string // empty = authentication disabled
	IngestKey     string // optional static ingest credential (--ingest-key)

	mu       sync.Mutex
	sessions map[string]time.Time // sha256(session id) → expiry
	fails    []time.Time
	touched  map[string]int64 // token id → last time we recorded its use
}

func (a *Auth) Enabled() bool { return a.AdminPassword != "" }

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func equal(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// credential returns the bearer token or X-Telepathy-Key of a request.
func credential(r *http.Request) string {
	if k := r.Header.Get("X-Telepathy-Key"); k != "" {
		return k
	}
	v := r.Header.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "Bearer ") {
		return v[7:]
	}
	return ""
}

// roleOfCredential resolves a presented secret to a role ("" if invalid).
func (a *Auth) roleOfCredential(cred string) string {
	if cred == "" {
		return ""
	}
	if a.IngestKey != "" && equal(cred, a.IngestKey) {
		return RoleIngest
	}
	if !strings.HasPrefix(cred, tokenPrefix) {
		return ""
	}
	t := a.Store.TokenByHash(hashOf(cred))
	if t == nil {
		return ""
	}
	now := time.Now().UnixMilli()
	a.mu.Lock()
	if a.touched == nil {
		a.touched = map[string]int64{}
	}
	stale := now-a.touched[t.ID] > 60_000
	if stale {
		a.touched[t.ID] = now
	}
	a.mu.Unlock()
	if stale {
		go a.Store.TouchToken(t.ID, now)
	}
	return t.Role
}

// AllowIngest reports whether a credential may send telemetry. It is the
// authorizer for OTLP/gRPC; nil-equivalent (always true) when nothing is configured.
func (a *Auth) AllowIngest(cred string) bool {
	if !a.Enabled() && a.IngestKey == "" {
		return true
	}
	role := a.roleOfCredential(cred)
	return role == RoleIngest || role == RoleAdmin
}

func (a *Auth) sessionValid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[hashOf(c.Value)]
	return ok && time.Now().Before(exp)
}

// identify returns the caller's role and whether it came from a browser session.
func (a *Auth) identify(r *http.Request) (role string, viaCookie bool) {
	if cred := credential(r); cred != "" {
		return a.roleOfCredential(cred), false
	}
	if a.sessionValid(r) {
		return RoleAdmin, true
	}
	return "", false
}

// required returns the role a request needs ("" = public).
func required(r *http.Request) string {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/v1/"):
		return RoleIngest
	case !strings.HasPrefix(p, "/api/"):
		return "" // the web app shell; it holds no data
	case p == "/api/v1/status", p == "/api/v1/session", p == "/api/v1/login", p == "/api/v1/logout":
		return ""
	case p == "/api/v1/events" && r.Method == http.MethodPost:
		return RoleIngest
	case strings.HasPrefix(p, "/api/v1/tokens"), strings.HasPrefix(p, "/api/v1/channels"):
		return RoleAdmin
	case r.Method == http.MethodGet, r.Method == http.MethodHead:
		return RoleRead
	case r.Method == http.MethodPost && (p == "/api/v1/query" || p == "/api/v1/explain"):
		return RoleRead // read-only despite the verb
	}
	return RoleAdmin
}

func satisfies(have, need string) bool {
	return have == RoleAdmin || (have != "" && have == need)
}

func deny(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// sameOrigin rejects cross-site requests that ride on the session cookie.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // not a browser cross-origin request
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

// Middleware enforces the role each route requires.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		need := required(r)
		if need == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !a.Enabled() {
			// Open mode: only the optional static ingest key is enforced.
			if need == RoleIngest && a.IngestKey != "" && strings.HasPrefix(r.URL.Path, "/v1/") && !a.AllowIngest(credential(r)) {
				deny(w, http.StatusUnauthorized, "invalid ingest key")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		role, viaCookie := a.identify(r)
		if role == "" {
			deny(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !satisfies(role, need) {
			deny(w, http.StatusForbidden, "this credential has the "+role+" role; "+need+" is required")
			return
		}
		if viaCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			deny(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Register adds the sign-in and token management endpoints.
func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/session", a.session)
	mux.HandleFunc("POST /api/v1/login", a.login)
	mux.HandleFunc("POST /api/v1/logout", a.logout)
	mux.HandleFunc("GET /api/v1/tokens", a.listTokens)
	mux.HandleFunc("POST /api/v1/tokens", a.createToken)
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", a.deleteToken)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (a *Auth) session(w http.ResponseWriter, r *http.Request) {
	role, _ := a.identify(r)
	if !a.Enabled() {
		role = RoleAdmin
	}
	writeJSON(w, 200, map[string]any{"authRequired": a.Enabled(), "authenticated": role != "", "role": role})
}

func (a *Auth) throttled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cut := time.Now().Add(-time.Minute)
	keep := a.fails[:0]
	for _, t := range a.fails {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	a.fails = keep
	return len(a.fails) >= maxFails
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	if !a.Enabled() {
		writeJSON(w, 200, map[string]any{"authenticated": true})
		return
	}
	if !sameOrigin(r) {
		deny(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	if a.throttled() {
		deny(w, http.StatusTooManyRequests, "too many failed sign-in attempts; wait a minute")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		deny(w, 400, "invalid request")
		return
	}
	if !equal(in.Password, a.AdminPassword) {
		a.mu.Lock()
		a.fails = append(a.fails, time.Now())
		a.mu.Unlock()
		deny(w, http.StatusUnauthorized, "wrong password")
		return
	}
	id := randomHex(32)
	now := time.Now()
	a.mu.Lock()
	if a.sessions == nil {
		a.sessions = map[string]time.Time{}
	}
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	a.sessions[hashOf(id)] = now.Add(sessionTTL)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: int(sessionTTL.Seconds())})
	writeJSON(w, 200, map[string]any{"authenticated": true, "role": RoleAdmin})
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, hashOf(c.Value))
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (a *Auth) listTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := a.Store.ListTokens()
	if err != nil {
		deny(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"tokens": ts})
}

func (a *Auth) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		deny(w, 400, "invalid request")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || (in.Role != RoleIngest && in.Role != RoleRead && in.Role != RoleAdmin) {
		deny(w, 400, "name and a role of ingest, read or admin are required")
		return
	}
	secret := tokenPrefix + randomHex(20)
	t := store.Token{Name: in.Name, Role: in.Role, Prefix: secret[:9]}
	if err := a.Store.CreateToken(&t, hashOf(secret)); err != nil {
		deny(w, 500, err.Error())
		return
	}
	// The secret is returned exactly once; only its hash is stored.
	writeJSON(w, 201, map[string]any{"token": t, "secret": secret})
}

func (a *Auth) deleteToken(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.DeleteToken(r.PathValue("id")); err != nil {
		deny(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
