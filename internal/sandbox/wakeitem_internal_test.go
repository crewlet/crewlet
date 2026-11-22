package sandbox

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A LATER BUILD'S KEYS IN A WAKE ITEM RIDE THIS BUILD'S WRITE. The item's wakes
// are a section of the record a later build may add fields to, at the item's
// level ([WakeItem.Extra]) and at each wake's ([SealedWake.Extra]): a node of
// this build reads and writes the record whole on every write, so a key it does
// not know survives only where its decode kept it. And the item is a note, whose
// own Extra carries `wake` through an older build that does not know the field.
//
// Mutation: drop WakeItem's or SealedWake's codec, and that level's key is gone
// after one write.
func TestAWakeItemKeepsALaterBuildsKeysThroughThisBuildsWrite(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"id":"wake:job-2:w-1","seq":4,"sealed":null,"at":"2026-11-20T09:00:00Z","for":"job-2",` +
		`"wake":{"wakes":[{"id":"w-1","sealed":"c2VhbA==","at":"2026-11-20T09:00:00Z","origin_node":"n-7"}],` +
		`"router":"r-1"}}`)
	var note RunNote
	if err := json.Unmarshal(raw, &note); err != nil {
		t.Fatal(err)
	}
	if !note.IsWake() || len(note.Wake.Wakes) != 1 || note.Wake.Wakes[0].ID != "w-1" ||
		string(note.Wake.Wakes[0].Sealed) != "seal" || note.For != "job-2" {
		t.Fatalf("the item decodes as %+v, want its one wake for the job", note)
	}
	written, err := json.Marshal(note)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Wake struct {
			Router json.RawMessage `json:"router"`
			Wakes  []struct {
				Origin json.RawMessage `json:"origin_node"`
			} `json:"wakes"`
		} `json:"wake"`
	}
	if err := json.Unmarshal(written, &back); err != nil {
		t.Fatal(err)
	}
	if string(back.Wake.Router) != `"r-1"` || len(back.Wake.Wakes) != 1 || string(back.Wake.Wakes[0].Origin) != `"n-7"` {
		t.Errorf("this build wrote the item as %s: a later build's keys at the item and its wake are lost", written)
	}
}

// EACH WAKE ENDS AS ITS RECORD KNOWS IT: delivered where a step read it,
// unconfirmed where it was served to its job's harness and no read of the
// job's session showed it taken (a live serve, a poll's, or a lost serve its
// session may still hold), unread where nothing served it or its every serve is
// proven absent; and a note or a late call's item is no wake at all.
//
// Mutation: report a wake served and lost to a cancel as unread
// (cancelled-is-unread), and its delivery is reported left untold though its
// session may hold it.
func TestEachWakeEndsAsItsRecordKnowsIt(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	wake := func() RunNote {
		return RunNote{ID: "wake:job-1:w-1", For: "job-1", Wake: &WakeItem{Wakes: []SealedWake{{ID: "w-1"}}}}
	}
	serve := func(lost ServeLoss) HookServed {
		return HookServed{Take: "take-1", Launch: "job-1", Boundary: "tool_batch", At: at, Lost: lost}
	}
	read, live, polled, cancelled, absent := wake(), wake(), wake(), wake(), wake()
	read.Read = &NoteRead{At: at, Order: 3, Boundary: "tool_batch", Launch: "job-1", Take: "take-1"}
	live.Served, live.Serves = "job-1", []HookServed{serve("")}
	polled.Served = "job-1"
	cancelled.Serves = []HookServed{serve(ServeCancelled)}
	absent.Serves = []HookServed{serve(ServeAbsent)}
	for name, tc := range map[string]struct {
		n    RunNote
		want types.JoinOutcome
	}{
		"read by a step":                  {read, types.JoinDelivered},
		"served by a hook, live":          {live, types.JoinUnconfirmed},
		"served by a poll":                {polled, types.JoinUnconfirmed},
		"served, its serve cancelled":     {cancelled, types.JoinUnconfirmed},
		"served, its serve proven absent": {absent, types.JoinUnread},
		"never served":                    {wake(), types.JoinUnread},
		"a person's note":                 {RunNote{ID: "n-1"}, ""},
		"a late call's item": {RunNote{ID: "call:job-1:1", For: "job-1",
			LateCall: &LateCall{Round: "job-1"}}, ""},
	} {
		if got := tc.n.WakeEnd(); got != tc.want {
			t.Errorf("%s: the wake ends %q, want %q", name, got, tc.want)
		}
	}
}

// A CAPTURE'S NOTE CURSOR STOPS AT A WAKE ITS HOOK SERVED, as at a note: the
// harness may hold the wake, and a continuation of that capture is told it
// again only where the cursor stops below its place; a late call's item stops
// nothing, its own telling deciding it ([CallTelling]).
//
// Mutation: pass over every item for one job in notesHeld, and the capture
// counts the served wake held.
func TestACapturesNoteCursorStopsAtAWakeItsHookServed(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	read := &NoteRead{At: at, Order: 1, Boundary: "brief", Launch: "job-1"}
	note := RunNote{ID: "n-1", Seq: 1, Read: read}
	served := HookServed{Take: "take-1", Launch: "job-1", Boundary: "tool_batch", At: at}
	wake := RunNote{ID: "wake:job-1:w-1", Seq: 2, For: "job-1", Place: 2, Served: "job-1",
		Serves: []HookServed{served}, Wake: &WakeItem{Wakes: []SealedWake{{ID: "w-1"}}}}
	late := RunNote{ID: "call:job-1:1", Seq: 2, For: "job-1", Place: 2, Served: "job-1",
		Serves: []HookServed{served}, LateCall: &LateCall{Round: "job-1"}}
	for name, tc := range map[string]struct {
		notes []RunNote
		want  int
	}{
		"a wake its hook served": {[]RunNote{note, wake}, 1},
		"a late call's item":     {[]RunNote{note, late}, 3},
	} {
		run := PendingRun{Steer: RunSteer{Reads: 3, Notes: tc.notes}}
		if got := notesHeld(run, false); got != tc.want {
			t.Errorf("%s: the cursor is %d, want %d", name, got, tc.want)
		}
	}
}

// A RELAUNCH KEEPS THE WAKES ITS JOB READ, AND REMOVES THE REST WITH THE JOB. A
// wake read is the turn's, its delivery worked, so every later launch tells it;
// one never read is the job's alone, whose delivery runs as a turn of its own,
// so the next job is handed none of it. A late call's item goes with its job
// as before.
//
// Mutation: keep every wake item (wakes-outlive-their-job) or remove the read
// ones too, and this goes red.
func TestARelaunchKeepsTheWakesItsJobReadAndRemovesTheRest(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	item := func(id string) RunNote {
		return RunNote{ID: id, For: "job-1", Wake: &WakeItem{Wakes: []SealedWake{{ID: id}}}}
	}
	read, served, unread := item("w-read"), item("w-served"), item("w-unread")
	read.Read = &NoteRead{At: at, Order: 1, Boundary: "step", Launch: "job-1"}
	served.Served = "job-1"
	late := RunNote{ID: "call:job-1:1", For: "job-1", LateCall: &LateCall{Round: "job-1"}}
	notes := []RunNote{read, served, unread, late}
	if got := idsOf(unreadWakes(notes)); !slices.Equal(got, []string{"w-served", "w-unread"}) {
		t.Errorf("the job's end reports %v, want the wakes it never read", got)
	}
	steer := RunSteer{Notes: notes, Reads: 1}
	openNotes(&steer, PendingRun{}, "job-2", at)
	if got := idsOf(steer.Notes); !slices.Equal(got, []string{"w-read"}) {
		t.Errorf("the next job's record holds %v, want the read wake alone", got)
	}
	if steer.Notes[0].IsFor("job-2") {
		t.Error("the read wake is handed to the next job")
	}
}

// idsOf is each note's id, in order.
func idsOf(notes []RunNote) []string {
	out := []string{}
	for _, n := range notes {
		out = append(out, n.ID)
	}
	return out
}
