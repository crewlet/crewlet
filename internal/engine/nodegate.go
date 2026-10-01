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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
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
// Then each identity-claiming log gets its gate record, in the register's own
// order, and each answers with its own three-valued outcome. The logs are
// independent — a gate on one log drops that log's records and lifts that log's
// pin, whatever the other did — so a log that refuses or cannot answer does
// not stop the next one being written: the gesture gets as far as it can, and
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
// DERIVED, from what a gesture waits on, and never written down as a number.
// It is one write per identity-claiming log — counted off the register
// ([identityLogCount]), four in this build: the tracker, the knowledge base, the
// org chart and the identity estate — and every wait inside a write is the
// publisher's own, each bounded by [statelog.DefaultResolveBudget]: a wait for
// this node's applier to reach a peer's record, and the resolution of its own
// ([gateWaitsPerLog]). At a margin of three ([gateBudgetMargin]) that is two
// minutes today.
//
// IT WAS A LITERAL MINUTE, sized when two logs claimed identity: a margin of
// three over twenty seconds. The register grew to four and the literal did
// not, so the margin fell to one and a half without anybody deciding it — the
// failure a number stated beside the arithmetic it came from always has.
// Counted from the register, a fifth identity log raises the budget in the
// same commit that adds it.
//
// A wedged broker still does not hold the gesture for the life of the
// process: this is a bound, not a wait. And a client waits [GateClientWait],
// derived from this, so the node's own answer — every log's outcome and the
// operation id — reaches the operator before the client gives up.
func GateBudget() time.Duration { return gateBudget() }

// gateBudget is [GateBudget], counted once: the register is fixed for the
// life of a build.
var gateBudget = sync.OnceValue(func() time.Duration {
	return gateBudgetFor(identityLogCount())
})

// gateWaitsPerLog is how many publisher waits one log's gate record makes: a
// wait for this node's applier to reach a peer's record, and the resolution
// of its own. Each is bounded by [statelog.DefaultResolveBudget].
const gateWaitsPerLog = 2

// gateBudgetMargin is how many times its worst legitimate case a gesture is
// allowed — three, the margin the budget was first sized at, so a gesture
// that meets every wait at its bound still lands with two thirds to spare
// rather than being cut off by a bound that only just fits it.
const gateBudgetMargin = 3

// gateBudgetFor is [GateBudget] for a register holding logs identity-claiming
// logs.
func gateBudgetFor(logs int) time.Duration {
	return time.Duration(logs*gateWaitsPerLog*gateBudgetMargin) * statelog.DefaultResolveBudget
}

// GateClientWait is how long a client waits for a gesture's answer: the
// node's own budget, plus the part of the request the budget does not cover —
// the judgement before the first record (a coordination read of the node's
// presence lease) and the round trip around the gesture — allowed ONE more
// resolve budget at the gesture's own margin.
//
// ONE STATED RELATION for every client, because each used to carry its own
// seventy-five seconds with a comment saying it was a minute plus fifteen:
// `crewlet retention evict` and `readmit` read this, and the dashboard's
// GATE_REQUEST_TIMEOUT_MS is held to it exactly by a gate in internal/api.
func GateClientWait() time.Duration {
	return GateBudget() + gateBudgetMargin*statelog.DefaultResolveBudget
}

// identityLogCount is how many domains in this build's register claim identity —
// each is a log a gesture writes a gate record to.
func identityLogCount() int {
	n := 0
	for _, domain := range registeredDomains() {
		if domain.ClaimsIdentity() {
			n++
		}
	}
	return n
}

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

	// By is the principal who ran it, recorded on every log's record as the
	// author and the kind of party ([iam.ActorFor]) — what `crewlet
	// retention status` names beside an eviction — and, on the tracker's,
	// the knowledge base's and the org chart's, the credential they acted
	// through as well.
	//
	// NOT THE CREDENTIAL ON THE IDENTITY ESTATE'S. A gate record there is
	// pinned at its first version for ever, so a node that cannot decode it
	// never defers it, and that version has no field for the credential
	// (iamdomain.GateRecordVersion); what the identity log records is the
	// author and the kind alone.
	//
	// THE PRINCIPAL AND NOT ITS NAME, for two reasons. The records carry
	// the author's kind — and three of them the credential — beside the
	// name, and a bare name recorded every gesture as an operator acting
	// through a credential named after themselves: a person bound to a
	// seat, or a machine token acting as its owner, read as a Tier A token
	// of that name. And the org chart's and the identity estate's writers
	// judge `fleet:operate` on the party's OWN grants at the record
	// ([registration.NewGate]), which a name cannot carry.
	By iam.Principal

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
//
// AND THE OPERATION ID TO THE GRAMMAR A CALLER'S IS ([statelog.CheckCallerOpID]),
// here and not only at the route: every log's operation is derived from it,
// and the publisher reads the instant it was minted at to decide whether this
// node's ledger can vouch for a retry. An id that carries none reads as
// minted at the epoch, so once any ledger has been swept the gesture is
// answered `unknown` on every log without being published — on the first
// attempt as on every retry, a gesture that can never run and never says why.
// The HTTP route held that and nothing else did.
func (r GateRequest) valid() error {
	opID := statelog.CheckCallerOpID(r.OpID)
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
	case opID != nil:
		return fmt.Errorf("%w: the gate on %s: %w", ErrInvalidGate, r.Node, opID)
	case iam.RecordOwner(r.By) == "":
		return fmt.Errorf("%w: the gate on %s names no operator, and every "+
			"log's record carries who ran it", ErrInvalidGate, r.Node)
	}
	return nil
}

// operator is who ran the gesture as the log lines name them: the author and
// the credential, the two columns an operator searches the trail by.
func (r GateRequest) operator() (name, credential string) {
	a := iam.ActorFor(r.By)
	return a.Name, a.OperatorID
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

	// Domains is one entry per identity-claiming log, in the register's own
	// order — every one of them, whatever it answered.
	Domains []DomainGate
}

// Complete reports whether every log holds the gate record durably — applied
// on this node, or pending here and applied by every node as it reaches it.
//
// FALSE IS NOT A FAILURE OF THE WHOLE: the logs that did answer hold their
// record, and a retry under the same [GateResult.OpID] writes only what is
// missing — where the log's own answer says a retry can ([DomainGate.Retry]).
func (r GateResult) Complete() bool {
	for _, d := range r.Domains {
		if !d.done() {
			return false
		}
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

	// Outcome is the write's own three-valued answer — applied, pending or
	// unknown — and EMPTY when Err is set: a write that answered with an
	// error gave no outcome, which is not one of the three.
	Outcome  statelog.Outcome
	Position statelog.Position

	// Unvouched says an `unknown` Outcome was answered because this node's
	// operation ledger cannot vouch for the operation — it was minted
	// before the point this node's ledger may have lost rows to, so this
	// node published nothing and cannot tell whether the record landed
	// ([statelog.Result.Unvouched]). It decides the remedy: the same
	// gesture here answers the same way every time.
	Unvouched bool

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
		switch refusal.Reason {
		case statelog.ReasonEvicted:
			return only(statelog.GateOtherNode, "this node is evicted itself and "+
				"writes nothing to any log: run the gesture through a node the fleet "+
				"still counts, under the same operation id")
		case statelog.ReasonWrongStream:
			return only(statelog.GateReanchor, fmt.Sprintf("the log under %s's name "+
				"is not the one this node's rows were derived from: re-anchor it "+
				"first — nothing can be written to it until then — and then the same "+
				"gesture under the same operation id finishes it", d.Stream))
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
			return retry("this node is catching up with the log, which clears on " +
				"its own: then the same gesture under the same operation id finishes it")
		case statelog.ReasonDeferred:
			return retry("this node holds a record it cannot decode: a node "+
				"running a newer build can finish the same gesture under the same "+
				"operation id", statelog.GateOtherNode)
		case statelog.ReasonFloorUnknown:
			return retry("the trim floor could not be read: the same gesture " +
				"under the same operation id finishes it once coordination answers")
		case statelog.ReasonEvictionUnknown:
			return retry("this node could not read its own eviction state, which " +
				"clears the moment it reads again: then the same gesture under the " +
				"same operation id finishes it")
		case statelog.ReasonBelowFloor:
			return retry("this node is adopting a peer's snapshot: the same "+
				"gesture under the same operation id finishes it once it has, here "+
				"or through another node", statelog.GateOtherNode)
		}
		// EVERY OTHER REASON, in the framework's own two classes rather
		// than one: a reason that clears on this node by itself
		// ([statelog.Reason.Retryable]) is offered the retry — this arm
		// told an operator no retry cleared `eviction_unknown`, which the
		// next read of the state does — and every reason is advised in
		// the REFUSAL'S OWN WORDS, because they are what say what clears
		// it: the limit that refused a record too large and what moves
		// it, the broker's words on a refusal it named. Reduced to the
		// reason's name, both reached the operator as a word with no
		// remedy, while the remedy written for one of them — "check the
		// broker's max_payload", on a bare queue.ErrTooLarge — sat on a
		// branch no refusal reached: every too-large answer the state log
		// gives is this typed refusal.
		own := ""
		if refusal.Detail != "" {
			own = " — " + refusal.Detail
		}
		if refusal.Reason.Retryable() {
			return retry("this refusal clears on this node by itself: " +
				string(refusal.Reason) + own)
		}
		return statelog.GateRemedy{
			Detail: "no retry clears this refusal: " + string(refusal.Reason) + own,
		}
	}
	if errors.Is(d.Err, statelog.ErrConflict) {
		return retry("the node's gate subject kept changing under this write: the " +
			"same gesture under the same operation id finishes it")
	}
	return retry("the write failed before it could answer: the same gesture under " +
		"the same operation id finishes it")
}

// NodeGate is the gesture, over every identity-claiming log this node runs.
type NodeGate struct {
	logs []gateLog

	// live lists the nodes holding a presence lease, which an eviction is
	// judged against, and readmissible is the state log's own judgement of
	// a readmission.
	live         func(ctx context.Context) ([]statelog.Presence, error)
	readmissible func(ctx context.Context, nodeID string) error
}

// gateLog is one identity-claiming log as the gate writes it.
type gateLog struct {
	domain string
	stream string

	// write publishes the log's own gate record — or, for a retried
	// operation whose record is already the gate in force, answers where it
	// landed without publishing, which the log's own snapshot judges
	// ([statelog.GateStanding]).
	write gateWrite
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
// EVERY LIVE NODE COUNTS ON EVERY LOG, whatever roles it declares, because
// every node runs every domain in the register: one that has published no
// position for a log is between its boot and its first heartbeat, never
// declining that log, so nothing here filters by what a node runs.
func livePresences(ctx context.Context, leases liveLeases) ([]statelog.Presence, error) {
	held, err := leases.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, fmt.Errorf("list the live nodes: %w", err)
	}
	out := make([]statelog.Presence, 0, len(held))
	for _, lease := range held {
		if id, ok := coord.NodeID(lease.Resource); ok {
			out = append(out, statelog.Presence{NodeID: id})
		}
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
// A LOG THAT CLAIMS IDENTITY AND HAS NO WRITER HERE REFUSES THE BOOT — and
// before anything is provisioned, since [checkRegister] asks every such entry
// for its [registration.NewGate]. The trim counts nodes on it, so an eviction
// that could not reach it would lift every pin but that one — the defect this
// gate replaced, reintroduced by adding a domain.
//
// Its OWN writers, rather than the surfaces' — the tracker's may not exist
// (a company on an external tracker still runs every log in the register) and
// the page store's may not either, and an eviction is a decision about a
// machine that has to reach every log whichever backends the company chose.
func newNodeGate(s *stateLog, leases liveLeases) (*NodeGate, error) {
	g := &NodeGate{
		live: func(ctx context.Context) ([]statelog.Presence, error) {
			return livePresences(ctx, leases)
		},
		readmissible: s.Readmissible,
	}
	for _, name := range s.order {
		running := s.domains[name]
		if !running.domain.ClaimsIdentity() {
			continue
		}
		gl, err := gateLogFor(s, running, running.publisher)
		if err != nil {
			return nil, err
		}
		g.logs = append(g.logs, gl)
	}
	if len(g.logs) == 0 {
		return nil, errors.New("engine: no registered domain claims identity, so " +
			"there is no log an eviction could be written to")
	}
	return g, nil
}

// gateLogFor is one identity-claiming log's writer for the gate, publishing
// through publisher — the domain's own write authority — and built by the
// domain's register entry, the one place every domain is declared.
func gateLogFor(s *stateLog, running *runningDomain,
	publisher *statelog.Publisher) (gateLog, error) {

	name := running.domain.Name()
	entry, found := registrationFor(name)
	if !found || entry.NewGate == nil {
		return gateLog{}, fmt.Errorf("engine: domain %q claims identity and the node "+
			"gate has no writer for its log — an eviction would lift every "+
			"other log's pin and leave the node counted on this one", name)
	}
	write, err := entry.NewGate(s, running, publisher)
	if err != nil {
		return gateLog{}, fmt.Errorf("engine: the node gate's writer for %s: %w", name, err)
	}
	return gateLog{domain: name, stream: running.domain.Stream().Name, write: write}, nil
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
	live, err := g.live(ctx)
	switch {
	case err != nil && !req.Force:
		return GateResult{}, &GateUnjudged{Node: req.Node, Err: err}
	case err != nil:
		by, credential := req.operator()
		log.WarnContext(ctx, "retention_eviction_forced_unjudged",
			"node", req.Node, "by", by, "operator", credential, "op_id", req.OpID,
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
			by, credential := req.operator()
			log.WarnContext(ctx, "retention_eviction_forced_live",
				"node", req.Node, "by", by, "operator", credential, "op_id", req.OpID,
				"detail", "the node holds a live presence lease and the "+
					"operator forced its eviction past it")
		}
	}
	return g.write(ctx, req, false), nil
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
	if err := g.readmissible(ctx, req.Node); err != nil {
		return GateResult{}, fmt.Errorf("engine: readmit node %s: %w", req.Node, err)
	}
	return g.write(ctx, req, true), nil
}

// write publishes the gate record to every identity-claiming log, in order.
//
// UNDER ITS OWN BUDGET, NOT THE CALLER'S. The judgement above ran under the
// caller's context, so a request abandoned before it wrote nothing; from here
// on the gesture is half-done the moment it stops, and the caller going away —
// a closed connection, a client's own timeout — is not a request to leave a
// node evicted on one log and counted on the other. The values travel, so each
// write keeps the trace and the operator it was asked under.
func (g *NodeGate) write(ctx context.Context, req GateRequest, readmit bool) GateResult {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), GateBudget())
	defer cancel()
	out := GateResult{Node: req.Node, OpID: req.OpID}
	for _, l := range g.logs {
		d := DomainGate{Domain: l.domain, Stream: l.stream,
			OpID: domainOpID(req.OpID, readmit, l.domain, req.Node)}
		res, err := l.write(ctx, req.By, d.OpID, req.Node, readmit)
		if err != nil {
			d.Err = err
		} else {
			d.Outcome, d.Position = res.Outcome, res.Position
			d.Unvouched = res.Outcome == statelog.OutcomeUnknown && res.Unvouched
		}
		out.Domains = append(out.Domains, d)
	}
	return out
}
