package host_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
)

// TestMain is the hook when real git starts this binary as proc-receive
// (gitlettest.HookCommand): every case pushes through the production relay,
// with the production hook at its other end.
func TestMain(m *testing.M) {
	if status, ok := gitlettest.RunHookIfAsked(); ok {
		os.Exit(status)
	}
	os.Exit(m.Run())
}
