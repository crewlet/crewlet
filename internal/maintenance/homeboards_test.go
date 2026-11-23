package maintenance_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/maintenance"
)

// boardGrace is the shipped grace, named short for the arithmetic below.
const boardGrace = maintenance.HomeBoardForgetGrace

// homeState is a company's Home boards and chat read state, a roster and a
// worker sweeping both personal records' jobs, on a clock the test moves.
type homeState struct {
	t      *testing.T
	fleet  *coordmem.Fleet
	roster *roster
	clock  *clock
	w      *maintenance.Worker
}

func newHomeState(t *testing.T) *homeState {
	t.Helper()
	hs := &homeState{t: t, fleet: coordmem.NewFleet(), roster: &roster{}, clock: &clock{at: base}}
	hs.w = newWorker(t, maintenance.Options{
		Now: hs.clock.now,
		Jobs: slices.Concat(maintenance.HomeBoardJobs(hs.fleet, hs.roster.humanSeats),
			maintenance.ChatReadJobs(hs.fleet, hs.roster.humanSeats)),
	})
	return hs
}

// save gives handle a board, as its person's first "Done" does.
func (hs *homeState) save(handle string) {
	hs.t.Helper()
	if _, ok, err := hs.fleet.SaveHomeBoard(context.Background(), handle,
		json.RawMessage(`{"widgets":[]}`), base, 0); err != nil || !ok {
		hs.t.Fatalf("SaveHomeBoard(%s) = (%v, %v)", handle, ok, err)
	}
}

// tick sweeps at an instant, reporting how many boards it forgot.
func (hs *homeState) tick(at time.Time) (int64, error) {
	hs.t.Helper()
	hs.clock.set(at)
	swept, err := hs.w.Tick(context.Background())
	return swept["home_board"], err
}

func (hs *homeState) owners() []coord.HomeBoardOwner {
	hs.t.Helper()
	owners, err := hs.fleet.HomeBoardOwners(context.Background())
	if err != nil {
		hs.t.Fatalf("HomeBoardOwners: %v", err)
	}
	return owners
}

// A SEAT THAT LEFT LOSES ITS HOME A GRACE AFTER A SWEEP FIRST SAW IT GONE, and
// no seat the company has is touched on the way: the register ages no key, so
// without this job every seat that ever arranged its Home kept it for ever.
func TestASeatThatLeftLosesItsHomeBoardAGraceAfterItsAbsenceIsSeen(t *testing.T) {
	t.Parallel()
	hs := newHomeState(t)
	for _, handle := range []string{"ceo", "former"} {
		hs.save(handle)
	}
	hs.roster.set("ceo", "swe")
	if forgot, err := hs.tick(base); err != nil || forgot != 0 {
		t.Fatalf("first sweep = (%d, %v), want nothing forgotten", forgot, err)
	}
	want := []coord.HomeBoardOwner{{Handle: "ceo"}, {Handle: "former", AbsentSince: base}}
	if got := hs.owners(); !slices.Equal(got, want) {
		t.Fatalf("after the first sighting the owners are %v, want %v", got, want)
	}
	if forgot, err := hs.tick(base.Add(boardGrace - time.Minute)); err != nil || forgot != 0 {
		t.Fatalf("a sweep inside the grace = (%d, %v), want nothing forgotten", forgot, err)
	}
	if forgot, err := hs.tick(base.Add(boardGrace)); err != nil || forgot != 1 {
		t.Fatalf("the sweep at the end of the grace = (%d, %v), want one board forgotten", forgot, err)
	}
	if got := hs.owners(); !slices.Equal(got, []coord.HomeBoardOwner{{Handle: "ceo"}}) {
		t.Fatalf("owners after the grace = %v, want ceo alone", got)
	}
}

// A SEAT RESTORED INSIDE THE GRACE KEEPS ITS BOARD, and a removal after that
// starts a new grace rather than resuming the old one.
func TestASeatBackInTheCompanyInsideTheGraceKeepsItsHomeBoard(t *testing.T) {
	t.Parallel()
	hs := newHomeState(t)
	hs.save("founder")
	hs.roster.set("ceo")
	if _, err := hs.tick(base); err != nil {
		t.Fatalf("tick: %v", err)
	}
	hs.roster.set("ceo", "founder")
	if _, err := hs.tick(base.Add(time.Hour)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := hs.owners(); !slices.Equal(got, []coord.HomeBoardOwner{{Handle: "founder"}}) {
		t.Fatalf("a seat back in the company reads %v, want its stamp cleared", got)
	}
	hs.roster.set("ceo")
	again := base.Add(2 * time.Hour)
	if _, err := hs.tick(again); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if forgot, err := hs.tick(base.Add(boardGrace + time.Hour)); err != nil || forgot != 0 {
		t.Fatalf("a sweep a grace after the FIRST removal = (%d, %v), want nothing", forgot, err)
	}
	if forgot, err := hs.tick(again.Add(boardGrace)); err != nil || forgot != 1 {
		t.Fatalf("a sweep a grace after the second removal = (%d, %v), want it forgotten", forgot, err)
	}
}

// EACH KIND IS ITS OWN JOB: one tick judges a seat's board and its read state
// under their own names, and a seat holding only one is judged on that one.
func TestEachPersonalRecordIsJudgedByItsOwnJob(t *testing.T) {
	t.Parallel()
	hs := newHomeState(t)
	hs.save("former")
	if _, err := hs.fleet.AdvanceChatRead(context.Background(), "reader", coord.ChatReadDelta{
		Cursors: map[string]int64{coordtest.ChatID("general"): 1}, At: base,
	}); err != nil {
		t.Fatalf("AdvanceChatRead: %v", err)
	}
	hs.roster.set("ceo")
	if _, err := hs.tick(base); err != nil {
		t.Fatalf("tick: %v", err)
	}
	hs.clock.set(base.Add(boardGrace))
	swept, err := hs.w.Tick(context.Background())
	if err != nil || swept["home_board"] != 1 || swept["chat_read_state"] != 1 {
		t.Fatalf("the sweep at the grace = (%v, %v), want one board and one read state forgotten", swept, err)
	}
}

// UNKNOWN IS NEVER ABSENT: an unreadable roster stamps and forgets no board,
// and a company nothing has activated is judged by nobody, without a fault.
func TestAnUnknownChartJudgesNoHomeBoard(t *testing.T) {
	t.Parallel()
	hs := newHomeState(t)
	hs.save("former")
	if _, err := hs.fleet.SetHomeBoardAbsence(context.Background(), "former", base); err != nil {
		t.Fatalf("SetHomeBoardAbsence: %v", err)
	}
	long := base.Add(10 * boardGrace)
	hs.roster.fail(coord.ErrUnavailable)
	if forgot, err := hs.tick(long); !errors.Is(err, coord.ErrUnavailable) || forgot != 0 {
		t.Fatalf("an unreadable chart swept (%d, %v), want nothing and the store's error", forgot, err)
	}
	hs.roster.fail(maintenance.ErrNoActiveRevision)
	if forgot, err := hs.tick(long); err != nil || forgot != 0 {
		t.Fatalf("a company with no activation swept (%d, %v), want nothing and no fault", forgot, err)
	}
	if got := hs.owners(); !slices.Equal(got, []coord.HomeBoardOwner{{Handle: "former", AbsentSince: base}}) {
		t.Fatalf("owners after sweeps of an unknown chart = %v, want them untouched", got)
	}
}

// THE JOB IS ONE FLEET JOB AT THE MAILBOX'S GRACE, and a node missing either
// half contributes nothing rather than a job that fails every tick.
func TestTheHomeBoardJobIsOneFleetJobAtTheMailboxGrace(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	if jobs := maintenance.HomeBoardJobs(nil, (&roster{}).humanSeats); len(jobs) != 0 {
		t.Fatalf("no store built %d jobs", len(jobs))
	}
	if jobs := maintenance.HomeBoardJobs(fleet, nil); len(jobs) != 0 {
		t.Fatalf("no roster built %d jobs", len(jobs))
	}
	jobs := maintenance.HomeBoardJobs(fleet, (&roster{}).humanSeats)
	if len(jobs) != 1 || jobs[0].Name != "home_board" || jobs[0].Scope != maintenance.Fleet ||
		jobs[0].Horizon == nil || jobs[0].Horizon() != boardGrace {
		t.Fatalf("jobs = %+v, want one home_board fleet job at %v", jobs, boardGrace)
	}
	if boardGrace != maintenance.MailboxRetirementGrace {
		t.Fatalf("the board's grace is %v and the mailbox's %v; a restored seat would get "+
			"one back and not the other", boardGrace, maintenance.MailboxRetirementGrace)
	}
}
