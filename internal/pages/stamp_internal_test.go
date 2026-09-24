package pages

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// novelFields stands in for the first field a later build adds to a pages
// record; the production table is empty until one needs a row.
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
