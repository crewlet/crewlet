package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// SESSION CARRY BURNS ONLY ON FAULTS, and every loss is counted: a run that
// lost its session deliberately (its context spent, its provider's refusal,
// another harness, the seat's budget floor) is said and counted by its reason,
// and is a good event; a fault is a bad one, and enough of them burn.
//
// Mutation: record a deliberate loss as bad, a fault as good, or count no loss,
// and a case goes red.
func TestSessionCarryBurnsOnlyOnFaults(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	for name, c := range map[string]struct {
		loss  executorkeep.Loss
		burns bool
	}{
		"deliberate": {executorkeep.LossContextExhausted, false},
		"a fault":    {executorkeep.LossTooLarge, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec, err := metrics.New()
			if err != nil {
				t.Fatal(err)
			}
			meter := sessionMeter{metrics: rec.WithClock(clock), objectives: objectives.NewMeter(nil, clock)}
			for range 10 {
				meter.SessionCarried(&executorkeep.NotContinued{Loss: c.loss, Instead: executorkeep.InsteadFresh},
					sandbox.HandoverHanded)
				meter.SessionCarried(nil, "")
			}
			burning := false
			for _, b := range meter.objectives.Burning() {
				burning = burning || b.Objective == objectives.SessionCarry
			}
			if burning != c.burns {
				t.Errorf("half the launches lost their session for %s: burning %v, want %v", c.loss, burning,
					c.burns)
			}
			var counted uint64
			for _, s := range rec.Read() {
				if s.Name == metrics.ExecutorSessionNotContinued && s.Attrs["reason"] == string(c.loss) {
					counted += s.Total
				}
			}
			if counted != 10 {
				t.Errorf("counted %d losses for %s, want 10", counted, c.loss)
			}
		})
	}
}

// A LOSS IS COUNTED BY WHAT ITS RUN WAS HANDED OF THE LOST SESSION'S SUMMARY:
// `summary` is the brief's hand-over, and a brief that said nothing of one
// handed none, so every loss carries one of the three values.
//
// Mutation: leave the attribute off, or count a silent brief under no value,
// and a case goes red.
func TestALossIsCountedByWhatItsRunWasHanded(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	meter := sessionMeter{metrics: rec, objectives: objectives.NewMeter(nil, nil)}
	lost := &executorkeep.NotContinued{Loss: executorkeep.LossTooLarge, Instead: executorkeep.InsteadFresh}
	for _, h := range []sandbox.SummaryHandover{sandbox.HandoverHanded, sandbox.HandoverUnread, ""} {
		meter.SessionCarried(lost, h)
	}
	counted := map[string]uint64{}
	for _, s := range rec.Read() {
		if s.Name == metrics.ExecutorSessionNotContinued {
			counted[s.Attrs["summary"]] += s.Total
		}
	}
	if len(counted) != 3 || counted["handed"] != 1 || counted["unread"] != 1 || counted["none"] != 1 {
		t.Fatalf("counted %v by summary, want one each of handed, unread and none", counted)
	}
}
