package sandboxtest

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// The cases of a WAKE ITEM ([sandbox.RunNote.Wake]): somebody's message about
// the work a job is doing, filed by the engine alone for that job while it
// works, joined by every wake filed before anything serves it, served to a
// hook once, read only once its session shows it, and the turn's once read.
var wakeItemCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"AWakeIsFiledOnlyForItsJobWhileItWorks", testAWakeIsFiledOnlyForItsJobWhileItWorks},
	{"WakesBetweenTwoStepsJoinOneItem", testWakesBetweenTwoStepsJoinOneItem},
	{"AWakeIsFiledOnceHoweverOftenItIsAsked", testAWakeIsFiledOnceHoweverOftenItIsAsked},
	{"AJobsAndATurnsWakeItemsStopAtTheirBounds", testAJobsAndATurnsWakeItemsStopAtTheirBounds},
	{"AWakeItemIsServedToAHookOnce", testAWakeItemIsServedToAHookOnce},
	{"AWakeItemIsReadOnlyOnceItsSessionShowsIt", testAWakeItemIsReadOnlyOnceItsSessionShowsIt},
	{"ARelaunchKeepsTheWakesItsJobReadAndHandsBackTheRest", testARelaunchKeepsTheWakesItsJobReadAndHandsBackTheRest},
	{"AWakeItemIsServedOnlyWholeAsItsServerReadIt", testAWakeItemIsServedOnlyWholeAsItsServerReadIt},
	{"AJobsWakesCrowdOutNoLateCall", testAJobsWakesCrowdOutNoLateCall},
	{"APersonCannotFileAWake", testAPersonCannotFileAWake},
}

// sealedWake is a wake as the engine hands the store one: its id and size
// bytes standing for its seal.
func sealedWake(id string, size int) sandbox.SealedWake {
	return sandbox.SealedWake{ID: id, Sealed: []byte(strings.Repeat("s", size))}
}

// fileWake files w for job on turnID's record, failing the case where the store
// refuses it, and answers the item that holds it.
func fileWake(t *testing.T, s sandbox.PendingStore, turnID, job string, w sandbox.SealedWake) sandbox.RunNote {
	t.Helper()
	item, err := s.FileWake(t.Context(), turnID, sandbox.WakeFiling{Launch: job, Wake: w})
	if err != nil {
		t.Fatalf("FileWake %s for job %s: %v", w.ID, job, err)
	}
	return item
}

// wakesIn is each wake item on turnID's record as the ids of its wakes, in the
// record's order: "w-1,w-2".
func wakesIn(t *testing.T, s sandbox.PendingStore, turnID string) []string {
	t.Helper()
	out := []string{}
	for _, n := range mustGet(t, s, turnID).Steer.Notes {
		if !n.IsWake() {
			continue
		}
		var ids []string
		for _, w := range n.Wake.Wakes {
			ids = append(ids, w.ID)
		}
		out = append(out, strings.Join(ids, ","))
	}
	return out
}

// hookServeOf is a hook's serve of ids to job under take, handing over every
// wake each wake item among them holds on turnID's record now.
func hookServeOf(t *testing.T, s sandbox.PendingStore, turnID, job, take string, ids ...string) sandbox.HookServe {
	t.Helper()
	return sandbox.HookServe{IDs: ids, Launch: job, Statuses: sandbox.ItemStatuses,
		Serve: sandbox.HookServed{Take: take, Boundary: "tool_batch", Iteration: 1},
		Wakes: wakesNow(t, s, turnID, ids)}
}

// wakesNow is how many wakes each wake item of ids holds on turnID's record
// now, by id: what a serve that read the record now hands over.
func wakesNow(t *testing.T, s sandbox.PendingStore, turnID string, ids []string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, n := range mustGet(t, s, turnID).Steer.Notes {
		if n.IsWake() && slices.Contains(ids, n.ID) {
			out[n.ID] = len(n.Wake.Wakes)
		}
	}
	return out
}

// A WAKE IS FILED ONLY FOR ITS JOB WHILE IT WORKS. The engine files one for the
// job the record names launching or running, its ending undecided: never for
// a run with no record, for another job, or for a run whose completion was
// claimed; and one sealed past what an item holds, or naming no valid id, is
// refused before anything is written. The item filed is the job's alone,
// waiting, served to nobody, and carries no seal of its own.
//
// Mutation: file a wake whatever job the record names (wake-for-any-job), and
// this goes red.
func testAWakeIsFiledOnlyForItsJobWhileItWorks(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wake-window"
	ctx := t.Context()
	if _, err := s.FileWake(ctx, turn, sandbox.WakeFiling{Launch: "job-0", Wake: sealedWake("w-0", 64)}); !errors.Is(err,
		sandbox.ErrWakeClosed) {
		t.Errorf("a wake for a run with no record answered %v, want ErrWakeClosed", err)
	}
	job := mustLaunched(t, s, run(turn))
	for name, filing := range map[string]sandbox.WakeFiling{
		"another job's":           {Launch: "job-9", Wake: sealedWake("w-1", 64)},
		"one sealed past an item": {Launch: job, Wake: sealedWake("w-2", sandbox.MaxWakeItemBytes+1)},
		"one with no id":          {Launch: job, Wake: sealedWake("", 64)},
		"one with a spaced id":    {Launch: job, Wake: sealedWake("w 3", 64)},
	} {
		if _, err := s.FileWake(ctx, turn, filing); err == nil {
			t.Errorf("%s wake was filed", name)
		}
	}
	if _, err := s.FileWake(ctx, turn, sandbox.WakeFiling{Launch: "job-9",
		Wake: sealedWake("w-4", 64)}); !errors.Is(err, sandbox.ErrWakeClosed) {
		t.Errorf("a wake for a job the record does not name answered %v, want ErrWakeClosed", err)
	}
	if _, err := s.FileWake(ctx, turn, sandbox.WakeFiling{Launch: job,
		Wake: sealedWake("w-5", sandbox.MaxWakeItemBytes+1)}); !errors.Is(err, sandbox.ErrWakeTooLarge) {
		t.Errorf("a wake sealed past an item answered %v, want ErrWakeTooLarge", err)
	}
	item := fileWake(t, s, turn, job, sealedWake("w-6", 64))
	if item.For != job || !item.IsWake() || !item.Waiting() || item.Served != "" || len(item.Sealed) != 0 ||
		item.IsFor("") || !item.IsFor(job) || item.LateCall != nil {
		t.Errorf("the item is %+v, want a waiting wake item for the job alone, with no seal of its own", item)
	}
	if got := wakesIn(t, s, turn); !slices.Equal(got, []string{"w-6"}) {
		t.Errorf("the record holds wakes %v, want the one filed", got)
	}
	if _, won, err := s.ClaimForResume(ctx, turn, sandbox.CompletionTail(job), sandbox.Fence{}); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	if _, err := s.FileWake(ctx, turn, sandbox.WakeFiling{Launch: job,
		Wake: sealedWake("w-7", 64)}); !errors.Is(err, sandbox.ErrWakeClosed) {
		t.Errorf("a wake for a job whose completion was claimed answered %v, want ErrWakeClosed", err)
	}
}

// WAKES BETWEEN TWO STEPS JOIN ONE ITEM, oldest first, so the bound counts
// items, not wakes: a wake joins the job's newest item while nothing has served
// it and it has room, and opens one once a hook served it, as a wake joining a
// served item would reach no harness, or once it is full.
//
// Mutation: open an item for every wake (no-join), or join a served item, and
// this goes red.
func testWakesBetweenTwoStepsJoinOneItem(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wake-join"
	ctx := t.Context()
	job := mustLaunched(t, s, run(turn))
	first := fileWake(t, s, turn, job, sealedWake("w-1", 64))
	if joined := fileWake(t, s, turn, job, sealedWake("w-2", 64)); joined.ID != first.ID {
		t.Errorf("the second wake opened item %s, want it to join %s", joined.ID, first.ID)
	}
	if served, err := s.ServeHook(ctx, turn, hookServeOf(t, s, turn, job, "take-1", first.ID)); err != nil || len(served) != 1 {
		t.Fatalf("ServeHook = %d, %v", len(served), err)
	}
	fileWake(t, s, turn, job, sealedWake("w-3", 64))
	for i := 4; i < 4+sandbox.MaxItemWakes; i++ {
		fileWake(t, s, turn, job, sealedWake("w-"+strings.Repeat("x", i), 64))
	}
	got := wakesIn(t, s, turn)
	want := []string{"w-1,w-2", "w-3,w-xxxx,w-xxxxx,w-xxxxxx", "w-xxxxxxx"}
	if !slices.Equal(got, want) {
		t.Errorf("the record holds wake items %v, want %v: a served item joined by nothing, a full one by "+
			"nothing more", got, want)
	}
}

// A WAKE IS FILED ONCE HOWEVER OFTEN IT IS ASKED: its id is its identity, so a
// retry, before its item is served or after, answers the item holding it and
// adds nothing.
//
// Mutation: file a retry as a new wake, and its item carries it twice.
func testAWakeIsFiledOnceHoweverOftenItIsAsked(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wake-once"
	ctx := t.Context()
	job := mustLaunched(t, s, run(turn))
	first := fileWake(t, s, turn, job, sealedWake("w-1", 64))
	if again := fileWake(t, s, turn, job, sealedWake("w-1", 64)); again.ID != first.ID {
		t.Errorf("the retry answered item %s, want %s", again.ID, first.ID)
	}
	if _, err := s.ServeHook(ctx, turn, hookServeOf(t, s, turn, job, "take-1", first.ID)); err != nil {
		t.Fatal(err)
	}
	if again := fileWake(t, s, turn, job, sealedWake("w-1", 64)); again.ID != first.ID {
		t.Errorf("the retry after a serve answered item %s, want %s", again.ID, first.ID)
	}
	if got := wakesIn(t, s, turn); !slices.Equal(got, []string{"w-1"}) {
		t.Errorf("the record holds wakes %v, want the one wake once", got)
	}
}

// A JOB'S AND A TURN'S WAKE ITEMS STOP AT THEIR BOUNDS: a job holds at most
// [sandbox.MaxJobWakes] items, so one hook record's takes stay within its
// masks, and the turn at most [sandbox.MaxTurnWakes], the read ones every
// relaunch keeps counted; a wake past either is refused, left to a turn of its
// own.
//
// Mutation: count the job's items alone (no-turn-bound), and a third job is
// handed a fifth item of a full turn.
func testAJobsAndATurnsWakeItemsStopAtTheirBounds(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wake-bound"
	ctx := t.Context()
	full := func(job, prefix string) []string {
		var ids []string
		for i := range sandbox.MaxJobWakes {
			ids = append(ids, fileWake(t, s, turn, job, sealedWake(prefix+strings.Repeat("x", i),
				sandbox.MaxWakeItemBytes)).ID)
		}
		return ids
	}
	readBy := func(job string, ids []string) {
		taken, err := s.TakeNotes(ctx, turn, sandbox.NoteTake{IDs: ids, Launch: job, Statuses: sandbox.ItemStatuses,
			Read:     sandbox.NoteRead{Phase: "execute", Boundary: "step", Launch: job, Take: "take-" + job},
			Confirms: true})
		if err != nil || len(taken) != len(ids) {
			t.Fatalf("TakeNotes = %d of %d, %v", len(taken), len(ids), err)
		}
	}
	first := mustLaunched(t, s, run(turn))
	firstIDs := full(first, "a")
	if _, err := s.FileWake(ctx, turn, sandbox.WakeFiling{Launch: first,
		Wake: sealedWake("a-past", 64)}); !errors.Is(err, sandbox.ErrWakesSpent) {
		t.Errorf("a wake past the job's items answered %v, want ErrWakesSpent", err)
	}
	readBy(first, firstIDs)
	second := carryRoundOn(t, s, turn)
	readBy(second, full(second, "b"))
	third := carryRoundOn(t, s, turn)
	if _, err := s.FileWake(ctx, turn, sandbox.WakeFiling{Launch: third,
		Wake: sealedWake("c-past", 64)}); !errors.Is(err, sandbox.ErrWakesSpent) {
		t.Errorf("a wake past the turn's items answered %v, want ErrWakesSpent", err)
	}
	if got := len(wakesIn(t, s, turn)); got != sandbox.MaxTurnWakes {
		t.Errorf("the record holds %d wake items, want the turn's %d", got, sandbox.MaxTurnWakes)
	}
}

// A WAKE ITEM IS SERVED TO A HOOK ONCE: a serve its harness gave up is lost, and
// the item is never served to the job again, its deliveries left to turns of
// their own, where a note beside it is served again; a serve the inbox
// answered nothing is removed and spends nothing, so the item is served again.
//
// Mutation: hold a wake item to a note's three serves, and it is served again
// after its serve was lost.
func testAWakeItemIsServedToAHookOnce(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wake-serve"
	ctx := t.Context()
	job := mustLaunched(t, s, run(turn))
	item := fileWake(t, s, turn, job, sealedWake("w-1", 64))
	if filed, _, err := s.FileNote(ctx, turn, sandbox.NoteFiling{MaxWaiting: 5, MaxNotes: 10,
		Note: sandbox.RunNote{ID: "n-1", Sealed: []byte("seal")}}); err != nil || filed != sandbox.NoteAccepted {
		t.Fatalf("FileNote = %s, %v", filed, err)
	}
	if served, err := s.ServeHook(ctx, turn, hookServeOf(t, s, turn, job, "take-1", item.ID, "n-1")); err != nil ||
		len(served) != 2 {
		t.Fatalf("the first serve = %d, %v", len(served), err)
	}
	answeredNothing := hookServeOf(t, s, turn, job, "take-2", item.ID, "n-1")
	answeredNothing.Unanswered = []string{"take-1"}
	call := sandbox.NoteCall{Session: "mcp-1", ID: "7"}
	answeredNothing.Serve.Call = &call
	if served, err := s.ServeHook(ctx, turn, answeredNothing); err != nil || len(served) != 2 {
		t.Fatalf("a serve after one answered nothing = %d, %v, want both served again", len(served), err)
	}
	cancelled := hookServeOf(t, s, turn, job, "take-3", item.ID, "n-1")
	cancelled.Cancelled = []sandbox.NoteCall{call}
	served, err := s.ServeHook(ctx, turn, cancelled)
	if err != nil || len(served) != 1 || served[0].ID != "n-1" {
		t.Errorf("a serve after one its harness gave up served %+v, %v, want the note alone", served, err)
	}
	for _, n := range mustGet(t, s, turn).Steer.Notes {
		if n.ID == item.ID && (len(n.Serves) != 1 || n.Serves[0].Lost != sandbox.ServeCancelled || n.Served != "") {
			t.Errorf("the item holds serves %+v, served %q, want its one serve lost to the cancel", n.Serves, n.Served)
		}
	}
}

// A WAKE ITEM IS READ ONLY ONCE ITS SESSION SHOWS IT: a hook's serve leaves it
// waiting, and the settle of the job's session that shows the hook took it
// records it read off that serve, at the serve's step.
//
// Mutation: record a wake item read by its hook's serve, and it is read before
// any session showed it.
func testAWakeItemIsReadOnlyOnceItsSessionShowsIt(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wake-read"
	ctx := t.Context()
	job := mustLaunched(t, s, run(turn))
	item := fileWake(t, s, turn, job, sealedWake("w-1", 64))
	if served, err := s.ServeHook(ctx, turn, hookServeOf(t, s, turn, job, "take-1", item.ID)); err != nil || len(served) != 1 ||
		!served[0].Waiting() {
		t.Fatalf("ServeHook = %+v, %v, want the item served and waiting", served, err)
	}
	settled, err := s.SettleHook(ctx, turn, sandbox.HookSettle{Launch: job, Held: []string{"take-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(settled) != 1 || settled[0].Read == nil || settled[0].Read.Take != "take-1" ||
		settled[0].Read.Boundary != "tool_batch" || settled[0].WakeEnd() != "delivered" {
		t.Errorf("the settle answered %+v, want the item read off its serve", settled)
	}
}

// A RELAUNCH KEEPS THE WAKES ITS JOB READ AND HANDS BACK THE REST. A wake read is
// the turn's, its delivery worked, so the next job's record keeps it, its own
// job's alone; every wake the job never read, served or not, goes with the
// job, and the relaunch is handed each for its ending to report.
//
// Mutation: hand back nothing the reset removed (no-prior-wakes), and a job's
// unread wakes go unreported.
func testARelaunchKeepsTheWakesItsJobReadAndHandsBackTheRest(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wake-relaunch"
	ctx := t.Context()
	job := mustLaunched(t, s, run(turn))
	read := fileWake(t, s, turn, job, sealedWake("w-read", sandbox.MaxWakeItemBytes))
	served := fileWake(t, s, turn, job, sealedWake("w-served", sandbox.MaxWakeItemBytes))
	unread := fileWake(t, s, turn, job, sealedWake("w-unread", sandbox.MaxWakeItemBytes))
	if _, err := s.TakeNotes(ctx, turn, sandbox.NoteTake{IDs: []string{read.ID}, Launch: job,
		Statuses: sandbox.ItemStatuses, Read: sandbox.NoteRead{Phase: "execute", Boundary: "step", Launch: job,
			Take: "take-1"}, Confirms: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ServeHook(ctx, turn, hookServeOf(t, s, turn, job, "take-2", served.ID)); err != nil {
		t.Fatal(err)
	}
	next := run(turn)
	next.Continues = true
	opened, err := s.BeginLaunch(ctx, next, sandbox.Fence{})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	var handed []string
	for _, n := range opened.Prior.Wakes {
		handed = append(handed, n.ID)
	}
	if want := []string{served.ID, unread.ID}; !slices.Equal(handed, want) {
		t.Errorf("the relaunch was handed wakes %v to report, want the job's unread ones %v", handed, want)
	}
	if got := wakesIn(t, s, turn); !slices.Equal(got, []string{"w-read"}) {
		t.Errorf("the next job's record holds wakes %v, want the one its job read", got)
	}
	for _, n := range mustGet(t, s, turn).Steer.Notes {
		if n.IsWake() && n.IsFor(opened.LaunchID) {
			t.Errorf("the read wake %s is handed to the next job", n.ID)
		}
	}
}

// A PERSON CANNOT FILE A WAKE: a filing of a person's note carrying wakes is
// refused before anything is written ([sandbox.ErrNoteKind]), so no filer can
// hand a job somebody's words on the store's account.
//
// Mutation: accept a filing naming wakes, and it lands as a wake item.
func testAPersonCannotFileAWake(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wake-person"
	mustLaunched(t, s, run(turn))
	_, _, err := s.FileNote(t.Context(), turn, sandbox.NoteFiling{MaxWaiting: 5, MaxNotes: 10,
		Note: sandbox.RunNote{ID: "n-1", Sealed: []byte("seal"),
			Wake: &sandbox.WakeItem{Wakes: []sandbox.SealedWake{sealedWake("w-1", 64)}}}})
	if !errors.Is(err, sandbox.ErrNoteKind) {
		t.Errorf("a filing carrying wakes answered %v, want ErrNoteKind", err)
	}
	if got := wakesIn(t, s, turn); len(got) != 0 {
		t.Errorf("the record holds wakes %v, want none", got)
	}
}

// A WAKE ITEM IS SERVED ONLY WHOLE, AS ITS SERVER READ IT. A serve hands over
// the wakes its reader rendered from the record it read; a wake that joined the
// item since would be served, read and claimed unseen, so neither a hook's
// serve nor a poll's serves an item holding more wakes than it hands over, and
// the item waits for a serve that read it whole.
//
// Mutation: serve a wake item whatever wakes a serve hands over
// (serve-unpinned), and the joined wake is served unseen.
func testAWakeItemIsServedOnlyWholeAsItsServerReadIt(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wake-whole"
	ctx := t.Context()
	job := mustLaunched(t, s, run(turn))
	item := fileWake(t, s, turn, job, sealedWake("w-1", 64))
	read := hookServeOf(t, s, turn, job, "take-1", item.ID)
	fileWake(t, s, turn, job, sealedWake("w-2", 64))
	if served, err := s.ServeHook(ctx, turn, read); err != nil || len(served) != 0 {
		t.Errorf("a hook's serve that read the item before a wake joined it served %d, %v, want nothing",
			len(served), err)
	}
	if served, err := s.ServeNotes(ctx, turn, sandbox.NoteServe{IDs: []string{item.ID}, Launch: job,
		Statuses: sandbox.ItemStatuses, Wakes: map[string]int{item.ID: 1}}); err != nil || len(served) != 0 {
		t.Errorf("a poll's serve that read the item before a wake joined it served %d, %v, want nothing",
			len(served), err)
	}
	served, err := s.ServeHook(ctx, turn, hookServeOf(t, s, turn, job, "take-2", item.ID))
	if err != nil || len(served) != 1 || len(served[0].Wake.Wakes) != 2 {
		t.Errorf("a serve that read the item whole served %+v, %v, want the item with both wakes", served, err)
	}
}

// A JOB'S WAKES CROWD OUT NO LATE CALL: the two kinds of item are held to bounds
// of their own, so a job holding every wake item it may is still handed as
// many late calls of its round as a job holding none.
//
// Mutation: count every item for the job toward the late calls' bound
// (items-share-a-bound), and the job is handed four calls fewer.
func testAJobsWakesCrowdOutNoLateCall(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wake-late"
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	next := carryRoundOn(t, s, turn)
	for i := range sandbox.MaxJobWakes {
		fileWake(t, s, turn, next, sealedWake("w-"+strings.Repeat("x", i+1), sandbox.MaxWakeItemBytes))
	}
	for seq := 1; seq <= sandbox.MaxJobItems; seq++ {
		if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
			Call: madeAt("delegate_"+strconv.Itoa(seq), seq, seq)}) {
			t.Fatalf("late call %d was not recorded", seq)
		}
	}
	if got := lateItems(t, s, turn); len(got) != sandbox.MaxJobItems {
		t.Errorf("the job holding %d wake items is handed late calls %v, want all %d", sandbox.MaxJobWakes,
			itemCalls(got), sandbox.MaxJobItems)
	}
}
