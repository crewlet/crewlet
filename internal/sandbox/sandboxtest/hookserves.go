package sandboxtest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A hook's serves of a person's notes, its session's verdict on them, and a
// job's items: the cases [Run] drives for them, through noteCases and
// noteRawCases.
//
// A HOOK'S SERVE IS NO READ. A harness answered its notes by a hook is served
// them by a write before its answer, and a note so served is read only once the
// job's own session shows the hook took it. Every property here is one of the
// store's conditional writes: what a serve and a settle record, the order they
// decide in, what they answer, and what a write in flight can never do after
// them.

// hookServe is a serve a run's hook makes after a batch of tool calls: pinned to
// its job while the run is launching or running, named take, answering call
// where it names one.
func hookServe(launch, take string, call *sandbox.NoteCall, ids ...string) sandbox.HookServe {
	return sandbox.HookServe{IDs: ids, Launch: launch,
		Statuses: []string{sandbox.StatusLaunching, sandbox.StatusRunning},
		Serve:    sandbox.HookServed{Take: take, Boundary: "tool_batch", Iteration: 1, Call: call}}
}

// callNo is a hook's call numbered n on the box's MCP session, as its harness
// names it when it gives the call up.
func callNo(n int) *sandbox.NoteCall {
	return &sandbox.NoteCall{Session: "mcp-session-1", ID: fmt.Sprint(n)}
}

// mustServe serves the notes ids by take, answering call, and fails the case
// unless the serve answers exactly want.
func mustServe(t *testing.T, s sandbox.PendingStore, turnID string, serve sandbox.HookServe, want ...string) {
	t.Helper()
	served, err := s.ServeHook(t.Context(), turnID, serve)
	if err != nil || !slices.Equal(idsOf(served), want) {
		t.Fatalf("serve %s of %v = %v, %v, want %v served", serve.Serve.Take, serve.IDs, idsOf(served), err, want)
	}
}

// mustSettle settles a job's serves and answers what the settle answered.
func mustSettle(t *testing.T, s sandbox.PendingStore, turnID string, settle sandbox.HookSettle) []sandbox.RunNote {
	t.Helper()
	settled, err := s.SettleHook(t.Context(), turnID, settle)
	if err != nil {
		t.Fatalf("SettleHook %+v: %v", settle, err)
	}
	return settled
}

// idsOf is the ids of notes, in their order.
func idsOf(notes []sandbox.RunNote) []string {
	out := []string{}
	for _, n := range notes {
		out = append(out, n.ID)
	}
	return out
}

// notesByID is turnID's record's notes, by id.
func notesByID(t *testing.T, s sandbox.PendingStore, turnID string) map[string]sandbox.RunNote {
	t.Helper()
	out := map[string]sandbox.RunNote{}
	for _, n := range mustGet(t, s, turnID).Steer.Notes {
		out[n.ID] = n
	}
	return out
}

// servesOf is n's serves, each as "take" or "take:loss".
func servesOf(n sandbox.RunNote) []string {
	out := []string{}
	for _, h := range n.Serves {
		if h.Lost != "" {
			out = append(out, h.Take+":"+string(h.Lost))
			continue
		}
		out = append(out, h.Take)
	}
	return out
}

// rowVersion is the version turnID's raw record is at.
func rowVersion(t *testing.T, runs coord.SandboxRuns, turnID string) uint64 {
	t.Helper()
	record, found, err := runs.SandboxRun(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("read the raw row of %s: %v %v", turnID, found, err)
	}
	return record.Version
}

// jobItem is an item for one job, filed as its filer files one.
func jobItem(id, job string) sandbox.RunNote {
	n := sealedNote(id)
	n.For = job
	return n
}

// fileItem files an item for job under the turn's bounds, and answers what
// became of it.
func fileItem(ctx context.Context, s sandbox.PendingStore, turnID, id, job string) (sandbox.NoteFiled, error) {
	filed, _, err := s.FileNote(ctx, turnID, sandbox.NoteFiling{Note: jobItem(id, job), MaxWaiting: maxWaiting,
		MaxNotes: maxNotes})
	return filed, err
}

// A HOOK'S SERVE RECORDS THE SERVE AND NO READ. A harness answered its notes by
// a hook is served them in a write before its answer, and nothing records them
// read: only its job's own session says whether the hook took them. So a serve
// leaves each note waiting and served to the job, its serve appended; the
// first serve of a note places it among the turn's reads, one place a write
// whatever it serves; a serve answers only what it served, never a note
// live-served to the job already; a note is served to one job at most
// [sandbox.MaxNoteServes] times, at the place its first serve took; and a serve
// is pinned as a take is, a serve naming no job, status or take refused.
//
// Mutation: record a read at the serve, serve a note live-served to the job,
// ignore the bound or stop one short of it, place a note served again, or give
// each note of one write a place of its own, and a check goes red.
func testAHooksServeRecordsTheServeAndNoRead(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-hook-serve"
	launch := mustLaunched(t, s, run(turn))
	for _, id := range []string{"n-1", "n-2", "n-3"} {
		mustFile(t, s, turn, id)
	}
	mustServe(t, s, turn, hookServe(launch, "take-1", callNo(1), "n-1", "n-2"), "n-1", "n-2")
	row := mustGet(t, s, turn)
	if row.Steer.Reads != 1 {
		t.Errorf("the record counts %d places after one serve, want 1", row.Steer.Reads)
	}
	for _, n := range row.Steer.Notes[:2] {
		if !n.Waiting() || n.Served != launch || n.Place != 1 || len(n.Serves) != 1 {
			t.Fatalf("note %s is %+v after its serve, want it waiting, served to job %s, at place 1, with one serve",
				n.ID, n, launch)
		}
		if h := n.Serves[0]; h.Take != "take-1" || h.Launch != launch || h.Boundary != "tool_batch" ||
			h.Iteration != 1 || h.At.IsZero() || h.Lost != "" || h.Call == nil || h.Call.ID != "1" {
			t.Errorf("note %s's serve is %+v, want take-1's to job %s after a tool batch, its call kept", n.ID, h,
				launch)
		}
	}
	// A NOTE LIVE-SERVED TO THE JOB IS NOT SERVED AGAIN, and a serve answers
	// only what it served.
	mustServe(t, s, turn, hookServe(launch, "take-2", callNo(2), "n-1", "n-3"), "n-3")
	notes := notesByID(t, s, turn)
	if n := notes["n-3"]; n.Place != 2 || mustGet(t, s, turn).Steer.Reads != 2 {
		t.Errorf("the note a second write served is at place %d, want 2", n.Place)
	}
	if got := servesOf(notes["n-1"]); !slices.Equal(got, []string{"take-1"}) {
		t.Errorf("a note live-served to the job is served %v after another serve named it, want take-1 alone", got)
	}
	// SERVED AGAIN ONCE A CANCEL LOST ITS LAST SERVE, at most MaxNoteServes
	// times, always at its first place.
	want := []string{"take-1"}
	for k := 3; k < 3+sandbox.MaxNoteServes; k++ {
		take := fmt.Sprintf("take-%d", k)
		serve := hookServe(launch, take, callNo(k), "n-1")
		serve.Cancelled = []sandbox.NoteCall{*callNo(k - 1)}
		if k == 3 {
			serve.Cancelled = []sandbox.NoteCall{*callNo(1)}
		}
		served, err := s.ServeHook(ctx, turn, serve)
		if err != nil {
			t.Fatal(err)
		}
		want[len(want)-1] += ":" + string(sandbox.ServeCancelled)
		if len(want) < sandbox.MaxNoteServes {
			want = append(want, take)
			if !slices.Equal(idsOf(served), []string{"n-1"}) {
				t.Errorf("serve %d of a note whose last serve a cancel lost answered %v, want it served", len(want),
					idsOf(served))
			}
		} else if len(served) != 0 {
			t.Errorf("a note served %d times to one job was served again: %v", sandbox.MaxNoteServes, idsOf(served))
		}
		n := notesByID(t, s, turn)["n-1"]
		if got := servesOf(n); !slices.Equal(got, want) || n.Place != 1 || !n.Waiting() {
			t.Fatalf("after serve %s the note is served %v at place %d (waiting %v), want %v at place 1", take, got,
				n.Place, n.Waiting(), want)
		}
	}
	if reads := mustGet(t, s, turn).Steer.Reads; reads != 2 {
		t.Errorf("serving a placed note again moved the record's count of places to %d, want 2", reads)
	}

	// PINNED AS A TAKE IS: each of these serves nothing, and answers no error.
	notServed := func(t *testing.T, turnID string, serve sandbox.HookServe, why string) {
		t.Helper()
		served, err := s.ServeHook(t.Context(), turnID, serve)
		if err != nil || len(served) != 0 {
			t.Fatalf("%s = %v, %v, want nothing served", why, idsOf(served), err)
		}
		if n := notesByID(t, s, turnID)["n-1"]; len(n.Serves) != 0 {
			t.Fatalf("%s served the note: %+v", why, n.Serves)
		}
	}
	t.Run("a replaced job", func(t *testing.T) {
		previous := mustLaunched(t, s, run("t-hook-pin-job"))
		mustFile(t, s, "t-hook-pin-job", "n-1")
		mustClaim(t, s, "t-hook-pin-job")
		mustBeginLaunch(t, s, run("t-hook-pin-job"))
		notServed(t, "t-hook-pin-job", hookServe(previous, "take-1", nil, "n-1"), "the replaced job's serve")
	})
	t.Run("a claimed run", func(t *testing.T) {
		job := mustLaunched(t, s, run("t-hook-pin-claimed"))
		mustFile(t, s, "t-hook-pin-claimed", "n-1")
		mustClaim(t, s, "t-hook-pin-claimed")
		notServed(t, "t-hook-pin-claimed", hookServe(job, "take-1", nil, "n-1"), "a serve once the run was claimed")
	})
	t.Run("a decided ending", func(t *testing.T) {
		job := mustLaunched(t, s, run("t-hook-pin-ending"))
		mustFile(t, s, "t-hook-pin-ending", "n-1")
		decide(t, s, "t-hook-pin-ending", everyJob(sandbox.Fence{}, sandbox.Active))
		notServed(t, "t-hook-pin-ending", hookServe(job, "take-1", nil, "n-1"), "a serve after the ending")
	})
	t.Run("a parked run", func(t *testing.T) {
		job := mustLaunched(t, s, run("t-hook-pin-park"))
		mustFile(t, s, "t-hook-pin-park", "n-1")
		park(t, s, "t-hook-pin-park")
		notServed(t, "t-hook-pin-park", hookServe(job, "take-1", nil, "n-1"), "a serve of a parked run")
	})
	t.Run("a run with no record", func(t *testing.T) {
		served, err := s.ServeHook(t.Context(), "t-hook-pin-gone", hookServe("job-gone", "take-1", nil, "n-1"))
		if err != nil || len(served) != 0 {
			t.Errorf("a serve on no record = %v, %v, want nothing and no error", idsOf(served), err)
		}
	})
	for name, serve := range map[string]sandbox.HookServe{
		"no job":    {IDs: []string{"n-1"}, Statuses: []string{sandbox.StatusRunning}, Serve: sandbox.HookServed{Take: "t"}},
		"no status": {IDs: []string{"n-1"}, Launch: launch, Serve: sandbox.HookServed{Take: "t"}},
		"no take":   {IDs: []string{"n-1"}, Launch: launch, Statuses: []string{sandbox.StatusRunning}},
	} {
		if _, err := s.ServeHook(ctx, turn, serve); err == nil {
			t.Errorf("a serve naming %s was answered", name)
		}
	}
}

// A SERVE LOSES THE CALLS ITS INBOX HEARD CANCELLED FIRST. A harness that gave
// a hook's call up never took what it was answered, and the inbox that served
// it heard the cancel: the job's next serve loses that serve and serves the
// note again in ONE write, whatever the inbox's view of the record or a settle
// off the hook's path has caught up with, at the note's own place.
//
// Mutation: ignore the serve's cancelled calls, or lose a serve whose call was
// not named, and a check goes red.
func testAServeLosesTheCallsItsInboxHeardCancelledFirst(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-serve-cancelled"
	launch := mustLaunched(t, s, run(turn))
	mustFile(t, s, turn, "n-1")
	mustFile(t, s, turn, "n-2")
	mustServe(t, s, turn, hookServe(launch, "take-1", callNo(1), "n-1"), "n-1")
	mustServe(t, s, turn, hookServe(launch, "take-2", callNo(2), "n-2"), "n-2")
	again := hookServe(launch, "take-3", callNo(3), "n-1", "n-2")
	again.Cancelled = []sandbox.NoteCall{*callNo(1)}
	mustServe(t, s, turn, again, "n-1")
	notes := notesByID(t, s, turn)
	if n := notes["n-1"]; !slices.Equal(servesOf(n), []string{"take-1:cancelled", "take-3"}) || n.Served != launch ||
		n.Place != 1 || !n.Waiting() {
		t.Errorf("the note whose call was cancelled is served %v at place %d (served to %q), want take-1 lost to "+
			"the cancel and take-3 serving it again at its own place", servesOf(n), n.Place, n.Served)
	}
	if n := notes["n-2"]; !slices.Equal(servesOf(n), []string{"take-2"}) {
		t.Errorf("a note whose call nobody cancelled is served %v, want take-2's serve alone", servesOf(n))
	}
	if reads := mustGet(t, s, turn).Steer.Reads; reads != 2 {
		t.Errorf("the record counts %d places, want 2: a note served again keeps its place", reads)
	}
}

// A SERVE REMOVES THE TAKES ITS INBOX SAW UNANSWERED FIRST. A take whose write
// landed and whose hook was answered nothing handed its harness nothing, so the
// job's next serve removes that serve and serves the note again in one write,
// and the bound is not spent: the note can still be served MaxNoteServes times.
//
// Mutation: ignore the serve's unanswered takes, or lose their serves rather
// than remove them, and a check goes red.
func testAServeRemovesTheTakesItsInboxSawUnansweredFirst(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-serve-unanswered"
	launch := mustLaunched(t, s, run(turn))
	mustFile(t, s, turn, "n-1")
	mustServe(t, s, turn, hookServe(launch, "take-1", callNo(1), "n-1"), "n-1")
	again := hookServe(launch, "take-2", callNo(2), "n-1")
	again.Unanswered = []string{"take-1"}
	mustServe(t, s, turn, again, "n-1")
	if n := notesByID(t, s, turn)["n-1"]; !slices.Equal(servesOf(n), []string{"take-2"}) || n.Place != 1 {
		t.Fatalf("the note whose take was answered nothing is served %v at place %d, want take-2 alone at place 1",
			servesOf(n), n.Place)
	}
	// THE BOUND IS NOT SPENT: two more serves after cancels.
	for k := 3; k <= 2+sandbox.MaxNoteServes-1; k++ {
		next := hookServe(launch, fmt.Sprintf("take-%d", k), callNo(k), "n-1")
		next.Cancelled = []sandbox.NoteCall{*callNo(k - 1)}
		mustServe(t, s, turn, next, "n-1")
	}
	if got := servesOf(notesByID(t, s, turn)["n-1"]); len(got) != sandbox.MaxNoteServes || got[len(got)-1] !=
		fmt.Sprintf("take-%d", 1+sandbox.MaxNoteServes) {
		t.Errorf("the note is served %v, want %d serves, the take answered nothing spending none", got,
			sandbox.MaxNoteServes)
	}
}

// A CANCEL LOSES ONLY A LIVE SERVE OF ITS JOB, ON ITS SESSION. A harness
// numbers its calls per session, and a cancel names the session and the call:
// so a settle loses a waiting note's live serve to the job it names that
// answered the very call, and nothing else. Another job's serve, a serve lost
// already, a read note's serve and a call of the same id on another session are
// each left as they are, and a settle that matched nothing writes nothing.
//
// Mutation: match a call by its id alone, lose a serve of another job, lose a
// read note's serve, or write where nothing matched, and a check goes red.
func testACancelLosesOnlyALiveServeOfItsJobOnItsSession(t *testing.T, s sandbox.PendingStore,
	runs coord.SandboxRuns,
) {
	ctx := t.Context()
	const turn = "t-cancel-live"
	launch := mustLaunched(t, s, run(turn))
	for _, id := range []string{"n-1", "n-2", "n-3", "n-4"} {
		mustFile(t, s, turn, id)
	}
	for k, id := range []string{"n-1", "n-2", "n-3", "n-4"} {
		mustServe(t, s, turn, hookServe(launch, "take-"+id, callNo(k+1), id), id)
	}
	mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Cancelled: []sandbox.NoteCall{*callNo(2)}})
	if _, err := s.TakeNotes(ctx, turn, bridgeTake(launch, "n-3")); err != nil {
		t.Fatal(err)
	}
	before := rowVersion(t, runs, turn)
	otherSession := *callNo(1)
	otherSession.Session = "mcp-session-2"
	for name, settle := range map[string]sandbox.HookSettle{
		"the same id on another session, a lost serve and a read note's": {Launch: launch,
			Cancelled: []sandbox.NoteCall{otherSession, *callNo(2), *callNo(3)}},
		"another job's": {Launch: "another-job", Cancelled: []sandbox.NoteCall{*callNo(4)}},
	} {
		if settled := mustSettle(t, s, turn, settle); len(settled) != 0 {
			t.Errorf("a cancel of %s changed %v", name, idsOf(settled))
		}
	}
	if after := rowVersion(t, runs, turn); after != before {
		t.Errorf("cancels that matched nothing wrote the record (version %d to %d)", before, after)
	}
	notes := notesByID(t, s, turn)
	for id, want := range map[string][]string{"n-1": {"take-n-1"}, "n-2": {"take-n-2:cancelled"},
		"n-3": {"take-n-3"}, "n-4": {"take-n-4"}} {
		if got := servesOf(notes[id]); !slices.Equal(got, want) {
			t.Errorf("note %s is served %v, want %v", id, got, want)
		}
	}
	settled := mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Cancelled: []sandbox.NoteCall{*callNo(1)}})
	if n := notesByID(t, s, turn)["n-1"]; !slices.Equal(idsOf(settled), []string{"n-1"}) ||
		!slices.Equal(servesOf(n), []string{"take-n-1:cancelled"}) || n.Served != "" {
		t.Errorf("the cancel of its own call answered %v and left the note served %v (to %q), want it lost and "+
			"served to no job", idsOf(settled), servesOf(n), n.Served)
	}
}

// AN UNANSWERED SERVE IS REMOVED, AND FENCES ITS WRITE IN FLIGHT. A take whose
// hook was answered nothing can still have its serve's write in flight: sent
// before its caller gave up, and taken by the store only after the settle read
// the record. So a settle that finds no serve of the take writes the record all
// the same, and the serve, conditioned on the version it read, cannot land
// after it. And once the serve has landed, the settle removes it, the note
// waiting again and served to no job: its hook handed the harness nothing.
//
// Mutation: write nothing where no serve matched, and the serve lands after
// the settle; or lose the serve rather than remove it, and the note keeps it.
func testAnUnansweredServeIsRemovedAndFencesItsWriteInFlight(t *testing.T, s sandbox.PendingStore,
	runs coord.SandboxRuns,
) {
	ctx := t.Context()
	const turn = "t-unanswered-fence"
	launch := mustLaunched(t, s, run(turn))
	mustFile(t, s, turn, "n-1")
	attempts := 0
	_, wrote, raceErr := runs.MutateSandboxRun(ctx, turn, func(read coord.Record) (coord.SandboxRunChange, error) {
		attempts++
		if attempts > 1 {
			// DECIDED AGAIN on the record the settle wrote: the take's caller
			// is gone, and its write is no more.
			return coord.SandboxRunChange{}, nil
		}
		settled, settleErr := s.SettleHook(ctx, turn, sandbox.HookSettle{Launch: launch,
			Unanswered: []string{"in-flight"}})
		if settleErr != nil || len(settled) != 0 {
			return coord.SandboxRunChange{}, fmt.Errorf("the settle beside the serve's write = %v, %v, want nothing",
				idsOf(settled), settleErr)
		}
		var row sandbox.PendingRun
		if decodeErr := json.Unmarshal(read.Value, &row); decodeErr != nil {
			return coord.SandboxRunChange{}, decodeErr
		}
		row.Steer.Reads++
		row.Steer.Notes[0].Place, row.Steer.Notes[0].Served = row.Steer.Reads, launch
		row.Steer.Notes[0].Serves = []sandbox.HookServed{{Take: "in-flight", Launch: launch,
			Boundary: "tool_batch", At: base}}
		raw, encodeErr := json.Marshal(row)
		return coord.SandboxRunChange{Put: raw}, encodeErr
	})
	if raceErr != nil {
		t.Fatal(raceErr)
	}
	if n := notesByID(t, s, turn)["n-1"]; wrote || attempts != 2 || len(n.Serves) != 0 || n.Served != "" {
		t.Errorf("the serve's write landed (%v) in %d attempt(s), leaving the note served %v, want it lost to the "+
			"settle's write", wrote, attempts, servesOf(n))
	}
	// THE SERVE LANDED, ITS HOOK ANSWERED NOTHING: removed.
	mustServe(t, s, turn, hookServe(launch, "landed", callNo(1), "n-1"), "n-1")
	settled := mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Unanswered: []string{"landed"}})
	n := notesByID(t, s, turn)["n-1"]
	if !slices.Equal(idsOf(settled), []string{"n-1"}) || len(n.Serves) != 0 || n.Served != "" || !n.Waiting() {
		t.Errorf("the settle of a landed serve answered %v and left the note %+v, want its serve removed and the "+
			"note waiting, served to no job", idsOf(settled), n)
	}
}

// A VERDICT READS A HELD SERVE AT ITS NOTE'S PLACE. Once the job's session shows
// a hook took a note, the note is read: off the EARLIEST of its serves the
// session shows taken, with that serve's take, step and instant, at the note's
// own place among the reads, which its first serve took, never a new one, and
// whatever serve the session shows. The record's count of places stays, the
// note stays served to the job, and its serves stay as the read's history.
//
// Mutation: read the note at a new place, off the latest held serve, at the
// settle's instant, or drop its serves, and a check goes red.
func testAVerdictReadsAHeldServeAtItsNotesPlace(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-verdict-held"
	launch := mustLaunched(t, s, run(turn))
	for _, id := range []string{"n-1", "n-2", "n-3"} {
		mustFile(t, s, turn, id)
	}
	// n-1 AND n-2 EACH SERVED, LOST TO A CANCEL AND SERVED AGAIN AT A LATER
	// STEP; n-3 served once, after both.
	mustServe(t, s, turn, hookServe(launch, "take-1", callNo(1), "n-1"), "n-1")
	mustServe(t, s, turn, hookServe(launch, "take-2", callNo(2), "n-2"), "n-2")
	for k, id := range map[int]string{3: "n-1", 4: "n-2"} {
		later := hookServe(launch, fmt.Sprintf("take-%d", k), callNo(k), id)
		later.Serve.Boundary, later.Serve.Iteration = "finish", 2
		later.Cancelled = []sandbox.NoteCall{*callNo(k - 2)}
		mustServe(t, s, turn, later, id)
	}
	mustServe(t, s, turn, hookServe(launch, "take-5", callNo(5), "n-3"), "n-3")
	first := notesByID(t, s, turn)["n-1"].Serves[0]
	reads := mustGet(t, s, turn).Steer.Reads

	// HELD THROUGH BOTH ITS SERVES: off the earliest.
	settled := mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Held: []string{"take-3", "take-1"}})
	if !slices.Equal(idsOf(settled), []string{"n-1"}) {
		t.Errorf("the verdict answered %v, want the note it read", idsOf(settled))
	}
	notes := notesByID(t, s, turn)
	if read := notes["n-1"].Read; read == nil || read.Take != "take-1" || read.Order != 1 ||
		read.Boundary != "tool_batch" || read.Iteration != 1 || !read.At.Equal(first.At) || read.Launch != launch ||
		read.Phase != "execute" || read.Round != 1 || read.Restarted {
		t.Errorf("the note held through both serves is read %+v, want take-1's read at place 1, at its step and "+
			"its instant %v", read, first.At)
	}
	if n := notes["n-1"]; n.Served != launch || !slices.Equal(servesOf(n), []string{"take-1:cancelled", "take-3"}) {
		t.Errorf("the read note is served %v to %q, want its serves kept and it served to job %s", servesOf(n),
			n.Served, launch)
	}
	// HELD THROUGH ITS LATER SERVE ALONE: off that serve, at the note's first
	// place all the same.
	mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Held: []string{"take-4"}})
	notes = notesByID(t, s, turn)
	if read := notes["n-2"].Read; read == nil || read.Take != "take-4" || read.Order != 2 ||
		read.Boundary != "finish" || read.Iteration != 2 || !read.At.Equal(notes["n-2"].Serves[1].At) {
		t.Errorf("the note held through its later serve is read %+v, want take-4's read at its first place 2", read)
	}
	if !notes["n-3"].Waiting() {
		t.Errorf("a note no held take served is read %+v", notes["n-3"].Read)
	}
	if got := mustGet(t, s, turn).Steer.Reads; got != reads {
		t.Errorf("the verdicts moved the record's count of places from %d to %d", reads, got)
	}
}

// A RESTARTED TAKE IS READ MARKED SO. The job's session can show a hook took a
// note only before the session's last restart: the harness took it, and a
// resume rebuilds the conversation from the restart on, which is not known to
// hold it. Such a note is read, at its place, marked restarted; but where the
// session shows another serve of the note taken after the restart, it is read
// off that serve, unmarked.
//
// Mutation: read a restarted take unmarked, or mark a note one of whose serves
// is held, and a check goes red.
func testARestartedTakeIsReadMarkedSo(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-verdict-restarted"
	launch := mustLaunched(t, s, run(turn))
	mustFile(t, s, turn, "n-1")
	mustFile(t, s, turn, "n-2")
	mustServe(t, s, turn, hookServe(launch, "take-1", callNo(1), "n-1", "n-2"), "n-1", "n-2")
	again := hookServe(launch, "take-2", callNo(2), "n-2")
	again.Cancelled = []sandbox.NoteCall{*callNo(1)}
	mustServe(t, s, turn, again, "n-2")
	mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Restarted: []string{"take-1"}, Held: []string{"take-2"}})
	notes := notesByID(t, s, turn)
	if read := notes["n-1"].Read; read == nil || !read.Restarted || read.Take != "take-1" || read.Order != 1 {
		t.Errorf("a note shown taken only before the session's last restart is read %+v, want it read at place 1, "+
			"marked restarted", read)
	}
	if read := notes["n-2"].Read; read == nil || read.Restarted || read.Take != "take-2" {
		t.Errorf("a note shown taken after the restart too is read %+v, want take-2's read, unmarked", read)
	}
}

// A VERDICT LOSES A SERVE IT IS TOLD IS ABSENT. A job's whole session can prove
// its hook never took a note: that serve is lost absent, whether it was live or
// already lost to a cancel, and the note is served to no job. A serve the
// verdict does not name is left as it is.
//
// Mutation: lose only a live serve, or a serve the verdict did not name, and
// a check goes red.
func testAVerdictLosesAServeItIsToldIsAbsent(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-verdict-absent"
	launch := mustLaunched(t, s, run(turn))
	for k, id := range []string{"n-1", "n-2", "n-3"} {
		mustFile(t, s, turn, id)
		mustServe(t, s, turn, hookServe(launch, "take-"+id, callNo(k+1), id), id)
	}
	mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Cancelled: []sandbox.NoteCall{*callNo(2)}})
	settled := mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Absent: []string{"take-n-1", "take-n-2"}})
	if !slices.Equal(idsOf(settled), []string{"n-1", "n-2"}) {
		t.Errorf("the verdict answered %v, want the two notes it lost", idsOf(settled))
	}
	notes := notesByID(t, s, turn)
	for id, want := range map[string][]string{"n-1": {"take-n-1:absent"}, "n-2": {"take-n-2:absent"},
		"n-3": {"take-n-3"}} {
		if n := notes[id]; !slices.Equal(servesOf(n), want) || !n.Waiting() {
			t.Errorf("note %s is served %v (waiting %v), want %v", id, servesOf(n), n.Waiting(), want)
		}
	}
	if n := notes["n-1"]; n.Served != "" {
		t.Errorf("a note whose one serve is absent is still served to %q", n.Served)
	}
}

// A HELD TAKE OUTRANKS A CANCEL. A harness's cancel says it gave a hook's call
// up, and its session can show the answer reached it all the same: the
// session outranks the cancel, in a later settle and within one settle, and an
// absence the same settle names.
//
// Mutation: let a cancel, or an absence, outrank the session, and the note
// waits.
func testAHeldTakeOutranksACancel(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-verdict-outranks"
	launch := mustLaunched(t, s, run(turn))
	mustFile(t, s, turn, "n-1")
	mustFile(t, s, turn, "n-2")
	mustServe(t, s, turn, hookServe(launch, "take-1", callNo(1), "n-1"), "n-1")
	mustServe(t, s, turn, hookServe(launch, "take-2", callNo(2), "n-2"), "n-2")
	mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Cancelled: []sandbox.NoteCall{*callNo(1)}})
	mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Held: []string{"take-1"}})
	mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Cancelled: []sandbox.NoteCall{*callNo(2)},
		Held: []string{"take-2"}, Absent: []string{"take-2"}})
	for id, n := range notesByID(t, s, turn) {
		if n.Read == nil || n.Read.Take != "take-"+strings.TrimPrefix(id, "n-") || n.Served != launch {
			t.Errorf("note %s is read %+v (served to %q), want it read off its held serve", id, n.Read, n.Served)
		}
	}
}

// A VERDICT IS IDEMPOTENT AND ANSWERS EVERY HOOK READ OF ITS JOB. A collection
// that settles its job's serves may be made again, and find its verdict landed
// with its answer lost: so a settle naming a take held finds the note read and
// writes nothing, and answers every note on the record read off a serve to the
// job, the earlier settle's included, which is what its caller publishes. A
// note a take read, which no serve handed over, is never among them.
//
// Mutation: answer only what the settle changed, or write a verdict that found
// its notes read, and a check goes red.
func testAVerdictIsIdempotentAndAnswersEveryHookReadOfItsJob(t *testing.T, s sandbox.PendingStore,
	runs coord.SandboxRuns,
) {
	ctx := t.Context()
	const turn = "t-verdict-again"
	launch := mustLaunched(t, s, run(turn))
	for _, id := range []string{"n-1", "n-2", "n-3"} {
		mustFile(t, s, turn, id)
	}
	mustServe(t, s, turn, hookServe(launch, "take-1", callNo(1), "n-1"), "n-1")
	mustServe(t, s, turn, hookServe(launch, "take-2", callNo(2), "n-2"), "n-2")
	if _, err := s.TakeNotes(ctx, turn, namedTake(launch, "read", "n-3")); err != nil {
		t.Fatal(err)
	}
	if got := idsOf(mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Held: []string{"take-1"}})); !slices.Equal(
		got, []string{"n-1"}) {
		t.Fatalf("the verdict answered %v, want n-1", got)
	}
	before := rowVersion(t, runs, turn)
	if got := idsOf(mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Held: []string{"take-1"}})); !slices.Equal(
		got, []string{"n-1"}) {
		t.Errorf("the verdict made again answered %v, want the note it read", got)
	}
	if after := rowVersion(t, runs, turn); after != before {
		t.Errorf("a verdict that found its note read wrote the record (version %d to %d)", before, after)
	}
	if got := idsOf(mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Held: []string{"take-2"}})); !slices.Equal(
		got, []string{"n-1", "n-2"}) {
		t.Errorf("a later verdict answered %v, want every note read off a serve to the job", got)
	}
}

// A CANCEL AND A VERDICT COMMUTE. The harness's cancel is heard by the inbox
// and settled off the hook's path, and the session's verdict is written at the
// job's collection, by two parties in either order: each decides on the record
// as it stands, so either order ends in one record as every reader reads it:
// the note's read, the job it is served to, and, while it waits, its serves and
// what lost them. A cancel after a verdict acts on no read note and on no serve
// a verdict lost, and a verdict after a cancel reads what the session shows and
// loses what it proves absent. (A read note keeps its serves as the history its
// read was confirmed from, a cancel heard before the verdict among them, which
// no reader of a read note reads.)
//
// Mutation: let a cancel act on a serve a verdict settled, or a verdict skip a
// serve a cancel lost, and the two orders end apart.
func testACancelAndAVerdictCommute(t *testing.T, s sandbox.PendingStore) {
	project := func(turnID, launch string) string {
		n := notesByID(t, s, turnID)["n-1"]
		if n.Read != nil {
			return fmt.Sprintf("read %s@%d (restarted %v), served to the job %v", n.Read.Take, n.Read.Order,
				n.Read.Restarted, n.Served == launch)
		}
		return fmt.Sprintf("waiting, served to the job %v, serves %v", n.Served == launch, servesOf(n))
	}
	cancel := func(launch string) sandbox.HookSettle {
		return sandbox.HookSettle{Launch: launch, Cancelled: []sandbox.NoteCall{*callNo(1)}}
	}
	for name, verdict := range map[string]func(string) sandbox.HookSettle{
		"held": func(launch string) sandbox.HookSettle {
			return sandbox.HookSettle{Launch: launch, Held: []string{"take-1"}}
		},
		"absent": func(launch string) sandbox.HookSettle {
			return sandbox.HookSettle{Launch: launch, Absent: []string{"take-1"}}
		},
		"undecided": func(launch string) sandbox.HookSettle { return sandbox.HookSettle{Launch: launch} },
	} {
		t.Run(name, func(t *testing.T) {
			ends := map[string]string{}
			for order, settles := range map[string]func(string) []sandbox.HookSettle{
				"cancel first":  func(l string) []sandbox.HookSettle { return []sandbox.HookSettle{cancel(l), verdict(l)} },
				"verdict first": func(l string) []sandbox.HookSettle { return []sandbox.HookSettle{verdict(l), cancel(l)} },
			} {
				turn := "t-commute-" + name + "-" + strings.ReplaceAll(order, " ", "-")
				launch := mustLaunched(t, s, run(turn))
				mustFile(t, s, turn, "n-1")
				mustServe(t, s, turn, hookServe(launch, "take-1", callNo(1), "n-1"), "n-1")
				for _, settle := range settles(launch) {
					mustSettle(t, s, turn, settle)
				}
				ends[order] = project(turn, launch)
			}
			if ends["cancel first"] != ends["verdict first"] {
				t.Errorf("the cancel first ends %s, the verdict first %s, want one record", ends["cancel first"],
					ends["verdict first"])
			}
		})
	}
}

// EVERY READ OF A PLACED NOTE TAKES ITS PLACE. A note's first serve fixed its
// place among the turn's reads, so whichever step reads it later reads it
// there: the review's take and a relaunch's brief alike. So a note a person
// filed after it, correcting it, stays after it in every later telling,
// whatever its harness saw first. A write moves the record's count of places
// only where one of its reads took the write's own new place.
//
// Mutation: read a placed note at the take's or the brief's own place, or move
// the count for a write whose reads are all placed, and a check goes red.
func testEveryReadOfAPlacedNoteTakesItsPlace(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-placed-reads"
	launch := mustLaunched(t, s, run(turn))
	for _, id := range []string{"n-1", "n-2", "n-3"} {
		mustFile(t, s, turn, id)
	}
	mustServe(t, s, turn, hookServe(launch, "take-1", callNo(1), "n-1"), "n-1")
	mustServe(t, s, turn, hookServe(launch, "take-2", callNo(2), "n-2"), "n-2")
	claimed := mustClaim(t, s, turn)
	review := func(ids ...string) sandbox.NoteTake {
		return sandbox.NoteTake{IDs: ids, Launch: launch, Statuses: []string{sandbox.StatusResumed},
			Fence: sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch},
			Read:  sandbox.NoteRead{Phase: "review", Iteration: 1, Round: 1, Boundary: "round"}}
	}
	taken, err := s.TakeNotes(ctx, turn, review("n-1"))
	if err != nil || len(taken) != 1 || taken[0].Read.Order != 1 {
		t.Fatalf("the review's take of a placed note = %+v, %v, want it read at its place 1", taken, err)
	}
	if reads := mustGet(t, s, turn).Steer.Reads; reads != 2 {
		t.Errorf("a take whose one read is placed moved the count of places to %d, want 2", reads)
	}
	if taken, err = s.TakeNotes(ctx, turn, review("n-3")); err != nil || len(taken) != 1 || taken[0].Read.Order != 3 {
		t.Fatalf("the review's take of an unplaced note = %+v, %v, want it read at the take's place 3", taken, err)
	}
	again := run(turn)
	brief := &sandbox.NoteRead{Phase: "execute", Iteration: 2, Round: 1, Boundary: "brief"}
	again.BriefNotes = []sandbox.RunNote{{ID: "n-2", Read: brief},
		{ID: "b-1", Sealed: []byte("sealed:b-1"), At: base, Read: brief}}
	if _, err := s.BeginLaunch(ctx, again, sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	row := mustGet(t, s, turn)
	notes := notesByID(t, s, turn)
	for id, place := range map[string]int{"n-1": 1, "n-2": 2, "n-3": 3, "b-1": 4} {
		if read := notes[id].Read; read == nil || read.Order != place {
			t.Errorf("note %s is read %+v, want it at place %d", id, read, place)
		}
	}
	if row.Steer.Reads != 4 {
		t.Errorf("the record counts %d places, want 4", row.Steer.Reads)
	}
}

// A FILING COUNTS NO HOOK-SERVED NOTE AS WAITING. The five that may wait are
// five a step has still to read, and a hook's answer handed its notes to the
// model at the step it answered: so a note a hook's live serve handed to the
// record's job leaves room, a note whose serve was lost counts again, and a
// note a poll served counts until its harness confirms it at its next step.
//
// Mutation: count a note live-served to the job, or stop counting one whose
// serve was lost, and a filing goes wrong.
func testAFilingCountsNoHookServedNoteAsWaiting(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-full-served"
	launch := mustLaunched(t, s, run(turn))
	var ids []string
	for i := range maxWaiting {
		ids = append(ids, fmt.Sprintf("n-%d", i+1))
		mustFile(t, s, turn, ids[i])
		mustServe(t, s, turn, hookServe(launch, "take-"+ids[i], callNo(i+1), ids[i]), ids[i])
	}
	for _, id := range []string{"n-6", "n-7", "n-8", "n-9"} {
		mustFile(t, s, turn, id)
	}
	mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Cancelled: []sandbox.NoteCall{*callNo(1)}})
	if filed, _, err := file(ctx, s, turn, "n-10"); err != nil || filed != sandbox.NoteFull {
		t.Errorf("a note filed behind four waiting and one whose serve was lost = %s, %v, want full", filed, err)
	}

	const polled = "t-full-polled"
	job := mustLaunched(t, s, run(polled))
	ids = nil
	for i := range maxWaiting {
		ids = append(ids, fmt.Sprintf("n-%d", i+1))
		mustFile(t, s, polled, ids[i])
	}
	if _, err := s.ServeNotes(ctx, polled, sandbox.NoteServe{IDs: ids, Launch: job,
		Statuses: []string{sandbox.StatusLaunching, sandbox.StatusRunning}}); err != nil {
		t.Fatal(err)
	}
	if filed, _, err := file(ctx, s, polled, "n-6"); err != nil || filed != sandbox.NoteFull {
		t.Errorf("a note filed behind five a poll served = %s, %v, want full", filed, err)
	}
}

// AN ITEM IS FILED ONLY WHILE ITS JOB IS THE RECORD'S. An item for one job is
// read only by a reader inside that job, and the launch that opens the next
// removes it: so a filing for a job the record no longer names, one whose filer
// lost its race to that launch, answers closed and writes nothing, since
// nothing in the job it names will read it. The current job's item is filed.
//
// Mutation: file an item for a job the record no longer names, and it lands
// behind the next job's launch.
func testAnItemIsFiledOnlyWhileItsJobIsTheRecords(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-item-job"
	previous := mustLaunched(t, s, run(turn))
	mustClaim(t, s, turn)
	current := mustBeginLaunch(t, s, run(turn))
	if filed, err := fileItem(ctx, s, turn, "i-1", previous); err != nil || filed != sandbox.NoteClosed {
		t.Errorf("an item for the job the record no longer names = %s, %v, want closed", filed, err)
	}
	if notes := mustGet(t, s, turn).Steer.Notes; len(notes) != 0 {
		t.Errorf("a refused item landed on the record: %+v", notes)
	}
	if filed, err := fileItem(ctx, s, turn, "i-2", current); err != nil || filed != sandbox.NoteAccepted {
		t.Fatalf("an item for the record's own job = %s, %v, want accepted", filed, err)
	}
	if n := notesByID(t, s, turn)["i-2"]; n.For != current || !n.Waiting() || n.Seq != 1 {
		t.Errorf("the item is %+v, want it waiting, for job %s, numbered 1", n, current)
	}
}

// A JOB'S ITEMS ARE BOUNDED APART FROM A PERSON'S NOTES. A job's items are
// bounded by [sandbox.MaxJobItems], which holds the row's share of them, and
// count toward neither bound of a person's: a turn that holds its job's ten
// still takes a person's note, five waiting, and an eleventh item is spent.
//
// Mutation: ignore the items' bound, or count an item toward a person's, and a
// filing goes wrong.
func testAJobsItemsAreBoundedApartFromAPersonsNotes(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-item-bound"
	launch := mustLaunched(t, s, run(turn))
	for i := range sandbox.MaxJobItems {
		if filed, err := fileItem(ctx, s, turn, fmt.Sprintf("i-%02d", i+1), launch); err != nil ||
			filed != sandbox.NoteAccepted {
			t.Fatalf("item %d = %s, %v, want accepted", i+1, filed, err)
		}
	}
	if filed, err := fileItem(ctx, s, turn, "i-11", launch); err != nil || filed != sandbox.NoteSpent {
		t.Errorf("the eleventh item = %s, %v, want spent", filed, err)
	}
	for i := range maxWaiting {
		mustFile(t, s, turn, fmt.Sprintf("n-%d", i+1))
	}
	if filed, _, err := file(ctx, s, turn, "n-6"); err != nil || filed != sandbox.NoteFull {
		t.Errorf("a person's sixth waiting note beside the job's items = %s, %v, want full", filed, err)
	}
}

// A LAUNCH REMOVES EVERY ITEM OF THE JOB BEFORE IT. The job a launch opens is
// new, so every item on the record was the job before it's alone, waiting,
// served or read, and nothing in the new job will read one: the launch's own
// write removes each, before anything it files, and leaves a person's notes as
// they are. The new job's items are then bounded from none.
//
// Mutation: keep an item of the job before, or count it against the new job's
// bound, and a check goes red.
func testALaunchRemovesEveryItemOfTheJobBeforeIt(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-item-removed"
	first := mustLaunched(t, s, run(turn))
	mustFile(t, s, turn, "n-1")
	for _, id := range []string{"i-waiting", "i-served", "i-read"} {
		if filed, err := fileItem(ctx, s, turn, id, first); err != nil || filed != sandbox.NoteAccepted {
			t.Fatalf("item %s = %s, %v", id, filed, err)
		}
	}
	mustServe(t, s, turn, hookServe(first, "take-1", callNo(1), "i-served"), "i-served")
	if taken, err := s.TakeNotes(ctx, turn, bridgeTake(first, "i-read")); err != nil || len(taken) != 1 {
		t.Fatalf("the job's own take of its item = %v, %v", idsOf(taken), err)
	}
	mustClaim(t, s, turn)
	again := run(turn)
	again.BriefNotes = []sandbox.RunNote{{ID: "n-1",
		Read: &sandbox.NoteRead{Phase: "execute", Iteration: 2, Round: 1, Boundary: "brief"}}}
	relaunched, err := s.BeginLaunch(ctx, again, sandbox.Fence{})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	notes := mustGet(t, s, turn).Steer.Notes
	if !slices.Equal(idsOf(notes), []string{"n-1"}) || notes[0].Read == nil ||
		notes[0].Read.Launch != relaunched.LaunchID {
		t.Fatalf("the relaunch left notes %v, want the person's note alone, read by its brief", idsOf(notes))
	}
	for i := range sandbox.MaxJobItems {
		if filed, err := fileItem(ctx, s, turn, fmt.Sprintf("j-%02d", i+1), relaunched.LaunchID); err != nil ||
			filed != sandbox.NoteAccepted {
			t.Fatalf("the new job's item %d = %s, %v, want accepted", i+1, filed, err)
		}
	}
}

// A CALL PAST ITS BOUND IS RECORDED AS NO CALL. A serve keeps the call it
// answered whole within [sandbox.MaxNoteCallBytes], and as none past it or
// where it names no session or no id: never cut, since two calls cut alike
// would be one, and a cancel of either would lose the other's serve. A serve
// kept with no call is settled by its job's session alone.
//
// Mutation: cut a call past its bound rather than drop it, and a cancel of
// the call it was cut to loses its serve too.
func testACallPastItsBoundIsRecordedAsNoCall(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-call-bound"
	launch := mustLaunched(t, s, run(turn))
	atBound := sandbox.NoteCall{Session: strings.Repeat("s", sandbox.MaxNoteCallBytes), ID: "7"}
	for id, call := range map[string]sandbox.NoteCall{
		"at-bound":       atBound,
		"session-past":   {Session: atBound.Session + "s", ID: "7"},
		"id-past":        {Session: "mcp-session-1", ID: strings.Repeat("9", sandbox.MaxNoteCallBytes+1)},
		"session-absent": {ID: "7"},
	} {
		mustFile(t, s, turn, id)
		mustServe(t, s, turn, hookServe(launch, "take-"+id, &call, id), id)
	}
	notes := notesByID(t, s, turn)
	if call := notes["at-bound"].Serves[0].Call; call == nil || call.Session != atBound.Session || call.ID != atBound.ID {
		t.Errorf("a call at its bound is kept as %+v, want it whole", call)
	}
	for _, id := range []string{"session-past", "id-past", "session-absent"} {
		if call := notes[id].Serves[0].Call; call != nil {
			t.Errorf("serve %s keeps the call %+v, want none", id, call)
		}
	}
	mustSettle(t, s, turn, sandbox.HookSettle{Launch: launch, Cancelled: []sandbox.NoteCall{atBound}})
	notes = notesByID(t, s, turn)
	if got := servesOf(notes["session-past"]); !slices.Equal(got, []string{"take-session-past"}) {
		t.Errorf("a cancel of the call at the bound left the serve past it served %v, want it live", got)
	}
	if got := servesOf(notes["at-bound"]); !slices.Equal(got, []string{"take-at-bound:cancelled"}) {
		t.Errorf("a cancel of the call at the bound left its own serve %v, want it lost", got)
	}
}

// EVERY WRITE HANDS AN ITEM ONLY TO ITS JOB. An item for one job is handed over
// only to a reader inside that job: a hook's serve and a poll's serve hand it
// only to that job, and a take only where its read names that job, so the
// review's take, whose read names none, takes no item at all. One written onto
// the record for a job the record no longer names, as a party that reaches the
// store could, is handed to nobody. And the launch that opens the next job
// removes every item, read or not.
//
// Mutation: let any of the three writes ignore an item's job, or keep an item
// past the next launch, and a check goes red.
func testEveryWriteHandsAnItemOnlyToItsJob(t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns) {
	ctx := t.Context()
	const turn = "t-item-writes"
	launch := mustLaunched(t, s, run(turn))
	mustFile(t, s, turn, "n-1")
	if filed, err := fileItem(ctx, s, turn, "i-mine", launch); err != nil || filed != sandbox.NoteAccepted {
		t.Fatalf("the job's item = %s, %v", filed, err)
	}
	// AN ITEM FOR ANOTHER JOB, written onto the record whole.
	if _, ok, err := runs.MutateSandboxRun(ctx, turn, func(current coord.Record) (coord.SandboxRunChange, error) {
		var row sandbox.PendingRun
		if err := json.Unmarshal(current.Value, &row); err != nil {
			return coord.SandboxRunChange{}, err
		}
		row.Steer.Seq++
		foreign := jobItem("i-other", "job-before")
		foreign.Seq = row.Steer.Seq
		row.Steer.Notes = append(row.Steer.Notes, foreign)
		raw, err := json.Marshal(row)
		return coord.SandboxRunChange{Put: raw}, err
	}); err != nil || !ok {
		t.Fatalf("write the other job's item: %v %v", ok, err)
	}
	mustServe(t, s, turn, hookServe(launch, "take-1", callNo(1), "i-other", "n-1", "i-mine"), "n-1", "i-mine")
	polled, err := s.ServeNotes(ctx, turn, sandbox.NoteServe{IDs: []string{"i-other", "i-mine"}, Launch: launch,
		Statuses: []string{sandbox.StatusLaunching, sandbox.StatusRunning}})
	if err != nil || !slices.Equal(idsOf(polled), []string{"i-mine"}) {
		t.Errorf("the job's poll served %v, %v, want its own item alone", idsOf(polled), err)
	}
	noJob := bridgeTake(launch, "n-1", "i-mine", "i-other")
	noJob.Read.Launch = ""
	taken, err := s.TakeNotes(ctx, turn, noJob)
	if err != nil || !slices.Equal(idsOf(taken), []string{"n-1"}) {
		t.Errorf("a take whose read names no job took %v, %v, want the person's note alone", idsOf(taken), err)
	}
	taken, err = s.TakeNotes(ctx, turn, bridgeTake(launch, "i-mine", "i-other"))
	if err != nil || !slices.Equal(idsOf(taken), []string{"i-mine"}) {
		t.Errorf("the job's own take took %v, %v, want its own item alone", idsOf(taken), err)
	}
	if n := notesByID(t, s, turn)["i-other"]; !n.Waiting() || n.Served != "" || len(n.Serves) != 0 {
		t.Errorf("the other job's item was handed over: %+v", n)
	}
	mustClaim(t, s, turn)
	if _, err := s.BeginLaunch(ctx, run(turn), sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if got := idsOf(mustGet(t, s, turn).Steer.Notes); !slices.Equal(got, []string{"n-1"}) {
		t.Errorf("the next launch left notes %v, want the person's note alone", got)
	}
}
