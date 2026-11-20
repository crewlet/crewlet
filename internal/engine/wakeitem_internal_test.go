package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/workkey"
)

// wakeAbout is a wake about a run's work from bob, written in the conversation
// chat:C1, standing for one delivery, and asking for an answer.
func wakeAbout(id, text string) steer.Wake {
	return steer.Wake{ID: id, Triggers: []string{"trigger-" + id}, Keys: []string{"own-" + id, "chat-" + id},
		By: "bob", ByKind: iam.ActorHuman, Conversation: "chat:C1", Partition: "chat:C1:root-1",
		Label: provenance.Label{Source: provenance.SourceChatThread, Trust: provenance.TrustExternal,
			Authors: []provenance.Author{{Kind: iam.ActorHuman}}},
		OwesAnswer: true, At: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), Text: text}
}

// joinedRecords is every agent_turn_joined p has published, by event id.
func joinedRecords(p *pub) map[uuid.UUID]types.AgentTurnJoined {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[uuid.UUID]types.AgentTurnJoined{}
	for _, ev := range p.events {
		if got, ok := events.DataAs[*types.AgentTurnJoined](ev); ok {
			out[ev.ID] = *got
		}
	}
	return out
}

// A WAKE OPENS ONLY UNDER ITS OWN SEAL, FOR ITS TURN, ITS JOB AND ITS ID. A wake
// is somebody's words a run's model reads, on a record any writer of the
// coordination store can rewrite: so one sealed for another turn, another job
// or another wake's id, one a note's seal holds, and one a node without its key
// reads never opens; and a wake's seal never opens as a person's note.
//
// Mutation: seal wakes in the note's domain, or drop the job from openWake's
// check (wake-any-job), and this goes red.
func TestAWakeOpensOnlyUnderItsOwnSealForItsTurnJobAndID(t *testing.T) {
	t.Parallel()
	runs, _ := steersOver(t, steerRing, sandbox.NewCoordStore(coordmem.NewFleet()))
	w := wakeAbout("w-1", "use staging, not production")
	sealed, err := runs.sealWake("run-1", "job-1", w)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := runs.openWake("run-1", "job-1", sealed)
	if !ok || got.Text != w.Text || got.By != "bob" || !got.OwesAnswer || !slices.Equal(got.Keys, w.Keys) ||
		got.Label.Source != provenance.SourceChatThread || got.Conversation != "chat:C1" {
		t.Fatalf("the wake opens as %+v, %v, want what was sealed", got, ok)
	}
	note, err := runs.seal("run-1", steer.Note{ID: "w-1", Text: "use staging, not production", By: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := steersOver(t, recordseal.OneKey("k2", "another fleet's key material"),
		sandbox.NewCoordStore(coordmem.NewFleet()))
	opens := func(by *runSteers, turnID, job string, w sandbox.SealedWake) bool {
		_, ok := by.openWake(turnID, job, w)
		return ok
	}
	for name, opened := range map[string]bool{
		"for another turn":   opens(runs, "run-2", "job-1", sealed),
		"for another job":    opens(runs, "run-1", "job-2", sealed),
		"as another wake":    opens(runs, "run-1", "job-1", sandbox.SealedWake{ID: "w-2", Sealed: sealed.Sealed}),
		"from a note's seal": opens(runs, "run-1", "job-1", sandbox.SealedWake{ID: "w-1", Sealed: note.Sealed}),
		"without its key":    opens(other, "run-1", "job-1", sealed),
	} {
		if opened {
			t.Errorf("a wake opened %s", name)
		}
	}
	if _, ok := runs.open("run-1", sandbox.RunNote{ID: "w-1", Sealed: sealed.Sealed}); ok {
		t.Error("a wake's seal opened as a person's note")
	}
}

// A WAKE ITEM IS HANDED TO ITS JOB AS A WAKE, NEVER AS ANYBODY'S NOTE. Filed for
// a working job, it reaches that job's hook after every person's note in the
// same answer, its words inside a fence under the session's token, named in
// the hook's line by its writer; it counts toward no person's bound, the turn
// carries none as a note, and no reader inside no job is handed it.
//
// Mutation: route a wake item to a person's note in waitingItems, or order a
// wake ahead of a note (wake-before-note), and this goes red.
func TestAWakeItemIsHandedToItsJobAsAWakeAndNeverAsANote(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	if _, err := runs.fileWake(ctx, "run-1", launch, wakeAbout("w-1", "the deploy target is staging")); err != nil {
		t.Fatal(err)
	}
	for i := range steer.MaxPendingNotes {
		fileSealed(t, runs, "run-1", steer.Note{ID: fmt.Sprintf("n-%d", i), Text: "a note", By: "founder"})
	}
	run := mustRow(t, store, "run-1")
	token := provenance.NewBoundary()
	box := runs.inbox(identityOfRun(run), 1)
	box.Bind(ctx, launch, token)
	t.Cleanup(box.Close)
	viewHolds(t, box, steer.MaxPendingNotes+1)
	answer := readHook(t, box.Take(ctx, mcpbridge.SteerEventToolBatch, hookCall()))
	said := answer.Output.Context
	heading := prompts.WakeHeadOpen + "bob" + prompts.WakeHeadClose
	lastNote, wake := strings.LastIndex(said, "## A note from founder"), strings.Index(said, heading)
	if wake < 0 || lastNote < 0 || wake < lastNote {
		t.Fatalf("the answer does not hand the wake after every note:\n%s", said)
	}
	if !slices.ContainsFunc(provenance.Fences(said, token), func(f string) bool {
		return strings.Contains(f, "the deploy target is staging") && strings.Contains(f, "source=chat_thread")
	}) {
		t.Errorf("the wake's words are in no fence of the session's token:\n%s", said)
	}
	if !strings.Contains(answer.Line, "a message from bob about this work") {
		t.Errorf("the hook's line is %q, which does not name the wake's writer", answer.Line)
	}
	row := mustRow(t, store, "run-1")
	if outside := runs.waiting("run-1", row.Steer.Notes, ""); slices.ContainsFunc(outside, func(o openedNote) bool {
		return o.held.IsWake()
	}) {
		t.Error("a reader inside no job, a review's or a brief's, is handed the wake")
	}
}

// A WAKE ITEM IS SERVED TO ITS JOB'S HOOKS ONCE. A serve its harness gave up
// leaves the wake unread, and an unread wake runs as a turn of its own once the
// run is over, so the next step's hook is not served it again, while a
// person's note beside it is.
//
// Mutation: hold a wake to a note's three serves (wake-served-thrice), and the
// next step is handed it again.
func TestAWakeItemIsServedToItsJobsHooksOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	if _, err := runs.fileWake(ctx, "run-1", launch, wakeAbout("w-1", "the deploy target is staging")); err != nil {
		t.Fatal(err)
	}
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "a note", By: "founder"})
	box := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, box, 2)
	call := hookCall()
	first := readHook(t, box.Take(ctx, mcpbridge.SteerEventToolBatch, call)).Output.Context
	if !strings.Contains(first, "the deploy target is staging") {
		t.Fatalf("the first step is not handed the wake:\n%s", first)
	}
	box.Cancelled(ctx, *call)
	again := box.Take(ctx, mcpbridge.SteerEventToolBatch, hookCall())
	if again == "" {
		t.Fatal("the next step is handed nothing, want the note served again")
	}
	said := readHook(t, again).Output.Context
	if strings.Contains(said, "the deploy target is staging") || !strings.Contains(said, "a note") {
		t.Errorf("the next step is handed %q, want the note alone", said)
	}
}

// A WAKE IS CLAIMED ONLY ONCE A STEP READ IT, AND EVERY WAKE IS REPORTED ONCE AS
// IT ENDED. A collection's verdict that the job's session took the wake records
// every key of its delivery worked in the completion ledger, beside the join's
// mark, and publishes it delivered; a wake served and never shown taken, and
// one never served, record nothing, so each runs as a turn of its own, and the
// ending reports them unconfirmed and unread, each under one id a repeat keeps.
//
// Mutation: claim a wake its job was served and never shown taking
// (claim-on-serve), or report a wake twice under two ids, and this goes red.
func TestAWakeIsClaimedOnlyOnceReadAndEveryWakeIsReportedOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, p := steersOver(t, steerRing, store)
	ledger := ledgerstore.NewMemoryCompletions()
	runs.claimingIn(ledger)
	launch := workingRun(t, store, runs, "run-1")
	file := func(w steer.Wake) {
		t.Helper()
		if _, err := runs.fileWake(ctx, "run-1", launch, w); err != nil {
			t.Fatal(err)
		}
	}
	file(wakeAbout("w-read", "use staging"))
	box := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, box, 1)
	read := box.Take(ctx, mcpbridge.SteerEventToolBatch, hookCall())
	file(wakeAbout("w-served", "and the old flag"))
	viewHolds(t, box, 2)
	served := box.Take(ctx, mcpbridge.SteerEventToolBatch, hookCall())
	file(wakeAbout("w-unread", "and ping me after"))
	settled, err := store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: launch, Held: []string{takeOf(t, read)}})
	if err != nil {
		t.Fatal(err)
	}
	runs.confirmed(ctx, mustRow(t, store, "run-1"), settled)
	keys := func(id string) []string {
		return []string{"own-" + id, "chat-" + id, joinedKey("trigger-" + id)}
	}
	eventually(t, "the read wake's delivery recorded worked", func() bool {
		return len(ledger.Worked(ctx, "swe", keys("w-read"))) == 3
	})
	for _, id := range []string{"w-served", "w-unread"} {
		if worked := ledger.Worked(ctx, "swe", keys(id)); len(worked) != 0 {
			t.Errorf("wake %s, never shown read, is recorded worked: %v", id, worked)
		}
	}
	takeOf(t, served)
	ending := mustRow(t, store, "run-1")
	ending.Ending = &sandbox.RecordedEnding{ID: "ending-1", At: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}
	for range 2 {
		if err := runs.ended(ctx, ending); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "every wake reported", func() bool { return len(joinedRecords(p)) == 3 })
	want := map[string]types.JoinOutcome{"w-read": types.JoinDelivered, "w-served": types.JoinUnconfirmed,
		"w-unread": types.JoinUnread}
	for id, rec := range joinedRecords(p) {
		if rec.Outcome != want[rec.WakeID] || id != uuid.NewSHA1(steerJoinedNamespace, []byte("run-1/"+rec.WakeID)) ||
			rec.LaunchID != launch || rec.WrittenBy != "bob" || !slices.Equal(rec.TriggerIDs, []string{"trigger-" + rec.WakeID}) {
			t.Errorf("wake %s is reported %+v under %s", rec.WakeID, rec, id)
		}
	}
	if worked := ledger.Worked(ctx, "swe", keys("w-served")); len(worked) != 0 {
		t.Errorf("the ending recorded the unconfirmed wake worked: %v", worked)
	}
}

// A WAKE ITEM'S MESSAGE FITS ONE HOOK ANSWER BESIDE A PERSON'S WIDEST NOTE. Its
// wakes are handed oldest first, the newest kept whole and the older ones that
// do not fit left out and counted; and a newest past the room alone is cut,
// what it cut counted in its line, so every item is served at the first step
// that has room for it.
//
// Mutation: drop the newest rather than the oldest (keep-oldest), or let a
// lone wake run past the room, and this goes red.
func TestAWakeItemsMessageFitsBesideAPersonsWidestNote(t *testing.T) {
	t.Parallel()
	token := provenance.NewBoundary()
	long := func(id string) steer.Wake {
		return wakeAbout(id, strings.Repeat("\U0001F680", steer.MaxWakeRunes-len(id))+id)
	}
	wakes := []steer.Wake{long("w-1"), long("w-2"), long("w-3")}
	msg := wakeItemMessage(wakes, token, "chat:C1")
	if executor.UTF16Len(msg) > maxWakeItemUnits {
		t.Fatalf("the item runs %d units, past its %d", executor.UTF16Len(msg), maxWakeItemUnits)
	}
	if !strings.Contains(msg, wakes[2].Text) || strings.Contains(msg, wakes[0].Text) ||
		!strings.Contains(msg, prompts.WakesLeftOut(2)) && !strings.Contains(msg, prompts.WakesLeftOut(1)) {
		t.Errorf("the item does not keep its newest wake whole and count the older it left out:\n%s", msg)
	}
	if !strings.HasPrefix(msg, prompts.WakeHeadOpen) {
		t.Errorf("the item does not open with a wake's heading:\n%s", msg)
	}
	lone := wakeAbout("w-4", strings.Repeat("\U0001F680", 3*steer.MaxWakeRunes))
	cut := wakeItemMessage([]steer.Wake{lone}, token, "chat:C1")
	if executor.UTF16Len(cut) > maxWakeItemUnits || !strings.Contains(cut, "characters past what is shown here") {
		t.Errorf("a lone wake past the room runs %d units, or says nothing of its cut:\n%s",
			executor.UTF16Len(cut), cut)
	}
	if room := executor.ClaudeRoomBeside(widestNote()); maxWakeItemUnits != room {
		t.Errorf("a wake item is held to %d units, and a hook's answer has %d beside the widest note",
			maxWakeItemUnits, room)
	}
}

// A JOINED WAKE'S DELIVERY IS DROPPED AS JOINED. Its run read it, so the turn
// worked it and recorded its delivery worked under the join's mark: when the
// seat's inbox is let go the delivery runs no turn, and its skip record says it
// joined a running turn, where a delivery a turn was woken for is skipped as
// worked.
//
// Mutation: drop the join's mark from claim (claim-without-mark), and the
// joined delivery is skipped as merely worked.
func TestAJoinedWakesDeliveryIsDroppedAsJoined(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ledger := ledgerstore.NewMemoryCompletions()
	var mu sync.Mutex
	var skips []types.TurnTriggerSkipped
	d := &Dispatcher{Ledgered: inbox.Ledgered, Completions: ledger, Observe: func(_ context.Context,
		ev *events.Event) {
		if got, ok := events.DataAs[*types.TurnTriggerSkipped](ev); ok {
			mu.Lock()
			skips = append(skips, *got)
			mu.Unlock()
		}
	}}
	notice := func() *events.Event {
		return events.New(types.ExternalNotification{NotificationSource: "slack"}, events.TraceContext{})
	}
	joined, worked, fresh := notice(), notice(), notice()
	runs, _ := steersOver(t, steerRing, sandbox.NewCoordStore(coordmem.NewFleet()))
	runs.claimingIn(ledger)
	w := wakeAbout("w-1", "use staging")
	w.Triggers, w.Keys = []string{joined.ID.String()}, []string{workkey.Derive([]string{joined.ID.String()})}
	runs.claim(ctx, runIdentity{turn: steer.Turn{AgentHandle: "swe", TurnID: "run-1"}}, []steer.Wake{w})
	if err := ledger.Record(ctx, "swe", workkey.Derive([]string{worked.ID.String()}), "run-0", time.Now()); err != nil {
		t.Fatal(err)
	}
	all := []*events.Event{joined, worked, fresh}
	out, skipped := d.dropWorked(ctx, "swe", all)
	if len(out) != 1 || out[0].ID != fresh.ID {
		t.Fatalf("the dispatch keeps %d triggers, want the fresh one alone", len(out))
	}
	if skipped[joined.ID] != skipJoined || skipped[worked.ID] != skipWorked {
		t.Errorf("the joined delivery is skipped as %v and the worked one as %v, want joined and worked",
			skipped[joined.ID], skipped[worked.ID])
	}
	d.noteSkipped(ctx, "swe", all, out, skipped)
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(skips, func(s types.TurnTriggerSkipped) bool {
		return s.TriggerID == joined.ID.String() && strings.Contains(s.Reason, "joined a running turn")
	}) {
		t.Errorf("no skip record says the joined delivery joined a running turn: %+v", skips)
	}
}
