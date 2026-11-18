package sandbox

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
)

// A RUN'S START IS KNOWN ONLY WHOLE: it began its session, or began after a
// message id a box may name, never both. The zero value, an id past its bound
// or holding a character no box's id holds, and both together are not known.
//
// Mutation: read the zero value or Began beside an id as known, or let an id
// past its bound pass, and a row goes red.
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
	} {
		if got := tc.start.Known(); got != tc.known {
			t.Errorf("%s: %+v known %v, want %v", name, tc.start, got, tc.known)
		}
	}
}

// A CAPTURE KEEPS WHERE ITS RUN BEGAN: the take puts the box's start on the
// stamp and the row's kept capture carries it, and a start past a message id's
// bound is refused before the row carries it.
//
// Mutation: leave the start off the stamp, or off Bounded's fields, and a check
// goes red.
func TestACaptureKeepsWhereItsRunBegan(t *testing.T) {
	t.Parallel()
	for name, start := range map[string]RunStart{
		"a run that began its session": {Began: true},
		"a run that continued one":     {After: "msg_1d8699fae001Qp6E4C1MPr6kP4"},
		"a run whose start is unknown": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, _ := keepingRig(t)
				rig.runner.Session.Archive.RunStart = start
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
	} {
		raw, err := json.Marshal(SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded,
			RunBegan: start.Began, RunAfter: start.After})
		if err != nil {
			t.Fatal(err)
		}
		declared := wireKeys(reflect.TypeFor[SessionCapture]())
		if !declared["run_began"] || !declared["run_after"] {
			t.Fatalf("the capture declares no keys run_began and run_after: %v", declared)
		}
		older := maps.Clone(declared)
		delete(older, "run_began")
		delete(older, "run_after")
		type plain SessionCapture
		var read plain
		extra, err := unmarshalCarrying(raw, &read, older)
		if err != nil {
			t.Fatal(err)
		}
		read.RunBegan, read.RunAfter, read.CaptureSeq = false, "", 2
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
