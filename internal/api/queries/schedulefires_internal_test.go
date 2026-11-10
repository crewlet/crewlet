package queries

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// firesUsage answers the week's fires in pages, newest first.
type firesUsage struct {
	UsageReads
	pages [][]usage.ScheduleRun
	reads *int
}

func (f firesUsage) ScheduleRuns(_ context.Context, q usage.ScheduleRunsQuery) ([]usage.ScheduleRun, bool, error) {
	if q.BeforeAt.IsZero() {
		*f.reads = 0
	}
	i := min(*f.reads, len(f.pages)-1)
	*f.reads++
	return f.pages[i], i < len(f.pages)-1, nil
}

// movesWork is a tracker that says which turns moved work.
type movesWork struct {
	WorkReader
	moved map[string]bool
	asked *[]string
}

func (m movesWork) TurnsMoved(_ context.Context, q worklet.TurnsMovedQuery) (map[string]bool, error) {
	*m.asked = q.Turns
	return m.moved, nil
}

// THE ADVISOR READS THE WEEK'S FIRES AND WHAT THEIR TURNS WROTE: every fire
// back to the window's start, a page at a time, each by its seat's handle or
// its unit, moved where the tracker says its turn moved work; a fire with no
// known turn, one before the window and one of a seat the chart no longer
// holds are left out.
func TestTheAdvisorReadsTheWeeksFiresAndWhatTheirTurnsWrote(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte("name: Acme\nproviders:\n  llm:\n    p: {model: m}\n  sandbox: {fake: true}\n" +
		"roles:\n  - name: SRE\n    handle: sre\n    llm: p\n"))
	if err != nil {
		t.Fatal(err)
	}
	roster, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	sre, _ := roster.AgentIDFor(roster.AgentSeatByHandle("sre"))
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	fire := func(scope, id, turn string, ago time.Duration) usage.ScheduleRun {
		return usage.ScheduleRun{ScopeType: scope, ScopeID: id, Name: "health-check",
			FiredAt: now.Add(-ago), TurnID: turn, Outcome: "fired"}
	}
	var reads int
	var asked []string
	s := Sources{
		Company: func() (*config.Company, *org.Organization) { return cfg, roster },
		Usage: firesUsage{reads: &reads, pages: [][]usage.ScheduleRun{
			{fire("role", sre.String(), "t-1", time.Hour), fire("role", sre.String(), "", 2*time.Hour)},
			{fire("unit", "night", "t-2", 3*time.Hour), fire("role", "a-gone", "t-3", 4*time.Hour),
				fire("role", sre.String(), "t-old", 8*24*time.Hour)},
		}},
		Work: movesWork{moved: map[string]bool{"t-2": true}, asked: &asked},
	}
	got, err := s.scheduleFires(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []advisor.ScheduleFire{
		{Seat: "sre", Name: "health-check"},
		{Unit: "night", Name: "health-check", Moved: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fires = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(asked, []string{"t-1", "t-2", "t-3"}) {
		t.Errorf("asked the tracker about %v, want every known turn in the window", asked)
	}
	if reads != 2 {
		t.Errorf("read %d pages, want both, stopping at the window's start", reads)
	}
}
