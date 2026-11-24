package sandbox

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
)

// A RUN'S START IS KNOWN ONLY WHOLE: it began its session, began after a
// message id a box may name, or began after a line count within its bound,
// exactly one of them. The zero value, an id past its bound or holding a
// character no box's id holds, a count at or below zero or past its bound, and
// any two together are not known.
//
// Mutation: read the zero value or Began beside an id or a count as known, or
// let an id or a count past its bound pass, and a row goes red.
func TestARunsStartIsKnownOnlyWhole(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		start RunStart
		known bool
	}{
		"the zero value":            {},
		"a run that began":          {start: RunStart{Began: true}, known: true},
		"after a message":           {start: RunStart{After: "msg_1d8699fae001Qp6E4C1MPr6kP4"}, known: true},
		"after an id at its bound":  {start: RunStart{After: strings.Repeat("a", MaxMessageIDBytes)}, known: true},
		"after an id past it":       {start: RunStart{After: strings.Repeat("a", MaxMessageIDBytes+1)}},
		"after an id holding a dot": {start: RunStart{After: "msg_a.b"}},
		"both":                      {start: RunStart{Began: true, After: "msg_a"}},
		"after a line":              {start: RunStart{Lines: 1}, known: true},
		"after lines at the bound":  {start: RunStart{Lines: MaxRunStartLines}, known: true},
		"after lines past it":       {start: RunStart{Lines: MaxRunStartLines + 1}},
		"after no lines":            {start: RunStart{Lines: -1}},
		"began and lines":           {start: RunStart{Began: true, Lines: 3}},
		"a message and lines":       {start: RunStart{After: "msg_a", Lines: 3}},
	} {
		if got := tc.start.Known(); got != tc.known {
			t.Errorf("%s: %+v known %v, want %v", name, tc.start, got, tc.known)
		}
	}
}

// A CAPTURE KEEPS WHERE ITS RUN BEGAN: the take puts the box's start on the
// stamp and the row's kept capture carries it; a chat file's line count is the
// take's, stamped LATE, once the take has read the archive whole and checked
// the box's digest of those lines, never on the begun stamp; and a start past
// its bound is refused before the row carries it.
//
// Mutation: leave the start off the stamp, or off Bounded's fields, or stamp
// the line count with the begun stamp, and a check goes red.
func TestACaptureKeepsWhereItsRunBegan(t *testing.T) {
	t.Parallel()
	for name, start := range map[string]RunStart{
		"a run that began its session": {Began: true},
		"a run that continued one":     {After: "msg_1d8699fae001Qp6E4C1MPr6kP4"},
		"a run that continued a chat":  {Lines: 17},
		"a run whose start is unknown": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, _ := keepingRig(t)
				rig.runner.Session.Archive.RunStart = RunStart{Began: start.Began, After: start.After}
				rig.runner.Session.RunLines = start.Lines
				rig.launchKept(t, "t1")
				session := rig.runner.Session
				rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1",
					HarnessVersion: "2.1.310", Captured: &session.Archive})
				collectHeld(t, rig, "t1")
				captures := writes.captured()
				if len(captures) != 1 {
					t.Fatalf("recorded %d captures, want 1", len(captures))
				}
				if got := captures[0].CaptureRecord.Stamp.RunStart(); got != start {
					t.Errorf("the capture recorded the run's start %+v, want %+v", got, start)
				}
				run := rig.get("t1")
				if run.Kept == nil || run.Kept.RunStart() != start {
					t.Errorf("the row keeps %+v, want the capture beginning %+v", run.Kept, start)
				}
			})
		})
	}
	over := SessionCapture{RunAfter: strings.Repeat("a", MaxMessageIDBytes+1)}
	if err := over.Bounded(); !errors.Is(err, ErrCaptureUnbounded) {
		t.Errorf("a stamp whose run's start is %d bytes passes its bound (%v)", len(over.RunAfter), err)
	}
	for _, lines := range []int64{MaxRunStartLines + 1, -1} {
		if err := (SessionCapture{RunAfterLines: lines}).Bounded(); !errors.Is(err, ErrCaptureUnbounded) {
			t.Errorf("a stamp whose run began after line %d passes its bound (%v)", lines, err)
		}
	}
	if err := (SessionCapture{RunAfterLines: MaxRunStartLines}).Bounded(); err != nil {
		t.Errorf("a stamp whose run began after line %d, the bound, is refused: %v", MaxRunStartLines, err)
	}
}

// AN OLDER BUILD'S WRITE CARRIES WHERE A RUN BEGAN: mid-upgrade, a build that
// does not know the start rewrites the row, so the start rides under keys of
// its own that such a build keeps verbatim and writes back (wirekeys.go).
//
// Mutation: leave the fields off the wire, and no older build's capture holds
// their keys; drop the carried keys from the capture's encode, and the start
// is gone after the older build's write.
func TestAnOlderBuildsWriteCarriesWhereARunBegan(t *testing.T) {
	t.Parallel()
	for name, start := range map[string]RunStart{
		"began": {Began: true},
		"after": {After: "msg_1d8699fae001Qp6E4C1MPr6kP4"},
		"lines": {Lines: 17},
	} {
		raw, err := json.Marshal(SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded,
			RunBegan: start.Began, RunAfter: start.After, RunAfterLines: start.Lines})
		if err != nil {
			t.Fatal(err)
		}
		declared := wireKeys(reflect.TypeFor[SessionCapture]())
		if !declared["run_began"] || !declared["run_after"] || !declared["run_after_lines"] {
			t.Fatalf("the capture declares no keys run_began, run_after and run_after_lines: %v", declared)
		}
		older := maps.Clone(declared)
		delete(older, "run_began")
		delete(older, "run_after")
		delete(older, "run_after_lines")
		type plain SessionCapture
		var read plain
		extra, err := unmarshalCarrying(raw, &read, older)
		if err != nil {
			t.Fatal(err)
		}
		read.RunBegan, read.RunAfter, read.RunAfterLines, read.CaptureSeq = false, "", 0, 2
		written, err := marshalCarrying(read, older, extra)
		if err != nil {
			t.Fatal(err)
		}
		var back SessionCapture
		if err := json.Unmarshal(written, &back); err != nil {
			t.Fatal(err)
		}
		if back.RunStart() != start || back.CaptureSeq != 2 {
			t.Errorf("%s: after the older build's write the capture begins %+v at place %d, want %+v beside "+
				"its own write: %s", name, back.RunStart(), back.CaptureSeq, start, written)
		}
	}
}
