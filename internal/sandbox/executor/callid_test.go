package executor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// EACH BOX RUNNER DECLARES THE CALL-ID KEY ITS HARNESS PROFILE READS. The bridge
// serving a box records each call's id under the `_meta` key the box's runner
// declares, so the runner's key is the one its harness sends: the key the
// harness's built-in profile reads the harness's own record by
// (`requests.call_id_meta`), measured where it was set. Claude Code sends the
// model's tool_use id under one, so its runner declares it, and it is not
// empty; OpenCode's `_meta` carries no id of a call, its profile names none,
// and its runner declares none, so every call of its runs is recorded with
// none rather than under a key it never sends.
//
// Mutation: declare no key for any runner (runner-declares-no-key), and a box
// of Claude Code's has every call recorded with no id.
func TestEachBoxRunnerDeclaresTheCallIDKeyItsHarnessProfileReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		runner *executor.Runner
		sends  bool
	}{
		{executor.NewClaudeCode(), true},
		{executor.NewOpenCode(), false},
	} {
		name := tc.runner.Name()
		profile, ok := harnessprofile.Builtin(name)
		if !ok {
			t.Fatalf("no built-in profile is named %s, the harness its runner drives", name)
		}
		var reads string
		if profile.Requests != nil {
			reads = profile.Requests.CallIDMeta
		}
		if got := tc.runner.CallIDMeta(); got != reads || (got != "") != tc.sends {
			t.Errorf("the %s runner declares the call-id key %q, and its profile reads %q: want the profile's, "+
				"which is empty only for a harness that sends no id of a call (sends one: %v)", name, got, reads,
				tc.sends)
		}
	}
}
