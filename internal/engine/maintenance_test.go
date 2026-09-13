package engine_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/schedule"
)

// THE assertion whose absence was the bug. Every one of these tables ships a
// Purge and an index for it, and every migration says the rows are swept —
// and if nothing ever calls any of them, all of them grow for the life of the
// deployment. A store gaining a Purge with no entry here
// now fails this list rather than going quietly unswept.
func TestTheEngineSweepsEveryShortHorizonTable(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})

	w := e.Maintenance()
	if w == nil {
		t.Fatal("the engine armed no retention sweep")
	}
	// STARTED, not merely constructed. A worker that was built and never
	// run looks identical from every other vantage point to one quietly
	// doing its job — which is exactly how a full set of Purge methods
	// ships with nothing ever calling them.
	if !w.Running() {
		t.Fatal("the sweep was built but never started")
	}
	got := w.Jobs()
	slices.Sort(got)
	want := []string{
		// NOT HERE: webhook_deliveries, rate_limits, turn_completions
		// and config_apply_status. All four moved to the coordination
		// store, where a bucket's own age is the retention and the
		// BROKER expires the records — so there is nothing on this node
		// left to sweep.
		"a2a_channels",
		"a2a_channels_idle",
		// The diary sweep earns its place here the hard way: Expire
		// shipped with the diary, diary.go described the background
		// sweep, and for as long as this list did not name it, nothing
		// anywhere called it — expired short-term memories stayed rows
		// every recall scanned, forever.
		"agent_diary",
		"agent_diary_long",
		"chat_thread_follows",
		"conversation_sessions",
		// Added the same way the diary was: the table shipped with a
		// memsync entry that republishes every row to every peer on
		// every cycle, and no horizon anywhere — so it grew with the
		// deployment's AGE rather than its size, and this list not
		// naming it was the only place that showed.
		"counterparty_profiles",
		"events",
		"scheduled_runs",
		// The one job that deletes BROKER state rather than rows: a
		// removed seat's mailbox, retired after its grace period. Named
		// here because a mailbox nothing retires retains mail for a seat
		// nobody runs for the life of the deployment, and the only symptom
		// is a stream that never shrinks.
		"seat_mailboxes",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("swept tables:\n got %v\nwant %v", got, want)
	}
}

// THE NODE THE ENGINE BUILDS REGISTERS EVERY SEAT'S MAILBOX WITH THE FLEET.
// The sweep above can only retire a mailbox the registry remembers, and the
// registry is fed by the node's walk. A node built without it creates every
// mailbox exactly as before and remembers none, so every removed seat's mail is
// retained for ever with nothing failing anywhere: the unit suites in node and
// maintenance each pass against their own fakes while the engine joins neither.
func TestTheEngineRegistersEverySeatMailboxWithTheFleet(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})

	e.Node().EnsureMailboxes(t.Context())

	records, err := e.Backends().Fleet.Mailboxes(t.Context())
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	var handles []string
	for _, rec := range records {
		handles = append(handles, rec.Handle)
	}
	var seats []string
	for _, seat := range e.Company().Seats() {
		seats = append(seats, seat.Handle)
	}
	slices.Sort(seats)
	if len(seats) == 0 || !slices.Equal(handles, seats) {
		t.Fatalf("registered mailboxes %v, want every agent seat %v", handles, seats)
	}
}

// A GRACEFUL STOP GIVES EVERY FLEET DUTY BACK, AND ONLY THE DUTIES.
//
// A duty is claimed per tick and its lease outlives several ticks (45 minutes
// for this sweep, three hours for the skill curator), and a restarted process
// is a new incarnation that cannot re-claim what the old one held. Kept, every
// deploy that restarted the holder left the duty dark for its whole TTL. A
// setup hold under the same prefix is different: it belongs to a pass that may
// still be running, and giving it back would let a second writer in mid-pass.
func TestAStoppedEngineGivesItsDutiesBackAndKeepsItsHolds(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	ctx := context.Background()
	leases := e.Backends().Coord
	owner := e.Node().Owner()

	if _, err := e.Maintenance().Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	duty := coord.WorkerResource("maintenance")
	if held, err := leases.Get(ctx, duty); err != nil || held == nil || held.Owner != owner {
		t.Fatalf("precondition: the sweep's tick did not leave %s held by this node: (%v, %v)", duty, held, err)
	}
	hold := coord.WorkerResource("setup-provision-github")
	if _, err := leases.TryAcquire(ctx, hold, coord.AcquireOptions{
		Owner: owner, TTL: 5 * time.Minute, Ungated: true,
	}); err != nil {
		t.Fatalf("precondition: take a setup hold: %v", err)
	}

	e.Stop(ctx)

	if got, err := leases.Get(ctx, duty); err != nil || got != nil {
		t.Fatalf("after a graceful stop %s reads (%v, %v), want it given back", duty, got, err)
	}
	if got, err := leases.Get(ctx, hold); err != nil || got == nil || got.Owner != owner {
		t.Fatalf("after a graceful stop the setup hold reads (%v, %v), want it left to its pass", got, err)
	}
}

// The tick must stay shorter than every horizon, or a table sits past its
// own horizon for the difference and the horizon stops describing the table.
// [maintenance.New] enforces this at runtime by raising a short horizon; this
// asserts the SHIPPED ones never need raising, so nobody discovers the rule
// from a warning in production.
func TestEveryRetentionOutlastsTheSweepInterval(t *testing.T) {
	t.Parallel()
	for table, horizon := range map[string]time.Duration{
		// NOT turn_completions: it is retained by the COORDINATION
		// bucket now, whose age no sweep here ticks against.
		// coordtest holds that horizon to the catchup ceiling.
		"scheduled_runs":        maintenance.ScheduledRunRetention,
		"conversation_sessions": maintenance.ConversationRetention,
		"a2a_channels":          maintenance.ChannelRetention,
		"a2a_channels_idle":     maintenance.ChannelIdleTimeout,
		"chat_thread_follows":   maintenance.FollowRetention,
		"counterparty_profiles": maintenance.CounterpartyRetention,
		"seat_mailboxes":        maintenance.MailboxRetirementGrace,
	} {
		if horizon <= maintenance.Interval {
			t.Errorf("%s retention (%v) is not longer than the %v tick",
				table, horizon, maintenance.Interval)
		}
	}
}

// A scheduler claim answers "did this fire already run?", so deleting a row a
// tick could still evaluate lets that fire run TWICE. Its floor is the catchup
// ceiling, not a number somebody liked. The completion ledger is held to the
// same rule in coordtest, against coord.LedgerRetention.
func TestTheScheduleHorizonOutlastsTheCatchupCeiling(t *testing.T) {
	t.Parallel()
	for name, horizon := range map[string]time.Duration{
		"scheduled_runs": maintenance.ScheduledRunRetention,
	} {
		if horizon <= schedule.DefaultCatchupMax {
			t.Errorf("%s retention (%v) can delete a row a tick could still evaluate (catchup %v)",
				name, horizon, schedule.DefaultCatchupMax)
		}
	}
}

// The operator's retention_days has existed since the conversation ledger
// shipped and nothing ever read it: setting it to 7 got you thirty days of
// conversations, silently, because there was no sweep to honour it.
func TestTheOperatorsConversationHorizonIsHonoured(t *testing.T) {
	t.Parallel()
	doc := companyDoc + `
turn_engine:
  conversation_session:
    retention_days: 3
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})

	if got := e.ConversationRetention(); got != 3*24*time.Hour {
		t.Fatalf("conversation retention = %v, want the configured 3 days", got)
	}
	// And an unset one falls back to the ledger's own default rather than
	// to zero, which would mean "delete everything on the next tick".
	plain := newEngine(t, engine.Options{})
	if got := plain.ConversationRetention(); got != 30*24*time.Hour {
		t.Fatalf("the default retention is %v, want 30 days", got)
	}
}

// The delivery horizon and the rate-window sweep are GONE, not forgotten:
// both moved to the coordination store, where the bucket's own age is the
// retention and the broker expires the records. The equivalent guard is
// coordtest's TestTheRetentionsOutlastWhatTheyCover, which holds the bucket
// ages against the cadences they have to outlast.
