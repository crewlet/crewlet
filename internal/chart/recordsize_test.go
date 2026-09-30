package chart_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE LOG'S DECLARED LARGEST RECORD HOLDS EVERY RECORD ITS BOUNDED GESTURES
// PRODUCE AT THEIR CAPS.
//
// The declaration is what the org chart log's gate reserve is sized by, so it
// is held from both sides: small enough that the reserve leaves the log most
// of itself at its Tier A floor (the engine's own gate holds that), and large
// enough that nothing a person can write inside this domain's caps is refused
// as too large. This is the second half. Each record is built the way the
// writer builds it — a full envelope, the actor, the credential, the turn and
// the revision beside the payload — at every cap the domain enforces, so
// raising a cap without revisiting [chart.ChartMaxRecordBytes] fails here
// rather than on somebody's reorganisation.
//
// WHAT IS NOT HERE IS STATED: prose is ASCII, because JSON escapes a control
// character or `<` six-fold and a seat whose whole biography is those is text
// nobody types; such a record is refused `record_too_large` at the write. A
// batch's operations carry no `manages:` lists here, because thirty-two
// kibibytes of them per operation is a batch that splits — the refusal says
// to. And an import is bounded by nothing but this declaration, so its case
// is a chart ten times the thousand seats the batch cap is sized against.
func TestTheLargestRecordHoldsEveryBoundedGesture(t *testing.T) {
	t.Parallel()
	limit := chart.Domain{}.Stream().MaxRecordBytes
	for name, tc := range map[string]struct {
		rec  chart.MutationRecord
		room int64
	}{
		// ROOM FOR THE RUNTIME HALF: the one field this domain cannot
		// bound, because it is opaque here. It is configuration — a
		// model chain, a sandbox cell, the credentials as `${VAR}`
		// references — and a quarter of the declaration is kilobytes of
		// that many times over.
		"a seat's content at every cap": {
			rec: sizedRecord(chart.SeatSubject(sizedKey("s", 0)), chart.OpUpsert,
				chart.ScopeSet{Subject: true, Unit: sizedKey("u", 0)}, chart.SeatPayload{
					V: chart.DocumentVersion, Handle: sizedKey("s", 0),
					Name: sizedProse(chart.MaxName), Email: sizedProse(chart.MaxEmail),
					Backstory: sizedProse(chart.MaxProse), Goal: sizedProse(chart.MaxProse),
					Responsibilities:     sizedList(chart.MaxList, chart.MaxProse),
					BehavioralGuidelines: sizedList(chart.MaxList, chart.MaxProse),
					Project:              sizedKey("p", 0), Space: sizedKey("c", 0),
				}),
			room: limit / 4,
		},
		"a unit's content at every cap": {
			rec: sizedRecord(chart.UnitSubject(sizedKey("u", 0)), chart.OpUpsert,
				chart.ScopeSet{Subject: true}, chart.UnitPayload{
					V: chart.DocumentVersion, Key: sizedKey("u", 0),
					Name: sizedProse(chart.MaxName), Type: sizedProse(chart.MaxKey),
					Purpose: sizedProse(chart.MaxProse), Goals: sizedList(chart.MaxList, chart.MaxProse),
					Channel: sizedKey("ch", 0), Project: sizedKey("p", 0), Space: sizedKey("c", 0),
					KnowledgeRefs: sizedList(chart.MaxList, chart.MaxKey),
				}),
			room: limit / 4,
		},
		// A BATCH AT ITS OPERATION CAP, every key at its own: the
		// arithmetic MaxBatchOperations is sized by.
		"a batch of the most operations one may carry": {
			rec: sizedRecord(chart.TreeSubject(), chart.OpPlace,
				chart.BatchScope(sizedTerms(chart.MaxBatchOperations)),
				chart.PlacementPayload{V: chart.DocumentVersion,
					Edges: sizedEdges(chart.MaxBatchOperations, chart.OpCreateSeat)}),
		},
		"an import of a chart of ten thousand seats": {
			rec: sizedRecord(chart.TreeSubject(), chart.OpImport,
				chart.BatchScope(sizedTerms(10_000)),
				chart.ImportPayload{V: chart.DocumentVersion,
					Revision: "rev-" + strings.Repeat("0", 32),
					Edges:    sizedEdges(10_000, "")}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body, err := chart.Encode(tc.rec)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			size := int64(len(body))
			t.Logf("%d bytes of a declared largest of %d", size, limit)
			if size+tc.room > limit {
				t.Errorf("the record is %d bytes and must leave %d beside it, "+
					"past the log's declared largest of %d — raise "+
					"chart.ChartMaxRecordBytes with the reserve it sizes, or "+
					"lower the cap that grew", size, tc.room, limit)
			}
		})
	}
}

// sizedRecord is one mutation record as the writer forms it, every field of the
// envelope and the party filled.
func sizedRecord(subject chart.Subject, op chart.OpKind, scope chart.ScopeSet,
	payload any) chart.MutationRecord {

	body, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("marshal a payload: %v", err))
	}
	return chart.MutationRecord{
		RecordEnvelope: chart.RecordEnvelope{
			V: chart.RecordVersion, OpID: statelog.NewOpID(time.Now(), "chart-batch"),
			Subject: subject, Op: op, CreatedAt: time.Now().UTC(),
			Gen: 1 << 20, Writer: sizedKey("node", 0), Scope: scope,
		},
		Mutation:   body,
		Actor:      sizedKey("actor", 0),
		ActorKind:  chart.AuthorHuman,
		OperatorID: "pat:" + strings.Repeat("f", 36),
		TurnID:     strings.Repeat("f", 36),
		Revision:   "rev-" + strings.Repeat("0", 32),
	}
}

// sizedKey is the i-th address of a kind at the cap every key is held to.
func sizedKey(kind string, i int) string {
	k := fmt.Sprintf("%s-%d-", kind, i)
	return k + strings.Repeat("k", chart.MaxKey-len(k))
}

// sizedProse is n bytes of text JSON carries as it is.
func sizedProse(n int) string { return strings.Repeat("x", n) }

// sizedList is n entries of size bytes each.
func sizedList(n, size int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = sizedProse(size)
	}
	return out
}

// sizedEdges is n seats, each under a unit and led by a seat, every key at its cap.
func sizedEdges(n int, op chart.OperationKind) []chart.Edge {
	out := make([]chart.Edge, n)
	for i := range out {
		out[i] = chart.Edge{
			Object: chart.ObjectRef{Kind: chart.KindSeat, ID: sizedKey("s", i)},
			Parent: sizedKey("u", i), Op: op, Kind: chart.SeatAgent,
		}
	}
	return out
}

// sizedTerms is the scope an n-object record states: its seats enumerated up to
// the cap, and the root past it.
func sizedTerms(n int) []chart.ScopeTerm {
	out := make([]chart.ScopeTerm, n)
	for i := range out {
		out[i] = chart.ScopeTerm{Kind: chart.TermSeat, Unit: sizedKey("u", i), ID: sizedKey("s", i)}
	}
	return out
}
