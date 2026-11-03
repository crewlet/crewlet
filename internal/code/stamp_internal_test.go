package code

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// novelFields stands in for a field a later build adds to a code record, so
// the stamping rule runs on a field the production table does not name — the
// first versioned field the committer's successor adds meets a rule that
// already runs.
var novelFields = statelog.RecordFields{
	{Name: "PushBatch.Novel", Since: 2, Op: string(OpPush), Path: []string{"mutation", "novel"}},
}

func stampRecord(v int, op OpKind, mutation string) MutationRecord {
	return MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: v, OpID: "op-stamp", Subject: RepoSubject("stamp"),
			Op: op, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Scope: Scope{RepoPath("stamp")},
		},
		Mutation: []byte(mutation), Actor: "node-a", ActorKind: ActorSystem,
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

// A CODE RECORD IS STAMPED WITH THE LOWEST VERSION THAT READS IT — the
// tracker's rule: a set version below that is refused, and the base version
// is what everything carrying nothing new gets, through the production encoder
// as much as under a stand-in table.
func TestACodeRecordIsStampedWithTheLowestVersionThatReadsIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		op       OpKind
		mutation string
		want     int
	}{
		"a push carrying nothing new":           {OpPush, `{"repo_id":"stamp"}`, 1},
		"a push carrying the new field":         {OpPush, `{"novel":0}`, 2},
		"another op carrying the same key":      {OpSettings, `{"novel":0}`, 1},
		"a push whose new field is set to null": {OpPush, `{"novel":null}`, 1},
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
	if _, err := encodeWith(stampRecord(1, OpPush, `{"novel":1}`), novelFields); err == nil ||
		!strings.Contains(err.Error(), "PushBatch.Novel (version 2)") {
		t.Fatalf("a push stamped 1 carrying a version-2 field encoded: %v", err)
	}
	for _, op := range OpKinds {
		body, err := Encode(stampRecord(0, op, `{}`))
		if err != nil {
			t.Fatalf("the production encoder on a %s: %v", op, err)
		}
		if got := stampOf(t, body); got != baseRecordVersion {
			t.Fatalf("the production encoder stamped a %s %d, want %d — the table is "+
				"empty, so nothing this build writes is above the base", op, got, baseRecordVersion)
		}
	}
}

// A RECORD EVERY BUILD MUST READ NEVER CARRIES A VERSIONED FIELD — a gate (an
// eviction, a purge, the activation fence), the read index's barrier, the
// trimmed record and a reanchor's generation, stamped or pinned. Each is what
// an older node cannot afford to hold back, so a field one of them would need
// is refused at the encoder rather than stamped above what every build reads.
func TestARecordEveryBuildReadsRefusesAVersionedField(t *testing.T) {
	t.Parallel()
	everyOp := statelog.RecordFields{
		{Name: "Anything.Novel", Since: 2, Path: []string{"mutation", "novel"}},
	}
	for name, rec := range map[string]MutationRecord{
		"an eviction": {RecordEnvelope: RecordEnvelope{
			Subject: EvictionSubject("node-b"), Op: OpEviction}},
		"a purge": {RecordEnvelope: RecordEnvelope{
			Subject: RepoSubject("stamp"), Op: OpPurge}},
		"the activation fence": {RecordEnvelope: RecordEnvelope{
			Subject: CompanySubject(), Op: OpActivate}},
		"a barrier": {RecordEnvelope: RecordEnvelope{
			V: baseRecordVersion, Subject: BarrierSubject(), Op: OpBarrier}},
		"a trimmed record": {RecordEnvelope: RecordEnvelope{
			V: baseRecordVersion, Subject: Subject{Kind: KindTrimmed, ID: "1.2"}, Op: OpTrimmed}},
		"a generation": {RecordEnvelope: RecordEnvelope{
			V: baseRecordVersion, Subject: GenerationSubject(2), Op: OpGeneration}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec.OpID = "op-pinned"
			rec.Scope = Scope{DomainPath()}
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
			// AND CARRYING NOTHING NEW, it encodes at the base.
			rec.V, rec.Mutation = 0, []byte(`{}`)
			body, err := encodeWith(rec, everyOp)
			if err != nil {
				t.Fatalf("%s carrying nothing new: %v", name, err)
			}
			if got := stampOf(t, body); got != baseRecordVersion {
				t.Fatalf("%s carrying nothing new stamped %d, want %d", name, got, baseRecordVersion)
			}
		})
	}
}
