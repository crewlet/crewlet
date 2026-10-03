package chart_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY RECORD THE WRITER PUBLISHES IS STAMPED THE VERSION ITS RULES NEED.
//
// The version is what a peer decides by — at or below what it reads it applies
// the record, above it retains it — and on THIS domain it is also what the
// applier reads its own rules off ([chart.RecordVersion]'s list): an edge's verb
// and a content record that never creates from version 2, a seat's `manages:`
// as structure from version 3. So every record this build decides under those
// rules must say version 3, every gate, barrier and generation version 1 for
// ever, and a stamp one lower would have this very build apply its own write
// under the older rules — a seat's content clearing its manager's list — while
// one higher would have every peer retain a gate.
//
// Each case is one gesture through the writer's own path, read back off the
// log as a peer receives it: the signed frame opened, the envelope decoded.
// Mutation: stamp a content record by its fields alone, or a removal at the
// build's version, and the case for it fails.
func TestEveryRecordTheWriterPublishesKeepsItsVersion(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	for _, tc := range []struct {
		name  string
		write func(t *testing.T)
		want  int
	}{
		{"a batch creating a unit", func(t *testing.T) {
			r.batch("op-unit", op(chart.OpCreateUnit, chart.KindUnit, "engineering", ""))
		}, chart.RecordVersion},
		{"a batch creating seats under it", func(t *testing.T) {
			r.batch("op-seats", op(chart.OpCreateSeat, chart.KindSeat, "dev", "engineering"),
				op(chart.OpCreateSeat, chart.KindSeat, "lead", "engineering"),
				op(chart.OpCreateSeat, chart.KindSeat, "ops", ""))
		}, chart.RecordVersion},
		{"a batch giving a seat a manages list", func(t *testing.T) {
			r.batch("op-manages", managesOp("lead", "dev"))
		}, chart.RecordVersion},
		{"a batch clearing a manages list", func(t *testing.T) {
			r.batch("op-unmanage", managesOp("lead"))
		}, chart.RecordVersion},
		{"a batch renaming a unit", func(t *testing.T) {
			if _, err := r.applyRekey("op-rename", "platform", "engineering"); err != nil {
				t.Fatalf("rename: %v", err)
			}
		}, chart.RecordVersion},
		{"a unit's content", func(t *testing.T) {
			if _, err := r.writer.WriteUnit(t.Context(), "op-unit-content",
				chart.UnitContent{Key: "platform", Name: "Platform"}); err != nil {
				t.Fatalf("write the unit: %v", err)
			}
			r.drain()
		}, chart.RecordVersion},
		{"a seat's content", func(t *testing.T) {
			if _, err := r.seat("op-seat-content", chart.SeatContent{
				Handle: "dev", Unit: "platform", Name: "Dev"}); err != nil {
				t.Fatalf("write the seat: %v", err)
			}
		}, chart.RecordVersion},
		{"an import of a revision", func(t *testing.T) {
			r.mustImport("op-import", "rev-1",
				chart.Edge{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "platform"}},
				chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "dev"},
					Parent: "platform", Kind: chart.SeatAgent})
		}, chart.RecordVersion},
		{"a removal", func(t *testing.T) {
			if _, err := r.writer.WithHolders(noHolders{}).WriteRemoval(t.Context(),
				"op-remove", chart.Batch{Reason: "the suite",
					Operations: []chart.Operation{
						op(chart.OpRemoveObject, chart.KindSeat, "ops", ""),
					}}); err != nil {
				t.Fatalf("remove: %v", err)
			}
			r.drain()
		}, chart.GateRecordVersion},
		{"an eviction", func(t *testing.T) {
			if _, err := r.writer.EvictNode(t.Context(), "op-evict", "node-z"); err != nil {
				t.Fatalf("evict: %v", err)
			}
			r.drain()
		}, chart.GateRecordVersion},
		{"a readmission", func(t *testing.T) {
			if _, err := r.writer.ReadmitNode(t.Context(), "op-readmit", "node-z"); err != nil {
				t.Fatalf("readmit: %v", err)
			}
			r.drain()
		}, chart.GateRecordVersion},
	} {
		before, err := r.log.End(t.Context())
		if err != nil {
			t.Fatalf("read the log's end: %v", err)
		}
		tc.write(t)
		last, err := r.log.End(t.Context())
		if err != nil {
			t.Fatalf("read the log's end: %v", err)
		}
		if last <= before {
			t.Fatalf("%s published nothing", tc.name)
		}
		for seq := before + 1; seq <= last; seq++ {
			if got := r.versionAt(seq); got != tc.want {
				t.Errorf("%s published record %d at version %d, want %d", tc.name,
					seq, got, tc.want)
			}
		}
	}

	// THE TWO RECORDS NO WRITER DECIDES, encoded as the framework asks for
	// them: a reanchor's generation and the read index's barrier.
	generation, keeps, err := chart.GenerationRecord{}.GenerationRecord(statelog.GenerationFacts{
		Generation: 2, Case: statelog.ReanchorRecreated,
		Inputs: statelog.ReanchorInputs{Stream: chart.Domain{}.Stream().Name},
		By:     "ops", Writer: "node-a", At: time.Unix(1_700_000_000, 0).UTC(),
	})
	if err != nil || !keeps {
		t.Fatalf("encode a generation record: (%v, %v)", keeps, err)
	}
	barrier, err := chart.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind, Gen: 1})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	for name, body := range map[string][]byte{
		"a generation": generation.Payload, "a barrier": barrier,
	} {
		env, err := chart.DecodeEnvelope(body)
		if err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if env.V != chart.BaseRecordVersion {
			t.Errorf("%s is stamped version %d, want %d — an older peer retains "+
				"one it cannot read", name, env.V, chart.BaseRecordVersion)
		}
	}
}

// versionAt is the version of the record at seq, read as a peer reads it.
func (r *writeRig) versionAt(seq uint64) int {
	r.t.Helper()
	_, payload, _, ok, err := r.log.At(r.t.Context(), seq)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: (%v, %v)", seq, ok, err)
	}
	body, verdict := r.verifier.Open(payload)
	if verdict != statelog.Verified {
		r.t.Fatalf("record %d did not verify: %s", seq, verdict)
	}
	env, err := chart.DecodeEnvelope(body)
	if err != nil {
		r.t.Fatalf("decode record %d: %v", seq, err)
	}
	return env.V
}
