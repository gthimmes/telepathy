package auth

import (
	"net/http/httptest"
	"testing"
)

func TestRequiredRoles(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"POST", "/api/v1/mcp", RoleRead},
		{"GET", "/api/v1/slos", RoleRead},
		{"POST", "/api/v1/slos", RoleAdmin},
		{"DELETE", "/api/v1/slos/x", RoleAdmin},
		{"GET", "/api/v1/silences", RoleRead},
		{"POST", "/api/v1/silences", RoleAdmin},
		{"POST", "/v1/traces", RoleIngest},
		{"GET", "/api/v1/channels", RoleAdmin},
		{"GET", "/", ""},
	} {
		if got := required(httptest.NewRequest(tc.method, tc.path, nil)); got != tc.want {
			t.Errorf("%s %s needs %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
