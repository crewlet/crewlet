package sandbox

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/secrets"
)

// worstAt is an instant at the widest the row's encoder writes one: nine
// digits of fraction and a zone that is not UTC.
var worstAt = time.Date(2026, 11, 3, 23, 59, 59, 999_999_999, time.FixedZone("x", 5*3600+1800))

// worstCall is one bridged call with every field at its widest, the payloads
// far past what the row keeps and in the bytes that escape the most: a name
// past its bound; arguments holding a body of markup beside the identifiers
// that say where it went, and forty notes of markup a few hundred bytes each,
// so what is kept once the body is left out is as close to its share as
// whole notes come; an output of markup; every kind of change a call can
// commit to its turn, each once ([everyKind]); and the id its harness gave it
// at its bound ([worstCallID]). NOT HELD AND NOT APPROVED: a call whose
// arguments are withheld keeps none of them ([BridgeCall.Withheld]), so the
// flags cost a call twenty-seven bytes and the arguments it then does not keep
// eight thousand.
func worstCall(t *testing.T, name string) BridgeCall {
	t.Helper()
	notes := map[string]any{}
	for i := range 40 {
		notes[fmt.Sprintf("n%02d", i)] = strings.Repeat("<", 300)
	}
	args, err := json.Marshal(map[string]any{
		"repo": "acme/api", "branch": "crewlet/eng/fix", "notes": notes,
		"files": []any{map[string]any{"path": "internal/a.go", "content": strings.Repeat("<", 64<<10)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := strings.Repeat("s", 32)
	return BridgeCall{
		Name: name, Args: string(args), Output: strings.Repeat("<", 64<<10),
		Failed: true, At: worstAt, Seq: math.MaxInt, Appended: math.MaxInt, Late: true, Abandoned: true,
		StartedAt: worstAt, DurationMS: math.MaxInt64, Origin: "mcp:" + server, Server: server,
		Source: "mcp:" + server, Refusal: string(mcp.RefusalReassignmentBudget), Digest: strings.Repeat("f", 64),
		HeldID: uuid.NewString(), Committed: everyKind(), CallID: worstCallID,
	}
}

// worstCallID is the id a call's harness gave it at the widest the log keeps
// one ([MaxBridgeCallIDBytes]): every character it may hold is written as
// itself, so any id at the bound is the widest.
var worstCallID = strings.Repeat("Az09-_", MaxBridgeCallIDBytes/6) + strings.Repeat("x", MaxBridgeCallIDBytes%6)

// everyKind is every kind of change a call can commit to its turn, each once,
// as the log spells them ([BridgeCall.Committed]): the closed set
// (types.CommittedChanges), read off it, so a kind added there is measured
// here. The most one call carries, since the bridge reads a call's kinds off
// its turn's set of changes, which holds each once (internal/agent/turnctx's
// Written.Since), and the most the log's union of them holds
// ([BridgeLog.Committed]).
func everyKind() []string {
	kinds := make([]string, 0, len(types.CommittedChanges))
	for _, c := range types.CommittedChanges {
		kinds = append(kinds, string(c))
	}
	return kinds
}

// A BRIDGED CALL IS HELD TO ITS SHARE OF THE LOG, AT ITS WORST. A call kept by
// the log, every field at its widest as the encoder writes it (markup name past
// its bound, megabyte arguments and output, both marks, every kind of change,
// id at its bound), weighs no more than [MaxBridgeCallBytes] less the room left
// for a later build; the newest submission no more than [MaxSubmissionCallBytes]
// less the same. The parts stay within rowbytes.go's figures, which close at the
// share. An id past its bound is kept as none.
// Mutation: keep the arguments or output whole, or measure a cut unescaped, and
// the call overruns; leave the name uncut, and the name does; drop the kinds or
// the id, and the case is no longer at its worst; keep an id whatever it is
// (log-keeps-any-id), and one past its bound is kept.
func TestABridgedCallIsHeldToItsShare(t *testing.T) {
	t.Parallel()
	const (
		others  = 800  // what rowbytes.go counts for a call's every other field
		reserve = 1758 // what rowbytes.go leaves a later build in one call
	)
	if MaxBridgeCallNameBytes*6+2+MaxBridgeArgsBytes+MaxBridgeOutputBytes+others+reserve != MaxBridgeCallBytes {
		t.Errorf("a call's name, arguments, output and other fields, with the room left for a later build, "+
			"do not close at its share %d", MaxBridgeCallBytes)
	}
	for name, tc := range map[string]struct {
		budget, share, args int
	}{
		"an ordinary call":      {MaxBridgeArgsBytes, MaxBridgeCallBytes, MaxBridgeArgsBytes},
		"the newest submission": {MaxSubmissionArgsBytes, MaxSubmissionCallBytes, MaxSubmissionArgsBytes},
	} {
		call := keptCall(worstCall(t, strings.Repeat("<", 4<<10)), tc.budget)
		raw, err := json.Marshal(call)
		if err != nil {
			t.Fatal(err)
		}
		nameBytes, argsBytes, outputBytes := kept.Quoted(call.Name), kept.Quoted(call.Args), kept.Quoted(call.Output)
		fixed := len(raw) - nameBytes - argsBytes - outputBytes
		switch {
		case nameBytes > 256*6+2:
			t.Errorf("%s: its name weighs %d, past the 1,538 its bound allows", name, nameBytes)
		case argsBytes > tc.args:
			t.Errorf("%s: its arguments weigh %d, past their %d", name, argsBytes, tc.args)
		case outputBytes > MaxBridgeOutputBytes:
			t.Errorf("%s: its output weighs %d, past its %d", name, outputBytes, MaxBridgeOutputBytes)
		case fixed > others:
			t.Errorf("%s: its other fields weigh %d, past the %d the per-call bounds count", name, fixed, others)
		case len(raw) > tc.share-reserve:
			t.Errorf("%s: weighs %d, past its share %d less the %d left for a later build",
				name, len(raw), tc.share, reserve)
		}
		t.Logf("%s weighs %d: name %d, arguments %d, output %d, the rest %d", name, len(raw),
			nameBytes, argsBytes, outputBytes, fixed)
		// AT ITS WORST, or the bound above held nothing: the arguments are
		// kept to within a note's weight of their share.
		if argsBytes < tc.args-2000 {
			t.Errorf("%s: its arguments are kept at %d, far under their %d: the case is not at its worst",
				name, argsBytes, tc.args)
		}
		if call.ArgsBytes == 0 || call.OutputBytes != 64<<10 {
			t.Errorf("%s: states a whole of %d arguments and %d output, want both", name,
				call.ArgsBytes, call.OutputBytes)
		}
		// AND EVERY KIND IT COMMITTED KEPT, or the other fields were measured
		// short of them, and so its id at its bound.
		if !slices.Equal(call.Committed, everyKind()) {
			t.Errorf("%s: is kept with %d of the %d kinds of change it committed: the case is not at its worst",
				name, len(call.Committed), len(types.CommittedChanges))
		}
		if call.CallID != worstCallID {
			t.Errorf("%s: is kept with the id %q, want its harness's whole: the case is not at its worst",
				name, call.CallID)
		}
	}
	handed := worstCall(t, strings.Repeat("<", 4<<10))
	handed.CallID = strings.Repeat("<", 64<<10)
	if kept := keptCall(handed, MaxBridgeArgsBytes); kept.CallID != "" {
		t.Errorf("a call handed an id of %d bytes is kept with one of %d, want none: an id past its bound is "+
			"kept as none, never cut", len(handed.CallID), len(kept.CallID))
	}
}

// worstItemCall is a late-call item's call at its widest ([itemCall]): kept
// from a call with every field an item copies set, at the widest the log's
// encoder writes it, failed so its error is copied, and neither held nor
// approved, whose arguments an item would keep none of.
func worstItemCall(t *testing.T) BridgeCall {
	t.Helper()
	call := worstCall(t, strings.Repeat("<", 4<<10))
	call.ArgsUnparsed, call.ArgsBytes, call.OutputBytes = true, math.MaxInt, math.MaxInt
	return itemCall(call)
}

// AN ITEM'S CALL IS HELD TO AN ORDINARY CALL'S SHARE, whatever share the log kept
// it at ([itemCall]): the log keeps its newest successful submission at eight
// times an ordinary call's arguments ([MaxSubmissionArgsBytes]), and an
// abandoned one of the job's own is that submission. An item keeps every call
// within [MaxBridgeCallBytes] less a later build's room, its arguments within
// [MaxBridgeArgsBytes], a failed call's error and no success's output, and
// none of what only the log reads: a digest, an id, who answered, what it
// committed.
//
// Mutation: copy the call as the log listed it (item-copies-the-listed-call),
// and the submission's arguments and its success's output ride the item.
func TestAnItemsCallIsHeldToItsShare(t *testing.T) {
	t.Parallel()
	const reserve = 1758 // what rowbytes.go leaves a later build in one call
	for name, tc := range map[string]struct {
		call   BridgeCall
		listed int
	}{
		"a failed call at its widest": {worstCall(t, strings.Repeat("<", 4<<10)), MaxBridgeArgsBytes},
		"a successful submission the log kept whole": {func() BridgeCall {
			c := worstCall(t, SubmitWorkTool)
			c.Failed, c.Late = false, false
			return keptCall(c, MaxSubmissionArgsBytes)
		}(), MaxSubmissionArgsBytes},
	} {
		if got := kept.Quoted(tc.call.Args); got < tc.listed-2000 {
			t.Fatalf("%s: the log keeps %d bytes of its arguments, far under the %d the case needs", name, got,
				tc.listed)
		}
		item := itemCall(tc.call)
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		if args := kept.Quoted(item.Args); args > MaxBridgeArgsBytes || len(raw) > MaxBridgeCallBytes-reserve {
			t.Errorf("%s: the item keeps %d bytes of arguments and weighs %d, past an ordinary call's %d and "+
				"%d less a later build's %d", name, args, len(raw), MaxBridgeArgsBytes, MaxBridgeCallBytes, reserve)
		}
		if tc.call.Failed != (item.Output != "") || item.Digest != "" || item.CallID != "" || item.Origin != "" ||
			len(item.Committed) != 0 {
			t.Errorf("%s: the item keeps output %d bytes (failed %v), digest %q, id %q, origin %q and %d kinds; "+
				"want a failure's error alone, and none of what only the log reads", name, len(item.Output),
				tc.call.Failed, item.Digest, item.CallID, item.Origin, len(item.Committed))
		}
	}
}

// everyField is [worstCall] with every other field a call can carry set
// besides: both its marks, both its sizes, the mark of arguments that did not
// parse, a runner past the most an
// incarnation may be in markup and a bound at the widest instant. What the
// models hand a writer that keeps a call (an entry in flight, [keptEntry]; the
// call a person approved, [approvedCall]), so a field the call it keeps leaves
// zero is one that writer leaves out, never one the model did not hand it
// ([zeroInTheWorstRow] and [zeroInTheWorstLog] name each).
func everyField(t *testing.T, name string) BridgeCall {
	t.Helper()
	call := worstCall(t, name)
	call.Held, call.Approved, call.ArgsBytes, call.OutputBytes = true, true, math.MaxInt, math.MaxInt
	call.ArgsUnparsed = true
	call.Runner, call.Until = strings.Repeat("<", 4<<10), worstAt
	return call
}

// worstEntry is one call's entry in flight at its widest as the log keeps it
// ([keptEntry]): a name past its bound in the bytes that escape the most, a
// digest, both places and every instant at their widest, a runner past the
// most an incarnation may be in markup, and the id its harness gave it at its
// bound ([worstCallID]). KEPT FROM A CALL WITH EVERY FIELD SET ([everyField]),
// so a field the entry leaves zero is one keptEntry leaves out.
func worstEntry(t *testing.T, seq int) BridgeCall {
	t.Helper()
	call := everyField(t, strings.Repeat("<", 4<<10))
	call.Seq = math.MaxInt - seq
	return keptEntry(call)
}

// A CALL IN FLIGHT IS HELD TO ITS SHARE. An entry written ahead of a call keeps
// its identity and its harness's id ([keptEntry]); at its widest (name and
// runner past their bounds in six-fold-escaped markup, every instant longest,
// id at its bound) it weighs no more than [MaxCallInFlightBytes], whatever the
// caller handed it. An id past its bound is kept as none, never cut. The log's
// split closes as its table states: [BridgeLogBytes], [InFlightBytes] and
// [LogRestBytes] are [RowBytes] together.
// Mutation: keep a caller's runner or arguments as handed
// (entry-keeps-its-runner-whole), or its id whatever it is (log-keeps-any-id),
// and an entry overruns; cut an id to its bound (entry-cuts-its-id), and one
// past it keeps a cut id; hold more calls in flight than the table states
// (more-calls-in-flight-than-the-table-says), and the split no longer closes.
func TestACallInFlightIsHeldToItsShare(t *testing.T) {
	t.Parallel()
	entry := worstEntry(t, 0)
	if entry.CallID != worstCallID {
		t.Errorf("the widest entry keeps the id %q, want its harness's whole: the case is not at its worst",
			entry.CallID)
	}
	handed := entry
	handed.Args, handed.Output = strings.Repeat("<", 64<<10), strings.Repeat("<", 64<<10)
	handed.Runner, handed.CallID = strings.Repeat("<", 64<<10), strings.Repeat("<", 64<<10)
	if kept := keptEntry(handed); kept.CallID != "" {
		t.Errorf("an entry handed an id of %d bytes is kept with one of %d, want none: an id past its bound is "+
			"kept as none, never cut", len(handed.CallID), len(kept.CallID))
	}
	for name, call := range map[string]BridgeCall{"at its widest": entry, "as a caller handed it": keptEntry(handed)} {
		raw, err := json.Marshal(call)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("an entry in flight %s weighs %d of its %d", name, len(raw), MaxCallInFlightBytes)
		if len(raw) > MaxCallInFlightBytes {
			t.Errorf("an entry in flight %s weighs %d, past its share %d", name, len(raw), MaxCallInFlightBytes)
		}
	}
	if sum := BridgeLogBytes + InFlightBytes + LogRestBytes; sum != RowBytes {
		t.Errorf("the log's calls (%d), its %d calls in flight (%d) and the rest of it (%d) are %d together, "+
			"want the %d a value may: a share moved without the table that states it", BridgeLogBytes,
			MaxCallsInFlight, InFlightBytes, LogRestBytes, sum, RowBytes)
	}
}

// KEEPING A CALL AGAIN CHANGES NOTHING. Every append keeps every call in the
// log again ([BridgeLog.appendCall]), so a call kept once must come back as it
// was — its sizes still the whole's, never the size of what was kept.
//
// Mutation: record the kept text's size as the whole on a second keep, and the
// board says a megabyte was a few KiB.
func TestKeepingACallAgainChangesNothing(t *testing.T) {
	t.Parallel()
	once := keptCall(worstCall(t, "code_commit_files"), MaxBridgeArgsBytes)
	twice := keptCall(once, MaxBridgeArgsBytes)
	a, _ := json.Marshal(once)
	b, _ := json.Marshal(twice)
	if string(a) != string(b) {
		t.Errorf("a call kept twice changed:\n%s\n%s", a[:200], b[:200])
	}
}

// A WITHHELD CALL KEEPS NEITHER ITS ARGUMENTS NOR THEIR DIGEST, whoever handed
// it to the log: a call held for a person's approval, and the one they
// approved, carry their arguments sealed on the held call (ADR-0077), and a
// digest of them in the clear would confirm a guess at what the seal hides. A
// call dropped from the middle is tallied the same way, by its tool alone.
//
// Mutation: keep a withheld call's digest, or tally it under one, and the log
// holds it in the clear.
func TestAWithheldCallKeepsNeitherItsArgumentsNorTheirDigest(t *testing.T) {
	t.Parallel()
	for name, call := range map[string]BridgeCall{
		"held":     {Name: "close_issue", Held: true, Failed: true, Args: `{"id":7}`, Digest: strings.Repeat("d", 64)},
		"approved": {Name: "close_issue", Approved: true, Args: `{"id":7}`, Digest: strings.Repeat("d", 64)},
	} {
		got := keptCall(call, MaxBridgeArgsBytes)
		if got.Args != "" || got.ArgsBytes != 0 || got.Digest != "" {
			t.Errorf("%s: the log keeps %q (%d bytes) under identity %q, want neither", name, got.Args,
				got.ArgsBytes, got.Digest)
		}
		if e := identity(call); e.Digest != "" || e.Name != "close_issue" || e.Calls != 1 {
			t.Errorf("%s: a dropped call is tallied as %+v, want its tool alone", name, e)
		}
	}
}

// A FINISHED CALL IS NOBODY'S TO WAIT FOR, whoever hands it to the log. Only a
// call's entry in flight carries the process running it and the bound a
// collection waits for it until ([keptEntry]); a call kept finished keeps
// neither ([keptCall]), though it is handed both: as the log keeps any call, as
// the store records the call a person approved on its held call
// ([approvedCall]), and as the log appends that call, which no append over the
// bridge takes ([BridgeLog.withApproved]). A runner on a finished call is
// bounded by nothing, so a call kept with the one it was handed would hold the
// log's share and the row's to nothing.
//
// Mutation: keep a finished call's runner and bound as handed
// (kept-call-keeps-its-runner), and each of the three keeps both.
func TestAFinishedCallIsNobodysToWaitFor(t *testing.T) {
	t.Parallel()
	waited := func(call BridgeCall) BridgeCall {
		call.Runner, call.Until = strings.Repeat("<", 4<<10), worstAt
		return call
	}
	approved := approvedCall(waited(BridgeCall{Name: "close_issue", Origin: "builtin"}),
		HeldCall{ID: "held-1", Tool: "close_issue", MadeAt: worstAt})
	appended, ok := BridgeLog{Round: "r1"}.withApproved("r1", waited(approved))
	if !ok || len(appended.Calls) != 1 {
		t.Fatalf("the approved call's append left %d calls (appended %v), want it appended", len(appended.Calls), ok)
	}
	for name, call := range map[string]BridgeCall{
		"kept by the log":               keptCall(waited(BridgeCall{Name: "post_message", Seq: 1}), MaxBridgeArgsBytes),
		"approved, on its held call":    approved,
		"approved, appended to its log": appended.Calls[0],
	} {
		if call.Runner != "" || !call.Until.IsZero() {
			t.Errorf("a finished call %s keeps a runner of %d bytes and the bound %s, want neither", name,
				len(call.Runner), call.Until)
		}
	}
}

// THE CALL A PERSON APPROVED STATES NO SIZE OF THE OUTPUT IT DROPS. Its result
// rides the row sealed on the held call, and the call keeps none of it
// ([approvedCall]), so a size beside no output would read as a whole output cut
// to nothing: whoever hands the call one, it keeps no size of its output, as it
// keeps none of its arguments' ([keptCall]).
//
// Mutation: keep the size of the approved call's output as handed
// (approved-call-keeps-an-output-size), and it states one.
func TestTheApprovedCallStatesNoSizeOfTheOutputItDrops(t *testing.T) {
	t.Parallel()
	got := approvedCall(BridgeCall{Name: "close_issue", Output: "closed", OutputBytes: 64 << 10,
		Args: `{"id":7}`, ArgsBytes: 64 << 10}, HeldCall{ID: "held-1", Tool: "close_issue", MadeAt: worstAt})
	if got.Output != "" || got.OutputBytes != 0 || got.Args != "" || got.ArgsBytes != 0 {
		t.Errorf("the approved call keeps the output %q stated at %d bytes and the arguments %q stated at %d, "+
			"want neither, nor a size of either", got.Output, got.OutputBytes, got.Args, got.ArgsBytes)
	}
}

// IDENTITIES FOLD PAST THEIR SHARE AND NEVER LOSE A COUNT. A long run's dropped
// middle is a tally of identities held to [MaxElidedIdentityBytes]: while they
// fit, every identity is exact; past it identities fold into their tool's
// count and then into one count with no tool — so the tally always fits, and
// always accounts for every call dropped, with its failures.
//
// Mutation: drop an identity rather than fold it, and the count falls short of
// the calls; skip the second step, and a tally of many tools overruns its
// share.
func TestIdentitiesFoldPastTheirShareAndNeverLoseACount(t *testing.T) {
	t.Parallel()
	var ids []ElidedCall
	calls, failed := 0, 0
	for i := range 4000 {
		call := BridgeCall{
			// SIXTY TOOLS, then ever more of them, each called with
			// arguments of its own: exact first, by tool past the share,
			// unnamed past that.
			Name:   fmt.Sprintf("tool_%04d_%s", i%(60+i/10), strings.Repeat("x", 200)),
			Digest: fmt.Sprintf("%064x", i),
			Failed: i%7 == 0,
		}
		ids = fold(ids, identity(call), elidedTally)
		calls++
		if call.Failed {
			failed++
		}
		if n := tallyBytes(ids, elidedTally); n > MaxElidedIdentityBytes {
			t.Fatalf("after %d drops the tally weighs %d, past its %d", calls, n, MaxElidedIdentityBytes)
		}
	}
	gotCalls, gotFailed, unnamed := 0, 0, false
	for _, e := range ids {
		gotCalls += e.Calls
		gotFailed += e.Failed
		unnamed = unnamed || e.Name == ""
	}
	if gotCalls != calls || gotFailed != failed {
		t.Errorf("the tally counts %d calls (%d failed), want %d (%d)", gotCalls, gotFailed, calls, failed)
	}
	if !unnamed {
		t.Error("four thousand drops across hundreds of long names never reached the unnamed count")
	}

	// AN EXACT TALLY WHILE IT FITS: a few identities, each counted apart.
	var few []ElidedCall
	for i := range 30 {
		few = fold(few, identity(BridgeCall{Name: "read_file", Digest: fmt.Sprintf("%064x", i%3)}),
			elidedTally)
	}
	if len(few) != 3 || few[0].Calls != 10 || few[0].Digest == "" {
		t.Errorf("thirty drops of three identities tally %+v, want three exact entries of ten", few)
	}
}

// worstLeads is the leads a run's leads part keeps at their share
// ([MaxKeptLeadBytes]), each sealed the way a launch seals one ([LeadSeal.Seal])
// and numbered at its widest ([KeptLead.Appended]): a held call's answer at its
// worst, its question, its words and who gave them in the bytes that escape the
// most and the result of held, the worst held call, sealed at its widest as its
// row seals it ([HeldCall.Kept]), beside a review grown through the keys a
// later build adds to a lead until the two weigh the share exactly. So the
// part's arithmetic holds any history a take keeps, whatever it holds
// ([heldLeads]), as the row's does an answer within its share; and the share
// holds one lead at its worst, which is what it is sized for.
func worstLeads(t *testing.T, held HeldCall) []KeptLead {
	t.Helper()
	markup := func(n int) string { return strings.Repeat("<", n) }
	seal, err := NewLeadSeal(recordseal.OneKey("leads-k1", "the row model's fleet key material"))
	if err != nil {
		t.Fatal(err)
	}
	turn := strings.Repeat("a", 64)
	answer, err := seal.Seal(turn, Lead{Kind: LeadKindAnswer, Job: uuid.NewString(), Seq: math.MaxInt64,
		Round: math.MaxInt, Answer: &LeadAnswer{Question: markup(decision.MaxQuestion),
			Words: markup(MaxLeadWordsBytes), Cut: math.MaxInt, By: markup(iam.MaxLogin),
			Label: &provenance.Label{Source: provenance.Source(markup(32)), Trust: provenance.Trust(markup(32)),
				Authors: []provenance.Author{{Kind: iam.ActorKind(markup(32)), Seat: markup(iam.MaxLogin)}}},
			Withdrawn: true, Refused: &LeadRefused{Messages: math.MaxInt, Bytes: math.MaxInt, MaxBytes: math.MaxInt},
			Held: &LeadHeld{Tool: markup(MaxBridgeCallNameBytes), Server: markup(128), HeldID: uuid.NewString(),
				Choice: markup(32), Outcome: LeadHeldOutcome(markup(32)), Failed: true, Why: markup(1 << 10),
				Result: held.Kept()}}})
	if err != nil {
		t.Fatalf("sealing the worst lead: %v", err)
	}
	answer.Appended = math.MaxInt
	if one, _ := json.Marshal([]KeptLead{answer}); len(one) > MaxKeptLeadBytes {
		t.Fatalf("one lead at its worst weighs %d, past the %d a run's leads part keeps: the share holds at "+
			"least one", len(one), MaxKeptLeadBytes)
	}
	review, err := seal.Seal(turn, Lead{Kind: LeadKindReview, Job: uuid.NewString(), Seq: math.MaxInt64,
		Round: math.MaxInt})
	if err != nil {
		t.Fatalf("sealing a review: %v", err)
	}
	review.Appended = math.MaxInt
	leads := paddedTo(t, MaxKeptLeadBytes, []KeptLead{answer, review}, func(l *[]KeptLead, pad string) {
		(*l)[1].Extra = map[string]json.RawMessage{"pad": json.RawMessage(`"` + pad + `"`)}
	})
	if kept := heldLeads(leads); len(kept) != len(leads) || len(leads) > MaxKeptLeads {
		t.Fatalf("leads at their share keep %d of %d, at most %d: the model is not what a take keeps", len(kept),
			len(leads), MaxKeptLeads)
	}
	return leads
}

// heldResultAtItsWorst is the result of a held call as the row holds it at its
// widest: an output at [MaxHeldResultBytes] of markup, sealed the way the
// engine seals one ([CallSeal.SealResult]) — encoded with every byte escaped
// six-fold, then sealed, then written as one string.
func heldResultAtItsWorst(t *testing.T, held HeldCall) string {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := NewCallSeal(cipher).SealResult(strings.Repeat("a", 64), held,
		HeldResult{Output: strings.Repeat("<", MaxHeldResultBytes), Failed: true})
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// paddedTo is v grown, by the string pad sets, until it encodes to exactly n
// bytes — the widest a field held to n can be.
func paddedTo[T any](t *testing.T, n int, v T, pad func(*T, string)) T {
	t.Helper()
	pad(&v, "")
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > n {
		t.Fatalf("%T encodes to %d bytes before it is padded, past the %d it is grown to", v, len(raw), n)
	}
	pad(&v, strings.Repeat("a", n-len(raw)))
	if raw, _ = json.Marshal(v); len(raw) != n {
		t.Fatalf("%T padded to %d bytes, want %d", v, len(raw), n)
	}
	return v
}

// admitsPanel reports whether a held call's own contract ([HeldCall.validate])
// admits a call put to a panel of n delegates: whether it refuses nothing of
// the call's delegation, whatever it refuses of the rest of a call built to
// hold that alone.
func admitsPanel(n int) bool {
	delegates := make([]string, n)
	for i := range delegates {
		delegates[i] = fmt.Sprintf("delegate-%d", i)
	}
	held := HeldCall{Act: string(delegated.KindSSHOperate), Delegation: &HeldDelegation{Rule: 1,
		Delegates: delegates, Digest: strings.Repeat("d", maxHeldBindingBytes)}}
	return !slices.ContainsFunc(held.validate(nil), func(err error) bool {
		return strings.HasPrefix(err.Error(), "held_call.delegation ")
	})
}

// worstRow is a run's row with every field at its bound, in the bytes that
// escape the most and, where this package seals or cuts a field, by that code:
// parked state, recorded answer and let-go copies, held call with sealed
// arguments and result, the approved call, its decision and delegates, the
// turn's conversation read, who it works for, the answers that woke it, and the
// job's Gitlet holds, repositories and floors.
// A FIELD ADDED TO THE ROW IS SET HERE AT ITS BOUND, in the change that adds it:
// a row field comes out of the rest's share (rowbytes.go's RestBytes, through
// ExtraRowBytes), a parked-state one out of the state's.
// TestTheWorstRowSetsEveryField fails on a field left zero here that
// [zeroInTheWorstRow] does not name, TestTheWholeRowFitsItsBudget on figures
// rowbytes.go's table no longer states. A log field is set in [worstLog].
func worstRow(t *testing.T) PendingRun {
	t.Helper()
	markup := func(n int) string { return strings.Repeat("<", n) }
	plain := func(n int) string { return strings.Repeat("a", n) }
	// A LAUNCH'S VERDICT ON A SESSION IT DID NOT CONTINUE, every field at its
	// bound and its detail fitted as the launch fits it.
	worstVerdict := func() *executorkeep.NotContinued {
		v := executorkeep.NotContinued{Loss: executorkeep.Loss(plain(executorkeep.WordMaxBytes)),
			Instead:  executorkeep.Instead(plain(executorkeep.WordMaxBytes)),
			LostTurn: plain(executorkeep.TurnIDMaxBytes),
			Older:    &executorkeep.CapturePlace{LaunchSeq: math.MaxInt64, CaptureSeq: math.MaxInt64},
			Detail:   markup(1 << 20)}.Fit()
		v.OlderTurn = plain(executorkeep.TurnIDMaxBytes)
		return &v
	}
	handles := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%064d", i)
		}
		return out
	}
	// EVERY OPTION'S ID AT THE THIRTY-TWO BYTES ITS PATTERN ALLOWS.
	options := make([]decision.Option, decision.MaxOptions)
	for i := range options {
		options[i] = decision.Option{ID: fmt.Sprintf("option-%025d", i), Label: markup(decision.MaxOptionLabel),
			Detail: markup(decision.MaxOptionDetail)}
	}
	evidence := make([]decision.Evidence, decision.MaxEvidence)
	for i := range evidence {
		evidence[i] = decision.Evidence{Kind: "link", Ref: markup(decision.MaxEvidenceRef),
			Label: markup(decision.MaxEvidenceLabel)}
	}
	// TEN NOTES (internal/agent/steer's MaxTurnNotes), each sealed at its
	// bound, served before read, read by a self-named take (a fresh UUID)
	// settled unanswered and by a brief telling it again, every field of both
	// reads at its widest and marked restarted; each served [MaxNoteServes]
	// times, every serve lost at the widest step, its call kept at the widest
	// [keptCallOf] keeps. Every counter at the width of what it counts: a
	// read's and a serve's iteration and a note's Seq at math.MaxInt
	// (max_iterations has no ceiling, and a relaunch keeps Seq), a read's round
	// at 99, past a review's and a job's.
	widestCall := NoteCall{Session: plain(MaxNoteCallBytes), ID: `"` + plain(MaxNoteCallBytes-2) + `"`}
	if keptCallOf(&widestCall) == nil {
		t.Fatalf("the store keeps no call at its widest, %+v", widestCall)
	}
	// AND NONE WIDER AS WRITTEN: a call at the bound holding one byte the
	// encoder escapes, in its session, in a quoted id or in a bare one, would
	// be written up to six bytes a byte, which this model never measures.
	for _, escaping := range []NoteCall{
		{Session: plain(MaxNoteCallBytes-1) + "<", ID: "7"},
		{Session: plain(MaxNoteCallBytes), ID: `"` + plain(MaxNoteCallBytes-3) + `<"`},
		{Session: plain(MaxNoteCallBytes), ID: plain(MaxNoteCallBytes-1) + "<"},
	} {
		if keptCallOf(&escaping) != nil {
			t.Fatalf("the store keeps a call holding a byte its encoder escapes, %+v: a kept call is no longer "+
				"written as itself, so this row no longer models its calls at their widest", escaping)
		}
	}
	read := func() *NoteRead {
		return &NoteRead{At: worstAt, Order: math.MaxInt, Phase: "execute", Iteration: math.MaxInt, Round: 99,
			Boundary: "tool_batch", Launch: uuid.NewString(), Take: uuid.NewString(), Unanswered: true,
			Restarted: true}
	}
	serves := func() []HookServed {
		out := make([]HookServed, MaxNoteServes)
		for i := range out {
			out[i] = HookServed{Take: uuid.NewString(), Launch: uuid.NewString(), Boundary: "tool_batch",
				Iteration: math.MaxInt, At: worstAt, Lost: ServeCancelled, Call: keptCallOf(&widestCall)}
		}
		return out
	}
	notes := make([]RunNote, 10)
	for i := range notes {
		told := read()
		told.Retold = read()
		notes[i] = RunNote{ID: uuid.NewString(), Seq: math.MaxInt - i, Sealed: []byte(plain(MaxNoteSealedBytes)),
			At: worstAt, Served: uuid.NewString(), Place: math.MaxInt, Serves: serves(), Read: told,
			PollServes: MaxNoteServes, Readmitted: uuid.NewString()}
	}
	// AND A JOB'S ITEMS, as many as the row holds ([MaxJobItems]): late-call
	// items, the one kind the store files, each carrying its call at the
	// widest an item keeps one ([itemCall], [worstItemCall]), unsealed, for a
	// job, under the widest id ([lateItemID]), served as often and read by its
	// job's own step: no brief tells an item again, the next launch removing it.
	for range MaxJobItems {
		job := uuid.NewString()
		notes = append(notes, RunNote{ID: lateItemID(job, math.MaxInt), Seq: math.MaxInt - len(notes),
			At: worstAt, Served: uuid.NewString(), Place: math.MaxInt, Serves: serves(), For: job, Read: read(),
			PollServes: MaxNoteServes, Readmitted: uuid.NewString(),
			LateCall: &LateCall{Round: uuid.NewString(), Call: worstItemCall(t)}})
	}
	// AND THE TURN'S WAKE ITEMS, as many as the row holds ([MaxTurnWakes]): each
	// of [MaxItemWakes] wakes under the widest id, sealed at an even share of
	// [MaxWakeItemBytes], for a job, under the widest item id ([wakeItemID]),
	// served as often as a wake is ([MaxWakeServes]) and read, as every one a
	// relaunch keeps is ([openNotes]).
	for range MaxTurnWakes {
		job := uuid.NewString()
		wakes := make([]SealedWake, MaxItemWakes)
		for i := range wakes {
			wakes[i] = SealedWake{ID: plain(MaxWakeIDBytes), Sealed: []byte(plain(MaxWakeItemBytes / MaxItemWakes)),
				At: worstAt, From: plain(MaxWakeIDBytes)}
		}
		notes = append(notes, RunNote{ID: wakeItemID(job, plain(MaxWakeIDBytes)), Seq: math.MaxInt - len(notes),
			At: worstAt, Served: uuid.NewString(), Place: math.MaxInt, Serves: serves()[:MaxWakeServes], For: job,
			PollServes: MaxWakeServes, Read: read(), Wake: &WakeItem{Wakes: wakes}})
	}
	// AND THE TELLINGS, as many as the row holds ([MaxCallTellings]), each of
	// a round and a job at their widest ids, every number at its widest, a
	// hook's.
	tellings := make([]CallTelling, MaxCallTellings)
	for i := range tellings {
		tellings[i] = CallTelling{Round: uuid.NewString(), Appended: math.MaxInt, Launch: uuid.NewString(),
			Place: math.MaxInt, Hook: true}
	}
	// THE CALLS A BRIEF LEFT OUT, as a launch records them on the job's record
	// and a capture states them: a whole log's calls ([MaxBridgeCalls]) between
	// the reads it owes and the late calls, each a place at its widest, one of
	// them late so that both keys are written.
	omitted := make([]int, MaxBridgeCalls-1)
	for i := range omitted {
		omitted[i] = math.MaxInt
	}
	lateOmitted := []int{math.MaxInt}
	capture := &SessionCapture{LaunchSeq: math.MaxInt64, CaptureSeq: math.MaxInt64, Launch: uuid.NewString(),
		State: CaptureUnresolved, Reason: markup(1 << 10), Loss: executorkeep.Loss(plain(executorkeep.WordMaxBytes)),
		Through: new(math.MaxInt), Omitted: omitted,
		LateOmitted: lateOmitted, BridgeSeq: math.MaxInt, BridgeOpen: make([]int, maxOpenPlaces),
		NoteCursor:    math.MaxInt,
		ContextTokens: math.MaxInt64, NextRequestTokens: math.MaxInt64, NextRequestOlderTokens: math.MaxInt64,
		At: worstAt, Harness: plain(32), HarnessVersion: plain(64), Entry: plain(executorkeep.EntryMaxBytes),
		SessionID: plain(128), ArchiveFormat: 99, RunAfter: plain(MaxMessageIDBytes),
		Object: plain(256), ObjectHash: plain(64), ObjectSize: math.MaxInt64, ContentKey: plain(256),
		Home: markup(1024), Workspace: markup(1024), Final: true, Interrupted: true,
		SeatBudget: math.MaxInt64, Minted: worstAt, Taker: plain(maxIncarnationBytes), TakerEpoch: math.MaxInt64,
		StandbyTaker: plain(maxIncarnationBytes), StandbyTakerEpoch: math.MaxInt64, StandbyAt: worstAt,
		ResumedCount: worstCount(), CountProvider: plain(MaxCountProviderBytes)}
	for i := range capture.BridgeOpen {
		capture.BridgeOpen[i] = math.MaxInt
	}
	if err := capture.Bounded(); err != nil {
		t.Fatalf("the worst capture is past what the row carries: %v", err)
	}
	// THE SAVE UNDER WAY ([PendingRun.Saving]) is a third capture at the same
	// bounds, but for what a save never is: final, holding a note, or stood by
	// for (zeroInTheWorstRow says why).
	save := *capture
	save.Final, save.NoteCursor = false, 0
	save.StandbyTaker, save.StandbyTakerEpoch, save.StandbyAt = "", 0, time.Time{}
	if err := save.Bounded(); err != nil {
		t.Fatalf("the worst save is past what the row carries: %v", err)
	}
	// THE DECISION THE HELD CALL CARRIES OUT ([HeldAct]), each field at the
	// widest its source admits: the question, which for one asked in words is
	// the question comment's whole body, at most internal/worklet's
	// MaxCommentBody (32 KiB, named here as a figure because that package
	// imports this one), in the bytes that escape the most; the option the
	// answer chose, its label and its id at theirs, and the act the engine
	// composed for the question ([delegated.Act]); who answered it, a login at
	// [iam.MaxLogin]; and the task, its key, the question's id and the act's
	// server each at the sixty-four bytes this model gives an identifier (an
	// SSH server's id is at most forty).
	acting := &HeldAct{Item: plain(64), Key: plain(64), Ask: plain(64), Question: markup(32 << 10),
		Choice: markup(decision.MaxOptionLabel), ChoiceID: options[0].ID,
		Act: &delegated.Act{Kind: delegated.Kind(plain(32)), Approve: options[0].ID, Digest: plain(64),
			Host: plain(64)},
		AnsweredBy: plain(iam.MaxLogin), AnsweredAt: worstAt}
	// THE DELEGATES THE CALL WAS PUT TO FIRST ([HeldDelegation], ADR-0125): a
	// panel of two, the widest a held call's own contract admits, which
	// refuses a third as internal/config refuses a rule naming one (checked
	// here against [HeldCall.validate], so a contract that widens the panel
	// fails this model until it is widened too); each delegate with its answer
	// at the bounds the verdict tool holds one to ([HeldVerdict]: its reason
	// and the most findings, in the bytes that escape the most), and the
	// escalation that put the call to people at the most reasons it carries
	// ([HeldEscalation]): each an engine word, none of which is longer than the
	// thirty-two bytes this model gives a closed word, and a sentence at its
	// bound.
	delegates := handles(2)
	if !admitsPanel(len(delegates)) || admitsPanel(len(delegates)+1) {
		t.Fatalf("a held call's own contract does not admit a panel of %d delegates and refuse one of %d: put "+
			"the worst held call to the widest panel HeldCall.validate admits", len(delegates), len(delegates)+1)
	}
	verdict := func(by string) HeldVerdict {
		findings := make([]string, MaxReviewFindings)
		for i := range findings {
			findings[i] = markup(MaxReviewFindingRunes)
		}
		return HeldVerdict{By: by, Verdict: plain(32), Confidence: plain(32), Reason: markup(MaxReviewReasonRunes),
			Findings: findings, At: worstAt}
	}
	given := make([]HeldVerdict, len(delegates))
	for i, by := range delegates {
		given[i] = verdict(by)
	}
	overruled, said := make([]string, maxEscalationReasons), make([]string, maxEscalationReasons)
	for i := range maxEscalationReasons {
		overruled[i], said[i] = plain(32), markup(maxEscalationRunes)
	}
	delegation := &HeldDelegation{Rule: math.MaxInt, Delegates: delegates, Digest: plain(maxHeldBindingBytes),
		Given: given, Escalated: &HeldEscalation{Overruled: overruled, Engine: said, At: worstAt}}
	// AND A FIXED CALL'S MARKS BESIDE THEM, which a call to an engine tool
	// carries: who may answer it, a closed word, and what its tool binds it to
	// and the identity of what it decides, each a digest at its bound
	// ([HeldCall.Binding], [HeldCall.Subject]).
	held := &HeldCall{ID: uuid.NewString(), LaunchID: uuid.NewString(), Server: plain(32), Tool: plain(128),
		Raw: plain(128), Digest: plain(64), Arguments: plain(MaxHeldArgsBytes - 2), Fixed: true,
		Approvers: plain(32), Binding: plain(maxHeldBindingBytes), Acting: acting, Act: plain(32),
		Subject: plain(maxHeldBindingBytes), Delegation: delegation, HeldAt: worstAt, LapsesAt: worstAt,
		CallingAt: worstAt, MadeAt: worstAt}
	held.Result = heldResultAtItsWorst(t, *held)
	// THE CALL A PERSON APPROVED, as the store records it on the held call
	// ([approvedCall]) and a reset carries it on to the log it is owed to
	// ([OwedCall]): KEPT FROM A CALL WITH EVERY FIELD SET ([everyField]), so
	// each field it carries is at its widest and each it leaves zero (its
	// arguments, their digest and size, its output and a size of it, an id a
	// harness gave it, its places and marks, a runner and a bound) is one
	// approvedCall or keptCall leaves out, never one the model did not hand it.
	approved := approvedCall(everyField(t, markup(4<<10)), *held)
	held.Call = &approved
	if err := HeldArgsFit(held.Arguments); err != nil {
		t.Fatalf("the worst held call's arguments are past their share: %v", err)
	}
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = uuid.NewString()
		}
		return out
	}
	answer := paddedTo(t, MaxAnswerBytes, RecordedAnswer{Sealed: []byte(plain(64 << 10)), Via: types.AnswerViaChat,
		EventIDs: ids(20), PostedAt: worstAt, RecordedAt: worstAt, From: StatusAwaiting, TakenAt: worstAt,
		LostClaims: math.MaxInt, FirstLostAt: worstAt}, func(a *RecordedAnswer, pad string) {
		a.Events = []json.RawMessage{json.RawMessage(`"` + pad + `"`)}
	})
	// A REFUSAL BESIDE IT, AT ITS BOUND, which the row never holds beside an
	// answer at its share (its own answer weighs a few KiB): one delivery's
	// ids at the most a drain hands one partition (internal/config's
	// `notification_coalesce_max_batch`, at most 100), its two figures at
	// their widest, and where it was posted at [MaxRefusalAddressBytes] in the
	// bytes that escape the most. And the round's refusals kept for its
	// review, as many as it keeps, each at its widest.
	address := map[string]string{"transport": ""}
	pad := MaxRefusalAddressBytes - addressBytes(address)
	address["transport"] = markup(pad/6) + plain(pad%6)
	if got := addressBytes(address); got != MaxRefusalAddressBytes {
		t.Fatalf("the worst address weighs %d, want its bound %d", got, MaxRefusalAddressBytes)
	}
	refusal := &AnswerRefusal{EventIDs: ids(100), Bytes: math.MaxInt, MaxBytes: math.MaxInt, Address: address}
	roundRefusals := make([]RoundRefusal, maxRoundRefusals)
	for i := range roundRefusals {
		roundRefusals[i] = RoundRefusal{Job: uuid.NewString(), Messages: math.MaxInt, Bytes: math.MaxInt,
			MaxBytes: math.MaxInt}
	}
	handBack := paddedTo(t, MaxAnswerBytes, []HandedBack{{ID: uuid.NewString(), Original: uuid.NewString()}},
		func(h *[]HandedBack, pad string) {
			(*h)[0].Event = json.RawMessage(`"` + pad + `"`)
		})
	// THE LEADS ITS BRIEF TOLD, AS MANY AS THE PART KEEPS, each number at its
	// widest: nineteen digits each, ascending, as the attach writes them
	// ([LaunchRecord.LeadMarks], [MaxKeptLeads]). The leads themselves are
	// beside the row ([worstLeadsPart]), never on it.
	marks := make([]int, MaxKeptLeads)
	for i := range marks {
		marks[i] = math.MaxInt - (MaxKeptLeads - 1) + i
	}
	launch := LaunchRecord{StartedAt: worstAt, Started: true, Model: markup(256), ProviderKey: plain(64),
		SignInID: uuid.NewString(), Entry: plain(executorkeep.EntryMaxBytes), FollowUp: FollowUpReason(plain(32)),
		Brief: BriefKind(plain(32)), NotContinued: worstVerdict(),
		BriefOmitted: omitted, FreshOmitted: omitted, BriefLateOmitted: lateOmitted, FreshLateOmitted: lateOmitted,
		LeadMarks: marks, FreshSessionID: plain(128), SessionStart: SessionStart(plain(32)), Minted: plain(128),
		ResumeCarry: ResumeCarry(plain(32)), Iteration: math.MaxInt, Published: true, CollectFailures: math.MaxInt,
		CollectFailingSince: worstAt,
		ResumeFailures:      math.MaxInt, ResumeFailingSince: worstAt, Collected: true, CollectedAt: worstAt,
		// THE WIDEST WINDOW A RELEASE COUNTS ONTO THE RECORD, a day's: a
		// release writes none that is no window (BudgetWindow.Valid), and of
		// the three periods a day spells the widest pair, written as itself.
		RelaunchRefusedIn: BudgetWindow{Period: period.Day, Label: period.At(period.Day, worstAt, nil).Label},
		EndedAtDeadline:   true, HostStoreSilentSince: worstAt, HarnessEnd: HarnessEnd(plain(32)),
		// A REFUSAL THE SEAT'S HOLDER OWES, its condition at the longest
		// the record takes, in the bytes that escape the most: nothing but
		// that bound holds a condition's spelling ([MaxOwedConditionBytes]).
		RefusalOwed: Condition(markup(MaxOwedConditionBytes)), RefusalOwedSince: worstAt,
		// WHAT THE COPY IN ITS BOX CARRIED, at its widest: a figure of
		// nineteen digits, a source as long as an end's word, and the job it
		// names a launch id as the begin mints one ([LaunchRecord.Context]).
		Context: &SessionMeasure{Tokens: math.MaxInt64, Source: MeasureSource(plain(32)), Launch: uuid.NewString()},
		// WHAT CLAUDE CODE COUNTS THE COPY AT, at its widest ([worstCount]),
		// and the restart its launch's session held, a uuid's thirty-six
		// bytes ([LaunchRecord.BasisRestart]) in the bytes that escape the
		// most: the box's thread writes it, and nothing holds it to a uuid's
		// shape.
		ResumedCount: worstCount(), BasisRestart: new(markup(RestartIDMaxBytes)),
		// AND THE POINT OF THE KEPT CAPTURE IT CONTINUED, each figure at its
		// widest spelling, twenty bytes for an int64's minimum
		// ([LaunchRecord.Continued]): nothing but the type bounds either.
		Continued: &ContinuedPoint{LaunchSeq: math.MinInt64, Through: math.MinInt},
		// AND THE PROVIDER BESIDE IT at its bound, every byte one a record
		// writes as itself ([ValidCountProvider]).
		CountProvider: plain(MaxCountProviderBytes),
		// AND THE RELEASE THAT WROTE THE COPY at the most a release is
		// ([ValidRelease]).
		CountRelease: plain(executorkeep.HarnessVersionMaxBytes),
		// AND WHAT THE HARNESS ESTIMATES ITS NEXT REQUEST AT, nineteen digits
		// at each rate ([LaunchRecord.NextRequestTokens]).
		NextRequestTokens: math.MaxInt64, NextRequestOlderTokens: math.MaxInt64,
		Bridged: EngineSpend{Aux: AuxTokens{Input: math.MaxInt, Output: math.MaxInt, CacheRead: math.MaxInt,
			CacheWrite: math.MaxInt}, Workers: math.MaxInt, WorkerInput: math.MaxInt, WorkerOutput: math.MaxInt},
		Condensed: AuxTokens{Input: math.MaxInt, Output: math.MaxInt, CacheRead: math.MaxInt,
			CacheWrite: math.MaxInt}}
	// THE PARKED STATE AT ITS SHARE, whatever it holds: the park condenses a
	// state's closed rounds to fit and refuses one still past it
	// (internal/agent/execstate's EncodeWithin), and what no park condenses —
	// the turn's own context, how its conversation's earlier turns ended among
	// it (Frozen.Endings, up to 24,000 bytes as the state holds them) — is
	// inside this share, never beside it.
	state := `"` + plain(ExecuteStateBytes-2) + `"`
	// THE TURN'S READ OF ITS CONVERSATION at its bound
	// ([ConversationBaseMaxBytes]), every field of it set and the whole grown
	// to the bound through the section's own carried keys, so the row's
	// arithmetic holds any base within its bound, as it does an answer within
	// its share: internal/engine's TestAFrozenReadFitsItsBound holds the
	// largest read the executor log admits, frozen as a turn's first launch
	// freezes it, under the bound.
	base := paddedTo(t, ConversationBaseMaxBytes, ConversationBase{ReadThrough: math.MaxInt64,
		Absorbs: []string{plain(64)},
		Siblings: []BaseSibling{{Turn: plain(64), Object: plain(256), ObjectHash: plain(64),
			ObjectSize: math.MaxInt64}},
		Session: &BaseSession{Turn: plain(64), Object: plain(256), ObjectHash: plain(64), ObjectSize: math.MaxInt64,
			ContentKey: plain(256), Harness: plain(32), HarnessVersion: plain(64), SessionID: plain(128),
			Entry:         plain(executorkeep.EntryMaxBytes),
			ArchiveFormat: 99, Home: plain(1024), Workspace: plain(1024), ContextTokens: math.MaxInt64,
			Interrupted: true, CapturedAt: worstAt, WentOn: true, Lost: true, EndedAt: worstAt}},
		func(b *ConversationBase, pad string) {
			b.Extra = map[string]json.RawMessage{"pad": json.RawMessage(`"` + pad + `"`)}
		})
	// THE OWED END AS ITS WRITER PUTS IT ON THE ROW ([owedEnd]): every prose
	// field past its bound in markup, fitted to the executor log's, and every
	// other at the log's own bound.
	end, err := owedEnd(plain(64), executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
		Decision: plain(executorkeep.WordMaxBytes), Delivered: true,
		DeliveredTo: plain(executorkeep.WordMaxBytes), Answer: plain(executorkeep.WordMaxBytes),
		DeliveryUnknown: true, CompletedWork: markup(1 << 20), BlockedOn: markup(1 << 20),
		StoppedBy: markup(executorkeep.ActorMaxBytes), StopReason: markup(1 << 20), Failure: markup(1 << 20),
		Unkept: &executorkeep.Unkept{Loss: executorkeep.LossStoreRefused, LaunchSeq: math.MaxInt64, CaptureSeq: math.MaxInt64,
			Reason: markup(1 << 20)}})
	if err != nil {
		t.Fatalf("the worst owed end: %v", err)
	}
	// WHAT THE JOB MAY DO ON GITLET: the most repositories a job is
	// given ([MaxRunRepos]), each at the widest its grammar admits
	// ([RunRepo.ValidateFor]) — a name of [gitref.MaxRepoNameBytes], a whole
	// commit id, a branch of the longest a ref allows in the bytes that
	// escape the most, the most cone paths at their longest, and an ask to
	// open a change into another such branch under a title at its bound
	// ([MaxChangeTitleBytes]); a change it works on is never beside that ask,
	// and weighs less.
	repos := make([]RunRepo, MaxRunRepos)
	for i := range repos {
		paths := make([]string, MaxConePaths)
		for j := range paths {
			paths[j] = fmt.Sprintf("%03d", j) + markup(MaxConePathBytes-3)
		}
		repos[i] = RunRepo{Repo: fmt.Sprintf("%063d", i), RepoID: uuid.NewString(),
			Base: strings.Repeat("f", gitref.OIDHexLen), Branch: fmt.Sprintf("%d", i) + markup(gitref.MaxBranchBytes-1),
			Access: RepoWrite, Paths: paths,
			Open: &OpenChange{Target: "refs/heads/" + markup(gitref.MaxBranchBytes), Title: markup(MaxChangeTitleBytes),
				Draft: true}}
		if err := repos[i].ValidateFor(CloneCoding); err != nil {
			t.Fatalf("the worst repository is not one a job may be given: %v", err)
		}
	}
	// THE TURN'S HOLDS, the most a turn may take ([MaxCodeHolds]), each a
	// seat branch's key at Gitlet's fixed width (internal/gitlet's
	// RefHoldKey, 69 bytes, the wider of the two kinds); and its floors, a
	// statelog position's token each — a stream's name, under 64 bytes, and
	// two 64-bit numbers at their widest — which the engine mints and this
	// package carries unread.
	holds := make([]string, MaxCodeHolds)
	for i := range holds {
		holds[i] = uuid.NewString() + ":" + fmt.Sprintf("%032x", i)
	}
	floor := fmt.Sprintf("%s@%d:%d", plain(64), uint64(math.MaxUint64), uint64(math.MaxUint64))
	// WHO THE TURN WORKS FOR ([types.WorkingFor], ADR-0109), on the row, on
	// its code writes as its checkout composed it ([PendingRun.CodeWorkingFor])
	// and on the colleague's ask it answers: every field set, a seat's handle
	// and a credential's login each at [iam.MaxLogin] beside every app, though
	// one names a seat or a credential and never both. Beside it on the row, the
	// credential that woke the turn ([PendingRun.RequesterLogin]), kept beside
	// a requester seat here though the row keeps it only where it names none,
	// and the answers whose deliveries woke the turn ([PendingRun.Answered]):
	// one comment's id for each delivery one drain hands a partition
	// (internal/config's `notification_coalesce_max_batch`, at most 100, which
	// bounds a digest's constituents too), each at the sixty-four bytes this
	// model gives an identifier.
	workingFor := func() *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForKind(plain(32)), Seat: plain(iam.MaxLogin),
			Login: plain(iam.MaxLogin), Apps: slices.Clone(iam.Apps)}
	}
	// THE CALLS THE JOB PARKED ON, as many as it may ([MaxParkedCalls]), each
	// tool name at its bound in its most-escaping bytes, each marked the ask and
	// naming the row's job (decodeRun reads a call of another job as none).
	launchID := uuid.NewString()
	parked := make([]CallWait, MaxParkedCalls)
	for i := range parked {
		parked[i] = CallWait{Tool: markup(coord.MaxMCPToolNameBytes), Seq: math.MaxInt, Ask: true,
			Job: launchID}
	}
	// THE JOB'S FOLD-INS, as many as it asks ([MaxJobFoldIns]), each answered,
	// every name at its widest and its words filling [MaxFoldInBytes]: the
	// store refuses one past it.
	foldInJob := uuid.NewString()
	foldIns := make([]FoldIn, MaxJobFoldIns)
	for i := range foldIns {
		f := FoldIn{ID: uuid.NewString(), Launch: foldInJob, Wake: plain(MaxWakeIDBytes), Asker: plain(64),
			AskerKind: iam.ActorOperator, Conversation: plain(256), Cut: math.MaxInt,
			Label: provenance.Label{Source: provenance.SourceChatThread, Trust: provenance.TrustExternal,
				Authors: []provenance.Author{{Kind: iam.ActorOperator, Seat: plain(64)}}},
			PutTo: handles(MaxFoldInPutTo), AskedAt: worstAt, Answer: FoldInRefuse, AnsweredBy: plain(64),
			AnsweredByKind: iam.ActorOperator, AnsweredAt: worstAt}
		size, err := foldInBytes(f)
		if err != nil {
			t.Fatal(err)
		}
		f.Words = plain(MaxFoldInBytes - size)
		foldIns[i] = f
	}
	// WHAT THE TURN'S CALLS WROTE, as many keys as the row keeps, each at the
	// one width a key has ([writtenKey]).
	wroteKeys := make([]string, types.MaxWrittenRecords)
	for i := range wroteKeys {
		wroteKeys[i] = writtenKey(types.ConversationWritten(fmt.Sprintf("chatlet:room:%d", i)))
	}
	run := PendingRun{
		TurnID: plain(64), WorkKey: plain(64), WorkSince: worstAt,
		AgentHandle: plain(64), AgentID: uuid.NewString(), Role: markup(128),
		SandboxID: plain(128), Harness: plain(32), Placement: plain(64), CommandID: plain(128),
		Status: StatusAnswered, LaunchID: launchID, LaunchSeq: math.MaxInt64,
		// THE MESSAGES ITS JOB SENT OTHER RUNS, at their bound, for a job id.
		RunSendsFor: uuid.NewString(), RunSends: MaxJobRunSends,
		// THE RUN ITS TURN'S LEADS ARE KEPT FOR, a launch id as the create
		// writes it, and how many leads that part has taken, at its widest.
		LeadsRun: uuid.NewString(), LeadsAppended: math.MaxInt,
		BoxSince: worstAt, Capture: capture, Kept: capture, Saving: &save, Base: &base,
		SpentSessions: slices.Repeat([]executorkeep.SpentSession{{Session: plain(executorkeep.SessionIDMaxBytes),
			Loss: executorkeep.Loss(plain(executorkeep.WordMaxBytes))}}, executorkeep.SpentMaxSessions),
		NotContinued: slices.Repeat([]executorkeep.NotContinued{*worstVerdict()}, executorkeep.NotContinuedMaxEntries),
		OwedEnd:      end, EndRecorded: true,
		Launch: launch, Owner: plain(maxIncarnationBytes), OwnerEpoch: math.MaxInt64,
		TaskDescription: keptTitle(markup(1 << 20)), Label: keptTitle(markup(1 << 20)),
		Reply: plain(64), PartitionKey: markup(512), ConversationKey: markup(512),
		Branch: markup(gitref.MaxBranchBytes), SessionID: plain(128),
		Question: markup(decision.MaxQuestion), QuestionContext: markup(MaxAskContextBytes),
		Decision: &decision.Decision{Question: markup(decision.MaxQuestion), Options: options,
			Recommended: options[0].ID, Rationale: markup(decision.MaxRationale), Evidence: evidence,
			Role: decision.RoleApprover, Inform: &decision.Inform{Surface: decision.InformMattermost,
				Channel: markup(decision.MaxInformChannel)}},
		Audience:          markup(MaxAskToBytes),
		HeldCall:          held,
		OwedCall:          &OwedCall{Round: uuid.NewString(), Call: approved},
		ParkedInputTokens: math.MaxInt, ParkedOutputTokens: math.MaxInt,
		ParkedCacheReadTokens: math.MaxInt, ParkedCacheWriteTokens: math.MaxInt,
		AskedAt: worstAt, Answer: &answer, AnswerRefusal: refusal, DeclinedAnswers: handles(maxDeclinedAnswers),
		HandBack: handBack, EndingReturnsRefusal: true, RoundRefusals: roundRefusals,
		Ending: &RecordedEnding{ID: uuid.NewString(), At: worstAt, Reason: plain(64), Detail: markup(1 << 10),
			ErrorKind: plain(32), Returned: true, Unused: true, Reclaim: true},
		Stop: &RunStop{By: markup(128), ByKind: plain(32), OperatorID: plain(64), Reason: markup(4 * 500),
			Collected: true, InputTokens: math.MaxInt, OutputTokens: math.MaxInt, CacheReadTokens: math.MaxInt,
			CacheWriteTokens: math.MaxInt, Condensed: AuxTokens{Input: math.MaxInt, Output: math.MaxInt,
				CacheRead: math.MaxInt, CacheWrite: math.MaxInt}},
		// THE LONGEST TRACKER A WORK ITEM LIVES IN, `github` and `gitlab`.
		WorkItem: &types.WorkItem{Backend: types.WorkGitHub, ID: plain(64), Key: plain(64),
			Project: plain(64)},
		WroteKeys:       wroteKeys,
		Answered:        handles(100),
		AudienceHandles: handles(MaxAudienceHandles), AudienceFallback: true, Requester: plain(64),
		RequesterLogin: plain(iam.MaxLogin), RequesterApps: slices.Clone(iam.Apps), WorkingFor: workingFor(),
		ColleagueAsk: new(ColleagueAsk{ChannelID: plain(64), Chain: handles(32), TraceID: plain(32),
			SpanID: plain(16), ParentSpanID: plain(16),
			WorkingFor: workingFor()}.WithQuestion(keptQuestion(markup(1 << 20)))),
		TraceID: plain(32), SpanID: plain(16), DelegationDepth: math.MaxInt, DelegationChain: handles(32),
		ExecuteState: json.RawMessage(state), Charged: true, CompanyCharged: true,
		CodeHolds: holds, Repos: repos, CodeFloor: floor, CodeLaunch: floor, CodeCeiling: plain(64),
		CodeWorkingFor: workingFor(), PauseTTLSeconds: math.MaxFloat64,
		PausedAt: worstAt, Deadline: worstAt, LaunchNode: plain(64),
		Lost: &JobLoss{Cause: LossCause(plain(32)), Node: plain(64), At: worstAt}, RoundLosses: math.MaxInt,
		Round: uuid.NewString(), AnsweredJob: uuid.NewString(),
		HeldFor: Condition(plain(32)), HeldUntil: worstAt, Holds: math.MaxUint64,
		Steer: RunSteer{Seat: []byte(plain(512)), Seq: math.MaxInt, Notes: notes, Pulled: uuid.NewString(),
			Reads: math.MaxInt, Tellings: tellings},
		FoldIns:     foldIns,
		ParkedCalls: parked,
		CreatedAt:   worstAt, UpdatedAt: worstAt,
	}
	return run
}

// worstLog is a run's bridged call log with every field at its bound, in the
// bytes that escape the most and through [BridgeLog.appendCall]: 200 worst
// calls kept and 100 dropped, so the identity tallies fold to their last step,
// the calls' own work too; [MaxPriorCalls] prior names; the log's identifiers;
// [BridgeLog.LateElided] at its most (every dropped call late, as all but the
// newest submission are); every kind of change on every call, so the union in
// [BridgeLog.Committed] is full; and [MaxCallsInFlight] entries ([worstEntry]).
// EACH CALL IS HANDED WHAT NO FINISHED CALL KEEPS (a runner, a bound, failed
// work), so a field the log leaves zero is one the append drops
// ([zeroInTheWorstLog]). A FIELD ADDED TO THE LOG IS SET HERE AT ITS BOUND:
// a call's comes out of [BridgeLogBytes], any other out of [RowBytes]'s room.
// TestTheWorstLogSetsEveryField fails on one left zero and unnamed.
func worstLog(t *testing.T) BridgeLog {
	t.Helper()
	bridged := BridgeLog{Launch: uuid.NewString(), Round: uuid.NewString(), CutFor: uuid.NewString(),
		AnsweredThrough: math.MaxInt, PriorCalls: make([]string, MaxPriorCalls), PriorCallsElided: true}
	for i := range bridged.PriorCalls {
		// A SUCCESSFUL call's name, which the surface resolved: a tool's
		// name, held to the MCP name grammar (internal/mcp's toolnames.go),
		// so it escapes to itself.
		bridged.PriorCalls[i] = fmt.Sprintf("%0128d", i)
	}
	for i := range MaxBridgeCalls + elidedWorst {
		call := worstCall(t, fmt.Sprintf("tool_%04d_%s", i, strings.Repeat("<", 300)))
		call.Seq = i + 1
		call.Failed = i%2 == 0
		call.Runner, call.Until = strings.Repeat("<", 4<<10), worstAt
		if i == MaxBridgeCalls+elidedWorst-1 {
			// AND NOT LATE: the whole share is never a late submission's
			// ([submitted]), so the log's worst is its own job's.
			call.Name, call.Failed, call.Late = SubmitWorkTool, false, false
		}
		worker := []ElidedCall{{Name: fmt.Sprintf("worker_%04d_%s", i, strings.Repeat("<", 300)),
			Digest: fmt.Sprintf("%064x", i), Calls: 1, Failed: 1}}
		bridged.appendCall(call, worker)
	}
	for i := range MaxCallsInFlight {
		bridged.InFlight = append(bridged.InFlight, worstEntry(t, i))
	}
	// THE WRITTEN SET AT ITS BOUND, each id at its own in the bytes that escape
	// the most, and one record more, refused, which marks it ([BridgeLog.wrote]).
	for i := range types.MaxWrittenRecords + 1 {
		bridged.wrote([]types.WrittenRecord{{Kind: types.WrittenConversation,
			ID: fmt.Sprintf("%04d", i) + strings.Repeat("<", types.MaxWrittenIDBytes-4)}})
	}
	return bridged
}

// elidedWorst is how many calls the worst log drops.
const elidedWorst = 100

// A RUN'S VALUES ARE BUDGETED TO WHAT ONE KEY OF THE STORE HOLDS.
//
// Every share in rowbytes.go is a share of one run value, which the store
// refuses past coord.MaxValueBytes, about a quarter under the transport's eight
// mebibytes (the largest a leader's read carries across a leaf's link). Closed
// at the transport, a seven-megabyte row would pass and every write of its run
// be refused. The budget tests hold a row and a log to these shares, so to the
// store.
// Mutation: budget to the transport's ceiling, and it is past the store's bound.
func TestARunsValuesAreBudgetedToWhatOneKeyOfTheStoreHolds(t *testing.T) {
	t.Parallel()
	if RowBytes > coord.MaxValueBytes {
		t.Errorf("a run's value may weigh %d bytes, past the %d one key of the store holds: every "+
			"write of a value between the two is refused", RowBytes, coord.MaxValueBytes)
	}
}

// THE WHOLE ROW FITS ITS BUDGET: rowbytes.go's arithmetic, held to the row's
// own encoder, every field at its bound in its most-escaping bytes and the
// handed set within [RowBytes]. And the table's measured figures
// ([measuredOtherFields], [measuredSpare]) are compared exactly, since the
// worst row weighs the same on every run: a figure only read goes stale.
//
// Mutation: raise a share or a bound past the rest's room (answers, held args,
// the conversation, repos, cone paths, holds, leads, incarnations at 700,000),
// or grow a seal or result, and the rest overruns; change a seal or the
// conversation read by a byte, and a measured figure is off; hold a served call
// to its length alone, and the worst row refuses to be built; keep a written
// record whole as its key, and the keys overrun their figure.
func TestTheWholeRowFitsItsBudget(t *testing.T) {
	t.Parallel()
	run := worstRow(t)
	whole := encoded(t, run)

	withoutState := run
	withoutState.ExecuteState = nil
	stateBytes := len(run.ExecuteState)
	rest := encoded(t, withoutState)

	withoutSource := withoutState
	withoutSource.Answer, withoutSource.HandBack, withoutSource.Base = nil, nil, nil
	held := *withoutSource.HeldCall
	held.Arguments = ""
	withoutSource.HeldCall = &held
	others := encoded(t, withoutSource)
	spare := RestBytes - ExtraRowBytes - rest
	t.Logf("the row weighs %d of its %d: the state %d of %d, the rest %d of %d with %d left for a later "+
		"build — of which a recorded answer, its hand-back, a held call's arguments and the turn's read of "+
		"its conversation %d, and every other field %d — and %d to spare", whole, RowBytes, stateBytes,
		ExecuteStateBytes, rest, RestBytes, ExtraRowBytes, rest-others, others, spare)

	if others != measuredOtherFields || spare != measuredSpare {
		t.Errorf("every other field weighs %d with %d to spare, and rowbytes.go states %d with %d: restate "+
			"measuredOtherFields, measuredSpare and the lines of the table that state them", others, spare,
			measuredOtherFields, measuredSpare)
	}
	if stateBytes > ExecuteStateBytes {
		t.Errorf("the parked state weighs %d, past its share %d", stateBytes, ExecuteStateBytes)
	}
	if rest > RestBytes-ExtraRowBytes {
		t.Errorf("the rest weighs %d, past its share %d less the %d left for a later build", rest, RestBytes,
			ExtraRowBytes)
	}
	if whole+ExtraRowBytes > RowBytes {
		t.Errorf("the row weighs %d, and with a later build's %d past its %d", whole, ExtraRowBytes, RowBytes)
	}
	if ExecuteStateBytes+RestBytes != RowBytes {
		t.Error("the row's two shares do not add up to its budget")
	}
	withoutKeys := run
	withoutKeys.WroteKeys = nil
	if keys := whole - encoded(t, withoutKeys); keys > wroteRowBytes {
		t.Errorf("the keys of what the turn wrote weigh %d, past their figure %d", keys, wroteRowBytes)
	}
	withoutParked := run
	withoutParked.ParkedCalls = nil
	if parked := whole - encoded(t, withoutParked); parked > parkedRowBytes {
		t.Errorf("the calls the job parked on weigh %d, past their figure %d", parked, parkedRowBytes)
	}

	// THE HANDED SET, the third value: the most secrets a launch keeps
	// ([MaxHandedBytes]), sealed and written as one string — a third more,
	// and the seal's own bytes.
	handed, err := json.Marshal(handedSet{Launch: uuid.NewString(), LaunchSeq: math.MaxInt64,
		Sealed: strings.Repeat("a", MaxHandedBytes*4/3+1<<10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(handed) > RowBytes {
		t.Errorf("the handed set weighs %d, past the %d a value may", len(handed), RowBytes)
	}
}

// zeroInTheWorstRow is every field of the row [worstRow] leaves zero, by its
// path ([zeroOnTheWire]), and why the row never carries it beside the rest at
// their worst: the code that writes the row leaves it out of a value the model
// hands that code with the field set, or the row holds it apart from a field
// that weighs more, which the worst row sets. Each of the first kind names the
// code that leaves it out, so the code ceasing to is a field the walk finds
// set, and its name a reason that no longer holds.
var zeroInTheWorstRow = func() map[string]string {
	zero := map[string]string{
		"Repos[0].Change": "a change the job works on is never beside an ask to open one (RunRepo.ValidateFor " +
			"refuses the two together), and the ask, which the worst row sets, weighs more",
		"Steer.Notes[0].Read.Retold.Retold": "a brief that tells a note again keeps the marked take's read " +
			"beside its own (openNotes), and that read is never one a brief told: only a read marked unanswered " +
			"is told again, a settle marks only a read naming its take, and a read a brief told names none",
		"Steer.Notes[0].For": "the walk reads the first note, a person's, which is an item of the whole turn and " +
			"names no job (RunNote.For); the worst row's job items, as many as it holds (MaxJobItems), each name " +
			"theirs",
		"Steer.Notes[0].LateCall": "the walk reads the first note, a person's, which hands no job a call " +
			"(RunNote.LateCall); the worst row's job items each carry theirs at its widest (worstItemCall)",
		"Steer.Notes[0].Serves[0].Digest": "a poll's serve records its item's digest and no call, a hook's a " +
			"call and no digest (PendingStore.ServeNotes, ServeHook), and the call, which the worst row's serves " +
			"keep at its widest, weighs more",
		"Steer.Notes[0].Wake": "the walk reads the first note, a person's, which hands no job a wake " +
			"(RunNote.Wake); the worst row's wake items, as many as it holds (MaxTurnWakes), each carry theirs " +
			"at their bound",
		"WaitsFor": "a wait is the job's one question, never beside a held call (RecordAsk takes one per job, " +
			"and a wait holds no call), and the held call, which the worst row sets, weighs more",
		"WaitOver": "the engine's carrying a run on claims only a run holding no answer (ClaimWaitOver), and an " +
			"answer recorded clears it (recordOutcome), so it is never beside the answer, which the worst row " +
			"sets and which weighs more",
	}
	// WHERE A CAPTURE'S RUN BEGAN is one of three (RunStart): a run that began
	// its session names no message it began after, nor a line count, and the
	// worst capture names a message at its bound, the widest of the three
	// (RunAfter), wider than a count at its own (MaxRunStartLines).
	for _, capture := range []string{"Capture", "Kept", "Saving"} {
		zero[capture+".RunBegan"] = "a run that began its session names no message it began after, which the " +
			"worst capture names at its bound, the wider of the two (RunStart, keptRunStart)"
		zero[capture+".RunAfterLines"] = "a run that began after a message names no line count (RunStart.Known), " +
			"and its id at its bound writes wider than a count at MaxRunStartLines"
		zero[capture+".RunUnshadowed"] = "only a Gemini CLI run's box says it (SessionArchive.Unshadowed), whose " +
			"start is a line count, never a message, and the message at its bound writes wider than the count " +
			"and the mark together"
	}
	// WHAT A SAVE NEVER IS ([PendingRun.Saving]): the store reserves none that
	// says otherwise (reservable), and no read stands by for one.
	zero["Saving.Final"] = "a save is of a job still working, and its reservation refuses a final one (reservable)"
	zero["Saving.NoteCursor"] = "a save is cut off and holds no note, and its reservation refuses one that " +
		"says otherwise (reservable)"
	for _, field := range []string{"StandbyTaker", "StandbyTakerEpoch", "StandbyAt"} {
		zero["Saving."+field] = "the section names no read standing by: the reservation that writes it " +
			"refuses a save naming one (reservable), and the save's later stamps are kept there without one " +
			"(stampOnto, savingAfter)"
	}
	// THE CALL A PERSON APPROVED, on the held call and owed to the log
	// ([OwedCall]), as approvedCall keeps it from a call with every field set
	// ([everyField]).
	for field, why := range map[string]string{
		"Args": "its arguments ride the row sealed on the held call, and neither it nor the log keeps any of " +
			"a withheld call's (approvedCall, keptCall, ADR-0077)",
		"ArgsBytes": "nor their size (keptCall)",
		"Digest": "nor a digest of them, which would confirm a guess at what the seal hides (approvedCall, " +
			"keptCall)",
		"Output": "its result rides the row sealed on the held call (approvedCall)",
		"OutputBytes": "nor a size of it, which beside no output would read as a whole output cut to nothing " +
			"(approvedCall)",
		"CallID": "no harness named it: the resume made it on the engine host (approvedCall)",
		"Held":   "it was made, so it is marked approved and never held (approvedCall)",
		"ArgsUnparsed": "its arguments were sealed and approved, so they parsed, and the log keeps none of a " +
			"withheld call's to read (approvedCall)",
		"Seq":       "it has no place until the log takes it and places it (approvedCall, BridgeLog.withApproved)",
		"Appended":  "nor a number, which the log gives it as it takes it (approvedCall, BridgeLog.appendCall)",
		"Late":      "the store appends it for its own round, so it is never late (approvedCall)",
		"Abandoned": "no harness gave it up: the resume made it on the engine host (approvedCall)",
		"Runner":    "nobody waits for a finished call, so it keeps no runner (keptCall)",
		"Until":     "nor a bound to wait for it until (keptCall)",
	} {
		zero["HeldCall.Call."+field], zero["OwedCall.Call."+field] = why, why
	}
	return zero
}()

// THE WORST ROW SETS EVERY FIELD THE ROW CARRIES. TestTheWholeRowFitsItsBudget
// fails only on a row past its shares; a field [worstRow] leaves out only
// shrinks the measured row. So every field the row's encoder writes is set in
// the worst row ([zeroOnTheWire]) except those [zeroInTheWorstRow] names, each
// with why the row never carries it at its worst; each is zero there, so a
// stale reason is struck, not left to hide a field.
// Mutation: leave the job's record without how its harness ended, a note
// without its job, the held call without its delegates, or the refusal's
// address with a key and no value, and a field is unnamed; name a field the
// worst row sets, and the reason fails; let approvedCall keep the held mark or
// output size, or keptCall a runner, and a named reason fails; widen the panel
// (panel-widened) or narrow it (panel-of-one), and it is not at its worst.
func TestTheWorstRowSetsEveryField(t *testing.T) {
	t.Parallel()
	everyFieldSet(t, "row", worstRow(t), zeroInTheWorstRow)
}

// worstLeadsPart is a run's leads part with every field at its bound: its
// leads at their share, numbered at their widest ([worstLeads]), the run it is
// of a launch id as the create names it, and how many leads it has taken at
// the widest that count can be.
func worstLeadsPart(t *testing.T) leadsPart {
	t.Helper()
	return leadsPart{Run: uuid.NewString(), Leads: worstLeads(t, *worstRow(t).HeldCall), Appends: math.MaxInt}
}

// THE WHOLE LEADS PART FITS ITS BUDGET: the turn's leads are a value of their
// own beside the row ([coord.RunLeads]), held to [RowBytes]. At their worst they
// weigh exactly [MaxKeptLeadBytes], at most [MaxKeptLeads] of them, and the
// whole part no more than [RowBytes]; the rest is room for a later build's keys.
// AND THE TABLE'S FIGURES (leads.go) ARE THE PART'S: its own fields
// ([measuredLeadsFields]) and the spare ([measuredLeadsSpare]) are compared
// exactly, as the row's are (TestTheWholeRowFitsItsBudget), since the worst
// part weighs the same every run (new ids, fixed width).
//
// Mutation: raise MaxKeptLeadBytes past what RowBytes leaves, and the part
// overruns; write a key a byte longer, and its fields miss the table's figure;
// move RowBytes, and the spare does.
func TestTheWholeLeadsPartFitsItsBudget(t *testing.T) {
	t.Parallel()
	part := worstLeadsPart(t)
	whole, err := json.Marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	leads, err := json.Marshal(part.Leads)
	if err != nil {
		t.Fatal(err)
	}
	fields := len(whole) - len(leads)
	spare := RowBytes - MaxKeptLeadBytes - fields
	t.Logf("the leads part weighs %d of the %d a value may: its %d leads %d of their %d, its own fields %d, "+
		"and %d to spare", len(whole), RowBytes, len(part.Leads), len(leads), MaxKeptLeadBytes, fields, spare)
	if len(leads) != MaxKeptLeadBytes || len(part.Leads) > MaxKeptLeads {
		t.Errorf("the worst part's %d leads weigh %d, want exactly their share %d and at most %d of them",
			len(part.Leads), len(leads), MaxKeptLeadBytes, MaxKeptLeads)
	}
	if len(whole) > RowBytes {
		t.Errorf("the leads part weighs %d, past the %d a value may", len(whole), RowBytes)
	}
	if fields != measuredLeadsFields || spare != measuredLeadsSpare {
		t.Errorf("the part's own fields weigh %d with %d to spare, and leads.go states %d with %d: restate "+
			"measuredLeadsFields, measuredLeadsSpare and the lines of the part's table that state them", fields,
			spare, measuredLeadsFields, measuredLeadsSpare)
	}
}

// THE WORST LEADS PART SETS EVERY FIELD THE PART CARRIES, for the reason the
// worst row does (TestTheWorstRowSetsEveryField): a field [worstLeadsPart]
// leaves zero is one TestTheWholeLeadsPartFitsItsBudget does not weigh at its
// widest, and the part's figures would be smaller than any part the store can
// hold.
//
// Mutation: leave the worst leads unnumbered, and the part has a field nothing
// names.
func TestTheWorstLeadsPartSetsEveryField(t *testing.T) {
	t.Parallel()
	everyFieldSet(t, "leads part", worstLeadsPart(t), nil)
}

// THE WHOLE LOG FITS ITS BUDGET: the log is a value of its own beside the row,
// held to [RowBytes] as the row is, its calls to their share of it. 200 worst
// calls with both tallies at their shares weigh no more than [BridgeLogBytes],
// and the whole log, with the most prior names and widest identifiers, no more
// than [RowBytes]; the rest is room for a later build's keys. Its calls carry
// every kind of change, and its union holds every kind. AND [MaxCallsInFlight]
// ENTRIES IN FLIGHT at their widest weigh no more than [InFlightBytes], inside
// the whole: a call's write-ahead bound measured beside every other field.
// Mutation: keep a call's output whole, or raise MaxBridgeCalls, and the calls
// overrun; raise MaxPriorCalls far enough, and the log does; stop the append
// folding kinds into the union, and the log is not at its worst; keep an
// entry's runner whole (entry-keeps-its-runner-whole), and the entries overrun.
func TestTheWholeLogFitsItsBudget(t *testing.T) {
	t.Parallel()
	bridged := worstLog(t)
	whole := logEncoded(t, bridged)
	withoutCalls := bridged
	withoutCalls.Calls, withoutCalls.Elided, withoutCalls.ElidedCalls, withoutCalls.AbsorbedCalls = nil, 0, nil, nil
	withoutInFlight := withoutCalls
	withoutInFlight.InFlight = nil
	others := logEncoded(t, withoutInFlight)
	inFlight := logEncoded(t, withoutCalls) - others
	calls := whole - others - inFlight
	t.Logf("the log weighs %d of the %d a value may: its calls and their tallies %d of %d, its %d calls in "+
		"flight %d of %d, and every other field %d — the turn's earlier rounds' tool names among them — with "+
		"%d left for a later build", whole, RowBytes, calls, BridgeLogBytes, MaxCallsInFlight, inFlight,
		InFlightBytes, others, RowBytes-whole)

	if calls > BridgeLogBytes {
		t.Errorf("the log's calls weigh %d, past their share %d", calls, BridgeLogBytes)
	}
	if inFlight > InFlightBytes {
		t.Errorf("the log's %d calls in flight weigh %d, past their line %d", MaxCallsInFlight, inFlight,
			InFlightBytes)
	}
	if whole > RowBytes {
		t.Errorf("the log weighs %d, past the %d a value may", whole, RowBytes)
	}
	// EVERY DROPPED CALL COUNTED, the tally folded as far as its share made it
	// fold: names that escape the most leave none of them a tool, so the
	// record is incomplete for a reader of whether the turn replied. And the
	// calls the log's calls made, each counted.
	if len(bridged.Calls) != MaxBridgeCalls || bridged.Elided != elidedWorst ||
		elidedCount(bridged.Unlisted()) != elidedWorst || bridged.ElisionKnown() {
		t.Errorf("the log keeps %d calls with %d dropped and %d counted (known %v), want %d with %d "+
			"counted and the elision unknown", len(bridged.Calls), bridged.Elided,
			elidedCount(bridged.Unlisted()), bridged.ElisionKnown(), MaxBridgeCalls, elidedWorst)
	}
	if bridged.LateElided != elidedWorst {
		t.Errorf("the log counts %d of its %d dropped calls late, want every one: the count is measured at "+
			"the most it can be", bridged.LateElided, elidedWorst)
	}
	if len(bridged.Wrote) != types.MaxWrittenRecords || !bridged.WroteMore {
		t.Errorf("the log's written set holds %d records (full %v), want its bound and the mark: the set is "+
			"measured at its most", len(bridged.Wrote), bridged.WroteMore)
	}
	if !slices.Equal(bridged.Committed, everyKind()) {
		t.Errorf("the log's union of what its calls committed holds %d kinds, want every one of the %d: the "+
			"union is measured at its most", len(bridged.Committed), len(types.CommittedChanges))
	}
	if n := elidedCount(bridged.Absorbed()); n != MaxBridgeCalls+elidedWorst {
		t.Errorf("the calls' own work is counted %d times, want %d", n, MaxBridgeCalls+elidedWorst)
	}
	if n := tallyBytes(bridged.AbsorbedCalls, absorbedTally); n > MaxAbsorbedIdentityBytes {
		t.Errorf("the absorbed tally weighs %d, past its %d", n, MaxAbsorbedIdentityBytes)
	}
}

// zeroInTheWorstLog is [zeroInTheWorstRow] for the log beside the row
// ([worstLog]).
var zeroInTheWorstLog = func() map[string]string {
	withheld := "the worst call is neither held nor approved: a withheld call keeps none of its arguments " +
		"(BridgeCall.Withheld), so it weighs less (worstCall)"
	finished := "nobody waits for a finished call, so the log keeps it with neither the runner nor the bound " +
		"it was handed (keptCall, on every append)"
	folded := "the worst log's dropped calls are named in the bytes that escape the most, so the tally of " +
		"them folds to its entry with no name, which sorts first (fold): the elision TestTheWholeLogFitsItsBudget " +
		"reads as unknown, with the tally's bound held by the fold and counted at its share in rowbytes.go"
	absorbed := "the calls' own work is tallied the same way, and " + folded
	zero := map[string]string{
		"Calls[0].Held":     withheld,
		"Calls[0].Approved": withheld,
		"Calls[0].ArgsUnparsed": "the worst call carries its arguments, and a call whose arguments did not " +
			"parse carries none (BridgeCall.ArgsUnparsed), so it weighs less (worstCall)",
		"Calls[0].Runner":         finished,
		"Calls[0].Until":          finished,
		"ElidedCalls[0].Name":     folded,
		"ElidedCalls[0].Digest":   folded,
		"AbsorbedCalls[0].Name":   absorbed,
		"AbsorbedCalls[0].Digest": absorbed,
		"AbsorbedCalls[0].Failed": "the log takes a call's own work in by its tool and identity alone, never " +
			"its failures (appendCall)",
	}
	// AN ENTRY IN FLIGHT, as keptEntry keeps it from a call with every field
	// set ([worstEntry]).
	for _, field := range []string{"Args", "ArgsBytes", "ArgsUnparsed", "Output", "OutputBytes", "Failed",
		"Appended", "Late", "Abandoned", "DurationMS", "Origin", "Server", "Source", "Refusal", "Held", "Approved",
		"HeldID", "Committed"} {
		zero["InFlight[0]."+field] = "an entry keeps its call's identity alone (keptEntry), and its one update " +
			"brings the rest"
	}
	return zero
}()

// THE WORST LOG SETS EVERY FIELD THE LOG CARRIES, for the reason the worst row
// does (TestTheWorstRowSetsEveryField): the log beside the row is measured by a
// model of its own ([worstLog]) under the same rule, which nothing held it to
// either. Its calls and its entries in flight are walked as the log keeps
// them: each entry kept from a call with every field set, and each call
// appended with a runner and a bound and its own work with its failures, so a
// field either leaves zero is one keptEntry or the append leaves out.
//
// Mutation: leave the log without its mark that the earlier rounds' tool names
// are incomplete, and the log has a field nothing names; let keptEntry keep a
// call's origin, keptCall a finished call's runner, or the append the failures
// of a call's own work, and the list names a field the worst log sets.
func TestTheWorstLogSetsEveryField(t *testing.T) {
	t.Parallel()
	everyFieldSet(t, "log", worstLog(t), zeroInTheWorstLog)
}

// logEncoded is what log weighs as the store's encoder writes it.
func logEncoded(t *testing.T, bridged BridgeLog) int {
	t.Helper()
	raw, err := json.Marshal(bridged)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw)
}

// encoded is what run weighs as the row's encoder writes it.
func encoded(t *testing.T, run PendingRun) int {
	t.Helper()
	raw, err := encodeRun(run)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw)
}

// everyFieldSet fails t on every field the worst value of a model leaves zero
// ([zeroOnTheWire]) that named does not name, and on every field named names
// that the value does not leave zero: a reason that stopped holding, struck
// rather than left to hide the field it names.
func everyFieldSet(t *testing.T, model string, v any, named map[string]string) {
	t.Helper()
	zero := zeroOnTheWire(reflect.ValueOf(v), "")
	for _, path := range zero {
		if _, ok := named[path]; !ok {
			t.Errorf("the worst %s leaves %s zero: set it at its bound in the model, in the change that adds "+
				"it, or name it with why the %s never carries it beside the rest at their worst", model, path,
				model)
		}
	}
	for _, path := range slices.Sorted(maps.Keys(named)) {
		if !slices.Contains(zero, path) {
			t.Errorf("the worst %s does not leave %s zero, which is named as a field it does: strike the name, "+
				"whose reason no longer holds or which names no field", model, path)
		}
	}
}

// zeroOnTheWire is the path of every field of v the store's encoder writes that
// v leaves at its zero value: every exported field whose JSON key is not "-",
// followed through pointers, structs, the first element of each slice and every
// value of each map, and named as Go names it (`HeldCall.Call.Args`,
// `Steer.Notes[0].Read`, `AnswerRefusal.Address[transport]`). A nil pointer, an
// empty slice or map and a zero value are one path each, all of what is under
// them being left out; a struct with no field of its own on the wire (an
// instant) and a run of bytes are one value each.
//
// A FIELD A MODEL LEAVES ZERO IS A FIELD IT LEAVES OUT: the value it measures is
// smaller than the one the store can hold by that field's weight, and no share
// is past its bound for it, so only a walk of the type finds it.
func zeroOnTheWire(v reflect.Value, path string) []string {
	named := func(field string) string {
		if path == "" {
			return field
		}
		return path + "." + field
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return []string{path}
		}
		return zeroOnTheWire(v.Elem(), path)
	case reflect.Struct:
		var zero []string
		carried := false
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if key, _, _ := strings.Cut(field.Tag.Get("json"), ","); !field.IsExported() || key == "-" {
				continue
			}
			carried = true
			zero = append(zero, zeroOnTheWire(v.Field(i), named(field.Name))...)
		}
		if !carried && v.IsZero() {
			return []string{path}
		}
		return zero
	case reflect.Slice:
		switch {
		case v.Len() == 0:
			return []string{path}
		case v.Type().Elem().Kind() == reflect.Uint8:
			return nil
		}
		return zeroOnTheWire(v.Index(0), path+"[0]")
	case reflect.Map:
		if v.Len() == 0 {
			return []string{path}
		}
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return cmp.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
		var zero []string
		for _, key := range keys {
			zero = append(zero, zeroOnTheWire(v.MapIndex(key), fmt.Sprintf("%s[%v]", path, key))...)
		}
		return zero
	}
	if v.IsZero() {
		return []string{path}
	}
	return nil
}

// AN ANSWER BY TURN AT ITS SOURCE'S BOUND FITS A RECORDED ANSWER'S SHARE. An
// answer by turn has no ordinary message to fall back on, so one the row could
// not carry would be one a person was told was taken and the run never was:
// `answer_executor` and the record both hold an answer to a page
// ([MaxAnswerTextBytes], which is MaxAskContextBytes), and that page at its
// worst — every byte one the encoder escapes six-fold, sealed with the longest
// names, and carried again as the delivery it came in — fits [MaxAnswerBytes],
// as the row holds it and as the copy a let-go of it would owe.
//
// Mutation: raise MaxAnswerTextBytes past what a recorded answer's share
// holds, or shrink the share under it, and this goes red.
func TestAnAnswerByTurnAtItsSourcesBoundFitsARecordedAnswersShare(t *testing.T) {
	t.Parallel()
	run := PendingRun{TurnID: strings.Repeat("t", 64), LaunchID: uuid.NewString()}
	given := types.ExecutorAnswerGiven{TurnID: run.TurnID, LaunchID: run.LaunchID,
		Answer: strings.Repeat("<", MaxAnswerTextBytes), AnsweredBy: strings.Repeat("<", 128),
		AnsweredByKind: strings.Repeat("k", 32), OperatorID: strings.Repeat("o", 64)}
	trigger := events.New(given, events.TraceContext{})
	words, answer, err := answerGivenOf(run, given, trigger, worstAt)
	if err != nil {
		t.Fatal(err)
	}
	answer = sealedOnto(t, rigAnswers(), run, words, answer)
	if err := answerFits(run.TurnID, answer); err != nil {
		t.Errorf("an answer by turn at its bound does not fit a recorded answer's share: %v", err)
	}
	raw, _ := json.Marshal(answer)
	t.Logf("an answer by turn at its bound weighs %d of the %d a recorded answer may", len(raw), MaxAnswerBytes)
}

// AN ANSWER IS MEASURED AS THE ROW WILL HOLD IT. The record and the steps after
// it write onto a recorded answer the status it took the run out of and the
// claims that take it or lose it — when it was taken, when it was first lost
// and how many times — none of which the reply handed over carries: so the
// share is judged with each of them at its widest. An answer at its share with
// all four written fits it; one at its share without any ONE of them is past
// it once the row writes that one, and is refused before it is recorded.
//
// Mutation: measure the answer without any one of the four, and an answer at
// its share without that one is recorded, to outgrow the share when the step
// that writes it lands.
func TestAnAnswerIsMeasuredAsTheRowWillHoldIt(t *testing.T) {
	t.Parallel()
	pad := func(a *RecordedAnswer, pad string) { a.Events = []json.RawMessage{json.RawMessage(`"` + pad + `"`)} }
	handed := RecordedAnswer{Sealed: []byte("words"), Via: types.AnswerViaChat, EventIDs: []string{"e1"},
		PostedAt: worstAt, RecordedAt: worstAt}
	// EACH AT ITS WIDEST: the longest status an answer takes a run out of, an
	// instant at its widest and the largest count.
	written := func() RecordedAnswer {
		a := handed
		a.From = slices.MaxFunc(Awaiting, func(x, y string) int { return cmp.Compare(len(x), len(y)) })
		a.TakenAt, a.FirstLostAt, a.LostClaims = widestInstant, widestInstant, math.MaxInt
		return a
	}
	if err := answerFits("t1", paddedTo(t, MaxAnswerBytes, written(), pad)); err != nil {
		t.Errorf("an answer at its share with every one of them written = %v, want it to fit", err)
	}
	for name, unset := range map[string]func(*RecordedAnswer){
		"the status it took the run out of": func(a *RecordedAnswer) { a.From = "" },
		"when it was taken":                 func(a *RecordedAnswer) { a.TakenAt = time.Time{} },
		"when it was first lost":            func(a *RecordedAnswer) { a.FirstLostAt = time.Time{} },
		"how many times it was lost":        func(a *RecordedAnswer) { a.LostClaims = 0 },
	} {
		short := written()
		unset(&short)
		if err := answerFits("t1", paddedTo(t, MaxAnswerBytes, short, pad)); !errors.Is(err, ErrAnswerTooLarge) {
			t.Errorf("an answer at its share without %s = %v, want ErrAnswerTooLarge: the row writes it later",
				name, err)
		}
	}
}

// worstCount is a count of a session at its widest, as a record carries one
// ([ResumedCount]): a figure and a window of nineteen digits each, a mark at its
// bound ([maxCountMarkBytes]), which the engine's own reading words, and a
// restart at a uuid's thirty-six bytes ([RestartIDMaxBytes]) in the bytes that
// escape the most, since the box's thread writes it as it likes and the layer
// hands it on as written; a record refuses or rewrites either past its bound.
func worstCount() *ResumedCount {
	return &ResumedCount{Tokens: math.MaxInt64, Mark: CountMark(strings.Repeat("a", maxCountMarkBytes)),
		Window: math.MaxInt64, Restart: new(strings.Repeat("<", RestartIDMaxBytes))}
}
