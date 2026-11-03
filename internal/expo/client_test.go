package expo_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/integration"
)

func clientFor(t *testing.T, f *fakeExpo, token string) *expo.Client {
	t.Helper()
	c, err := expo.NewClient(expo.ClientOptions{Token: token, HTTP: f.server(t)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A TOKEN GOES TO EXPO AND NOWHERE ELSE: the endpoint is fixed.
func TestTheClientOnlyAddressesExpo(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	_, tok := f.robot("crewlet-acme-qa", expo.RoleRunner)
	if _, err := clientFor(t, f, tok.value).Viewer(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.onlyExpo(t)
	if _, err := expo.NewClient(expo.ClientOptions{Token: "  "}); err == nil {
		t.Fatal("a client was built with no token")
	}
}

// THE VIEWER IS WHO THE TOKEN ACTS AS: a robot with its tokens by prefix, or a
// person with none.
func TestTheViewerIsTheTokensActor(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	robot, tok := f.robot("crewlet-acme-qa", expo.RoleRunner)
	second := f.mint(robot.id)
	f.revoke(second)
	me, err := clientFor(t, f, tok.value).Viewer(context.Background())
	if err != nil || !me.Robot() || me.ID != robot.id || me.Name != "crewlet-acme-qa" || len(me.Tokens) != 2 {
		t.Fatalf("viewer = %+v, %v", me, err)
	}
	revoked := 0
	for _, t := range me.Tokens {
		if t.Revoked {
			revoked++
		}
	}
	if revoked != 1 {
		t.Errorf("tokens = %+v, want one revoked", me.Tokens)
	}
	person := f.person("Avery", expo.RoleAdmin)
	me, err = clientFor(t, f, person.value).Viewer(context.Background())
	if err != nil || me.Robot() || me.Name != "Avery" || len(me.Tokens) != 0 {
		t.Fatalf("a person's viewer = %+v, %v", me, err)
	}
}

// A TOKEN EXPO REFUSES IS A 401, the credential's, and the error never carries
// the token.
func TestARefusedTokenIsA401(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	_, tok := f.robot("crewlet-acme-qa", expo.RoleRunner)
	f.revoke(tok)
	_, err := clientFor(t, f, tok.value).Viewer(context.Background())
	if expo.Status(err) != http.StatusUnauthorized ||
		!errors.Is(integration.Reject(err, expo.Status(err)), integration.ErrCredentialRejected) {
		t.Fatalf("a revoked token = %v (status %d), want a rejected credential", err, expo.Status(err))
	}
	if strings.Contains(err.Error(), tok.value) {
		t.Fatalf("the error carries the token: %v", err)
	}
}

// THE ROLE IS READ BY EXPO'S ROLE, and an account the actor is not on is a
// refusal rather than a role.
func TestTheRoleIsReadByExposRole(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	robot, tok := f.robot("crewlet-acme-qa", expo.RoleDeveloper)
	c := clientFor(t, f, tok.value)
	role, err := c.RoleOn(context.Background(), testAccountName)
	if err != nil || role != expo.RoleDeveloper {
		t.Fatalf("role = %q, %v", role, err)
	}
	f.setRole(robot.id, expo.RoleAdmin)
	if role, _ := c.RoleOn(context.Background(), testAccountName); !role.Administers() {
		t.Errorf("Admin read as %q, which does not administer", role)
	}
	_, err = c.RoleOn(context.Background(), "someone-else")
	if s := expo.Status(err); s != http.StatusForbidden {
		t.Fatalf("an account the robot is not on = %v (status %d), want 403", err, s)
	}
}

// A GRAPHQL ERROR ON A 200 IS A REFUSAL WITH THE STATUS ITS CODE STANDS FOR.
func TestAGraphQLErrorReadsAsItsStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		ref    fakeRefusal
		status int
	}{
		{"not permitted", fakeRefusal{code: "UNAUTHORIZED_ERROR", message: "not allowed"}, http.StatusForbidden},
		{"not found", fakeRefusal{code: "ENTITY_NOT_FOUND"}, http.StatusNotFound},
		{"bad input", fakeRefusal{code: "BAD_USER_INPUT", message: "bad"}, http.StatusBadRequest},
		{"the caller's", fakeRefusal{code: "SOMETHING_NEW", errorType: "USER"}, http.StatusBadRequest},
		{"transient", fakeRefusal{code: "SOMETHING_NEW", transient: true}, http.StatusServiceUnavailable},
		{"unclassified", fakeRefusal{code: "INTERNAL"}, http.StatusInternalServerError},
		{"an HTTP refusal", fakeRefusal{status: http.StatusBadGateway, message: "<html>bad gateway</html>"},
			http.StatusBadGateway},
	} {
		f := newFakeExpo()
		_, tok := f.robot("crewlet-acme-qa", expo.RoleRunner)
		f.refuse["CrewletViewer"] = tc.ref
		_, err := clientFor(t, f, tok.value).Viewer(context.Background())
		if got := expo.Status(err); got != tc.status {
			t.Errorf("%s: status %d (%v), want %d", tc.name, got, err, tc.status)
		}
	}
	if got := expo.Status(errors.New("dial tcp: i/o timeout")); got != 0 {
		t.Errorf("a transport fault reads as status %d", got)
	}
}

// A 429 IS WAITED OUT AS EXPO ASKS, a bounded number of times, and then handed
// back as the 429 it is.
func TestARateLimitIsWaitedOut(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	_, tok := f.robot("crewlet-acme-qa", expo.RoleRunner)
	c := clientFor(t, f, tok.value)
	f.limited["CrewletViewer"] = 2
	if _, err := c.Viewer(context.Background()); err != nil {
		t.Fatalf("two 429s were not waited out: %v", err)
	}
	f.limited["CrewletViewer"] = 10
	if _, err := c.Viewer(context.Background()); expo.Status(err) != http.StatusTooManyRequests {
		t.Fatalf("a lasting 429 = %v, want 429", err)
	}
}
