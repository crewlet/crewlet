package subagent_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/mcprelay"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// sessions is this test binary's one session host, as the engine's process has
// one: every worker the suite runs serves its tools on it.
var sessions *toolsession.Host

func TestMain(m *testing.M) {
	// FIRST, before anything else runs: a CLI told to start this binary as a
	// worker's relay starts it with `mcp-relay` and nothing else.
	mcprelay.DispatchIfRelay()
	host, err := toolsession.NewHost()
	if err != nil {
		fmt.Fprintln(os.Stderr, "subagent tests:", err)
		os.Exit(1)
	}
	sessions = host
	code := m.Run()
	_ = host.Close()
	os.Exit(code)
}
