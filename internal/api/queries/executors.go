package queries

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/runanswer"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The executors answer: every detached executor the engine still holds.
//
// THE LIVE PROJECTION IS THE WRONG SOURCE: it is in memory, empty after a
// restart, and sweeps an entry after hours while a run parked on a question can
// wait days. A `reseed` run would have no surface and look finished.
//
// This reads the fleet's durable run record, every node's runs, because a run
// is recovered by whichever node owns its seat and a per-node read would
// disagree with itself.
//
// It deliberately does NOT return the serialised Execute conversation: the
// row's largest column, its prompts already reachable through the event store.

// PendingRuns is the durable record an executors answer reads.
//
// NO STATUS FILTER: a run's record is deleted once its ending is finished, so
// [sandbox.Active] is every status a record can hold; a run whose ending is
// decided but unfinished stays on the board (`ending`). A `done` or `failed`
// filter would be structurally empty and read as "nothing failed". How a run
// ENDED is on the event stream: `executor_failed` names the reason,
// `executor_stopped` the person.
//
// Get reads one run by its turn: `executor_tail` learns whose seat a run is
// from the record before showing that seat's transcript.
type PendingRuns interface {
	ListActive(ctx context.Context) ([]sandbox.PendingRun, error)
	Get(ctx context.Context, turnID string) (sandbox.PendingRun, bool, error)
	BridgeLog(ctx context.Context, run sandbox.PendingRun) (sandbox.BridgeLog, error)
}

// ExecutorTails answers a running executor's live output from the node that
// launched its box — the one method this surface calls of [sandbox.TailReader].
type ExecutorTails interface {
	Tail(ctx context.Context, q sandbox.TailQuery) (sandbox.TailAnswer, error)
}

// executorTail answers `executor_tail{turn_id, launch_id}`: the tail of that
// launch while it runs, `launching` while its box is being made, `not_running`
// with the record's status once it is not, `box_paused`, `box_gone`, or the
// node hosting the box NAMED where it did not answer (`host_silent`).
//
// BOTH IDS ARE REQUIRED: a turn-only request would show another job once a
// second launch replaces the first. BY CURSOR: `epoch`, `after` and `digest`
// say what the asker holds (all absent for nothing yet); the offset is whole
// bytes. DECIDED ON THE RUN'S SEAT, as its trail: a harness's output is its
// transcript, read by the seat's lead or an admin, never by everybody the
// run's status reaches. See [Sources.mayTail].
func (s Sources) executorTail(ctx context.Context, p Params) (any, error) {
	turnID := strings.TrimSpace(p.String("turn_id"))
	launchID := strings.TrimSpace(p.String("launch_id"))
	if turnID == "" || launchID == "" {
		return nil, fmt.Errorf("%w: executor_tail needs a turn_id and the launch_id of the "+
			"run's job", ErrBadParams)
	}
	var after int64
	if p.Has("after") {
		var whole bool
		if after, whole = p.WholeInt("after"); !whole || after < 0 {
			return nil, fmt.Errorf("%w: executor_tail's after is the byte offset the asker "+
				"holds through, a whole number of zero or more", ErrBadParams)
		}
	}
	gone, err := s.mayTail(ctx, turnID)
	if err != nil {
		return nil, err
	}
	if gone {
		// NO RECORD NAMES THE RUN: nothing says whose it is, and a settled
		// run's record is deleted, so `not_running` with no status is what
		// the tail reader would answer. Answered here because its own read
		// is later and could find a record this decision never saw.
		return sandbox.TailAnswer{Outcome: sandbox.TailNotRunning,
			TurnID: turnID, LaunchID: launchID}, nil
	}
	return s.ExecutorTail.Tail(ctx, sandbox.TailQuery{
		TurnID: turnID, LaunchID: launchID,
		Cursor: sandbox.TailCursor{Epoch: p.String("epoch"), Offset: after, Digest: p.String("digest")},
	})
}

// mayTail decides whether the caller may read the live output of the run of
// turnID, and reports a run no record names as gone.
//
// The COMPANY-WIDE trail ([authz.ActionAuditRead]) reads every run's, decided
// first without a record; a node with no run record decides nothing else and
// refuses with that. Anybody else is decided on the run's SEAT
// ([authz.ActionSeatTrailRead], its lead), which only the record names and
// never changes, so this read and the tail reader's later one agree. That verb
// is decided FIRST on an unresolved run ([authz.Object.Unresolved], under
// [unresolvedRunSeat]): a caller who leads nobody is refused there, and only
// one told to resolve ([authz.ErrUnresolved]) has the record read, so a
// failing run store never sends them to a refusal it cannot change.
func (s Sources) mayTail(ctx context.Context, turnID string) (gone bool, err error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return false, unresolved(ctx, "executor_tail")
	}
	company := authz.Decide(ctx, principal, authz.ActionAuditRead,
		authz.Object{Kind: authz.KindCompany}, s.Chart, s.clock())
	switch {
	case company.Allowed:
		return false, nil
	case company.Unknown():
		return false, fmt.Errorf("%w: this node cannot decide %s yet: %w",
			ErrUnavailable, authz.ActionAuditRead, company.Err)
	case s.Sandbox == nil:
		return false, NewRefusal("executor_tail", company)
	}
	first := authz.Decide(ctx, principal, authz.ActionSeatTrailRead, authz.Object{
		Kind: authz.KindPerson, ID: turnID, Owner: unresolvedRunSeat, Unresolved: true,
	}, s.Chart, s.clock())
	switch {
	case first.Allowed:
		return false, nil
	case errors.Is(first.Err, authz.ErrUnresolved):
		// LEADS SOMEBODY: read the record and decide on its seat below.
	case first.Unknown():
		return false, fmt.Errorf("%w: this node cannot decide %s yet: %w",
			ErrUnavailable, authz.ActionSeatTrailRead, first.Err)
	default:
		return false, NewRefusal("executor_tail", first)
	}
	run, found, err := s.Sandbox.Get(ctx, turnID)
	switch {
	case err != nil:
		return false, fmt.Errorf("%w: the record of run %s, which names its seat, "+
			"could not be read: %w", ErrUnavailable, turnID, err)
	case !found:
		return true, nil
	}
	return false, s.mayRead(ctx, principal, authz.ActionSeatTrailRead, run.AgentHandle)
}

// unresolvedRunSeat is the owner a run nobody has resolved to its seat is
// decided under. NOT EMPTY, because the owner-or-lead class refuses an empty
// owner as unnamed before it reaches the arm that asks whether the caller
// leads anybody; and NOT THE TURN ID, which has a seat handle's shape and so
// could equal a caller's seat and read as their own record. Parentheses are
// in neither a login's grammar nor a seat handle's, so nobody's login or seat
// is ever this.
const unresolvedRunSeat = "(unresolved)"

// executorRows answers the board, or — with `audience=<name>` — the runs whose
// question is put to that one person and still waits for them ([putTo]).
//
// THE NAME IS A RECORD'S ([iam.OwnerOf]): the caller's own with no name, a
// person's LOGIN resolved to their seat through the identity directory (never
// read as a handle), anything else as the seat it names. The directory is
// asked only once [authz.ActionPersonRead] admits looking. NO SCOPE RULE
// beyond that: the unfiltered board carries every audience. A run whose
// question resolved to nobody is nobody's by this filter.
//
// EVERY RUN'S STATUS IS EVERYBODY'S; what it was asked and answered is its
// seat's trail: see [runKeys] and [Sources.runReach].
func (s Sources) executorRows(ctx context.Context, p Params) (any, error) {
	runs, err := s.Sandbox.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	who, filtered := "", false
	if asked := strings.TrimSpace(p.String("audience")); asked != "" {
		if who, err = s.audienceOf(ctx, asked); err != nil {
			return nil, err
		}
		filtered = true
	}
	reach := s.runReach(ctx)
	out := make([]any, 0, len(runs))
	for _, run := range runs {
		if filtered && !putTo(run, who, s.delegating) {
			continue
		}
		r := reach(run)
		calls, err := s.runCalls(ctx, run, r)
		if err != nil {
			return nil, err
		}
		waiting, err := s.waiting(ctx, run, r.Record)
		if err != nil {
			return nil, err
		}
		out = append(out, serialiseRun(run, calls, r, s.opened(run, waiting.MayAnswer), waiting))
	}
	return map[string]any{"runs": out}, nil
}

// runCalls is the log of a run's bridged calls as reader r is shown it.
//
// READ ONLY FOR A READER SHOWN THE CALLS: the log is kept beside the row
// ([sandbox.BridgeLog]) and is up to two hundred calls with their arguments and
// output, so an answer to a reader the run's body is withheld from never reads
// one, and the zero log it answers then is never drawn. A log that cannot be
// read fails the answer rather than drawing a run that called nothing.
func (s Sources) runCalls(ctx context.Context, run sandbox.PendingRun, r livestate.Reach) (sandbox.BridgeLog, error) {
	if !r.Body {
		return sandbox.BridgeLog{}, nil
	}
	return s.Sandbox.BridgeLog(ctx, run)
}

// runReach is what the asking principal may read of each executor: its
// seat's body where the authority table's `audit.seat.read` admits them to
// the run's seat — an admin, or its lead — through [Sources.reach], the one
// decision a seat's live call, its turns and its memory are read under; its
// work item where they hold Worklet; and their own record, which a run's
// question may be put to.
//
// ONE DECISION PER SEAT per answer, however many runs it has: a board of runs
// is a handful of seats, and the decision asks the chart.
func (s Sources) runReach(ctx context.Context) func(sandbox.PendingRun) livestate.Reach {
	decided := map[string]livestate.Reach{}
	return func(run sandbox.PendingRun) livestate.Reach {
		r, ok := decided[run.AgentHandle]
		if !ok {
			r = s.reach(ctx, run.AgentHandle)
			decided[run.AgentHandle] = r
		}
		return r
	}
}

// putTo reports whether a run's question is put to this record and is still
// waiting for it.
//
// STILL WAITING, because "what is waiting on me" is the question: an answered
// ([sandbox.StatusAnswered]) or resuming run keeps its audience, and an
// answered question must not be shown again; a run whose ending is decided
// takes no answer. The home screen's decisions ask the same, through this.
//
// ONE RECORD, never a set of identities: audiences are seat handles, and a
// caller's name is resolved to one record first ([Sources.audienceOf],
// [iam.OwnerOf]). An empty record is put nothing.
func putTo(run sandbox.PendingRun, record string, delegating func(sandbox.PendingRun) bool) bool {
	return record != "" && run.Ending == nil && slices.Contains(sandbox.Awaiting, run.Status) &&
		slices.Contains(run.AudienceHandles, record) && !delegating(run)
}

// waiting is where run stands for the reader in ctx, whose record is record
// ([runanswer.Waiting]): who it waits on now, whether [putTo] puts it to them,
// and whether they may answer it, decided by [runanswer.May] — the verb and
// object the answer's own route decides on, at the read's proof. A run that
// waits on nobody (running, answered, ending) is put to nobody and answered
// by nobody.
//
// UNDECIDABLE IS AN ERROR, never a "no": a node whose chart is behind cannot
// say whether a lead may answer, and drawing their run as one they may not
// would hide exactly what they came to unblock.
func (s Sources) waiting(ctx context.Context, run sandbox.PendingRun, record string) (runanswer.Waiting, error) {
	if run.Ending != nil || !slices.Contains(sandbox.Awaiting, run.Status) {
		return runanswer.Waiting{}, nil
	}
	now := s.clock()
	_, escalated, overdue := runanswer.Delegation(run, s.currentDelegation().Limits().Deadline, now)
	w := runanswer.Waiting{PutTo: slices.Clone(run.AudienceHandles), PutToYou: putTo(run, record, s.delegating),
		Escalated: escalated}
	if s.delegating(run) {
		w.PutTo, w.Overdue = slices.Clone(run.HeldCall.Delegation.Delegates), overdue
	}
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return w, nil
	}
	var company *config.Company
	if s.Company != nil {
		company, _ = s.Company()
	}
	d, err := runanswer.May(ctx, principal, run, company, s.Chart, now)
	switch {
	case errors.Is(err, runanswer.ErrUnreadableSeat):
		// A ROW NAMING NO SEAT this build can read is answered by nobody:
		// the answer's own route refuses it the same way.
		log.WarnContext(ctx, "run_answer_unreadable_seat", "turn_id", run.TurnID, "error", err.Error())
		return w, nil
	case err != nil:
		return runanswer.Waiting{}, err
	case d.Unknown():
		return runanswer.Waiting{}, fmt.Errorf("%w: this node cannot decide yet whether the caller may "+
			"answer %s's run: %w", ErrUnavailable, run.AgentHandle, d.Err)
	}
	w.MayAnswer = d.Allowed
	return w, nil
}

// delegating reports whether a run's held call still waits on its delegates
// alone (ADR-0125): put to delegates, not escalated, and still delegated by
// the running document to those seats. TURNING DELEGATION OFF puts the call
// in front of people on the next read, before the sweep records why.
func (s Sources) delegating(run sandbox.PendingRun) bool {
	held := run.HeldCall
	if held == nil || !held.Delegated() || s.Company == nil {
		return false
	}
	company, _ := s.Company()
	if company == nil {
		return false
	}
	rule, _, ok := company.DelegatedDecisions.RuleFor(delegated.Kind(held.Act))
	if !ok {
		return false
	}
	for _, d := range held.Delegation.Delegates {
		if !slices.Contains(rule.Delegates, d) {
			return false
		}
	}
	return true
}

// audienceOf resolves the name `audience=` filters by to the record it names —
// see [Sources.executorRows]. An EMPTY record with no error is a login nobody
// holds a seat by, which no run's question can be put to.
func (s Sources) audienceOf(ctx context.Context, asked string) (string, error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return "", unresolved(ctx, "executors")
	}
	owner, _, err := iam.OwnerOf(ctx, principal, asked, s.Holders,
		s.mayLook(principal, authz.ActionPersonRead))
	var looked *iam.LookRefused
	switch {
	case errors.As(err, &looked):
		return "", looked.Err
	case errors.Is(err, iam.ErrNoHolder), errors.Is(err, iam.ErrHolderUnseated):
		// A LOGIN THAT NAMES NO SEAT is put nothing: a run's question is
		// put to seats, and a person holding none has none put to them.
		// Answered as an empty board rather than a refusal, because the
		// caller was let look and the directory's answer is the fact.
		return "", nil
	case err != nil:
		// THE DIRECTORY'S OWN WORDS GO TO THE LOG: they name the seat a
		// login is bound to, and this error is what a caller is answered.
		log.WarnContext(ctx, "queries_audience_undecidable", "name", asked,
			"error", err.Error())
		return "", fmt.Errorf("%w: this node cannot say whose record %q is yet",
			ErrUnavailable, asked)
	case owner == "":
		return "", errNoRecord
	}
	return owner, nil
}

// runShown is who one key of an executor's row is shown to.
type runShown uint8

const (
	// runStatus is everybody's: what the run is, where, in what state, at
	// what cost — the overview.
	runStatus runShown = iota + 1
	// runBody is the run's seat's trail: what it was asked, what its
	// agent called and was answered. Shown to an admin and the seat's lead
	// ([livestate.Reach.Body]).
	runBody
	// runQuestion is the body, and also shown to the people the question
	// is put to — asked it whatever their reach to the seat that asked, and
	// unable to answer what they were not shown.
	runQuestion
	// runTracker is the tracker's: a work item, shown to whoever holds it.
	runTracker
	// runConfig is the company's configuration: which `providers.llm` entry
	// the job was launched on, shown to an admin alone
	// ([livestate.Reach.Config]) — what the live panel's copy of a run
	// withholds too, and the spend breakdown's entries.
	runConfig
)

// runKeys is THE SPLIT of an executor's row, declared once: every key
// [runRow] writes, classified. [serialiseRun] withholds by it, and a walk
// (TestEveryExecutorRowKeyIsClassified) fails the build on a key nobody
// classified — which is what keeps a field added to the row from reaching
// everybody's overview because it was added, rather than because somebody
// decided it is status. It is the same split livestate's `ExecutorEntry.For`
// makes of the live panel's copy of a run (ADR-0033): what a run was asked
// (`task_description`), the question it parked on, who is waiting on it
// (`reply`) and the bridge's call log — every argument and output its agent
// passed through the seat's tools — are the seat's body, and the entry the
// job was launched on is the company's configuration, which a seat's lead
// reads its trail without.
var runKeys = map[string]runShown{
	"turn_id":             runStatus,
	"work_key":            runStatus, // a delivery identity (internal/workkey), not a task key
	"work_item":           runTracker,
	"agent_handle":        runStatus,
	"launch_id":           runStatus,
	"role":                runStatus,
	"status":              runStatus,
	"harness":             runStatus,
	"provider_key":        runConfig, // which entry's plan the job spends: the company's configuration
	"model":               runStatus, // the model it runs under: what it is doing
	"placement":           runStatus,
	"task_description":    runBody,
	"question":            runQuestion,
	"question_context":    runQuestion,
	"decision":            runQuestion,
	"held_call":           runQuestion, // the tool and server a held call names, never its arguments
	"held_arguments":      runBody,     // what the held call would send: the seat's trail
	"audience":            runStatus,
	"audience_handles":    runStatus,
	"audience_fallback":   runStatus,
	"branch":              runStatus,
	"trace_id":            runStatus,
	"owner":               runStatus,
	"box_exists":          runStatus,
	"paused_at":           runStatus,
	"pause_ttl_seconds":   runStatus,
	"started_at":          runStatus,
	"updated_at":          runStatus,
	"answerable_in_chat":  runStatus,
	"declined_answers":    runStatus, // delivery ids, never what the answers said
	"answer_ids":          runStatus, // the recorded answer's delivery ids, not its text
	"answered_by":         runStatus,
	"answered_at":         runStatus,
	"answer_refused":      runStatus, // a refused reply's figures and message count, never its words
	"ending":              runStatus, // that the run is over and when, never what its turn did
	"reply":               runBody,
	"bridge_calls":        runBody,
	"bridge_calls_elided": runStatus, // a count of what was cut, never a call
	"bridge_elided_calls": runBody,   // what was cut, by tool and argument digest: the seat's trail
	"bridge_calls_told":   runStatus, // places in the log and job ids, never a call
	"session_id":          runStatus,
	"command_id":          runStatus,
	"delegation_chain":    runStatus,
	"body_withheld":       runStatus,
	"put_to":              runStatus, // who it waits on now, by handle: [runanswer.Waiting]
	"put_to_you":          runStatus, // whether [putTo] puts it to the reader
	"may_answer":          runStatus, // whether the reader may answer it: [runanswer.May]
	"needs_you":           runStatus, // whether it waits on the reader: [runanswer.Waiting.NeedsYou]
}

// serialiseRun is one executor's row as reader r receives it: [runRow], with
// every key [runKeys] does not show them left out (absent rather than empty,
// because an empty task is a value a board would draw) and `body_withheld`
// saying whether the body was. A KEY THE TABLE DOES NOT NAME IS WITHHELD:
// left out is the direction a mistake has to fail in.
//
// calls is the run's log as [PendingRuns.BridgeLog] read it, the zero value
// for a reader shown no body. The row also states where the run stands for
// the reader ([Sources.waiting]), so no screen decides "needs you" itself.
func serialiseRun(run sandbox.PendingRun, calls sandbox.BridgeLog, r livestate.Reach,
	opened runOpened, waiting runanswer.Waiting) map[string]any {

	row := runRow(run, calls, opened)
	asked := r.Record != "" && slices.Contains(run.AudienceHandles, r.Record)
	for key := range row {
		switch runKeys[key] {
		case runStatus:
		case runBody:
			if !r.Body {
				delete(row, key)
			}
		case runQuestion:
			if !r.Body && !asked {
				delete(row, key)
			}
		case runTracker:
			if !r.Tracker {
				delete(row, key)
			}
		case runConfig:
			if !r.Config {
				delete(row, key)
			}
		default:
			delete(row, key)
		}
	}
	row["body_withheld"] = !r.Body
	row["put_to"] = nonNil(waiting.PutTo)
	row["put_to_you"] = waiting.PutToYou
	row["may_answer"] = waiting.MayAnswer
	row["needs_you"] = waiting.NeedsYou()
	return row
}

// runOpened is what a row reads out of a run's seals ([Sources.opened]): who
// gave its recorded answer, and the arguments of a call it holds for a
// person's approval, as its reader is shown them.
type runOpened struct {
	answeredBy string
	held       map[string]any
}

// opened is what run's seals open to on this node for a reader who may, or may
// not, answer it ([runanswer.May]): empty where a seal does not open here, as
// each reader of one answers it.
func (s Sources) opened(run sandbox.PendingRun, mayAnswer bool) runOpened {
	return runOpened{answeredBy: s.answeredBy(run), held: s.heldArguments(run, mayAnswer)}
}

// heldArguments is the arguments of the call run holds for a person's
// approval, opened from its seal by [sandbox.CallSeal.OpenArgumentsFor]'s
// rule; nil where the run holds no call or its seal does not open here.
func (s Sources) heldArguments(run sandbox.PendingRun, mayAnswer bool) map[string]any {
	if run.HeldCall == nil || s.HeldCalls == nil {
		return nil
	}
	args, _, err := s.HeldCalls.OpenArgumentsFor(run, mayAnswer)
	if err != nil {
		log.Warn("held_call_arguments_unopened", "turn_id", run.TurnID, "held_call", run.HeldCall.ID,
			"error", err.Error())
		return nil
	}
	return args
}

// runRow is every fact an executor's row carries, with the calls of the log
// kept beside it, whoever reads it — see [serialiseRun] for what each reader is
// shown of it. opened is what its seals open to ([Sources.opened]).
func runRow(run sandbox.PendingRun, calls sandbox.BridgeLog, opened runOpened) map[string]any {
	answeredBy := opened.answeredBy
	// HELD, NOT STAMPED. A box parked on a question holds its node's disk
	// and memory whether or not the pause instant reached the row, and
	// drawing the raw stamp showed exactly that box as a live one — the one
	// reading that says nothing is held. It is the same answer the pause reaper
	// acts on ([sandbox.PendingRun.HeldSince]), so the screen and the
	// reclaim cannot disagree about which boxes are held.
	heldSince, _ := run.HeldSince()
	// A BOX ITS ENDING RECLAIMS IS NO LONGER THE RUN'S, whether or not the
	// reclaim has reached it yet: drawn as up and held, an ended turn read as
	// a paused, held box beside a run still resuming, for as long as the
	// ending waited on a capture's keep or on the executor log.
	reclaimed := run.Ending != nil && run.Ending.Reclaim
	if reclaimed {
		heldSince = time.Time{}
	}
	return withHeld(run, opened, withDecision(run, map[string]any{
		"turn_id": run.TurnID,
		// The unit of work behind that run, so a board row links back to
		// the trigger rather than only to the one execution that detached.
		// Empty when the run's turn had no ledgerable trigger.
		"work_key": run.WorkKey,
		// THE ITEM THE LAUNCHING TURN WAS CHARGED TO, which the row has
		// carried since runs named one and this answer never served — so a
		// parked run's question reached a person with no task beside it.
		// Null on a run launched by a turn charged to no item.
		"work_item":    run.WorkItem,
		"agent_handle": run.AgentHandle,
		// THE JOB THE ROW HOLDS NOW, which is what `executor_tail` is asked
		// by: a turn can launch more than one, and the run's own page polls
		// the live output of the job it is showing rather than of whichever
		// replaced it.
		"launch_id": run.LaunchID,
		"role":      run.Role,
		"status":    run.Status,
		"harness":   run.Harness,
		// THE EXECUTOR-CHAIN MEMBER THE JOB RUNS ON, and its model: a
		// fallback at launch makes it other than the chain's head, and
		// which entry's plan a job is spending is the first thing an admin
		// watching a long run asks. The entry is theirs alone ([runConfig]).
		"provider_key": run.Launch.ProviderKey,
		"model":        run.Launch.Model,
		// WHERE the run is, an operator question once providers.sandbox
		// serves two cells: one company runs some seats as a process tree
		// on their node's host and others in a container there, and no
		// other surface says which isolation a job has. Empty is the
		// provider default.
		"placement":        run.Placement,
		"task_description": run.TaskDescription,
		// THE QUESTION AS IT WAS ASKED (ADR-0068): one sentence and its
		// Markdown context, always sent, the context empty where it gave
		// none, as every reader of a run's ask sends them. The choice it
		// offers is added below where it has one ([withDecision]).
		"question":         run.Question,
		"question_context": run.QuestionContext,
		"audience":         run.Audience,
		// WHO THE QUESTION IS PUT TO, resolved against the chart when the
		// run parked, and whether that is a fallback — the seat's lead
		// chain — because the audience above named nobody the chart has.
		// Always an array, empty on a run that is not parked on a
		// question.
		"audience_handles":  nonNil(run.AudienceHandles),
		"audience_fallback": run.AudienceFallback,
		"branch":            run.Branch,
		"trace_id":          run.TraceID,
		"owner":             run.Owner,
		// The two facts the board draws, rather than the ids themselves: a
		// non-empty sandbox id means a box exists, and a set paused_at
		// means it is currently paused, holding its node's disk and
		// memory — neither, once an ending reclaims the box.
		"box_exists":        run.SandboxID != "" && !reclaimed,
		"paused_at":         isoOrEmpty(heldSince),
		"pause_ttl_seconds": run.PauseTTLSeconds,
		"started_at":        isoOrEmpty(run.CreatedAt),
		"updated_at":        isoOrEmpty(run.UpdatedAt),
		// NEVER A HELD CALL, which its approval answers alone.
		"answerable_in_chat": answerableInChat(run.ConversationKey) && run.HeldCall == nil,
		// THE ANSWERS THE QUESTION LET GO OF, by the id of the delivery each
		// arrived in (answer_executor answers it as `answer_id`): a person who
		// answered is told from here that the run could not take their
		// answer and is waiting on its question again, rather than left
		// looking at a question that never changes. Always an array.
		"declined_answers": nonNil(run.DeclinedAnswers),
		// THE ANSWER RECORDED ON THE RUN, by its deliveries, who gave it and
		// when: the first recorded answer wins, and a person whose answer
		// was not that one is told whose was, rather than shown their own
		// as taken. Empty on a run with no recorded answer; who gave it,
		// also where its seal does not open on this node.
		"answer_ids":  answerIDs(run.Answer),
		"answered_by": answeredBy,
		"answered_at": answeredAt(run.Answer),
		// AND WHETHER THAT ANSWER IS A REFUSAL (ADR-0114): a reply too large
		// for the run to take, recorded as the question's outcome in the
		// answer's place, with what it came to and what an answer may and
		// how many messages it was — none of its words, which no record of
		// the run keeps. The ids, who and when above are that reply's. The
		// run is resumed told the reply did not reach it, so a reader tells
		// a reply refused from an answer the run carries on with by this
		// alone. Null on every other row.
		"answer_refused": answerRefused(run),
		// THAT THE RUN IS OVER, though its record is not gone yet: its
		// ending is decided ([sandbox.RecordedEnding]) and finishing — on a
		// node that records every turn's end, waiting for the turn's
		// captures to settle and the executor log to take the end, which
		// can be minutes — while its status still reads as the run it was.
		// Null on a run whose ending nobody has decided.
		"ending": endingOf(run),
		// WHO IS WAITING, which is the question a board full of parked
		// runs exists to answer and had no field for. Persisted rather
		// than re-derived precisely because the resumed turn does not see
		// its trigger — empty means nobody is waiting, which is the safe
		// half.
		"reply": run.Reply,
		// THE BRIDGE'S OWN TOOL LOG. Every run's calls are made by a
		// process outside the engine, over its bridge, minutes or hours
		// apart and possibly across a restart, so the log beside the row is
		// the only copy there is. `bridge_calls_elided` says how many were cut from the
		// middle, because a log that silently skips is a log that lies
		// about what the run did.
		// ALWAYS AN ARRAY on a row the store answered: every run is
		// bridged, so an empty log is a run that has called nothing — a
		// fact the run's page states — and never "not known", which a
		// reader is told only by the key being absent (a withheld body).
		"bridge_calls":        nonNil(shownCalls(calls.Calls)),
		"bridge_calls_elided": calls.Elided,
		// AND WHAT THOSE WERE, by tool and argument digest with how many
		// calls had each and how many failed: the count bound costs the board
		// the calls themselves, never which tools the run called how often.
		// Always an array beside `bridge_calls`, empty where nothing was cut.
		// READ AS THE LOG ACCOUNTS FOR THEM ([sandbox.BridgeLog.Unlisted]):
		// calls a build without identities cut are one entry naming no tool.
		"bridge_elided_calls": nonNil(calls.Unlisted()),
		// AND WHICH LATE CALLS THE JOB WORKING AS EACH LANDED TOOK INTO ITS
		// OWN SESSION ([toldCalls]): no continuation of that session is told
		// them again. Always an array, empty where the run's sessions took none.
		"bridge_calls_told": toldCalls(calls, run.Steer.Tellings),
		// THE THREE IDENTIFIERS a person needs to find this run in
		// somebody else's system: the harness's own session, the
		// command the engine launched, and the chain of asks that led
		// here.
		"session_id":       run.SessionRan(),
		"command_id":       run.CommandID,
		"delegation_chain": run.DelegationChain,
		// WHETHER [runKeys]' body was left out for this reader, so a board
		// draws "not shown to you" over a run's task rather than a run that
		// was asked nothing. Set by [serialiseRun].
		"body_withheld": false,
		// WHERE IT STANDS FOR THE READER ([runanswer.Waiting]), set by
		// [serialiseRun] for the reader it serialises the row for.
		"put_to":     []string{},
		"put_to_you": false,
		"may_answer": false,
		"needs_you":  false,
	}))
}

// toldCall is one late call of a run's log a job's own session is known to
// hold ([sandbox.RunSteer.Tellings]): the call's place in the log
// ([sandbox.BridgeCall.Appended]) and the job whose session took it, never a
// word of the call.
type toldCall struct {
	Appended int    `json:"appended"`
	Launch   string `json:"launch"`
}

// toldCalls is the tellings of calls' round among tellings
// ([sandbox.BridgeLog.Told]): a telling names a place in its own round's log
// alone, so one of another round would tag a call it never named.
func toldCalls(calls sandbox.BridgeLog, tellings []sandbox.CallTelling) []toldCall {
	out := []toldCall{}
	for _, t := range calls.Told(tellings) {
		out = append(out, toldCall{Appended: t.Appended, Launch: t.Launch})
	}
	return out
}

// withDecision adds the choice a run's question offers to its row, and leaves
// the key ABSENT on an open question, as every reader of a run's ask does:
// absent is "no choice", never a null a client has to tell from one.
func withDecision(run sandbox.PendingRun, row map[string]any) map[string]any {
	if run.Decision != nil {
		row["decision"] = run.Decision
	}
	return row
}

// withHeld adds the call a run holds for a person's approval to its row
// (ADR-0077): `held_call`, the call its question is about, which the audience
// reads with the question, and `held_arguments`, what the call would send,
// opened from their seal (null where it does not open here), which only a
// reader of the seat's trail reads: an administrator or the seat's lead. Both
// ABSENT on a run that holds none. The arguments are shown as sent only to who
// may answer the call while it waits ([sandbox.CallSeal.OpenArgumentsFor]).
func withHeld(run sandbox.PendingRun, opened runOpened, row map[string]any) map[string]any {
	if held := run.HeldCall; held != nil {
		row["held_call"] = map[string]any{
			"id": held.ID, "server": held.Server, "tool": held.Tool, "raw": held.Raw,
			"digest": held.Digest, "held_at": isoOrEmpty(held.HeldAt),
			// A CALL ITS TOOL ASKS ABOUT EVERY TIME, answered once or deny
			// alone, and who answers it where not the seat's lead.
			"fixed": held.Fixed, "approvers": held.Approvers,
			// WHAT IT IS FOR, and the delegates it went to first: their
			// answers and why it reached people, which an escalation's
			// card shows before the call itself (ADR-0125).
			"act": held.Act, "delegation": delegationOf(held.Delegation),
			// WHEN A GROWTH ACT LAPSES unanswered, "" for a call that waits
			// until answered (ADR-0171).
			"lapses_at": isoOrEmpty(held.LapsesAt),
		}
		row["held_arguments"] = opened.held
	}
	return row
}

// delegationOf is a held call's delegation as the board carries it, or an
// untyped nil, sent as null, for a call put to people at once.
func delegationOf(d *sandbox.HeldDelegation) any {
	if d == nil {
		return nil
	}
	given := make([]map[string]any, 0, len(d.Given))
	for _, g := range d.Given {
		given = append(given, map[string]any{"by": g.By, "verdict": g.Verdict, "confidence": g.Confidence,
			"reason": g.Reason, "findings": nonNilStrings(g.Findings), "at": isoOrEmpty(g.At)})
	}
	out := map[string]any{"rule": d.Rule, "delegates": nonNilStrings(d.Delegates), "given": given,
		"escalated": nil}
	if e := d.Escalated; e != nil {
		out["escalated"] = map[string]any{"overruled": nonNilStrings(e.Overruled),
			"engine": nonNilStrings(e.Engine), "at": isoOrEmpty(e.At)}
	}
	return out
}

// nonNilStrings is xs, or an empty list where it is nil, so a board reads a
// list it can map over.
func nonNilStrings(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// endingOf is a run's decided ending as the board carries it — when it was
// decided, the reason it announces ("" for a run whose turn came back, which
// announces nothing) and who stopped it, for a person's stop — or an untyped
// nil, sent as null, for a run whose ending nobody has decided.
func endingOf(run sandbox.PendingRun) any {
	if run.Ending == nil {
		return nil
	}
	stoppedBy := ""
	if run.Stop != nil {
		stoppedBy = run.Stop.By
	}
	return map[string]any{
		"at": isoOrEmpty(run.Ending.At), "reason": run.Ending.Reason, "stopped_by": stoppedBy,
	}
}

// answerIDs and answeredAt are the recorded answer's deliveries and instant,
// empty for a run with none.
func answerIDs(answer *sandbox.RecordedAnswer) []string {
	if answer == nil {
		return []string{}
	}
	return nonNil(answer.EventIDs)
}

// answeredBy is who gave run's recorded answer, which only the answer's seal
// says: empty for a run with none, and where the seal does not open on this
// node ([sandbox.OpenedAnswerer]). No copy of it is kept in the clear for a
// row to read instead.
func (s Sources) answeredBy(run sandbox.PendingRun) string {
	return sandbox.OpenedAnswerer(s.RunAnswers, run)
}

// answerRefused is the refusal run's recorded answer is, as a board reads it —
// how many messages the reply was and its two figures — or nil where the
// answer is none ([sandbox.PendingRun.Refused]): an untyped nil, so a reader of
// the row finds no value at all rather than an empty object.
func answerRefused(run sandbox.PendingRun) any {
	refusal, refused := run.Refused()
	if !refused {
		return nil
	}
	return map[string]any{
		"messages":  refusal.Messages(),
		"bytes":     refusal.Bytes,
		"max_bytes": refusal.MaxBytes,
	}
}

func answeredAt(answer *sandbox.RecordedAnswer) string {
	if answer == nil {
		return ""
	}
	return isoOrEmpty(answer.RecordedAt)
}

// answerableInChat reports whether a reply on a chat surface could ever reach
// this run.
//
// A task assignment's or A2A wake's run stored NOTHING: no key names a
// conversation, so [notify.ConversationIdentityOfAll] answers "". A
// schedule's run stored the SCHEDULE'S conversation
// ([notify.ScheduleConversation]), which nobody writes into. Neither is
// answerable, and "reply in the thread" would name a thread that does not
// exist. It asks of the durable conversation, never the partition;
// [notify.Derived] also refuses the `event:` fallback [notify.KeyOf] mints at
// READ time, so a row a peer keyed that way stays honest.
func answerableInChat(conversation string) bool {
	return notify.Derived(conversation)
}

// nonNil is a list that encodes as `[]` rather than `null` when empty, so a
// reader indexes it without a presence check.
func nonNil[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}
