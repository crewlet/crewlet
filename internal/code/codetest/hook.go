package codetest

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// HookCommand is the argv a suite's cache execs as proc-receive: THIS test
// binary re-executed as host.HookCommand(<test binary>) — the production
// spelling — under /usr/bin/env with GORACE set to procgrouptest's stand-in
// options, because the pool passes no GORACE and a race-built stand-in
// otherwise sleeps a second at every exit: a second a push.
func HookCommand(t testing.TB) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("codetest: this test binary's path: %v", err)
	}
	return append([]string{"/usr/bin/env", procgrouptest.GORACE + "=" + procgrouptest.StandInRaceOptions()},
		host.HookCommand(exe)...)
}

// RunHookIfAsked is the first statement of the TestMain of every package that
// pushes for real: when this process was started as the hook —
// host.HookArgs recognises its arguments, the same function main() asks — it
// runs host.RunHook and reports its exit status; otherwise ok is false and
// the tests run.
func RunHookIfAsked() (status int, ok bool) {
	rest, ok := host.HookArgs(os.Args[1:])
	if !ok {
		return 0, false
	}
	return host.RunHook(rest, os.Stdin, os.Stdout, os.Stderr, os.Getenv), true
}
