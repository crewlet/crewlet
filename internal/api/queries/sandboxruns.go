package queries

import (
	"context"
	"fmt"
	"slices"
	"strconv"

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
//
// # A bridged run's calls: both ends, and the middle counted where it falls
//
// Every call a bridged run makes is its own record, and a long run makes
// thousands, each carrying its output — so the whole log in one answer is an
// answer of any size at all. Each row carries, in `bridge_calls` and in the
// order the run made them, the log's FIRST calls and then its NEWEST ones:
// how the run set about the work and how it ended, which for a bridged run is
// its submission. The calls between the two are not in the answer, and the row
// says so where a reader of the list looks — `bridge_calls_elided` counts
// them and `bridge_calls_elided_after` is how many entries of the list come
// before them. `bridge_calls_next` is present when they can be read, as the
// cursor to read them from; `turn_id` narrows the answer to one run, and
// `calls_after` set to that cursor pages through them, `calls_limit` calls or
// fewer at a time. A page is bounded by bytes as well as by count, so it can
// carry fewer calls than asked for — the cursor, not the count, says whether
// there is more. A run an older build recorded has calls that build did not
// keep, which no cursor reaches; its row counts them the same way and carries
// no cursor.
//
// Each call is AS ITS RECORD HOLDS IT. A call too large for one record is its
// record's fitted form, marked in whichever field was cut: an output that did
// not fit ends in "…", and arguments that did not fit are replaced by a
// one-member object keyed "…". One whose whole was filed in parts carries
// `whole_bytes` and `whole_parts`, the length of its whole and how many parts
// hold it under the call's record, which is what the run's resume reads it
// back whole from. One that carries neither and whose whole could not be kept
// says so in the field that was cut: the arguments' object says they were not
// kept, and a cut output ends in a note after its "…". A record an older build
// fitted, before parts, carries neither the reference nor a note: its cut
// output ends in "…" alone, and arguments it set aside are the object saying
// they were not kept. A call that a server set below the ceiling refused is its
// least form: its name and outcome, with each text it had replaced by its
// mark. The answer never carries a part, nor counts one.

// PendingRuns is the durable record a sandbox-runs answer reads.
//
// Declared here rather than imported as a concrete store so this package
// depends on the shape, and so the memory twin answers it too.
//
// NO STATUS FILTER, because there is nothing left to filter: a run's record is
// deleted the moment the run settles, so [sandbox.Active] is every status a
// record can hold and "every run this store holds" and "the active ones" name
// one set. A `status=` parameter selecting `done` or `failed` would be a
// question whose answer is structurally empty — worse than absent, because an
// empty board reads as "nothing failed" rather than "this is not where that is
// recorded". How a run ENDED is on the event stream: the resumed turn's own
// events, or a `sandbox_run_failed` naming the reason.
type PendingRuns interface {
	ListActive(ctx context.Context) ([]sandbox.PendingRun, error)
	BridgeCallPage(ctx context.Context, run sandbox.PendingRun, after uint64, limit int) (sandbox.BridgeCallPage, error)
}

// BridgeCallsPageDefault and BridgeCallsPageMax are the count half of a page of
// a bridged run's calls: what `calls_limit` takes when it is absent, and the
// most it is served.
//
// The COUNT bounds READS — on the coordination backend each call a page
// carries is one read of its record, and a first answer carries two pages'
// worth, the first calls and the newest — while each page's bytes are bounded
// by [sandbox.BridgeCallPageBytes] whatever the count. A HUNDRED is the default
// because a first answer then carries the first and newest hundred calls,
// which is the shape of the run row's own list for older builds — its first
// and newest [sandbox.MaxBridgeCalls]/2. FIVE HUNDRED as the ceiling is a
// judgement about how many record reads one question may cost, not a
// measurement.
const (
	BridgeCallsPageDefault = 100
	BridgeCallsPageMax     = 500
)

func (s Sources) sandboxRuns(ctx context.Context, p Params) (any, error) {
	only := p.String("turn_id")
	// A CURSOR IS REFUSED WHEN IT IS NOT ONE, unlike the filters [Params.Int]
	// forgives: read as zero, it would answer the first page to a reader
	// asking for the one after, and a reader paging on would take the first
	// calls again as the next ones.
	var after uint64
	if p.Has("calls_after") {
		parsed, err := strconv.ParseUint(p.String("calls_after"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: calls_after is a cursor from bridge_calls_next, and %q is not one",
				ErrBadParams, p.String("calls_after"))
		}
		after = parsed
	}
	if after > 0 && only == "" {
		// A cursor belongs to ONE run's log. Applied to every row of the
		// board, it would start each run's page at another run's place.
		return nil, fmt.Errorf("%w: calls_after pages one run's calls, so it needs turn_id",
			ErrBadParams)
	}
	limit := Clamp(p.Int("calls_limit", 0), BridgeCallsPageDefault, BridgeCallsPageMax)

	runs, err := s.Sandbox.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(runs))
	for _, run := range runs {
		if only != "" && run.TurnID != only {
			continue
		}
		page, err := s.Sandbox.BridgeCallPage(ctx, run, after, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, serialiseRun(run, page))
	}
	return map[string]any{"runs": out}, nil
}

func serialiseRun(run sandbox.PendingRun, calls sandbox.BridgeCallPage) map[string]any {
	row := map[string]any{
		"turn_id": run.TurnID,
		// The unit of work behind that run, so a board row links back to
		// the trigger rather than only to the one execution that detached.
		// THROUGH THE ACCESSOR, because nothing rewrites a parked row: a
		// run suspended before the identities were split carries the key
		// in its turn id instead. Empty when the run genuinely has none.
		"work_key":     run.UnitOfWork(),
		"agent_handle": run.AgentHandle,
		"role":         run.Role,
		"status":       run.Status,
		"coding_agent": run.CodingAgent,
		// WHERE the run is, which became an operator question the moment
		// providers.sandbox became a catalogue: one company now runs some
		// seats on the engine host and others in a remote box, and "is
		// this job on my machine" has no other surface. Empty on a row
		// written before the field existed.
		"placement":        run.Placement,
		"task_description": run.TaskDescription,
		"question":         run.Question,
		"audience":         run.Audience,
		"branch":           run.Branch,
		"trace_id":         run.TraceID,
		"owner":            run.Owner,
		// The two facts the board draws, rather than the ids themselves: a
		// non-empty sandbox id means a box exists, and a set paused_at
		// means it is currently held as a snapshot and being paid for.
		"box_exists":         run.SandboxID != "",
		"paused_at":          isoOrEmpty(run.PausedAt),
		"pause_ttl_seconds":  run.PauseTTLSeconds,
		"started_at":         isoOrEmpty(run.CreatedAt),
		"updated_at":         isoOrEmpty(run.UpdatedAt),
		"answerable_in_chat": answerableInChat(run.ConversationKey),
		// WHO IS WAITING, which is the question a board full of parked
		// runs exists to answer and had no field for. Persisted rather
		// than re-derived precisely because the resumed turn does not see
		// its trigger — empty means nobody is waiting, which is the safe
		// half.
		"reply": run.Reply,
		// THE BRIDGE'S OWN TOOL LOG, or as much of it as this answer
		// carries. A native loop keeps its calls in memory and the turn
		// writes them at the end; a bridged run's are made by a process
		// outside the engine, minutes or hours apart and possibly across a
		// restart, so the run's log in the coordination store is the only
		// copy there is. `bridge_calls_total` is how many the log holds,
		// whatever the answer carries.
		"bridge_calls_total": calls.Total,
		// EVERY CALL A FIRST ANSWER'S LIST LEAVES OUT, counted: those
		// between its first and newest calls, which a cursor reaches, or —
		// on a run an older build recorded — those that build dropped,
		// which nothing reaches. A list that silently skips is a list that
		// lies about what the run did. A page asked for with calls_after
		// is a window by request, and bridge_calls_next says whether the
		// log goes on past it.
		"bridge_calls_elided": calls.Between + calls.Dropped,
		// THE THREE IDENTIFIERS a person needs to find this run in
		// somebody else's system: the coding CLI's own session, the
		// command the engine launched, and the chain of asks that led
		// here.
		"session_id":       run.SessionID,
		"command_id":       run.CommandID,
		"delegation_chain": run.DelegationChain,
	}
	// LEFT OUT ON A RUN THAT MADE NO BRIDGED CALLS: the board draws no
	// tool-call panel at all for a run nothing called through a bridge.
	if calls.Total > 0 {
		// The first calls, then the newest: End is empty on a page that
		// reached the log's end itself, and on every page after the first.
		row["bridge_calls"] = append(slices.Clip(calls.Calls), calls.End...)
	}
	// WHERE the left-out calls fall. A first page leaves out at most one
	// stretch: the calls between Calls and End, or the ones an older build
	// dropped, which it places within Calls — never both, because a log that
	// build kept is one page with no End.
	switch {
	case calls.Between > 0:
		row["bridge_calls_elided_after"] = len(calls.Calls)
	case calls.Dropped > 0:
		row["bridge_calls_elided_after"] = calls.DroppedAfter
	}
	// The cursor for the calls this answer does not carry, present exactly
	// when a page can reach them.
	if calls.Next > 0 {
		row["bridge_calls_next"] = calls.Next
	}
	return row
}

// answerableInChat reports whether a reply on a chat surface could ever reach
// this run.
//
// The resume path matches an inbound notification's conversation key against
// the one stored at kick-off, by exact string equality. A run started by
// anything OTHER than an external notification — a schedule tick, a task
// assignment, an A2A wake — stored a key derived from an event id, which no
// inbound message can reproduce. Such a run is not answerable through any chat
// surface, and telling somebody to "reply in the thread" would send them to a
// thread that does not exist.
//
// [notify.Derived] answers exactly that, and this had its own copy of the
// prefix to answer it with — so a rename of the fallback's namespace would
// have left this route confidently offering a thread that does not exist.
func answerableInChat(conversationKey string) bool {
	return notify.Derived(conversationKey)
}
