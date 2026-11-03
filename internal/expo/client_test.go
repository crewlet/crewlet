package expo_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/integration"
)

func newClient(t *testing.T, f *fakeExpo) *expo.Client {
	t.Helper()
	c, err := expo.NewClient(expo.ClientOptions{Token: provisioner, HTTP: f.server(t)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// THE PROVISIONER TOKEN GOES TO EXPO AND NOWHERE ELSE: the endpoint is fixed.
func TestTheClientOnlyAddressesExpo(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	c := newClient(t, f)
	if _, err := c.Viewer(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.onlyExpo(t)
	if _, err := expo.NewClient(expo.ClientOptions{Token: "  "}); err == nil {
		t.Fatal("a client was built with no token")
	}
}

// THE VIEWER IS WHO THE TOKEN ACTS AS, and a token Expo answers with no actor
// for is refused as a 401, like one it refuses outright.
func TestTheViewerIsTheTokensActor(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	c := newClient(t, f)
	actor, err := c.Viewer(context.Background())
	if err != nil || actor.ID != provisionerID || actor.Kind != "Robot" {
		t.Fatalf("viewer = %+v, %v", actor, err)
	}
	f.noViewer = true
	_, err = c.Viewer(context.Background())
	if expo.Status(err) != http.StatusUnauthorized ||
		!errors.Is(integration.Reject(err, expo.Status(err)), integration.ErrCredentialRejected) {
		t.Fatalf("a null actor = %v (status %d), want a rejected credential", err, expo.Status(err))
	}

	bad, err := expo.NewClient(expo.ClientOptions{Token: "fake-wrong-token", HTTP: f.server(t)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bad.Viewer(context.Background())
	if expo.Status(err) != http.StatusUnauthorized {
		t.Fatalf("a refused token = %v, want 401", err)
	}
	if strings.Contains(err.Error(), "fake-wrong-token") {
		t.Fatalf("the error carries the token: %v", err)
	}
}

// AN ACCOUNT RESOLVES BY NAME, without regard to case, and a name Expo does
// not know is a 404 refusal.
func TestAnAccountResolvesByName(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	c := newClient(t, f)
	acct, err := c.Account(context.Background(), "Example-Org")
	if err != nil || acct.ID != testAccountID {
		t.Fatalf("account = %+v, %v", acct, err)
	}
	_, err = c.Account(context.Background(), "nobody")
	if expo.Status(err) != http.StatusNotFound {
		t.Fatalf("an unknown account = %v, want 404", err)
	}
	var api *expo.APIError
	if !errors.As(err, &api) || api.Code != "ENTITY_NOT_FOUND" || !strings.Contains(api.Detail, "not found") {
		t.Fatalf("the refusal = %+v, want Expo's code and message", api)
	}
	perms, err := c.ViewerPermissions(context.Background(), testAccountName)
	if err != nil || !slices.Contains(perms, expo.PermissionAdmin) {
		t.Fatalf("viewer permissions = %v, %v", perms, err)
	}
	n, err := c.AppCount(context.Background(), testAccountName)
	if err != nil || n != 3 {
		t.Fatalf("app count = %d, %v", n, err)
	}
}

// THE ROBOT LISTING READS EVERY PAGE, each robot with its permissions, its
// tokens and whether its deletion is scheduled.
func TestTheRobotListingReadsEveryPage(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	var ids []string
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		ids = append(ids, f.addRobot(name, expo.PermissionView).id)
	}
	f.addToken(ids[0], "crewlet seat a")
	f.robots[ids[4]].deleting = true
	c := newClient(t, f)
	robots, err := c.Robots(context.Background(), testAccountName)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range robots {
		got = append(got, r.ID)
	}
	if !slices.Equal(got, ids) {
		t.Fatalf("listed %v, want %v", got, ids)
	}
	if len(robots[0].Tokens) != 1 || robots[0].Tokens[0].Note != "crewlet seat a" || robots[0].Tokens[0].Revoked {
		t.Errorf("robot a's tokens = %+v", robots[0].Tokens)
	}
	if !robots[4].Deleting || robots[0].Deleting {
		t.Errorf("deleting = %v, %v", robots[0].Deleting, robots[4].Deleting)
	}
	if n := strings.Count(strings.Join(f.operations(), " "), "CrewletRobots"); n != 3 {
		t.Errorf("read %d pages, want 3 of 2", n)
	}
}

// A ROBOT IS CREATED, RENAMED AND DELETED; its tokens are minted, revoked
// and deleted; its permissions granted and revoked. Each write lands on the
// object it names.
func TestEveryRobotAndTokenWriteLands(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	c := newClient(t, f)
	ctx := context.Background()

	robot, err := c.CreateRobot(ctx, testAccountID, "crewlet-acme-qa",
		[]expo.Permission{expo.PermissionView, expo.PermissionRun})
	if err != nil || robot.ID == "" {
		t.Fatalf("create = %+v, %v", robot, err)
	}
	if got, _ := f.robot(robot.ID); got.name != "crewlet-acme-qa" ||
		!slices.Equal(got.perms, []expo.Permission{expo.PermissionView, expo.PermissionRun}) {
		t.Fatalf("created %+v", got)
	}
	if err := c.RenameRobot(ctx, robot.ID, "crewlet-acme-qa2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.robot(robot.ID); got.name != "crewlet-acme-qa2" {
		t.Fatalf("renamed to %q", got.name)
	}

	if err := c.Grant(ctx, testAccountID, robot.ID, []expo.Permission{expo.PermissionPublish}); err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke(ctx, testAccountID, robot.ID, []expo.Permission{expo.PermissionRun}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.robot(robot.ID); !slices.Equal(got.perms, []expo.Permission{expo.PermissionView, expo.PermissionPublish}) {
		t.Fatalf("permissions = %v", got.perms)
	}
	before := len(f.operations())
	if err := c.Grant(ctx, testAccountID, robot.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke(ctx, testAccountID, robot.ID, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.operations()) != before {
		t.Fatal("an empty grant or revoke made a call")
	}

	tok, value, err := c.CreateToken(ctx, robot.ID, "crewlet seat qa")
	if err != nil || tok.ID == "" || value == "" || value != f.value(tok.ID) || tok.Note != "crewlet seat qa" {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	if err := c.RevokeToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if live := f.liveTokens(robot.ID); len(live) != 0 {
		t.Fatalf("live tokens after a revoke = %v", live)
	}
	if err := c.DeleteToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	// ALREADY GONE IS THE STATE ASKED FOR.
	if err := c.DeleteToken(ctx, tok.ID); err != nil {
		t.Fatalf("deleting a deleted token = %v", err)
	}
	if err := c.RevokeToken(ctx, tok.ID); err != nil {
		t.Fatalf("revoking a deleted token = %v", err)
	}

	if err := c.DeleteRobot(ctx, robot.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.robot(robot.ID); !got.deleting {
		t.Fatal("the robot's deletion was not scheduled")
	}
	if err := c.DeleteRobot(ctx, "robot-unknown"); err != nil {
		t.Fatalf("deleting a robot Expo does not have = %v", err)
	}
}

// A GRAPHQL ERROR ON A 200 IS A REFUSAL WITH THE STATUS ITS CODE STANDS FOR,
// so the shared rules read it as they read an HTTP status.
func TestAGraphQLErrorReadsAsItsStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		ref      fakeRefusal
		status   int
		rejected bool
	}{
		{"not permitted", fakeRefusal{code: "UNAUTHORIZED_ERROR", message: "not allowed"}, http.StatusForbidden, true},
		{"not found", fakeRefusal{code: "ENTITY_NOT_FOUND"}, http.StatusNotFound, false},
		{"bad input", fakeRefusal{code: "BAD_USER_INPUT", message: "bad"}, http.StatusBadRequest, false},
		{"the caller's", fakeRefusal{code: "SOMETHING_NEW", errorType: "USER"}, http.StatusBadRequest, false},
		{"transient", fakeRefusal{code: "SOMETHING_NEW", transient: true}, http.StatusServiceUnavailable, false},
		{"unclassified", fakeRefusal{code: "INTERNAL"}, http.StatusInternalServerError, false},
		{"an HTTP refusal", fakeRefusal{status: http.StatusBadGateway, message: "<html>bad gateway</html>"},
			http.StatusBadGateway, false},
	} {
		f := newFakeExpo()
		f.refuse["CrewletViewer"] = tc.ref
		c := newClient(t, f)
		_, err := c.Viewer(context.Background())
		if got := expo.Status(err); got != tc.status {
			t.Errorf("%s: status %d (%v), want %d", tc.name, got, err, tc.status)
		}
		if got := errors.Is(integration.Reject(err, expo.Status(err)), integration.ErrCredentialRejected); got != tc.rejected {
			t.Errorf("%s: rejected = %v, want %v", tc.name, got, tc.rejected)
		}
		if strings.Contains(err.Error(), provisioner) {
			t.Errorf("%s: the error carries the token: %v", tc.name, err)
		}
	}
	if got := expo.Status(errors.New("dial tcp: i/o timeout")); got != 0 {
		t.Errorf("a transport fault reads as status %d", got)
	}
}

// A 429 IS WAITED OUT AS EXPO ASKS, a bounded number of times, and then
// handed back as the 429 it is.
func TestARateLimitIsWaitedOut(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	f.limited["CrewletViewer"] = 2
	c := newClient(t, f)
	if _, err := c.Viewer(context.Background()); err != nil {
		t.Fatalf("two 429s were not waited out: %v", err)
	}
	f.limited["CrewletViewer"] = 10
	_, err := c.Viewer(context.Background())
	if expo.Status(err) != http.StatusTooManyRequests {
		t.Fatalf("a lasting 429 = %v, want 429", err)
	}
	if n := strings.Count(strings.Join(f.operations(), " "), "CrewletViewer"); n != 3+4 {
		t.Errorf("asked %d times, want 3 then 4", n)
	}
}

// AN ANSWER WITH NO DATA AND NO ERRORS is a fault, never an empty success.
func TestAnAnswerWithNoDataIsAFault(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	f.refuse["CrewletAppCount"] = fakeRefusal{status: http.StatusOK, message: `{"data":null}`}
	c := newClient(t, f)
	if _, err := c.AppCount(context.Background(), testAccountName); err == nil {
		t.Fatal("no data read as a count")
	}
}
