package queries

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/gitlet"
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

// movesWork is a tracker that says which turns moved work, and movesPages and
// movesCode the pages and the code host.
type (
	movesWork struct {
		WorkReader
		moved map[string]bool
		asked *[]string
	}
	movesPages struct {
		PageReader
		moved map[string]bool
		asked *[]string
	}
	movesCode struct {
		CodeReads
		moved map[string]bool
		asked *[]string
	}
	codeHost struct {
		CodeSource
		reads movesCode
	}
)

func (m movesWork) TurnsMoved(_ context.Context, q worklet.TurnsMovedQuery) (map[string]bool, error) {
	*m.asked = q.Turns
	return m.moved, nil
}

func (m movesPages) TurnsMovedPages(_ context.Context, q doclet.TurnsMovedQuery) (map[string]bool, error) {
	*m.asked = q.Turns
	return m.moved, nil
}

func (h codeHost) CodeReads() (CodeReads, error) { return h.reads, nil }

func (m movesCode) TurnsMovedCode(_ context.Context, q gitlet.TurnsMovedQuery) (map[string]bool, error) {
	*m.asked = q.Turns
	return m.moved, nil
}

// vendorWork is a tracker this company runs on a vendor.
type vendorWork struct{ WorkReader }

func (vendorWork) Kept() error {
	return fmt.Errorf("%w: this company does not run Worklet", ErrUnknown)
}

// THE ADVISOR READS THE WEEK'S FIRES AND WHAT THEIR TURNS WROTE: every fire
// back to the window's start, a page at a time, each by its seat's handle or
// its unit, moved where the tracker, the pages or the code host says its turn
// moved work (a schedule that saves pages or pushes code moves work). A fire
// with no known turn, one still settling, one before the window and one of a
// seat the chart no longer holds are left out.
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
	var askedWork, askedPages, askedCode []string
	s := Sources{
		Company: func() (*config.Company, *org.Organization) { return cfg, roster },
		Usage: firesUsage{reads: &reads, pages: [][]usage.ScheduleRun{
			{fire("role", sre.String(), "t-new", 10*time.Minute), fire("role", sre.String(), "t-1", time.Hour),
				fire("role", sre.String(), "", 2*time.Hour)},
			{fire("unit", "night", "t-2", 3*time.Hour), fire("role", "a-gone", "t-3", 4*time.Hour),
				fire("role", sre.String(), "t-4", 5*time.Hour), fire("role", sre.String(), "t-5", 6*time.Hour),
				fire("role", sre.String(), "t-old", 8*24*time.Hour)},
		}},
		Work:  movesWork{moved: map[string]bool{"t-2": true}, asked: &askedWork},
		Pages: movesPages{moved: map[string]bool{"t-4": true}, asked: &askedPages},
		Code:  codeHost{reads: movesCode{moved: map[string]bool{"t-1": true}, asked: &askedCode}},
	}
	got, err := s.scheduleFires(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []advisor.ScheduleFire{
		{Seat: "sre", Name: "health-check", Moved: true},
		{Unit: "night", Name: "health-check", Moved: true},
		{Seat: "sre", Name: "health-check", Moved: true},
		{Seat: "sre", Name: "health-check"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fires = %+v, want %+v", got, want)
	}
	known := []string{"t-1", "t-2", "t-3", "t-4", "t-5"}
	for half, asked := range map[string][]string{"tracker": askedWork, "pages": askedPages, "code host": askedCode} {
		if !reflect.DeepEqual(asked, known) {
			t.Errorf("asked the %s about %v, want every settled turn in the window", half, asked)
		}
	}
	if reads != 2 {
		t.Errorf("read %d pages, want both, stopping at the window's start", reads)
	}

	s.Work = vendorWork{}
	if got, err := s.scheduleFires(t.Context(), now); err != nil || got != nil {
		t.Errorf("a vendor's tracker: fires = %+v, %v, want none judged and no error", got, err)
	}
}

// skipsUsage answers the week's runs of each outcome in one page.
type skipsUsage struct {
	UsageReads
	runs map[string][]usage.ScheduleRun
}

func (s skipsUsage) ScheduleRuns(_ context.Context, q usage.ScheduleRunsQuery) ([]usage.ScheduleRun, bool, error) {
	return s.runs[q.Outcome], false, nil
}

// A SCHEDULE'S SKIPPED RUNS ARE READ FROM THE DISPATCH LEDGER by why each was
// skipped, a unit's schedule by its unit and a seat's by its handle; one whose
// seat the chart no longer holds is left out. Mutation: read one outcome
// alone, and the paused or the missed run is lost.
func TestASchedulesSkippedRunsAreReadByWhy(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	run := func(scope, id, name string, at time.Duration) usage.ScheduleRun {
		return usage.ScheduleRun{ScopeType: scope, ScopeID: id, Name: name, FiredAt: now.Add(-at)}
	}
	s := Sources{Usage: skipsUsage{runs: map[string][]usage.ScheduleRun{
		"skipped_paused":  {run("unit", "eng", "standup", time.Hour), run("role", "gone-seat", "poll", 2*time.Hour)},
		"skipped_catchup": {run("unit", "eng", "standup", 3*time.Hour), run("unit", "eng", "standup", 9*24*time.Hour)},
	}}}
	got, err := s.scheduleSkips(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []advisor.ScheduleSkip{{Unit: "eng", Name: "standup", Paused: true}, {Unit: "eng", Name: "standup"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("skips %+v, want the unit's paused and missed runs in the week", got)
	}
	if _, err := (Sources{}).scheduleSkips(t.Context(), now); err == nil {
		t.Error("a node with no usage half read the skipped runs")
	}
}
