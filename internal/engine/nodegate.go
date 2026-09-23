package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
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
// ONE MINUTE, from what a gesture waits on. It is one write per identity log
// (two today), and every wait inside a write is the publisher's own, each
// bounded by [statelog.DefaultResolveBudget]: a wait for this node's applier to
// reach a peer's record, and the resolution of its own. Two logs of two waits
// is twenty seconds; a minute is three times that, so a broker under load still
// finishes the gesture, and a wedged one does not hold it for the life of the
// process. `crewlet retention evict` waits a little longer than this, so the
// node's own answer — every log's outcome and the operation id — reaches the
// operator before the client gives up.
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

// Remedy is what the operator does about it.
func (e *GateUnjudged) Remedy() string {
	return fmt.Sprintf("this node could not read the presence leases, so it cannot "+
		"tell whether %s is still running: retry once coordination answers, or — "+
		"if you know %s is gone — evict it with -force, which does not need them",
		e.Node, e.Node)
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
func (d DomainGate) Retry() bool {
	retry, _ := d.judge()
	return retry
}

// Remedy is what the operator does about a log the gesture did not finish, or
// empty for one it did.
func (d DomainGate) Remedy() string {
	_, remedy := d.judge()
	return remedy
}

func (d DomainGate) judge() (bool, string) {
	if d.done() {
		return false, ""
	}
	if d.Err == nil {
		// UNKNOWN: the record may or may not be on the log, and nothing
		// here can tell which — the log's own ledger can, next time.
		return true, "its outcome is unknown: the same gesture under the same " +
			"operation id answers from this log's own ledger if the record landed, " +
			"and writes it if it did not"
	}
	var refusal *statelog.Unavailable
	if errors.As(d.Err, &refusal) {
		switch refusal.Reason {
		case statelog.ReasonEvicted:
			return false, "this node is evicted itself and writes nothing to any " +
				"log: run the gesture from a node the fleet still counts (-url)"
		case statelog.ReasonWrongStream:
			return false, fmt.Sprintf("the log under %s's name is not the one "+
				"this node's rows were derived from: re-anchor it with `crewlet "+
				"retention reanchor` first — nothing can be written to it until "+
				"then", d.Stream)
		case statelog.ReasonLogFull:
			// PAST THE GATE RESERVE: a gate record is admitted into the
			// room kept above the ceiling ordinary writes are refused
			// at, so a gate record refused `log_full` found even that
			// spent — which no retry refills.
			return false, fmt.Sprintf("%s is full to its broker ceiling, past even "+
				"the reserve kept there for gate records: raise its ceiling with "+
				"`crewlet retention set-capacity`, which is the only thing that "+
				"makes room for it", d.Stream)
		case statelog.ReasonSuperseded:
			return false, "a later gate record has undone this operation's since: " +
				"start a new gesture, without -op-id, if the node should change again"
		case statelog.ReasonOpReused:
			return false, "the operation id already names a record on another " +
				"object: run the gesture without -op-id, so it takes a fresh one"
		case statelog.ReasonSkew:
			return false, "a store and a stream were restored out of step, which " +
				"no retry clears: restore them from one backup"
		case statelog.ReasonBehind:
			return true, "this node is catching up with the log, which clears on its own"
		case statelog.ReasonDeferred:
			return true, "this node holds a record it cannot decode and a node " +
				"running a newer build can serve it (-url)"
		case statelog.ReasonFloorUnknown:
			return true, "the trim floor could not be read; it clears once " +
				"coordination answers"
		case statelog.ReasonBelowFloor:
			return true, "this node is adopting a peer's snapshot; it clears once " +
				"it has, or another node can serve it (-url)"
		}
		return false, "no retry clears this refusal: " + string(refusal.Reason)
	}
	if errors.Is(d.Err, statelog.ErrConflict) {
		return true, "the node's gate subject kept changing under this write"
	}
	if errors.Is(d.Err, queue.ErrTooLarge) {
		return false, "the record is larger than the broker carries, which no " +
			"retry changes: check the broker's max_payload"
	}
	return true, "the write failed before it could answer: the same gesture under " +
		"the same operation id finishes it"
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
	write func(ctx context.Context, by, opID, node string, readmit bool) (statelog.Result, error)
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
// A LOG THAT CLAIMS IDENTITY AND HAS NO WRITER HERE REFUSES THE BOOT. The trim
// counts nodes on it, so an eviction that could not reach it would lift every
// pin but that one — the defect this gate replaced, reintroduced by adding a
// domain.
//
// Its OWN writers, rather than the surfaces' — the tracker's may not exist
// (a company on an external tracker still runs every log in the register) and
// the page store's may not either, and an eviction is a decision about a
// machine that has to reach every log whichever backends the company chose.
func newNodeGate(s *stateLog, leases liveLeases, db *store.DB, nodeID string,
	rec *metrics.Recorder) (*NodeGate, error) {

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
		gl, err := gateLogFor(running, running.publisher, db, nodeID, rec)
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
// through publisher — the domain's own write authority.
func gateLogFor(running *runningDomain, publisher *statelog.Publisher, db *store.DB,
	nodeID string, rec *metrics.Recorder) (gateLog, error) {

	name := running.domain.Name()
	gl := gateLog{domain: name, stream: running.domain.Stream().Name}
	switch name {
	case tracker.Domain{}.Name():
		w, err := tracker.NewWriter(tracker.WriterDeps{
			Publisher: publisher, DB: db, NodeID: nodeID,
			Drain: running.runner.Drain, Metrics: rec,
			// THE NODE'S OWN IDENTITY, overridden per gesture with the
			// operator who ran it — see [tracker.Writer.As].
			Actor: nodeID, ActorKind: tracker.AuthorSystem,
		})
		if err != nil {
			return gateLog{}, fmt.Errorf("engine: the node gate's writer for %s: %w", name, err)
		}
		gl.write = func(ctx context.Context, by, opID, node string,
			readmit bool) (statelog.Result, error) {

			as := w.As(by, tracker.AuthorOperator, tracker.Provenance{OperatorID: by})
			gate := as.EvictNode
			if readmit {
				gate = as.ReadmitNode
			}
			res, err := gate(ctx, opID, node)
			return res.Result, err
		}
	case pages.Domain{}.Name():
		kb, err := pages.NewStore(pages.Options{Publisher: publisher, DB: db})
		if err != nil {
			return gateLog{}, fmt.Errorf("engine: the node gate's writer for %s: %w", name, err)
		}
		gl.write = func(ctx context.Context, by, opID, node string,
			readmit bool) (statelog.Result, error) {

			// THE OPERATOR AS THE PAGE STORE NAMES ONE, which is the
			// credential's own id — the same spelling the tracker's
			// record carries, so both logs name the same person.
			actor := pages.Actor{Handle: by, Kind: pages.AuthorOperator, OperatorID: by}
			if readmit {
				return kb.ReadmitNode(ctx, actor, opID, node)
			}
			return kb.EvictNode(ctx, actor, opID, node)
		}
	default:
		return gateLog{}, fmt.Errorf("engine: domain %q claims identity and the node "+
			"gate has no writer for its log — an eviction would lift every "+
			"other log's pin and leave the node counted on this one", name)
	}
	return gl, nil
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), GateBudget)
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
		}
		out.Domains = append(out.Domains, d)
	}
	return out
}
