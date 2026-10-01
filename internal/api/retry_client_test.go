package api_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DASHBOARD'S TWO RETRY CONSTANTS ARE THE ENGINE'S OWN HINTS, held here —
// see internal/clientsource for why a copy exists at all and why this side
// checks it.
//
// Every `unavailable` answer the engine sends says when to ask again, and the
// dashboard waits that out (dashboard/src/protocol/retry.ts). Two values are
// its own. UNAVAILABLE_RETRY_MS is what it waits on an answer that carried no
// hint, and it claims to be what the engine says when it has nothing better —
// the health tick — which is only true while the two agree. RETRY_AFTER_MAX_MS
// bounds how long a hint is waited out, and it claims to cut only the DERIVED
// hint (a backlog over a drain rate), never one the engine fixes — which is
// only true while every fixed hint is at or under it. Either claim broken
// silently is a dashboard asking a node sooner than it said it could answer.
func TestTheDashboardRetriesOnTheEnginesOwnHints(t *testing.T) {
	t.Parallel()
	fallback := dashboardMillis(t, "UNAVAILABLE_RETRY_MS")
	if fallback != stream.HealthInterval {
		t.Errorf("the dashboard re-asks an unavailable answer with no hint after %s, "+
			"where the engine's own hint when it has nothing better is its health "+
			"tick, %s", fallback, stream.HealthInterval)
	}

	bound := dashboardMillis(t, "RETRY_AFTER_MAX_MS")
	// EVERY HINT THE ENGINE FIXES, wherever a surface the dashboard asks
	// writes one: a socket frame's `retry_after` or a 503's Retry-After.
	for _, hint := range []struct {
		what  string
		after time.Duration
	}{
		{"the health tick (a degraded posture, an unreadable store)", stream.HealthInterval},
		{"a barrier waiting on an election", statelog.ElectionRetryHint},
		{"a floor re-read on the position heartbeat", coord.ReconcileInterval},
		{"a node not yet handed a company", httpjson.NoActiveRevisionRetry},
		{"a draining node", api.DrainRetryAfter},
		{"an identity estate this node cannot read", time.Duration(auth.RetryIdentitySeconds) * time.Second},
		{"an authority this node cannot decide", time.Duration(authz.RetryUndecidedSeconds) * time.Second},
		{"a surface another writer holds", setupapi.RetryBusy},
	} {
		if hint.after > bound {
			t.Errorf("the engine tells a client to wait %s for %s, and the dashboard "+
				"asks again at %s: raise RETRY_AFTER_MAX_MS to it, or the bound meant "+
				"for a derived estimate is cutting a hint the engine fixed",
				hint.after, hint.what, bound)
		}
	}
}

// dashboardMillis is a millisecond constant the dashboard declares, as a
// duration.
func dashboardMillis(t *testing.T, name string) time.Duration {
	t.Helper()
	body, err := clientsource.Declaration(clientsource.Tree(t),
		`export const `+name+` = ([0-9_]+);`)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := strconv.ParseInt(strings.ReplaceAll(body, "_", ""), 10, 64)
	if err != nil {
		t.Fatalf("%s = %q is not a number: %v", name, body, err)
	}
	return time.Duration(ms) * time.Millisecond
}
