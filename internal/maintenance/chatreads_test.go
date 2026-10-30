package maintenance_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/maintenance"
)

// readGrace is the shipped grace, named short for the arithmetic below.
const readGrace = maintenance.ChatReadForgetGrace

// clock is a worker clock a test moves between ticks.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// readState is a company's chat read state, a roster and a worker sweeping
// only the reconcile, on a clock the test moves.
type readState struct {
	t      *testing.T
	fleet  *coordmem.Fleet
	roster *roster
	clock  *clock
	w      *maintenance.Worker
}

func newReadState(t *testing.T, records maintenance.ChatReadRecords, fleet *coordmem.Fleet) *readState {
	t.Helper()
	rs := &readState{t: t, fleet: fleet, roster: &roster{}, clock: &clock{at: base}}
	if records == nil {
		records = fleet
	}
	rs.w = newWorker(t, maintenance.Options{
		Now:  rs.clock.now,
		Jobs: maintenance.ChatReadJobs(records, rs.roster.humanSeats),
	})
	return rs
}

// humanSeats reads the roster as the chat read reconcile's roster is read:
// the handles of the human seats the active revision names.
func (r *roster) humanSeats(ctx context.Context) ([]string, error) {
	seats, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	handles := make([]string, 0, len(seats))
	for _, seat := range seats {
		handles = append(handles, seat.Handle)
	}
	return handles, nil
}

// read gives handle a record, as its person's first flush does.
func (rs *readState) read(handle string) {
	rs.t.Helper()
	if _, err := rs.fleet.AdvanceChatRead(context.Background(), handle, coord.ChatReadDelta{
		Cursors: map[string]int64{"general": 42}, At: base,
	}); err != nil {
		rs.t.Fatalf("AdvanceChatRead(%s): %v", handle, err)
	}
}

// tick sweeps at an instant, reporting how many records it forgot.
func (rs *readState) tick(at time.Time) (int64, error) {
	rs.t.Helper()
	rs.clock.set(at)
	swept, err := rs.w.Tick(context.Background())
	return swept["chat_read_state"], err
}

func (rs *readState) readers() []coord.ChatReader {
	rs.t.Helper()
	readers, err := rs.fleet.ChatReaders(context.Background())
	if err != nil {
		rs.t.Fatalf("ChatReaders: %v", err)
	}
	return readers
}

func (rs *readState) absentSince(handle string) (time.Time, bool) {
	rs.t.Helper()
	for _, r := range rs.readers() {
		if r.Handle == handle {
			return r.AbsentSince, true
		}
	}
	return time.Time{}, false
}

// A SEAT THAT LEFT IS FORGOTTEN A GRACE AFTER A SWEEP FIRST SAW IT GONE, and
// no seat the company has is touched on the way.
//
// This is the whole of the repair: the bucket has no age, and before this job
// nothing anywhere forgot a record, so every seat anybody ever read a room as
// kept its cursors for the life of the deployment.
func TestASeatThatLeftIsForgottenAGraceAfterItsAbsenceIsSeen(t *testing.T) {
	t.Parallel()
	rs := newReadState(t, nil, coordmem.NewFleet())
	for _, handle := range []string{"ceo", "founder", "former"} {
		rs.read(handle)
	}
	rs.roster.set("ceo", "founder", "swe")

	// THE FIRST SIGHTING stamps, and forgets nothing.
	if forgot, err := rs.tick(base); err != nil || forgot != 0 {
		t.Fatalf("first sweep = (%d, %v), want nothing forgotten", forgot, err)
	}
	if since, ok := rs.absentSince("former"); !ok || !since.Equal(base) {
		t.Fatalf("former's absence = (%v, %v), want stamped at the sweep that saw it, %v", since, ok, base)
	}
	for _, handle := range []string{"ceo", "founder"} {
		if since, _ := rs.absentSince(handle); !since.IsZero() {
			t.Fatalf("%s is in the company and was stamped absent at %v", handle, since)
		}
	}

	// INSIDE THE GRACE: kept, and the stamp is the FIRST sighting's.
	if forgot, err := rs.tick(base.Add(readGrace - time.Minute)); err != nil || forgot != 0 {
		t.Fatalf("a sweep inside the grace = (%d, %v), want nothing forgotten", forgot, err)
	}
	if since, _ := rs.absentSince("former"); !since.Equal(base) {
		t.Fatalf("a later sweep moved the stamp to %v; the grace is measured from the first sighting", since)
	}

	// THE GRACE HAS RUN: forgotten, and only it.
	if forgot, err := rs.tick(base.Add(readGrace)); err != nil || forgot != 1 {
		t.Fatalf("the sweep at the end of the grace = (%d, %v), want one record forgotten", forgot, err)
	}
	want := []coord.ChatReader{{Handle: "ceo"}, {Handle: "founder"}}
	if got := rs.readers(); !slices.Equal(got, want) {
		t.Fatalf("readers after the grace = %v, want %v", got, want)
	}
}

// A SEAT RESTORED TO THE CHART INSIDE THE GRACE KEEPS EVERY CURSOR, and a
// removal after that starts a new grace rather than resuming the old one.
func TestASeatBackInTheCompanyInsideTheGraceKeepsItsRecord(t *testing.T) {
	t.Parallel()
	rs := newReadState(t, nil, coordmem.NewFleet())
	rs.read("founder")
	rs.roster.set("ceo")
	if _, err := rs.tick(base); err != nil {
		t.Fatalf("tick: %v", err)
	}
	rs.roster.set("ceo", "founder")
	if _, err := rs.tick(base.Add(time.Hour)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if since, _ := rs.absentSince("founder"); !since.IsZero() {
		t.Fatalf("a seat back in the company is still stamped absent since %v", since)
	}
	// REMOVED AGAIN: the clock starts over.
	rs.roster.set("ceo")
	again := base.Add(2 * time.Hour)
	if _, err := rs.tick(again); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if forgot, err := rs.tick(base.Add(readGrace + time.Hour)); err != nil || forgot != 0 {
		t.Fatalf("a sweep a grace after the FIRST removal = (%d, %v), want nothing: the "+
			"seat came back in between, so the grace runs from the second", forgot, err)
	}
	state, err := rs.fleet.ChatRead(context.Background(), "founder")
	if err != nil || state.Cursors["general"] != 42 {
		t.Fatalf("a seat restored inside the grace reads (%+v, %v), want its cursors", state, err)
	}
	if forgot, err := rs.tick(again.Add(readGrace)); err != nil || forgot != 1 {
		t.Fatalf("a sweep a grace after the second removal = (%d, %v), want them forgotten", forgot, err)
	}
}

// UNKNOWN IS NEVER ABSENT. A roster that cannot be read stamps nothing and
// forgets nothing — not even a record whose grace ran out long ago — and says
// why; a company nothing has activated is not a fault and is judged by nobody.
func TestAnUnknownChartJudgesNobody(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	rs := newReadState(t, nil, fleet)
	rs.read("ceo")
	rs.read("former")
	if _, err := fleet.SetChatReadAbsence(context.Background(), "former", base); err != nil {
		t.Fatalf("SetChatReadAbsence: %v", err)
	}
	long := base.Add(10 * readGrace)

	rs.roster.fail(coord.ErrUnavailable)
	if forgot, err := rs.tick(long); !errors.Is(err, coord.ErrUnavailable) || forgot != 0 {
		t.Fatalf("an unreadable chart swept (%d, %v), want nothing done and the store's error", forgot, err)
	}
	rs.roster.fail(maintenance.ErrNoActiveRevision)
	if forgot, err := rs.tick(long); err != nil || forgot != 0 {
		t.Fatalf("a company with no activation swept (%d, %v), want nothing and no fault", forgot, err)
	}
	want := []coord.ChatReader{{Handle: "ceo"}, {Handle: "former", AbsentSince: base}}
	if got := rs.readers(); !slices.Equal(got, want) {
		t.Fatalf("readers after sweeps of an unknown chart = %v, want them untouched, %v", got, want)
	}
}

// AN UNREADABLE LISTING IS REPORTED, never read as "nobody holds read state".
func TestAnUnreadableListingIsReported(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	rs := newReadState(t, failingReaders{fleet}, fleet)
	rs.roster.set("ceo")
	if _, err := rs.tick(base); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("a sweep over an unreadable listing answered %v, want the store's error", err)
	}
}

type failingReaders struct{ *coordmem.Fleet }

func (failingReaders) ChatReaders(context.Context) ([]coord.ChatReader, error) {
	return nil, coord.ErrUnavailable
}

// A FORGET A PEER OVERTOOK IS NOT COUNTED. Between this sweep's listing and its
// forget, another sweep — two overlap during a duty handoff — found the seat
// back and cleared the stamp; the store refuses the forget, and the job
// neither counts it nor treats it as a failure.
func TestAForgetAPeerOvertookIsNeitherCountedNorAFailure(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	rs := newReadState(t, overtaken{fleet}, fleet)
	rs.read("former")
	if _, err := fleet.SetChatReadAbsence(context.Background(), "former", base); err != nil {
		t.Fatalf("SetChatReadAbsence: %v", err)
	}
	rs.roster.set("ceo")
	if forgot, err := rs.tick(base.Add(readGrace)); err != nil || forgot != 0 {
		t.Fatalf("an overtaken forget swept (%d, %v), want nothing counted and no error", forgot, err)
	}
	if state, err := fleet.ChatRead(context.Background(), "former"); err != nil || state.Cursors["general"] != 42 {
		t.Fatalf("a seat a peer found back lost its record: (%+v, %v)", state, err)
	}
}

// overtaken is the store with a peer's sweep landing just before every forget:
// it finds the seat back and clears its stamp.
type overtaken struct{ *coordmem.Fleet }

func (o overtaken) ForgetChatRead(ctx context.Context, handle string, absentBy time.Time) (bool, error) {
	if _, err := o.Fleet.SetChatReadAbsence(ctx, handle, time.Time{}); err != nil {
		return false, err
	}
	return o.Fleet.ForgetChatRead(ctx, handle, absentBy)
}

// THE JOB IS ONE FLEET JOB AT ITS GRACE, and a node missing either half
// contributes nothing rather than a job that fails every tick.
func TestTheChatReadJobIsOneFleetJobAtItsGrace(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	if jobs := maintenance.ChatReadJobs(nil, (&roster{}).humanSeats); len(jobs) != 0 {
		t.Fatalf("no store built %d jobs", len(jobs))
	}
	if jobs := maintenance.ChatReadJobs(fleet, nil); len(jobs) != 0 {
		t.Fatalf("no roster built %d jobs", len(jobs))
	}
	jobs := maintenance.ChatReadJobs(fleet, (&roster{}).humanSeats)
	if len(jobs) != 1 {
		t.Fatalf("built %d jobs, want one", len(jobs))
	}
	j := jobs[0]
	if j.Name != "chat_read_state" || j.Scope != maintenance.Fleet || j.Horizon == nil {
		t.Fatalf("job = %q %q, want chat_read_state under the fleet duty, with a horizon", j.Name, j.Scope)
	}
	if got := j.Horizon(); got != readGrace {
		t.Fatalf("the job's horizon is %v, want the grace, %v", got, readGrace)
	}
	// ONE ANSWER FOR THE UNDO WINDOW: a seat restored inside it gets its
	// mailbox AND its rooms back, never one without the other.
	if readGrace != maintenance.MailboxRetirementGrace {
		t.Fatalf("the read state's grace is %v and the mailbox's %v; a restored seat "+
			"would get one back and not the other", readGrace, maintenance.MailboxRetirementGrace)
	}
}
