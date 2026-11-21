package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE GROWTH AUDIT (ADR-0171): the access log's record of every step of a
// growth act, one record per step, written from whichever node holds the seat
// (this node's own log where it keeps one, a data node's through the router
// otherwise). The held call records its ask; a product records what it makes:
// a read or operate call once it reached the vendor (Made or Failed), and a
// held call's answer before it acts (Verdict) and its outcome (Applied, Void
// or Failed). A published text leaves this node as its digest alone.

// growthAuditor is the engine's own name on every growth record it writes.
var growthAuditor = iam.Actor{Name: "growth", Kind: iam.ActorSystem}

// errNoGrowthLog is a node that neither keeps the access log nor reaches one
// that does, so it records no growth act.
var errNoGrowthLog = errors.New("engine: this node reaches no access log, so it records no growth act")

// growthWriter and growthCounter are the access log's growth write and count
// as this node reaches them.
type (
	growthWriter interface {
		Growth(ctx context.Context, c accessdomain.Call, n accessdomain.NewGrowth) (statelog.Result, error)
	}
	growthCounter interface {
		GrowthChanged(ctx context.Context, q accessdomain.GrowthCount, fresh statelog.Freshness) (int, error)
	}
)

// GrowthAudit is [Engine.GrowthAudit].
type GrowthAudit struct{ engine *Engine }

// GrowthAudit records the steps of growth acts and answers their reads.
func (e *Engine) GrowthAudit() GrowthAudit { return GrowthAudit{engine: e} }

// writer is this node's own access log, or a data node's through the router.
func (a GrowthAudit) writer() (growthWriter, error) {
	if c := a.engine.core.Load(); c != nil && c.accessWriter != nil {
		return c.accessWriter, nil
	}
	if a.engine.router == nil {
		return nil, errNoGrowthLog
	}
	// NO SEALER: a growth record holds no text to seal.
	return a.engine.router.AccessWrites(nil), nil
}

// counter is this node's own access log, or a data node's through the router.
func (a GrowthAudit) counter() (growthCounter, error) {
	if c := a.engine.core.Load(); c != nil && c.accessReader != nil {
		return c.accessReader, nil
	}
	if a.engine.router == nil {
		return nil, errNoGrowthLog
	}
	return a.engine.router.AccessReads(), nil
}

// Made records a read or operate call that reached the vendor.
func (a GrowthAudit) Made(ctx context.Context, r growth.Record) (bool, error) {
	return a.record(ctx, growth.StepMade, r)
}

// Asked records a held call put to an administrator.
func (a GrowthAudit) Asked(ctx context.Context, r growth.Record) (bool, error) {
	return a.record(ctx, growth.StepAsked, r)
}

// Verdict records a held call's answer: an administrator's allow or deny
// (r.Answerer their human seat), or the engine's lapse. first is false, with no
// error, where the call already holds an answer: the earlier one stands, so a
// product must not act on this one.
func (a GrowthAudit) Verdict(ctx context.Context, step growth.Step, r growth.Record) (first bool, err error) {
	switch step {
	case growth.StepAllowed, growth.StepDenied, growth.StepLapsed:
		return a.record(ctx, step, r)
	}
	return false, fmt.Errorf("engine: %q is no answer to a held growth call: allowed, denied or lapsed", step)
}

// Applied records an allowed call made at the vendor.
func (a GrowthAudit) Applied(ctx context.Context, r growth.Record) (bool, error) {
	return a.record(ctx, growth.StepApplied, r)
}

// Void records an allowed call whose target moved since the ask: nothing was sent.
func (a GrowthAudit) Void(ctx context.Context, r growth.Record) (bool, error) {
	return a.record(ctx, growth.StepVoid, r)
}

// Failed records a call that did not reach its result, r.Failure saying why.
func (a GrowthAudit) Failed(ctx context.Context, r growth.Record) (bool, error) {
	return a.record(ctx, growth.StepFailed, r)
}

// record writes r as step, under the operation derived from now: first is
// false, with no error, where the act's phase already holds a record (this
// step's earlier write, or another answer). A record whose landing is not
// known is an error.
func (a GrowthAudit) record(ctx context.Context, step growth.Step, r growth.Record) (first bool, err error) {
	w, err := a.writer()
	if err != nil {
		return false, err
	}
	n := accessdomain.GrowthOf(step, r)
	act := n.HeldCall
	if act == "" {
		act = n.Call
	}
	res, err := w.Growth(ctx, accessdomain.Call{
		By: growthAuditor, OpID: accessdomain.GrowthOpID(time.Now().UTC(), n.Handle, act, step),
	}, n)
	if errors.Is(err, accessdomain.ErrRecorded) {
		return false, nil
	}
	if landed := accessLanded(res, err, fmt.Sprintf("%s's %s", act, step)); landed != nil {
		return false, landed
	}
	return true, nil
}

// Changed counts the changes to one target that took effect at the vendor
// since since (allowed calls applied and operate calls made), every seat's
// where handle is "": what a product holds to a vendor's rate rule before it
// calls, read at the seat surface's level.
func (a GrowthAudit) Changed(ctx context.Context, product, account, target, handle string,
	since time.Time) (int, error) {
	r, err := a.counter()
	if err != nil {
		return 0, err
	}
	return r.GrowthChanged(ctx, accessdomain.GrowthCount{Product: product, Account: account, Target: target,
		Handle: handle, Since: since.UTC()}, statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)})
}

// Changes pages the recent agent changes from this node's access log: the
// Growth page's read, served where the API is, which holds the estate.
func (a GrowthAudit) Changes(ctx context.Context, q accessdomain.GrowthQuery,
	fresh statelog.Freshness) (accessdomain.GrowthPage, error) {
	r := a.engine.AccessReader()
	if r == nil {
		return accessdomain.GrowthPage{}, errNoGrowthLog
	}
	return r.GrowthChanges(ctx, q, fresh)
}
