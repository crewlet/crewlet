package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
)

// TestMain silences the suite and, more to the point, OWNS THE PROCESS SINK.
//
// Every test here calls run(), and run() is handed a writer so its output can
// be asserted on. A command that installed that writer as the process-wide
// log destination would make the global depend on whichever test configured
// it last — see [TestACommandLogsToTheProcessSinkNotItsCallersWriter].
// Installing io.Discard once, here, is what a process legitimately does.
func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError+1, logging.FormatText, io.Discard)
	os.Exit(m.Run())
}

// unresolvedBootstrap is a Tier A document whose ${VAR} nothing answers,
// which is what makes config.LogUnresolved emit a warning during validate.
// The log line is the whole point: it is the one thing a command writes that
// does not go through the writers it was handed.
func unresolvedBootstrap(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boot.yaml")
	body := "node:\n  id: n1\nstore:\n  path: \"${NOT_SET_ANYWHERE}\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A COMMAND LOGS TO THE PROCESS'S SINK, NEVER TO THE WRITER IT WAS HANDED.
//
// `run` takes stdout and stderr so a test can read what a command printed.
// It used to also install that stderr as the process-wide slog destination,
// which is a different thing entirely: log lines are the PROCESS's
// telemetry, and pointing the global at a caller's buffer makes every other
// caller's lines land there too.
//
// Asserted on where the bytes go, not with the race detector, so it fails on
// a plain `go test` as well as under -race.
func TestACommandLogsToTheProcessSinkNotItsCallersWriter(t *testing.T) {
	var sink bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &sink)
	t.Cleanup(func() {
		logging.Configure(slog.LevelError+1, logging.FormatText, io.Discard)
	})

	var out, errOut bytes.Buffer
	_ = run([]string{"validate", unresolvedBootstrap(t)}, &out, &errOut)

	if !strings.Contains(sink.String(), "NOT_SET_ANYWHERE") {
		t.Fatalf("the warning did not reach the process sink, so this test "+
			"is asserting nothing: sink=%q", sink.String())
	}
	if strings.Contains(errOut.String(), "NOT_SET_ANYWHERE") {
		t.Errorf("the command wrote a log line into the writer it was handed; "+
			"under `go test` that writer belongs to ONE test and every other "+
			"test's lines would land in it too: %q", errOut.String())
	}
}

// AND TWO COMMANDS AT ONCE DO NOT WRITE THE SAME BUFFER.
//
// The shape CI actually hit: 29 parallel tests in this package each call
// run() with their own bytes.Buffer, one of them writes usage() into its
// buffer directly while another's slog line is routed into the same one.
// This forces the overlap so -race adjudicates it rather than leaving it to
// how a runner happens to schedule.
func TestConcurrentCommandsDoNotShareABuffer(t *testing.T) {
	t.Parallel()
	cfg := unresolvedBootstrap(t)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			var out bytes.Buffer
			if i%2 == 0 {
				_ = run([]string{"help"}, &out, &out)
				return
			}
			_ = run([]string{"validate", cfg}, &out, &out)
		})
	}
	wg.Wait()
}
