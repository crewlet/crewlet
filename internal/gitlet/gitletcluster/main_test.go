package gitletcluster_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/solo"
)

func TestMain(m *testing.M) {
	// THIS BINARY IS ALSO THE HOOK: a member's cache execs it as
	// proc-receive (gitlettest.HookCommand).
	if status, ok := gitlettest.RunHookIfAsked(); ok {
		os.Exit(status)
	}
	// THE RUNNER TO ITSELF: three members, each embedding a NATS server, in
	// one process (internal/solo).
	os.Exit(solo.Run(m))
}
