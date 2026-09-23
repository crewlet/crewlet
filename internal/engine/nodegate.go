package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
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
// the result says exactly how far.
//
// # And a retry finishes it, idempotently
//
// Each log's record is published under an operation id DERIVED from the
// gesture's own ([domainOpID]), so a retry under the same gesture id is the
// same operation on every log. A log whose own ledger already holds it answers
// from the ledger, at the position it landed, without appending again; a log
// that never got it is written now. That is what makes a partial result — the
// tracker applied, the pages log `unknown` — something an operator finishes by
// running the same command again rather than something to repair.

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
	// [statelog.PermitEviction]. A readmission ignores it.
	Force bool
}

// valid refuses a request that could not be retried as the same operation.
func (r GateRequest) valid() error {
	switch {
	case r.Node == "":
		return errors.New("engine: a node gate names no node")
	case r.OpID == "":
		return fmt.Errorf("engine: the gate on %s has no operation id — a retry "+
			"under a fresh one would be a second gesture rather than the same "+
			"one finished", r.Node)
	case r.By == "":
		return fmt.Errorf("engine: the gate on %s names no operator, and every "+
			"log's record carries who ran it", r.Node)
	}
	return nil
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
// missing.
func (r GateResult) Complete() bool {
	for _, d := range r.Domains {
		if d.Err != nil || (d.Outcome != statelog.OutcomeApplied &&
			d.Outcome != statelog.OutcomePending) {
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
	// answer. A retry under the same gesture id is safe either way — a
	// record that did land is answered for by the log's own ledger.
	Err error
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

	// applied answers from this node's own operation ledger for the log,
	// which is how a retried gesture is answered without a second record.
	applied func(ctx context.Context, opID string) (statelog.Position, bool, error)

	// write publishes the log's own gate record.
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
// same operation on that log, and DISTINCT per log, so each log's ledger
// answers for its own record — the tracker's own step-id idiom for a sequence
// that publishes several records.
//
// THE SIGN IS PART OF IT. The ledger answers a retry without writing, so an id
// an operator carried from an eviction to the readmission after it would
// otherwise be answered "applied" out of the eviction's own entry — every log
// reporting the node back while every applier still drops its records.
func domainOpID(gesture string, readmit bool, domain string) string {
	verb := "evict"
	if readmit {
		verb = "readmit"
	}
	return gesture + "." + verb + "." + domain
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
		gl := gateLog{
			domain: name, stream: running.domain.Stream().Name,
			applied: running.runner.Op,
		}
		switch name {
		case tracker.Domain{}.Name():
			w, err := tracker.NewWriter(tracker.WriterDeps{
				Publisher: running.publisher, DB: db, NodeID: nodeID,
				Drain: running.runner.Drain, Metrics: rec,
				// THE NODE'S OWN IDENTITY, overridden per gesture with the
				// operator who ran it — see [tracker.Writer.As].
				Actor: nodeID, ActorKind: tracker.AuthorSystem,
			})
			if err != nil {
				return nil, fmt.Errorf("engine: the node gate's writer for %s: %w", name, err)
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
			kb, err := pages.NewStore(pages.Options{Publisher: running.publisher, DB: db})
			if err != nil {
				return nil, fmt.Errorf("engine: the node gate's writer for %s: %w", name, err)
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
			return nil, fmt.Errorf("engine: domain %q claims identity and the node "+
				"gate has no writer for its log — an eviction would lift every "+
				"other log's pin and leave the node counted on this one", name)
		}
		g.logs = append(g.logs, gl)
	}
	if len(g.logs) == 0 {
		return nil, errors.New("engine: no registered domain claims identity, so " +
			"there is no log an eviction could be written to")
	}
	return g, nil
}

// Evict removes a node from every identity-claiming log's counted set.
//
// JUDGED BEFORE ANYTHING IS WRITTEN: a node holding a live presence lease is
// refused unless the request forces it, and a lease listing that cannot be
// read refuses too — a judgement nobody could make is not one that came back
// clear. Past the judgement the error is nil and every log's own answer is in
// the result.
func (g *NodeGate) Evict(ctx context.Context, req GateRequest) (GateResult, error) {
	if err := req.valid(); err != nil {
		return GateResult{}, err
	}
	live, err := g.live(ctx)
	if err != nil {
		return GateResult{}, fmt.Errorf("engine: judge the eviction of %s: %w",
			req.Node, err)
	}
	if err := statelog.PermitEviction(req.Node, live, req.Force); err != nil {
		return GateResult{}, fmt.Errorf("engine: evict node %s: %w", req.Node, err)
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
func (g *NodeGate) write(ctx context.Context, req GateRequest, readmit bool) GateResult {
	out := GateResult{Node: req.Node, OpID: req.OpID}
	for _, l := range g.logs {
		d := DomainGate{Domain: l.domain, Stream: l.stream,
			OpID: domainOpID(req.OpID, readmit, l.domain)}
		// THE LOG'S OWN LEDGER FIRST. A gesture retried after a partial
		// result is this same operation, and a log that already applied
		// it answers where it landed rather than appending it again. A
		// ledger that cannot be read is not a ledger that said no: the
		// write below takes its own snapshot of the same estate and says
		// why it cannot.
		if at, done, err := l.applied(ctx, d.OpID); err == nil && done {
			d.Outcome, d.Position = statelog.OutcomeApplied, at
			out.Domains = append(out.Domains, d)
			continue
		}
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
