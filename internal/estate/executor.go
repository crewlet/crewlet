package estate

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE EXECUTOR LOG AS OPERATIONS: the engine's record of each seat's executor
// sessions (ADR-0073), reached from whichever node holds the seat whose box
// ran. A box runs where its seat is held, a seats-only leaf included, and a
// state-log publisher exists only on a data node, so without these a session
// captured off a data node would be an uploaded object nothing names, which
// the collector deletes a day later — and the next launch there could not read
// the conversation it continues.
//
// # Every write is the engine's
//
// Nobody asks for a session to be captured, a turn to be ended, a session to
// be aged out or a removed seat's to be taken away: the engine records what its
// own runs did. So every write here is ENGINE-ORIGINATED ([originEngine]): the
// log is in no list of the domains a surface writes ([SurfaceWriteDomains]), so
// the person's write surfaces have no refusal of it to class and no object of
// it to name, and its writer facade ([ExecutorWrites]) is the engine's alone,
// which a gate over the packages serving a person or a model holds.
//
// # Each write is the same operation however often it is asked
//
// Every write carries, on its [executordomain.Call], the operation id the
// domain derives PER EXECUTION — a capture's from its turn, its launch and its
// ordinal, an end's from its turn, a retention delete's from its turn and the
// reaper's slot, a removed seat's from the seat and the instant its retirement
// was marked — and the log's ledger answers a repeat with the first copy's
// outcome. So every write is an [opIdempotentWrite]: one some data node never
// answered, or answered unvouched, is asked of the next under the same id, and
// [ErrOutcomeUnknown] is the caller's to retry under it, never under a fresh
// one.
//
// # Reads at the caller's level, on this log's floor
//
// Every read is answered at the level its caller passes and carries this
// node's floor on the executor log, so a read after this node's own write is
// answered by a copy that has applied it. A LAUNCH READS ITS CONVERSATION AT
// `linearizable`: the previous turn's end may have been published through
// another node, and a seat that moved holds no floor for it, so only the read
// index's barrier, on whichever data node answers, holds the head to it. The
// reads are ordinary ones, which a surface may hold ([ExecutorReads]).

// ExecutorReader is the executor log's read side: exactly the domain reader's
// method set, which [ExecutorReads] answers routed and a data node's own
// reader answers in-process.
type ExecutorReader interface {
	Head(ctx context.Context, handle, conversationKey string,
		fresh statelog.Freshness) (executordomain.Head, error)
	Session(ctx context.Context, handle, turn string,
		fresh statelog.Freshness) (executordomain.Session, error)
	Sessions(ctx context.Context, handle, cursor string, limit int,
		fresh statelog.Freshness) (executordomain.Page, error)
	Expired(ctx context.Context, cutoff time.Time, limit int) ([]executordomain.Expiry, error)
	Unended(ctx context.Context, before time.Time, limit int) ([]executordomain.Expiry, error)
	Handles(ctx context.Context, fresh statelog.Freshness) ([]string, error)
	SealedKeys(ctx context.Context) (executordomain.SealedCount, error)
}

// ExecutorWriter is the executor log's write side: exactly the domain writer's
// method set, every one of them the engine's ([originEngine]).
type ExecutorWriter interface {
	Capture(ctx context.Context, c executordomain.Call,
		n executordomain.NewCapture) (executordomain.CaptureResult, error)
	End(ctx context.Context, c executordomain.Call, n executordomain.NewEnd) (statelog.Result, error)
	EndLost(ctx context.Context, c executordomain.Call, handle, conversationKey, turn string,
		before time.Time) (statelog.Result, error)
	DeleteExpired(ctx context.Context, c executordomain.Call, handle, conversationKey, turn string,
		cutoff time.Time) (statelog.Result, error)
	ExpireRemoval(ctx context.Context, c executordomain.Call, handle string,
		cutoff time.Time) (statelog.Result, error)
	DeleteSeat(ctx context.Context, c executordomain.Call, handle string) (statelog.Result, error)
}

// The domain's own sides are what a data node serves the log with.
var (
	_ ExecutorReader = (*executordomain.Reader)(nil)
	_ ExecutorWriter = (*executordomain.Writer)(nil)
)

// ExecutorHalf is the executor log a data node serves: its reader and its
// writer, which every data node runs. A nil half is one this node's core
// runtime has not built, answered like any half a node does not run, and a nil
// writer beside a reader is a node that publishes nothing (a maintenance
// mode), whose reads are served and whose writes move on to a node that
// publishes.
type ExecutorHalf struct {
	Reader ExecutorReader
	Writer ExecutorWriter
}

// executorReader is the half's reader, or [errNoHalf].
func (b Backend) executorReader() (ExecutorReader, error) {
	if b.Executor.Reader == nil {
		return nil, errNoHalf
	}
	return b.Executor.Reader, nil
}

// executorWriter is the half's writer, or [errNoHalf].
func (b Backend) executorWriter() (ExecutorWriter, error) {
	if b.Executor.Writer == nil {
		return nil, errNoHalf
	}
	return b.Executor.Writer, nil
}

// ---- the reads ------------------------------------------------------------ //

type executorHeadArgs struct {
	Handle          string
	ConversationKey string
	Fresh           statelog.Freshness
}

// A CONVERSATION'S HEAD, its other ended turns and its siblings, read in one
// transaction on the node that answers, at the level the caller asked: a
// launch asks `linearizable` (see the file comment).
var opExecutorHead = define("executor.head", opRead, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorHeadArgs) (executordomain.Head, error) {
		r, err := b.executorReader()
		if err != nil {
			return executordomain.Head{}, err
		}
		return r.Head(ctx, a.Handle, a.ConversationKey, a.Fresh)
	})

type executorSessionArgs struct {
	Handle string
	Turn   string
	Fresh  statelog.Freshness
}

var opExecutorSession = define("executor.session", opRead, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorSessionArgs) (executordomain.Session, error) {
		r, err := b.executorReader()
		if err != nil {
			return executordomain.Session{}, err
		}
		return r.Session(ctx, a.Handle, a.Turn, a.Fresh)
	})

type executorPageArgs struct {
	Handle string
	Cursor string
	Limit  int
	Fresh  statelog.Freshness
}

var opExecutorSessions = define("executor.sessions", opRead, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorPageArgs) (executordomain.Page, error) {
		r, err := b.executorReader()
		if err != nil {
			return executordomain.Page{}, err
		}
		return r.Sessions(ctx, a.Handle, a.Cursor, a.Limit, a.Fresh)
	})

// executorListingArgs is one of the retention duty's listings: the instant it
// is cut at, and the most it nominates.
type executorListingArgs struct {
	At    time.Time
	Limit int
}

var opExecutorExpired = define("executor.expired", opRead, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorListingArgs) ([]executordomain.Expiry, error) {
		r, err := b.executorReader()
		if err != nil {
			return nil, err
		}
		return r.Expired(ctx, a.At, a.Limit)
	})

var opExecutorUnended = define("executor.unended", opRead, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorListingArgs) ([]executordomain.Expiry, error) {
		r, err := b.executorReader()
		if err != nil {
			return nil, err
		}
		return r.Unended(ctx, a.At, a.Limit)
	})

type executorHandlesArgs struct {
	Fresh statelog.Freshness
}

// EVERY SEAT HOLDING A ROW, or [executordomain.ErrIncomplete] where the node
// that answers may be missing one: never a shorter list, which a retirement
// would match a departed seat against and find nothing to remove.
var opExecutorHandles = define("executor.handles", opRead, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorHandlesArgs) ([]string, error) {
		r, err := b.executorReader()
		if err != nil {
			return nil, err
		}
		return r.Handles(ctx, a.Fresh)
	})

// executorSealedArgs carries nothing: the count is the whole log's.
type executorSealedArgs struct{}

var opExecutorSealedKeys = define("executor.sealed_keys", opRead, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, _ executorSealedArgs) (executordomain.SealedCount, error) {
		r, err := b.executorReader()
		if err != nil {
			return executordomain.SealedCount{}, err
		}
		return r.SealedKeys(ctx)
	})

// ---- the writes: the engine's, each under the operation it carries ---------- //

type executorCaptureArgs struct {
	Call    executordomain.Call
	Capture executordomain.NewCapture
}

// A CAPTURE names a session's object for a live turn, under the operation its
// turn, launch and ordinal derive, and answers whether the row the serving
// node holds names it now ([executordomain.CaptureResult.Recorded]), read back
// on that node.
var opExecutorCapture = define("executor.capture", opIdempotentWrite, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorCaptureArgs) (executordomain.CaptureResult, error) {
		w, err := b.executorWriter()
		if err != nil {
			return executordomain.CaptureResult{}, err
		}
		return w.Capture(ctx, a.Call, a.Capture)
	}).byEngine()

type executorEndArgs struct {
	Call executordomain.Call
	End  executordomain.NewEnd
}

var opExecutorEnd = define("executor.end", opIdempotentWrite, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorEndArgs) (statelog.Result, error) {
		w, err := b.executorWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.End(ctx, a.Call, a.End)
	}).byEngine()

// executorTurnArgs is a retention write about one turn the duty listed: the
// seat, the conversation and the turn, and the instant the listing was cut at.
type executorTurnArgs struct {
	Call            executordomain.Call
	Handle          string
	ConversationKey string
	Turn            string
	At              time.Time
}

var opExecutorEndLost = define("executor.end_lost", opIdempotentWrite, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorTurnArgs) (statelog.Result, error) {
		w, err := b.executorWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.EndLost(ctx, a.Call, a.Handle, a.ConversationKey, a.Turn, a.At)
	}).byEngine()

var opExecutorDeleteExpired = define("executor.delete_expired", opIdempotentWrite, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorTurnArgs) (statelog.Result, error) {
		w, err := b.executorWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.DeleteExpired(ctx, a.Call, a.Handle, a.ConversationKey, a.Turn, a.At)
	}).byEngine()

type executorRemovalArgs struct {
	Call   executordomain.Call
	Handle string
	Cutoff time.Time
}

var opExecutorExpireRemoval = define("executor.expire_removal", opIdempotentWrite, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorRemovalArgs) (statelog.Result, error) {
		w, err := b.executorWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.ExpireRemoval(ctx, a.Call, a.Handle, a.Cutoff)
	}).byEngine()

type executorSeatArgs struct {
	Call   executordomain.Call
	Handle string
}

// A REMOVED SEAT'S DELETE is always published, whatever the serving node's
// copy holds ([executordomain.Writer.DeleteSeat]), so where it lands is the
// log's, never the copy's that took the request.
var opExecutorDeleteSeat = define("executor.delete_seat", opIdempotentWrite, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorSeatArgs) (statelog.Result, error) {
		w, err := b.executorWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.DeleteSeat(ctx, a.Call, a.Handle)
	}).byEngine()

// ---- the asking half ---------------------------------------------------- //

// ExecutorReads is the executor log's reads, answered by a data node whose copy
// serves (this one where it does): exactly the domain reader's method set, so
// a consumer cannot tell local from routed.
//
// AN ORDINARY READ, which a surface may hold — a person's view of a seat's
// sessions, a run's search of its own — as much as the engine's launch and its
// retention may.
type ExecutorReads struct{ r *Router }

// ExecutorReads answers the executor log's reads.
func (r *Router) ExecutorReads() ExecutorReads { return ExecutorReads{r: r} }

var _ ExecutorReader = ExecutorReads{}

// Head answers a conversation's head, its other ended turns and its siblings,
// at the level fresh asks for: a launch's is `linearizable`, because the end
// it must include may be another node's write.
func (x ExecutorReads) Head(ctx context.Context, handle, conversationKey string,
	fresh statelog.Freshness) (executordomain.Head, error) {
	return call(ctx, x.r, opExecutorHead, nil,
		executorHeadArgs{Handle: handle, ConversationKey: conversationKey, Fresh: fresh})
}

// Session answers one turn's row, at the level fresh asks for.
func (x ExecutorReads) Session(ctx context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.Session, error) {
	return call(ctx, x.r, opExecutorSession, nil, executorSessionArgs{Handle: handle, Turn: turn, Fresh: fresh})
}

// Sessions answers one page of a seat's rows, the latest begun first.
func (x ExecutorReads) Sessions(ctx context.Context, handle, cursor string, limit int,
	fresh statelog.Freshness) (executordomain.Page, error) {
	return call(ctx, x.r, opExecutorSessions, nil,
		executorPageArgs{Handle: handle, Cursor: cursor, Limit: limit, Fresh: fresh})
}

// Expired answers what the retention may remove: at most limit things last
// touched at or before cutoff, the longest untouched first.
func (x ExecutorReads) Expired(ctx context.Context, cutoff time.Time, limit int) (
	[]executordomain.Expiry, error) {
	return call(ctx, x.r, opExecutorExpired, nil, executorListingArgs{At: cutoff, Limit: limit})
}

// Unended answers the turns whose end may have been lost: at most limit that
// never ended and were last captured at or before before.
func (x ExecutorReads) Unended(ctx context.Context, before time.Time, limit int) (
	[]executordomain.Expiry, error) {
	return call(ctx, x.r, opExecutorUnended, nil, executorListingArgs{At: before, Limit: limit})
}

// Handles answers every seat holding a row, in order, at the level fresh asks
// for.
func (x ExecutorReads) Handles(ctx context.Context, fresh statelog.Freshness) ([]string, error) {
	return call(ctx, x.r, opExecutorHandles, nil, executorHandlesArgs{Fresh: fresh})
}

// SealedKeys answers how many sessions sit sealed under each keyring key.
func (x ExecutorReads) SealedKeys(ctx context.Context) (executordomain.SealedCount, error) {
	out, err := call(ctx, x.r, opExecutorSealedKeys, nil, executorSealedArgs{})
	if out.Values == nil && err == nil {
		// AN EMPTY COUNT IS AN EMPTY MAP, as the domain answers it: the
		// wire leaves an empty map out, and an answer that came back from a
		// peer as no map at all would be one a caller could tell from the
		// same answer read in-process.
		out.Values = secrets.KeyCounts{}
	}
	return out, err
}

// ExecutorWrites is the executor log's writes, answered by a data node whose
// copy serves (this one where it does): exactly the domain writer's method
// set, so a consumer cannot tell local from routed.
//
// THE ENGINE'S ALONE ([originEngine]): it is for the engine's record of what
// its own runs did — the executor's captures and ends, the retention's deletes
// and a removed seat's — and no package that serves a person or a model names
// it, which the gate over those packages holds. Each write's position is this
// node's next floor on the log, so the read that follows it, on whichever data
// node answers, includes it.
type ExecutorWrites struct{ r *Router }

// ExecutorWrites answers the executor log's writes.
func (r *Router) ExecutorWrites() ExecutorWrites { return ExecutorWrites{r: r} }

var _ ExecutorWriter = ExecutorWrites{}

// Capture records a new object for a live turn's session. Asked again of the
// next data node, under the operation its call carries, wherever one did not
// answer — and so is every write below.
func (x ExecutorWrites) Capture(ctx context.Context, c executordomain.Call,
	n executordomain.NewCapture) (executordomain.CaptureResult, error) {
	out, err := call(ctx, x.r, opExecutorCapture, nil, executorCaptureArgs{Call: c, Capture: n})
	x.r.Observe(out.Position)
	return out, err
}

// End records how a turn ended, and tombstones it.
func (x ExecutorWrites) End(ctx context.Context, c executordomain.Call,
	n executordomain.NewEnd) (statelog.Result, error) {
	return x.written(call(ctx, x.r, opExecutorEnd, nil, executorEndArgs{Call: c, End: n}))
}

// EndLost records a turn whose end was lost as ended `lost`, as the retention
// duty.
func (x ExecutorWrites) EndLost(ctx context.Context, c executordomain.Call, handle, conversationKey,
	turn string, before time.Time) (statelog.Result, error) {
	return x.written(call(ctx, x.r, opExecutorEndLost, nil, executorTurnArgs{Call: c, Handle: handle,
		ConversationKey: conversationKey, Turn: turn, At: before}))
}

// DeleteExpired removes one turn's row and tombstone past the retention, as
// the retention duty.
func (x ExecutorWrites) DeleteExpired(ctx context.Context, c executordomain.Call, handle,
	conversationKey, turn string, cutoff time.Time) (statelog.Result, error) {
	return x.written(call(ctx, x.r, opExecutorDeleteExpired, nil, executorTurnArgs{Call: c, Handle: handle,
		ConversationKey: conversationKey, Turn: turn, At: cutoff}))
}

// ExpireRemoval forgets a removed seat's record of its removal past the
// retention, as the retention duty.
func (x ExecutorWrites) ExpireRemoval(ctx context.Context, c executordomain.Call, handle string,
	cutoff time.Time) (statelog.Result, error) {
	return x.written(call(ctx, x.r, opExecutorExpireRemoval, nil,
		executorRemovalArgs{Call: c, Handle: handle, Cutoff: cutoff}))
}

// DeleteSeat removes every row a seat the company removed holds, as the
// mailbox retirement.
func (x ExecutorWrites) DeleteSeat(ctx context.Context, c executordomain.Call,
	handle string) (statelog.Result, error) {
	return x.written(call(ctx, x.r, opExecutorDeleteSeat, nil, executorSeatArgs{Call: c, Handle: handle}))
}

// written observes a write's position for the next read's floor and answers
// it.
func (x ExecutorWrites) written(out statelog.Result, err error) (statelog.Result, error) {
	x.r.Observe(out.Position)
	return out, err
}
