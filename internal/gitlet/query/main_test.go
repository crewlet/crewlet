package query_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
)

// TestMain is the proc-receive hook when real git starts this binary as one
// (gitlettest.HookCommand), so every case pushes through the production relay
// and hook. After the run it removes the histories BenchmarkBlame and
// BenchmarkLogPath share, which outlive the benchmark that built them.
func TestMain(m *testing.M) {
	if status, ok := gitlettest.RunHookIfAsked(); ok {
		os.Exit(status)
	}
	status := m.Run()
	removeHistoryRigs()
	os.Exit(status)
}
