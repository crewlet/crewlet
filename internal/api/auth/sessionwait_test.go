package auth_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD WAITS FOR ITS SESSION EXACTLY AS LONG AS THE ENGINE SAYS A
// CLIENT SHOULD: [auth.SessionClientWait], past the longest the guard holds the
// read ([auth.SessionCatchUp]). Held exactly rather than above, so a catch-up
// that grows moves the client's wait with it instead of leaving a tab that
// gives up on a read the guard is still holding.
//
// Mutation: set SESSION_READ_TIMEOUT_MS to 30_000, red.
func TestTheDashboardWaitsForItsSessionAsLongAsTheEngineSays(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "SESSION_READ_TIMEOUT_MS")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("SESSION_READ_TIMEOUT_MS = %q is not a number: %v", raw, err)
	}
	if wait := time.Duration(ms) * time.Millisecond; wait != auth.SessionClientWait {
		t.Errorf("the dashboard waits %s for its session, where a client waits %s: the "+
			"guard holds the read up to %s", wait, auth.SessionClientWait, auth.SessionCatchUp)
	}
	if auth.SessionClientWait <= auth.SessionCatchUp {
		t.Errorf("a client waits %s, no longer than the guard holds the read (%s)",
			auth.SessionClientWait, auth.SessionCatchUp)
	}
}
