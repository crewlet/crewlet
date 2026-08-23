package schedule

// Behavioural tests for the tick loop, ported from tests/test_schedule/
// test_scheduler.py case for case, plus what Go's own shape makes reachable.
//
// Internal (package schedule) rather than black-box for exactly one reason:
// the Python suite drives the loop by assigning `_last_tick_utc`, which is how
// it separates "the first tick after a restart" from "an ordinary tick with a
// window behind it". That distinction is the whole of the catchup design and
// there is no honest way to reach it from outside — an exported seeder would
// be test-only API in production code, and sleeping through a real window
// would make every case here slow and flaky.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
)

// --- fakes ----------------------------------------------------------------

// fakeQueue records every publish and can refuse a named topic.
type fakeQueue struct {
	mu        sync.Mutex
	published []published
	refuse    map[string]error
}

type published struct {
	topic string
	ev    *events.Event
}

func newQueue() *fakeQueue { return &fakeQueue{refuse: map[string]error{}} }

func (q *fakeQueue) Publish(_ context.Context, topic string, ev *events.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err, refused := q.refuse[topic]; refused {
		return err
	}
	q.published = append(q.published, published{topic: topic, ev: ev})
	return nil
}

// inboxTasks is every TaskAssigned that reached a seat's inbox.
func (q *fakeQueue) inboxTasks() []published {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []published
	for _, p := range q.published {
		if _, ok := events.DataAs[*types.TaskAssigned](p.ev); ok && strings.HasSuffix(p.topic, ".inbox") {
			out = append(out, p)
		}
	}
	return out
}

func (q *fakeQueue) inboxTopics() []string {
	var out []string
	for _, p := range q.inboxTasks() {
		out = append(out, p.topic)
	}
	return out
}

func (q *fakeQueue) lifecycleEvents() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, p := range q.published {
		if _, ok := events.DataAs[*types.ScheduledTaskFired](p.ev); ok {
			n++
		}
	}
	return n
}

// countingLedger wraps a ledger and counts the claims that reached it, so a
// case can assert that a gated tick did not merely fail to publish but never
// reached the store at all.
type countingLedger struct {
	inner  *MemoryLedger
	mu     sync.Mutex
	claims int
	err    error
}

func newCountingLedger() *countingLedger { return &countingLedger{inner: NewMemoryLedger()} }

func (c *countingLedger) Claim(ctx context.Context, run Run) (bool, error) {
	c.mu.Lock()
	c.claims++
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return false, err
	}
	return c.inner.Claim(ctx, run)
}

func (c *countingLedger) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claims
}

func (c *countingLedger) rows(t *testing.T) []Run {
	t.Helper()
	rows, err := c.inner.Recent(context.Background(), 100)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	return rows
}

// --- fixtures -------------------------------------------------------------

// at is a fixed Monday (2026-06-08) so weekday-restricted crons are stable.
func tickAt(h, m, s int) time.Time {
	return time.Date(2026, time.June, 8, h, m, s, 0, time.UTC)
}

func roleOrg(schedules ...org.Schedule) *org.Organization {
	if len(schedules) == 0 {
		schedules = []org.Schedule{{Name: "smoke", Cron: "0 9 * * *", Task: "run smoke tests"}}
	}
	return &org.Organization{
		Name: "Acme",
		Roles: []*org.Role{
			{Name: "QA Engineer", DeclaredHandle: "qa", Schedules: schedules},
		},
	}
}

func unitOrg(target org.ScheduleTarget) *org.Organization {
	return &org.Organization{
		Name: "Acme",
		Units: []*org.OrgUnit{{
			Name: "Quality",
			Type: org.UnitTypeTeam,
			Lead: "QA Lead",
			Roles: []*org.Role{
				{Name: "QA Lead", DeclaredHandle: "qa-lead"},
				{Name: "QA Dev", DeclaredHandle: "qa-dev"},
			},
			Schedules: []org.Schedule{{
				Name: "standup", Cron: "0 9 * * *", Task: "standup", Target: target,
			}},
		}},
	}
}

// harness is a scheduler with the fakes it was built from.
type harness struct {
	t      *testing.T
	s      *Scheduler
	q      *fakeQueue
	ledger *countingLedger
	org    *org.Organization
	mu     sync.Mutex
}

func build(t *testing.T, o *org.Organization, apply ...func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, q: newQueue(), ledger: newCountingLedger(), org: o}
	opts := Options{
		Publisher: h.q,
		Org:       h.currentOrg,
		Ledger:    h.ledger,
	}
	for _, fn := range apply {
		fn(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.s = s
	return h
}

func (h *harness) currentOrg() *org.Organization {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.org
}

// reload swaps the company, the way engine.reload_config does.
func (h *harness) reload(o *org.Organization) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.org = o
}

// seed pretends the previous evaluated tick ended at t, putting the scheduler
// in WINDOW mode. Without it every case would be a first tick, which is the
// catchup path rather than the ordinary one.
func (h *harness) seed(t time.Time) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	h.s.lastTick = t
}

func (h *harness) lastTick() time.Time {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return h.s.lastTick
}

func (h *harness) tick(now time.Time) int {
	h.t.Helper()
	return h.s.Tick(context.Background(), now)
}

// --- construction ---------------------------------------------------------

func TestNewRefusesAnUnusableWiring(t *testing.T) {
	t.Parallel()
	ok := Options{Publisher: newQueue(), Org: func() *org.Organization { return nil }, Ledger: NewMemoryLedger()}
	for _, tc := range []struct {
		name string
		opts Options
		want error
	}{
		{"no publisher", func() Options { o := ok; o.Publisher = nil; return o }(), ErrNoPublisher},
		{"no org", func() Options { o := ok; o.Org = nil; return o }(), ErrNoOrg},
		// Not defaulted to the memory twin. A process-local claim is
		// indistinguishable from a correct one until there are two nodes,
		// and then every company gets two standups.
		{"no ledger", func() Options { o := ok; o.Ledger = nil; return o }(), ErrNoLedger},
		{"a tick at minute granularity", func() Options { o := ok; o.Tick = time.Minute; return o }(), ErrTickTooLong},
		{"a tick above minute granularity", func() Options { o := ok; o.Tick = 5 * time.Minute; return o }(), ErrTickTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tc.opts); !errors.Is(err, tc.want) {
				t.Fatalf("New = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg())
	if h.s.tick != DefaultTick {
		t.Errorf("tick = %v, want %v", h.s.tick, DefaultTick)
	}
	if h.s.defaultTZ != DefaultTimezone {
		t.Errorf("timezone = %q, want %q", h.s.defaultTZ, DefaultTimezone)
	}
	if h.s.catchupMin != DefaultCatchupMin || h.s.catchupMax != DefaultCatchupMax {
		t.Errorf("catchup = [%v, %v], want [%v, %v]",
			h.s.catchupMin, h.s.catchupMax, DefaultCatchupMin, DefaultCatchupMax)
	}
	if h.s.RetentionFloor() != DefaultCatchupMax {
		t.Errorf("RetentionFloor = %v, want the catchup ceiling %v — a ledger row a tick could "+
			"still evaluate must survive the sweep", h.s.RetentionFloor(), DefaultCatchupMax)
	}
}

func TestAnInvertedCatchupWindowCollapsesToItsFloor(t *testing.T) {
	t.Parallel()
	// A max below the min is a config nobody meant. Taking the min is the
	// conservative reading: it can only SHORTEN the window a missed fire is
	// replayed in, never lengthen it.
	h := build(t, roleOrg(), func(o *Options) {
		o.CatchupMin = time.Hour
		o.CatchupMax = time.Minute
	})
	if h.s.catchupMax != time.Hour {
		t.Fatalf("catchupMax = %v, want it raised to the min (%v)", h.s.catchupMax, time.Hour)
	}
}

// --- role schedule --------------------------------------------------------

func TestARoleScheduleFiresToItsOwnInbox(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg())
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("Tick = %d fires, want 1", got)
	}

	tasks := h.q.inboxTasks()
	if len(tasks) != 1 {
		t.Fatalf("inbox tasks = %d, want 1", len(tasks))
	}
	if tasks[0].topic != "crewlet.agent.qa.inbox" {
		t.Fatalf("topic = %q, want the seat's inbox", tasks[0].topic)
	}
	payload := tasks[0].ev.Payload
	for field, want := range map[string]any{
		"task_description": "run smoke tests",
		"scheduled":        true,
		"schedule_name":    "smoke",
		"scope_type":       "role",
		"scope_id":         "qa",
		// Three minutes. A scheduled turn is a ritual, not open-ended work,
		// and the cap is what releases the seat long before the next fire.
		"timeout_seconds": 180,
	} {
		if got := payload[field]; got != want {
			t.Errorf("payload[%q] = %v, want %v", field, got, want)
		}
	}

	// The id is DERIVED from (org name, handle) — the same value every node
	// computes, and the one the seat's turn actually runs under.
	want, _ := org.DeriveAgentID("Acme", "qa")
	data, _ := events.DataAs[*types.TaskAssigned](tasks[0].ev)
	if data.Agent != want.String() {
		t.Errorf("agent id = %q, want the derived %q", data.Agent, want)
	}
	if data.RoleName != "QA Engineer" {
		t.Errorf("role = %q, want the seat's name", data.RoleName)
	}
	if h.q.lifecycleEvents() != 1 {
		t.Errorf("ScheduledTaskFired events = %d, want 1", h.q.lifecycleEvents())
	}
}

func TestASecondEvaluationOfOneMinuteIsDeduped(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg())
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("first Tick = %d, want 1", got)
	}
	// Rewind the window so the 09:00 fire is in range again. The ledger
	// claim is what refuses it — nothing in the tick remembers.
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 0 {
		t.Fatalf("second Tick = %d, want 0", got)
	}
	if got := len(h.q.inboxTasks()); got != 1 {
		t.Fatalf("inbox tasks = %d, want 1", got)
	}
}

func TestAFireRecordsItsTraceAndSharesItWithTheTask(t *testing.T) {
	t.Parallel()
	// Each fire is its own trace: the ledger row stores it and the
	// TaskAssigned carries it, so the turn joins the same trace. This is
	// what the dashboard's "view calls" link follows.
	h := build(t, roleOrg())
	h.seed(tickAt(8, 59, 30))
	h.tick(tickAt(9, 0, 30))

	rows := h.ledger.rows(t)
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rows))
	}
	if rows[0].Outcome != OutcomeFired {
		t.Errorf("outcome = %q, want %q", rows[0].Outcome, OutcomeFired)
	}
	if rows[0].TraceID == "" {
		t.Fatal("the ledger row carries no trace id")
	}
	if got := h.q.inboxTasks()[0].ev.TraceID; got != rows[0].TraceID {
		t.Fatalf("task trace %q != ledger trace %q — the dashboard link goes nowhere",
			got, rows[0].TraceID)
	}
}

func TestEachMemberRunGetsItsOwnTrace(t *testing.T) {
	t.Parallel()
	h := build(t, unitOrg(org.TargetEach))
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 2 {
		t.Fatalf("Tick = %d fires, want 2", got)
	}
	seen := map[string]struct{}{}
	for _, p := range h.q.inboxTasks() {
		if p.ev.TraceID == "" {
			t.Fatal("a fire carried no trace id")
		}
		seen[p.ev.TraceID] = struct{}{}
	}
	if len(seen) != 2 {
		t.Fatalf("distinct traces = %d, want 2 — each member's run is self-contained", len(seen))
	}
}

// --- unit delivery --------------------------------------------------------

func TestUnitEachFansOutToEveryDirectMember(t *testing.T) {
	t.Parallel()
	h := build(t, unitOrg(org.TargetEach))
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 2 {
		t.Fatalf("Tick = %d fires, want 2", got)
	}
	requireTopics(t, h.q.inboxTopics(), "crewlet.agent.qa-lead.inbox", "crewlet.agent.qa-dev.inbox")
}

func TestUnitTargetLeadFiresOnlyToTheLead(t *testing.T) {
	t.Parallel()
	h := build(t, unitOrg(org.TargetLead))
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("Tick = %d fires, want 1", got)
	}
	requireTopics(t, h.q.inboxTopics(), "crewlet.agent.qa-lead.inbox")
}

func TestAnEmptyTargetMeansEach(t *testing.T) {
	t.Parallel()
	// The zero ScheduleTarget. A unit schedule with no target written is the
	// common case, and reading the zero as anything but `each` would make
	// the default silently different from the documented one.
	h := build(t, unitOrg(""))
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 2 {
		t.Fatalf("Tick = %d fires, want 2", got)
	}
}

func TestOneMembersPublishFailureDoesNotBlockItsSiblings(t *testing.T) {
	t.Parallel()
	h := build(t, unitOrg(org.TargetEach))
	h.q.refuse["crewlet.agent.qa-dev.inbox"] = errors.New("broker refused the publish")
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("Tick = %d fires, want 1 — the healthy member still runs", got)
	}
	requireTopics(t, h.q.inboxTopics(), "crewlet.agent.qa-lead.inbox")
}

func TestAFailedPublishKeepsItsClaim(t *testing.T) {
	t.Parallel()
	// The at-most-once side of the trade, asserted rather than assumed. A
	// publish failure is AMBIGUOUS — the broker may have persisted the event
	// and lost the acknowledgement — so releasing the claim to retry would
	// risk waking the seat twice. The fire is dropped and the row stands.
	h := build(t, roleOrg())
	h.q.refuse["crewlet.agent.qa.inbox"] = errors.New("broker refused the publish")
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}
	rows := h.ledger.rows(t)
	if len(rows) != 1 || rows[0].Outcome != OutcomeFired {
		t.Fatalf("ledger rows = %v, want one 'fired' row — the claim is spent", rows)
	}
	// And a later tick does not re-fire it.
	h.q.refuse = map[string]error{}
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 0 {
		t.Fatalf("re-tick = %d, want 0", got)
	}
}

func TestAHandleNamingNoSeatDoesNotBurnTheClaim(t *testing.T) {
	t.Parallel()
	// A decommissioned role, or a config edit landing between runner
	// resolution and the fire. A later tick against a corrected org must
	// still be able to fire, so nothing may be claimed.
	h := build(t, roleOrg())
	entry := Entries(h.currentOrg())[0]
	loc := time.UTC
	if h.s.fire(context.Background(), h.currentOrg(), entry, "ghost", tickAt(9, 0, 0), loc) {
		t.Fatal("fire to an unknown handle reported success")
	}
	if got := h.ledger.count(); got != 0 {
		t.Fatalf("the ledger saw %d claims, want 0", got)
	}
	if got := len(h.q.inboxTasks()); got != 0 {
		t.Fatalf("published %d tasks, want 0", got)
	}
}

func TestAHumanSeatIsNeverARunner(t *testing.T) {
	t.Parallel()
	// Humans are addressable and run no turns, so a fire addressed to one
	// would sit in an inbox nothing consumes. `each` filters them; a `lead`
	// schedule under a human lead resolves to nobody at all, which surfaces
	// as schedule_no_runners rather than as a silent nothing.
	human := &org.Role{
		Name: "Sarah Chen", Kind: org.KindHuman,
		Contact: &org.HumanContact{SlackUserID: "U0HUMAN"},
	}
	each := &org.Organization{Name: "Acme", Units: []*org.OrgUnit{{
		Name: "Quality", Type: org.UnitTypeTeam, Lead: "QA Lead",
		Roles: []*org.Role{{Name: "QA Lead", DeclaredHandle: "qa-lead"}, human},
		Schedules: []org.Schedule{{
			Name: "standup", Cron: "0 9 * * *", Task: "standup", Target: org.TargetEach,
		}},
	}}}
	h := build(t, each)
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("Tick = %d fires, want 1 (the agent only)", got)
	}
	requireTopics(t, h.q.inboxTopics(), "crewlet.agent.qa-lead.inbox")

	lead := &org.Organization{Name: "Acme", Units: []*org.OrgUnit{{
		Name: "Quality", Type: org.UnitTypeTeam, Lead: "Sarah Chen",
		Roles: []*org.Role{human, {Name: "QA Dev", DeclaredHandle: "qa-dev"}},
		Schedules: []org.Schedule{{
			Name: "report", Cron: "0 9 * * *", Task: "report", Target: org.TargetLead,
		}},
	}}}
	h2 := build(t, lead)
	h2.seed(tickAt(8, 59, 30))
	if got := h2.tick(tickAt(9, 0, 30)); got != 0 {
		t.Fatalf("Tick = %d fires, want 0 — a human lead runs nothing", got)
	}
	if got := h2.ledger.count(); got != 0 {
		t.Fatalf("the ledger saw %d claims, want 0 — a fire with no runner must not be recorded", got)
	}
}

func TestADisabledScheduleDoesNotFire(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg(org.Schedule{
		Name: "s", Cron: "0 9 * * *", Task: "x", Enabled: org.Off(),
	}))
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}
	if got := h.ledger.count(); got != 0 {
		t.Fatalf("the ledger saw %d claims, want 0 — a disabled schedule is never evaluated", got)
	}
}

func TestAnUnparseableScheduleDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()
	// Failure is per-fire. Config validation rejects both of these up front,
	// so reaching the tick means a hand-built org or a schema that moved —
	// and either way one typo must not stop a company's ritual calendar.
	h := build(t, roleOrg(
		org.Schedule{Name: "bad-cron", Cron: "not a cron at all", Task: "x"},
		org.Schedule{Name: "bad-zone", Cron: "0 9 * * *", Task: "x", Timezone: "Mars/Olympus"},
		org.Schedule{Name: "good", Cron: "0 9 * * *", Task: "x"},
	))
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("Tick = %d fires, want 1 — the healthy schedule still runs", got)
	}
}

// --- timezone -------------------------------------------------------------

func TestADSTFallbackFiresOnce(t *testing.T) {
	t.Parallel()
	// Europe/Amsterdam falls back 2026-10-25 03:00 -> 02:00, so local 02:30
	// happens at both 00:30 and 01:30 UTC. A `30 2 * * *` schedule fires
	// ONCE, deduped on the shared local fire label.
	h := build(t, roleOrg(org.Schedule{
		Name: "nightly", Cron: "30 2 * * *", Task: "x", Timezone: "Europe/Amsterdam",
	}))
	h.seed(time.Date(2026, time.October, 25, 0, 29, 30, 0, time.UTC))
	if got := h.tick(time.Date(2026, time.October, 25, 0, 30, 30, 0, time.UTC)); got != 1 {
		t.Fatalf("first instant: Tick = %d, want 1", got)
	}
	h.seed(time.Date(2026, time.October, 25, 1, 29, 30, 0, time.UTC))
	if got := h.tick(time.Date(2026, time.October, 25, 1, 30, 30, 0, time.UTC)); got != 0 {
		t.Fatalf("second instant: Tick = %d, want 0 — one local minute is one fire", got)
	}
	if got := len(h.q.inboxTasks()); got != 1 {
		t.Fatalf("inbox tasks = %d, want 1", got)
	}
}

func TestAScheduleFiresOnItsOwnZoneNotUTC(t *testing.T) {
	t.Parallel()
	sch := org.Schedule{Name: "ams", Cron: "30 9 * * *", Task: "x", Timezone: "Europe/Amsterdam"}

	// 09:30 Amsterdam (CEST) is 07:30 UTC in June.
	h := build(t, roleOrg(sch))
	h.seed(tickAt(7, 29, 30))
	if got := h.tick(tickAt(7, 30, 30)); got != 1 {
		t.Fatalf("Tick at 07:30Z = %d, want 1", got)
	}

	// And it does not fire at 09:30 UTC.
	h2 := build(t, roleOrg(sch))
	h2.seed(tickAt(9, 29, 30))
	if got := h2.tick(tickAt(9, 30, 30)); got != 0 {
		t.Fatalf("Tick at 09:30Z = %d, want 0", got)
	}
}

func TestTheDefaultTimezoneAppliesToAScheduleThatNamesNone(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg(org.Schedule{Name: "ams", Cron: "30 9 * * *", Task: "x"}),
		func(o *Options) { o.DefaultTimezone = "Europe/Amsterdam" })
	h.seed(tickAt(7, 29, 30))
	if got := h.tick(tickAt(7, 30, 30)); got != 1 {
		t.Fatalf("Tick = %d, want 1 — the system default zone applies", got)
	}
}

// --- catchup --------------------------------------------------------------

func TestCatchupFiresARecentlyMissedTick(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg())
	// No prior tick, two minutes after a missed 09:00 fire.
	if got := h.tick(tickAt(9, 2, 0)); got != 1 {
		t.Fatalf("Tick = %d, want 1", got)
	}
	requireTopics(t, h.q.inboxTopics(), "crewlet.agent.qa.inbox")
}

func TestCatchupRecordsASkipOutsideTheWindow(t *testing.T) {
	t.Parallel()
	// A daily schedule's window clamps to the two-hour ceiling, so a 09:00
	// fire seen at 17:00 is far outside it: skipped, not fired, and RECORDED
	// so an operator asking "why did the standup not run" gets an answer.
	h := build(t, roleOrg())
	if got := h.tick(tickAt(17, 0, 0)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}
	if got := len(h.q.inboxTasks()); got != 0 {
		t.Fatalf("published %d tasks, want 0", got)
	}
	rows := h.ledger.rows(t)
	if len(rows) != 1 || rows[0].Outcome != OutcomeSkippedCatchup {
		t.Fatalf("ledger rows = %v, want one skipped_catchup row", rows)
	}
	if rows[0].TargetHandle != "" {
		t.Fatalf("the skip row names runner %q, want none — an empty handle is what keeps it "+
			"from colliding with a fire row for the same minute", rows[0].TargetHandle)
	}
}

func TestASkipRowDoesNotSuppressALaterFireOfThatMinute(t *testing.T) {
	t.Parallel()
	// The consequence of the empty handle, stated as behaviour. The skip and
	// the fire are different identities, so a corrected later tick can still
	// dispatch the minute that was skipped.
	h := build(t, roleOrg())
	if got := h.tick(tickAt(17, 0, 0)); got != 0 {
		t.Fatalf("catchup Tick = %d, want 0", got)
	}
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("later Tick = %d, want 1 — the skip row must not consume the fire's claim", got)
	}
}

func TestCatchupDisabledFiresNothing(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg(org.Schedule{
		Name: "s", Cron: "0 9 * * *", Task: "x", Catchup: org.Off(),
	}))
	if got := h.tick(tickAt(9, 2, 0)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}
	if got := h.ledger.count(); got != 0 {
		t.Fatalf("the ledger saw %d claims, want 0 — opting out records nothing either", got)
	}
}

func TestCatchupUsesHalfTheSchedulesOwnPeriod(t *testing.T) {
	t.Parallel()
	// A five-minute schedule's window is half its period — 150 s — which the
	// floor then raises to the two-minute minimum, so 150 s stands. A fire
	// missed by four minutes is outside it; one missed by two is inside.
	// This is what stops a frequent schedule from replaying a fire that is
	// closer to the NEXT one than to itself.
	sch := org.Schedule{Name: "often", Cron: "*/5 * * * *", Task: "x"}

	inside := build(t, roleOrg(sch))
	if got := inside.tick(tickAt(9, 2, 0)); got != 1 {
		t.Fatalf("two minutes late: Tick = %d, want 1", got)
	}
	outside := build(t, roleOrg(sch))
	if got := outside.tick(tickAt(9, 4, 0)); got != 0 {
		t.Fatalf("four minutes late: Tick = %d, want 0", got)
	}
	rows := outside.ledger.rows(t)
	if len(rows) != 1 || rows[0].Outcome != OutcomeSkippedCatchup {
		t.Fatalf("ledger rows = %v, want one skipped_catchup row", rows)
	}
}

func TestCatchupIsEvaluatedOnlyOnTheFirstTick(t *testing.T) {
	t.Parallel()
	// The second tick is a WINDOW tick, and a window that contains no fire
	// dispatches nothing — the catchup pass does not run again and re-offer
	// a fire already considered.
	h := build(t, roleOrg())
	if got := h.tick(tickAt(9, 2, 0)); got != 1 {
		t.Fatalf("first Tick = %d, want 1", got)
	}
	if got := h.tick(tickAt(9, 3, 0)); got != 0 {
		t.Fatalf("second Tick = %d, want 0", got)
	}
}

func TestAScheduleWithNoReachableFireCatchesUpNothing(t *testing.T) {
	t.Parallel()
	// February 30th. Prev finds nothing within the horizon, so the catchup
	// pass has nothing to consider — and must not record a skip for a fire
	// that does not exist.
	h := build(t, roleOrg(org.Schedule{Name: "never", Cron: "0 0 30 2 *", Task: "x"}))
	if got := h.tick(tickAt(9, 2, 0)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}
	if got := h.ledger.count(); got != 0 {
		t.Fatalf("the ledger saw %d claims, want 0", got)
	}
}

// --- the window -----------------------------------------------------------

func TestAWindowTickCoversEveryFireInsideIt(t *testing.T) {
	t.Parallel()
	// A tick that ran late must not lose the fires it stepped over. Three
	// minutes of a per-minute schedule in one tick is three dispatches.
	h := build(t, roleOrg(org.Schedule{Name: "minutely", Cron: "* * * * *", Task: "x"}))
	h.seed(tickAt(9, 0, 0))
	if got := h.tick(tickAt(9, 3, 0)); got != 3 {
		t.Fatalf("Tick = %d fires, want 3 (09:01, 09:02, 09:03)", got)
	}
}

func TestConsecutiveWindowsPartitionTime(t *testing.T) {
	t.Parallel()
	// A fire landing exactly on a tick boundary belongs to ONE window. The
	// ledger would absorb the duplicate, so this is asserted at the tick
	// level where the bug would otherwise be invisible.
	h := build(t, roleOrg(org.Schedule{Name: "minutely", Cron: "* * * * *", Task: "x"}))
	h.seed(tickAt(9, 0, 0))
	if got := h.tick(tickAt(9, 1, 0)); got != 1 {
		t.Fatalf("first window = %d, want 1", got)
	}
	if got := h.tick(tickAt(9, 2, 0)); got != 1 {
		t.Fatalf("second window = %d, want 1", got)
	}
	if got := len(h.q.inboxTasks()); got != 2 {
		t.Fatalf("inbox tasks = %d, want 2", got)
	}
}

func TestAnEvaluatedTickAdvancesTheWindow(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg())
	at := tickAt(9, 0, 30)
	h.tick(at)
	if got := h.lastTick(); !got.Equal(at) {
		t.Fatalf("lastTick = %v, want %v", got, at)
	}
}

// --- hot reload -----------------------------------------------------------

func TestHotReloadPicksUpANewSchedule(t *testing.T) {
	t.Parallel()
	h := build(t, &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{{Name: "QA", DeclaredHandle: "qa"}},
	})
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}

	// The operator hot-reloads: the same seat now has a schedule. Nothing is
	// re-wired — the tick reads the live org.
	h.reload(roleOrg())
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("Tick after reload = %d, want 1", got)
	}
}

func TestHotReloadDroppingAScheduleStopsIt(t *testing.T) {
	t.Parallel()
	// The other direction, which the Python suite never sent. A removed
	// schedule must stop firing on the next tick, not on the next restart.
	h := build(t, roleOrg())
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("Tick = %d, want 1", got)
	}
	h.reload(&org.Organization{Name: "Acme", Roles: []*org.Role{{Name: "QA", DeclaredHandle: "qa"}}})
	h.seed(tickAt(9, 59, 30))
	if got := h.tick(tickAt(10, 0, 30)); got != 0 {
		t.Fatalf("Tick after removal = %d, want 0", got)
	}
}

func TestATickAgainstNoCompanyFiresNothing(t *testing.T) {
	t.Parallel()
	// A nil org is what a node serving no active configuration reads. It
	// must be quiet rather than panicking one frame into the walk.
	h := build(t, nil)
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}
}

// --- jitter ---------------------------------------------------------------

func TestJitterDelaysFiringDeterministically(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg(), func(o *Options) { o.Jitter = 45 * time.Second })
	offset := h.s.jitterFor("qa", "smoke")
	if offset <= 0 || offset > 45*time.Second {
		t.Fatalf("jitter offset = %v, want it inside (0, 45s] for this fixture", offset)
	}
	fire := tickAt(9, 0, 0)

	// One second before the jittered instant: not yet due.
	before := fire.Add(offset - time.Second)
	h.seed(fire.Add(-time.Minute))
	if got := h.tick(before); got != 0 {
		t.Fatalf("before the offset: Tick = %d, want 0", got)
	}
	// At it: fires.
	h.seed(before)
	if got := h.tick(fire.Add(offset)); got != 1 {
		t.Fatalf("at the offset: Tick = %d, want 1", got)
	}

	// And firing exactly on the minute is too early.
	h2 := build(t, roleOrg(), func(o *Options) { o.Jitter = 45 * time.Second })
	h2.seed(fire.Add(-time.Minute))
	if got := h2.tick(fire); got != 0 {
		t.Fatalf("on the minute: Tick = %d, want 0", got)
	}
}

func TestJitterIsStableAcrossNodesAndDistinctBetweenSchedules(t *testing.T) {
	t.Parallel()
	// Deterministic, because two nodes computing different offsets would put
	// their windows out of step and let a fire fall between them. And spread,
	// because an offset that collapsed to one value would not smooth
	// anything.
	a := build(t, roleOrg(), func(o *Options) { o.Jitter = time.Minute })
	b := build(t, roleOrg(), func(o *Options) { o.Jitter = time.Minute })
	if a.s.jitterFor("qa", "smoke") != b.s.jitterFor("qa", "smoke") {
		t.Fatal("two schedulers computed different offsets for one schedule")
	}
	seen := map[time.Duration]struct{}{}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		seen[a.s.jitterFor("qa", name)] = struct{}{}
	}
	if len(seen) < 4 {
		t.Fatalf("eight schedules produced %d distinct offsets — the spread is not spreading", len(seen))
	}
}

func TestNoJitterFiresExactlyOnTheMinute(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg())
	if got := h.s.jitterFor("qa", "smoke"); got != 0 {
		t.Fatalf("offset = %v with jitter off, want 0", got)
	}
}

func TestSubSecondJitterIsNoJitter(t *testing.T) {
	t.Parallel()
	// Quantised to whole seconds. A sub-second window rounds to nothing
	// rather than to a divide-by-zero, and the canonical fire minute is what
	// forms the identity anyway.
	h := build(t, roleOrg(), func(o *Options) { o.Jitter = 500 * time.Millisecond })
	if got := h.s.jitterFor("qa", "smoke"); got != 0 {
		t.Fatalf("offset = %v for a sub-second window, want 0", got)
	}
}

// --- the config-posture gate ----------------------------------------------

func TestAShedTickFiresNothingAndLeavesItsWindowOpen(t *testing.T) {
	t.Parallel()
	// A node that cannot apply the current epoch must not fire the PREVIOUS
	// company's schedules: crons that were edited, seats that were deleted,
	// schedules removed outright. Unlike a delivery there is no queued copy
	// to fall back on, so the tick skips whole.
	h := build(t, roleOrg(), func(o *Options) { o.Admits = func() bool { return false } })
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}
	if got := h.ledger.count(); got != 0 {
		t.Fatalf("the ledger saw %d claims, want 0", got)
	}
	if got := h.lastTick(); !got.Equal(tickAt(8, 59, 30)) {
		t.Fatalf("lastTick moved to %v — a tick that evaluated nothing must not close a "+
			"window nobody looked at", got)
	}
}

func TestAConvergedNodeCatchesUpTheWindowItShed(t *testing.T) {
	t.Parallel()
	// The consequence of leaving the window open, and the reason it matters.
	// A node that sheds from boot stays on its FIRST tick, so when it
	// converges it evaluates the catchup window rather than a window
	// stretching back to boot.
	admits := false
	h := build(t, roleOrg(), func(o *Options) { o.Admits = func() bool { return admits } })
	if got := h.tick(tickAt(9, 0, 30)); got != 0 {
		t.Fatalf("shed Tick = %d, want 0", got)
	}
	admits = true
	if got := h.tick(tickAt(9, 2, 0)); got != 1 {
		t.Fatalf("converged Tick = %d, want 1 — the missed 09:00 fire is inside the window", got)
	}
}

// --- the fleet-singleton duty ---------------------------------------------

func TestANodeWithoutTheDutyFiresNothing(t *testing.T) {
	t.Parallel()
	// Every node enumerating every schedule is pure duplicated work. Not
	// incorrect — the ledger claim means all but one lose the race on every
	// due fire — but it is N walks of the org and N claim round trips per
	// tick to produce one dispatch.
	h := build(t, roleOrg(), func(o *Options) {
		o.Duty = func(context.Context) (bool, error) { return false, nil }
	})
	h.seed(tickAt(8, 59, 0))
	if got := h.tick(tickAt(9, 0, 0)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}
	if got := h.ledger.count(); got != 0 {
		t.Fatalf("the ledger saw %d claims, want 0 — the point is not reaching the store", got)
	}
}

func TestANodeWithoutTheDutyLeavesItsWindowOpen(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg(), func(o *Options) {
		o.Duty = func(context.Context) (bool, error) { return false, nil }
	})
	if !h.lastTick().IsZero() {
		t.Fatal("a fresh scheduler already has a window behind it")
	}
	h.tick(tickAt(9, 0, 0))
	if !h.lastTick().IsZero() {
		t.Fatalf("lastTick moved to %v — a node that never wins the duty must stay on its "+
			"first tick, so that winning one later evaluates the catchup window rather than "+
			"a window stretching back to boot", h.lastTick())
	}
}

func TestTheDutyHolderFiresNormally(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg(), func(o *Options) {
		o.Duty = func(context.Context) (bool, error) { return true, nil }
	})
	h.seed(tickAt(8, 59, 0))
	if got := h.tick(tickAt(9, 0, 0)); got != 1 {
		t.Fatalf("Tick = %d, want 1", got)
	}
}

func TestADutyClaimThatFailsFiresNothing(t *testing.T) {
	t.Parallel()
	// Fail closed. An unreadable lease store says nothing about who holds
	// the duty, and firing on that basis puts every node back to racing the
	// ledger claim — the exact duplicated work the duty exists to remove.
	h := build(t, roleOrg(), func(o *Options) {
		o.Duty = func(context.Context) (bool, error) {
			return false, errors.New("lease store unreachable")
		}
	})
	h.seed(tickAt(8, 59, 0))
	if got := h.tick(tickAt(9, 0, 0)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}
	if !h.lastTick().Equal(tickAt(8, 59, 0)) {
		t.Fatalf("lastTick moved to %v, want the window left open", h.lastTick())
	}
}

func TestADutyClaimThatSaysTrueAndAlsoErrorsIsNotTrusted(t *testing.T) {
	t.Parallel()
	// A backend answering (true, err) is confused about its own state, and
	// the safe reading of a confused answer is the same as of an unknown
	// one. The alternative — trusting the boolean — makes a store that
	// returns a zero value beside its error look like a granted duty.
	h := build(t, roleOrg(), func(o *Options) {
		o.Duty = func(context.Context) (bool, error) { return true, errors.New("half an answer") }
	})
	h.seed(tickAt(8, 59, 0))
	if got := h.tick(tickAt(9, 0, 0)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}
}

func TestASingleNodeHasNoDutyToClaim(t *testing.T) {
	t.Parallel()
	// There is no fleet to be a singleton within, so the default must be
	// unchanged from the pre-duty behaviour.
	h := build(t, roleOrg())
	if h.s.duty != nil {
		t.Fatal("a scheduler built with no Duty has one anyway")
	}
	h.seed(tickAt(8, 59, 0))
	if got := h.tick(tickAt(9, 0, 0)); got != 1 {
		t.Fatalf("Tick = %d, want 1", got)
	}
}

// --- the ledger seam ------------------------------------------------------

func TestAnUnreadableLedgerFiresNothing(t *testing.T) {
	t.Parallel()
	// Fail closed, the same polarity as the duty and for a sharper reason:
	// not knowing whether a fire was already claimed has exactly one safe
	// answer, and it is "do not fire".
	h := build(t, roleOrg())
	h.ledger.err = errors.New("store unreachable")
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 0 {
		t.Fatalf("Tick = %d, want 0", got)
	}
	if got := len(h.q.inboxTasks()); got != 0 {
		t.Fatalf("published %d tasks on an unreadable ledger, want 0", got)
	}
	// And the fire is not lost: the window has closed, but the next tick
	// against a healthy store still has the ledger row absent, so a rewound
	// window fires it.
	h.ledger.err = nil
	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("Tick after recovery = %d, want 1", got)
	}
}

func TestClaimHappensBeforeThePublish(t *testing.T) {
	t.Parallel()
	// Order, not merely presence. A publish that preceded its claim would
	// turn at-most-once into at-least-once, and the only visible difference
	// is under a REFUSED claim — which is what this drives.
	h := build(t, roleOrg())
	h.seed(tickAt(8, 59, 30))
	h.tick(tickAt(9, 0, 30))
	before := len(h.q.published)

	h.seed(tickAt(8, 59, 30))
	if got := h.tick(tickAt(9, 0, 30)); got != 0 {
		t.Fatalf("re-tick = %d, want 0", got)
	}
	if got := len(h.q.published); got != before {
		t.Fatalf("a refused claim still published %d event(s) — the claim does not gate the "+
			"dispatch", got-before)
	}
}

// --- Run ------------------------------------------------------------------

func TestRunTicksUntilItsContextIsDone(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg(org.Schedule{Name: "minutely", Cron: "* * * * *", Task: "x"}),
		func(o *Options) { o.Tick = time.Millisecond })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.s.Run(ctx)
		close(done)
	}()

	// The first tick is one interval in, deliberately: it is the tick that
	// evaluates catchup, and running it before the rest of the engine is up
	// would dispatch into subscriptions that do not exist yet.
	deadline := time.After(2 * time.Second)
	for h.lastTick().IsZero() {
		select {
		case <-deadline:
			t.Fatal("Run did not tick within 2s")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of its context being cancelled")
	}
}

// --- helpers --------------------------------------------------------------

func requireTopics(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("inbox topics = %v, want %v", got, want)
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			t.Fatalf("inbox topics = %v, want %v", got, want)
		}
		seen[w]--
	}
}
