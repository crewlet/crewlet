package engine_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ENGINE'S SWEEP NAMES EVERY SHORT-HORIZON JOB. Each table ships a Purge
// and an index for it, and with nothing calling them every one grows for the
// life of the deployment; a store gaining a Purge gains an entry here.
func TestTheEngineSweepsEveryShortHorizonTable(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})

	w := e.Maintenance()
	if w == nil {
		t.Fatal("the engine armed no retention sweep")
	}
	// Started, not merely constructed: a worker never run looks, from
	// everywhere else, like one doing its job.
	if !w.Running() {
		t.Fatal("the sweep was built but never started")
	}
	got := w.Jobs()
	slices.Sort(got)
	want := []string{
		// Not webhook_deliveries, rate_limits, turn_completions or
		// config_apply_status: coordination buckets, which the broker
		// ages.
		"a2a_channels",
		"a2a_channels_idle",
		// The access log's operation ledger, swept per node like every
		// domain's.
		"access_ops",
		// The activation's bodies no pointer names: content addressed, a
		// key per document, so each activation leaves one behind.
		"activation_bodies",
		// The advisor log's operation ledger (see pages_ops).
		"advisor_ops",
		// The diary's Expire has no other caller: unswept, expired
		// short-term memories stay rows every recall scans.
		"agent_diary",
		// Notes an upgrade or an access change held back, retired a held
		// note's life later (learning.HeldNoteLife).
		"agent_diary_held",
		"agent_diary_long",
		// A comment record's notices to people, divergent, each node's own,
		// swept at the company's inbox horizon (artifactsdomain.NoticeJobs).
		"artifact_notifications",
		// The datastore log's operation ledger (see pages_ops). Its
		// documents, history and tombstones are Replicated: their
		// retention rides each record.
		"artifactdata_ops",
		// The artifacts log's. Its history and trash are Replicated,
		// retired by a record (the applier's history term, the trash
		// reaper), not a local delete.
		"artifacts_ops",
		// Each human seat's Ask Crewlet instructions, beside its Home board.
		"assistant_instructions",
		// The assistant log's. Its conversations are Replicated (the trash
		// reaper's retention purge), and its anchors go by the trim (see
		// pages_ops).
		"assistant_ops",
		// The atlas log's operation ledger (see pages_ops).
		"atlas_ops",
		// The chat log's operation ledger, and not its anchors (see
		// pages_ops).
		"chat_ops",
		// Each human seat's chat read state, a coordination record with no
		// age: unswept, a removed seat's cursors, mutes and do-not-disturb
		// stay for the life of the deployment.
		"chat_read_state",
		// The code log's notices, a person's and a seat's, each kind its
		// own horizon: per node, because the table is divergent.
		"code_notifications",
		"code_notifications_agents",
		// The code log's operation ledger (see pages_ops).
		"code_ops",
		// The configuration archive: each node keeps a whole-document row
		// per revision it has met, every seat's address among them. Per
		// node, since each holds its own copy (see pages_ops).
		"company_config",
		// The coordination store's removal markers: an unaged bucket keeps
		// every removal's marker, and every listing reads each again from
		// the stream leader, so a listing's cost grows with every removal.
		"coordination_markers",
		// Memsync republishes every row to every peer each cycle, so
		// unswept the table grows with the deployment's age.
		"counterparty_profiles",
		"events",
		// The executor log's operation ledger. Its sessions are Replicated
		// (the trash reaper's retention delete), and its anchors go by the
		// trim (see pages_ops).
		"executor_ops",
		// Each human seat's Home board, in the ageless positions register:
		// unswept, a removed seat's widgets stay for the life of the
		// deployment.
		"home_board",
		// The identity estate's ledger, per data node like every domain's.
		// Not its authentication trail's horizons: those rows are
		// identity-claimed, so a record on the log retires them, never one
		// node's clock (internal/iamdomain's sweep).
		"iam_ops",
		// Its session subjects' ledger: a shorter horizon (op ids nobody
		// re-asks after the request), in a job of its own so its deletions
		// are counted by name.
		"iam_ops_session",
		// The fleet's MCP listings in the positions register, which no clock
		// may age: a listing no reader counts and a schema no listing named
		// for two sweeps. Nothing else removes them (ADR-0074).
		"mcp_listings",
		// Every registered domain's `<domain>_ops` ledger, a row per applied
		// record. Per node, because the rows record what THIS applier
		// wrote: a fleet singleton would tidy one node only.
		//
		// Not a domain's anchors: Replicated rows, so a per-node delete
		// would be a second writer of rows every copy holds alike
		// (ADR-0002); the trim retires them by record (ADR-0080). Nor
		// Gitlet's `code_packs_retired` (the code-retention duty's
		// retire_sweep after gitlet.RetireGrace,
		// TestARetirementIsSweptByARecordTheEngineRuns) or `code_checks`
		// and `code_check_logs` past gitlet.CheckHistory (each ending, and
		// the duty's checks_sweep), for the same reason.
		"pages_ops",
		// No native backend sweeps rows: the knowledge base's prune rides
		// each commit, a create is one transaction, and history is a
		// Replicated table only an applier deletes.
		// The parts a detached run keeps beside its record (a suspended
		// conversation, a bridged tool log) once the record is gone, in an
		// ageless bucket.
		"scheduled_runs",
		// The one job deleting broker state: a removed seat's mailbox,
		// retired after its grace, or the stream keeps its mail for ever.
		"seat_mailboxes",
		// The tracker's own jobs delete nothing (its records are a log the
		// retention gate trims): they finish what a crash left half-done
		// and tell tasks a close unblocked. Unregistered, a job never runs.
		"tracker_abandoned_merges",
		"tracker_abandoned_moves",
		"tracker_duplicate_ranks",
		// The poll of the pull requests tasks wait on, for what no webhook
		// delivered (ADR-0121).
		"tracker_linked_changes",
		// A create a lagging node decided before it heard of a finished
		// move lands in the old project under a root in the new one; only
		// this carries it.
		"tracker_move_stragglers",
		// A person's inbox, the one entry deleting something a person
		// reads, at the company's inbox horizon. Per node, because the
		// table is Divergent; what ages out is a pointer, never the
		// history row behind it.
		"tracker_notifications",
		// The one-sided dependency repair: the mirror of a dependency's
		// two commits is best effort, and this writes the one that did not
		// land, the only wake the blocker's assignee gets.
		"tracker_one_sided",
		"tracker_ops",
		"tracker_respread",
		"tracker_unblocked",
		// Not `vectors_*` or `usage_*`: compacted domains keep no ledger,
		// and a job named for one would report itself working while
		// purging nothing.
	}
	if !slices.Equal(got, want) {
		t.Fatalf("swept tables:\n got %v\nwant %v", got, want)
	}
}

// THE NODE THE ENGINE BUILDS REGISTERS EVERY SEAT'S MAILBOX WITH THE FLEET.
// The sweep retires only a mailbox the registry remembers, fed by the node's
// walk; without it a removed seat's mail is kept for ever, while the node and
// maintenance unit suites each pass against their own fakes.
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
// A duty's lease outlives several ticks (45 minutes for this sweep, three
// hours for the learning passes) and a restarted process cannot re-claim the
// old incarnation's, so a duty kept goes dark for its whole TTL. A setup hold
// under the same prefix belongs to a pass that may still be running, and
// giving it back would let a second writer in mid-pass.
//
// The test owns the backends so their lifetime is its own: on embedded-kv an
// engine-owned Close would make the reads below error rather than observe.
// What makes them observable is the hold's name (see holdName).
func TestAStoppedEngineGivesItsDutiesBackAndKeepsItsHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	boot := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	})
	company := parsedCompany(t, companyDoc)
	backends, err := engine.OpenBackends(t.Context(), boot, company, nil)
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
	// A hold no production path can claim. The rule is about the `worker:`
	// prefix, but a real `setup-provision-<kind>` is taken under this owner
	// by anything writing at that integration ([setupDutyName]), so a
	// reconcile tick would renew and release it mid-test. The guard keeps
	// the suffix off every [integration.Kind].
	const holdName = "setup-provision-no-such-surface"
	for _, kind := range integration.Kinds {
		if holdName == "setup-provision-"+string(kind) {
			t.Fatalf("%q now names a real integration surface, whose reconcile "+
				"tick takes this lease: pick a suffix that is not a kind", holdName)
		}
	}
	// The lease is checked, not just the error: a refused claim is the
	// ordinary (nil, nil).
	hold := coord.WorkerResource(holdName)
	if got, _, err := leases.TryAcquire(ctx, hold, coord.AcquireOptions{
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

// AN IDLE ASK OUTLASTS THE LONGEST SILENCE OF ONE THAT IS STILL OWED.
//
// The idle sweep closes a channel quiet for [maintenance.ChannelIdleTimeout]
// unless its ask waits where the engine can see it. An ask in flight it cannot
// see: a dead node's unacknowledged delivery (the broker's ack wait),
// redelivered once the seat's next holder takes its lease, then up to two
// dispatch segments, each bounded by an onboarding pass, before a run's record
// is written. A shorter timeout closes the channel of an ask about to be
// answered, and the answer is refused.
//
// Four constants in four packages, nothing else connecting them; only the
// order is asserted, for TestARevisionOutlivesTheAuditRowThatNamesIt's reason.
func TestAnIdleAskOutlastsTheSilenceOfOneStillOwed(t *testing.T) {
	t.Parallel()
	silence := jetstream.DefaultAckWait + seat.SeatLeaseTTL + 2*runner.ClaimTTL
	if maintenance.ChannelIdleTimeout <= silence {
		t.Errorf("an idle A2A channel is closed after %v, and an ask still owed its answer can "+
			"be silent for %v (the broker's ack wait %v, a seat lease %v, two onboarding claims "+
			"of %v).\nThe sweep would close the channel of an ask about to be answered, and the "+
			"answer would be refused. Raise ChannelIdleTimeout, or shorten what it outlasts.",
			maintenance.ChannelIdleTimeout, silence, jetstream.DefaultAckWait, seat.SeatLeaseTTL,
			runner.ClaimTTL)
	}
}

// The tick must stay shorter than every horizon, or a table sits past its
// horizon for the difference. [maintenance.New] raises a short horizon at
// runtime; this asserts the shipped ones never need raising.
func TestEveryRetentionOutlastsTheSweepInterval(t *testing.T) {
	t.Parallel()
	for table, horizon := range map[string]time.Duration{
		// Not turn_completions or chat_thread_follows: coordination
		// buckets age them, and coordtest holds those horizons.
		"scheduled_runs":         maintenance.ScheduledRunRetention,
		"a2a_channels":           maintenance.ChannelRetention,
		"a2a_channels_idle":      maintenance.ChannelIdleTimeout,
		"activation_bodies":      coord.BodyRetention,
		"counterparty_profiles":  maintenance.CounterpartyRetention,
		"seat_mailboxes":         maintenance.MailboxRetirementGrace,
		"chat_read_state":        maintenance.ChatReadForgetGrace,
		"home_board":             maintenance.HomeBoardForgetGrace,
		"assistant_instructions": maintenance.HomeBoardForgetGrace,
		// No native backend has a row sweep: an applier writes its rows,
		// and chat's message horizon is a prune record a fleet duty
		// publishes. Their operation ledgers are the exception, one entry
		// since every domain takes the same horizon.
		"<domain>_ops": statelog.OpsRetention,
		// The identity estate's sessions go in an hour, the shortest
		// horizon the sweep keeps.
		"iam_ops_session": iamdomain.SessionOpsRetention,
	} {
		if horizon <= maintenance.Interval {
			t.Errorf("%s retention (%v) is not longer than the %v tick",
				table, horizon, maintenance.Interval)
		}
	}
}

// A scheduler claim answers "did this fire already run?", so deleting a row a
// tick could still evaluate lets that fire run twice: its floor is the catchup
// ceiling. coordtest holds the completion ledger to coord.LedgerRetention.
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

// The delivery horizon and the rate window are coordination buckets the broker
// ages; coordtest's TestTheRetentionsOutlastWhatTheyCover is their guard.
