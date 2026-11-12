package query_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
)

// TestMain is the hook when real git starts this binary as proc-receive
// (gitlettest.HookCommand): every case's repository is pushed through the
// production relay, with the production hook at its other end. Once every
// case and benchmark has run, it removes the histories BenchmarkBlame and
// BenchmarkLogPath share, which outlive the benchmark that built them.
func TestMain(m *testing.M) {
	if status, ok := gitlettest.RunHookIfAsked(); ok {
		os.Exit(status)
	}
	status := m.Run()
	removeHistoryRigs()
	os.Exit(status)
}
