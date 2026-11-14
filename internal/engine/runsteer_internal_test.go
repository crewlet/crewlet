package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner/steersourcetest"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A note on a run's record, as this node opens it. Every case here is about what
// the record cannot vouch for itself: the coordination store authenticates no
// writer (ADR-0031), so a note reaches a model, a resumed segment's carry or a
// record of anybody's only once its seal opens, under this turn and this note.

// steersOver is a node's run steers over store under ring, and what it
// publishes.
func steersOver(t *testing.T, ring recordseal.Keyring, store sandbox.PendingStore) (*runSteers, *pub) {
	t.Helper()
	seals, err := newSteerSeals(ring)
	if err != nil {
		t.Fatal(err)
	}
	p := &pub{}
	return newRunSteers(seals, store, p), p
}

// workingRun is turnID's record with its job launched and working, anchored on
// the swe seat; it answers the job.
func workingRun(t *testing.T, store sandbox.PendingStore, runs *runSteers, turnID string) string {
	t.Helper()
	opened, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: turnID, AgentID: sweAgent, AgentHandle: "swe", Role: "SWE", Harness: "claude-code",
		TraceID: "tr-1", CreatedAt: time.Now().UTC(),
		Steer: sandbox.RunSteer{Seat: runs.anchor(turnID, sweAgent, "swe")},
	}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	suspended, err := store.MarkSuspended(t.Context(), turnID,
		sandbox.Suspension{State: json.RawMessage(`{"version":4}`), Iteration: 1})
	if err != nil || !suspended {
		t.Fatalf("MarkSuspended = %v, %v", suspended, err)
	}
	return opened.LaunchID
}

// fileRaw files a note on turnID's record exactly as given — the write any
// party that reaches the store can make.
func fileRaw(t *testing.T, store sandbox.PendingStore, turnID string, n sandbox.RunNote) {
	t.Helper()
	filed, _, err := store.FileNote(t.Context(), turnID, sandbox.NoteFiling{
		Note: n, MaxWaiting: steer.MaxPendingNotes, MaxNotes: steer.MaxTurnNotes,
	})
	if err != nil || filed != sandbox.NoteAccepted {
		t.Fatalf("FileNote %s = %s, %v", n.ID, filed, err)
	}
}

// fileSealed seals note for turnID under runs and files it.
func fileSealed(t *testing.T, runs *runSteers, turnID string, note steer.Note) {
	t.Helper()
	filed, err := runs.File(t.Context(), turnID, note)
	if err != nil || filed.Status != steer.StatusAccepted {
		t.Fatalf("File %s = %+v, %v", note.ID, filed, err)
	}
}

// boundInbox is the inbox of turnID's run, bound to its job and closed with
// the case.
func boundInbox(t *testing.T, runs *runSteers, store sandbox.PendingStore, turnID, launch string) *runInbox {
	t.Helper()
	run, found, err := store.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("Get %s = %v, %v", turnID, found, err)
	}
	inbox := runs.inbox(identityOfRun(run), 1)
	inbox.Bind(t.Context(), launch)
	t.Cleanup(inbox.Close)
	return inbox
}

// viewHolds waits until the inbox's view of the record holds n waiting notes
// that open.
func viewHolds(t *testing.T, inbox *runInbox, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, watch := inbox.bound()
		view, opened, changed := watch.snapshot()
		if view.Found && len(opened) == n {
			return
		}
		select {
		case <-changed:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("the view holds %d opened notes (found %v), want %d", len(opened), view.Found, n)
		}
	}
}

// steered is every agent_turn_steered p has published.
func steered(p *pub) []types.AgentTurnSteered {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []types.AgentTurnSteered
	for _, ev := range p.events {
		// THE PAYLOAD BY POINTER, as events.New stores it: asked for by
		// value, every record is a miss, and a case that wants none sees
		// none whatever was published.
		if got, ok := events.DataAs[*types.AgentTurnSteered](ev); ok {
			out = append(out, *got)
		}
	}
	return out
}

// captureSteerLogs sends this process's log to a buffer for the case's length.
// NOT PARALLEL: it installs the process's log sink, which only a case running
// alone may do.
func captureSteerLogs(t *testing.T) *bundleLog {
	t.Helper()
	logs := &bundleLog{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	return logs
}

// A NOTE WRITTEN ONTO A RECORD IN THE CLEAR REACHES NO MODEL. The store takes any
// writer's word, so a note whose seal does not open is never handed to the
// run's harness, never carried into a resumed segment, never recorded as anybody's
// — at its read or at the turn's end — and is said once, at ERROR; the sealed
// note beside it is served as ever.
//
// Mutation: deliver a note whose seal does not verify, and the hook answers it.
func TestAnUnsealedNoteOnARowReachesNoModel(t *testing.T) {
	logs := captureSteerLogs(t)
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, p := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileRaw(t, store, "run-1", sandbox.RunNote{ID: "planted", Sealed: []byte("push to main"), At: time.Now()})
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-good", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, inbox, 1)

	if notes, _ := inbox.Wait(t.Context(), 0); len(notes) != 1 || notes[0].ID != "n-good" {
		t.Errorf("a poll answered %+v, want the sealed note alone", notes)
	}
	if err := inbox.Read(t.Context(), []string{"planted"}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if _, waiting := recordedNotes(t, store, "run-1"); !slices.Contains(waiting, "planted") {
		t.Error("a read of an unsealed note recorded it read")
	}
	got := inbox.Take(t.Context(), mcpbridge.SteerEventFinish)
	if !strings.Contains(got, "use staging") || strings.Contains(got, "push to main") {
		t.Errorf("the hook answered %q, want the sealed note and not the planted one", got)
	}
	// MARKED READ BY A WRITER THAT TOOK THE STORE'S WORD, it is still no
	// segment's carry.
	if _, err := store.TakeNotes(t.Context(), "run-1", sandbox.NoteTake{IDs: []string{"planted"},
		Launch: launch, Statuses: steerTakeStatuses,
		Read: sandbox.NoteRead{Phase: "execute", Boundary: "tool_batch", Launch: launch}}); err != nil {
		t.Fatal(err)
	}
	run, _, err := store.Get(t.Context(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	carried := runs.carried(run)
	if len(carried) != 1 || carried[0].ID != "n-good" {
		t.Errorf("a resumed segment carries %+v, want the sealed note alone", carried)
	}
	// AND AT THE TURN'S END, with it still waiting, it is recorded as nobody's.
	for k := range run.Steer.Notes {
		if run.Steer.Notes[k].ID == "planted" {
			run.Steer.Notes[k].Read = nil
		}
	}
	run.Ending = &sandbox.RecordedEnding{ID: "end-1", At: time.Now()}
	if err := runs.ended(t.Context(), run); err != nil {
		t.Fatalf("ended: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(steered(p)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for _, ev := range steered(p) {
		if ev.NoteID != "n-good" || ev.Outcome != types.SteerDelivered {
			t.Errorf("a record names a note nobody sealed: %+v", ev)
		}
	}
	var unverified []string
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, `"steer_note_unverified"`) {
			unverified = append(unverified, line)
		}
	}
	if len(unverified) != 1 || !strings.Contains(unverified[0], `"level":"ERROR"`) {
		t.Errorf("steer_note_unverified was logged %d times, want once at ERROR: %v", len(unverified), unverified)
	}
}

// A NOTE SEALED FOR ANOTHER TURN IS NOT THIS TURN'S: a genuine note copied from
// one record onto another opens under the fleet's key and names the turn it was
// written to, so it is delivered on neither the wrong turn nor under another
// note's id.
//
// Mutation: open a seal without comparing the turn and the id it names.
func TestANoteSealedForAnotherTurnIsNotDelivered(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, p := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	elsewhere, err := runs.seal("run-2", steer.Note{ID: "n-1", Text: "drop the table", By: "founder"})
	if err != nil {
		t.Fatal(err)
	}
	fileRaw(t, store, "run-1", elsewhere)
	renamed, err := runs.seal("run-1", steer.Note{ID: "n-2", Text: "drop the table", By: "founder"})
	if err != nil {
		t.Fatal(err)
	}
	renamed.ID = "n-3"
	fileRaw(t, store, "run-1", renamed)
	inbox := boundInbox(t, runs, store, "run-1", launch)
	if got := inbox.Take(t.Context(), mcpbridge.SteerEventFinish); got != "" {
		t.Errorf("a note sealed for another turn or note was delivered: %s", got)
	}
	_, waiting := recordedNotes(t, store, "run-1")
	if len(waiting) != 2 {
		t.Errorf("the notes waiting are %v, want both untouched", waiting)
	}
	if got := steered(p); len(got) != 0 {
		t.Errorf("records were published for notes that are not this turn's: %+v", got)
	}
}

// A NOTE UNDER A KEY THIS NODE DOES NOT HOLD WAITS — it is not refused, because
// a node that holds the key reads it — and the warning names the keys this node
// does hold, which is what the person fixing the keyring needs.
//
// Mutation: treat an unknown key as a seal that does not verify, and the note
// is logged at ERROR and the WARN names nothing.
func TestANoteUnderAnUnheldKeyWaitsAndNamesTheHeldKeys(t *testing.T) {
	logs := captureSteerLogs(t)
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	oldRing := recordseal.OneKey("k-old", "the old fleet key material")
	newRing := recordseal.OneKey("k-new", "the new fleet key material")
	sealer, _ := steersOver(t, oldRing, store)
	reader, p := steersOver(t, newRing, store)
	launch := workingRun(t, store, reader, "run-1")
	fileSealed(t, sealer, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})

	inbox := boundInbox(t, reader, store, "run-1", launch)
	if got := inbox.Take(t.Context(), mcpbridge.SteerEventFinish); got != "" {
		t.Errorf("a note under an unheld key was delivered: %s", got)
	}
	if _, waiting := recordedNotes(t, store, "run-1"); !slices.Equal(waiting, []string{"n-1"}) {
		t.Fatalf("the note does not wait: %v", waiting)
	}
	// The watch Bind starts may open the note first and log the warning on its
	// own goroutine, after Take has returned.
	eventually(t, "the unheld key's warning", func() bool {
		return strings.Contains(logs.String(), `"steer_note_key_unknown"`)
	})
	logged := logs.String()
	var warned string
	for line := range strings.SplitSeq(logged, "\n") {
		if strings.Contains(line, `"steer_note_key_unknown"`) {
			warned = line
		}
	}
	if !strings.Contains(warned, `"level":"WARN"`) || !strings.Contains(warned, `"k-new"`) {
		t.Errorf("the warning does not name the held keys: %q", warned)
	}
	if strings.Contains(logged, "steer_note_unverified") {
		t.Errorf("a note under an unheld key was reported as one that does not verify:\n%s", logged)
	}
	// A NODE THAT HOLDS THE KEY READS IT.
	both := recordseal.Keyring{ActiveID: "k-new", Keys: append(slices.Clone(newRing.Keys), oldRing.Keys...)}
	rotated, _ := steersOver(t, both, store)
	if got := boundInbox(t, rotated, store, "run-1", launch).Take(t.Context(), mcpbridge.SteerEventFinish); !strings.Contains(got, "use staging") {
		t.Errorf("a node holding the key did not read the note: %q", got)
	}
	if got := steered(p); len(got) != 0 {
		t.Errorf("the node without the key recorded the note: %+v", got)
	}
}

// A CONFIRMATION OF A NOTE UNDER A KEY THIS NODE DOES NOT HOLD WAITS FOR A NODE
// THAT HOLDS IT. In a fleet whose nodes hold different keys, the node serving a
// run's session can lack the key a note is sealed under while the node that
// served the note to the job's harness held it: the run's seat moved, or a
// recovery opened its session on another node. This node's take hands over
// only a note it opened, so a confirmation naming that note took no read of
// it, and answered done, the plugin never confirmed it again: the read was
// recorded nowhere. So the confirmation is turned away
// (mcpbridge.ErrInboxNotServing), the note still waiting and served to the
// job, and said once, at WARN with the keys this node holds, however often the
// plugin asks again. A note beside it that this node opens is recorded and
// reported all the same, and reported again through the job's earlier read
// when the plugin asks again. The confirmation made again where the key is
// held is recorded there. A note this node cannot open that waits no more, or
// that waits for another job, is no such note: answered done.
//
// Mutation: answer the note this node cannot open done, and the first
// confirmation goes red; say it at every confirmation, and its line is
// counted twice; turn it away before the take, or before the job's earlier
// read is reported, and the note beside it goes unrecorded or unreported
// again; turn a note this node cannot open away whether or not it waits, or
// whatever job it is for, and the last two confirmations go red. Not
// parallel: it installs the process's log sink.
func TestAConfirmationOfANoteUnderAnUnheldKeyWaitsForANodeThatHoldsIt(t *testing.T) {
	logs := captureSteerLogs(t)
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	oldRing := recordseal.OneKey("k-old", "the old fleet key material")
	newRing := recordseal.OneKey("k-new", "the new fleet key material")
	both := recordseal.Keyring{ActiveID: "k-new", Keys: append(slices.Clone(newRing.Keys), oldRing.Keys...)}
	sealer, _ := steersOver(t, oldRing, store)
	holder, held := steersOver(t, both, store)
	keyless, p := steersOver(t, newRing, store)
	launch := workingRun(t, store, holder, "run-1")
	fileSealed(t, sealer, "run-1", steer.Note{ID: "n-old", Text: "use staging", By: "founder"})
	fileSealed(t, holder, "run-1", steer.Note{ID: "n-new", Text: "and keep the flag off", By: "founder"})

	// SERVED TO THE JOB by a node that holds both keys.
	served := boundInbox(t, holder, store, "run-1", launch)
	viewHolds(t, served, 2)
	if notes, _ := served.Wait(ctx, 0); len(notes) != 2 {
		t.Fatalf("the poll was answered %+v, want n-old and n-new", notes)
	}

	// THE SESSION GOES ON ON A NODE WITHOUT THE OLD KEY, which the plugin
	// confirms both notes to, and confirms them to again.
	inbox := boundInbox(t, keyless, store, "run-1", launch)
	viewHolds(t, inbox, 1)
	for try := range 2 {
		if err := inbox.Read(ctx, []string{"n-old", "n-new"}); !errors.Is(err, mcpbridge.ErrInboxNotServing) {
			t.Fatalf("confirmation %d of a note this node cannot open answered %v, want ErrInboxNotServing",
				try+1, err)
		}
	}
	for _, n := range mustRow(t, store, "run-1").Steer.Notes {
		if n.ID == "n-old" && (!n.Waiting() || n.Served != launch) {
			t.Fatalf("the confirmations left n-old %+v, want it waiting, served to job %s", n, launch)
		}
	}
	if read := readOn(ctx, store, "run-1", "n-new"); read == nil || read.Launch != launch ||
		read.Boundary != string(types.SteerBoundaryStep) {
		t.Fatalf("n-new, which this node opens, is read %+v, want it read at job %s's step", read, launch)
	}
	// N-NEW REPORTED BY EACH CONFIRMATION, the second through the job's earlier
	// read, and n-old by neither.
	eventually(t, "n-new reported by each confirmation", func() bool { return len(steered(p)) == 2 })
	for _, rec := range steered(p) {
		if rec.NoteID != "n-new" || rec.Outcome != types.SteerDelivered {
			t.Errorf("the node without the key reported %+v, want n-new delivered alone", rec)
		}
	}

	// MADE AGAIN WHERE THE KEY IS HELD, the confirmation is recorded, and
	// reported there.
	if err := served.Read(ctx, []string{"n-old", "n-new"}); err != nil {
		t.Fatalf("the confirmation made again where the key is held answered %v", err)
	}
	if read := readOn(ctx, store, "run-1", "n-old"); read == nil || read.Launch != launch ||
		read.Boundary != string(types.SteerBoundaryStep) {
		t.Fatalf("the confirmation made again left n-old read %+v, want it read at job %s's step", read, launch)
	}
	eventually(t, "n-old reported where its key is held", func() bool {
		return slices.ContainsFunc(steered(held), func(rec types.AgentTurnSteered) bool {
			return rec.NoteID == "n-old" && rec.Outcome == types.SteerDelivered
		})
	})

	// ONCE MORE ON THE NODE WITHOUT THE KEY, the note read: no such note.
	if err := inbox.Read(ctx, []string{"n-old"}); err != nil {
		t.Errorf("a confirmation of a note this node cannot open that waits no more answered %v, "+
			"want it done", err)
	}
	// AN ITEM FOR ANOTHER JOB UNDER THE OLD KEY, written onto the record whole,
	// as a party that reaches the store could: no take of this job's would
	// hand it over, wherever the key is held.
	other, err := sealer.seal("run-1", steer.Note{ID: "i-before", Text: "item for the job before",
		By: "founder", ByKind: iam.ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	other.For = "job-before"
	if _, ok, err := fleet.MutateSandboxRun(ctx, "run-1", func(current coord.Record) (coord.SandboxRunChange, error) {
		var row sandbox.PendingRun
		if err := json.Unmarshal(current.Value, &row); err != nil {
			return coord.SandboxRunChange{}, err
		}
		row.Steer.Seq++
		other.Seq = row.Steer.Seq
		row.Steer.Notes = append(row.Steer.Notes, other)
		raw, err := json.Marshal(row)
		return coord.SandboxRunChange{Put: raw}, err
	}); err != nil || !ok {
		t.Fatalf("write the other job's item: %v %v", ok, err)
	}
	if err := inbox.Read(ctx, []string{"i-before"}); err != nil {
		t.Errorf("a confirmation naming another job's item this node cannot open answered %v, want it done", err)
	}

	// SAID ONCE, for the one note that waited for its key.
	var said []map[string]any
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "steer_read_key_unknown" {
			said = append(said, rec)
		}
	}
	if len(said) != 1 {
		t.Fatalf("steer_read_key_unknown was logged %d times, want once: %v", len(said), said)
	}
	keys, _ := said[0]["held_keys"].([]any)
	if said[0]["level"] != "WARN" || said[0]["turn_id"] != "run-1" || said[0]["note_id"] != "n-old" ||
		!slices.Equal(keys, []any{"k-new"}) {
		t.Errorf("the line %v doesn't name the turn, the note and the one key this node holds, at WARN", said[0])
	}
}

// AN ANCHOR THAT DOES NOT OPEN DECIDES NO AUTHORITY: a person's authority to
// steer a turn is decided on the seat the run's record names, so a record with
// no anchor, one that does not verify, or one sealed for another turn names no
// seat anybody can decide on — and says why, rather than naming one. The why is
// a VERDICT the steer tool words for its readers, never a sentence of this
// node's: one sealed under a key this node does not hold is the condition the
// fleet's keys clear ([steer.ErrSeatKeyUnknown]), and the rest a record no
// keyring opens.
//
// Mutation: answer an unverified anchor's agent, and the planted seat decides;
// answer the unheld key as an anchor that does not verify, and its row fails.
func TestAnAnchorThatDoesNotOpenDecidesNoAuthority(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	stranger, _ := steersOver(t, recordseal.OneKey("k-x", "another fleet's key material"), store)
	for _, tc := range []struct {
		name   string
		anchor func(turnID string) []byte
		why    error
	}{
		{"no anchor", func(string) []byte { return nil }, steer.ErrSeatUnnamed},
		{"an anchor in the clear", func(turnID string) []byte {
			return []byte(`{"turn_id":"` + turnID + `","agent_id":"x","handle":"ceo"}`)
		}, steer.ErrSeatUnverified},
		{"another turn's anchor", func(string) []byte {
			return runs.anchor("run-9", "x", "ceo")
		}, steer.ErrSeatUnverified},
		{"an anchor under an unheld key", func(turnID string) []byte {
			return stranger.anchor(turnID, "x", "ceo")
		}, steer.ErrSeatKeyUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turnID := "run-" + strings.ReplaceAll(tc.name, " ", "-")
			if _, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
				TurnID: turnID, AgentID: sweAgent, AgentHandle: "swe", Role: "SWE",
				Harness: "claude-code", Steer: sandbox.RunSteer{Seat: tc.anchor(turnID)},
			}, sandbox.Fence{}); err != nil {
				t.Fatal(err)
			}
			seat, found, err := runs.Seat(t.Context(), turnID)
			if err != nil || !found {
				t.Fatalf("Seat = %v, %v", found, err)
			}
			if seat.Agent != "" || seat.Handle != "" || !errors.Is(seat.Unverified, tc.why) {
				t.Errorf("the record's seat read as %+v, want no seat and why (%v)", seat, tc.why)
			}
		})
	}
	launchedRun(t, store, runs, "run-good", nil, nil)
	if seat, _, _ := runs.Seat(t.Context(), "run-good"); seat.Agent != sweAgent || seat.Unverified != nil {
		t.Errorf("a sealed anchor read as %+v", seat)
	}
}

// AN ANCHOR UNDER A KEY THIS NODE DOES NOT HOLD NAMES THE KEYS IT HOLDS IN ITS
// LOG, at WARN and with the turn, where whoever administers the keyring reads
// them: steer_turn's caller is told only who fixes it, and an anchor in a key
// this node lacks is no forgery, so nothing is said at ERROR.
//
// Mutation: drop held_keys from the line, or log it as steer_seat_unverified,
// and this fails. Not parallel: it installs the process's log sink.
func TestAnAnchorUnderAnUnheldKeyNamesTheHeldKeysInTheLog(t *testing.T) {
	logs := captureSteerLogs(t)
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	stranger, _ := steersOver(t, recordseal.OneKey("k-x", "another fleet's key material"), store)
	if _, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "run-1", AgentID: sweAgent, AgentHandle: "swe", Role: "SWE",
		Harness: "claude-code", Steer: sandbox.RunSteer{Seat: stranger.anchor("run-1", sweAgent, "swe")},
	}, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	if seat, found, err := runs.Seat(t.Context(), "run-1"); err != nil || !found ||
		!errors.Is(seat.Unverified, steer.ErrSeatKeyUnknown) {

		t.Fatalf("Seat = %+v, %v, %v, want the anchor unopened", seat, found, err)
	}
	var warned []map[string]any
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch rec["msg"] {
		case "steer_seat_key_unknown":
			warned = append(warned, rec)
		case "steer_seat_unverified":
			t.Errorf("an anchor under an unheld key was reported as one that does not verify: %v", rec)
		}
	}
	if len(warned) != 1 {
		t.Fatalf("steer_seat_key_unknown was logged %d times, want once: %v", len(warned), warned)
	}
	held, _ := warned[0]["held_keys"].([]any)
	if warned[0]["level"] != "WARN" || warned[0]["turn_id"] != "run-1" ||
		!slices.Equal(held, []any{"k1"}) {
		t.Errorf("the line %v doesn't name the turn and the one key this node holds, at WARN", warned[0])
	}
}

// A CLAUDE CODE TAKE FITS ITS BUDGET IN UTF-16 UNITS: the harness measures a hook's
// context as JavaScript does, so notes of characters outside the Basic
// Multilingual Plane count two units each — three full notes of emoji are 7,000
// characters, inside the budget by a count of characters and past it by the
// harness's. The oldest that fit are taken, and the rest wait for the next step.
//
// Mutation: count runes in executor.UTF16Len, and the take hands all three
// to a harness that files the answer away unread.
func TestAClaudeTakeFitsItsBudgetInUTF16Units(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	text := strings.Repeat("\U0001F680", steer.MaxNoteRunes)
	for _, id := range []string{"n-1", "n-2", "n-3"} {
		fileSealed(t, runs, "run-1", steer.Note{ID: id, Text: text, By: "founder", ByKind: iam.ActorHuman})
	}
	inbox := boundInbox(t, runs, store, "run-1", launch)
	answer := inbox.Take(t.Context(), mcpbridge.SteerEventFinish)
	var hook struct {
		Output struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(answer), &hook); err != nil {
		t.Fatalf("the hook's answer does not decode: %v: %q", err, answer)
	}
	if n := executor.UTF16Len(hook.Output.Context); n > executor.ClaudeContextBudget+200 {
		t.Errorf("the hook carries %d UTF-16 units, past the budget of %d", n, executor.ClaudeContextBudget)
	}
	if _, waiting := recordedNotes(t, store, "run-1"); !slices.Equal(waiting, []string{"n-3"}) {
		t.Errorf("the notes still waiting are %v, want the newest, which did not fit", waiting)
	}
}

// A RESUMED TURN CARRIES ITS NOTES IN THE ORDER THEY WERE READ — the record's
// own order of the writes that read them, whatever waits between them and
// whatever each writer's clock said. A record holds a waiting note between two
// read ones whenever a note waits while a later one is read — a seal this node
// cannot open yet, or a later note a harness took first — and the writes that read
// notes are made by whichever nodes launch the run, serve its bridge and resume
// the turn, each stamping its own time. The carry is what every later phase and
// brief renders in order: two notes from one person that disagree must reach the
// reviewer with the later read last, since a note is the later word.
//
// Mutation: sort on the reads' times, or with a comparator that ties every pair
// with a waiting note in it, as carried did, and the read notes come back out of
// the order they were read in.
func TestAResumedTurnCarriesItsNotesInTheOrderRead(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	carriedIDs := func(runs *runSteers, run sandbox.PendingRun) []string {
		var got []string
		for _, r := range runs.carried(run) {
			got = append(got, r.Note.ID)
		}
		return got
	}

	t.Run("a waiting note between two read ones", func(t *testing.T) {
		t.Parallel()
		runs, _ := steersOver(t, steerRing, sandbox.NewCoordStore(coordmem.NewFleet()))
		sealed := func(seq int, id string, read *sandbox.NoteRead) sandbox.RunNote {
			t.Helper()
			n, err := runs.seal("run-1", steer.Note{ID: id, Text: "note " + id, By: "founder",
				ByKind: iam.ActorHuman})
			if err != nil {
				t.Fatal(err)
			}
			n.Seq, n.Read = seq, read
			return n
		}
		readIn := func(order int) *sandbox.NoteRead {
			return &sandbox.NoteRead{At: at, Order: order, Phase: "execute", Iteration: 1, Round: 1,
				Boundary: string(types.SteerBoundaryStep), Launch: "job-1"}
		}
		run := sandbox.PendingRun{TurnID: "run-1", Steer: sandbox.RunSteer{Seq: 3, Reads: 2,
			Notes: []sandbox.RunNote{
				sealed(1, "use-postgres", readIn(2)),
				sealed(2, "waits", nil),
				sealed(3, "use-sqlite", readIn(1)),
			}}}
		if got, want := carriedIDs(runs, run), []string{"use-sqlite", "use-postgres"}; !slices.Equal(got, want) {
			t.Errorf("the turn carries %v, want the read notes in the order read, %v", got, want)
		}
	})

	t.Run("reads two nodes stamped on clocks out of step", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		fleet := coordmem.NewFleet()
		// THE NODE THAT READ FIRST IS FIVE MINUTES AHEAD of the one that read
		// second: their reads' times say the opposite of the order they were
		// made in.
		ahead := sandbox.NewCoordStore(fleet).WithClock(func() time.Time { return at.Add(5 * time.Minute) })
		behind := sandbox.NewCoordStore(fleet).WithClock(func() time.Time { return at })
		runs, _ := steersOver(t, steerRing, ahead)
		launch := workingRun(t, ahead, runs, "run-1")
		fileSealed(t, runs, "run-1", steer.Note{ID: "use-postgres", Text: "use postgres", By: "founder"})
		fileSealed(t, runs, "run-1", steer.Note{ID: "use-sqlite", Text: "actually, sqlite", By: "founder"})
		for _, read := range []struct {
			store *sandbox.CoordStore
			id    string
		}{{ahead, "use-postgres"}, {behind, "use-sqlite"}} {
			taken, err := read.store.TakeNotes(ctx, "run-1", sandbox.NoteTake{IDs: []string{read.id},
				Launch: launch, Statuses: steerTakeStatuses, Read: sandbox.NoteRead{Phase: "execute",
					Iteration: 1, Round: 1, Boundary: string(types.SteerBoundaryToolBatch), Launch: launch}})
			if err != nil || len(taken) != 1 {
				t.Fatalf("the take of %s = %v, %v", read.id, taken, err)
			}
		}
		run := mustRow(t, ahead, "run-1")
		if got, want := carriedIDs(runs, run), []string{"use-postgres", "use-sqlite"}; !slices.Equal(got, want) {
			var reads []string
			for _, n := range run.Steer.Notes {
				reads = append(reads, fmt.Sprintf("%s read %d at %s", n.ID, n.Read.Order,
					n.Read.At.Format(time.TimeOnly)))
			}
			t.Errorf("the turn carries %v, want the notes in the order their writes read them, %v (%v)",
				got, want, reads)
		}
	})
}

// A NOTE'S INBOX ID IS ITS JOB'S. OpenCode's message ids are unique across a
// box's whole database, which a relaunch in the same box shares, and it answers
// a second admission under an id it holds with the first — so the id a note is
// admitted under is derived from the run's job and the note: the same for every
// poll of one job, which admits it once, and another for the next job, which
// would otherwise be refused the note its predecessor's session already holds.
//
// Mutation: derive the id from the note alone, and the relaunch's poll answers
// the first job's id.
func TestANotesInboxIDIsItsJobs(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	first := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "req-1", Text: "use staging", By: "founder",
		ByKind: iam.ActorHuman})
	waitFor := func(inbox *runInbox) []mcpbridge.SteerNote {
		t.Helper()
		viewHolds(t, inbox, 1)
		notes, _ := inbox.Wait(t.Context(), 0)
		if len(notes) != 1 || notes[0].ID != "req-1" || notes[0].Seq != 1 {
			t.Fatalf("the poll answered %+v, want the one note", notes)
		}
		return notes
	}
	firstNotes := waitFor(boundInbox(t, runs, store, "run-1", first))
	if again := waitFor(boundInbox(t, runs, store, "run-1", first)); again[0].Inbox != firstNotes[0].Inbox {
		t.Errorf("two polls of one job answered ids %q and %q, want one", firstNotes[0].Inbox, again[0].Inbox)
	}
	if want := executor.OpenCodeInboxID(first, "req-1"); firstNotes[0].Inbox != want {
		t.Errorf("the note's inbox id is %q, want %q, the job's", firstNotes[0].Inbox, want)
	}
	relaunched, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "run-1", AgentID: sweAgent, AgentHandle: "swe", Role: "SWE", Harness: "opencode",
	}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	second := waitFor(boundInbox(t, runs, store, "run-1", relaunched.LaunchID))
	if second[0].Inbox == firstNotes[0].Inbox ||
		second[0].Inbox != executor.OpenCodeInboxID(relaunched.LaunchID, "req-1") {
		t.Errorf("the relaunch's poll answered id %q, want its own job's, not %q", second[0].Inbox,
			firstNotes[0].Inbox)
	}
}

// droppingWatch is a run store whose first watch of a run's notes stops
// hearing after its first view — the close the coordination contract says
// means "watch again" (a broker that went away, a watch the store gave up on).
type droppingWatch struct {
	sandbox.PendingStore
	once    sync.Once
	dropped chan struct{}
}

func (w *droppingWatch) WatchNotes(ctx context.Context, turnID string) (<-chan sandbox.NotesView, error) {
	first := false
	w.once.Do(func() { first = true })
	if !first {
		return w.PendingStore.WatchNotes(ctx, turnID)
	}
	ctx, cancel := context.WithCancel(ctx)
	views, err := w.PendingStore.WatchNotes(ctx, turnID)
	if err != nil {
		cancel()
		return nil, err
	}
	out := make(chan sandbox.NotesView, 1)
	go func() {
		defer close(w.dropped)
		defer close(out)
		defer cancel()
		select {
		case view, ok := <-views:
			if ok {
				out <- view
			}
		case <-ctx.Done():
		}
	}()
	return out, nil
}

// A RUN'S VIEW OF ITS NOTES WATCHES AGAIN WHEN ITS WATCH STOPS HEARING. A hook
// after a batch and every poll of the notes route are answered from the inbox's
// view of the record, never a store read; a view that stopped at the first
// broker hiccup would answer "nothing waiting" for the rest of the run, and
// every note would wait for the run's finish or its review. So a watch that
// closes is opened again, and a note filed after the close reaches both.
//
// Mutation: stop following once the first watch closes, and the note never
// reaches the view.
func TestARunsViewOfItsNotesWatchesAgainWhenItsWatchCloses(t *testing.T) {
	t.Parallel()
	store := &droppingWatch{PendingStore: sandbox.NewCoordStore(coordmem.NewFleet()),
		dropped: make(chan struct{})}
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	inbox := boundInbox(t, runs, store, "run-1", launch)
	select {
	case <-store.dropped:
	case <-time.After(10 * time.Second):
		t.Fatal("the first watch was never dropped, so this asserts nothing")
	}
	fileSealed(t, runs, "run-1", steer.Note{ID: "req-1", Text: "use staging", By: "founder",
		ByKind: iam.ActorHuman})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if notes, _ := inbox.Wait(t.Context(), 0); len(notes) == 1 && notes[0].ID == "req-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a note filed after the watch closed never reached the run's poll")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if answer := inbox.Take(t.Context(), mcpbridge.SteerEventToolBatch); !strings.Contains(answer, "use staging") {
		t.Errorf("the hook after a batch was answered %q, want the note filed after the watch closed", answer)
	}
}

// stallingStore is a run store whose note reads — the record's read and a take
// — wait while a case holds them.
type stallingStore struct {
	sandbox.PendingStore
	mu   sync.Mutex
	gate chan struct{}
}

func (s *stallingStore) wait(ctx context.Context) {
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
}

func (s *stallingStore) Get(ctx context.Context, turnID string) (sandbox.PendingRun, bool, error) {
	s.wait(ctx)
	return s.PendingStore.Get(ctx, turnID)
}

func (s *stallingStore) TakeNotes(ctx context.Context, turnID string, take sandbox.NoteTake) ([]sandbox.RunNote, error) {
	s.wait(ctx)
	return s.PendingStore.TakeNotes(ctx, turnID, take)
}

// hold holds every note read until the function it answers is called.
func (s *stallingStore) hold() func() {
	gate := make(chan struct{})
	s.mu.Lock()
	s.gate = gate
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if s.gate == gate {
			s.gate = nil
		}
		s.mu.Unlock()
		close(gate)
	}
}

// endRun plays a run's ending as the coordinator finishes one: decided under
// fence, every note no job ran with reported expired, and the record deleted.
func endRun(ctx context.Context, t *testing.T, store sandbox.PendingStore, runs *runSteers, turnID string,
	fence sandbox.Fence,
) {
	t.Helper()
	decided, ok, err := store.DecideEnding(ctx, turnID, sandbox.Decision{License: sandbox.License{
		Fence: fence, WhileIn: sandbox.Active, Launch: sandbox.EveryLaunch}})
	if err != nil || !ok || decided.Ending == nil {
		t.Fatalf("DecideEnding = %v, %v (%+v)", ok, err, decided.Ending)
	}
	if err := runs.ended(ctx, decided); err != nil {
		t.Fatalf("ended: %v", err)
	}
	if _, _, err := store.Finish(ctx, turnID, decided.Ending.ID); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

// A RUN'S RECORD IS A STEER SOURCE LIKE THE FIRST SEGMENT'S BOX: the one suite
// every source runs (internal/agent/runner/steersourcetest), against the source
// a segment resuming the turn reads — its takes pinned to the claimed job, the
// resumed status and its lease, Waiting answered from its view of the record,
// and a brief's notes reported expired once, by the run's ending alone, whether
// its relaunch recorded them read and failed or another node ended the run
// first.
//
// Mutation: answer Waiting false, read the store from Waiting, publish from
// Return, or leave a never-started job's brief reads out of the ending's, and
// the suite goes red.
func TestARunsRecordIsASteerSource(t *testing.T) {
	t.Parallel()
	steersourcetest.Run(t, func(t *testing.T) steersourcetest.Source {
		ctx := t.Context()
		store := &stallingStore{PendingStore: sandbox.NewCoordStore(coordmem.NewFleet())}
		runs, published := steersOver(t, steerRing, store)
		launch := workingRun(t, store, runs, "run-1")
		claimed, won, err := store.ClaimForResume(ctx, "run-1", sandbox.CompletionTail(launch), sandbox.Fence{})
		if err != nil || !won {
			t.Fatalf("ClaimForResume = %v, %v", won, err)
		}
		fence := sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch}
		source := runs.source(ctx, claimed, fence)
		t.Cleanup(func() { source.Close(t.Context()) })
		return steersourcetest.Source{
			SteerSource: source,
			File: func(t *testing.T, n steer.Note) {
				t.Helper()
				fileSealed(t, runs, "run-1", n)
			},
			Expired: func() []string {
				var out []string
				for _, rec := range steered(published) {
					if rec.Outcome == types.SteerExpired {
						out = append(out, rec.NoteID)
					}
				}
				return out
			},
			LaunchFailed: func(t *testing.T, notes []steer.Note) {
				t.Helper()
				sealed, err := runs.briefNotes("run-1", 2, notes, source)
				if err != nil {
					t.Fatal(err)
				}
				relaunch := claimed
				relaunch.BriefNotes = sealed
				if _, err := store.BeginLaunch(ctx, relaunch, fence); err != nil {
					t.Fatalf("the relaunch's write: %v", err)
				}
				endRun(ctx, t, store, runs, "run-1", fence)
			},
			End:   func(t *testing.T) { endRun(ctx, t, store, runs, "run-1", fence) },
			Stall: store.hold,
		}
	})
}

// A NOTE AT ITS WORST FITS WHAT A RUN'S ROW KEEPS OF ONE. A person's note is
// sealed by this engine and filed on the run's row, which holds a note's seal to
// a bound of its own (sandbox.MaxNoteSealedBytes) so the row's arithmetic
// closes; the steer tool holds a note to its characters (steer.MaxNoteRunes),
// so the most a note can seal to — every character one the row's encoder
// escapes six-fold, the longest handle, the longest turn id — must fit that
// bound, or the store would refuse a note the tool accepted.
//
// Mutation: raise steer.MaxNoteRunes past what the row's bound holds, or seal
// more of a note than its words and who sent it, and this goes red.
func TestANoteAtItsWorstFitsWhatARunsRowKeepsOfOne(t *testing.T) {
	t.Parallel()
	runs, _ := steersOver(t, steerRing, nil)
	note, err := runs.seal(strings.Repeat("t", 64), steer.Note{
		ID: "0190f3a8-0000-7000-8000-000000000001", Text: strings.Repeat("<", steer.MaxNoteRunes),
		By: strings.Repeat("a", iam.MaxLogin), ByKind: iam.ActorKind(strings.Repeat("k", 32)),
		OperatorID: strings.Repeat("o", 64), At: time.Date(2026, 11, 3, 23, 59, 59, 999_999_999, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(note.Sealed) > sandbox.MaxNoteSealedBytes {
		t.Errorf("a note at its worst seals to %d bytes, past the %d a run's row keeps of one",
			len(note.Sealed), sandbox.MaxNoteSealedBytes)
	}
	t.Logf("a note at its worst seals to %d bytes of the %d", len(note.Sealed), sandbox.MaxNoteSealedBytes)
}

// fileItemFor seals an item for job as a note is sealed, and files it on
// turnID's record under the turn's bounds.
func fileItemFor(t *testing.T, runs *runSteers, store sandbox.PendingStore, turnID, id, job string) {
	t.Helper()
	n, err := runs.seal(turnID, steer.Note{ID: id, Text: "item for the job " + id, By: "founder",
		ByKind: iam.ActorHuman})
	if err != nil {
		t.Fatal(err)
	}
	n.For = job
	filed, _, err := store.FileNote(t.Context(), turnID, sandbox.NoteFiling{Note: n,
		MaxWaiting: steer.MaxPendingNotes, MaxNotes: steer.MaxTurnNotes})
	if err != nil || filed != sandbox.NoteAccepted {
		t.Fatalf("file item %s = %s, %v", id, filed, err)
	}
}

// EVERY READER OF A RUN'S WAITING NOTES NAMES ITS JOB. An item for one job
// (sandbox.RunNote.For) is handed to a reader inside that job alone, so every
// reader of the record's waiting notes names the job it reads inside: the run
// inbox the job it serves, at a batch off its view and at the finish off the
// store, each handing the job its own item; and the review and a relaunch's
// brief none, neither handed an item, nor the review's round ended for one.
// An item for a job the record no longer names reaches no reader.
//
// Mutation: drop the job a reader names, name another, or keep no item in the
// view a reader filters, and a check goes red.
func TestEveryReaderOfWaitingNotesNamesItsJob(t *testing.T) {
	t.Parallel()
	t.Run("each job its own", func(t *testing.T) {
		t.Parallel()
		runs, _ := steersOver(t, steerRing, sandbox.NewCoordStore(coordmem.NewFleet()))
		sealed := func(id, job string) sandbox.RunNote {
			t.Helper()
			n, err := runs.seal("run-1", steer.Note{ID: id, Text: "note " + id, By: "founder",
				ByKind: iam.ActorHuman})
			if err != nil {
				t.Fatal(err)
			}
			n.For = job
			return n
		}
		notes := []sandbox.RunNote{sealed("person", ""), sealed("mine", "job-1"), sealed("other", "job-0")}
		for job, want := range map[string][]string{"": {"person"}, "job-1": {"person", "mine"},
			"job-0": {"person", "other"}} {
			var got []string
			for _, o := range runs.waiting("run-1", notes, job) {
				got = append(got, o.held.ID)
			}
			if !slices.Equal(got, want) {
				t.Errorf("a reader inside job %q is handed %v, want %v", job, got, want)
			}
		}
	})
	t.Run("the run inbox hands its job its item", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		store := sandbox.NewCoordStore(coordmem.NewFleet())
		runs, _ := steersOver(t, steerRing, store)
		launch := workingRun(t, store, runs, "run-1")
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
		fileItemFor(t, runs, store, "run-1", "i-1", launch)
		inbox := boundInbox(t, runs, store, "run-1", launch)
		viewHolds(t, inbox, 2)
		if answer := inbox.Take(ctx, mcpbridge.SteerEventToolBatch); !strings.Contains(answer, "use staging") ||
			!strings.Contains(answer, "item for the job i-1") {
			t.Errorf("the batch's hook was answered %q, want the person's note and the job's own item", answer)
		}
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-2", Text: "and tag it", By: "founder"})
		fileItemFor(t, runs, store, "run-1", "i-2", launch)
		if answer := inbox.Take(ctx, mcpbridge.SteerEventFinish); !strings.Contains(answer, "and tag it") ||
			!strings.Contains(answer, "item for the job i-2") {
			t.Errorf("the finish's hook was answered %q, want the person's note and the job's own item", answer)
		}
	})
	t.Run("the poll passes another job's item over", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		fleet := coordmem.NewFleet()
		store := sandbox.NewCoordStore(fleet)
		runs, _ := steersOver(t, steerRing, store)
		launch := workingRun(t, store, runs, "run-1")
		// AN ITEM FOR A JOB THE RECORD NO LONGER NAMES, written onto the record
		// whole, as a party that reaches the store could: ahead of the
		// person's note, so a poll that stopped at it answered nothing.
		other, err := runs.seal("run-1", steer.Note{ID: "i-other", Text: "item for the job before", By: "founder",
			ByKind: iam.ActorHuman})
		if err != nil {
			t.Fatal(err)
		}
		other.For = "job-before"
		if _, ok, err := fleet.MutateSandboxRun(ctx, "run-1", func(current coord.Record) (coord.SandboxRunChange, error) {
			var row sandbox.PendingRun
			if err := json.Unmarshal(current.Value, &row); err != nil {
				return coord.SandboxRunChange{}, err
			}
			row.Steer.Seq++
			other.Seq = row.Steer.Seq
			row.Steer.Notes = append(row.Steer.Notes, other)
			raw, err := json.Marshal(row)
			return coord.SandboxRunChange{Put: raw}, err
		}); err != nil || !ok {
			t.Fatalf("write the other job's item: %v %v", ok, err)
		}
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
		inbox := boundInbox(t, runs, store, "run-1", launch)
		viewHolds(t, inbox, 2)
		if notes, _ := inbox.Wait(ctx, 0); len(notes) != 1 || notes[0].ID != "n-1" {
			t.Errorf("the job's poll was answered %+v, want the person's note alone", notes)
		}
	})
	t.Run("the review and a brief take no item", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		store := sandbox.NewCoordStore(coordmem.NewFleet())
		runs, _ := steersOver(t, steerRing, store)
		launch := workingRun(t, store, runs, "run-1")
		fileItemFor(t, runs, store, "run-1", "i-1", launch)
		source := claimedSource(t, store, runs, launch)
		eventually(t, "the segment's view to hold the item", func() bool {
			view, opened, _ := source.watch.snapshot()
			return view.Found && len(opened) == 1
		})
		if source.Waiting() {
			t.Error("the review reports a note waiting where only the job's item waits")
		}
		if brief := source.Brief(ctx); len(brief.Notes) != 0 {
			t.Errorf("a relaunch's brief carries %+v, want no job's item", brief.Notes)
		}
		if taken := source.Take(ctx, reviewRound(1)); len(taken) != 0 {
			t.Errorf("the review took %+v, want no job's item", taken)
		}
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
		eventually(t, "the review to see the person's note", source.Waiting)
		if taken := source.Take(ctx, reviewRound(2)); len(taken) != 1 || taken[0].ID != "n-1" {
			t.Errorf("the review took %+v, want the person's note alone", taken)
		}
	})
}

// A TURN CARRIES NO JOB'S ITEM. What a segment resuming the turn carries into
// every later phase is the notes the turn has read, a person's, and the marks
// a relaunch's brief tells again are a person's notes' too: an item for one
// job (sandbox.RunNote.For) was that job's alone, so it is never carried nor
// told again, read or marked, and the person's notes read around it keep the
// order they were read in.
//
// Mutation: carry a read item, or tell a marked one again, and the case goes
// red.
func TestATurnCarriesNoJobsItem(t *testing.T) {
	t.Parallel()
	runs, _ := steersOver(t, steerRing, sandbox.NewCoordStore(coordmem.NewFleet()))
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	sealed := func(seq int, id, job string, order int, unanswered bool) sandbox.RunNote {
		t.Helper()
		n, err := runs.seal("run-1", steer.Note{ID: id, Text: "note " + id, By: "founder", ByKind: iam.ActorHuman})
		if err != nil {
			t.Fatal(err)
		}
		n.Seq, n.For = seq, job
		n.Read = &sandbox.NoteRead{At: at, Order: order, Phase: "execute", Iteration: 1, Round: 1,
			Boundary: string(types.SteerBoundaryToolBatch), Launch: "job-1", Unanswered: unanswered}
		return n
	}
	run := sandbox.PendingRun{TurnID: "run-1", Steer: sandbox.RunSteer{Seq: 4, Reads: 4, Notes: []sandbox.RunNote{
		sealed(1, "use-postgres", "", 1, false),
		sealed(2, "late-call", "job-1", 2, false),
		sealed(3, "use-sqlite", "", 3, true),
		sealed(4, "marked-call", "job-1", 4, true),
	}}}
	var carried []string
	for _, r := range runs.carried(run) {
		carried = append(carried, r.Note.ID)
	}
	if want := []string{"use-postgres", "use-sqlite"}; !slices.Equal(carried, want) {
		t.Errorf("the turn carries %v, want the person's notes alone, in the order read: %v", carried, want)
	}
	if got := unansweredNotes(run); !slices.Equal(got, []string{"use-sqlite"}) {
		t.Errorf("a relaunch's brief tells %v again, want the person's marked note alone", got)
	}
}

// A NOTE WHOSE SERVE A CANCEL LOST IS TOLD AS ONE A SESSION MAY ALREADY SHOW. A
// continued session is told a note its harness was served and never confirmed
// as one it may already show. A hook's serve its harness cancelled clears the
// note's Served, but the hook's answer may have outrun the cancel, so the note
// is told so all the same, as it is under a loss this build does not know;
// only where the job's whole session proves its lost serve absent is it told
// plainly. And an item for one job is told as nothing here, served or not.
//
// Mutation: key the set on Served alone, count a serve proven absent, or list
// an item, and the case goes red.
func TestANoteWhoseServeACancelLostIsToldAsOneItMayShow(t *testing.T) {
	t.Parallel()
	serves := func(lost sandbox.ServeLoss) []sandbox.HookServed {
		return []sandbox.HookServed{{Take: "take-1", Launch: "job-1", Boundary: "tool_batch", Lost: lost}}
	}
	run := sandbox.PendingRun{Steer: sandbox.RunSteer{Notes: []sandbox.RunNote{
		{ID: "polled", Served: "job-1"},
		{ID: "hook-live", Served: "job-1", Place: 1, Serves: serves("")},
		{ID: "hook-cancelled", Place: 2, Serves: serves(sandbox.ServeCancelled)},
		{ID: "hook-absent", Place: 3, Serves: serves(sandbox.ServeAbsent)},
		{ID: "hook-later-loss", Place: 4, Serves: serves("dropped")},
		{ID: "unserved"},
		{ID: "item", For: "job-1", Served: "job-1", Place: 5, Serves: serves("")},
		{ID: "item-cancelled", For: "job-1", Place: 6, Serves: serves(sandbox.ServeCancelled)},
	}}}
	if got, want := servedNotes(run), []string{"polled", "hook-live", "hook-cancelled", "hook-later-loss"}; !slices.Equal(
		got, want) {
		t.Errorf("a continued session is told %v as notes it may already show, want %v", got, want)
	}
}
