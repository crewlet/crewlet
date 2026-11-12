package estate

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ACCESS LOG AS OPERATIONS: what each seat was told about its access and
// every SSH certificate and command (ADR-0103), reached from whichever node
// holds the seat. A turn is dispatched and launched where its seat is held, a
// seats-only leaf included, and only a data node publishes.
//
// Every write is the engine's own record of what it did ([originEngine]): the
// log is in no list of the domains a surface writes, and its writer facade
// ([AccessWrites]) is the engine's alone. Each carries the operation id the
// domain derives per execution, so a write one data node never answered is
// asked of the next under the same id. Every read is answered at the level its
// caller passes, on this node's floor on the log: a dispatch reads its seat's
// baseline at `linearizable`.

// AccessReader is the access log's read side: the domain reader's methods a
// node off the estate needs.
type AccessReader interface {
	Recent(ctx context.Context, handle string, since time.Time,
		fresh statelog.Freshness) (accessdomain.Recent, error)
	Told(ctx context.Context, handle, cursor string, limit int,
		fresh statelog.Freshness) (accessdomain.ToldPage, error)
	Certificates(ctx context.Context, q accessdomain.AuditQuery,
		fresh statelog.Freshness) (accessdomain.CertificatePage, error)
	Commands(ctx context.Context, q accessdomain.AuditQuery,
		fresh statelog.Freshness) (accessdomain.CommandPage, error)
	HostUse(ctx context.Context, week, month time.Time,
		fresh statelog.Freshness) ([]accessdomain.HostUse, error)
	SealedKeys(ctx context.Context) (accessdomain.SealedCount, error)
}

// AccessWriter is the access log's write side: the domain writer's records a
// turn or an SSH call makes, every one of them the engine's.
type AccessWriter interface {
	Tell(ctx context.Context, c accessdomain.Call, n accessdomain.NewTold) (statelog.Result, error)
	Issued(ctx context.Context, c accessdomain.Call, n accessdomain.NewCertificate) (statelog.Result, error)
	RanSealed(ctx context.Context, c accessdomain.Call, n accessdomain.NewCommand,
		sealed accessdomain.SealedCommand) (statelog.Result, error)
	VerdictSealed(ctx context.Context, c accessdomain.Call, n accessdomain.NewVerdict,
		sealed accessdomain.SealedVerdict) (statelog.Result, error)
}

// The domain's own sides are what a data node serves the log with.
var (
	_ AccessReader = (*accessdomain.Reader)(nil)
	_ AccessWriter = (*accessdomain.Writer)(nil)
)

// AccessHalf is the access log a data node serves; a nil writer beside a reader
// is a node that publishes nothing, whose writes move on to one that does.
type AccessHalf struct {
	Reader AccessReader
	Writer AccessWriter
}

// accessReader is the half's reader, or [errNoHalf].
func (b Backend) accessReader() (AccessReader, error) {
	if b.Access.Reader == nil {
		return nil, errNoHalf
	}
	return b.Access.Reader, nil
}

// accessWriter is the half's writer, or [errNoHalf].
func (b Backend) accessWriter() (AccessWriter, error) {
	if b.Access.Writer == nil {
		return nil, errNoHalf
	}
	return b.Access.Writer, nil
}

// ---- the reads ------------------------------------------------------------ //

type accessRecentArgs struct {
	Handle string
	Since  time.Time
	Fresh  statelog.Freshness
}

// A SEAT'S BASELINE AND ITS RECENT TOLD RECORDS, in one transaction on the node
// that answers: a dispatch asks `linearizable`.
var opAccessRecent = define("access.recent", opRead, accessDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a accessRecentArgs) (accessdomain.Recent, error) {
		r, err := b.accessReader()
		if err != nil {
			return accessdomain.Recent{}, err
		}
		return r.Recent(ctx, a.Handle, a.Since, a.Fresh)
	})

type accessToldArgs struct {
	Handle string
	Cursor string
	Limit  int
	Fresh  statelog.Freshness
}

var opAccessTold = define("access.told", opRead, accessDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a accessToldArgs) (accessdomain.ToldPage, error) {
		r, err := b.accessReader()
		if err != nil {
			return accessdomain.ToldPage{}, err
		}
		return r.Told(ctx, a.Handle, a.Cursor, a.Limit, a.Fresh)
	})

type accessAuditArgs struct {
	Query accessdomain.AuditQuery
	Fresh statelog.Freshness
}

var opAccessCertificates = define("access.certificates", opRead, accessDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a accessAuditArgs) (accessdomain.CertificatePage, error) {
		r, err := b.accessReader()
		if err != nil {
			return accessdomain.CertificatePage{}, err
		}
		return r.Certificates(ctx, a.Query, a.Fresh)
	})

type accessHostUseArgs struct {
	Week, Month time.Time
	Fresh       statelog.Freshness
}

// opAccessHostUse is each server's use, counted on a data node's copy: one row
// per server and seat, never the commands themselves.
var opAccessHostUse = define("access.host_use", opRead, accessDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a accessHostUseArgs) ([]accessdomain.HostUse, error) {
		r, err := b.accessReader()
		if err != nil {
			return nil, err
		}
		return r.HostUse(ctx, a.Week, a.Month, a.Fresh)
	})

var opAccessCommands = define("access.commands", opRead, accessDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a accessAuditArgs) (accessdomain.CommandPage, error) {
		r, err := b.accessReader()
		if err != nil {
			return accessdomain.CommandPage{}, err
		}
		return r.Commands(ctx, a.Query, a.Fresh)
	})

// accessSealedArgs carries nothing: the count is the whole log's.
type accessSealedArgs struct{}

var opAccessSealedKeys = define("access.sealed_keys", opRead, accessDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, _ accessSealedArgs) (accessdomain.SealedCount, error) {
		r, err := b.accessReader()
		if err != nil {
			return accessdomain.SealedCount{}, err
		}
		return r.SealedKeys(ctx)
	})

// ---- the writes: the engine's, each under the operation it carries ---------- //

type accessTellArgs struct {
	Call accessdomain.Call
	Told accessdomain.NewTold
}

var opAccessTell = define("access.tell", opIdempotentWrite, accessDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a accessTellArgs) (statelog.Result, error) {
		w, err := b.accessWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.Tell(ctx, a.Call, a.Told)
	}).byEngine()

type accessIssuedArgs struct {
	Call        accessdomain.Call
	Certificate accessdomain.NewCertificate
}

var opAccessIssued = define("access.issued", opIdempotentWrite, accessDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a accessIssuedArgs) (statelog.Result, error) {
		w, err := b.accessWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.Issued(ctx, a.Call, a.Certificate)
	}).byEngine()

type accessRanArgs struct {
	Call    accessdomain.Call
	Command accessdomain.NewCommand
	Sealed  accessdomain.SealedCommand
}

// A COMMAND'S RECORD crosses the router with its text sealed by the asking
// node, under the keyring every node holds, so no production command travels
// between nodes in the clear; the data node that takes it opens it to check it.
var opAccessRan = define("access.ran", opIdempotentWrite, accessDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a accessRanArgs) (statelog.Result, error) {
		w, err := b.accessWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.RanSealed(ctx, a.Call, a.Command, a.Sealed)
	}).byEngine()

type accessVerdictArgs struct {
	Call    accessdomain.Call
	Verdict accessdomain.NewVerdict
	Sealed  accessdomain.SealedVerdict
}

// A VERDICT'S RECORD crosses the router as a command's does (ADR-0125): what
// it decided and its giver's words sealed by the asking node, opened by the
// data node that takes it to check them.
var opAccessVerdict = define("access.verdict", opIdempotentWrite, accessDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a accessVerdictArgs) (statelog.Result, error) {
		w, err := b.accessWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.VerdictSealed(ctx, a.Call, a.Verdict, a.Sealed)
	}).byEngine()

// ---- the asking half ---------------------------------------------------- //

// AccessReads is the access log's reads, answered by a data node whose copy
// serves (this one where it does), so a consumer cannot tell local from routed.
type AccessReads struct{ r *Router }

// AccessReads answers the access log's reads.
func (r *Router) AccessReads() AccessReads { return AccessReads{r: r} }

var _ AccessReader = AccessReads{}

// Recent answers a seat's baseline and its told records since since.
func (x AccessReads) Recent(ctx context.Context, handle string, since time.Time,
	fresh statelog.Freshness) (accessdomain.Recent, error) {
	return call(ctx, x.r, opAccessRecent, nil, accessRecentArgs{Handle: handle, Since: since, Fresh: fresh})
}

// Told answers one page of a seat's told records, newest first.
func (x AccessReads) Told(ctx context.Context, handle, cursor string, limit int,
	fresh statelog.Freshness) (accessdomain.ToldPage, error) {
	return call(ctx, x.r, opAccessTold, nil,
		accessToldArgs{Handle: handle, Cursor: cursor, Limit: limit, Fresh: fresh})
}

// Certificates answers one page of the certificates, newest first.
func (x AccessReads) Certificates(ctx context.Context, q accessdomain.AuditQuery,
	fresh statelog.Freshness) (accessdomain.CertificatePage, error) {
	return call(ctx, x.r, opAccessCertificates, nil, accessAuditArgs{Query: q, Fresh: fresh})
}

// Commands answers one page of the commands, newest first, their text sealed.
func (x AccessReads) Commands(ctx context.Context, q accessdomain.AuditQuery,
	fresh statelog.Freshness) (accessdomain.CommandPage, error) {
	return call(ctx, x.r, opAccessCommands, nil, accessAuditArgs{Query: q, Fresh: fresh})
}

// HostUse answers each server's use since month, the week's counted apart.
func (x AccessReads) HostUse(ctx context.Context, week, month time.Time,
	fresh statelog.Freshness) ([]accessdomain.HostUse, error) {
	return call(ctx, x.r, opAccessHostUse, nil, accessHostUseArgs{Week: week, Month: month, Fresh: fresh})
}

// SealedKeys answers how many commands sit sealed under each keyring key.
func (x AccessReads) SealedKeys(ctx context.Context) (accessdomain.SealedCount, error) {
	out, err := call(ctx, x.r, opAccessSealedKeys, nil, accessSealedArgs{})
	if out.Values == nil && err == nil {
		// An empty count is an empty map, as the domain answers it.
		out.Values = secrets.KeyCounts{}
	}
	return out, err
}

// AccessWrites is the access log's writes, the engine's alone, answered by a
// data node whose copy serves. Each write's position is this node's next floor
// on the log, so the read that follows includes it.
type AccessWrites struct {
	r      *Router
	sealer *accessdomain.Sealer
}

// AccessWrites answers the access log's writes, a command's text sealed with
// sealer, this node's keyring, before it leaves the node.
func (r *Router) AccessWrites(sealer *accessdomain.Sealer) AccessWrites {
	return AccessWrites{r: r, sealer: sealer}
}

// Tell records what one turn of a seat was told.
func (x AccessWrites) Tell(ctx context.Context, c accessdomain.Call,
	n accessdomain.NewTold) (statelog.Result, error) {
	return x.written(call(ctx, x.r, opAccessTell, nil, accessTellArgs{Call: c, Told: n}))
}

// Issued records one SSH certificate the engine signed.
func (x AccessWrites) Issued(ctx context.Context, c accessdomain.Call,
	n accessdomain.NewCertificate) (statelog.Result, error) {
	return x.written(call(ctx, x.r, opAccessIssued, nil, accessIssuedArgs{Call: c, Certificate: n}))
}

// Ran records one SSH command the engine ran or a person denied, its text
// sealed here.
func (x AccessWrites) Ran(ctx context.Context, c accessdomain.Call,
	n accessdomain.NewCommand) (statelog.Result, error) {
	if x.sealer == nil {
		return statelog.Result{}, errors.New("estate: recording a command needs this node's keyring")
	}
	sealed, err := x.sealer.SealCommand(n.Handle, n.ID, n.Command)
	if err != nil {
		return statelog.Result{}, err
	}
	n.Command = ""
	return x.written(call(ctx, x.r, opAccessRan, nil, accessRanArgs{Call: c, Command: n, Sealed: sealed}))
}

// Verdict records one verdict on a delegated request, what it decided and its
// giver's words sealed here.
func (x AccessWrites) Verdict(ctx context.Context, c accessdomain.Call,
	n accessdomain.NewVerdict) (statelog.Result, error) {
	if x.sealer == nil {
		return statelog.Result{}, errors.New("estate: recording a verdict needs this node's keyring")
	}
	sealed, err := x.sealer.SealVerdict(n.Handle, accessdomain.VerdictID(n.Request, n.By), n.Subject,
		n.Reason, n.Findings)
	if err != nil {
		return statelog.Result{}, err
	}
	n.Subject, n.Reason, n.Findings = "", "", nil
	return x.written(call(ctx, x.r, opAccessVerdict, nil, accessVerdictArgs{Call: c, Verdict: n, Sealed: sealed}))
}

// written observes a write's position for the next read's floor and answers it.
func (x AccessWrites) written(out statelog.Result, err error) (statelog.Result, error) {
	x.r.Observe(out.Position)
	return out, err
}
