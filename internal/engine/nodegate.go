package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE NODE GATE: eviction and readmission as ONE fleet gesture over every
// identity-claiming log.
//
// # Why one gesture, and why it lives here
//
// The trim counts nodes PER LOG, and a node stops being counted on a log only
// once an eviction record on THAT log is older than the fence window — so
// evicting a node is a record on every log that claims identity, each at a
// position in its own sequence space, and a readmission is the inverse commit
// on every one of them. For as long as the gesture was the tracker writer's
// alone, it wrote the tracker's log and nothing else: the pages log's
// applier, fence and table for an eviction existed and nothing in production
// ever filled them, so an evicted node stayed counted there, the pages log's
// applied term stayed pinned at its last position, and the log grew toward the
// ceiling that refuses writes — behind a machine the operator had been told was
// gone.
//
// The engine is the one place holding every domain's write authority, the
// positions register, the published floors and the presence leases, which is
// why the gesture is here rather than on any one domain's writer.
//
// # Judged once, then written log by log
//
// Whether the gesture is permitted — an eviction refuses a node that still holds
// a live presence lease ([statelog.PermitEviction]); a readmission refuses one
// below a trim floor it would be counted against ([statelog.PermitReadmission])
// — is asked ONCE, before anything is appended. Judged per log, one gesture
// could reach two answers about one node and leave it evicted on one log and
// counted on the other. A refusal writes nothing anywhere.
//
// Then each identity-claiming log THE GESTURE CONCERNS gets its gate record,
// and each answers with its own three-valued outcome, reported in the
// register's order of domains ([stateLog.identityLogs]). Under layout 0 that is every identity-claiming log,
// since every data node holds the one partition. Under a divided layout an
// eviction writes every log the node is COUNTED ON — whose partition the
// estate map names it a holder of, in any state, or whose key its positions
// row names and has not released (contract §E4): exactly the logs the trim
// waits for it on ([stateLog.countedOnLogs]). A readmission writes every
// identity-claiming log of the layout, because where the node is evicted is a
// fact each log's own rows hold and nothing outside them can list — a holder
// evicted before it first reported was counted on a log its row never named,
// and the map has let it go since — and a readmission on a log that never
// evicted the node changes no row there.
//
// Each log is written by a node SERVING its partition: this one, where it
// serves it and runs the log; for every other log the record is sent, as the
// estate's `statelog.gate` operation, to a holder that does
// ([estate.Router.Gate]), which publishes it through its own write authority
// on this node's behalf — the judgement above is not made again there. So one
// gesture on any node reaches every log, and each log's answer names the node
// that wrote it where that is not this one ([DomainGate.Writer]), because a
// refusal is about the node whose authority gave it.
//
// # And the estate map
//
// Under a divided layout an eviction also BARS the node from the estate map
// ([EstateControl.Bar]) — takes it out, and keeps it out through its removal
// for absence and its return — so the maintainer never places a partition on
// an evicted node that comes back, nor makes it a server of a copy it comes
// back with (partmap's "a barred node is never made a server"), and a
// readmission lifts the bar and puts it back IN ([EstateControl.Readmit]) —
// the one gesture that does: the operator's own `crewlet estate in` refuses a
// barred node, since only the readmission knows when its logs have taken it
// back. An operator's plain out would not do: it ends when membership removes
// the member, which for a machine an operator evicts is usually already the
// case, and a repaired machine restarted under its old id was then placed on
// after its probation while every log it was placed to serve still gated it.
//
// Both AFTER the logs, and the in ONLY ONCE EVERY LOG HAS TAKEN THE NODE BACK:
// an in placing the node while one of its logs still holds its eviction is the
// one order in which a partition is placed on a node that partition's log
// gates — every write it decides there refused, and the trim, counting it from
// a tombstone rather than its row, passing the holder that applies the log. So
// a readmission with a log unfinished leaves the map unwritten, saying it
// waits for the logs ([ErrMapAwaitsLogs]), and the same gesture under the same
// operation id — which answers the finished logs from their own ledgers —
// makes the in once the last log is done — on whichever node it runs, since a
// gesture reaches every log from any node, each one this node does not serve
// through a holder that does. An eviction's bar needs no such wait: it errs in
// the safe direction whatever the logs answered. The map's answer is its own
// part of the result ([MapGate]).
//
// The logs are independent — a gate on one log drops that log's records and
// lifts that log's pin, whatever another did — so they are written AT ONCE,
// and a log that refuses or cannot answer does not stop another being written:
// the gesture gets as far as it can, and
// the result says exactly how far. And it gets there whatever its CALLER does
// meanwhile: once the first record is about to be written the gesture runs
// under its own budget ([GateBudget]) rather than the request's, because a
// dropped connection is not a request to leave a node evicted on one log and
// counted on the other.
//
// # And a retry finishes it, idempotently
//
// Each log's record is published under an operation id DERIVED from the
// gesture's own, its sign, the log and the node ([domainOpID]), so a retry
// under the same gesture id is the same operation on every log. Each log's
// snapshot reads that id's ledger row before anything is decided
// ([statelog.Snap.Held]) and judges it against the node's own row in the same
// transaction ([statelog.GateStanding]): a log whose record is already the
// gate in force answers applied at the position it landed without appending
// again, a log that never got it is written now, and a log whose record has
// been undone since — an eviction retried after a readmission — is refused
// `superseded` rather than reported as though it had just happened. That is what makes a
// partial result — the tracker applied, the pages log `unknown` — something an
// operator finishes by running the same command again rather than something to
// repair.

// GateBudget bounds one gesture from its first record to its last answer.
//
// ONE MINUTE, from what a gesture waits on. It is one write per log it
// concerns, every log written at once ([NodeGate.write]), so the gesture waits
// as long as its SLOWEST log, whether it writes layout 0's two logs or a
// divided layout's every partition's — written one log after another, as it
// was while a gesture wrote two, the bound would have had to grow with the
// partitions a node serves.
//
// The slowest log is one this node does not write itself. A log it writes is
// the publisher's own waits, each bounded by [statelog.DefaultResolveBudget]
// — its applier reaching a peer's record, and the resolution of its own: ten
// seconds. A log it sends to the partition's holders ([estate.Router.Gate]) is
// asked of one holder at a time, each for at most one [estate.AppendAttempt]
// (fifteen seconds: those same ten, and the request's transit), the next asked
// only once one has gone silent. At the company's default copies
// ([config.DefaultEstateReplicas], three) a partition has at most three
// holders this node is not, and the walk past two silent ones to the third's
// answer, with a holder whose copy lags asked again last, is four attempts —
// a minute. A company that keeps more copies, with more of them silent, has
// that log answer that its holders did not, and the same gesture under the
// same operation id asks the silent ones last ([estate.Router]'s suspicion,
// thirty seconds) and finishes it. Bounded by the request's own deadline
// instead, a wedged broker would hold the gesture for the life of the process.
//
// `crewlet retention evict` and the dashboard wait a little longer than this,
// so the node's own answer — every log's outcome and the operation id —
// reaches the operator before the client gives up.
const GateBudget = time.Minute

// ErrInvalidGate is what a gate request that could not be carried out as given
// wraps: no node, a node id no node could run under, no operation id, or no
// operator. Nothing is judged and nothing is written.
var ErrInvalidGate = errors.New("engine: invalid gate request")

// GateRequest is one eviction or readmission, as the operator asked for it.
type GateRequest struct {
	// Node is the machine being evicted or taken back.
	Node string

	// OpID is the GESTURE's operation id, supplied by the caller so a retry
	// of a partial result is the same operation on every log.
	OpID string

	// By is the operator who ran it, recorded on every log's record — it is
	// what `crewlet retention status` names beside an eviction.
	By string

	// Force evicts a node that still holds a live presence lease — see
	// [statelog.PermitEviction] — and one whose lease could not be read at
	// all. A readmission ignores it.
	Force bool
}

// valid refuses a request that could not be retried as the same operation.
//
// THE NODE ID IS HELD TO THE RULE A NODE'S OWN ID IS, because it becomes a
// subject token on every identity log: an id no node could run under is a
// typo, and one carrying a wildcard is an append the broker refuses outright.
func (r GateRequest) valid() error {
	switch {
	case r.Node == "":
		return fmt.Errorf("%w: a node gate names no node", ErrInvalidGate)
	case !config.ValidNodeID(r.Node):
		return fmt.Errorf("%w: %q is not a node id any node can run under — one "+
			"starts alphanumeric and holds only letters, digits, '.', '_' or '-', "+
			"at most 64 characters", ErrInvalidGate, r.Node)
	case r.OpID == "":
		return fmt.Errorf("%w: the gate on %s has no operation id — a retry "+
			"under a fresh one would be a second gesture rather than the same "+
			"one finished", ErrInvalidGate, r.Node)
	case r.By == "":
		return fmt.Errorf("%w: the gate on %s names no operator, and every "+
			"log's record carries who ran it", ErrInvalidGate, r.Node)
	}
	return nil
}

// GateUnjudged is an eviction nobody could judge: the presence leases it is
// decided against could not be read, and the request did not force it.
//
// ITS OWN TYPE, NOT AN [*statelog.EvictionRefusal], because the remedy is the
// opposite one: a refusal names a node still reaching the fleet and says to
// stop it, and this names a coordination read that failed on THIS node — the
// target may well be gone, which is the case eviction exists for.
type GateUnjudged struct {
	Node string
	Err  error
}

func (e *GateUnjudged) Error() string {
	return fmt.Sprintf("engine: the eviction of %s cannot be judged: %v — a lease "+
		"listing nobody could read is not one that came back clear", e.Node, e.Err)
}

func (e *GateUnjudged) Unwrap() error { return e.Err }

// Remedy is what the operator does about it: ask again once coordination
// answers, or force the eviction, which does not need the leases.
//
// IN NO SURFACE'S VOCABULARY — see [statelog.GateAction]. This is the refusal
// most likely to meet an operator in a browser, since a coordination fault is
// exactly when an absent node most needs evicting, and its sentence used to
// tell them to pass a flag the dashboard does not have.
func (e *GateUnjudged) Remedy() statelog.GateRemedy {
	return statelog.GateRemedy{
		Actions: []statelog.GateAction{statelog.GateRetrySameOp, statelog.GateForce},
		Detail: fmt.Sprintf("this node could not read the presence leases, so it "+
			"cannot tell whether %s is still running: ask again once coordination "+
			"answers, or — if you know %s is gone — force the eviction, which does "+
			"not need them", e.Node, e.Node),
	}
}

// GateResult is what one gesture did to every log it had to reach.
type GateResult struct {
	Node string
	OpID string

	// Domains is one entry per identity-claiming log the node is counted
	// on, in the register's order of domains — every one of them, whatever it
	// answered.
	Domains []DomainGate

	// Map is what the gesture did to the estate map, nil under a layout
	// that places no map (layout 0).
	Map *MapGate
}

// ErrMapAwaitsLogs is a readmission's map part left unwritten because a log the
// gesture concerns has not taken the node back yet: the node is put back in the
// estate map only once every log has, or the maintainer could place a partition
// on it that the partition's log still gates. The gesture's own entries say
// which logs are unfinished, and the same gesture under the same operation id —
// on any node, since every log is reached from any node — finishes them and
// then makes the in.
var ErrMapAwaitsLogs = errors.New("engine: the node is put back in the estate map only once " +
	"every log has taken it back")

// MapGate is the estate map's part of a gesture: an eviction bars the node from
// it ([membership.Bar], recorded as `evicted`) — out, and kept out whatever
// becomes of its membership — and a readmission lifts the bar and puts it back
// ([membership.Readmit]).
type MapGate struct {
	// Gesture is "out" for an eviction's bar and "in" for a readmission.
	Gesture string

	// Landed is whether the stored map now says what the gesture asked.
	Landed bool

	// Err is why the map was not changed, or could not be read or written.
	Err error
}

// done reports a map that says what the gesture asked — or, for a readmission,
// one that keeps nothing of the node to lift: a node it neither holds,
// remembers nor bars has nothing keeping it off the map.
//
// NEVER AN EVICTION'S REFUSAL: a bar is written whether or not the map holds
// the node, so an eviction whose map part did not land has not finished — a
// map that "places nothing on the node" today is one that will the day the
// machine comes back.
func (m MapGate) done() bool {
	if m.Err != nil {
		return m.Gesture == "in" && errors.Is(m.Err, membership.ErrUnknownMember)
	}
	return m.Landed
}

// Remedy is what the operator does about a map the gesture did not finish, or
// the zero remedy for one it did — beside which a map that places nothing on
// the node already still says so.
func (m MapGate) Remedy() statelog.GateRemedy {
	switch {
	case m.Err == nil && m.Landed:
		return statelog.GateRemedy{}
	case m.Err == nil:
		return statelog.GateRemedy{Actions: []statelog.GateAction{statelog.GateRetrySameOp},
			Detail: "the estate map kept changing under the gesture: the same gesture " +
				"under the same operation id writes it again, and every log that holds " +
				"its record answers from its own rows"}
	case errors.Is(m.Err, ErrMapAwaitsLogs):
		return statelog.GateRemedy{Actions: []statelog.GateAction{statelog.GateRetrySameOp},
			Detail: "the node is put back in the estate map only once every log has taken " +
				"it back, so that no partition is placed on it while its log still gates " +
				"it: the same gesture under the same operation id finishes the logs this " +
				"answer names, and then puts it back"}
	case m.Gesture == "in" && errors.Is(m.Err, membership.ErrUnknownMember):
		return statelog.GateRemedy{Detail: "the estate map keeps nothing of the node to " +
			"lift: it is no member the map holds, and no node it remembers removing or " +
			"bars, so nothing keeps it off the map"}
	case errors.Is(m.Err, membership.ErrNothingPlaceable):
		// THE SAME OPERATION, once there is somewhere else to place: a
		// fresh one would write every log that already holds the record
		// again.
		return statelog.GateRemedy{Actions: []statelog.GateAction{statelog.GateRetrySameOp},
			Detail: "taking the node out would leave the estate map no present member " +
				"to place copies on: add a data node, then the same gesture under the " +
				"same operation id finishes it"}
	case errors.Is(m.Err, partmap.ErrNoMap):
		return statelog.GateRemedy{Actions: []statelog.GateAction{statelog.GateRetrySameOp},
			Detail: "no estate map has been written for this layout yet, so there is " +
				"nothing to take the node out of: the same gesture under the same " +
				"operation id finishes it once the map's duty has written the first"}
	case errors.Is(m.Err, ErrEstateNewerMap):
		return statelog.GateRemedy{Actions: []statelog.GateAction{statelog.GateOtherNode},
			Detail: "the estate map was written by a newer build: run the same gesture " +
				"through a node running it, under the same operation id"}
	}
	return statelog.GateRemedy{Actions: []statelog.GateAction{statelog.GateRetrySameOp},
		Detail: "the estate map could not be read or written: the same gesture under " +
			"the same operation id finishes it once coordination answers"}
}

// Complete reports whether every log holds the gate record durably — applied
// on this node, or pending here and applied by every node as it reaches it.
//
// FALSE IS NOT A FAILURE OF THE WHOLE: the logs that did answer hold their
// record, and a retry under the same [GateResult.OpID] writes only what is
// missing — where the log's own answer says a retry can ([DomainGate.Retry]).
func (r GateResult) Complete() bool {
	if !r.logsDone() {
		return false
	}
	if r.Map != nil {
		// A NODE COUNTED ON NO LOG is complete once the map says so: a
		// member that holds nothing yet is still one the map places on.
		return r.Map.done()
	}
	return len(r.Domains) > 0
}

// DomainGate is one log's answer to one gesture.
type DomainGate struct {
	// Domain names the log as the positions register keys it, and Stream as
	// the broker does.
	Domain string
	Stream string

	// OpID is the operation this log's record is published under, derived
	// from the gesture's.
	OpID string

	// Duplicates is the log's duplicate window: how long the broker holds an
	// operation id on the record it first landed, collapsing the same id
	// sent again onto it. A record of this gesture's that landed and applies
	// nowhere — refused `evicted` or `released` with a position — keeps the
	// gesture's id spent on this log for that long, which the remedy says.
	Duplicates time.Duration

	// Outcome is the write's own three-valued answer — applied, pending or
	// unknown — and EMPTY when Err is set: a write that answered with an
	// error gave no outcome, which is not one of the three.
	Outcome  statelog.Outcome
	Position statelog.Position

	// Unvouched says an `unknown` Outcome was answered because the writing
	// node's operation ledger cannot vouch for the operation — it was
	// minted before the point that ledger may have lost rows to, so the
	// node published nothing and cannot tell whether the record landed
	// ([statelog.Result.Unvouched]). It decides the remedy: the same
	// gesture there answers the same way every time.
	Unvouched bool

	// Writer is the node whose write authority gave this log's answer when
	// it is NOT this one: a holder of the log's partition the gesture sent
	// the record to ([estate.Router.Gate]), because this node does not
	// serve the partition, could not tell whether it does, or does not run
	// the log right now. Empty where this node wrote the log itself, and
	// where no holder answered at all. Every refusal and every `unknown`
	// is about the node that wrote, so the remedy names it.
	Writer string

	// Err is why this log gave no outcome: a refusal naming its reason
	// (a [*statelog.Unavailable]), or a failure before the write could
	// answer. Whether running the gesture again can clear it is
	// [DomainGate.Retry]'s.
	Err error
}

// done reports a log that holds the record durably.
func (d DomainGate) done() bool {
	return d.Err == nil &&
		(d.Outcome == statelog.OutcomeApplied || d.Outcome == statelog.OutcomePending)
}

// Retry reports whether running the same gesture again, under the same
// operation id, can finish this log — false for a log that is finished and for
// one whose refusal no retry clears.
//
// # Why this is the gate's to say, once
//
// An operator told to run the command again reads it as the remedy. For an
// `unknown` outcome, a lost race or a node still catching up, it is: the log's
// own decision answers from its rows if the record landed and writes it if it
// did not. For a node the fleet has evicted, a log rebuilt under this node, a
// full log or an operation superseded since, the same command fails the same
// way for ever — and advising it anyway sent operators round a loop the answer
// already knew the end of. The route and the command both render this one
// judgement, so they can never advise two different things.
func (d DomainGate) Retry() bool { return d.Remedy().Offers(statelog.GateRetrySameOp) }

// Remedy is what the operator does about a log the gesture did not finish, or
// the zero remedy for one it did.
//
// THE ACTIONS ARE WHAT A SURFACE SWITCHES ON and the detail names no surface's
// controls: the command line renders [statelog.GateNewGesture] as "without
// -op-id" and the dashboard as starting afresh, and a sentence spelling the
// flags was rendered word for word by a screen that has none of them.
func (d DomainGate) Remedy() statelog.GateRemedy {
	if d.done() {
		return statelog.GateRemedy{}
	}
	retry := func(detail string, also ...statelog.GateAction) statelog.GateRemedy {
		return statelog.GateRemedy{
			Actions: append([]statelog.GateAction{statelog.GateRetrySameOp}, also...),
			Detail:  detail,
		}
	}
	only := func(action statelog.GateAction, detail string) statelog.GateRemedy {
		return statelog.GateRemedy{Actions: []statelog.GateAction{action}, Detail: detail}
	}
	if d.Err == nil && d.Unvouched && d.Writer != "" {
		// UNKNOWN, AND NO HOLDER THAT ANSWERED COULD SETTLE IT: the router
		// asks the next holder after an unvouched answer, so this is what
		// every one that answered said. Another node is no way round it —
		// any node sends the record to the same holders — but a holder
		// that did not answer this time, or one that joins the partition,
		// may vouch.
		return retry(fmt.Sprintf("node %s, which serves the partition %s is on, "+
			"cannot tell whether the record landed, and neither could any other "+
			"holder of it that answered: each one's operation ledger may have lost "+
			"the record of this operation, which was minted before it adopted a "+
			"snapshot or swept its ledger. The same gesture under the same "+
			"operation id asks every holder again, and one that did not answer "+
			"this time may vouch for it", d.Writer, d.Stream))
	}
	if d.Err == nil && d.Unvouched {
		// UNKNOWN, AND NOT FOR THIS NODE TO SETTLE. The operation was
		// minted before the point this node's ledger may have lost rows
		// to — a snapshot adopted since, the ledger's own sweep — so it
		// published nothing, and the same gesture here answers `unknown`
		// again every time: the row the answer needs is the one the loss
		// took. Offered `retry_same_op`, every Finish and every -op-id
		// rerun went round that loop for ever. A node whose ledger
		// reaches back that far can answer it under the same id.
		return only(statelog.GateOtherNode, "this node cannot tell whether "+
			"the record landed: its operation ledger may have lost the record "+
			"of this operation, which was minted before it adopted a snapshot "+
			"or swept its ledger, so the same gesture here answers the same way "+
			"every time. Run it through a node whose ledger reaches back that "+
			"far, under the same operation id")
	}
	if d.Err == nil {
		// UNKNOWN: the record may or may not be on the log, and nothing
		// here can tell which — the log's own ledger can, next time.
		return retry("its outcome is unknown: the same gesture under the same " +
			"operation id answers from this log's own ledger if the record landed, " +
			"and writes it if it did not")
	}
	var refusal *statelog.Unavailable
	if errors.As(d.Err, &refusal) {
		if refusal.CopyWriter != "" {
			// ANOTHER NODE'S COPY, whatever the gate: the reason is that
			// node's standing, and every case below is about this one's.
			return retry(d.anotherNodesCopy(refusal))
		}
		switch refusal.Reason {
		case statelog.ReasonEvicted:
			if refusal.Position.Stream != "" {
				return only(statelog.GateOtherNode, d.who()+" is evicted itself, and "+
					d.landedNowhere(refusal.Position, "a node the fleet still counts"))
			}
			return only(statelog.GateOtherNode, d.who()+" is evicted itself and "+
				"writes nothing to any log: run the gesture through a node the fleet "+
				"still counts, under the same operation id")
		case statelog.ReasonReleased:
			// ALWAYS A RECORD THAT LANDED: a node that has begun to leave
			// is refused `not_holder` before anything is appended, so a
			// release gate only ever drops a write already on its way.
			return only(statelog.GateOtherNode, fmt.Sprintf("%s released %s "+
				"when it left that log's partition, and ", d.who(), d.Stream)+
				d.landedNowhere(refusal.Position, "a node that serves the partition"))
		case statelog.ReasonOvertaken:
			// A RECORD A RESTORED REANCHOR OVERTOOK: this node wrote from
			// rows the reanchor did not keep, before it learned of the move.
			// Always one that landed, and this node refuses the log until it
			// re-keys to the new generation.
			return only(statelog.GateOtherNode, "a restored reanchor had overtaken "+
				d.whose()+" rows when it wrote, and "+d.landedNowhere(refusal.Position,
				"a node on the log's current generation"))
		case statelog.ReasonAbandoned:
			return only(statelog.GateOtherNode, d.who()+" wrote in a generation a "+
				"reanchor abandoned, and "+d.landedNowhere(refusal.Position,
				"a node the fleet still counts"))
		case statelog.ReasonNotHolder:
			// THIS NODE'S OWN WRITE, refused by gate 3 between the gesture
			// choosing to write the log here and the write itself: the
			// partition moved off this node meanwhile. Run again, the
			// gesture sends the record to a node that serves it now.
			return retry(fmt.Sprintf("this node stopped serving the partition %s is "+
				"on while the gesture wrote it, and only a node that serves a "+
				"partition writes its logs: the same gesture under the same operation "+
				"id sends the record to one that does", d.Stream))
		case statelog.ReasonHoldingUnknown:
			return retry(fmt.Sprintf("this node could not tell whether it serves "+
				"the partition %s is on when it wrote: the same gesture under the same "+
				"operation id writes it here once it can tell, or sends it to a node "+
				"that serves the partition", d.Stream))
		case statelog.ReasonWrongStream:
			return only(statelog.GateReanchor, fmt.Sprintf("the log under %s's name "+
				"is not the one %s rows were derived from: re-anchor it "+
				"first — nothing can be written to it until then — and then the same "+
				"gesture under the same operation id finishes it", d.Stream, d.whose()))
		case statelog.ReasonLogFull:
			// PAST THE GATE RESERVE: a gate record is admitted into the
			// room kept above the ceiling ordinary writes are refused
			// at, so a gate record refused `log_full` found even that
			// spent — which no retry refills.
			return only(statelog.GateSetCapacity, fmt.Sprintf("%s is full to its "+
				"broker ceiling, past even the reserve kept there for gate records: "+
				"raise its ceiling, which is the only thing that makes room for it — "+
				"then the same gesture under the same operation id finishes it, where "+
				"a fresh one would write every log that already holds the record "+
				"again", d.Stream))
		case statelog.ReasonSuperseded:
			return only(statelog.GateNewGesture, "a later gate record has undone "+
				"this operation's since: start a new gesture, under a fresh operation "+
				"id, if the node should change again")
		case statelog.ReasonOpReused:
			return only(statelog.GateNewGesture, "the operation id already names a "+
				"record on another object: start the gesture again under a fresh "+
				"operation id")
		case statelog.ReasonSkew:
			return only(statelog.GateRestore, "a store and a stream were restored "+
				"out of step, which no retry clears: restore them from one backup")
		case statelog.ReasonBehind:
			return retry(d.who() + " is catching up with the log, which clears on " +
				"its own: then the same gesture under the same operation id finishes it")
		case statelog.ReasonDeferred:
			return retry(d.who()+" holds a record it cannot decode: a node "+
				"running a newer build can finish the same gesture under the same "+
				"operation id", statelog.GateOtherNode)
		case statelog.ReasonFloorUnknown:
			return retry("the trim floor could not be read: the same gesture " +
				"under the same operation id finishes it once coordination answers")
		case statelog.ReasonBelowFloor:
			return retry(d.who()+" is adopting a peer's snapshot: the same "+
				"gesture under the same operation id finishes it once it has, or "+
				"through another node", statelog.GateOtherNode)
		}
		return statelog.GateRemedy{
			Detail: "no retry clears this refusal: " + string(refusal.Reason),
		}
	}
	if errors.Is(d.Err, statelog.ErrConflict) {
		return retry("the node's gate subject kept changing under this write: the " +
			"same gesture under the same operation id finishes it")
	}
	var unserved *estate.ErrPartitionUnserved
	if errors.As(d.Err, &unserved) {
		// NOBODY WROTE IT: every holder of the partition the gesture
		// sent the record to ran nothing, or did not answer. Nothing
		// landed under the operation id, so the same one finishes it.
		return retry(fmt.Sprintf("no node that serves the partition %s is on wrote "+
			"the record: each one it was sent to did not serve it, could not run it "+
			"or did not answer. The same gesture under the same operation id "+
			"finishes it once one does", d.Stream))
	}
	if errors.Is(d.Err, estate.ErrGateKind) {
		// A KIND THE WRITING NODE DOES NOT WRITE: it runs an older build
		// than this one, which knows neither the kind nor what to do with
		// it — and refused it rather than writing the wrong gate. Nothing
		// landed, so the same operation id finishes it once that node runs
		// the build this one does.
		writer := "the node that served the partition " + d.Stream + " is on"
		if d.Writer != "" {
			writer = "node " + d.Writer
		}
		return retry(fmt.Sprintf("%s does not write this kind of gate record: it runs "+
			"an older build than this node, and refused the record rather than write "+
			"another kind in its place. Once it runs this node's build, the same gesture "+
			"under the same operation id finishes it", writer))
	}
	if errors.Is(d.Err, queue.ErrTooLarge) {
		return statelog.GateRemedy{Detail: "the record is larger than the broker " +
			"carries, which no retry changes: check the broker's max_payload"}
	}
	return retry("the write failed before it could answer: the same gesture under " +
		"the same operation id finishes it")
}

// anotherNodesCopy is the remedy for a record of this gesture that ANOTHER node
// wrote, which this node's append was collapsed onto
// ([statelog.Unavailable.CopyWriter]) and a gate dropped: an operation id
// carried to this node inside the log's duplicate window from a node that had
// already written under it, and has since been evicted or left the partition,
// or wrote it in a generation a reanchor abandoned or from rows one overtook.
//
// # Why those four gates and no others
//
// A copy's writer is named only under a gate that blames it
// ([statelog.Reason.BlamesWriter]), and of those a GATE RECORD meets exactly
// four. It is never `wrong_partition`: an eviction, a readmission and a release
// are the log's own records, which both identity-claiming domains place in no
// partition — their PartitionOf answers none for the eviction kind, which
// statelogtest.Placement holds both to — so no copy of one is ever on the
// wrong log. And never `deleted`, which blames no writer and is asked only of
// a task or a page. The fallback names the reason of a gate a later build adds
// to that set without a sentence here, rather than describing it as one of the
// four.
//
// # Why here, and not another node
//
// The gate is about the copy's writer. This node passed its own fences — it is
// counted, and it serves the partition — before it appended, so it is exactly
// the node that finishes the gesture: once the broker has let go of the id,
// the same gesture under it here writes afresh, and cannot apply twice because
// the copy in the way applies nowhere. Read as this node's own `evicted`, the
// operator was told the node they ran it on was evicted and sent to another,
// with the node that could finish it a minute later in front of them.
func (d DomainGate) anotherNodesCopy(refusal *statelog.Unavailable) string {
	why := fmt.Sprintf("a gate dropped it (%s)", refusal.Reason)
	switch refusal.Reason {
	case statelog.ReasonEvicted:
		why = "that node is evicted"
	case statelog.ReasonReleased:
		why = fmt.Sprintf("that node released %s when it left that log's partition", d.Stream)
	case statelog.ReasonOvertaken:
		why = "that node wrote it from rows a restored reanchor had overtaken"
	case statelog.ReasonAbandoned:
		why = "that node wrote it in a generation a reanchor abandoned"
	}
	about, where := "this one", " here"
	if d.Writer != "" {
		about, where = "node "+d.Writer, ""
	}
	return fmt.Sprintf("the record of this gesture at %s on %s is node %s's copy, "+
		"which %s own write was collapsed onto, and it applies nowhere "+
		"because %s — a fact about node %s and not about %s. The broker holds "+
		"its operation id for %s from when it landed. Once that has passed, the same "+
		"gesture under the same operation id finishes it%s: before then the id "+
		"is collapsed onto that record and refused the same way, and a fresh id "+
		"would write every log that already holds the gesture's record again",
		refusal.Position, d.Stream, refusal.CopyWriter, d.whose(), why, refusal.CopyWriter,
		about, d.window(), where)
}

// who is the node a refusal on this log is about, as a remedy names it: this
// node, where it wrote the log itself, or the holder of the partition it sent
// the record to ([DomainGate.Writer]) — whose standing a refusal describes,
// and never this node's.
func (d DomainGate) who() string {
	if d.Writer == "" {
		return "this node"
	}
	return fmt.Sprintf("node %s, which serves the partition and wrote this log for "+
		"the gesture,", d.Writer)
}

// whose is [DomainGate.who] as a possessive.
func (d DomainGate) whose() string {
	if d.Writer == "" {
		return "this node's"
	}
	return fmt.Sprintf("node %s's", d.Writer)
}

// window names the log's duplicate window for a remedy — its length where the
// gate knows it.
func (d DomainGate) window() string {
	if d.Duplicates > 0 {
		return fmt.Sprintf("the log's duplicate window, %s,", d.Duplicates)
	}
	return "the log's duplicate window"
}

// landedNowhere is the remedy for a record of this gesture that landed at on
// this log and applies nowhere: the gesture is finished through another node,
// named by who, under the SAME operation id — but only once the broker has let
// go of that id.
//
// # Why neither "now" nor a fresh id
//
// The broker holds the operation id on the record it first landed for the
// log's duplicate window, so the same id sent before then — by any node — is
// collapsed onto that record and refused the same way. A fresh id would be
// written at once, but it is a SECOND gesture: every log that already holds
// this one's record would be written again, its gate re-dated, and the first
// id would answer `superseded` to anyone finishing it. The record in the way
// applies nowhere, so the same id after the window cannot apply twice.
func (d DomainGate) landedNowhere(at statelog.Position, who string) string {
	return fmt.Sprintf("the record this gesture put on %s at %s applies nowhere — "+
		"the broker holds its operation id for %s from when it landed. Once that "+
		"has passed, run the gesture through %s under the same operation id: "+
		"before then the id is collapsed onto that record and refused the same "+
		"way, and a fresh id would write every log that already holds the "+
		"gesture's record again", d.Stream, at, d.window(), who)
}

// NodeGate is the gesture, over every identity-claiming log the node it names
// is counted on.
type NodeGate struct {
	// logs is every identity-claiming log the named node is counted on AT
	// THE GESTURE, each with this node's writer or the route to a node that
	// writes it:
	// looked up at every call rather than captured when the gate was
	// built, because the logs a node runs change while it runs
	// ([stateLog.startLogs], [stateLog.stopLogs]). A captured set wrote
	// through the publisher of a log that had stopped — waiting, for the
	// whole [GateBudget], on a runner that no longer applies — and never
	// reached a log started after the gate was built, which the trim then
	// went on counting the evicted node on.
	logs func(ctx context.Context, node string, readmit bool) ([]gateLog, error)

	// estate takes a node out of the estate map and puts it back, nil
	// under a layout that places no map.
	estate estateMembership

	// live lists the nodes holding a presence lease, which an eviction is
	// judged against, and readmissible is the state log's own judgement of
	// a readmission.
	live         func(ctx context.Context) ([]statelog.Presence, error)
	readmissible func(ctx context.Context, nodeID string) error

	// publishing refuses a gesture on a node whose mode appends nothing
	// ([ErrNotPublishing]) — asked FIRST, before anything is judged, since
	// no judgement could make the write allowed.
	publishing func(gesture string) error
}

// estateMembership is the estate map's two membership gestures, as the node
// gate makes them ([EstateControl]): the eviction's bar, and the readmission's
// lift of it — never the operator's in, which refuses a barred node.
type estateMembership interface {
	Bar(ctx context.Context, node, by, reason string) (EstateGesture, error)
	Readmit(ctx context.Context, node, by string) (EstateGesture, error)
}

// gateMembership is the estate map's gestures as the node gate makes them
// under layout, nil under a layout that places no map — layout 0, where every
// data node holds the one partition and the map's duty writes none — or on a
// node with no coordination store to hold one.
//
// AN UNTYPED NIL, never a nil *EstateControl in the interface: the gate asks
// whether it has a map to gesture on by comparing with nil.
func (e *Engine) gateMembership(layout statelog.Layout) estateMembership {
	if layout.Number == 0 || e.estateControl == nil {
		return nil
	}
	return e.estateControl
}

// evictedReason is what an eviction records on the estate map as why the node
// is out, so a surface can tell a node an operator judged gone from one taken
// out for maintenance.
const evictedReason = "evicted"

// gateLog is one identity-claiming log as the gate writes it: through this
// node's own write authority where it serves the log's partition and runs the
// log (write), and otherwise through a node that does (route). Exactly one of
// the two is set.
type gateLog struct {
	domain string
	stream string

	// duplicates is the log's duplicate window, which a remedy for a record
	// that landed and applies nowhere names ([DomainGate.Duplicates]).
	duplicates time.Duration

	// write publishes the log's own gate record — or, for a retried
	// operation whose record is already the gate in force, answers where it
	// landed without publishing, which the log's own snapshot judges
	// ([statelog.GateStanding]).
	write func(ctx context.Context, by, opID, node string, readmit bool) (statelog.Result, error)

	// route sends the record to a node serving the log's partition, which
	// publishes it through its own write authority on this node's behalf
	// ([estate.Router.Gate]), and answers the node whose authority gave
	// the answer — empty for this node, and where no node answered.
	route func(ctx context.Context, by, opID, node string, readmit bool) (statelog.Result, string, error)
}

// publish writes the log's record — here, or through a holder of its
// partition — and answers its outcome and the node that wrote it, empty for
// this one.
func (l gateLog) publish(ctx context.Context, by, opID, node string,
	readmit bool) (statelog.Result, string, error) {

	if l.route != nil {
		return l.route(ctx, by, opID, node, readmit)
	}
	res, err := l.write(ctx, by, opID, node, readmit)
	return res, "", err
}

// gateRoute is how the gate sends a log's record to a node serving the log's
// partition: the estate's router ([estate.Router.Gate]), which tries this node
// first where it serves the partition and then the partition's holders in
// order, under the same operation id.
type gateRoute interface {
	Gate(ctx context.Context, a estate.GateArgs) (statelog.Result, string, error)
}

// routedGateLog is the gate log for id, sent through route as `statelog.gate`
// under layout. An answer this node itself gave — the router asks it first
// where it serves the partition — names no other writer.
func routedGateLog(route gateRoute, layout statelog.Layout, id statelog.LogID,
	spec statelog.StreamSpec, self string) gateLog {

	return gateLog{domain: id.String(), stream: spec.Name, duplicates: spec.Duplicates,
		route: func(ctx context.Context, by, opID, node string,
			readmit bool) (statelog.Result, string, error) {

			res, writer, err := route.Gate(ctx, estate.GateArgs{
				Layout: layout.Number, Domain: id.Domain, Partition: id.Partition.String(),
				Node: node, By: by, OpID: opID, Kind: gateKind(readmit),
			})
			if writer == self {
				writer = ""
			}
			return res, writer, err
		}}
}

// gateKind is the wire's name for a gesture's sign ([estate.GateKind]).
func gateKind(readmit bool) estate.GateKind {
	if readmit {
		return estate.GateReadmit
	}
	return estate.GateEvict
}

// readmits is the sign of a gate record that arrived over the wire: whether it
// is a readmission, and an error for a kind this build does not write.
//
// EVERY KIND NAMED, and no default that writes: a record that is not a
// readmission is not thereby an eviction, which is the one reading that
// publishes the most destructive record a gate has ([estate.GateKind]).
func readmits(kind estate.GateKind) (bool, error) {
	switch kind {
	case estate.GateEvict:
		return false, nil
	case estate.GateReadmit:
		return true, nil
	}
	return false, fmt.Errorf("%w: %q", estate.ErrGateKind, kind)
}

// liveLeases is the slice of the coordination backend the gate and the trim
// both judge presence from.
type liveLeases interface {
	ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error)
}

// livePresences is every node holding a live presence lease.
//
// ONE READING FOR THE TRIM AND FOR THE GATE, because they ask the same
// question: which nodes are still reaching the fleet. The trim counts such a
// node at position zero until its first heartbeat; the gate refuses to evict
// one.
//
// DATA NODES ONLY. A node without `data` applies no log and publishes no
// position, so the trim that counted it at zero would never advance again,
// and the gate has no copy of it to fence.
func livePresences(ctx context.Context, leases liveLeases) ([]statelog.Presence, error) {
	ids, err := dataNodes(ctx, leases)
	if err != nil {
		return nil, fmt.Errorf("list the live nodes: %w", err)
	}
	out := make([]statelog.Presence, 0, len(ids))
	for _, id := range ids {
		out = append(out, statelog.Presence{NodeID: id})
	}
	return out, nil
}

// domainOpID is one log's operation id for a gesture: STABLE, so a retry is the
// same operation on that log, and DISTINCT per log, per sign and per node, so
// each log's ledger answers for its own record — the tracker's own step-id
// idiom for a sequence that publishes several records.
//
// THE SIGN IS PART OF IT, so an id an operator carried from an eviction to the
// readmission after it is a different operation rather than one the eviction's
// ledger entry answers.
//
// AND THE NODE IS, so a gesture id carried to ANOTHER node is that node's own
// operation. Without it an eviction of node-b under node-a's gesture id was
// answered from node-a's ledger entries — complete, at node-a's positions —
// with nothing written for node-b at all, and inside the broker's duplicate
// window the append itself would have collapsed onto node-a's record too.
//
// THE STATE LOG'S OWN GRAMMAR ([statelog.StepOpID]), so each log's id carries
// the gesture's mint instant: the ledger's vouching reads it off the id, and
// one spelled here in a shape that grammar did not recognise would be read as
// minted at the zero instant — answered `unknown` on any node whose ledger
// ever lost a row, to its sweep or to a snapshot from a donor that scrubbed
// it. The sign is one step, and the log and the node together the last.
//
// INJECTIVE, which a plain join of four steps is not: a gesture id may carry
// a tail of its own and a node id may hold dots, so gesture `x` on node
// `y.evict.tracker.z` and gesture `x.evict.tracker.y` on node `z` would
// otherwise spell one id. A node id never holds a colon
// ([config.ValidNodeID]) and the sign and the log hold neither separator, so
// reading from the right — the node after the last colon, then the log and
// the sign after the last two dots — recovers every part. (A collision would
// still be refused rather than answered, its ledger row being on another
// node's subject ([statelog.ReasonOpReused]); an id that cannot collide is
// one whose refusal nobody has to read.)
func domainOpID(gesture string, readmit bool, domain, node string) string {
	verb := "evict"
	if readmit {
		verb = "readmit"
	}
	return statelog.StepOpID(gesture, verb, domain+":"+node)
}

// newNodeGate builds the gesture over every identity-claiming log in the
// register, in its own order.
//
// A LOG THAT CLAIMS IDENTITY AND HAS NO WRITER HERE REFUSES THE BOOT. The trim
// counts nodes on it, so an eviction that could not reach it would lift every
// pin but that one — the defect this gate replaced, reintroduced by adding a
// domain.
//
// Its OWN writers, rather than the surfaces' — the tracker's may not exist
// (a company on an external tracker still runs every log in the register) and
// the page store's may not either, and an eviction is a decision about a
// machine that has to reach every log whichever backends the company chose.
//
// CHECKED ONCE HERE, against the REGISTER, so the boot is what refuses a
// register no gesture could reach; every gesture then builds its writers over
// the logs running at that moment ([NodeGate.logs]). Never against the logs
// this node runs: under a divided layout a data node may serve no partition
// yet, and its gestures reach every log through the router all the same —
// checked against its running logs, such a node refused to boot.
//
// holders is who holds each partition of the layout — the half of "which logs
// is a node counted on" that is not its positions row — estate the map's
// membership gestures, nil under a layout that places no map, and route the
// estate's router, through which a log this node does not write is sent to a
// node that does: REQUIRED, because without it a gesture would reach only the
// logs of the partitions this node serves and report itself unfinished on
// every other for ever.
func newNodeGate(s *stateLog, leases liveLeases, holders partitionHolders,
	estate estateMembership, route gateRoute, db *store.DB, nodeID string,
	rec *metrics.Recorder) (*NodeGate, error) {

	if route == nil {
		return nil, errors.New("engine: the node gate needs the estate's router, " +
			"which carries each log this node does not write to a node that serves it")
	}
	g := &NodeGate{
		live: func(ctx context.Context) ([]statelog.Presence, error) {
			return livePresences(ctx, leases)
		},
		readmissible: func(ctx context.Context, node string) error {
			return s.Readmissible(ctx, node, holders)
		},
		publishing: s.appends,
		logs: func(ctx context.Context, node string, readmit bool) ([]gateLog, error) {
			return countedGateLogs(ctx, s, holders, route, db, nodeID, rec, node, readmit)
		},
		estate: estate,
	}
	if err := checkGateWriters(registeredDomains()); err != nil {
		return nil, err
	}
	return g, nil
}

// countedGateLogs is a gate log for every identity-claiming log of s's layout a
// gesture on node concerns, in [stateLog.identityLogs]' order — for an eviction the logs
// the node is counted on ([stateLog.countedOnLogs]), for a readmission every
// one ([stateLog.identityLogs]) — each with this node's own writer where it
// serves the log's partition and runs the log, and otherwise sent through
// route to a node that does ([routedGateLog]).
//
// A LOG THIS NODE CANNOT WRITE NOW IS ANOTHER NODE'S TO WRITE, whichever of
// the three reasons it is: a partition it does not serve, one it cannot tell
// whether it serves, or one it serves and whose log it does not run right now
// (stopped for an adoption, a reanchor or a runner cycling). Each was once
// reported here as unwritten, with the remedy of finishing it through another
// node — which on a divided estate was the ordinary answer for most logs, and
// left a readmission's map part waiting on logs no single node could finish.
func countedGateLogs(ctx context.Context, s *stateLog, holders partitionHolders,
	route gateRoute, db *store.DB, nodeID string, rec *metrics.Recorder, node string,
	readmit bool) ([]gateLog, error) {

	counted, err := s.identityLogs()
	if err == nil && !readmit {
		counted, err = s.countedOnLogs(ctx, holders, node)
	}
	if err != nil {
		return nil, err
	}
	out := make([]gateLog, 0, len(counted))
	for _, id := range counted {
		domain, err := registeredDomain(id.Domain)
		if err != nil {
			return nil, err
		}
		spec := s.layout.StreamSpec(domain, id)
		serving, holdErr := s.holding.Serving(id.Partition)
		running := s.Log(id.String())
		switch {
		case holdErr != nil || !serving || running == nil:
			out = append(out, routedGateLog(route, s.layout, id, spec, nodeID))
		default:
			gl, err := gateLogFor(running, running.publisher,
				db.PartitionHandle(id.Partition.String()).Reader(), nodeID, rec)
			if err != nil {
				return nil, err
			}
			out = append(out, gl)
		}
	}
	return out, nil
}

// countedOnLogs is every identity-claiming log of this node's layout that node
// is COUNTED on, in [stateLog.identityLogs]' order (contract §E4's "eviction gesture's
// logs"): a log whose partition the fleet says node holds — in any state —
// or whose key its positions row names and has not released.
//
// UNDER LAYOUT 0, EVERY IDENTITY-CLAIMING LOG. Its one partition is held by
// every data node whether or not it is running now, which is the answer the
// gesture has always had: a node the operator evicts is usually gone, so its
// presence is not where its holding is read from.
//
// A holder set that cannot be read is an error, and the gesture writes
// nothing: a node the map names a holder of a partition it has never reported
// on is counted there at zero, so a gesture written from the register alone
// would leave it pinning that log while reporting itself complete.
//
// It reads the positions register itself, once, and only under a divided
// layout; a caller that goes on to judge from the register hands its own
// reading to [stateLog.countedOn] instead.
func (s *stateLog) countedOnLogs(ctx context.Context, holders partitionHolders,
	node string) ([]statelog.LogID, error) {

	if s.layout.Number == 0 {
		return s.identityLogs()
	}
	rows, err := s.positions(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: read the positions register for the logs %s is "+
			"counted on: %w", node, err)
	}
	return s.countedOn(ctx, holders, node, rows)
}

// countedOn is [stateLog.countedOnLogs] over a reading of the positions
// register the caller already took — rows, as [stateLog.positions] answers
// them — so a caller that judges the node against the register chooses the
// logs from the SAME reading it judges on. Two readings of one register can
// disagree: a row that lands between them names a log the first never
// offered to be judged, and one that is gone from the second judges a log the
// first chose against no position at all.
func (s *stateLog) countedOn(ctx context.Context, holders partitionHolders,
	node string, rows []coord.NodePositions) ([]statelog.LogID, error) {

	identity, err := s.identityLogs()
	if err != nil || s.layout.Number == 0 {
		return identity, err
	}
	var partitions []statelog.PartitionID
	for _, id := range identity {
		if !slices.Contains(partitions, id.Partition) {
			partitions = append(partitions, id.Partition)
		}
	}
	var row *coord.NodePositions
	for i := range rows {
		if rows[i].NodeID == node {
			row = &rows[i]
		}
	}
	held := map[statelog.PartitionID][]statelog.Presence{}
	if holders != nil {
		if held, err = holders.Holders(ctx, partitions); err != nil {
			return nil, fmt.Errorf("engine: which partitions %s holds: %w", node, err)
		}
	}
	var out []statelog.LogID
	for _, id := range identity {
		holds := slices.ContainsFunc(held[id.Partition], func(p statelog.Presence) bool {
			return p.NodeID == node
		})
		var reported bool
		if row != nil {
			at, names := row.Domains[id.String()]
			reported = names && at.State != coord.LogReleased
		}
		if holds || reported {
			out = append(out, id)
		}
	}
	return out, nil
}

// identityLogs is every identity-claiming log of this node's layout — the logs
// that carry a node gate at all — in the REGISTER'S order of domains, and each
// domain's logs in the layout's order of partitions.
//
// THE REGISTER'S ORDER, because it is the order a gesture's answer has always
// listed its logs in — the tracker's, then the pages log — and under layout 0
// the answer is what it was, entry for entry. The layout's own order is by
// partition name, which would put the pages log first.
func (s *stateLog) identityLogs() ([]statelog.LogID, error) {
	var out []statelog.LogID
	for _, domain := range registeredDomains() {
		if domain.ClaimsIdentity() {
			out = append(out, s.layout.LogsOf(domain.Name())...)
		}
	}
	for _, id := range s.layout.AllLogs() {
		if _, err := registeredDomain(id.Domain); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// gateLogs is a fixed set of gate logs, as a [NodeGate.logs].
func gateLogs(logs ...gateLog) func(context.Context, string, bool) ([]gateLog, error) {
	return func(context.Context, string, bool) ([]gateLog, error) { return logs, nil }
}

// gateLogFor is one identity-claiming log's writer for the gate, publishing
// through publisher — the domain's own write authority.
//
// db is the log's own partition, which the domain's writer reads the rows it
// decides from out of.
func gateLogFor(running *runningLog, publisher *statelog.Publisher, db store.PartitionReader,
	nodeID string, rec *metrics.Recorder) (gateLog, error) {

	// THE LOG'S KEY NAMES THE GATE, never the domain alone: a domain with a
	// log in each of two partitions has two gates to write, and each is
	// reported — and its operation derived — as its own.
	name := running.domain.Name()
	build, known := gateWriterOf[name]
	if !known {
		return gateLog{}, errNoGateWriter(name)
	}
	write, err := build(running, publisher, db, nodeID, rec)
	if err != nil {
		return gateLog{}, fmt.Errorf("engine: the node gate's writer for %s: %w", name, err)
	}
	return gateLog{domain: running.key, stream: running.spec.Name,
		duplicates: running.spec.Duplicates, write: write}, nil
}

// gateWrite publishes one log's gate record — see [gateLog.write].
type gateWrite func(ctx context.Context, by, opID, node string, readmit bool) (statelog.Result, error)

// gateWriterOf is the node gate's writer for each domain that claims identity,
// by the domain's name.
//
// ONE TABLE, read both where a log's writer is built ([gateLogFor]) and where
// the boot checks that every identity-claiming domain of the register has one
// ([checkGateWriters]), so the check can never pass a domain the gate cannot
// write.
var gateWriterOf = map[string]func(running *runningLog, publisher *statelog.Publisher,
	db store.PartitionReader, nodeID string, rec *metrics.Recorder) (gateWrite, error){

	tracker.Domain{}.Name(): func(running *runningLog, publisher *statelog.Publisher,
		db store.PartitionReader, nodeID string, rec *metrics.Recorder) (gateWrite, error) {

		w, err := tracker.NewWriter(tracker.WriterDeps{
			Publisher: publisher, DB: db, NodeID: nodeID,
			Drain: running.runner.Drain, Metrics: rec,
			// THE NODE'S OWN IDENTITY, overridden per gesture with the
			// operator who ran it — see [tracker.Writer.As].
			Actor: nodeID, ActorKind: tracker.AuthorSystem,
		})
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, by, opID, node string,
			readmit bool) (statelog.Result, error) {

			as := w.As(by, tracker.AuthorOperator, tracker.Provenance{OperatorID: by})
			gate := as.EvictNode
			if readmit {
				gate = as.ReadmitNode
			}
			res, err := gate(ctx, opID, node)
			return res.Result, err
		}, nil
	},

	pages.Domain{}.Name(): func(_ *runningLog, publisher *statelog.Publisher,
		db store.PartitionReader, _ string, _ *metrics.Recorder) (gateWrite, error) {

		kb, err := pages.NewStore(pages.Options{Publisher: publisher, DB: db})
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, by, opID, node string,
			readmit bool) (statelog.Result, error) {

			// THE OPERATOR AS THE PAGE STORE NAMES ONE, which is the
			// credential's own id — the same spelling the tracker's
			// record carries, so both logs name the same person.
			actor := pages.Actor{Handle: by, Kind: pages.AuthorOperator, OperatorID: by}
			if readmit {
				return kb.ReadmitNode(ctx, actor, opID, node)
			}
			return kb.EvictNode(ctx, actor, opID, node)
		}, nil
	},
}

// checkGateWriters refuses a register with a domain that claims identity and
// has no gate writer ([gateWriterOf]).
func checkGateWriters(domains []statelog.Domain) error {
	for _, domain := range domains {
		if _, known := gateWriterOf[domain.Name()]; domain.ClaimsIdentity() && !known {
			return errNoGateWriter(domain.Name())
		}
	}
	return nil
}

// errNoGateWriter is an identity-claiming domain the gate cannot write.
func errNoGateWriter(name string) error {
	return fmt.Errorf("engine: domain %q claims identity and the node gate has no "+
		"writer for its log — an eviction would lift every other log's pin and "+
		"leave the node counted on this one", name)
}

// Evict removes a node from every identity-claiming log's counted set.
//
// JUDGED BEFORE ANYTHING IS WRITTEN: a node holding a live presence lease is
// refused unless the request forces it, and a lease listing that cannot be
// read refuses too — a judgement nobody could make is not one that came back
// clear ([*GateUnjudged]). Past the judgement the error is nil and every log's
// own answer is in the result.
//
// # And force decides the unreadable listing as well
//
// The listing is the judgement's ONLY input, and force is the operator saying
// that input does not decide this node: a machine wedged in a way that still
// renews its lease reads exactly like one whose lease this node cannot list,
// and refusing the second with a 500 during a coordination fault — which is
// when an absent node is most likely to need evicting — made force a flag that
// worked only when it was least needed. A forced eviction past an unreadable
// listing, or past a lease it held, is logged as such, because it is the one
// gesture here nobody's evidence cleared.
func (g *NodeGate) Evict(ctx context.Context, req GateRequest) (GateResult, error) {
	if err := req.valid(); err != nil {
		return GateResult{}, err
	}
	if err := g.publishing("an eviction"); err != nil {
		return GateResult{}, err
	}
	live, err := g.live(ctx)
	switch {
	case err != nil && !req.Force:
		return GateResult{}, &GateUnjudged{Node: req.Node, Err: err}
	case err != nil:
		log.WarnContext(ctx, "retention_eviction_forced_unjudged",
			"node", req.Node, "operator", req.By, "op_id", req.OpID,
			"error", err.Error(),
			"detail", "the presence leases could not be read, so nothing "+
				"established that the node is gone; the operator forced it")
	default:
		if err := statelog.PermitEviction(req.Node, live, req.Force); err != nil {
			return GateResult{}, fmt.Errorf("engine: evict node %s: %w", req.Node, err)
		}
		if req.Force && slices.ContainsFunc(live, func(p statelog.Presence) bool {
			return p.NodeID == req.Node
		}) {
			log.WarnContext(ctx, "retention_eviction_forced_live",
				"node", req.Node, "operator", req.By, "op_id", req.OpID,
				"detail", "the node holds a live presence lease and the "+
					"operator forced its eviction past it")
		}
	}
	return g.write(ctx, req, false)
}

// Readmit is the inverse commit on every identity-claiming log.
//
// JUDGED BEFORE ANYTHING IS WRITTEN, against every log at once: a node below a
// trim floor it would be counted against is refused with a
// [*statelog.ReadmissionRefusal] naming the log, its position and the floor,
// and nothing is appended anywhere — see [stateLog.Readmissible].
func (g *NodeGate) Readmit(ctx context.Context, req GateRequest) (GateResult, error) {
	if err := req.valid(); err != nil {
		return GateResult{}, err
	}
	if err := g.publishing("a readmission"); err != nil {
		return GateResult{}, err
	}
	if err := g.readmissible(ctx, req.Node); err != nil {
		return GateResult{}, fmt.Errorf("engine: readmit node %s: %w", req.Node, err)
	}
	return g.write(ctx, req, true)
}

// write publishes the gate record to every identity-claiming log the gesture
// concerns, all at once, and then makes the estate map's part of it.
//
// UNDER ITS OWN BUDGET, NOT THE CALLER'S. The judgement above ran under the
// caller's context, so a request abandoned before it wrote nothing; from here
// on the gesture is half-done the moment it stops, and the caller going away —
// a closed connection, a client's own timeout — is not a request to leave a
// node evicted on one log and counted on the other. The values travel, so each
// write keeps the trace and the operator it was asked under.
//
// The only error is that the logs to write cannot be told: nothing was
// written.
func (g *NodeGate) write(ctx context.Context, req GateRequest, readmit bool) (GateResult, error) {
	logs, err := g.logs(ctx, req.Node, readmit)
	if err != nil {
		return GateResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), GateBudget)
	defer cancel()
	out := GateResult{Node: req.Node, OpID: req.OpID, Domains: make([]DomainGate, len(logs))}
	var wg sync.WaitGroup
	for i, l := range logs {
		// EACH ENTRY ITS OWN GOROUTINE'S, so the answers are in the
		// logs' order however the writes finish.
		d := &out.Domains[i]
		*d = DomainGate{Domain: l.domain, Stream: l.stream, Duplicates: l.duplicates,
			OpID: domainOpID(req.OpID, readmit, l.domain, req.Node)}
		wg.Go(func() {
			res, writer, err := l.publish(ctx, req.By, d.OpID, req.Node, readmit)
			d.Writer = writer
			if err != nil {
				d.Err = err
				return
			}
			d.Outcome, d.Position = res.Outcome, res.Position
			d.Unvouched = res.Outcome == statelog.OutcomeUnknown && res.Unvouched
		})
	}
	wg.Wait()
	// THE MAP AFTER THE LOGS, and a readmission's in only once every one of
	// them has taken the node back — see the file's doc.
	switch {
	case g.estate == nil:
	case readmit && !out.logsDone():
		out.Map = &MapGate{Gesture: "in", Err: ErrMapAwaitsLogs}
	default:
		out.Map = g.mapGesture(ctx, req, readmit)
	}
	return out, nil
}

// logsDone reports whether every log the gesture concerns holds its record
// durably.
func (r GateResult) logsDone() bool {
	for _, d := range r.Domains {
		if !d.done() {
			return false
		}
	}
	return true
}

// mapGesture is the estate map's part of a gesture: the bar for an eviction,
// and for a readmission its lift — the in, under the gesture's name for it.
func (g *NodeGate) mapGesture(ctx context.Context, req GateRequest, readmit bool) *MapGate {
	if readmit {
		res, err := g.estate.Readmit(ctx, req.Node, req.By)
		return &MapGate{Gesture: "in", Landed: res.Landed, Err: err}
	}
	res, err := g.estate.Bar(ctx, req.Node, req.By, evictedReason)
	return &MapGate{Gesture: "out", Landed: res.Landed, Err: err}
}
