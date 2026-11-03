package code_test

import (
	"context"
	"math"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A BARRIER COSTS THE CODE LOG WHAT ITS CENSUS SAYS. The git census
// (code.GitReadsPerDay) is the barriers that fill a tenth of the log's ceiling,
// priced at BarrierRecordBytes each: a figure that UNDER-states a barrier sizes
// the census past what the tenth holds, so the log fills before the alarm says
// anything; one that wildly OVER-states it fires census_drift on a git host the
// log has plenty of room for. So it is held to a real append through the
// domain's own read index, on a real broker: a thousand barriers move the
// stream by at most a thousand times the figure, and by at least half of it —
// at the first generation, whose envelope carries no generation at all, and at
// the last, whose generation is the widest number a barrier can carry.
func TestABarrierCostsTheCodeLogWhatTheCensusSays(t *testing.T) {
	t.Parallel()
	for name, generation := range map[string]uint32{
		"the first generation": 0,
		"the last generation":  math.MaxUint32,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := openLog(t)
			spec := code.Domain{}.Stream()
			reserve, err := statelog.NewReserve(spec,
				func(ctx context.Context) (statelog.Usage, error) {
					stats, err := log.Stats(ctx)
					return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, err
				})
			if err != nil {
				t.Fatalf("build the gate reserve: %v", err)
			}
			// SIGNED AS EVERY BARRIER A NODE APPENDS IS, since the frame is
			// part of what the log stores and so of what a barrier costs it.
			index, err := statelog.NewReadIndex(code.Domain{}, spec, log, log.Members, testSigner(t), reserve,
				code.EncodeBarrier, func() uint32 { return generation }, nil)
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
			most := int64(barriers * code.BarrierRecordBytes)
			if grew > most || grew < most/2 {
				t.Errorf("%d barriers grew the code log by %d bytes, %d each; the census "+
					"prices one at %d (BarrierRecordBytes), and a barrier must cost between "+
					"half of that and all of it", barriers, grew, grew/barriers,
					code.BarrierRecordBytes)
			}
		})
	}
}
