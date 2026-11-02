package sandbox

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// controlSubject is a seat's sandbox-control subject from the id a run's row
// carries, or empty when the row names no routable seat.
//
// ONE PARSE, in one place. The id rides these rows as TEXT — the record is a
// JSON blob on the node estate and the framework that carries it never
// decodes it — so the three publishers that need this subject would otherwise
// each have their own idea of what an unparseable value means. Here it means
// exactly what a nil id means: not routable, say so, and do not publish to a
// subject named after nothing.
func controlSubject(agentID string) string {
	return topics.AgentControl(seatOf(agentID))
}

// inboxSubject is a seat's inbox from the id a run's row carries, or empty when
// the row names no routable seat: where a chat reply the run did not take is
// handed back ([Coordinator.deliverHandBack]). The same one parse as
// [controlSubject], so the two subjects a run is reached on can never disagree
// about whether its row names a seat.
func inboxSubject(agentID string) string {
	return topics.AgentInbox(seatOf(agentID))
}

// seatOf is the one parse both subjects take: a row's text id as a seat id,
// with an unparseable value read as [uuid.Nil], which every topics builder
// answers with an empty subject.
func seatOf(agentID string) uuid.UUID {
	id, err := uuid.Parse(agentID)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// TurnRef identifies the turn a detached run belongs to.
//
// Carried on the row rather than looked up later, because the resume happens
// in a different process from the launch as a matter of routine: a restart, a
// seat handoff, or simply days passing while a person answers a question.
type TurnRef struct {
	// TurnID is the RUN this job belongs to — one execution of a turn, and
	// the key the whole pending-run record is stored under. It has to be
	// unique per execution: a redelivered trigger re-runs, and two runs
	// sharing this value would share one row and one box. See ADR-0017.
	TurnID string

	// WorkKey is the unit of work behind that run, carried so the row can
	// say which trigger it came from after the run outlives its process.
	WorkKey string

	// WorkSince is when that unit of work began, carried beside the key
	// for the same reason — see [PendingRun.WorkSince].
	WorkSince time.Time

	AgentID     string
	AgentHandle string
	Role        string

	// PartitionKey is the inbox batch the kick-off trigger arrived in and
	// ConversationKey the durable conversation it belongs to. BOTH travel,
	// and both are written onto the row: the conversation is what matches a
	// person's answer back to this run and where the resumed turn reports,
	// the partition tells two runs parked on one direct message apart
	// ([ConversationRef.Best]). See [PendingRun].
	//
	// NAMED AS [turnctx.Turn] NAMES THEM, field for field, because that is
	// where both values come from and the launch copies them across one
	// after the other. They were ConversationKey and
	// ConversationIdentity here, holding the partition and the conversation
	// respectively — so each assignment read as its own opposite and a
	// swapped pair looked exactly like a correct one. [PendingRun] spells
	// them the same way, on the row and on the wire.
	//
	// Two fields where there was one, and the miss this guards against is
	// now available twice: this struct's partition was empty at its only
	// construction site for the whole life of the feature, so every resumed
	// turn recorded nothing at all.
	PartitionKey    string
	ConversationKey string

	TraceID string
	SpanID  string

	// Reply is who is waiting for this turn, persisted so the resumed turn
	// inherits the same delivery obligation. See [PendingRun.Reply].
	Reply string

	// Depth and Chain are the delegation state a resumed turn inherits.
	Depth int
	Chain []string

	// WorkItem is the item the launching turn is charged to, nil when it is
	// on nothing. Written onto the row, because the resumed turn has no
	// trigger to resolve it from — see [PendingRun.WorkItem].
	WorkItem *types.WorkItem

	// Requester is the seat whose wake started the turn, by its handle,
	// "" for none — what a question addressed to
	// "requester" is put to. See [PendingRun.Requester].
	Requester string

	// ColleagueAsk is the colleague's A2A question the turn answers, nil
	// for a turn no ask woke — written onto the row, because the segment
	// that ends a parked turn has no trigger to read it from. See
	// [PendingRun.ColleagueAsk].
	ColleagueAsk *ColleagueAsk

	// Deadline is when the turn's wall-clock cap ends, zero for none —
	// written onto the row so the poll can end a run that outlasts it. See
	// [PendingRun.Deadline].
	Deadline time.Time
}

// LaunchRequest is everything one executor run needs.
//
// TWO TEXTS, KEPT APART to the end: what the harness is asked to do
// (Brief) and how it is to work (System). The runner hands the first to its
// harness as the prompt and the second on the harness's channel for adding to its own
// system prompt ([RunRequest.System]), so the vendor's harness prompt stays in
// force and the standing instructions sit beneath it rather than in front of
// the task as a message the model reads as part of the request.
type LaunchRequest struct {
	Turn TurnRef

	// System is the run's own standing instructions: the seat's executor
	// prompt. It is FRAMED as the brief is: what the box provides, and how
	// to ask a person, follow it ([buildSystem]).
	System string

	// Brief is the task: the turn's ask, as the executor's prompt renders
	// it, and the whole of what the harness is prompted with.
	Brief string

	// Task is what the launching turn's work is read by, in a person's
	// words (what it was asked, or the line a feed shows its trigger by),
	// carried onto the row ([PendingRun.TaskDescription]) as what the run is
	// listed under. Never the brief: a resume reads its brief off the
	// turn's park, where somebody else's words in it are fenced as the
	// model read them, and a person reading the row would read those fence
	// lines.
	Task string

	// Label is what a person sees the run named by — on the running-runs
	// panel, live and after a restart alike ([LaunchRequest.label]). The
	// launch names what its turn's work is read by ([LaunchRequest.Task]):
	// the brief is the turn's whole message, which opens with a heading
	// rather than with what the run is doing. Empty takes the brief's first
	// line.
	Label string

	// Ask is the addendum telling the harness how to get a person's
	// answer (asked with its tools when it can go on without it, or parked
	// through `ask_and_wait` when it cannot, ADR-0068) with the names it may
	// address. The ENGINE composes it, because the tools and the names are
	// the seat's and the chart's and this package holds neither; empty adds
	// nothing, and leaves a run that holds the tool with no word of when to
	// park on it. A standing instruction, so it rides System's channel
	// ([buildSystem]).
	Ask string

	Spec       Spec
	Setup      []SetupStep
	MCPServers map[string]MCPServer

	// Skills are the skills the job's harness is handed as its own — see
	// [RunRequest.Skills]. Nil hands none. Where the job reports loading one
	// is its opening's to say ([Opening.SkillLoads]), as the job's endpoint
	// is: a job handed skills and opened with nowhere to report a load is
	// refused by its runner.
	Skills []Skill

	// Model is the model the run works under — see [RunRequest.Model].
	// Empty names none, and the harness chooses its own.
	Model string

	// ProviderKey is the providers.llm entry the run was launched on: the
	// member of the seat's executor chain that took it, which is not always
	// the chain's head. Written onto the job's record ([LaunchRecord]) so
	// the launch's own record and the resumed executor's are charged to the
	// entry that served them, on whichever node collects the run.
	ProviderKey string

	// SignInID is the sign-in identity the run's harness is handed, written onto
	// the job's record ([LaunchRecord.SignInID]).
	SignInID string

	// Continue is set when this job CONTINUES the job before it, which
	// parked on a person's question that has now been answered, or was lost
	// with its host ([PendingRun.Lost]) — and so is the same round of the
	// turn, carried on rather than begun again. Nil for every other launch.
	//
	// The job continues the asking job's own harness session where it runs in
	// that job's box — the session lives there — with Continue.Brief, the
	// answer, as its next prompt ([RunRequest.Resume]). Where it
	// does not — the box was reclaimed while the question waited, or this
	// launch runs another harness or cell, or the job before it was lost, whose
	// session names none — it starts fresh with Brief, which then carries
	// the question and the answer, or how the job before it was cut off, and
	// where its work is; the decision is the launch's, made once it knows
	// which box it has ([Coordinator.acquire]). And either way the row keeps
	// the round's bridged calls: the ones made before the question, or the
	// loss, are this round's too ([PendingRun.Continues]).
	Continue *Continuation

	// Fence is the launching turn's own seat fence — its seat still held
	// under the grant the turn was admitted on, and no person's pause asking
	// its running work to stop — asked AGAIN once the job's row exists and
	// before any box does. Nil asks nothing: a launch no turn makes.
	//
	// ASKED AFTER THE ROW, not only before the launch, because a stop is
	// carried out by listing the seat's rows ([Coordinator.StopSeat]): one
	// that lands while a launch is between its first check and its row finds
	// no row to end, and the launch then starts a job on a seat a person has
	// just stopped. With the check after the row, every stop either finds
	// the row and ends it, or happened before this check and is refused by
	// it — the node takes the pause into its copy before it lists.
	Fence func() error

	// SteerSeat is the turn's seat, sealed by the engine — the anchor a
	// person's authority to send the turn a note is decided on
	// ([RunSteer.Seat]) — and BriefNotes the notes the job's brief carries,
	// each sealed, which the write that opens the job records read by it
	// ([PendingRun.BriefNotes]). HandedNotes are the notes the turn's first
	// segment's box still held unread as the launch began, each sealed, which
	// the same write files WAITING after them ([PendingRun.HandedNotes]). All
	// written onto the record the launch opens, never anywhere else. Empty
	// writes no anchor and files nothing.
	SteerSeat   []byte
	BriefNotes  []RunNote
	HandedNotes []RunNote

	// Recorded is told the job's name the moment the store has written the
	// record the launch opens ([PendingStore.BeginLaunch]) — before the
	// launch's fence is asked again, and before anything else can fail. From
	// that write the record holds the turn's notes, the brief's and the
	// box's ([LaunchRequest.HandedNotes]), and a launch that fails after it
	// ENDS the record, whose ending reports every one of them no job ran
	// with ([PendingRun.UnreadNotes]); a launch that fails before it wrote
	// nothing, and its notes are the caller's still. Nil tells nobody.
	Recorded func(launchID string)

	// Opened is told the job the moment the store has opened it
	// ([PendingStore.BeginLaunch]) and BEFORE ANY BOX EXISTS, for what must
	// name the job before its box can act: the run's bridge session, which
	// records every call the box makes under its own job
	// ([BridgeAppend.Launch]), and whose endpoint names the job it serves.
	// Learned any later — from the row, say, on the session's first append
	// — the name would be whichever job the row held by then, which for a
	// call that outlived its job is the next one. And where the job's calls
	// are numbered from ([OpenedJob.LastSeq]), off the same write.
	//
	// It answers what the job's box is given that can only be minted once
	// the job has a name ([Opening]): MCP servers BESIDE MCPServers — the
	// run's tool bridge is one, its token naming the job, so a node that
	// takes the run over can serve the job's session again under the token
	// the box already holds and a relaunch's box cannot reach its
	// predecessor's; a name it answers that MCPServers also has replaces
	// it — and where the job's harness asks for its turn's notes, which is the
	// same bridge. An error abandons the launch, before any box exists. Nil
	// tells nobody and adds nothing.
	Opened func(job OpenedJob) (Opening, error)

	// Now is the clock. Nil takes time.Now.
	Now func() time.Time
}

// OpenedJob is the job a launch opened, as [LaunchRequest.Opened] is told it.
type OpenedJob struct {
	// LaunchID names the job: the launch id the store minted for it, which
	// the run's row holds ([PendingRun.LaunchID]).
	LaunchID string

	// LastSeq is the place of the last bridged call the row keeps from
	// before this job ([PendingRun.LastBridgeSeq]): zero for a job whose
	// round starts its log afresh, and the asking job's last call for a job
	// that continues one which asked a person ([PendingRun.Continues]).
	//
	// READ OFF THE WRITE THAT OPENED THE JOB, which is the write that kept
	// those calls, so the job's own are numbered after exactly what the row
	// holds. Numbered from one instead, a continuation's calls were sorted
	// in among the asking job's by the collection that closes the round,
	// which reads the round's calls in that order: its record, the log its
	// reviewer reads, and its submissions, where the last one made wins.
	LastSeq int
}

// Opening is what a job's box is given once the job has a name — see
// [LaunchRequest.Opened].
type Opening struct {
	// Servers join the box's MCP servers.
	Servers map[string]MCPServer

	// Steer is where the job's harness asks for its turn's notes
	// ([RunRequest.Steer]); the zero value takes none inside the run.
	Steer SteerWiring

	// SkillLoads is where the job's harness reports loading one of the
	// skills it is handed as its own ([RunRequest.SkillLoads]); the zero
	// value names nowhere, which a job handed none needs.
	SkillLoads SkillWiring
}

// Continuation is how a job continues the job before it — see
// [LaunchRequest.Continue].
type Continuation struct {
	// SessionID is the asking job's harness session ([PendingRun.SessionID]).
	// Empty where its harness named none, or the job before was lost, which
	// continues nothing: the job starts fresh with the launch's Brief.
	SessionID string
	// Brief is the prompt the continued session is given: the answer, and
	// what to do with it.
	Brief string
	// Reason is why the round is carried on, where the job's collection
	// acts on it ([LaunchRecord.FollowUp]); empty otherwise.
	Reason FollowUpReason
}

// LaunchResult reports what a launch produced.
type LaunchResult struct {
	// LaunchID is the job the launch opened, as the store named it
	// ([PendingRun.LaunchID]).
	LaunchID string

	SandboxID string
	CommandID string
	Harness   string

	// Reused is whether the job continues in the box the turn's previous
	// job used, rather than in a fresh one — see [PriorBox.ContinuesIn].
	Reused bool
}

// Launch provisions a box, starts the harness detached, and persists the
// row that outlives the turn — and HOLDS THE SEAT from the moment that row is
// written.
//
// THE ORDER IS THE CONTRACT. The row is created and the box attached to it
// BEFORE the job starts: a crash in that window leaves a row naming a box
// nobody is using, which recovery can kill, while the reverse ordering leaves
// a box that nothing names and nothing can ever reclaim.
//
// On any failure after the box exists, the box is reclaimed before the error
// propagates — an unreferenced box is billed for until its TTL and collected
// by nobody.
//
// # The seat is held from the row, not from the announcement
//
// A run is counted into the seat's [Holding] set, and the seat's inbox hold
// taken ([SeatHold]), in the step that writes its row — before the box is
// provisioned, and so before the launching turn can return and its seat's
// consumer go on to the next thing it has. It used to be counted when its
// [types.ExecutorLaunched] was processed off the seat's control topic, which
// is a separate subscription the launching turn does not wait for: mail
// already drained beside the launching delivery (the next partition of the
// same batch) and mail fetched after the turn returned both reached a seat the
// screening called free, and ran a turn beside the job. The announcement still
// RECOUNTS the seat from the store wherever it is processed — that is what
// carries a held seat across a restart or a handoff — but nothing waits for it.
//
// A launch that fails after the row is written gives the count back as it
// abandons the row ([Coordinator.abandon]); one that fails to suspend into the
// row is settled by [Coordinator.FailRun], whose ending gives it back the same
// way.
//
// m is the manager the request's [Spec] was built with ([Manager.BuildSpec]),
// passed rather than read off the coordinator here: an apply landing between
// the two swaps the coordinator's, and a spec resolved against one catalogue
// is launched on that catalogue.
func (c *Coordinator) Launch(ctx context.Context, m *Manager, req LaunchRequest) (LaunchResult, error) {
	store, q := c.pending, c.queue
	now := req.Now
	if now == nil {
		now = time.Now
	}
	if strings.TrimSpace(req.Turn.TurnID) == "" {
		return LaunchResult{}, fmt.Errorf("sandbox: a launch needs a turn id")
	}
	if strings.TrimSpace(req.Brief) == "" {
		return LaunchResult{}, fmt.Errorf("sandbox: a launch needs a brief")
	}
	// THE RUNNER IS THE LAUNCH'S OWN, never a default: it is the harness of the
	// entry whose login and model the run carries, and a box running any
	// other harness could read neither. Refused before the row, so a request
	// built without one costs nothing.
	if req.Spec.Harness == "" {
		return LaunchResult{}, fmt.Errorf("sandbox: a launch needs the harness it runs")
	}
	// THE SEAT'S LEASE, which the row is stamped with and every write below
	// carries — see [CoordinatorOptions.Lease]. A seat this node does not
	// hold launches nothing: the turn asking is one its lease no longer
	// stands behind.
	fence, held := c.leaseOf(req.Turn.AgentHandle)
	if !held {
		return LaunchResult{}, notHeld(ctx, req.Turn.AgentHandle, req.Turn.TurnID, "launching a run")
	}

	// The row FIRST, so a crash between here and the box leaves a record
	// rather than nothing. It opens in [StatusLaunching] and stays there
	// until the turn writes the conversation a resume re-enters: nothing
	// polls or claims a run in that window, which is what stops a job that
	// finishes before the turn unwinds from being collected into nothing.
	opened, err := store.BeginLaunch(ctx, PendingRun{
		TurnID: req.Turn.TurnID, WorkKey: req.Turn.WorkKey,
		WorkSince:   req.Turn.WorkSince,
		AgentHandle: req.Turn.AgentHandle,
		AgentID:     req.Turn.AgentID, Role: req.Turn.Role,
		// WRITTEN WITH THE ROW, before the box exists, because every
		// failure path from here on reclaims through it — and because the
		// process that collects this run may not be this one, nor reading
		// the configuration that chose the cell.
		Placement:       string(req.Spec.Placement),
		Harness:         req.Spec.Harness,
		TaskDescription: req.Task,
		Label:           req.label(),
		PartitionKey:    req.Turn.PartitionKey,
		// The conversation an answer is matched on and the resume
		// reports back to, because a resume days later has neither the
		// trigger nor this frame.
		ConversationKey: req.Turn.ConversationKey,
		Reply:           req.Turn.Reply,
		TraceID:         req.Turn.TraceID, SpanID: req.Turn.SpanID,
		DelegationDepth: req.Turn.Depth, DelegationChain: req.Turn.Chain,
		WorkItem: req.Turn.WorkItem,
		// Who asked, for the park that resolves a question's audience:
		// that frame sees no trigger.
		Requester: req.Turn.Requester,
		// And the colleague's ask the turn answers, for the segment that
		// ends it: that frame sees no trigger either.
		ColleagueAsk: req.Turn.ColleagueAsk,
		// The turn's cap, for the poll — see [PendingRun.Deadline].
		Deadline: req.Turn.Deadline,
		// Whose host a local box is on — see [PendingRun.LaunchNode]. This
		// node's: the box is made here. A relaunch whose row still names the
		// previous job's box keeps that box's host until the attach below
		// names this node with this job's box ([BoxRef.Node]).
		LaunchNode: c.node,
		// The round carried on, whose earlier calls the row keeps — see
		// [PendingRun.Continues].
		Continues: req.Continue != nil,
		// The model and the entry the launch's own record is filed under;
		// the store keys the rest of that record on the launch it mints.
		Launch: LaunchRecord{Model: req.Model, ProviderKey: req.ProviderKey,
			SignInID: req.SignInID, FollowUp: followUpOf(req.Continue)},
		// THE TURN'S NOTES: the seat anchor, the brief's notes, read by the
		// job this write opens, and the box's, waiting after them — see
		// [PendingStore.BeginLaunch].
		Steer:       RunSteer{Seat: req.SteerSeat},
		BriefNotes:  req.BriefNotes,
		HandedNotes: req.HandedNotes,
		CreatedAt:   now(),
	}, fence)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("sandbox: recording the run: %w", err)
	}
	// THE RECORD HOLDS THE TURN'S NOTES FROM HERE — see
	// [LaunchRequest.Recorded]: told before anything below can fail, so the
	// caller knows the record, and no longer itself, answers for them.
	if req.Recorded != nil {
		req.Recorded(opened.LaunchID)
	}
	// HELD FROM HERE, before anything below can fail or the turn can
	// return — see [Coordinator.Launch]. Every failure past this point
	// gives it back through [Coordinator.abandon].
	c.countRun(req.Turn.AgentHandle, StatusLaunching)
	// A STOP OF THE TURN'S LAST LAUNCH IS NOT THIS ONE'S: this job is new.
	c.mu.Lock()
	delete(c.launchStops, req.Turn.TurnID)
	c.mu.Unlock()
	// THE TURN'S FENCE AGAIN, now that a stop can find the row — see
	// [LaunchRequest.Fence]. Refused, the launch is over before any box
	// exists, and the error is the fence's own, so the turn reads it as the
	// stop or the move it is.
	if req.Fence != nil {
		if fenceErr := req.Fence(); fenceErr != nil {
			c.abandon(ctx, m, req, fence, "")
			return LaunchResult{}, fenceErr
		}
	}
	// THE JOB IS NAMED BEFORE ITS BOX EXISTS, so nothing the box does can be
	// filed under a job that is not its own — see [LaunchRequest.Opened] —
	// and what it answers joins the box's servers, on the request every step
	// below reads: the job's own runner and the standing instructions that
	// list the servers a box has.
	var steer SteerWiring
	var skillLoads SkillWiring
	if req.Opened != nil {
		named, openErr := req.Opened(OpenedJob{LaunchID: opened.LaunchID, LastSeq: opened.LastBridgeSeq()})
		if openErr != nil {
			c.abandon(ctx, m, req, fence, "")
			return LaunchResult{}, fmt.Errorf("sandbox: naming the job to what serves its box: %w", openErr)
		}
		if len(named.Servers) > 0 {
			servers := make(map[string]MCPServer, len(req.MCPServers)+len(named.Servers))
			maps.Copy(servers, req.MCPServers)
			maps.Copy(servers, named.Servers)
			req.MCPServers = servers
		}
		steer = named.Steer
		skillLoads = named.SkillLoads
	}

	// THE BOX IS MADE FOR THIS TURN, which a local box records so its host
	// can tell when no run names it any more ([Spec.TurnID]).
	req.Spec.TurnID = req.Turn.TurnID
	box, runner, reused, err := c.acquire(ctx, m, req, opened.Prior)
	if err != nil {
		// The row is open and this launch is over. Every failure past this
		// point closes it: a run left LAUNCHING is polled by nothing and
		// claimed by nothing, so it sits on its seat's busy count and its
		// box until the seat happens to move to another node.
		c.abandon(ctx, m, req, fence, "")
		return LaunchResult{}, err
	}

	// Attached before the job starts, for the reason above — WITH ITS CELL,
	// in the same write: a relaunch may run in another cell than the job
	// before it, and the row is how every later reader reaches the box.
	if err = store.AttachSandbox(ctx, req.Turn.TurnID, BoxRef{
		SandboxID: box.ID(), Placement: string(req.Spec.Placement), Node: c.node,
		Harness: req.Spec.Harness, PauseTTLSec: req.Spec.PauseTTLSec,
	}, fence); err != nil {
		c.abandon(ctx, m, req, fence, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: attaching the box: %w", err)
	}
	// THE PREVIOUS JOB'S BOX ON ANOTHER NODE'S HOST is named by nothing from
	// here, which is the moment its host may reclaim it: told now rather
	// than found at its next pass ([Coordinator.acquire]).
	if prior := opened.Prior; prior.SandboxID != "" && !c.hostsPrior(m, prior) {
		c.tellHost(ctx, PendingRun{TurnID: req.Turn.TurnID, LaunchID: opened.LaunchID})
	}
	// A KEPT BOX'S EXPIRING FILES, brought up to date before the job that
	// continues in it starts: it ran no setup, so they hold what the last
	// poll of the job before wrote, which a pause may have outlived — see
	// [CoordinatorOptions.Refresh]. Handed the row as this launch opened
	// it, which already names this box, its cell, its harness and its host: a
	// kept box is the previous job's, continued only where all four match
	// ([PriorBox.ContinuesIn], [Coordinator.hostsPrior]).
	if reused && c.refresh != nil {
		c.refresh(ctx, m, opened, box)
	}

	// THE ASKING JOB'S SESSION, CONTINUED, only where this job runs in the
	// box that session lives in; anywhere else the job starts fresh, told
	// the question and the answer in its brief ([LaunchRequest.Continue]).
	brief, session := req.Brief, ""
	if continued := req.Continue; continued != nil && reused && continued.SessionID != "" {
		brief, session = continued.Brief, continued.SessionID
	}
	handle, err := runner.Start(ctx, box, RunRequest{
		System: buildSystem(req),
		Brief:  brief,
		Resume: session,
		Env:    withTelemetry(m, req, box),
		// FROM THE SPEC, which is where the provider default and the
		// seat's override have already been reconciled. A Limits the
		// caller passed alongside would be a second answer to the same
		// question, and it was the one nobody filled in: every launch
		// went out uncapped while the field looked wired.
		Limits:     Limits{MaxTurns: req.Spec.MaxTurns},
		Model:      req.Model,
		MCPServers: req.MCPServers,
		// WHERE THE JOB'S HARNESS ASKS FOR ITS TURN'S NOTES, as the job's own
		// opening answered it — see [Opening.Steer].
		Steer: steer,
		// THE SKILLS ITS HARNESS IS HANDED AS ITS OWN, and where it reports
		// loading one, as the job's own opening answered it — see
		// [Opening.SkillLoads].
		Skills:     req.Skills,
		SkillLoads: skillLoads,
		// THE CREDIT ON THE JOB'S OWN COMMITS, naming the turn: the run's
		// page is addressed by it, and every job of the turn is that one
		// run's ([Manager.CommitAttribution]).
		Attribution: m.CommitAttribution(req.Turn.TurnID),
	})
	if err != nil {
		c.abandon(ctx, m, req, fence, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: starting the harness: %w", err)
	}

	// The command id is written SECOND, once the job exists: a row naming a
	// command that was never started would have the waiter poll for a
	// completion marker nothing is ever going to write.
	if err := store.AttachSandbox(ctx, req.Turn.TurnID, BoxRef{
		SandboxID: box.ID(), CommandID: handle.CommandID,
		Placement: string(req.Spec.Placement), Node: c.node,
		Harness: req.Spec.Harness, SessionID: handle.SessionID,
		PauseTTLSec: req.Spec.PauseTTLSec,
	}, fence); err != nil {
		c.abandon(ctx, m, req, fence, box.ID())
		return LaunchResult{}, fmt.Errorf("sandbox: recording the job: %w", err)
	}

	// WHICH JOB THIS IS, as the store opened it: the announcement is what a
	// watcher pairs a start with its job's record by, and what a request for
	// its live output names. The store's own answer rather than a read of the
	// row back, which could fail and cost the pairing — and which could, in
	// principle, find a later job than this one.
	started := types.ExecutorLaunched{
		LaunchID: opened.LaunchID, StartedAt: opened.Launch.StartedAt,
		Agent: req.Turn.AgentID, AgentHandle: req.Turn.AgentHandle,
		RoleName: req.Turn.Role, TurnID: req.Turn.TurnID, WorkKey: req.Turn.WorkKey,
		SandboxID: box.ID(), Harness: req.Spec.Harness,
		// THE MEMBER THAT TOOK IT — see [types.ExecutorLaunched.ProviderKey].
		ProviderKey: req.ProviderKey, Model: req.Model,
		// THE IDENTITY on the announcement: this event is read for
		// display, and what a person means by "which conversation is
		// this run for" is the durable thread rather than the batch the
		// trigger arrived in.
		ConversationKey: req.Turn.ConversationKey,
		WorkItem:        req.Turn.WorkItem,
		Task:            req.label(),
	}
	ev := events.New(started, events.TraceContext{
		TraceID: req.Turn.TraceID, ParentSpanID: req.Turn.SpanID,
	})
	ev.Source = req.Turn.Role

	// TWO PUBLISHES, as for a completion. The events copy is the
	// announcement the dashboard's running-sandboxes panel reads; the
	// per-seat control copy RECOUNTS the seat from the store on whichever
	// node holds it when the copy is processed. The seat is already held
	// here — the row's write counted it — so nothing waits for this; what
	// the copy adds is the store's own answer, after a restart or a handoff
	// as much as now.
	if err := q.Publish(ctx, topics.Event(started.EventType()), ev); err != nil {
		log.WarnContext(ctx, "sandbox_started_publish_failed",
			"turn_id", req.Turn.TurnID, "error", err.Error())
	}
	if control := controlSubject(req.Turn.AgentID); control != "" {
		if err := q.Publish(ctx, control, ev); err != nil {
			log.WarnContext(ctx, "sandbox_started_control_failed",
				"turn_id", req.Turn.TurnID, "error", err.Error())
		}
	}

	log.InfoContext(ctx, "executor_launched",
		"turn_id", req.Turn.TurnID, "agent", req.Turn.AgentHandle,
		"sandbox_id", box.ID(), "placement", string(req.Spec.Placement),
		"harness", req.Spec.Harness, "provider_key", req.ProviderKey,
		"reused", reused, "continues_session", session != "")

	return LaunchResult{
		LaunchID: opened.LaunchID, SandboxID: box.ID(), CommandID: handle.CommandID,
		Harness: req.Spec.Harness, Reused: reused,
	}, nil
}

// acquire is the box a launch runs its job in: the turn's previous box where
// the new job may continue in it, else a fresh one — and never a fresh one
// while the previous box is still unaccounted for.
//
// CONTINUING KEEPS THE CHECKOUT, which is the expensive half of a run: a turn
// its reviewer sent back relaunches its executor on the same work, and a job
// that continues where the last one stopped need not clone, install and set up
// again. Only the same harness in the same cell, once the last job is over
// ([PriorBox.ContinuesIn]).
//
// EVERY OTHER PRIOR BOX IS RECLAIMED HERE, THROUGH ITS OWN CELL, before the
// fresh box exists. The row is about to name the new box instead — the attach
// that follows overwrites the reference — and nothing else would ever name the
// old one again: the turn's settle reclaims the box the row names, and the
// pause reaper the box a row records as paused. A relaunch that provisioned
// beside it left a paused box, a stopped process tree or a billed snapshot for
// every send-back of a turn, reclaimed by nobody. Its own cell, because it may
// not be this launch's: a fallback onto a member with another run_in moves the
// run, and a box asked for through the wrong backend is a box that "vanished".
//
// A box that cannot be reattached is reclaimed too, then replaced: the work
// re-seeds from the pushed branch, which is the case that branch exists for,
// and a reattach that failed for a moment must not leave a live box behind it.
func (c *Coordinator) acquire(ctx context.Context, m *Manager, req LaunchRequest, prior PriorBox) (Sandbox, Runner, bool, error) {
	if prior.SandboxID != "" && !c.hostsPrior(m, prior) {
		// ANOTHER NODE'S HOST: the seat moved since the previous job
		// launched, and its box is a process tree or a container there,
		// which this node can neither continue in nor kill. Left to that
		// node, whose waiter reclaims it once the row names this job's box
		// instead (boxhost.go) — told to look once it does ([Coordinator.Launch]);
		// the job starts fresh here, from the pushed branch, as one whose
		// box was reclaimed does.
		log.InfoContext(ctx, "sandbox_prior_left_to_host",
			"turn_id", req.Turn.TurnID, "sandbox_id", prior.SandboxID,
			"placement", prior.Placement, "node", prior.LaunchNode)
		prior = PriorBox{}
	}
	if prior.SandboxID != "" {
		if prior.ContinuesIn(req.Spec) {
			box, runner, err := m.Reconnect(ctx, req.Spec.Placement, prior.SandboxID, req.Spec.Harness)
			if err == nil {
				return box, runner, true, nil
			}
			log.WarnContext(ctx, "sandbox_reuse_failed",
				"turn_id", req.Turn.TurnID, "sandbox_id", prior.SandboxID, "error", err.Error(),
				"detail", "the turn's previous box could not be reattached; it is reclaimed "+
					"and the job starts in a fresh one, from the pushed branch")
		}
		c.reclaimPrior(ctx, m, req, prior)
	}
	box, runner, err := m.Acquire(ctx, req.Spec, req.Setup)
	if err != nil {
		return nil, nil, false, err
	}
	return box, runner, false, nil
}

// hostsPrior reports whether this node can reach the turn's previous box: one
// in a cell every node reaches, or one this node launched ([Coordinator.hosts]).
// The cell is resolved through the newest catalogue first, as the reclaim's is,
// and the launch's where that one does not name it.
func (c *Coordinator) hostsPrior(m *Manager, prior PriorBox) bool {
	cell := Placement(prior.Placement)
	if cell == "" {
		if current := c.mgr(); current != nil {
			cell = current.DefaultPlacement()
		} else {
			cell = m.DefaultPlacement()
		}
	}
	return !cell.OnEngineHost() || prior.LaunchNode == c.node
}

// reclaimPrior kills the turn's previous box through the cell that holds it.
//
// THROUGH THE COORDINATOR'S CATALOGUE FIRST, then the launch's. The launch's
// is the epoch its turn pinned, which configures the cells a NEW box may go
// to; the coordinator's is the newest one, carrying every cell a box already
// out may still be in ([Coordinator.SetManager]) — a cell a later revision
// dropped included, which is exactly where a run that is moving cells may have
// left its last box.
//
// BEST EFFORT, and the launch goes ahead whatever it answers: a kill that
// failed leaves the box to its own TTL and the orphan reaper, which is what any
// unreclaimable box gets, and refusing the launch over it would cost the turn
// as well as the box. Under a context of its own, for the reason every
// reclaim takes one ([Coordinator.abandon]).
func (c *Coordinator) reclaimPrior(ctx context.Context, m *Manager, req LaunchRequest, prior PriorBox) {
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	provider, err := c.mgr().Provider(Placement(prior.Placement))
	if err != nil {
		provider, err = m.Provider(Placement(prior.Placement))
	}
	if err != nil {
		log.WarnContext(ctx, "sandbox_prior_reclaim_no_backend",
			"turn_id", req.Turn.TurnID, "sandbox_id", prior.SandboxID,
			"placement", prior.Placement, "error", err.Error())
		return
	}
	if err := provider.Kill(killCtx, prior.SandboxID); err != nil {
		log.WarnContext(ctx, "sandbox_prior_reclaim_failed",
			"turn_id", req.Turn.TurnID, "sandbox_id", prior.SandboxID,
			"placement", prior.Placement, "error", err.Error())
		return
	}
	log.InfoContext(ctx, "sandbox_prior_reclaimed",
		"turn_id", req.Turn.TurnID, "sandbox_id", prior.SandboxID,
		"placement", prior.Placement, "harness", prior.Harness,
		"next_placement", string(req.Spec.Placement), "next_harness", req.Spec.Harness)
}

// abandon closes out a launch that could not finish: the box is reclaimed, then
// the run is ended, which deletes its record, and the seat the launch held is
// given back.
//
// ALL OF IT, EVERY TIME, and each step attempted whether or not the one before
// it succeeded: they are separate calls that fail separately, and none failing
// is a reason to leave the others undone. Three of the four failure paths used
// to do none of it and simply return, which left the row OPEN: a launching run
// is polled by nothing and claimed by nothing, so it held its seat and, on two
// of those paths, a box, until the seat happened to move to another node and
// recovery reaped it. The box goes first, so a record that cannot be deleted
// names a box that is already gone rather than the reverse.
//
// THE ENDING EVERY OTHER PATH MAKES ([Coordinator.endRecord]), not a delete of
// its own: that is the one place a run's record is deleted, so it is the one
// place a stop is reported and the one place what the row still owes the seat
// goes out before the row does (see [PendingRun.HandBack]) — a relaunch is
// opened on a row that has carried a whole run's history.
//
// A context of its own, because the failure that got us here is often the
// caller's context expiring — and a teardown skipped for that reason leaves a
// box running to its TTL with nobody to collect it, and a row nobody settles.
func (c *Coordinator) abandon(ctx context.Context, m *Manager, req LaunchRequest, fence Fence, sandboxID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	if sandboxID != "" {
		provider, err := m.Provider(req.Spec.Placement)
		if err != nil {
			log.WarnContext(ctx, "sandbox_launch_reclaim_no_backend",
				"sandbox_id", sandboxID, "placement", string(req.Spec.Placement),
				"error", err.Error())
		} else if err := provider.Kill(ctx, sandboxID); err != nil {
			log.WarnContext(ctx, "sandbox_launch_reclaim_failed",
				"sandbox_id", sandboxID, "error", err.Error())
		}
	}
	// Every live status and every job: this launch is abandoned whatever the
	// row reached. TWO BOXES CAN BE OWED A RECLAIM, and each is reclaimed by
	// the one party that can name it. The launch's own is killed above,
	// because the launch may never have attached it to the row. The box the
	// ROW names is reclaimed by the ending, after its decision: a relaunch
	// that never got a box of its own leaves the row naming the previous
	// job's paused one, and the ending deletes the only record of it — so a
	// box left to the turn's own settle was named by nothing once the turn
	// came back, and was killed only off that settle's stale snapshot, which
	// a settle may no longer act on ([Coordinator.finish]). Where the two are
	// one box — the attach landed — it is killed twice, which every provider
	// answers as the reclaim it wanted: a kill of a box already gone succeeds.
	run := PendingRun{TurnID: req.Turn.TurnID, AgentHandle: req.Turn.AgentHandle}
	if _, err := c.endRecord(ctx, run, ending{
		fence: fence, whileIn: Active, launch: EveryLaunch, reclaim: true,
	}); err != nil {
		log.WarnContext(ctx, "sandbox_launch_finish_kept",
			"turn_id", req.Turn.TurnID, "error", err.Error(),
			"detail", "the abandoned launch's record is not deleted yet; this node finishes "+
				"the ending, and the seat's next recovery pass reaps the row if the seat "+
				"moves first")
	}
	// AND THE SEAT GOES BACK whether or not the record could be deleted:
	// the launch is over and its box reclaimed, so nothing is driving a job
	// the seat's mail must wait behind. A record that survived the delete is
	// a launching row whose ending this node keeps and retries.
	c.uncountRun(req.Turn.AgentHandle, StatusLaunching)
}

// label is what the run is named by, cut by [summarise]: the caller's own
// label where it gave one, else the brief's first line. ONE ANSWER for the
// announcement and the record, so a run a process learns of from the record
// after a restart is named exactly as the one it saw announced.
func (r LaunchRequest) label() string {
	if strings.TrimSpace(r.Label) != "" {
		return summarise(r.Label)
	}
	return summarise(r.Brief)
}

// summarise is a brief's first line, WHOLE: the label a running-runs panel
// draws for a run. The record keeps the label it cut ([PendingRun.Label]), so
// a process that learns of the run from the record after a restart names it as
// the announcement did, with no second rule of its own.
//
// The first line because that is where a brief states what it is about. Never
// cut to a width: it used to be held to 120 bytes and marked, which put a
// sentence's first half on the run's page as its title — and a row too narrow
// for a label is the renderer's to elide, which the dashboard's rows already
// do, with the whole line still there to read.
func summarise(brief string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(brief), "\n")
	return strings.TrimSpace(line)
}

// buildSystem assembles the run's standing instructions: what its harness adds to
// its own system prompt ([RunRequest.System]).
//
// THREE PARTS, each present only where it says something: the run's own
// instructions (the seat's executor prompt), what the environment already
// provides, and how to stop and ask a person. The brief is not among them:
// what the box provides and how to ask a person hold for the whole run rather
// than being part of its request, which is what a system prompt is for. The environment is the
// setup steps' own briefs — the mechanism and the hint together, so the agent
// does not spend rounds rediscovering that git auth is already wired — and,
// like the ask, it holds for every round of the run, which is what a system
// prompt is for. The runner adds the last standing instruction, the report
// it must write, because only it knows the box's path for it.
//
// THE RUN'S OWN TEXT FIRST, the framing after it: that text is who the model is
// and what it answers for, and the box and how to ask a person are details of
// where it is working.
func buildSystem(req LaunchRequest) string {
	names := slices.Collect(maps.Keys(req.MCPServers))
	own := strings.TrimSpace(req.System)
	if own != "" {
		own = agentRunNote + "\n\n" + own
	}
	parts := []string{
		own,
		strings.TrimSpace(EnvironmentBrief(req.Setup, names)),
		strings.TrimSpace(req.Ask),
	}
	return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), "\n\n")
}

// agentRunNote opens a run's own instructions — the seat's executor prompt —
// where they follow the harness's system prompt.
//
// The vendor's prompt says what the model is, and says it first: Claude Code
// opens with "You are Claude Code, Anthropic's official harness for Claude", and
// no flag removes it. A seat prompt that only implies who it is loses to that,
// so this says outright which of the two prompts is the role. The engine's own
// runs on this host open with the same note (internal/providers/llm/harness
// harnessNote), less the sentence it adds on a call that offers tools — act
// through the `crewlet` server's tools, never the harness's own: here the run's
// shell and editor are what the box is for, and the seat's tools are called
// the way its harness calls any tool. Constant, so it stays part of the prefix a
// vendor caches.
const agentRunNote = "You are running inside a command-line coding tool as one seat of a " +
	"Crewlet company, and Crewlet — not a person at a terminal — reads what you " +
	"report. The tool's own instructions describe the program you run in, not your " +
	"role: who you are and what you do are the instructions that follow, and they " +
	"win wherever the two disagree."

// withTelemetry adds the run's OTel environment to the spec's.
//
// MERGED HERE rather than in the spec, because the endpoint carries a token
// scoped to THIS run and minted with a TTL: putting it in the spec would have
// it persisted with the pending row and re-used by a resume hours later,
// against a token that expired while the run was parked.
//
// THE OPERATOR'S OWN VALUES WIN. role.sandbox.env is where an operator
// declares what a box gets, and a deployment that points its harnesses at
// its own collector directly has said so deliberately — silently overriding
// it would be the engine choosing where somebody else's telemetry goes.
//
// THE ENDPOINT IS THE RECEIVER'S AS BOX DIALS IT: a `direct` box, a process of
// this host, exports to its node's own listener where the node forwards, and
// every other box to the fleet's address ([OtelReceiver.Reaches]); that
// address is written in the engine host's network, and a loopback one reaches
// nothing from inside a container ([EngineAddress]). An operator's own value
// is left as written, in the box's network already.
func withTelemetry(m *Manager, req LaunchRequest, box Sandbox) map[string]string {
	if m.telemetry == nil {
		return req.Spec.Env
	}
	added := m.telemetry.RunEnv(req.Turn.TraceID, req.Turn.SpanID,
		req.Turn.TurnID, req.Turn.AgentHandle, req.Spec.Placement == Direct,
		otelTokenTTL(req.Spec))
	if len(added) == 0 {
		return req.Spec.Env
	}
	if endpoint, ok := added[otelBoxEndpointVar]; ok {
		added[otelBoxEndpointVar] = EngineAddress(box, endpoint)
	}
	env := make(map[string]string, len(req.Spec.Env)+len(added))
	for k, v := range added {
		env[k] = v
	}
	for k, v := range req.Spec.Env {
		env[k] = v
	}
	return env
}

// otelTokenTTL is how long a run's export endpoint stays valid.
//
// TIED TO THE BOX'S OWN LIFETIME rather than picked: a token that expires
// while its box is still running loses the tail of the run's telemetry —
// exactly the part that explains a failure — and one that outlives the box is
// a credential nothing needs. The pause TTL is added because a run parked on
// a human's answer resumes into the same box and keeps exporting, and the
// doubling covers the keepalive refreshing the box's own timer.
func otelTokenTTL(spec Spec) time.Duration {
	ttl := time.Duration(spec.TimeoutSec) * time.Second
	if ttl <= 0 {
		ttl = DefaultBoxTimeout
	}
	ttl *= 2
	if pause := time.Duration(spec.PauseTTLSec) * time.Second; pause > 0 {
		ttl += pause
	}
	return ttl
}

// RunEnvFor is [withTelemetry] under a name a test can reach.
//
// EXPORTED FOR THE ONE PROPERTY THAT IS OTHERWISE UNOBSERVABLE: the merge
// order between the engine's telemetry variables and the operator's own. Both
// end up inside a box that a test cannot look into, and getting the order
// wrong sends somebody else's spans to the engine — silently, because they do
// arrive somewhere.
func RunEnvFor(m *Manager, req LaunchRequest, box Sandbox) map[string]string {
	return withTelemetry(m, req, box)
}

// handBackSubject is where a reply a run let go of goes back to: the seat's
// control topic for an answer BY TURN, which is addressed to a run and is
// settled there ([Coordinator.OnAnswerGiven]), and the seat's inbox for a chat
// reply, which becomes the ordinary message it looks like. Empty when the row
// names no routable seat.
func handBackSubject(agentID string, ev *events.Event) string {
	if _, ok := events.DataAs[*types.ExecutorAnswerGiven](ev); ok {
		return controlSubject(agentID)
	}
	return inboxSubject(agentID)
}

// followUpOf is the reason a launch carries a round on, as its job's record
// keeps it.
func followUpOf(c *Continuation) FollowUpReason {
	if c == nil {
		return ""
	}
	return c.Reason
}
