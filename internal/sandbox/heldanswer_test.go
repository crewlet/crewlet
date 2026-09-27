package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// heldRef is the reference to the parts the answer held on a run is kept in,
// failing the test when the run holds none.
func heldRef(t *testing.T, store *sandbox.CoordStore, turnID string) sandbox.HeldParts {
	t.Helper()
	run, _, err := store.Get(t.Context(), turnID)
	if err != nil {
		t.Fatalf("Get(%s): %v", turnID, err)
	}
	answer, ok := run.Held()
	if !ok || answer.Parts == nil {
		t.Fatalf("the premise: run %s holds its answer in parts (it holds %+v, held=%v)", turnID, answer, ok)
	}
	return *answer.Parts
}

// held reads the answer held on a run whole, failing the test on an error.
func held(t *testing.T, store *sandbox.CoordStore, turnID string) sandbox.HeldAnswer {
	t.Helper()
	run, _, err := store.Get(t.Context(), turnID)
	if err != nil {
		t.Fatalf("Get(%s): %v", turnID, err)
	}
	answer, ok, err := store.Answer(t.Context(), run)
	if err != nil || !ok {
		t.Fatalf("Answer(%s) = %v, %v", turnID, ok, err)
	}
	return answer
}

// A REPLY THE ROW'S RECORD REFUSES IS HELD IN PARTS THE SERVER TAKES.
//
// Within the bound a row holds an answer within, and refused all the same: a
// server set below the contract's ceiling does that, and the refusal is
// permanent, so handing it back would have its redelivery refused the same way
// until it was dead-lettered. Its whole goes to parts split until that server
// takes them, and reads back whole.
func TestAReplyTheRowRefusesIsHeldInPartsTheServerTakes(t *testing.T) {
	t.Parallel()
	const limit = 100 << 10
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(lowRunServer{Fleet: fleet, limit: limit})
	run := parked(t, store, "t-low")
	reply := strings.Repeat("r", 150<<10)
	at := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)

	landed, err := store.HoldAnswer(t.Context(), "t-low", sandbox.HeldAnswer{Launch: run.LaunchID, Text: reply, At: at})
	if err != nil || !landed {
		t.Fatalf("HoldAnswer = %v, %v, want the reply held in parts", landed, err)
	}
	if got := held(t, store, "t-low"); got.Text != reply || !got.At.Equal(at) {
		t.Fatalf("the answer read back is %d bytes held at %v, want the whole %d-byte reply", len(got.Text),
			got.At, len(reply))
	}
	parts, err := fleet.AnswerParts(t.Context(), "t-low", run.LaunchID, heldRef(t, store, "t-low").ID)
	if err != nil || len(parts) < 2 {
		t.Fatalf("the reply is in %d parts, %v: want it split to what the server takes", len(parts), err)
	}
	for _, part := range parts {
		if len(part.Value) > limit {
			t.Errorf("part %d is %d bytes, past the %d the server takes", part.Part, len(part.Value), limit)
		}
	}
}

// A REFERENCE NAMING NO ANSWER IS UNREADABLE, not a store to retry.
//
// Its parts are filed under the answer's id, and one with no id names parts no
// address can hold. Asked of the store it is refused as an address, which
// reads as a failed read: the resume would hand its claim back and meet the
// same refusal on every signal, for as long as the budget had room.
func TestAReferenceNamingNoAnswerIsUnreadable(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{
		TurnID: "t1", LaunchID: "l-1",
		HeldAnswer: &sandbox.HeldAnswer{Launch: "l-1", Parts: &sandbox.HeldParts{Parts: 1, Bytes: 9}},
	}
	_, ok, err := sandbox.NewCoordStore(memory.NewFleet()).Answer(t.Context(), run)
	if !ok || !errors.Is(err, sandbox.ErrAnswerUnreadable) {
		t.Errorf("Answer = %v, %v, want ErrAnswerUnreadable", ok, err)
	}
}

// AN ANSWER HELD IN PARTS IS A WHOLE OF ITS OWN, beside a suspension in parts.
//
// Both are the launch's and both are filed under it, and each starts at part 1
// of its own address: the answer is filed among none of the suspension's
// parts, the suspension's are read without the answer's, and each reads back
// whole — the reply the person gave, and the conversation the resume
// re-enters.
func TestAnAnswerHeldInPartsIsAWholeOfItsOwn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", ConversationKey: "chat:C1",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	// Past the half of a record the row keeps a suspension within.
	state := map[string]any{"version": 2, "messages": strings.Repeat("s", coord.MaxRecordBytes/2+64<<10)}
	if ok, err := store.MarkSuspended(ctx, "t1", state); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	if err := store.MarkAwaiting(ctx, "t1", sandbox.Clarification{Question: "which branch?"}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	run, _, err := store.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	before, err := fleet.SuspensionParts(ctx, "t1", run.LaunchID)
	if err != nil || len(before) == 0 {
		t.Fatalf("the premise: the suspension is in parts (%d, %v)", len(before), err)
	}

	reply := strings.Repeat("r", sandbox.MaxHeldAnswerBytes)
	if landed, err := store.HoldAnswer(ctx, "t1", sandbox.HeldAnswer{Launch: run.LaunchID, Text: reply}); err != nil || !landed {
		t.Fatalf("HoldAnswer = %v, %v", landed, err)
	}
	ref := heldRef(t, store, "t1")
	parts, err := fleet.AnswerParts(ctx, "t1", run.LaunchID, ref.ID)
	if err != nil || len(parts) != ref.Parts || parts[0].Part != 1 {
		t.Fatalf("the answer's parts are %d (%v), want the %d its reference names, from part 1",
			len(parts), err, ref.Parts)
	}
	after, err := fleet.SuspensionParts(ctx, "t1", run.LaunchID)
	if err != nil || len(after) != len(before) {
		t.Errorf("the suspension holds %d parts after the hold (%v), want its own %d", len(after), err, len(before))
	}
	if got := held(t, store, "t1"); got.Text != reply {
		t.Errorf("the answer read back is %d bytes, want the whole %d-byte reply", len(got.Text), len(reply))
	}
	run, _, err = store.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, err := store.Suspension(ctx, run); err != nil || !sameJSON(t, got, state) {
		t.Errorf("the suspension read back is not the one suspended (%v)", err)
	}
}

// A REPLY THE RUN CAN NO LONGER TAKE FILES NO PARTS.
//
// The row is read before any part is filed, so a reply that arrives after the
// run was claimed — or relaunched, ended or answered — costs no write at all;
// filed first, its parts would sit named by nothing until the launch was
// purged, megabytes at a time.
func TestAReplyTheRunCanNoLongerTakeFilesNoParts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := &answerPartsCounted{Fleet: memory.NewFleet()}
	store := sandbox.NewCoordStore(fleet)
	run := parked(t, store, "t1")
	if _, won, err := store.ClaimForResume(ctx, "t1", sandbox.AnswerTail(run.LaunchID)); err != nil || !won {
		t.Fatalf("the premise: another reply claimed the run (%v, %v)", won, err)
	}

	landed, err := store.HoldAnswer(ctx, "t1", sandbox.HeldAnswer{
		Launch: run.LaunchID, Text: strings.Repeat("r", sandbox.MaxHeldAnswerBytes),
	})
	if err != nil || landed {
		t.Fatalf("HoldAnswer = %v, %v, want a run no longer waiting to hold nothing", landed, err)
	}
	if n := fleet.filed.Load(); n != 0 {
		t.Errorf("%d parts were filed for a reply the run could no longer take", n)
	}
}

// A HOLD AFTER ONE WHOSE RECORD DID NOT LAND STILL LANDS.
//
// A hold files its parts, then writes the row that names them; a store that
// fails between the two leaves parts no row names, and the reply's delivery is
// handed back and comes again. Each hold files under an id of its own, so the
// redelivery's hold files a whole of its own beside the first one's parts
// rather than meeting them at its first address and failing there on every
// redelivery until the reply was dead-lettered.
func TestAHoldAfterOneWhoseRecordDidNotLandStillLands(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := &rowWriteFailsOnce{Fleet: memory.NewFleet()}
	store := sandbox.NewCoordStore(fleet)
	run := parked(t, store, "t1")
	reply := sandbox.HeldAnswer{Launch: run.LaunchID, Text: strings.Repeat("r", sandbox.MaxHeldAnswerBytes)}

	fleet.armed.Store(true)
	if landed, err := store.HoldAnswer(ctx, "t1", reply); err == nil || landed {
		t.Fatalf("the premise: the first hold's record did not land (%v, %v)", landed, err)
	}
	if landed, err := store.HoldAnswer(ctx, "t1", reply); err != nil || !landed {
		t.Fatalf("the redelivered reply's hold = %v, %v, want it held", landed, err)
	}
	if got := held(t, store, "t1"); got.Text != reply.Text {
		t.Errorf("the answer read back is %d bytes, want the whole %d-byte reply", len(got.Text), len(reply.Text))
	}
}

// rowWriteFailsOnce refuses the first update of a run's record once armed, as a
// store that cannot be reached for one write does.
type rowWriteFailsOnce struct {
	*memory.Fleet
	armed, failed atomic.Bool
}

func (r *rowWriteFailsOnce) UpdateSandboxRun(ctx context.Context, turnID string, value []byte, version uint64) (bool, error) {
	if r.armed.Load() && r.failed.CompareAndSwap(false, true) {
		return false, fmt.Errorf("the store could not be reached: %w", coord.ErrUnavailable)
	}
	return r.Fleet.UpdateSandboxRun(ctx, turnID, value, version)
}

// answerPartsCounted counts the parts of held answers filed through it.
type answerPartsCounted struct {
	*memory.Fleet
	filed atomic.Int64
}

func (a *answerPartsCounted) CreateAnswerPart(ctx context.Context, turnID, launchID, answerID string, part int, value []byte) (bool, error) {
	a.filed.Add(1)
	return a.Fleet.CreateAnswerPart(ctx, turnID, launchID, answerID, part, value)
}

// A RELAUNCH DROPS AN ANSWER HELD IN PARTS, AND ITS PARTS. The answer is the
// replaced launch's and answers a call the new job has not made, and its parts
// are filed under that launch, whose purge takes them.
func TestARelaunchDropsAnAnswerHeldInPartsAndItsParts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	run := parked(t, store, "t1")
	if landed, err := store.HoldAnswer(ctx, "t1", sandbox.HeldAnswer{
		Launch: run.LaunchID, Text: strings.Repeat("r", sandbox.MaxHeldAnswerBytes),
	}); err != nil || !landed {
		t.Fatalf("HoldAnswer = %v, %v", landed, err)
	}
	ref := heldRef(t, store, "t1")
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "t1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	after, _, err := store.Get(ctx, "t1")
	if err != nil || after.HeldAnswer != nil {
		t.Errorf("the relaunched row holds %+v (%v), want none", after.HeldAnswer, err)
	}
	if parts, err := fleet.AnswerParts(ctx, "t1", run.LaunchID, ref.ID); err != nil || len(parts) != 0 {
		t.Errorf("the replaced launch still holds %d parts (%v)", len(parts), err)
	}
}

// A MEMBER A NEWER BUILD ADDED INSIDE AN OBJECT ON THE ROW SURVIVES EVERY WRITE.
//
// Every write is a read-modify-write of the whole row, and each object on it
// — the held answer, the reference to its parts, an entry of older builds'
// view of the calls — is decoded into this build's struct for it. A member
// that struct has no field for would be gone from the first write this build
// made, and the peer that wrote it would act as though it had never been
// written. Each write that leaves those objects on the row is walked here, and
// every one of them keeps the member, byte for byte.
func TestAMemberANewerBuildAddedInsideAnObjectSurvivesEveryWrite(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	run := parked(t, store, "t1")
	appended(t, store, "t1", "read_page")
	if landed, err := store.HoldAnswer(ctx, "t1", sandbox.HeldAnswer{
		Launch: run.LaunchID, Text: strings.Repeat("r", sandbox.MaxHeldAnswerBytes),
	}); err != nil || !landed {
		t.Fatalf("HoldAnswer = %v, %v", landed, err)
	}
	newer := json.RawMessage(`{"counted":true}`)
	nested := func(members map[string]json.RawMessage) map[string]json.RawMessage {
		out := map[string]json.RawMessage{}
		var answer, parts map[string]json.RawMessage
		if err := json.Unmarshal(members["held_answer"], &answer); err != nil {
			t.Fatalf("decode held_answer: %v", err)
		}
		if err := json.Unmarshal(answer["parts"], &parts); err != nil {
			t.Fatalf("decode held_answer.parts: %v", err)
		}
		out["held_answer"], out["held_answer.parts"] = answer["a_newer_builds_fact"], parts["a_newer_builds_fact"]
		var calls []map[string]json.RawMessage
		if err := json.Unmarshal(members["bridge_calls"], &calls); err != nil || len(calls) == 0 {
			t.Fatalf("decode bridge_calls (%d): %v", len(calls), err)
		}
		out["bridge_calls[0]"] = calls[0]["a_newer_builds_fact"]
		return out
	}
	members, version := rawMembers(t, fleet, "t1")
	var answer, parts map[string]json.RawMessage
	if err := json.Unmarshal(members["held_answer"], &answer); err != nil {
		t.Fatalf("decode held_answer: %v", err)
	}
	if err := json.Unmarshal(answer["parts"], &parts); err != nil {
		t.Fatalf("the premise: the held answer names its parts (%v)", err)
	}
	parts["a_newer_builds_fact"] = newer
	answer["parts"], _ = json.Marshal(parts)
	answer["a_newer_builds_fact"] = newer
	members["held_answer"], _ = json.Marshal(answer)
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(members["bridge_calls"], &calls); err != nil || len(calls) != 1 {
		t.Fatalf("the premise: the row's view holds one call (%d, %v)", len(calls), err)
	}
	calls[0]["a_newer_builds_fact"] = newer
	members["bridge_calls"], _ = json.Marshal(calls)
	writeMembers(t, fleet, "t1", members, version)

	var claimed sandbox.PendingRun
	for _, step := range []struct {
		name  string
		write func() error
	}{
		{"append a bridged call", func() error {
			_, err := store.AppendBridgeCall(ctx, "t1", sandbox.BridgeCall{Name: "search"})
			return err
		}},
		{"pause the box", func() error { return store.MarkBoxPaused(ctx, "t1", time.Now()) }},
		{"expire the pause", func() error {
			_, err := store.ExpirePause(ctx, "t1")
			return err
		}},
		{"take ownership", func() error {
			_, err := store.ClaimOwnership(ctx, "t1", "node-b:2", 3)
			return err
		}},
		{"claim the held answer", func() error {
			var err error
			claimed, _, err = store.ClaimForResume(ctx, "t1", sandbox.HeldTail(run.LaunchID))
			return err
		}},
		{"hand the claim back", func() error {
			_, err := store.ReleaseClaim(ctx, "t1", sandbox.Release{
				Launch: claimed.LaunchID, To: claimed.ClaimedFrom,
			})
			return err
		}},
		{"release the box", func() error { return store.ReleaseBox(ctx, "t1") }},
	} {
		if err := step.write(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		got, _ := rawMembers(t, fleet, "t1")
		for where, member := range nested(got) {
			if !bytes.Equal(member, newer) {
				t.Fatalf("%s left the newer build's member in %s as %s, want %s", step.name, where,
					member, newer)
			}
		}
	}
	if got := held(t, store, "t1"); len(got.Text) != sandbox.MaxHeldAnswerBytes {
		t.Errorf("the answer read back is %d bytes, want the whole reply", len(got.Text))
	}
}

// AN ANSWER'S PARTS THAT DO NOT MAKE ITS WHOLE ARE UNREADABLE, never a shorter
// reply passed off as the one given.
func TestAnAnswerWhosePartsAreShortIsUnreadable(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	run := parked(t, store, "t1")
	if landed, err := store.HoldAnswer(t.Context(), "t1", sandbox.HeldAnswer{
		Launch: run.LaunchID, Text: strings.Repeat("r", sandbox.MaxHeldAnswerBytes),
	}); err != nil || !landed {
		t.Fatalf("HoldAnswer = %v, %v", landed, err)
	}
	got, _, err := store.Get(t.Context(), "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.HeldAnswer.Parts.Bytes++
	if _, _, err := store.Answer(t.Context(), got); !errors.Is(err, sandbox.ErrAnswerUnreadable) {
		t.Errorf("Answer over parts one byte short = %v, want ErrAnswerUnreadable", err)
	}
}
