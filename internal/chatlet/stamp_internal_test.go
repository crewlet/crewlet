package chatlet

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// novelFields stands in for a field a later build adds to a chat record, so the
// stamping rule can be exercised while the production table is still empty.
var novelFields = statelog.RecordFields{
	{Name: "MessagePost.Novel", Since: 2, Op: string(OpPost), Path: []string{"mutation", "novel"}},
}

func stampRecord(v int, op OpKind, mutation string) MutationRecord {
	return MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: v, OpID: "op-stamp", Subject: MessageSubject("room-stamp"),
			Op: op, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Scope: ScopeSet{Subject: true},
		},
		Mutation: []byte(mutation), Actor: "ana", ActorKind: AuthorAgent,
	}
}

func stampOf(t *testing.T, body []byte) int {
	t.Helper()
	env, err := DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("decode the envelope: %v", err)
	}
	return env.V
}

// CHAT STAMPS ON THE TRACKER'S RULE: the lowest version that reads what a
// record carries, a set version below that refused, and the base version for
// everything that carries nothing new — under a stand-in table as much as
// through the production encoder, whose table is empty.
//
// A WRITER LEAVES THE VERSION UNSET, and the encoder stamps it. A record that
// went out at version 0 instead would be refused by every node's envelope
// decode — which is what every chat record did before this encoder learned to
// stamp.
func TestAChatRecordIsStampedWithTheLowestVersionThatReadsIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		op       OpKind
		mutation string
		want     int
	}{
		"a post carrying nothing new":           {OpPost, `{"body":"x"}`, 1},
		"a post carrying the new field":         {OpPost, `{"novel":0}`, 2},
		"another op carrying the same key":      {OpEdit, `{"novel":0}`, 1},
		"a post whose new field is set to null": {OpPost, `{"novel":null}`, 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body, err := encodeWith(stampRecord(0, tc.op, tc.mutation), novelFields)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got := stampOf(t, body); got != tc.want {
				t.Fatalf("stamped %d, want %d", got, tc.want)
			}
		})
	}

	if _, err := encodeWith(stampRecord(1, OpPost, `{"novel":1}`), novelFields); err == nil ||
		!strings.Contains(err.Error(), "MessagePost.Novel (version 2)") {
		t.Fatalf("a post stamped 1 carrying a version-2 field encoded: %v", err)
	}
	body, err := Encode(stampRecord(0, OpPost, `{"body":"x"}`))
	if err != nil {
		t.Fatalf("the production encoder: %v", err)
	}
	if got := stampOf(t, body); got != baseRecordVersion {
		t.Fatalf("the production encoder stamped %d, want %d", got, baseRecordVersion)
	}
}

// A RECORD EVERY BUILD MUST READ NEVER CARRIES A VERSIONED FIELD — a gate, the
// read index's barrier and a reanchor's generation, stamped or pinned.
//
// CHAT HAS THREE GATES, and two of them are by their OP on an ordinary room:
// the erase and the prune. Both delete rows, so an older node that deferred one
// would go on serving what every other node destroyed, with no inverse that
// repairs it — and a field either needs is refused at the encoder rather than
// stamped above what every build reads.
func TestARecordEveryBuildReadsRefusesAVersionedField(t *testing.T) {
	t.Parallel()
	everyOp := statelog.RecordFields{
		{Name: "Anything.Novel", Since: 2, Path: []string{"mutation", "novel"}},
	}
	for name, rec := range map[string]MutationRecord{
		"an eviction": {RecordEnvelope: RecordEnvelope{
			Subject: EvictionSubject("node-b"), Op: OpEviction}},
		"an erase": {RecordEnvelope: RecordEnvelope{
			Subject: ChannelSubject("room-stamp"), Op: OpErase}},
		"a prune": {RecordEnvelope: RecordEnvelope{
			Subject: ChannelSubject("room-stamp"), Op: OpPrune}},
		"a barrier": {RecordEnvelope: RecordEnvelope{
			V: baseRecordVersion, Subject: BarrierSubject(), Op: OpBarrier}},
		"a generation": {RecordEnvelope: RecordEnvelope{
			V: baseRecordVersion, Subject: GenerationSubject(2), Op: OpGeneration}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec.OpID = "op-pinned"
			rec.Scope = ScopeSet{Subject: true}
			rec.Mutation = []byte(`{"novel":1}`)
			_, err := encodeWith(rec, everyOp)
			if err == nil || !strings.Contains(err.Error(), "every build for ever") {
				t.Fatalf("%s carrying a versioned field encoded: %v", name, err)
			}
			// STAMPED ABOVE WHAT EVERY BUILD READS IS NO WAY ROUND IT.
			rec.V = 2
			if _, err := encodeWith(rec, everyOp); err == nil {
				t.Fatalf("%s set at version 2 carrying a versioned field encoded", name)
			}
			// AND CARRYING NOTHING NEW, it encodes at 1.
			rec.V, rec.Mutation = 0, []byte(`{}`)
			body, err := encodeWith(rec, everyOp)
			if err != nil {
				t.Fatalf("%s carrying nothing new: %v", name, err)
			}
			if got := stampOf(t, body); got != baseRecordVersion {
				t.Fatalf("%s carrying nothing new stamped %d, want %d", name, got,
					baseRecordVersion)
			}
		})
	}
}

// THE BARRIER AND THE GENERATION ARE PINNED AT THE BASE VERSION, never at
// [RecordVersion], because what an older node does with one it cannot read is
// retain it — a deferral row per linearizable read, or a transition it never
// makes. Pinned at RecordVersion, the first version a later build adds would
// make every barrier it writes unreadable to the build before it.
func TestTheBarrierAndTheGenerationArePinnedAtTheBaseVersion(t *testing.T) {
	t.Parallel()
	barrier, err := EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind, Gen: 3})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	if got := stampOf(t, barrier); got != baseRecordVersion {
		t.Errorf("a barrier is version %d, want the base %d", got, baseRecordVersion)
	}
	generation, ok, err := GenerationRecord{}.GenerationRecord(statelog.GenerationFacts{
		Generation: 2, By: "tok-1", Writer: "node-a",
		At:     time.Unix(1_700_000_000, 0).UTC(),
		Inputs: statelog.ReanchorInputs{StreamCreatedAt: time.Unix(1_700_000_000, 0).UTC()},
	})
	if err != nil || !ok {
		t.Fatalf("encode a generation: %v (kept: %v)", err, ok)
	}
	if got := stampOf(t, generation.Payload); got != baseRecordVersion {
		t.Errorf("a generation is version %d, want the base %d", got, baseRecordVersion)
	}
	env, err := (Domain{}).Envelope(generation.Payload)
	if err != nil {
		t.Fatalf("the generation's envelope: %v", err)
	}
	if env.Writer != "node-a" || env.Gen != 2 || env.OpID != generation.OpID {
		t.Errorf("the generation's envelope carries writer %q, gen %d, op %q — "+
			"it opens the generation it names, and the eviction gate reads "+
			"the node that wrote it", env.Writer, env.Gen, env.OpID)
	}
}
