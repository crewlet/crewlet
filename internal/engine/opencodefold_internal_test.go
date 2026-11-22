package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A POLL SERVES A SECOND ID ONLY TO AN ITEM WHOSE SERVE BUDGET HOLDS TWO
// ADMISSIONS. OpenCode's plugin admits a note again, once, under its `again`
// id where a compaction folded the first admission, so each id served is an
// admission the item's serve budget for the job must hold beside the serves the
// record holds there ([readmittable]): a person's note gets one, its job's
// own; a wake and a run's message, served once, get none.
//
// Mutation: serve `again` to every item (again-for-all), or to none, or ignore
// the serves the record holds in readmittable, and this goes red.
func TestAPollServesASecondIDOnlyToAnItemWhoseBudgetHoldsTwoAdmissions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRunOn(t, store, runs, "run-1", executor.OpenCodeName)
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder", ByKind: iam.ActorHuman})
	if err := runs.fileWake(ctx, "run-1", launch, wakeAbout("w-1", "the deploy target is staging")); err != nil {
		t.Fatal(err)
	}
	other := workingRunOn(t, store, runs, "run-2", executor.OpenCodeName)
	message := wakeAbout("m-1", "my half of ENG-1 is merged")
	message.FromRun, message.FromSeat = "run-9", "qa"
	if err := runs.fileRunMessage(ctx, "run-2", other, message); err != nil {
		t.Fatal(err)
	}

	first := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, first, 2)
	served, _ := first.Wait(ctx, 0)
	second := boundInbox(t, runs, store, "run-2", other)
	viewHolds(t, second, 1)
	messaged, _ := second.Wait(ctx, 0)
	if len(served) != 2 || len(messaged) != 1 {
		t.Fatalf("the polls answered %+v and %+v, want the note and the wake, and the run's message", served,
			messaged)
	}
	check := func(job string, notes []mcpbridge.SteerNote) {
		t.Helper()
		for _, n := range notes {
			wake := strings.HasPrefix(n.ID, "wake:")
			switch {
			case wake && n.Again != "":
				t.Errorf("the item %s, served once, was served a second id %q", n.ID, n.Again)
			case !wake && (n.Again != executor.AgainID(executor.OpenCodeName, job, n.ID) || n.Again == n.Inbox):
				t.Errorf("the note %s was served the second id %q beside %q, want its job's own", n.ID, n.Again,
					n.Inbox)
			}
		}
	}
	check(launch, served)
	check(other, messaged)
	if !slices.ContainsFunc(served, func(n mcpbridge.SteerNote) bool { return n.ID == "n-1" }) ||
		!strings.HasPrefix(messaged[0].ID, "wake:") {
		t.Fatalf("the polls answered %+v and %+v, want the note n-1 and the run's message as a wake item",
			served, messaged)
	}
	// AND THE BUDGET IS THE JOB'S, counted with the serves the record holds.
	servesTo := func(launches ...string) []sandbox.HookServed {
		var out []sandbox.HookServed
		for _, l := range launches {
			out = append(out, sandbox.HookServed{Take: "take-" + l, Launch: l})
		}
		return out
	}
	wakeItem := mustRow(t, store, "run-1").Steer.Notes
	for name, c := range map[string]struct {
		note sandbox.RunNote
		want bool
	}{
		"an unserved note":                    {sandbox.RunNote{ID: "n"}, true},
		"a note served once to the job":       {sandbox.RunNote{ID: "n", Serves: servesTo("job-1")}, true},
		"a note served twice to the job":      {sandbox.RunNote{ID: "n", Serves: servesTo("job-1", "job-1")}, false},
		"a note served to another job":        {sandbox.RunNote{ID: "n", Serves: servesTo("job-0", "job-0")}, true},
		"a wake, its one serve still unspent": {wakeItem[slices.IndexFunc(wakeItem, sandbox.RunNote.IsWake)], false},
	} {
		if got := readmittable(c.note, "job-1"); got != c.want {
			t.Errorf("%s: readmittable = %v, want %v", name, got, c.want)
		}
	}
}

// A FOLD IS SAID ONCE A NOTE, BY ITS ID, AND NEVER WITH ITS WORDS: the plugin's
// post of a note it gave up after a second compaction is logged at WARN naming
// the note, its job and its turn and why it waits for the review; a repeat says
// nothing more, an id the job does not hold says nothing at all, and no line
// carries a word of the note.
//
// Mutation: log the note's text, log every repeat, or log an id the job does
// not hold, and this goes red.
func TestAFoldIsSaidOnceANoteByItsIDAndNeverWithItsWords(t *testing.T) {
	logs := captureEngineLogs(t)
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRunOn(t, store, runs, "run-1", executor.OpenCodeName)
	const words = "deploy to the staging cluster first"
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: words, By: "founder", ByKind: iam.ActorHuman})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, inbox, 1)
	inbox.Folded(ctx, []string{"n-1", "planted"})
	inbox.Folded(ctx, []string{"n-1"})
	said := logs.about("steer_note_folded", "note_id", "n-1")
	if len(said) != 1 || said[0]["level"] != "WARN" || said[0]["launch_id"] != launch ||
		said[0]["turn_id"] != "run-1" || !strings.Contains(said[0]["detail"].(string), "compacted twice") ||
		!strings.Contains(said[0]["detail"].(string), "waits for the review") {
		t.Errorf("the fold was said %v, want once at WARN naming the job, the turn and why", said)
	}
	if planted := logs.about("steer_note_folded", "note_id", "planted"); len(planted) != 0 {
		t.Errorf("a fold of a note the job does not hold was said: %v", planted)
	}
	logs.mu.Lock()
	all := logs.buf.String()
	logs.mu.Unlock()
	if strings.Contains(all, words) || strings.Contains(all, "staging cluster") {
		t.Errorf("the log carries the note's words:\n%s", all)
	}
}
