package sandbox

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A LAUNCH CONTINUES A SESSION ONLY UNDER THE ENTRY IT RAN UNDER, on a harness
// not measured to resume on its launch's route: the turn's kept capture, whose
// row names the entry it ran under, and the copy its job's own box holds,
// whose job's record does. Under another entry, or beside an entry no record
// names, the launch starts a session of its own; a launch-routed harness
// continues either under any entry.
//
// Mutation: drop the entry from the kept path's restorer, from the in-place
// branch, take the launch's entry for the session's, or skip the runner's
// trait, and a case goes red.
func TestALaunchContinuesASessionOnlyUnderTheEntryItRanUnder(t *testing.T) {
	t.Parallel()
	const ran, other = "entry-primary", "entry-secondary"
	for name, tc := range map[string]struct {
		launchRouted bool
		// inPlace offers the job's own box's copy in place of the kept capture.
		inPlace          bool
		recorded, launch string
		continues        bool
	}{
		"the kept capture, under its own entry":       {recorded: ran, launch: ran, continues: true},
		"the kept capture, under another entry":       {recorded: ran, launch: other},
		"the kept capture, naming no entry":           {launch: ran},
		"the kept capture, launch-routed, another":    {launchRouted: true, recorded: ran, launch: other, continues: true},
		"the box's own copy, under its own entry":     {inPlace: true, recorded: ran, launch: ran, continues: true},
		"the box's own copy, under another entry":     {inPlace: true, recorded: ran, launch: other},
		"the box's own copy, launch-routed, another":  {inPlace: true, launchRouted: true, recorded: ran, launch: other, continues: true},
		"the box's own copy, its job naming no entry": {inPlace: true, launch: ran},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			rig.coordinator.spoolDir = t.TempDir()
			rig.runner.Carries = true
			rig.runner.EntryBound = !tc.launchRouted
			key := objstore.KeyAt(time.Now())
			kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, Final: true,
				Through: new(0), Object: key.String(), ObjectHash: strings.Repeat("a", 64), ObjectSize: 14,
				SessionID: "s-1", Entry: tc.recorded}
			row := keptRow(t, kept)
			row.Entry = tc.recorded
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": row},
				archives: map[string][]byte{key.String(): []byte("the transcript")}}
			opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2, Capture: &kept, Kept: &kept}
			req := continuingKept(kept)
			req.Entry = tc.launch
			want := BriefContinuedNewBox
			if tc.inPlace {
				req.Session = nil
				req.Continue = &Continuation{SessionID: "s-1", Brief: "carry the box's copy on"}
				opened.Prior.Entry = tc.recorded
				want = BriefContinued
			}
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.inPlace, false, RunSources{})
			t.Cleanup(got.release)
			switch {
			case tc.continues && (got.kind != want || got.session != "s-1"):
				t.Errorf("the launch hands %s on session %q, want %s on s-1", got.kind, got.session, want)
			case !tc.continues && (got.kind != BriefFresh || got.session != ""):
				t.Errorf("the launch under entry %q continues a session that ran under %q: %s on %q",
					tc.launch, tc.recorded, got.kind, got.session)
			}
		})
	}
}

// AN ENTRY CONTINUES ONLY ITS OWN SESSIONS, unless its harness resumes on its
// launch's route; and an entry nothing recorded matches none, even an
// unrecorded launch's.
//
// Mutation: compare the entries otherwise, let an empty one match, or ignore
// the trait, and a row goes red.
func TestAnEntryContinuesOnlyItsOwnSessions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ran, launch  string
		launchRouted bool
		continues    bool
	}{
		{"a", "a", false, true},
		{"a", "b", false, false},
		{"", "", false, false},
		{"", "a", false, false},
		{"a", "b", true, true},
		{"", "a", true, true},
	} {
		ok, why := EntryContinues(tc.ran, tc.launch, "claude-code", tc.launchRouted)
		if ok != tc.continues || ok != (why == "") || (!ok && !strings.Contains(why, "claude-code")) {
			t.Errorf("EntryContinues(%q, %q, routed %v) = %v, %q; want %v", tc.ran, tc.launch, tc.launchRouted,
				ok, why, tc.continues)
		}
	}
}
