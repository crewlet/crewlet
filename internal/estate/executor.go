package estate

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The executor log as operations (ADR-0073), reached from whichever node holds
// the seat whose box ran. Only a data node publishes, so without these a
// session captured off one would be an object nothing names, which the
// collector deletes, and the next launch could not continue its conversation.
//
// Every write is the engine's ([originEngine]; package doc, "Who originates a
// write: a surface, or the engine") and an [opIdempotentWrite]: its
// [executordomain.Call] carries the id the domain derives per execution, so an
// unanswered or unvouched write, or an [ErrOutcomeUnknown], is retried under
// that id, never a fresh one. Reads run at the caller's level on this log's
// floor; a launch reads its conversation `linearizable`: a seat that moved
// holds no floor for an end published through another node.

// ExecutorReader is the executor log's read side: exactly the domain reader's
// method set, which [ExecutorReads] answers routed and a data node's own
// reader answers in-process.
type ExecutorReader interface {
	Head(ctx context.Context, handle, conversationKey string,
		fresh statelog.Freshness) (executordomain.Head, error)
	Session(ctx context.Context, handle, turn string,
		fresh statelog.Freshness) (executordomain.SessionRead, error)
	Sessions(ctx context.Context, handle, cursor string, limit int,
		fresh statelog.Freshness) (executordomain.Page, error)
	Search(ctx context.Context, handle string, ask executordomain.SessionSearch,
		fresh statelog.Freshness) (executordomain.SearchPage, error)
	Expired(ctx context.Context, cutoff time.Time, limit int) ([]executordomain.Expiry, error)
	Unended(ctx context.Context, before time.Time, limit int) ([]executordomain.Expiry, error)
	Handles(ctx context.Context, fresh statelog.Freshness) ([]string, error)
	SealedKeys(ctx context.Context) (executordomain.SealedCount, error)
}

// ExecutorWriter is the executor log's write side: exactly the domain writer's
// method set, every one of them the engine's ([originEngine]).
type ExecutorWriter interface {
	Begin(ctx context.Context, c executordomain.Call,
		n executordomain.NewBegin) (executordomain.BeginResult, error)
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

// ExecutorHalf is the executor log a data node serves. A nil half is one this
// node's core runtime has not built, answered like any half a node does not
// run; a nil writer beside a reader is a node that publishes nothing (a
// maintenance mode), whose reads are served and whose writes move on.
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

// A conversation's head, its other ended turns and its siblings, read in one
// transaction at the caller's level: a launch asks `linearizable` (file comment).
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

// One turn's row, and the position it was read at, which a surface states
// beside the row.
var opExecutorSession = define("executor.session", opRead, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorSessionArgs) (executordomain.SessionRead, error) {
		r, err := b.executorReader()
		if err != nil {
			return executordomain.SessionRead{}, err
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

type executorSearchArgs struct {
	Handle string
	Search executordomain.SessionSearch
	Fresh  statelog.Freshness
}

// A search of a seat's sessions runs where the rows are, one page in one read,
// sending back only its hits: a node without `data` is never shipped a seat's
// history to filter. An older data node answers it unknown, unserved, and the
// router asks the next.
var opExecutorSearch = define("executor.search", opRead, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorSearchArgs) (executordomain.SearchPage, error) {
		r, err := b.executorReader()
		if err != nil {
			return executordomain.SearchPage{}, err
		}
		return r.Search(ctx, a.Handle, a.Search, a.Fresh)
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

// Every seat holding a row, or [executordomain.ErrIncomplete] where the node
// may be missing one: never a shorter list, against which a retirement would
// find nothing to remove for a departed seat.
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

type executorBeginArgs struct {
	Call  executordomain.Call
	Begin executordomain.NewBegin
}

// A turn's begin records its first read of its conversation and the base it
// froze, and answers whether the serving node's row holds that base now
// ([executordomain.BeginResult.Held]).
var opExecutorBegin = define("executor.begin", opIdempotentWrite, executorDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a executorBeginArgs) (executordomain.BeginResult, error) {
		w, err := b.executorWriter()
		if err != nil {
			return executordomain.BeginResult{}, err
		}
		return w.Begin(ctx, a.Call, a.Begin)
	}).byEngine()

type executorCaptureArgs struct {
	Call    executordomain.Call
	Capture executordomain.NewCapture
}

// A capture names a session's object for a live turn, and answers whether the
// serving node's row names it now ([executordomain.CaptureResult.Recorded]).
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

// A removed seat's delete is always published, whatever the serving copy holds
// ([executordomain.Writer.DeleteSeat]): what it removes is the log's to decide.
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
// a consumer cannot tell local from routed. They are ordinary reads, which a
// surface may hold as much as the engine may.
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

// Session answers one turn's row, at the level fresh asks for, and where it
// was read.
func (x ExecutorReads) Session(ctx context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.SessionRead, error) {
	return call(ctx, x.r, opExecutorSession, nil, executorSessionArgs{Handle: handle, Turn: turn, Fresh: fresh})
}

// Sessions answers one page of a seat's rows, the latest begun first.
func (x ExecutorReads) Sessions(ctx context.Context, handle, cursor string, limit int,
	fresh statelog.Freshness) (executordomain.Page, error) {
	return call(ctx, x.r, opExecutorSessions, nil,
		executorPageArgs{Handle: handle, Cursor: cursor, Limit: limit, Fresh: fresh})
}

// Search answers one page of a seat's sessions whose words hold every term of
// ask's query, the latest begun first, at the level fresh asks for.
func (x ExecutorReads) Search(ctx context.Context, handle string, ask executordomain.SessionSearch,
	fresh statelog.Freshness) (executordomain.SearchPage, error) {
	return call(ctx, x.r, opExecutorSearch, nil, executorSearchArgs{Handle: handle, Search: ask, Fresh: fresh})
}

// Expired answers what the retention may remove: at most limit things last
// touched at or before cutoff, the longest untouched first.
func (x ExecutorReads) Expired(ctx context.Context, cutoff time.Time, limit int) (
	[]executordomain.Expiry, error) {
	return call(ctx, x.r, opExecutorExpired, nil, executorListingArgs{At: cutoff, Limit: limit})
}

// Unended answers the turns whose end may have been lost: at most limit that
// never ended and whose last record (a capture, or the begin of a turn that
// captured nothing) was at or before before.
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
		// The wire leaves an empty map out: answer the empty map the domain
		// answers in-process, so routed and local stay alike.
		out.Values = secrets.KeyCounts{}
	}
	return out, err
}

// ExecutorWrites is the executor log's writes, answered by a data node whose
// copy serves (this one where it does): exactly the domain writer's method
// set, so a consumer cannot tell local from routed.
//
// The engine's alone ([originEngine]): no package that serves a person or a
// model names it (TestNoSurfaceHoldsTheEnginesExecutorWrites). Each write's
// position becomes this node's floor on the log, so the next read, on whichever
// data node answers, includes it.
type ExecutorWrites struct{ r *Router }

// ExecutorWrites answers the executor log's writes.
func (r *Router) ExecutorWrites() ExecutorWrites { return ExecutorWrites{r: r} }

var _ ExecutorWriter = ExecutorWrites{}

// Begin records a turn's first read of its conversation and the base it froze.
func (x ExecutorWrites) Begin(ctx context.Context, c executordomain.Call,
	n executordomain.NewBegin) (executordomain.BeginResult, error) {
	out, err := call(ctx, x.r, opExecutorBegin, nil, executorBeginArgs{Call: c, Begin: n})
	x.r.Observe(out.Position)
	return out, err
}

// Capture records a new object for a live turn's session.
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

// EndLost records a turn whose end was lost as ended `lost`, for the retention.
func (x ExecutorWrites) EndLost(ctx context.Context, c executordomain.Call, handle, conversationKey,
	turn string, before time.Time) (statelog.Result, error) {
	return x.written(call(ctx, x.r, opExecutorEndLost, nil, executorTurnArgs{Call: c, Handle: handle,
		ConversationKey: conversationKey, Turn: turn, At: before}))
}

// DeleteExpired removes one turn's row and tombstone past the retention.
func (x ExecutorWrites) DeleteExpired(ctx context.Context, c executordomain.Call, handle,
	conversationKey, turn string, cutoff time.Time) (statelog.Result, error) {
	return x.written(call(ctx, x.r, opExecutorDeleteExpired, nil, executorTurnArgs{Call: c, Handle: handle,
		ConversationKey: conversationKey, Turn: turn, At: cutoff}))
}

// ExpireRemoval forgets a removed seat's record of its removal past the
// retention.
func (x ExecutorWrites) ExpireRemoval(ctx context.Context, c executordomain.Call, handle string,
	cutoff time.Time) (statelog.Result, error) {
	return x.written(call(ctx, x.r, opExecutorExpireRemoval, nil,
		executorRemovalArgs{Call: c, Handle: handle, Cutoff: cutoff}))
}

// DeleteSeat removes every row of a seat the company removed, for the mailbox
// retirement.
func (x ExecutorWrites) DeleteSeat(ctx context.Context, c executordomain.Call,
	handle string) (statelog.Result, error) {
	return x.written(call(ctx, x.r, opExecutorDeleteSeat, nil, executorSeatArgs{Call: c, Handle: handle}))
}

// written observes a write's position as the next read's floor.
func (x ExecutorWrites) written(out statelog.Result, err error) (statelog.Result, error) {
	x.r.Observe(out.Position)
	return out, err
}
