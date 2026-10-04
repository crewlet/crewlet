package engine

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE INDEX'S ALARM INPUT DESCRIBES THE SPACE THE COMPANY SEARCHES IN, and
// carries the latest measurement exactly as the partition recorded it.
//
// An index trained under a model the company has since left serves no query:
// every search is in the new space and scans until the duty retrains. Its
// recall describes nothing a search does, so a reading carrying it would page
// an operator about a model they already moved off — and the remedy the alarm
// names (retrain, re-evaluate) does nothing to it. And the floor is the one
// the measurement judged its worst shape against — the evaluation's curve at
// the size of the corpus that shape searched, which only the measurement knew
// — never one recomputed here from the partition's size.
func TestTheIndexReadingDescribesOnlyTheSpaceTheCompanySearches(t *testing.T) {
	t.Parallel()
	const width = 64
	const model = "text-embedding-3-small" // embeddingDoc's model
	for _, tc := range []struct {
		name       string
		model      string
		dim        int
		configured bool
		reported   bool
	}{
		{"the current space", model, width, true, true},
		{"a model the company left", "text-embedding-ada-002", width, true, false},
		{"another width of the same model", model, width * 2, true, false},
		{"no embeddings configured", model, width, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := engineOverStore(t, 0)
			if tc.configured {
				e.epoch.current.Store(companyWith(t, fmt.Sprintf(embeddingDoc, width)))
				var fake embeddings.Embedder = embeddings.NewFake(width)
				e.embeddings.Store(&fake)
			} else {
				e.epoch.current.Store(companyWith(t, noEmbeddingsDoc))
			}
			// A FLOOR NO SIZE OF THIS PARTITION GIVES: the measured shape
			// searched a corpus of its own size, and a reading that
			// recomputed the floor from the partition's would not carry it.
			const measuredOn = 60_000
			floor := search.FloorAt(9_000)
			measurement := search.Measurement{Sources: measuredOn, Recall: 0.5,
				Floor: floor, Shape: search.ShapeSource, ShapeSource: search.SourcePage,
				HeadMisses: 7}
			verdict := search.VectorRecord{
				RecordEnvelope: search.RecordEnvelope{
					Subject: search.IndexCentroids, Op: search.OpCentroids, Gen: 1,
					CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
					Scope: statelog.ScopeSet{Paths: []string{
						search.IndexScopePath(search.IndexCentroids)}},
				},
				Model: tc.model, Dim: tc.dim,
				Index: &search.IndexRecord{Log: "S", TrainedOn: measuredOn,
					Measurement: &measurement, Why: search.VerdictNotWorthwhile},
			}
			payload, err := verdict.Encode()
			if err != nil {
				t.Fatalf("encode the verdict: %v", err)
			}
			if err := storetest.EstateOf(e.backends.Store).Tx(t.Context(), func(tx *sql.Tx) error {
				_, err := search.NewApplier().Apply(t.Context(), tx, statelog.Record{
					Position: statelog.Position{Stream: "S", Generation: 1, Seq: 9},
					Payload:  payload,
				}, statelog.ApplyOptions{})
				return err
			}); err != nil {
				t.Fatalf("apply the verdict: %v", err)
			}

			var out statelog.Reading
			e.indexReading(t.Context(), &out)
			if !tc.reported {
				if out.IVFRecall != nil {
					t.Fatalf("the reading carries recall %.2f for an index the "+
						"company's searches never read", *out.IVFRecall)
				}
				return
			}
			if out.IVFRecall == nil || *out.IVFRecall != measurement.Recall {
				t.Fatalf("recall = %v, want the measurement's %.2f", out.IVFRecall,
					measurement.Recall)
			}
			if out.IVFRecallFloor != floor || floor == search.FloorAt(measuredOn) {
				t.Fatalf("floor = %.4f, want the %.4f the measurement judged its "+
					"shape against", out.IVFRecallFloor, floor)
			}
			if out.IVFMeasuredOn != measuredOn || out.IVFShape != "source:page" {
				t.Fatalf("measured on %d in the %q shape, want %d in source:page",
					out.IVFMeasuredOn, out.IVFShape, measuredOn)
			}
		})
	}
}
