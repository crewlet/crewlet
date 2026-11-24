package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// AN UNRESOLVED REFERENCE IS WARNED ABOUT, NOT REFUSED, EXCEPT WHERE IT LEAVES
// A ROUTE NO ROUTE. An entry's env is resolved when it is built, and an
// optional variable left unset must not take the company down; but a route
// variable's value is what the entry's identity is built from, so one an unset
// reference left malformed (`https://${HOST}/v1`) refuses the entry, naming the
// variable, while one that resolved wholly to nothing is simply unset.
//
// Mutation: skip the route check on a built entry, or refuse every unresolved
// reference, and a row goes red.
func TestAnUnresolvedReferenceRefusesAnEntryOnlyWhereItLeavesARouteNoRoute(t *testing.T) {
	t.Parallel()
	build := func(value string) error {
		spec := config.LLMProvider{Harness: "claude-code", Model: "opus", StateDir: t.TempDir(),
			Env: map[string]string{"ANTHROPIC_BASE_URL": value, "MAX_THINKING_TOKENS": "${UNSET_TUNING}"}}
		_, err := BuildHarness("gw", spec, config.NewResolver(config.MapSource{}), nil)
		return err
	}
	if err := build("https://${UNSET_GATEWAY_HOST}/v1"); err == nil ||
		!strings.Contains(err.Error(), "ANTHROPIC_BASE_URL") {
		t.Errorf("a gateway address an unset reference left hostless built: %v", err)
	}
	if err := build("${UNSET_GATEWAY_URL}"); err != nil {
		t.Errorf("a gateway address that resolved to nothing refused its entry: %v", err)
	}
}
