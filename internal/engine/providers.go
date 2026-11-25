// Package engine assembles one process's participation in a company: the
// config, the org, the models, the tools, and the turn machinery between them.
// It is the WIRING, which concrete thing satisfies which seam; the hard
// passages are packages of their own (internal/agent/inbox, the config plane).
//
// Decided here because only this package holds their parts together: a turn's
// charge to its item (worksubject.go, turnspend.go; ADR-0022); a person's note
// to a running turn (steer.go, runsteer.go; ADR-0067); parking a seat whose
// models are all at their limit (providerpark.go; ADR-0064); pausing a seat on
// a failure no retry cures, twice in a row, and holding a conversation that
// does not fit (failurehold.go, conversationhold.go; ADR-0117); absorbing a
// wake a completed turn read past (seenthrough.go; ADR-0120); settling a task
// on its pull request with no turn (changesettle.go; ADR-0121); and a remote
// MCP server's or an integration's OAuth grant, its refresh token sealed,
// its access token in memory (oauthgrant.go; ADR-0075, ADR-0178).
//
// # The core runtime
//
// core.go splits what every data node runs from boot (the state log for every
// domain, the node gate, the identity estate, the party registry) from the
// native half, which waits for the first company, by what depends on a
// company, never by domain: a join replaces the whole replicated file before
// any applier, a snapshot names every domain, and the trim counts every node
// per log. A node with no company still signs in the session that creates it
// (ADR-0032). A node without `data` (ADR-0025) asks a data node through the
// estate's router ([routedIdentity], [Engine.MachineToken], [Engine.BoundSeat])
// and applies chart and tokens on its own side, so no secret crosses. Sign-in
// is served only where `ingress` is: 503 when behind, never 401, no cache.
//
// # Maintenance mode
//
// A stream's byte ceiling changes safely only while nothing publishes, and only
// a broker restart retires a queued request, so a capacity change restarts the
// fleet into a mode that starts brokers and no publisher. A normal node joins
// by handshake, never an absence check: it writes its admission, re-reads the
// operation and withdraws if one is there. A maintenance node acknowledges the
// current attempt, retires a dead predecessor's admission, keeps its presence
// lease (membership) and starts no writer, duty or barrier
// (statelog.RefuseMaintenance).
//
// # Broker membership
//
// fleetbroker.go: a member gone for good still votes until removed, while its
// presence lapses. Non-members ask a member over
// `crewlet.fleet.broker.<member>`, signed both ways (internal/signedask), since
// the broker vouches for connections, never messages. Voters are matched by
// raft peer id, never name; an external cluster's membership is its operator's.
//
// # The live relay
//
// A node whose copy of a domain is out of service routes reads to a sound peer
// ([localEstate.For]) and its sockets hear that peer's applier, its own
// announcements withheld (liverelay.go). Per domain, since a fault is one
// log's (ADR-0079), and a switch re-reads that domain's screens, since nothing
// between the two sources is replayed. A follower reads one server by its sequence
// under an epoch; anything else is a re-read. Signed, unlike read-only
// scatters (ADR-0031), because what it takes reaches sockets as a colleague's
// words; answers pass the per-socket filters ([chatlet.Visible]).
//
// # Dead letters
//
// deadLetterLedger drops and deadLetterWatch reads feed and inbound dead
// letters. Each beat reads the marks (counts, last sequence) and relists only
// what moved, recording them agreed only when a listing agrees (a replica
// trails); a failed read keeps the last listing. An entry gone at its expiry is
// a lost wake, counted once fleet-wide by whoever takes its claim (goneKey).
//
// # Budget holds on the pinned clock
//
// A meter holds a window it saw full until it turns over on the turn's pinned
// clock ([meter.Refused]), though a peer may have rolled the shared slot. The
// turn counts on its own calendar, and a false hold fails closed: it costs
// work done, never a call billed and refused.
//
// # Executor launch floor
//
// A launch needs the room its first request does (launchfloor.go, ADR-0066):
// the static floor (turn_engine.executor_min_budget_tokens) or the largest
// brief's, at a token per four bytes. A refusal carries that floor to its
// wait, or it retries into the same refusal. A session past the window's room
// (windowRoom) is not continued, and a fresh brief past it is held at the
// static floor or the room, since an empty window may never come. A floor
// within the room waits one turnover, the window kept for the work, never the
// seat. A session's context is its harness's next-request estimate, else its
// last input, never the larger; which one continues is asked of every chain
// member, since the launch picks late (agentLauncher.member).
//
// # Seat credentials issued at use
//
// Some integrations issue an agent's hour-long credential when a seat's value
// names it, renewed in tool servers and token files (issued.go; ADR-0058),
// through the [Engine.seatIssuers] registry, never a kind. Only the seat's own
// resolution answers it ([seatToken]), so an identity stays with its agent by
// construction. Unissuable it is waiting, a cached refusal, or absent; an
// executor run is never refused for it (`sandbox_env_unresolved`).
//
// # Token files in a running box
//
// A run outlives its token, so a setup file wholly `${VAR}` is REPLACED whole
// on renewal ([sandbox.Sandbox.WriteFile]), by the waiter that polls the box
// (ADR-0065) and before a kept box's job starts. It is read back, not
// remembered. A token is RECORDED before it is written
// ([sandbox.Coordinator.RecordHanded]) so captures are scrubbed of it.
//
// # A running executor's session, saved as it works
//
// The waiter offers internal/sandbox's saves, charged to the seat's ledger at
// this node's interval; a drain saves its Direct jobs ([Engine.Drain]).
//
// # MCP servers
//
// A SHARED server is one child per company, reconciled each apply. A PER-ROLE
// template's child carries one seat's credentials and lives with that seat's
// LEASE here, or every node runs the company and a child outlives its seat.
// Each seat gets its own bridge and registry ([Engine.ToolsFor]), since a
// template's children share tool names. A child restarts only when its
// resolved spec moves; one that will not start costs only its tools.
//
// # Tool server credentials
//
// A server starts only once every `${VAR}` it references resolves, or it acts
// as nobody. [credentialGap] says: a pass will mint it (waiting); a person
// must act ([mintStopped]); or nothing will. None retries on a timer, since
// only a secret snapshot change can help, and each triggers a pass
// ([refreshingSink.Flush]). [config.MCPServer.OptionalKeys] leaves an
// unresolved key out whole, never the url. [resolveServer] is the one
// resolution: internal/mcp sees only strings. A mailbox never waits for it.
//
// # Testing an MCP server
//
// "Test now" runs where the server runs (mcpcheck.go): a shared one on every
// node, a per-agent one on the seat's holder, over a signed channel
// (mcpCheckChannel) with values sealed to the request. A draft is tested as a
// write would leave it; a saved server's latest is kept (coord.MCPCheck).
//
// # Memory sync cycles
//
// A cycle ([memorySyncInterval]) laps each held seat's pass, then a backlogs'
// turn of their own, never the cycle's leftovers, which a burst leaves none of.
// The next starts at the cut turn, or after it if that turn
// had the whole cycle ([nextStart]): always the cut lets one turn keep every
// cycle, always the next starves. No seat waits more than 2k + 2 cycles.
//
// # Dispatch's order
//
// internal/agent/inbox owns the guards. [Dispatcher.Dispatch] reads completion
// after every park and before coalescing, offers a parked run's answer
// ([Dispatcher.answered]) before a turn eats it, then reads the conversation,
// and writes completion after the turn, failed or not, so a bad trigger never
// loops; a broken phase only when [turn.Abandon] gives a reason.
//
// # A failure of this node's is never the mail's own
//
// [nakFailure] NAKs as the node's ([queue.NakNode]) where [nodesFailure] says,
// else as the mail's: same backoff and budget, but a spent node's failure is
// never poison. [nodesFailure] is ONE answer for the mailbox, [controlOutcome]
// and the give-up: [queue.NodeFault], an infrastructure condition
// ([builtin.InfrastructureKind]), [sandbox.ErrStoreSilent],
// [sandbox.ErrUnannounced].
//
// # A refused completion is handed back by the condition it names
//
// A completion [Engine.admitResume] refused leaves its run running, and the
// waiter re-announces it at most twenty polls apart, so [controlOutcome] keys
// on the condition ([sandbox.TurnRefused]), never the error, and a new
// condition needs no edit here. One that clears by waiting is ACKED: a NAK or
// deferral would spend deliveries toward the dead letter while nothing is
// wrong; the coordinator records it owed on the run's row and re-fires it
// ([Engine.readmitResumes]). A draining node ([sandbox.Draining]) or a seat let
// go ([sandbox.SeatLost]) DEFERS, handing it to the seat's next holder.
//
// # While no waiter can announce a run
//
// While the store of the box's host is silent to it, that waiter announces
// nothing and a kept completion lives only in this node's memory. So every
// hand-back is marked [sandbox.ErrUnannounced] and NAK'd as the node's, never
// acked; near its last delivery the coordinator announces it afresh and
// answers nil, since the broker's budget is shorter than the host grace
// (internal/sandbox's renew.go). That waiter's own announcement, from a read
// asked after the silence began, ends it.
//
// # Everything else on the control topic NAKs
//
// A completion not settled here comes back on the broker's backoff rather than
// waiting five minutes for the waiter, as the node's ([queue.NakNode]) for a
// row a newer lease owns while this node's still reads held (routing), a
// store's silence, a [queue.NodeFault], an infrastructure condition, a resume
// elsewhere, and a claim the recovery gate refused ([sandbox.ErrSeatRecovering];
// the recovery pass attaches the topic, [sandbox.Coordinator.RecoverSeat]). A
// run that moved on arrives nil and is acked. A drain's cut decides nothing:
// it fires after the broker connection closes. A failure the completion caused
// stays the message's. A dead letter loses nothing: the waiter re-announces
// within 300 s, inside the broker's minimum ~481 s delivery budget.
//
// # A resume another node or build can make
//
// A parked run's reply kind comes off the row ([resumeReply]); an unknown one
// is refused, never guessed, as a routing failure ([sandbox.ErrResumeElsewhere]),
// and an absent one as [sandbox.ErrResumeUnavailable]. The give-up counts it
// only once no other live node reads its state's version
// ([Engine.fleetReadsParkedState], execstate.VersionOf only), or, for a reply
// kind (no part of the version), once every live node runs this build
// ([Engine.anotherBuildLive]); judged by version, two rolled-back nodes would
// wait on each other for ever.
//
// # Every hold is taken for one attachment of the inbox
//
// A hold is keyed on the seat's id and TAKEN FOR this node's attachment
// ([queue.Attachment]), recorded in [seatInboxes] as the seat is taken
// ([Engine.prepareSeat]) and dropped on release ([Engine.releaseSeat]). One
// asked after the detach is refused ([queue.ErrAttachmentGone]); keyed on the
// subscription it would carry onto a later attachment with nothing to lift it.
// A release drops pause-hold records ([Engine.forgetPauseHold],
// [Engine.forgetModelHold]). This node alone agrees on them.
//
// # A lift takes back a hold taken in its window
//
// A lift drops its record under its lock and calls the queue outside it, since
// the in-memory twin's resume drains synchronously into handlers reading that
// state. A hold recorded between the two would be lifted by the late call, so
// each lift reads again after its call and re-holds what was recorded meanwhile
// ([Engine.liftPauseHold], [seatParks.reholdParked], [Engine.reholdModelHold]),
// logging `held_again`.
//
// # A seat parked until an instant
//
// The budget and provider parks share seatpark.go: a seat that cannot run until
// a known instant holds its inbox under the park's reason and DEFERS the
// delivery, rather than NAK it into the same refusal until it dead-letters. A
// budget park arms its own alarm; a provider park is released by one probe per
// limited chain. A PARK OUTLIVES NO SEAT: a release detaches, forgets parks and
// disarms alarms, and a park taken around it undoes itself
// ([Engine.unparkReleased]), or its probe would act as a seat a peer runs.
// Holds are keyed by reason, so two parks never lift each other's. Only this
// node agrees on a park; a restart decides it again.
//
// # A provider park's release and its wait
//
// A release ([Engine.releaseProviderParking]) leaves the probe and notes its
// notice as the record drops, under the parks' lock, so a re-park in its window
// comes after it; the probes' and notices' locks are taken only under the
// parks'. A seat's wait ([limitProbes.waits]) is the SEAT'S, not a probe's, or
// every apply's re-park would restart `llm_limit_waiting` ([parkCarryWindow]);
// a release's late settle leaves a standing park's wait ([Engine.unlessParked]).
//
// # A provider park's notices
//
// [parkNotices] keeps the newest notice per seat: repeats dropped, newer ones
// replacing waiting ones, published in decision order ([parkNotices.note]),
// and held and retried on a backoff while the broker refuses, logged once per
// outage, since a hot decision path once flooded both broker and log.
//
// # A person's pause, carried out by whichever node holds the seat
//
// seatpause.go caches coord.SeatPause, keyed on the seat's id (ADR-0013), never
// written back. It HOLDS THE INBOX ([inbox.HoldSeatPaused]) for a held seat,
// hold and record one step under the pauses' lock; answers screening
// ([inbox.Conditions.Paused], unknown until the watch first answers, kept when
// it ends); skips the schedule; and, with StopRunning, ends the turn at its
// next round and a launched executor at once ([Engine.stopSeatRuns]); a run
// parked on a person's question stays parked behind the pause. A paused
// seat is ATTACHED ALREADY HELD ([Engine.acquireInbox]), or its held mail would
// run first; a hold that cannot be taken refuses the seat.
//
// # The model alarms' inputs
//
// Judged from the live nodes' status records, so every node answers alike; a
// token's expiry from its snapshot ([Engine.storedAt]). A failed read keeps
// the last good one, and a live node its last record at its presence epoch
// ([coord.LiveNodeStatusesKept]): a blip neither clears nor raises. This node
// reads its own memory; the listing feeds [mcpConditions].
//
// # Whose alarm a status record carries
//
// A seat's node counts blocked resumes ([sandbox.Coordinator.ResumesBlocked])
// into its record; every data node raises executor_resume_blocked from the
// live records, kept as above: the alarm is the run's. What a node measures
// of itself ([statelog.LocalKinds]) it judges on its own table
// ([Engine.startLocalAlarms]), a node without `data` included.
//
// # A note to a turn whose executor is out
//
// From launch to the turn's end a note waits on the run's record
// ([sandbox.RunSteer], ADR-0067): the nodes that receive, bridge, resume and
// end can all differ and each reads the record, so any files with one
// compare-and-swap and node memory is only derived.
//
// # A note is sealed, because it is an instruction
//
// The store authenticates no writer (ADR-0031), so each note is sealed under
// the fleet keyring ([SteerNoteDomain]) beside a sealed seat anchor
// ([SteerSeatDomain]) that authority is decided on. A seal that fails is never
// used; one under a key this node lacks waits, said once
// ([runSteers.waitsForItsKey]). The unsealed `read` marker is ADR-0067's
// residual.
//
// # Every read is on the record, so the record is the carry
//
// A note reaches a model only by a write that records it (a launch, a take,
// or a hook's serve, read once its session shows it, [runSteers.confirmed]), so
// a resuming segment carries exactly what the turn read ([runSteers.carried]).
// Order is the record's ([sandbox.NoteRead.Order]), never nodes' clocks;
// waiting notes are set aside before the sort, never compared equal. A
// relaunch's brief ([rowSource.Brief]) places notes where the record does.
//
// # A take whose answer was lost is settled, never left saying it was read
//
// A write can land with its answer lost, so every take names itself and one
// that errored is SETTLED by that name: a hook's off its path
// ([runSteers.settleHook]) until the store answers, never on a clock, with a
// last try at teardown ([steerSettleStopBudget]) off the stop's allowance.
//
// # An OpenCode confirmation is recorded once, and reported
//
// A poll answers a note only once it is recorded served, so a continued session
// is told what it may already show ([servedNotes]). A confirmation's read is
// true even if its answer is lost, so it is settled and reported on its path.
// One the inbox cannot take (no job served, no read taken, or no key here) is
// TURNED AWAY to ask again ([mcpbridge.ErrNoteUntaken]), never answered done.
// The inbox is bound before the bridge files the opening ([Engine.openBridge]).
//
// # Who records what became of a note
//
// The node whose step read it, and for every note no step reported, the node
// ending the run ([runSteers.ended]), under ids derived from the ending; never
// at a park. A read marked unanswered is retold by every later brief.
//
// # An item for one job is that job's alone
//
// Only the store files one ([sandbox.RunNote.For]); every reader names its job
// ([runSteers.waiting]), and every step reading people's notes passes it over.
//
// # A late call is handed to the job working as it lands
//
// The store's LATE-CALL ITEM ([sandbox.RunNote.LateCall]) is unsealed, routed
// by kind before any seal opens, and rendered in the engine's words
// ([lateItemMessage]) under the session's fence token. Claude Code takes it at
// a tool batch only, since at the finish it would replace the report; OpenCode
// at its next step. [maxLateItemUnits] fits it in one hook answer.
//
// # A continued session's floor
//
// That session's own context with its brief: the kept capture's measure
// (SessionBasis.ContextTokens) or the box copy's (SessionBasis.CopyContext),
// never a newer capture's.
//
// # A busy seat's wake router
//
// While a seat's executor run holds it, the seat's node reads the held inbox
// with the queue's backlog read, which spends nothing, and files to the run as
// a wake item each wake from its requester in its conversation, about its work
// item or about a record its calls wrote, by exact identity ([routeOf],
// [sandbox.BridgeLog.Wrote]); another writer's never on that rule (ADR-0109).
// One router per busy seat ([Engine.seatRunsMoved]), nudged by the inbox's and
// lane's streams, an idle dispatch and a backstop ([routeBackstop]); each wake
// decided once per job. A claimed wake's delivery is skipped `joined`, naming
// its turn, its seen-through mark moved at the claim and its owed answer the
// resumed turn's ([runSteers.turnReply]); an unread one runs on its own
// (ADR-0167).
//
// A wake that rule leaves about the run's work, or a lead's ([candidate]), is
// described once per job by one call on the role's `triage` chain
// ([runTriage]) and the engine gates the description ([gateOf]): input joins
// as information, a lead's steer as their note ([Engine.fileSteer]), noise
// owing nothing is absorbed, the rest waits (ADR-0168). Another writer's
// request is put to the run's requester and the seat's leads as a fold-in on
// the run's row ([Engine.askFoldIn]); approved, it joins as the approver's
// note ([Engine.fileFoldIn]), else it waits (ADR-0109). A waiting person is
// told why, once ([Engine.tellWaiting]). Runs see each other (ADR-0176).
package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// buildProviders turns providers.llm into the ordered registry the per-phase
// resolver reads.
//
// Order comes from the document (config.Company.ProviderOrder): "the first
// provider configured" over a Go map would differ between seats and restarts.
// No providers is a nil registry, not an error: the dispatcher holds work until
// one arrives ([Company.Models]).
//
// Every entry shares the node's one [harness.Plans], which outlives the
// revision, so a rebuilt entry finds the windows its predecessor sat out; nil
// (validation only) gives each its own. The LLM fleets balance on ledger, the
// node's memory, likewise; nil gives the epoch its own.
func buildProviders(c *config.Company, r *config.Resolver, plans *harness.Plans,
	ledger *fleet.Ledger) (*phase.Registry, error) {
	order := c.Providers.ProviderOrder()
	if len(order) == 0 {
		return nil, nil
	}
	entries := make([]phase.Entry, 0, len(order))
	members := make(map[string]fleetMember, len(order))
	for _, key := range order {
		spec, ok := c.Providers.LLM[key]
		if !ok {
			// Cannot happen (ProviderOrder comes from the map), but a silent
			// skip would move a seat onto another model with nothing said.
			return nil, fmt.Errorf("engine: provider %q is in the order but not in the map", key)
		}
		p, err := BuildHarness(key, spec, r, plans)
		if err != nil {
			return nil, err
		}
		entries = append(entries, phase.Entry{Key: key, Provider: p})
		members[key] = fleetMember{provider: p, kind: spec.FleetKind()}
	}
	if ledger == nil {
		ledger = fleet.NewLedger(nil)
	}
	return phase.NewFleetRegistry(entries, c.Providers.PhaseModels().Fleets, phase.Balance{
		Ledger:  ledger,
		Signals: func(key string) fleet.Signals { return members[key].signals() },
	})
}

// fleetMember is one entry as an LLM fleet's balancer reads it.
type fleetMember struct {
	provider *harness.Provider
	kind     fleet.Kind
}

// signals is what this node knows of the member's sign-in now: its windows,
// its sit-out, its last refusal and answer, and whether its slots are taken.
func (m fleetMember) signals() fleet.Signals {
	out := fleet.Signals{Kind: m.kind}
	if m.provider == nil {
		return out
	}
	state := m.provider.Plan().State(time.Now())
	out.Answered, out.Full = state.LastAnswer, m.provider.SlotsTaken()
	for _, l := range state.Limits {
		out.Readings = append(out.Readings, fleet.Reading{LimitReading: l.LimitReading, Seen: l.Seen})
	}
	if held, ok := m.provider.SitOut(); ok {
		out.SitOut = &fleet.SitOut{Until: held.Until, Window: held.Window}
	}
	if f := state.LastFailure; f != nil && f.Kind == llm.KindAuth {
		out.Refused = f.At
	}
	// A SIGN-IN KNOWN BAD BEFORE A CALL (no login, an expired one): the verdict
	// the node holds, never a sign-in resolved here, as a pick waits on nothing.
	out.NotSignedIn = llm.KindOf(m.provider.KnownUnready()) == llm.KindAuth
	return out
}

// buildHelpers builds a helper's run of every entry in
// [config.Company.HelperKeys]: the entry again, web tools forced off
// ([config.LLMProvider.HelperOverrides]), on its own state directory and
// sign-in, with slots of its own so seats never queue a person's question. An
// entry no helper can run on is skipped; its helper answers why
// ([config.Company.HelperModelProblem]).
func buildHelpers(c *config.Company, r *config.Resolver,
	plans *harness.Plans) (map[string]llm.Provider, error) {

	var out map[string]llm.Provider
	for _, key := range c.HelperKeys() {
		if _, done := out[key]; done || c.HelperModelProblem(key) != nil {
			continue
		}
		p, err := BuildHelper(c, key, r, plans)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]llm.Provider{}
		}
		out[key] = p
	}
	return out, nil
}

// BuildHelper constructs a helper's run of the providers.llm entry key, or
// why no helper runs on it ([config.Company.HelperModelProblem]). Exported for
// `crewlet assistant eval`, which must measure the run Ask Crewlet makes.
func BuildHelper(c *config.Company, key string, r *config.Resolver,
	plans *harness.Plans) (*harness.Provider, error) {

	if err := c.HelperModelProblem(key); err != nil {
		return nil, err
	}
	spec := c.Providers.LLM[key]
	spec.Overrides = spec.HelperOverrides()
	p, err := BuildHarness(key, spec, r, plans)
	if err != nil {
		return nil, fmt.Errorf("engine: a helper's run of providers.llm.%s: %w", key, err)
	}
	return p, nil
}

// buildSearchers builds Ask Crewlet's search run of each entry `assistant.model`
// runs on (the entry, or each member of its LLM fleet): the entry again with
// its harness's web search and no other tool ([config.LLMProvider.SearchOverrides]),
// on the entry's own state directory and sign-in, by key. An entry no helper
// can run on, or whose harness has no measured search-only run, has none: Ask
// Crewlet then offers no search while it runs there.
func buildSearchers(c *config.Company, r *config.Resolver,
	plans *harness.Plans) (map[string]*harness.Provider, error) {

	if c.Assistant.Model == "" {
		return nil, nil
	}
	var out map[string]*harness.Provider
	for _, key := range c.FleetMembers(c.Assistant.Model) {
		if c.HelperModelProblem(key) != nil {
			// A MODEL NO HELPER RUNS ON has no search run either; its helper says why.
			continue
		}
		spec := c.Providers.LLM[key]
		spec.Overrides = spec.SearchOverrides()
		if profile, err := harnessprofile.Load(spec.HarnessName(), spec.Overrides); err != nil ||
			!profile.CanSearchWeb() {
			continue
		}
		p, err := BuildHarness(key, spec, r, plans)
		if err != nil {
			return nil, fmt.Errorf("engine: Ask Crewlet's search run of providers.llm.%s: %w", key, err)
		}
		if out == nil {
			out = map[string]*harness.Provider{}
		}
		out[key] = p
	}
	return out, nil
}

// log is the package logger.
var log = logging.Get("engine")

// restoreBundles restores every entry's configured credential bundle where its
// state directory holds no login yet ([harness.Provider.RestoreConfiguredBundle]).
//
// Only boot and apply call it, never [NewCompany]: `crewlet validate` must
// neither write a login nor wait on its lock. It runs before anything calls the
// epoch's models ([Engine.prepareReflection] on an apply) and, on an apply, after
// every stage that can refuse the revision, so a refused one leaves no login
// behind. A failure only warns (a node can be logged in by hand); an entry
// `crewlet llm logout` emptied ([harness.ErrLoggedOut]) warns as
// `harness_bundle_withheld`.
func (c *Company) restoreBundles(ctx context.Context) {
	if c == nil || c.Models == nil {
		return
	}
	for key, provider := range c.Models.All() {
		cli, ok := provider.(*harness.Provider)
		if !ok {
			continue
		}
		err := cli.RestoreConfiguredBundle(ctx)
		switch {
		case err == nil:
		case errors.Is(err, harness.ErrLoggedOut):
			log.WarnContext(ctx, "harness_bundle_withheld", "provider", key, "detail", err.Error())
		default:
			log.WarnContext(ctx, "harness_bundle_restore_failed", "provider", key, "error", err)
		}
	}
}

// warnAncestorFiles warns, per entry, of a file its harness would read into
// every call from above the entry's state directory
// ([harness.Provider.AncestorRefusal]), in the words each call's refusal uses:
// `harness_ancestor_files`. Warned, not refused, on [BuildHarness]'s rule for
// a missing credential: one entry's layout must not take the company down at
// boot, its calls are refused anyway, and `crewlet llm doctor` reports it.
func (c *Company) warnAncestorFiles(ctx context.Context) {
	if c == nil || c.Models == nil {
		return
	}
	for key, provider := range c.Models.All() {
		cli, ok := provider.(*harness.Provider)
		if !ok {
			continue
		}
		if err := cli.AncestorRefusal(); err != nil {
			log.WarnContext(ctx, "harness_ancestor_files", "provider", key, "detail", err.Error())
		}
	}
}

// BuildHarness constructs one providers.llm entry's provider. Exported for
// `crewlet llm`, which must report on the same provider the engine builds.
//
// Missing credentials still build: every call then fails with the harness's own
// "not authenticated" (`crewlet llm doctor` explains), rather than one login
// taking the company down at boot. An entry holds one login and at most one
// token or API key: a second key is a second fallback entry.
//
// plans is the node's sign-in registry ([harness.Config.Plans]); nil gives the
// entry its own.
func BuildHarness(key string, spec config.LLMProvider, r *config.Resolver,
	plans *harness.Plans) (*harness.Provider, error) {
	stateDir, err := spec.ResolvedStateDir(key)
	if err != nil {
		return nil, fmt.Errorf("engine: provider %q: %w", key, err)
	}
	// Resolved here: Tier B stores "${VAR}" verbatim, which the harness would
	// get literally on --model.
	//
	// A reference that resolved to nothing is refused, naming the variable:
	// unlike a missing credential, a missing model silently runs the harness's
	// default, a model nobody chose. [config.Resolver.Missing] counts an empty
	// answer as absent and tests every reference (`${PROVIDER}/sonnet` with
	// PROVIDER unset is refused); the blank check catches all white space.
	model := strings.TrimSpace(r.Value(spec.Model))
	if missing := r.Missing(spec.Model); len(missing) > 0 || model == "" {
		if len(missing) == 0 {
			// Config refuses a blank written field, so the blank came from
			// the references.
			missing = envref.Names(spec.Model)
		}
		return nil, fmt.Errorf(
			"engine: provider %q: model is %q and %s resolved to nothing — unset, "+
				"or set to an empty value. Set it in the environment, or with "+
				"`crewlet secrets set`",
			key, spec.Model, strings.Join(missing, ", "))
	}
	env, missing := r.Map(fmt.Sprintf("providers.llm.%s.env", key), spec.Env)
	for _, ref := range missing {
		// Warned, not refused: an unset optional tuning variable must not
		// take the company down.
		log.Warn("harness_env_unresolved", "provider", key,
			"path", ref.Path, "variables", strings.Join(ref.Names, ","))
	}

	// Validation loads this profile too, so a failure means an unvalidated
	// document: refused.
	if _, err := harnessprofile.Load(spec.HarnessName(), spec.Overrides); err != nil {
		return nil, fmt.Errorf("engine: provider %q: %w", key, err)
	}

	// Every credential falls back to a conventional name
	// ([config.LLMProvider.CredentialSources]), and the resolver reads the
	// secret store before the environment, so a token stored by `crewlet llm
	// login <key> -capture-token` needs no ${VAR}. The API key's convention is
	// the harness's own variable (ANTHROPIC_API_KEY), reaching the child only
	// under auth.mode api-key ([harness.Auth]).
	sources := spec.CredentialSources(key)
	auth := harness.Auth{
		Mode:            harness.AuthMode(spec.ResolvedAuthMode()),
		Token:           resolveOrConvention(r, sources.Token.Value, sources.Token.Fallback),
		TokenConfigured: spec.Auth.Token != "",
		APIKey:          strings.TrimSpace(resolveOrConvention(r, sources.APIKey.Value, sources.APIKey.Fallback)),
	}
	bundle := resolveOrConvention(r, sources.Bundle.Value, sources.Bundle.Fallback)
	bundleConfigured := spec.Auth.CredentialBundle != ""

	return harness.New(harness.Config{
		Key:                        key,
		Model:                      model,
		Effort:                     spec.Effort,
		Harness:                    spec.HarnessName(),
		StateDir:                   stateDir,
		Overrides:                  spec.Overrides,
		Timeout:                    time.Duration(spec.Timeout() * float64(time.Second)),
		MaxConcurrent:              spec.Concurrency(),
		Env:                        env,
		Auth:                       auth,
		CredentialBundle:           bundle,
		CredentialBundleConfigured: bundleConfigured,
		Plans:                      plans,
	})
}

// resolveOrConvention resolves a configured ${VAR} reference, or looks up a
// conventional name. The fallback is a lookup, not an expansion: Value would
// return the bare name as if it were the credential.
func resolveOrConvention(r *config.Resolver, configured, conventional string) string {
	if configured != "" {
		return r.Value(configured)
	}
	if conventional == "" {
		return ""
	}
	return r.Lookup(conventional)
}
