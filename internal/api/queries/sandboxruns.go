package queries

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The sandbox-runs answer: the detached coding runs the engine still holds.
//
// THE LIVE PROJECTION IS THE WRONG SOURCE for this question, in two ways. It
// is in memory, so it starts empty after a restart; and it sweeps an entry
// after hours, while a run parked on a question can legitimately wait days for
// a person to answer. The states that most need somebody were therefore the
// ones least likely to be on screen — and a `reseed` run (its pause expired,
// its box reclaimed, its work safe on a pushed branch) had no surface at all.
// It looked exactly like work that had finished.
//
// The fleet's run record is the durable one, and this reads it — every node's
// runs, not this node's, because a run is recovered by whichever node owns its
// seat and a per-node read drew a board that disagreed with itself depending on
// which node answered.
//
// What it deliberately does NOT return is the execute state, the serialised
// Execute conversation. It is by far the largest column in the row, and every
// prompt in it is already reachable through the event store — shipping it to a
// board that renders one line per run would be a page-weight problem in
// exchange for nothing.

// PendingRuns is the durable record a sandbox-runs answer reads.
//
// Declared here rather than imported as a concrete store so this package
// depends on the shape, and so the memory twin answers it too.
//
// ONE METHOD AND NO STATUS FILTER, because there is nothing left to filter:
// a run's record is deleted the moment the run settles, so [sandbox.Active] is
// every status a record can hold and "every run this store holds" and "the
// active ones" name one set. A `status=` parameter selecting `done` or
// `failed` would be a question whose answer is structurally empty — worse than
// absent, because an empty board reads as "nothing failed" rather than "this
// is not where that is recorded". How a run ENDED is on the event stream: the
// resumed turn's own events, a `sandbox_run_failed` naming the reason, or a
// `sandbox_run_stopped` naming the person who stopped it.
//
// AND ONE RUN BY ITS TURN, which `sandbox_tail` reads to learn whose seat a
// run is before it shows anybody that seat's transcript — the record is the
// only thing that names it.
type PendingRuns interface {
	ListActive(ctx context.Context) ([]sandbox.PendingRun, error)
	Get(ctx context.Context, turnID string) (sandbox.PendingRun, bool, error)
}

// SandboxTails answers a running coding run's live output from the node that
// owns it — the one method this surface calls of [sandbox.TailReader].
type SandboxTails interface {
	Tail(ctx context.Context, q sandbox.TailQuery) (sandbox.TailAnswer, error)
}

// sandboxTail answers `sandbox_tail{turn_id, launch_id}`: the tail of that
// launch while it runs, `launching` while its box is being made, `not_running`
// with the record's own status once it is not, `box_paused` for a running
// record whose box is paused, or the owning node NAMED where it did not answer
// (`owner_silent`).
//
// BOTH IDS ARE REQUIRED. A run is one execution of a turn and a turn can launch
// more than one job; a request naming only the turn would show whichever job
// its row holds now, which is a different job from the span a person clicked
// the moment a second launch replaces the first.
//
// BY CURSOR: `epoch`, `after` and `digest` say what the asker holds — all
// three absent for one holding nothing yet — and the answer carries what it
// lacks, or a reset. A cursor's offset is a whole number of bytes, refused by
// name otherwise.
//
// DECIDED ON THE RUN'S SEAT, as its trail: what a coding agent prints is its
// transcript, so it is read by whoever reads the seat's trail — its lead, or
// an admin — and never by everybody the run's status reaches. See
// [Sources.mayTail].
func (s Sources) sandboxTail(ctx context.Context, p Params) (any, error) {
	turnID := strings.TrimSpace(p.String("turn_id"))
	launchID := strings.TrimSpace(p.String("launch_id"))
	if turnID == "" || launchID == "" {
		return nil, fmt.Errorf("%w: sandbox_tail needs a turn_id and the launch_id of the "+
			"run's job", ErrBadParams)
	}
	var after int64
	if p.Has("after") {
		var whole bool
		if after, whole = p.WholeInt("after"); !whole || after < 0 {
			return nil, fmt.Errorf("%w: sandbox_tail's after is the byte offset the asker "+
				"holds through, a whole number of zero or more", ErrBadParams)
		}
	}
	gone, err := s.mayTail(ctx, turnID)
	if err != nil {
		return nil, err
	}
	if gone {
		// NO RECORD NAMES THE RUN, so nothing says whose it is — and
		// nothing is left to show: a settled run's record is deleted, and
		// a launch with no record is `not_running` with no status, which
		// is exactly what the tail reader answers for one. Answered here
		// rather than by asking it, because its own read of the record is
		// a moment later than this one and could find a record this
		// decision never saw.
		return sandbox.TailAnswer{Outcome: sandbox.TailNotRunning,
			TurnID: turnID, LaunchID: launchID}, nil
	}
	return s.SandboxTail.Tail(ctx, sandbox.TailQuery{
		TurnID: turnID, LaunchID: launchID,
		Cursor: sandbox.TailCursor{Epoch: p.String("epoch"), Offset: after, Digest: p.String("digest")},
	})
}

// mayTail decides whether the caller may read the live output of the run of
// turnID, and reports a run no record names as gone.
//
// # The company-wide trail first, then the run's seat
//
// Whoever reads the COMPANY-WIDE trail ([authz.ActionAuditRead]) reads every
// run's, and is decided before anything is read: the table answers that
// without a record, and the tail reader's own read of the record is the one
// that counts for them. Anybody else is decided on the run's SEAT
// ([authz.ActionSeatTrailRead] — its lead), which only the run's record names,
// so it is read here: a seat a turn's run belongs to never changes, which is
// what makes deciding on this read and tailing on the reader's a moment later
// the same decision.
//
// A NODE WITH NO RUN RECORD TO READ decides nothing but the company-wide
// trail, and refuses with that decision: it can establish no seat a caller
// might lead.
//
// # A caller who leads nobody is refused before the record is read
//
// The seat verb is decided FIRST on a run nobody has resolved to a seat yet
// ([authz.Object.Unresolved], under [unresolvedRunSeat]), as a steer is: the
// only admission left past the company-wide trail is leading whoever the run
// turns out to belong to, so a caller who leads nobody — an unbound service
// account or token, a person with no reports — is refused there, and only a
// caller the table tells to resolve ([authz.ErrUnresolved]) has the record
// read for them. Read first, a run store that could not answer told every
// such caller to come back to a refusal no answer of it could change.
func (s Sources) mayTail(ctx context.Context, turnID string) (gone bool, err error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return false, unresolved(ctx, "sandbox_tail")
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
		return false, refused("sandbox_tail", company)
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
		return false, refused("sandbox_tail", first)
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

// sandboxRuns answers the board, or — with `audience=<name>` — the runs whose
// question is put to that one person and still waits for them ([putTo]).
//
// THE NAME IS A RECORD'S, resolved as every personal read here resolves one
// ([iam.OwnerOf]): the caller's own with no name or one of theirs, a person's
// LOGIN to the seat the identity directory binds them to — never read as a
// seat's handle — and anything else as the seat it names. The directory is
// asked only once [authz.ActionPersonRead] admits looking — the caller's own,
// their lead's or an admin's, each with the tracker, since whose queue
// somebody is on is the tracker's question — so a caller who leads nobody
// filters by a seat's handle and cannot read the directory off this filter. NO SCOPE RULE beyond that, unlike the tracker's personal
// reads: the unfiltered board already carries every run's audience to anybody
// who may read it, so a narrower answer reveals nothing the wider one did not.
//
// A run whose question resolved to nobody carries no audience, and is
// therefore nobody's by this filter — which is the truth about it: nobody was
// recorded as the one its question was put to.
//
// EVERY RUN'S STATUS IS EVERYBODY'S, and what it was asked, asked back and
// answered is its seat's trail — see [runKeys] for the split, and
// [Sources.runReach] for who reads which.
func (s Sources) sandboxRuns(ctx context.Context, p Params) (any, error) {
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
		if filtered && !putTo(run, who) {
			continue
		}
		out = append(out, serialiseRun(run, reach(run), s.answeredBy(run)))
	}
	return map[string]any{"runs": out}, nil
}

// runReach is what the asking principal may read of each coding run: its
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
// STILL WAITING, because "what is waiting on me" is the question this answers:
// a run whose question already has its answer recorded ([sandbox.StatusAnswered])
// or is being resumed with it keeps the audience it was asked of, and listing
// it would put a question the person already answered back in front of them.
// The home screen's decisions ask the same two things, through this.
//
// ONE RECORD, never a set of identities: a run's audience is recorded as seat
// handles, and the name a caller asked about is resolved to the one record it
// names before anything is compared ([Sources.audienceOf], [iam.OwnerOf]). An
// empty record — a login nobody holds a seat by — is put nothing.
func putTo(run sandbox.PendingRun, record string) bool {
	return record != "" && slices.Contains(sandbox.Awaiting, run.Status) &&
		slices.Contains(run.AudienceHandles, record)
}

// audienceOf resolves the name `audience=` filters by to the record it names —
// see [Sources.sandboxRuns]. An EMPTY record with no error is a login nobody
// holds a seat by, which no run's question can be put to.
func (s Sources) audienceOf(ctx context.Context, asked string) (string, error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return "", unresolved(ctx, "sandbox_runs")
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

// runShown is who one key of a coding run's row is shown to.
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

// runKeys is THE SPLIT of a coding run's row, declared once: every key
// [runRow] writes, classified. [serialiseRun] withholds by it, and a walk
// (TestEverySandboxRunKeyIsClassified) fails the build on a key nobody
// classified — which is what keeps a field added to the row from reaching
// everybody's overview because it was added, rather than because somebody
// decided it is status. It is the same split livestate's `SandboxEntry.For`
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
	"coding_agent":        runStatus,
	"provider_key":        runConfig, // which entry's plan the job spends: the company's configuration
	"model":               runStatus, // the model it runs under: what it is doing
	"placement":           runStatus,
	"task_description":    runBody,
	"question":            runQuestion,
	"question_context":    runQuestion,
	"decision":            runQuestion,
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
	"reply":               runBody,
	"bridge_calls":        runBody,
	"bridge_calls_elided": runStatus, // a count of what was cut, never a call
	"session_id":          runStatus,
	"command_id":          runStatus,
	"delegation_chain":    runStatus,
	"body_withheld":       runStatus,
}

// serialiseRun is one coding run's row as reader r receives it: [runRow],
// with every key [runKeys] does not show them left out — absent rather than
// empty, because an empty task is a value a board would draw — and
// `body_withheld` saying whether the body was.
//
// A KEY THE TABLE DOES NOT NAME IS WITHHELD, which the walk keeps unreachable:
// left out is the direction a mistake here has to fail in.
func serialiseRun(run sandbox.PendingRun, r livestate.Reach, answeredBy string) map[string]any {
	row := runRow(run, answeredBy)
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
	return row
}

// runRow is every fact a coding run's row carries, whoever reads it — see
// [serialiseRun] for what each reader is shown of it. answeredBy is who gave
// the run's recorded answer, as its seal opens ([Sources.answeredBy]).
func runRow(run sandbox.PendingRun, answeredBy string) map[string]any {
	// HELD, NOT STAMPED. A box parked on a question is being paid for
	// whether or not the pause instant reached the row, and drawing the raw
	// stamp showed exactly that box as a live one — the one reading that
	// says nobody is being billed. It is the same answer the pause reaper
	// acts on ([sandbox.PendingRun.HeldSince]), so the screen and the
	// reclaim cannot disagree about which boxes are held.
	heldSince, _ := run.HeldSince()
	return map[string]any{
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
		// THE JOB THE ROW HOLDS NOW, which is what `sandbox_tail` is asked
		// by: a turn can launch more than one, and the run's own page polls
		// the live output of the job it is showing rather than of whichever
		// replaced it.
		"launch_id":    run.LaunchID,
		"role":         run.Role,
		"status":       run.Status,
		"coding_agent": run.CodingAgent,
		// THE EXECUTOR-CHAIN MEMBER THE JOB RUNS ON, and its model: a
		// fallback at launch makes it other than the chain's head, and
		// which entry's plan a job is spending is the first thing an admin
		// watching a long run asks. The entry is theirs alone ([runConfig]).
		"provider_key": run.Launch.ProviderKey,
		"model":        run.Launch.Model,
		// WHERE the run is, which became an operator question the moment
		// providers.sandbox became a catalogue: one company now runs some
		// seats on the engine host and others in a remote box, and "is
		// this job on my machine" has no other surface. Empty is the
		// provider default.
		"placement":        run.Placement,
		"task_description": run.TaskDescription,
		// THE QUESTION AS IT WAS ASKED (ADR-0068): one sentence, its
		// Markdown context, and the choice it offers, which a person
		// answers with one press. The context is empty and the decision
		// null on an open question with nothing to add, never absent: a
		// reader shown the question is shown all of it.
		"question":         run.Question,
		"question_context": run.QuestionContext,
		"decision":         run.Decision,
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
		// means it is currently held as a snapshot and being paid for.
		"box_exists":         run.SandboxID != "",
		"paused_at":          isoOrEmpty(heldSince),
		"pause_ttl_seconds":  run.PauseTTLSeconds,
		"started_at":         isoOrEmpty(run.CreatedAt),
		"updated_at":         isoOrEmpty(run.UpdatedAt),
		"answerable_in_chat": answerableInChat(run.ConversationKey),
		// THE ANSWERS THE QUESTION LET GO OF, by the id of the delivery each
		// arrived in (answer_run answers it as `answer_id`): a person who
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
		// WHO IS WAITING, which is the question a board full of parked
		// runs exists to answer and had no field for. Persisted rather
		// than re-derived precisely because the resumed turn does not see
		// its trigger — empty means nobody is waiting, which is the safe
		// half.
		"reply": run.Reply,
		// THE BRIDGE'S OWN TOOL LOG. Every run's calls are made by a
		// process outside the engine, over its bridge, minutes or hours
		// apart and possibly across a restart, so this row is the only
		// copy there is. `bridge_calls_elided` says how many were cut from the
		// middle, because a log that silently skips is a log that lies
		// about what the run did.
		// ALWAYS AN ARRAY on a row the store answered: every run is
		// bridged, so an empty log is a run that has called nothing — a
		// fact the run's page states — and never "not known", which a
		// reader is told only by the key being absent (a withheld body).
		"bridge_calls":        nonNil(run.BridgeCalls),
		"bridge_calls_elided": run.BridgeCallsElided,
		// THE THREE IDENTIFIERS a person needs to find this run in
		// somebody else's system: the coding CLI's own session, the
		// command the engine launched, and the chain of asks that led
		// here.
		"session_id":       run.SessionID,
		"command_id":       run.CommandID,
		"delegation_chain": run.DelegationChain,
		// WHETHER [runKeys]' body was left out for this reader, so a board
		// draws "not shown to you" over a run's task rather than a run that
		// was asked nothing. Set by [serialiseRun].
		"body_withheld": false,
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

func answeredAt(answer *sandbox.RecordedAnswer) string {
	if answer == nil {
		return ""
	}
	return isoOrEmpty(answer.RecordedAt)
}

// answerableInChat reports whether a reply on a chat surface could ever reach
// this run.
//
// A run started by anything OTHER than an external notification — a schedule
// tick, a task assignment, an A2A wake — stored NOTHING: its trigger names
// neither key, [notify.ConversationIdentityOfAll] answers "" for a partition
// like that rather than inventing one, and [notify.Stamp] declines an empty
// value, so both of the row's conversation columns are empty. Such a run is
// not answerable through any chat surface, and telling somebody to "reply in
// the thread" would send them to a thread that does not exist.
//
// The per-event `event:` fallback is a different thing in a different place:
// [notify.KeyOf] mints it at READ time, for the broker's partition function,
// which has to give an event that names no conversation a partition of its
// own. It is not written onto a row from here. [notify.Derived] refuses it as
// well as the empty string, which is what keeps this column honest for a row
// whose key was minted that way by somebody else — a peer, or a writer this
// package does not know about.
//
// ASKED OF THE CONVERSATION the run reports back to and is answered on, never
// of the partition beside it. The question a person reading this board has is
// "is there a thread I can answer in", which is a property of the durable
// conversation; the partition is an inbox-grouping artefact with no surface
// anybody could reply to. The two are derivable or not TOGETHER on every
// source — a chat event naming a channel yields both, one naming none yields
// neither — so the verdict is the same either way; what this states is that
// the column answers the same question the resume path does.
//
// [notify.Derived] answers exactly that, and this had its own copy of the
// prefix to answer it with — so a rename of the fallback's namespace would
// have left this route confidently offering a thread that does not exist.
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
