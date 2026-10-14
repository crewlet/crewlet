package host_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/code/codetest"
)

// TestMain is the hook when real git starts this binary as proc-receive
// (codetest.HookCommand): every case pushes through the production relay,
// with the production hook at its other end.
func TestMain(m *testing.M) {
	if status, ok := codetest.RunHookIfAsked(); ok {
		os.Exit(status)
	}
	os.Exit(m.Run())
}
