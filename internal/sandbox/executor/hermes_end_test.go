package executor_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser/boxhelper/live"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// AN ENDED HERMES RUN'S PATH FITS THE ENGINE'S WAIT, as OpenCode's does
// (TestAnEndedRunsPathFitsTheEnginesWait): the gateway's wait, the client's
// grace and the pack sum to no more than [sandbox.JobEndWait], so the read
// that ends a job never kills a Hermes script packing on its way out. The
// job's browser relay adds nothing to it: the script's end TERMs the relay
// before it stops the gateway and the client, and the relay leaves within its
// own bound, below the grace, while they stop.
//
// Mutations, each red: raise hermesGatewayStop to 15 s; TERM the relay after
// `end "$harness"`; set live.TermMS to 5000.
func TestAnEndedHermesRunsPathFitsTheEnginesWait(t *testing.T) {
	t.Parallel()
	if path := executor.HermesEndPath(); path > sandbox.JobEndWait {
		t.Errorf("an ended Hermes run's path takes up to %s, past the %s the engine waits for an ended job",
			path, sandbox.JobEndWait)
	}
	session := executor.Hermes{}.Session(sandbox.RunRequest{})
	script := scriptOf(t, executor.Hermes{}.Command(sandbox.RunRequest{}, session, executor.Paths{Home: "/box"}, ""))
	_, end, ok := strings.Cut(script, "endharness() {")
	end, _, _ = strings.Cut(end, "}\n")
	relay, gateway, client := strings.Index(end, `kill "$relay"`), strings.Index(end, "endgw"),
		strings.Index(end, `end "$harness"`)
	if !ok || relay < 0 || relay > gateway || gateway > client {
		t.Errorf("the script's end does not TERM the relay before it stops the gateway and the client:\n%s", end)
	}
	if term := time.Duration(live.TermMS) * time.Millisecond; term >= sandbox.JobStopGrace {
		t.Errorf("a relay TERMed is given %s to leave, not below the %s grace its job's processes get", term,
			sandbox.JobStopGrace)
	}
}
