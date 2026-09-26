package sandbox_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

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
	parts, err := fleet.SuspensionParts(t.Context(), "t-low", run.LaunchID)
	if err != nil || len(parts) < 2 {
		t.Fatalf("the reply is in %d parts, %v: want it split to what the server takes", len(parts), err)
	}
	for _, part := range parts {
		if len(part.Value) > limit {
			t.Errorf("part %d is %d bytes, past the %d the server takes", part.Part, len(part.Value), limit)
		}
	}
}

// A REFERENCE NAMING ANOTHER ANSWER IS NOBODY'S. It names the answer it was
// written with, and a row holding a different one beside it — which a build
// that carries the reference without knowing it can leave — is answered with
// what the row holds.
func TestAReferenceNamingAnotherAnswerIsNobodys(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	run := sandbox.PendingRun{
		TurnID: "t1", LaunchID: "l-1",
		HeldAnswer:      &sandbox.HeldAnswer{Launch: "l-1", Text: "on the row", At: at},
		HeldAnswerParts: &sandbox.HeldParts{Launch: "l-1", At: at.Add(-time.Minute), First: 1, Parts: 1, Bytes: 9},
	}
	got, ok, err := sandbox.NewCoordStore(memory.NewFleet()).Answer(t.Context(), run)
	if err != nil || !ok || got.Text != "on the row" {
		t.Errorf("Answer = %+v, %v, %v, want the answer the row holds", got, ok, err)
	}
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
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "t1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	after, _, err := store.Get(ctx, "t1")
	if err != nil || after.HeldAnswer != nil || after.HeldAnswerParts != nil {
		t.Errorf("the relaunched row holds %+v beside %+v (%v), want neither",
			after.HeldAnswer, after.HeldAnswerParts, err)
	}
	if parts, err := fleet.SuspensionParts(ctx, "t1", run.LaunchID); err != nil || len(parts) != 0 {
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
		for _, key := range []string{"held_answer", "held_answer_parts"} {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(members[key], &object); err != nil {
				t.Fatalf("decode %s: %v", key, err)
			}
			out[key] = object["a_newer_builds_fact"]
		}
		var calls []map[string]json.RawMessage
		if err := json.Unmarshal(members["bridge_calls"], &calls); err != nil || len(calls) == 0 {
			t.Fatalf("decode bridge_calls (%d): %v", len(calls), err)
		}
		out["bridge_calls[0]"] = calls[0]["a_newer_builds_fact"]
		return out
	}
	members, version := rawMembers(t, fleet, "t1")
	for _, key := range []string{"held_answer", "held_answer_parts"} {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(members[key], &object); err != nil {
			t.Fatalf("decode %s: %v", key, err)
		}
		object["a_newer_builds_fact"] = newer
		members[key], _ = json.Marshal(object)
	}
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

// EVERY OBJECT ON A RUN'S ROW CARRIES WHAT IT DOES NOT KNOW.
//
// The carry is per object, so an object added to the row without it is one a
// newer build's member inside is erased from by this build's first write — and
// nothing else would say so. Every struct reachable from the row's type is
// held to the shape the carry takes: its own MarshalJSON and UnmarshalJSON, and
// an Extra field the encoder skips. A time is a JSON string rather than an
// object, and is the one struct that is not.
func TestEveryObjectOnARunsRowCarriesWhatItDoesNotKnow(t *testing.T) {
	t.Parallel()
	marshaler := reflect.TypeFor[json.Marshaler]()
	unmarshaler := reflect.TypeFor[json.Unmarshaler]()
	extra := reflect.TypeFor[map[string]json.RawMessage]()
	seen := map[reflect.Type]bool{}
	var walk func(t2 reflect.Type, at string)
	walk = func(typ reflect.Type, at string) {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(typ.Elem(), at)
			return
		case reflect.Struct:
		default:
			return
		}
		if typ == reflect.TypeFor[time.Time]() || seen[typ] {
			return
		}
		seen[typ] = true
		if !typ.Implements(marshaler) && !reflect.PointerTo(typ).Implements(marshaler) ||
			!reflect.PointerTo(typ).Implements(unmarshaler) {
			t.Errorf("%s (%s) encodes without the carry: it has no MarshalJSON and UnmarshalJSON "+
				"of its own", typ, at)
		}
		field, ok := typ.FieldByName("Extra")
		if !ok || field.Type != extra || field.Tag.Get("json") != "-" {
			t.Errorf("%s (%s) has no Extra map[string]json.RawMessage the encoder skips", typ, at)
		}
		if typ.PkgPath() != reflect.TypeFor[sandbox.PendingRun]().PkgPath() {
			// Another package's object states its own wire rule; its shape
			// is checked, its fields are that package's.
			return
		}
		for i := range typ.NumField() {
			if f := typ.Field(i); f.IsExported() && f.Tag.Get("json") != "-" {
				walk(f.Type, at+"."+f.Name)
			}
		}
	}
	walk(reflect.TypeFor[sandbox.PendingRun](), "PendingRun")
	if !seen[reflect.TypeFor[sandbox.HeldParts]()] || !seen[reflect.TypeFor[sandbox.BridgeCall]()] {
		t.Fatal("the walk did not reach the objects the row is known to hold")
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
	got.HeldAnswerParts.Bytes++
	if _, _, err := store.Answer(t.Context(), got); !errors.Is(err, sandbox.ErrAnswerUnreadable) {
		t.Errorf("Answer over parts one byte short = %v, want ErrAnswerUnreadable", err)
	}
}
