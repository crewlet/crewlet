package pages

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// novelFields stands in for a field a later build adds to a pages record, so
// the stamping rule can be exercised on a field the production table does not
// name.
var novelFields = statelog.RecordFields{
	{Name: "PagePatch.Novel", Since: 2, Op: string(OpPatch), Path: []string{"mutation", "novel"}},
}

func stampRecord(v int, op OpKind, mutation string) MutationRecord {
	return MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: v, OpID: "op-stamp", Subject: Subject{Kind: KindPage, ID: "stamp"},
			Op: op, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Scope: ScopeSet{Subject: true, Container: "SUITE"},
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

// THE PAGES DOMAIN STAMPS ON THE TRACKER'S RULE: the lowest version that reads
// what a record carries, a set version below that refused, and the base
// version for everything that carries nothing new — through the production
// encoder as much as under a stand-in table.
func TestAPagesRecordIsStampedWithTheLowestVersionThatReadsIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		op       OpKind
		mutation string
		want     int
	}{
		"a patch carrying nothing new":           {OpPatch, `{"body":"x"}`, 1},
		"a patch carrying the new field":         {OpPatch, `{"novel":0}`, 2},
		"another op carrying the same key":       {OpCreate, `{"novel":0}`, 1},
		"a patch whose new field is set to null": {OpPatch, `{"novel":null}`, 1},
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

	if _, err := encodeWith(stampRecord(1, OpPatch, `{"novel":1}`), novelFields); err == nil ||
		!strings.Contains(err.Error(), "PagePatch.Novel (version 2)") {
		t.Fatalf("a patch stamped 1 carrying a version-2 field encoded: %v", err)
	}
	body, err := Encode(stampRecord(0, OpPatch, `{"body":"x"}`))
	if err != nil {
		t.Fatalf("the production encoder: %v", err)
	}
	if got := stampOf(t, body); got != 1 {
		t.Fatalf("the production encoder stamped %d, want 1", got)
	}
}

// THE PRODUCTION TABLE STAMPS A CONTAINER'S SETTINGS AT THE VERSION THAT ADDED
// THEIR EPOCH — a re-stamp of unchanged settings included, since the bytes are
// the same — and leaves every record that carries no epoch at 1.
func TestTheContainerEpochIsStampedAtVersionTwo(t *testing.T) {
	t.Parallel()
	container := func(mutation string) MutationRecord {
		rec := stampRecord(0, OpPatch, mutation)
		rec.Subject = ContainerSubject("ENG")
		rec.Scope = ScopeSet{Subject: true}
		return rec
	}
	for name, tc := range map[string]struct {
		rec  MutationRecord
		want int
	}{
		"settings carrying their activation": {
			container(`{"v":1,"key":"ENG","name":"Engineering","chart_epoch":1767603600000}`), 2},
		"settings an older build wrote, with none": {
			container(`{"v":1,"key":"ENG","name":"Engineering"}`), 1},
		"a page patch": {stampRecord(0, OpPatch, `{"body":"x"}`), 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body, err := Encode(tc.rec)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got := stampOf(t, body); got != tc.want {
				t.Fatalf("stamped %d, want %d", got, tc.want)
			}
		})
	}
	// A SETTINGS RECORD SET BELOW ITS EPOCH'S VERSION IS REFUSED — which is
	// the re-stamp at version 1 an older node would have applied without
	// the stamp, leaving its row open to a stale activation after its
	// upgrade.
	below := container(`{"v":1,"key":"ENG","name":"Engineering","chart_epoch":1767603600000}`)
	below.V = 1
	if _, err := Encode(below); err == nil ||
		!strings.Contains(err.Error(), "ContainerPayload.ChartEpoch (version 2)") {
		t.Fatalf("a container record set at version 1 carrying its epoch encoded: %v", err)
	}
}

// A RECORD EVERY BUILD MUST READ NEVER CARRIES A VERSIONED FIELD — a gate, the
// read index's barrier and a reanchor's generation, stamped or pinned.
//
// Each is what an older node cannot afford to hold back: a deferred gate
// licenses every record it was meant to drop, a retained barrier is a deferral
// row per linearizable read, and a retained generation is a transition that
// node never makes. So a field one of them would need is refused at the
// encoder rather than stamped above what every build reads.
func TestARecordEveryBuildReadsRefusesAVersionedField(t *testing.T) {
	t.Parallel()
	everyOp := statelog.RecordFields{
		{Name: "Anything.Novel", Since: 2, Path: []string{"mutation", "novel"}},
	}
	for name, rec := range map[string]MutationRecord{
		"an eviction": {RecordEnvelope: RecordEnvelope{
			Subject: EvictionSubject("node-b"), Op: OpEviction}},
		"a purge": {RecordEnvelope: RecordEnvelope{
			Subject: PageSubject("stamp"), Op: OpPurge}},
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
