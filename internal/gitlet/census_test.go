package gitlet_test

import (
	"context"
	"math"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A BARRIER COSTS THE CODE LOG WHAT ITS CENSUS SAYS. The git census
// (gitlet.GitReadsPerDay) prices each barrier at BarrierRecordBytes: an
// understated figure lets the log fill before the alarm speaks, a wildly
// overstated one fires census_drift on a log with room. So a thousand real
// appends through the domain's read index, on a real broker, must move the
// stream by at most a thousand times the figure and at least half of it, at
// the first generation (none in the envelope) and the last (the widest).
func TestABarrierCostsTheCodeLogWhatTheCensusSays(t *testing.T) {
	t.Parallel()
	for name, generation := range map[string]uint32{
		"the first generation": 0,
		"the last generation":  math.MaxUint32,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := openLog(t)
			spec := gitlet.Domain{}.Stream()
			reserve, err := statelog.NewReserve(spec,
				func(ctx context.Context) (statelog.Usage, error) {
					stats, err := log.Stats(ctx)
					return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, err
				})
			if err != nil {
				t.Fatalf("build the gate reserve: %v", err)
			}
			// Signed as a node's barriers are: the frame is part of the cost.
			index, err := statelog.NewReadIndex(gitlet.Domain{}, spec, log, log.Members, testSigner(t), reserve,
				gitlet.EncodeBarrier, func() uint32 { return generation }, nil)
			if err != nil {
				t.Fatalf("build the read index: %v", err)
			}
			before, err := log.Stats(t.Context())
			if err != nil {
				t.Fatalf("read the log: %v", err)
			}
			const barriers = 1_000
			for range barriers {
				if _, err := index.Read(t.Context()); err != nil {
					t.Fatalf("a linearizable read's barrier: %v", err)
				}
			}
			after, err := log.Stats(t.Context())
			if err != nil {
				t.Fatalf("read the log: %v", err)
			}
			if got := after.Messages - before.Messages; got != barriers {
				t.Fatalf("the premise: %d sequential reads appended %d barriers, want one each",
					barriers, got)
			}
			grew := int64(after.Bytes) - int64(before.Bytes)
			t.Logf("%d barriers grew the code log by %d bytes, %d each", barriers, grew, grew/barriers)
			most := int64(barriers * gitlet.BarrierRecordBytes)
			if grew > most || grew < most/2 {
				t.Errorf("%d barriers grew the code log by %d bytes, %d each; the census "+
					"prices one at %d (BarrierRecordBytes), and a barrier must cost between "+
					"half of that and all of it", barriers, grew, grew/barriers,
					gitlet.BarrierRecordBytes)
			}
		})
	}
}
