package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/config"
)

// THE ACCESS ANSWER NAMES EVERY CREDENTIAL AND CARRIES NO VALUE, over the route
// the API really mounts, built from the guard it really mounts: the posture
// type has no member a value could travel in, and this is where that is
// observed rather than argued. Anonymous gets the refusal and a member the
// 403, whatever the anonymous posture opens.
func TestTheAccessRouteNamesTokensAndNeverTheirValues(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.API.Auth.AllowedOrigins = []string{"https://ops.example.com"}
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Role: config.RoleAdmin, Token: "value-of-ops-7Qx"},
		{ID: "ci", Role: config.RoleMember, Token: "value-of-ci-9Zr"},
	}
	a := newApp(t, api.Options{Bootstrap: &b})

	anonymous := httptest.NewRecorder()
	a.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/access", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET /access = %d, want 401", anonymous.Code)
	}
	// AND A MEMBER IS FORBIDDEN IT: which key reaches what is a map of which
	// credential to take, and a member key is one a teammate holds.
	asMember := httptest.NewRequest(http.MethodGet, "/access", nil)
	asMember.Header.Set("Authorization", "Bearer value-of-ci-9Zr")
	member := httptest.NewRecorder()
	a.ServeHTTP(member, asMember)
	if member.Code != http.StatusForbidden {
		t.Errorf("a member's GET /access = %d, want 403", member.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/access", nil)
	req.Header.Set("Authorization", "Bearer value-of-ops-7Qx")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	raw, _ := io.ReadAll(rec.Result().Body)
	body := string(raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /access as ops = %d: %s", rec.Code, body)
	}
	for _, want := range []string{`"id":"ci"`, `"id":"ops"`, `"yours":true`, `"role":"admin"`,
		`"role":"member"`, `"anonymous":"public"`, "https://ops.example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer does not carry %s: %s", want, body)
		}
	}
	for _, value := range []string{"value-of-ops-7Qx", "value-of-ci-9Zr"} {
		if strings.Contains(body, value) {
			t.Errorf("the answer carries a token value: %s", body)
		}
	}
}
