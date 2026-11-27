package executor_test

import (
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// EACH JOB START LEAVES ITS PREDECESSOR'S STREAMS WHOLE, on a real box: a read
// of the last job's event and error streams still open when [executor.Runner]
// starts the next job (Start, which every launch, relaunch, resume and retry
// goes through) reads the last job's bytes, and its file tells unchanged
// ([sandbox.ChangeOf]), since the start's clear removes both streams before the
// job's `>` and `2>` make new ones, which a `>` into the old file would cut.
//
// Mutation: leave either stream out of the clear.
func TestEachJobStartLeavesItsPredecessorsStreamsWhole(t *testing.T) {
	t.Parallel()
	runner, box := realBox(t)
	p := executor.PathsFor(box)
	harness := strings.Replace(sessionHarness("2.1.310"), "for a; do",
		"echo 'a note on the error stream' >&2\nfor a; do", 1)
	writeBoxStandIn(t, box, p.BinDir()+"/claude", harness)
	streams := []string{p.Stream(), p.Err()}

	first, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "begin"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitOver(t, runner, box, first, 15*time.Second, "the stand-in answers at once")
	var opened []io.ReadCloser
	var was []string
	for _, path := range streams {
		was = append(was, boxFile(t, box, path))
		body, _, err := box.OpenRange(t.Context(), path, 0, math.MaxInt64)
		if err != nil {
			t.Fatalf("OpenRange %s: %v", path, err)
		}
		t.Cleanup(func() { _ = body.Close() })
		opened = append(opened, body)
	}
	if was[0] == "" || was[1] == "" {
		t.Fatalf("the first job left an empty stream (event %q, error %q), so a cut would not show", was[0], was[1])
	}

	second, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "begin"})
	if err != nil {
		t.Fatalf("the second Start: %v", err)
	}
	awaitOver(t, runner, box, second, 15*time.Second, "the stand-in answers at once")
	for i, body := range opened {
		got, err := io.ReadAll(body)
		if err != nil {
			t.Fatalf("read %s: %v", streams[i], err)
		}
		change, err := sandbox.ChangeOf(body)
		if err != nil || change != sandbox.FileUnchanged || string(got) != was[i] {
			t.Errorf("a read of the last job's %s, open over the next start, read %q and tells %q (%v); "+
				"want %q, unchanged", streams[i], got, change, err, was[i])
		}
	}
}
