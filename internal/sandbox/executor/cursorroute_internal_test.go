package executor

import (
	"slices"
	"testing"
)

// A CURSOR RUN'S ROUTE IS WHAT ITS RELEASE READS: the variables the
// 2026.11.22-e9042b2 bundle reads for where a call goes, how it is signed and
// which flags shape it, kept here as read and the runner's declaration held to
// them, so a name dropped from both the declaration and identity_env is
// caught, and one dropped from the script's unset list with it.
//
// Mutation: drop a name from cursorRouting, and the test goes red.
func TestACursorRunsRouteIsWhatItsReleaseReads(t *testing.T) {
	t.Parallel()
	read := []string{
		"CURSOR_AGENT_CLI_AUTHLESS_MODE", "CURSOR_AGENT_CLI_LOCAL_MODE", "CURSOR_API_BASE_URL",
		"CURSOR_API_ENDPOINT", "CURSOR_API_KEY", "CURSOR_AUTH_TOKEN", "CURSOR_STATSIG_OVERRIDES",
	}
	if got := slices.Sorted(slices.Values(routeNames(CursorAgent{}.routing()))); !slices.Equal(got, read) {
		t.Errorf("the Cursor runner declares %v, and %s reads %v", got, cursorSupported, read)
	}
}
