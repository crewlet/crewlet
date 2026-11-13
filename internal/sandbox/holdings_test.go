package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// openingOf is the digest a reading records of a result's text
// ([CallRecords.Opening]).
func openingOf(text string) [32]byte {
	return sha256.Sum256([]byte(text[:min(HoldingsOpeningBytes, len(text))]))
}

// recordsOf is what a reading counts of a call whose records lie in the main
// thread: one use, and one live result holding text, failed or not.
func recordsOf(text string, failed bool) CallRecords {
	records := CallRecords{Threads: 1, Uses: 1, Results: 1, Live: true, Failed: failed, TextBytes: len(text),
		Opening: openingOf(text)}
	if len(text) <= MaxBridgeOutputBytes {
		records.Whole = sha256.Sum256([]byte(text))
	}
	return records
}

// readingOf is a reading by id that counted records of the calls named.
func readingOf(records map[string]CallRecords) SessionHoldings {
	return SessionHoldings{Read: true, Kind: HoldingsByID, Calls: records}
}

// A CALL IS HELD ONLY THROUGH ITS LOGGED RESULT. The session holds a call's
// logged result where the reading counted records naming it in one thread
// alone, one use and one result there, the result live, its failure the log's
// and its text the log's, every byte of an output the log kept whole and the
// size and opening of one it cut; then it is held, an output the log cut as
// much as one it kept whole. Anything else is told, in the main list: a call
// no record names, one whose id the log never recorded, records in two
// threads, a use or a result missing or repeated, a result before a restart, a
// failure the log and the session do not share, either way, a text of another
// length, a whole output's text otherwise anywhere, past its opening among it,
// a cut output's text that opens otherwise, content that is no text, a log
// whose kept opening is shorter than the digest or whose size is no size; and
// every call of no reading, or of a reading of a kind this build does not
// read, which is by_id alone.
//
// Mutation: hold a call whose failure the session and the log do not share,
// measure a cut output by what the log kept of it rather than by its whole
// size, or compare a whole output by its opening alone, and a case goes red;
// drop any other condition and its case goes red.
func TestACallIsHeldOnlyThroughItsLoggedResult(t *testing.T) {
	t.Parallel()
	const id = "toolu_01H8xQ"
	output := "the issue's title, as the tool answered it"
	// A CUT OUTPUT, as the log keeps one past its share: its opening and a
	// marker, beside the whole output's size.
	long := strings.Repeat("a line of the tool's long answer\n", 400)
	cut := keptCall(BridgeCall{Output: long}, MaxBridgeArgsBytes)
	if cut.OutputBytes != len(long) || len(cut.Output) >= len(long) {
		t.Fatalf("the premise: the log keeps %d bytes of the output and says it was %d, want it cut from %d",
			len(cut.Output), cut.OutputBytes, len(long))
	}
	logged := LoggedCall{ID: id, Asked: true, Output: output}
	failed := LoggedCall{ID: id, Asked: true, Failed: true, Output: output}
	// AN OPENING AS LONG AS ITS DIGEST, which an output the log keeps whole
	// and a result four bytes longer share.
	opened := strings.Repeat("o", HoldingsOpeningBytes)
	edited := func(edit func(*CallRecords)) SessionHoldings {
		records := recordsOf(output, false)
		edit(&records)
		return readingOf(map[string]CallRecords{id: records})
	}
	for name, tc := range map[string]struct {
		reading SessionHoldings
		call    LoggedCall
		held    bool
	}{
		"its one live result, the log's": {reading: edited(func(*CallRecords) {}), call: logged, held: true},
		"a failed call the session says failed": {reading: readingOf(map[string]CallRecords{
			id: recordsOf(output, true)}), call: failed, held: true},
		"a cut output, the session's text as long and opening alike": {reading: readingOf(map[string]CallRecords{
			id: recordsOf(long, false)}), call: LoggedCall{ID: id, Asked: true, Output: cut.Output,
			OutputBytes: cut.OutputBytes}, held: true},
		"an empty output, its result empty": {reading: readingOf(map[string]CallRecords{id: recordsOf("", false)}),
			call: LoggedCall{ID: id, Asked: true}, held: true},

		"no record names it": {reading: readingOf(map[string]CallRecords{"toolu_other": recordsOf(output, false)}),
			call: logged},
		"the log recorded no id": {reading: readingOf(map[string]CallRecords{"": recordsOf(output, false)}),
			call: LoggedCall{Asked: true, Output: output}},
		"records in two threads":      {reading: edited(func(r *CallRecords) { r.Threads = 2 }), call: logged},
		"two uses":                    {reading: edited(func(r *CallRecords) { r.Uses = 2 }), call: logged},
		"no use":                      {reading: edited(func(r *CallRecords) { r.Uses = 0 }), call: logged},
		"two results":                 {reading: edited(func(r *CallRecords) { r.Results = 2 }), call: logged},
		"no result":                   {reading: edited(func(r *CallRecords) { r.Results = 0 }), call: logged},
		"its result before a restart": {reading: edited(func(r *CallRecords) { r.Live = false }), call: logged},
		"failed in the session alone": {reading: edited(func(r *CallRecords) { r.Failed = true }), call: logged},
		"failed in the log alone":     {reading: edited(func(*CallRecords) {}), call: failed},
		"a text of another length":    {reading: edited(func(r *CallRecords) { r.TextBytes++ }), call: logged},
		"a whole output's text otherwise": {reading: edited(func(r *CallRecords) {
			r.Whole = sha256.Sum256([]byte(strings.Repeat("x", len(output))))
		}), call: logged},
		"a whole output's text alike to its opening's end, and otherwise past it": {
			reading: readingOf(map[string]CallRecords{id: recordsOf(opened+"TAIL", false)}),
			call:    LoggedCall{ID: id, Asked: true, Output: opened + "tail"}},
		"a cut output's text that opens otherwise": {reading: readingOf(map[string]CallRecords{id: func() CallRecords {
			records := recordsOf(long, false)
			records.Opening = openingOf(strings.Repeat("x", len(long)))
			return records
		}()}), call: LoggedCall{ID: id, Asked: true, Output: cut.Output, OutputBytes: cut.OutputBytes}},
		"content that is no text": {reading: edited(func(r *CallRecords) { r.TextBytes = -1 }), call: logged},
		"a kept opening shorter than the digest": {reading: readingOf(map[string]CallRecords{
			id: recordsOf(long, false)}), call: LoggedCall{ID: id, Asked: true, Output: long[:100],
			OutputBytes: len(long)}},
		"a logged size that is no size": {reading: edited(func(r *CallRecords) { r.TextBytes = -1 }),
			call: LoggedCall{ID: id, Asked: true, Output: output, OutputBytes: -1}},
		"no reading": {reading: SessionHoldings{Kind: HoldingsByID,
			Calls: map[string]CallRecords{id: recordsOf(output, false)}}, call: logged},
		"a reading of no kind": {reading: SessionHoldings{Read: true,
			Calls: map[string]CallRecords{id: recordsOf(output, false)}}, call: logged},
		"a reading of a kind this build does not read": {reading: SessionHoldings{Read: true, Kind: "by_content",
			Calls: map[string]CallRecords{id: recordsOf(output, false)}}, call: logged},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			held, under := tc.reading.Holds(tc.call)
			if held != tc.held || under != "" {
				t.Fatalf("Holds = %v, under %q; want held %v, in the main list", held, under, tc.held)
			}
		})
	}
	// THE KINDS THIS BUILD READS: by_id, and no other.
	if !HoldingsByID.Valid() || len(HoldingsKinds) != 1 {
		t.Errorf("this build reads %d kinds, by_id among them %v; want by_id alone", len(HoldingsKinds),
			HoldingsByID.Valid())
	}
	for _, k := range []HoldingsKind{"", "by_content", "BY_ID"} {
		if k.Valid() {
			t.Errorf("%q reads as a kind this build reads", k)
		}
	}
}

// A TOLD CALL IS TOLD UNDER A SUBAGENT ONLY WHERE THAT SUBAGENT IS UNREPORTED.
// A call whose records lie in a subagent's thread is held as any other where
// the reading reports that subagent. Where the reading reports it but the
// call's result is not the log's, the call is told in the main list, as every
// call the session does not hold. Where the reading does not report it, or
// says nothing of it, the call is told under the subagent's heading whatever
// its records say, so the relaunch can say whose work it was.
//
// Mutation: tell a reported subagent's mismatched call under its heading, or
// hold or tell in the main list an unreported subagent's call, and a case goes
// red.
func TestAToldCallIsToldUnderASubagentOnlyWhereThatSubagentIsUnreported(t *testing.T) {
	t.Parallel()
	const id, agent = "toolu_01Sub", "a0123456789abcdef"
	output := "what the subagent's call returned"
	inThread := recordsOf(output, false)
	inThread.Thread = agent
	failedThere := inThread
	failedThere.Failed = true
	reported := map[string]SubagentThread{agent: {Reported: true, Description: "reads the failing test"}}
	unreported := map[string]SubagentThread{agent: {Why: "a leg with no live answer"}}
	call := LoggedCall{ID: id, Asked: true, Output: output}
	for name, tc := range map[string]struct {
		records   CallRecords
		subagents map[string]SubagentThread
		held      bool
		under     string
	}{
		"a reported subagent's call, the log's":        {records: inThread, subagents: reported, held: true},
		"a reported subagent's call, not the log's":    {records: failedThere, subagents: reported},
		"an unreported subagent's call, the log's":     {records: inThread, subagents: unreported, under: agent},
		"an unreported subagent's call, not the log's": {records: failedThere, subagents: unreported, under: agent},
		"a subagent the reading says nothing of":       {records: inThread, under: agent},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reading := readingOf(map[string]CallRecords{id: tc.records})
			reading.Subagents = tc.subagents
			held, under := reading.Holds(call)
			if held != tc.held || under != tc.under {
				t.Fatalf("Holds = %v, under %q; want %v, under %q", held, under, tc.held, tc.under)
			}
		})
	}
}

// A LOGGED OUTPUT KEEPS THE OPENING A HOLDING COMPARES. Past its share the log
// keeps an output's opening and a marker, and a holding compares the first
// HoldingsOpeningBytes of what the log kept with the session's text: so those
// bytes must be the output's own, however long the output is and however it
// escapes. An output whose every byte the log's encoding escapes to six (a
// control byte, `<`, `>`, `&`, a byte that is no UTF-8) keeps the least of
// its opening, and still keeps more than that.
//
// Mutation: raise HoldingsOpeningBytes past what the log keeps of such an
// output (672 bytes), or keep less of a cut output's opening, and this goes
// red.
func TestALoggedOutputKeepsTheOpeningAHoldingCompares(t *testing.T) {
	t.Parallel()
	const sixEach = "<>&\x00\x01\x1f\xff"
	for _, size := range []int{MaxBridgeOutputBytes + 1, 64 << 10, 1 << 20} {
		output := strings.Repeat(sixEach, size/len(sixEach)+1)[:size]
		kept := keptCall(BridgeCall{Output: output}, MaxBridgeArgsBytes)
		if kept.OutputBytes != size {
			t.Fatalf("an output of %d bytes is kept as one of %d", size, kept.OutputBytes)
		}
		opening := 0
		for opening < len(kept.Output) && kept.Output[opening] == output[opening] {
			opening++
		}
		if opening < HoldingsOpeningBytes {
			t.Errorf("an output of %d bytes, each escaping to six, keeps %d bytes of its opening, fewer than the "+
				"%d a holding compares", size, opening, HoldingsOpeningBytes)
		}
	}
}

// holdingsSpy is a runner that reads what a session holds as a case plays it:
// whether it restores an archive, every archive it was asked to read, and
// what the opener it was handed streamed of each.
type holdingsSpy struct {
	// refuses is why it does not restore an archive, "" where it does.
	refuses string

	mu       sync.Mutex
	asked    []SessionArchive
	streamed []string
}

var _ HoldingsReader = (*holdingsSpy)(nil)

func (s *holdingsSpy) Restores(SessionArchive) (bool, string) { return s.refuses == "", s.refuses }

// ReadHoldings reads what open streams to its end, and answers a reading that
// holds the logged result of every call asked.
func (s *holdingsSpy) ReadHoldings(ctx context.Context, archive SessionArchive,
	open func(context.Context) (io.ReadCloser, error), ask HoldingsAsk) (SessionHoldings, error) {

	s.mu.Lock()
	s.asked = append(s.asked, archive)
	s.mu.Unlock()
	body, err := open(ctx)
	if err != nil {
		return SessionHoldings{}, err
	}
	defer func() { _ = body.Close() }()
	read, err := io.ReadAll(body)
	if err != nil {
		return SessionHoldings{}, err
	}
	s.mu.Lock()
	s.streamed = append(s.streamed, string(read))
	s.mu.Unlock()
	reading := readingOf(map[string]CallRecords{})
	for _, c := range ask.Calls {
		if c.Asked {
			reading.Calls[c.ID] = recordsOf(c.Output, c.Failed)
		}
	}
	return reading, nil
}

// reads is every archive the spy was asked to read, and what its opener
// streamed of each.
func (s *holdingsSpy) reads() ([]SessionArchive, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked), slices.Clone(s.streamed)
}

// keepsNone is the keep of a company that keeps no session, of which a case
// asks only whether it keeps.
type keepsNone struct{ SessionKeep }

func (keepsNone) Keeps() bool { return false }

// keptArchive is what the object of the capture a [holdingsWorld] keeps
// holds.
const keptArchive = "the session's archive, as its capture packed it"

// holdingsWorld is a relaunch's world as it asks what the session it continues
// holds: turn t1's job kept a capture of its session, its save point stamped
// and its harness cut off, which the turn's row on the executor log names and
// whose object holds [keptArchive], beside another turn's object; and a runner
// that reads it.
type holdingsWorld struct {
	rig     *coordRig
	restore *keptRestore
	kept    SessionCapture
	reader  *holdingsSpy
}

// newHoldingsWorld is a [holdingsWorld] whose job's harness ended its own run
// where ownRun says so, rather than being cut off.
func newHoldingsWorld(t *testing.T, ownRun bool) *holdingsWorld {
	t.Helper()
	rig := newCoordRig(t)
	// ITS SPOOLS ITS OWN, so a spool a reading left is the case's to see.
	rig.coordinator.spoolDir = t.TempDir()
	run := rig.launch("t1")
	key := objstore.KeyAt(rig.now)
	kept := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID, State: CaptureLanded,
		Final: true, Interrupted: !ownRun, Through: new(2), At: rig.now, Harness: "claude-code",
		HarnessVersion: "2.1.310", SessionID: "s-1", ArchiveFormat: FakeArchiveFormat, Object: key.String(),
		ObjectHash: strings.Repeat("a", 64), ObjectSize: 1024, ContentKey: "k1"}
	if took, err := rig.pending.StampCapture(t.Context(), "t1", kept); err != nil || !took {
		t.Fatalf("stamping the job's capture = %v, %v", took, err)
	}
	if got := rig.get("t1"); got.Kept == nil || !got.Kept.samePlace(kept) {
		t.Fatalf("the premise: the record keeps %+v, want the job's capture", got.Kept)
	}
	restore := &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": keptRow(t, kept)},
		archives: map[string][]byte{key.String(): []byte(keptArchive),
			objstore.KeyAt(rig.now).String(): []byte("another turn's archive")}}
	rig.coordinator.restore = restore
	return &holdingsWorld{rig: rig, restore: restore, kept: kept, reader: &holdingsSpy{}}
}

// row edits the turn's row on the executor log as edit says.
func (w *holdingsWorld) row(edit func(*executorkeep.TurnRow)) {
	w.restore.mu.Lock()
	defer w.restore.mu.Unlock()
	row := w.restore.rows["t1"]
	edit(&row)
	w.restore.rows["t1"] = row
}

// holdingsQuestion is what a relaunch hands [Coordinator.HoldingsOf].
type holdingsQuestion struct {
	turn            string
	launch, capture int64
	ask             HoldingsAsk
	reader          HoldingsReader
}

// askingAbout is an ask of the calls with the ids given, each listed with an
// outcome as the log keeps one, and each asked about.
func askingAbout(ids ...string) HoldingsAsk {
	ask := HoldingsAsk{Calls: make([]LoggedCall, 0, len(ids))}
	for _, id := range ids {
		ask.Calls = append(ask.Calls, LoggedCall{ID: id, Asked: true, Output: "what " + id + " returned"})
	}
	return ask
}

// HOLDINGS ARE READ ONLY OF THE CAPTURE ITS RELAUNCH CONTINUES. A reading is
// made of the capture the turn's run record keeps, cut off, where the one
// decision that continues a kept session (ContinuityOf) still continues the
// turn's row with the runner that reads it: that runner is handed the archive
// the row names and an opener streaming exactly the row's object, its reading
// is the answer, and no spool is left. Anything else is no reading, said, the
// runner never asked and nothing streamed: a node that keeps no session, or a
// company that keeps none; no runner that reads; no run record; a record that
// keeps another capture, or one whose harness ended its own run; a row naming
// another capture, or none; a turn ended, or whose end is decided; a session
// that ran out of context; a newer capture of its job begun; a runner that
// does not restore the archive; and an ask of no call, or of more than a
// round's log lists.
//
// Mutation: skip ContinuityOf, read a capture the record does not keep or
// whose harness ended its own run, read with nothing to restore from, or
// spool the object rather than stream it, and a case goes red.
func TestHoldingsAreReadOnlyOfTheCaptureItsRelaunchContinues(t *testing.T) {
	t.Parallel()
	tooMany := make([]string, MaxBridgeCalls+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("toolu_%03d", i)
	}
	for name, tc := range map[string]struct {
		ownRun bool
		world  func(t *testing.T, w *holdingsWorld, q *holdingsQuestion)
		// why is what the answer says, "" where the session is read.
		why string
	}{
		"the capture its relaunch continues": {},

		"a node that keeps no session": {why: "built with nothing to restore one from",
			world: func(_ *testing.T, w *holdingsWorld, _ *holdingsQuestion) { w.rig.coordinator.restore = nil }},
		"a company that keeps none": {why: NoKeptSession,
			world: func(_ *testing.T, w *holdingsWorld, _ *holdingsQuestion) { w.rig.coordinator.keep = keepsNone{} }},
		"no runner that reads": {why: "no runner of this build reads",
			world: func(_ *testing.T, _ *holdingsWorld, q *holdingsQuestion) { q.reader = nil }},
		"a turn with no run record": {why: "the turn has no run record",
			world: func(_ *testing.T, _ *holdingsWorld, q *holdingsQuestion) { q.turn = "t9" }},
		"a record keeping another capture": {why: "does not keep the capture the relaunch continues",
			world: func(_ *testing.T, _ *holdingsWorld, q *holdingsQuestion) { q.capture++ }},
		"a record keeping another launch's capture": {why: "does not keep the capture the relaunch continues",
			world: func(_ *testing.T, _ *holdingsWorld, q *holdingsQuestion) { q.launch++ }},
		"a capture whose harness ended its own run": {ownRun: true, why: "ended its own run"},

		"a row naming another capture": {why: "the executor log names the turn's capture",
			world: func(_ *testing.T, w *holdingsWorld, _ *holdingsQuestion) {
				w.row(func(row *executorkeep.TurnRow) { row.CaptureSeq++ })
			}},
		"a row naming no session": {why: "names no kept session",
			world: func(_ *testing.T, w *holdingsWorld, _ *holdingsQuestion) {
				w.row(func(row *executorkeep.TurnRow) { row.Object = objstore.Object{} })
			}},
		"an ended turn": {why: "the turn has ended",
			world: func(_ *testing.T, w *holdingsWorld, _ *holdingsQuestion) {
				w.row(func(row *executorkeep.TurnRow) { row.Ended = true })
			}},
		"a turn whose end is decided": {why: "the turn is ending",
			world: func(t *testing.T, w *holdingsWorld, _ *holdingsQuestion) {
				if owed, err := w.rig.coordinator.OweEnd(t.Context(), "t1",
					executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}); err != nil || !owed {
					t.Fatalf("OweEnd = %v, %v", owed, err)
				}
			}},
		"a session that ran out of context": {why: ranOutWords,
			world: func(t *testing.T, w *holdingsWorld, _ *holdingsQuestion) {
				if err := w.rig.pending.RetireSessions(t.Context(), "t1", []string{"s-1"}); err != nil {
					t.Fatalf("RetireSessions: %v", err)
				}
			}},
		"a newer capture of its job begun": {why: "was not kept",
			world: func(t *testing.T, w *holdingsWorld, _ *holdingsQuestion) {
				begun := w.kept
				begun.CaptureSeq, begun.State, begun.At = w.kept.CaptureSeq+1, CaptureBegun, w.rig.now
				if took, err := w.rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
					t.Fatalf("stamping the newer capture = %v, %v", took, err)
				}
			}},
		"a runner that does not restore it": {why: "older than 2.1.300",
			world: func(_ *testing.T, w *holdingsWorld, _ *holdingsQuestion) { w.reader.refuses = "older than 2.1.300" }},

		"an ask of no call": {why: "names 0 calls",
			world: func(_ *testing.T, _ *holdingsWorld, q *holdingsQuestion) { q.ask = HoldingsAsk{} }},
		"an ask of more calls than a round's log lists": {why: fmt.Sprintf("names %d calls", MaxBridgeCalls+1),
			world: func(_ *testing.T, _ *holdingsWorld, q *holdingsQuestion) { q.ask = askingAbout(tooMany...) }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newHoldingsWorld(t, tc.ownRun)
			q := &holdingsQuestion{turn: "t1", launch: w.kept.LaunchSeq, capture: w.kept.CaptureSeq,
				ask: askingAbout("toolu_01a", "toolu_01b"), reader: w.reader}
			if tc.world != nil {
				tc.world(t, w, q)
			}
			got, err := w.rig.coordinator.HoldingsOf(t.Context(), q.turn, q.launch, q.capture, q.ask, q.reader)
			asked, streamed := w.reader.reads()
			if tc.why != "" {
				if got.Read || got.Calls != nil || err == nil || !strings.Contains(err.Error(), tc.why) {
					t.Fatalf("HoldingsOf = %+v, %v; want no reading, saying %q", got, err, tc.why)
				}
				if len(asked) != 0 || w.restore.streams() != 0 {
					t.Fatalf("a session not to be read had its runner asked %d times and %d objects streamed",
						len(asked), w.restore.streams())
				}
				return
			}
			if err != nil || !got.Read || got.Kind != HoldingsByID || len(got.Calls) != 2 {
				t.Fatalf("HoldingsOf = %+v, %v; want the runner's reading of the two calls asked", got, err)
			}
			if want := ArchiveOf(keptRow(t, w.kept)); len(asked) != 1 || asked[0] != want {
				t.Fatalf("the runner was asked to read %+v, want once the archive the turn's row names, %+v",
					asked, want)
			}
			if len(streamed) != 1 || streamed[0] != keptArchive || w.restore.streams() != 1 {
				t.Fatalf("the runner's opener streamed %q (%d objects), want the row's own object once, %q",
					streamed, w.restore.streams(), keptArchive)
			}
			if left := restoreSpools(t, w.rig); len(left) != 0 {
				t.Errorf("the reading left a spool of the session behind: %v", left)
			}
		})
	}
}

// THE HOLDINGS WAIT BEGINS BEFORE THE RECORD IS READ. A reading's one wait
// bounds all of it, the run's record included, on a frame that holds the seat
// and a slot of the node's admission: a record that does not answer is waited
// for until the wait's own deadline, armed as the reading began, and no
// further; then there is no reading, and the runner is never asked.
//
// Mutation: take the wait after the record is read, or arm it at any other
// length, and the record is read with no deadline, or another one.
func TestTheHoldingsWaitBeginsBeforeTheRecordIsRead(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		w := newHoldingsWorld(t, false)
		stalled := &stalledRecord{PendingStore: w.rig.pending}
		w.rig.coordinator.pending = stalled
		armed := time.Now()
		got, err := w.rig.coordinator.HoldingsOf(t.Context(), "t1", w.kept.LaunchSeq, w.kept.CaptureSeq,
			askingAbout("toolu_01a"), w.reader)
		if got.Read || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("HoldingsOf = %+v, %v; want no reading, its wait passed", got, err)
		}
		if deadline, set := stalled.deadline(); !set || !deadline.Equal(armed.Add(holdingsReadWait)) {
			t.Errorf("the record was read under deadline %v (set: %t), want the %s wait armed as the reading "+
				"began, %v", deadline, set, holdingsReadWait, armed.Add(holdingsReadWait))
		}
		if asked, _ := w.reader.reads(); len(asked) != 0 || w.restore.streams() != 0 {
			t.Errorf("a reading past its wait asked its runner %d times and streamed %d objects", len(asked),
				w.restore.streams())
		}
	})
}

// stalledRecord is a run store whose record of a turn does not answer: a read
// of it notes the deadline it was asked under, and waits that deadline out.
type stalledRecord struct {
	PendingStore

	mu  sync.Mutex
	saw time.Time
	set bool
}

func (s *stalledRecord) Get(ctx context.Context, _ string) (PendingRun, bool, error) {
	deadline, set := ctx.Deadline()
	s.mu.Lock()
	s.saw, s.set = deadline, set
	s.mu.Unlock()
	if !set {
		return PendingRun{}, false, errors.New("the record was read with no wait to bound it")
	}
	<-ctx.Done()
	return PendingRun{}, false, ctx.Err()
}

// deadline is the deadline the record was read under, and whether it had one.
func (s *stalledRecord) deadline() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saw, s.set
}
