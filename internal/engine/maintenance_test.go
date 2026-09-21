package engine_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/integration"
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
		// THE FOURTH DOMAIN'S LEDGER, and it is the first one on this
		// list whose horizon is not the framework's thirty days: chat
		// declares seven, because the table takes one row per applied
		// record and its size is that domain's commit rate times the
		// horizon — a log carrying conversation commits an order of
		// magnitude more often than a work tracker does. See
		// [chat.ChatOpsRetention]. It is a PER-NODE job for the same
		// reason every other `<domain>_ops` entry is.
		"chat_ops",
		"conversation_sessions",
		// Added the same way the diary was: the table shipped with a
		// memsync entry that republishes every row to every peer on
		// every cycle, and no horizon anywhere — so it grew with the
		// deployment's AGE rather than its size, and this list not
		// naming it was the only place that showed.
		"counterparty_profiles",
		"events",
		// EVERY REGISTERED DOMAIN'S OPERATION LEDGER, and they are on
		// this list for exactly the reason the list exists: each
		// `<domain>_ops` migration says the table is swept and ships
		// `<domain>_ops_swept_idx` for the range delete, and nothing
		// swept any of them — a row per applied record, kept for ever,
		// on every node. They are also the list's first PER-NODE jobs:
		// the rows record what THIS applier wrote, so under the fleet
		// singleton they would be tidied on one node and grow for ever
		// on the others.
		"pages_ops",
		// NEITHER NATIVE BACKEND SWEEPS ANY MORE, and the absence of
		// their entries is the point. The knowledge base had three —
		// a change retention, a revision prune and an orphan collector
		// — and adopting the log removed all three: the prune RIDES
		// EACH COMMIT as the record's own list of retired versions, the
		// orphans cannot occur because a create is one transaction, and
		// the history is a Replicated table an applier owns, so a
		// delete here on one node's own authority is exactly what the
		// identity claim forbids.
		"scheduled_runs",
		// The one job that deletes BROKER state rather than rows: a
		// removed seat's mailbox, retired after its grace period. Named
		// here because a mailbox nothing retires retains mail for a seat
		// nobody runs for the life of the deployment, and the only symptom
		// is a stream that never shrinks.
		"seat_mailboxes",
		// The TRACKER'S OWN JOBS, and they are a different kind of
		// thing from every entry above: nothing here deletes anything.
		// Its records are a LOG, which is trimmed by the retention gate
		// rather than swept — so what these do is finish work a crash
		// left half-done and tell the tasks a close unblocked. They are
		// on this list because the list is what says a job exists at
		// all, and a job nobody registered is a re-spread that never
		// runs and a board that stays wrong.
		"tracker_abandoned_merges",
		"tracker_duplicate_ranks",
		// AND A PERSON'S INBOX, which IS a range delete and is the one
		// entry on this list that deletes something a person reads. The
		// table shipped `tracker_notifications_swept_idx ON
		// (created_at)` naming "the per-node inbox retention sweep",
		// the horizon is a validated company setting with a default and
		// bounds, and its own doc calls it "the one horizon here that
		// deletes anything" — and nothing deleted anything, so every
		// routed change kept a row per recipient for the life of the
		// deployment, on every node.
		//
		// PER NODE, because the table is Divergent: what it holds
		// depends on the epoch's own horizon, so two nodes legitimately
		// hold different rows and a singleton would tidy one and let
		// the rest grow. What ages out is a POINTER — the history row
		// behind each notice is never swept.
		"tracker_notifications",
		// AND THE ONE-SIDED DEPENDENCY REPAIR, which is the same kind
		// of thing: a dependency is two commits on two subjects, the
		// mirror is best effort because the authored edge is durable
		// without it, and this writes the one that did not land. Its
		// repair is LOUD — the authored commit routes to the
		// DEPENDENT's parties, so the blocker's assignee was never told
		// at all — which makes it the only wake that side ever gets.
		// Unregistered, a half-written dependency stays half-written
		// and nobody hears about it.
		"tracker_one_sided",
		"tracker_ops",
		"tracker_respread",
		"tracker_unblocked",
		"vectors_ops",
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
// BACKENDS THE TEST OWNS, and what that buys is narrower than this comment
// once claimed. It used to say that reading through engine-owned backends
// makes every post-stop lease read answer "definitively not held" whichever
// the truth was. That is false in both topologies this test can run in, and
// it is worth saying why, because it asserted the exact thing
// [internal/coord] exists to deny — that an unreachable store reads as an
// empty one.
//
// Under the default coordination type the store is coordmem, which
// [engine.Backends.Close] does not touch at all: Close takes down the queue,
// the connection, the embedded server and the SQL store, and Coord is not
// among them. Under embedded-kv a post-close read is coord.ErrUnavailable,
// never (nil, nil) — kv's readOne answers (nil, nil) for a missing KEY and
// wraps every other failure. So neither configuration produces the race the
// old rationale was written against.
//
// What supplying them actually buys is a lifetime this test controls, which
// matters only if this case is ever moved onto embedded-kv: then an
// engine-owned Close would make both reads ERROR rather than observe, and an
// assertion reading only the error would call that a pass. Keeping the shape
// is cheap; keeping the old explanation was not, because a wrong WHY in a
// test is the next reader's wrong diagnosis.
//
// The reason the assertions below are observable is the HOLD NAME, and that
// reason is stated where it belongs, at the hold itself.
func TestAStoppedEngineGivesItsDutiesBackAndKeepsItsHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	boot := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	company := parsedCompany(t, companyDoc)
	backends, err := engine.OpenBackends(t.Context(), boot, company)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { backends.Close(ctx) })
	e := newEngine(t, engine.Options{Bootstrap: boot, Company: company, Backends: backends})
	leases := backends.Coord
	owner := e.Node().Owner()

	if _, err := e.Maintenance().Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	duty := coord.WorkerResource("maintenance")
	if held, err := leases.Get(ctx, duty); err != nil || held == nil || held.Owner != owner {
		t.Fatalf("precondition: the sweep's tick did not leave %s held by this node: (%v, %v)", duty, held, err)
	}
	// A HOLD NO PRODUCTION PATH CAN CLAIM, which is the other half of making
	// this observable. What the rule is about is the PREFIX — a `worker:`
	// lease that is not a recorded duty — and any name satisfies that. But a
	// real `setup-provision-<kind>` is the lease held while ANYTHING writes
	// at that integration, a tick of the reconcile loop included
	// ([setupDutyName]), and this engine takes it under the very owner below.
	// Named for a live kind, the hold is not this test's: a reconcile tick
	// landing in the window renews it and releases it when its work ends, so
	// the assertion fails on a second writer rather than on the rule. The
	// suffix is deliberately not an [integration.Kind], and the guard keeps
	// it that way — a kind added later would quietly restore the collision.
	const holdName = "setup-provision-no-such-surface"
	for _, kind := range integration.Kinds {
		if holdName == "setup-provision-"+string(kind) {
			t.Fatalf("%q now names a real integration surface, whose reconcile "+
				"tick takes this lease: pick a suffix that is not a kind", holdName)
		}
	}
	// THE LEASE IS CHECKED, not just the error. A refused claim is the
	// ordinary (nil, nil) here — a peer holding it, or the duty layout gate
	// — so a precondition reading only err calls a refusal success and
	// leaves the assertion below failing for a reason that is not the rule
	// it is about.
	hold := coord.WorkerResource(holdName)
	if got, err := leases.TryAcquire(ctx, hold, coord.AcquireOptions{
		Owner: owner, TTL: 5 * time.Minute, Ungated: true,
	}); err != nil || got == nil {
		t.Fatalf("precondition: take a setup hold: (%v, %v)", got, err)
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
		// NOT turn_completions, and NOT chat_thread_follows: both are
		// retained by a COORDINATION bucket now, whose age no sweep
		// here ticks against. coordtest holds those horizons — the
		// ledger's to the catchup ceiling, the follows' to its own
		// declared constant.
		"scheduled_runs":        maintenance.ScheduledRunRetention,
		"conversation_sessions": maintenance.ConversationRetention,
		"a2a_channels":          maintenance.ChannelRetention,
		"a2a_channels_idle":     maintenance.ChannelIdleTimeout,
		"counterparty_profiles": maintenance.CounterpartyRetention,
		"seat_mailboxes":        maintenance.MailboxRetirementGrace,
		// NEITHER NATIVE BACKEND HAS A ROW-SWEEP ENTRY, and the
		// absence is the point rather than an omission. Both are
		// state-log domains: their records are a log the retention gate
		// trims, and their durable rows are written by an applier — so
		// there is no per-node sweep of ROWS to give a horizon to, and
		// a delete on one node's own authority is what the identity
		// claim forbids.
		//
		// NOR THEIR OPERATION LEDGERS, which were one entry here while
		// every domain took one constant. The horizon is per domain
		// now — a ledger's size is that domain's own commit rate times
		// its retention — and the rule is certified against EVERY
		// registered domain by statelogtest's declaration case, which
		// a fourth domain inherits without being added anywhere. A row
		// per domain here would be a second list, and the list a new
		// domain is not added to is the list that stops covering it.
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
