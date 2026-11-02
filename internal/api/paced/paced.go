// Package paced is THE PER-MEBIBYTE PACE A CLIENT'S CONNECTION IS HELD TO,
// both ways: a request's body read, and a response's body written, each
// mebibyte inside the object store's one pace.
//
// # Why every streamed body needs it
//
// The server's ReadTimeout and WriteTimeout are deliberately unset
// (cmd/crewlet's apiIdleTimeout says why), so a route that streams a body
// bounds it itself: unbounded, a client could trickle a gibibyte upload a byte
// at a time, or open a download and never read it, and hold a handler and a
// connection slot as long as it liked. One deadline over the whole body
// cannot be the bound either, because a file may be a gibibyte and so can a
// push, and any single figure is either a cap on real transfers or no bound on
// a trickle.
//
// # The pace is the object store's, and only the client's time is charged
//
// [Pace] is [objstore.MiBPace], THE object store's one floor, which every leg
// a file's bytes cross is held to, the client's included, so no leg can be
// retuned out from under another. And it is CHARGED ONLY FOR THE TIME THE
// CLIENT IS WAITED ON, never the server's own: storing what arrived can take a
// broker's publish acknowledgements or a bucket's part upload, and fetching
// what a download sends can wait on the store, and neither is the client's
// time to spend.
//
// Three shapes, one pace: [Body] for a request read as the server needs it,
// [Writer] for a response written as it is produced, and [Send] for a stored
// file handed back whole — which is a Writer fed the file's pieces as the
// store hands them over.
//
// ONE PACKAGE for every surface that streams a body to or from a client — a
// project's files, an artifact's uploads and downloads, and the code host's
// git requests and responses — so two surfaces cannot hold one client to two
// paces, and two copies of the rule cannot drift (ADR-0008).
// TestOnePacingImplementation refuses a deadline set on a client's connection
// anywhere else under internal/api or internal/code/host, the two places a
// client's connection is served. The one other such deadline there is
// httpjson.ReadBody's, which bounds a small body as a whole and is not a pace.
//
// # Measured
//
// On four vCPUs of a 2.8 GHz Xeon shared with other builds, with Go 1.27, in
// ONE SESSION of bench_test.go's BenchmarkConnection — five rounds of three
// seconds an arm, each round also sending through Send with the piece sizes
// [sendPiece] was chosen against, interleaved so that a drift in the
// machine's load lands on every arm alike: over a loopback connection, 64
// MiB a response, a Writer fed 64 KiB writes — a git response's shape —
// moves 1.69–2.18 GB/s against 1.70–2.68 GB/s for the same writes made
// plainly (medians 1.80 and 2.12): the deadline, the two clock readings and
// the flush at every write cost about a seventh where nothing but the CPU
// limits the transfer, which no link of 10 Gb/s or less can see. A stored
// file through Send, which copies each piece out of its source first, moves
// 1.22–1.42 GB/s (median 1.35) in those rounds. Send's own cost, into a
// writer that takes every byte at once (BenchmarkSend, five counts of three
// seconds, the same day), is 0.28–0.31 µs and one 16-byte allocation for a
// 4 KiB file, and 6.4–7.8 GB/s for a 64 MiB one.
package paced

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// Pace is how long each mebibyte is given to cross the connection, in either
// direction: a request's to arrive, a response's to be taken by the client.
const Pace = objstore.MiBPace

// ErrBody is a request body that could not be read to its end — the client's
// connection, not the store's, so a route answers it 400 rather than 503.
var ErrBody = errors.New("paced: the request body could not be read")

// Unread is [ErrBody] as the person whose upload it was reads it, on every
// surface that streams a file in.
//
// NEVER THE ERROR ITSELF, whose words after the sentinel are the read's own
// failure as Go and net/http spell it — "unexpected EOF", "read tcp
// 10.0.0.4:443->203.0.113.9:51234: i/o timeout", "a mebibyte of it took more
// than 30s to arrive" — which a surface logs. Every one has the same remedy.
const Unread = "The upload didn't arrive in full, so nothing was stored. " +
	"Send it again."

// Body is a request's body, every mebibyte of it read inside a budget of
// [Pace]: each Read is given what is left of its mebibyte's budget as the
// connection's read deadline, and only the time spent inside Reads is charged
// against it. A failure is [ErrBody]; the end of the body is io.EOF itself.
type Body struct {
	r    io.Reader
	rc   *http.ResponseController
	pace time.Duration

	// now is the clock the time spent inside a Read is measured on, and
	// each read deadline is set from.
	now func() time.Time

	// left is how many bytes of the current mebibyte are still to arrive,
	// and budget how much of its pace is left to spend on them.
	left   int64
	budget time.Duration
}

// NewBody paces a request's body r at [Pace] a mebibyte, measured on now; rc
// carries the read deadlines to the request's connection.
func NewBody(r io.Reader, rc *http.ResponseController, now func() time.Time) *Body {
	return newBody(r, rc, Pace, now)
}

// newBody is [NewBody] at any pace, for a case that shortens it.
func newBody(r io.Reader, rc *http.ResponseController, pace time.Duration, now func() time.Time) *Body {
	return &Body{r: r, rc: rc, pace: pace, now: now}
}

// Read reads the body, inside what is left of the current mebibyte's budget.
//
// THE CALLER DRIVES THE READS — a store asks for the next piece once it has
// done whatever it does with the last one — so the pace is kept HERE, on the
// time spent inside Read, which is the only time the client is the one being
// waited on. Each Read is given the rest of its mebibyte's budget as its
// deadline, and what it took is charged against it; a mebibyte that arrives
// opens the next one's whole budget. A deadline re-armed whole at every Read
// would admit a byte every twenty-nine seconds for ever, and one charged by
// the wall clock would bill the client for the server's own work.
//
// http.ErrNotSupported is ignored, as httpjson.ReadBody ignores it: a
// writer that cannot carry a deadline has no connection to bound — and the
// budget is still kept, so a trickle is refused here even where no deadline
// could be set.
//
// A failure is tagged as the client's ([ErrBody]) — a body cut short, which
// net/http answers as io.ErrUnexpectedEOF, included. The end of the body,
// io.EOF itself, is passed through untouched: the store ends an object only
// there ([objstore.Fill]), and a tagged one would read as a failure.
func (b *Body) Read(p []byte) (int, error) {
	if b.left <= 0 {
		b.left, b.budget = objstore.MiB, b.pace
	}
	if b.budget <= 0 {
		return 0, fmt.Errorf("%w: a mebibyte of it took more than %v to arrive",
			ErrBody, b.pace)
	}
	start := b.now()
	if err := b.rc.SetReadDeadline(start.Add(b.budget)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		return 0, fmt.Errorf("%w: %w", ErrBody, err)
	}
	n, err := b.r.Read(p)
	b.budget -= b.now().Sub(start)
	b.left -= int64(n)
	if b.left <= 0 {
		// A READ THAT CROSSED INTO THE NEXT MEBIBYTE has paid for this
		// one; what it carried past the boundary counts toward the next,
		// which opens with its whole budget.
		b.left, b.budget = objstore.MiB+b.left, b.pace
	}
	if err == nil || err == io.EOF {
		return n, err
	}
	return n, fmt.Errorf("%w: %w", ErrBody, err)
}

// Writer writes to a client at [Pace] a mebibyte, charged only for the time
// spent inside the connection's Write and Flush, FLUSHING AFTER EVERY WRITE.
//
// WHY IT EXISTS beside [Send]: a git process writes its answer into an
// io.Writer as it produces it — a keepalive every five seconds while it
// counts objects, a progress line, the engine's own wait line — and each has
// to reach the client when it is written, or a balancer's idle timeout cuts a
// transfer that is merely working. Send fills a piece before it writes,
// which is right for a file the store hands back and wrong for a stream of
// small writes, which would wait in its buffer until a piece had gathered.
// Both keep one pace, because Send is this Writer fed its pieces.
//
// The accounting is [Body]'s, mirrored: each mebibyte opens a budget of
// Pace; before each write the connection's write deadline is the budget
// left; the time the write and its flush took is charged — the flush's above
// all, since a write of a few bytes lands in net/http's buffer at once and it
// is the flush that waits on the client; a mebibyte taken opens the next
// one's whole budget. http.ErrNotSupported from the controller is ignored (a
// writer with no deadline has no connection to bound) and the budget still
// kept.
//
// ONE DEPARTURE FROM THE MIRROR, because a write is not a read: a Read
// returns whatever has arrived, so one that crosses into the next mebibyte
// never waits for more than the bytes already there, while a Write returns
// only once ALL of p is taken. So a write that would cross a mebibyte's end
// is written as two pieces, each written and flushed under its own
// mebibyte's deadline, the first under what is left of this mebibyte's
// budget and the rest under the next one's — or a client keeping the pace
// exactly, near the end of a mebibyte's budget, would be given the leftover
// of that budget for the next mebibyte's bytes too, and cut.
//
// A response written through a Writer is ENDED through it too: the handler
// defers [Writer.End] as it makes the Writer, so the end of the response —
// which net/http writes itself — is held to what the client has left of its
// pace, measured from the moment the handler returns.
//
// Not safe for concurrent use, as the http.ResponseWriter beneath it is not.
type Writer struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	pace time.Duration

	// now is the clock the time spent inside a write is measured on, and
	// each write deadline is set from.
	now func() time.Time

	// left is how many bytes of the current mebibyte are still to be
	// taken, and budget how much of its pace is left to spend on them.
	left   int64
	budget time.Duration
}

// NewWriter paces writes to w at [Pace] a mebibyte, measured on now.
func NewWriter(w http.ResponseWriter, now func() time.Time) *Writer {
	return &Writer{w: w, rc: http.NewResponseController(w), pace: Pace, now: now}
}

// Write writes p to the client and flushes it, each piece of it — the part
// in one mebibyte — written and flushed inside what is left of that
// mebibyte's budget.
//
// A budget already spent is refused before anything is written, with an
// error naming the pace; so is a write the connection cut at the deadline
// this set (os.ErrDeadlineExceeded), since what that says is the same — the
// client took longer than the pace to take a mebibyte. Any other failure is
// the connection's own, returned as it is.
//
// An empty p writes nothing and still flushes, so a caller can push what it
// wrote through the ResponseWriter directly — its headers — to the client.
func (w *Writer) Write(p []byte) (int, error) {
	written := 0
	for {
		if w.left <= 0 {
			w.left, w.budget = objstore.MiB, w.pace
		}
		if w.budget <= 0 {
			return written, w.tooSlow(nil)
		}
		piece := p[written:]
		last := int64(len(piece)) <= w.left
		if !last {
			piece = piece[:w.left]
		}
		start := w.now()
		if err := w.rc.SetWriteDeadline(start.Add(w.budget)); err != nil &&
			!errors.Is(err, http.ErrNotSupported) {
			return written, err
		}
		n, err := w.w.Write(piece)
		if err == nil && n < len(piece) {
			// A short write with no error breaks io.Writer's contract;
			// io.Copy reads one as io.ErrShortWrite, and so does this.
			err = io.ErrShortWrite
		}
		if err == nil {
			// FLUSHED HERE, under the same deadline and charged to the
			// same mebibyte: the flush is what puts the bytes on the
			// wire, so its time is the client's as much as the write's.
			// Every piece, a crossing write's first among them: left in
			// net/http's buffer, the bytes that end a mebibyte would go
			// out with the next piece, under the next mebibyte's budget.
			if ferr := w.rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
				err = ferr
			}
		}
		w.budget -= w.now().Sub(start)
		w.left -= int64(n)
		written += n
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded):
			return written, w.tooSlow(err)
		case err != nil:
			return written, err
		case last:
			return written, nil
		}
	}
}

// End gives the connection's write deadline what is left of the current
// mebibyte's budget — a whole [Pace] once the last write completed a mebibyte
// — measured from now, for the bytes net/http writes after the handler
// returns.
//
// WHY: net/http writes the end of a response itself once the handler returns
// — a chunked response's terminator — under the write deadline in force, and
// resets it only after. The deadline the last Write set was measured from
// that write's START, so everything the handler did after it — reaping a git
// process, publishing an audit record — would come out of the client's
// budget, which this package never charges: a client on its last mebibyte
// near the floor would be handed a chunked response cut short of its
// terminator, and git reports a fetch or a push that succeeded as a failed
// RPC. A handler therefore defers End as soon as it makes the Writer, so it
// runs as the handler returns, after the last of its own work.
//
// A budget already spent leaves net/http no time at all: the deadline is now,
// and the end of the response is refused, as the next write would have been.
// End reports nothing, because nobody is left to act on it — the handler is
// returning, and net/http answers a write that fails by closing the
// connection whatever this says; http.ErrNotSupported, from a writer with no
// connection to bound, is the common case of that.
func (w *Writer) End() {
	budget := w.budget
	if w.left <= 0 {
		// The last write completed its mebibyte: what follows is the next
		// one's, which opens whole.
		budget = w.pace
	}
	_ = w.rc.SetWriteDeadline(w.now().Add(max(budget, 0)))
}

// tooSlow is the error of a client that took longer than the pace to take a
// mebibyte — cut, when the connection cut it, as cut.
func (w *Writer) tooSlow(cut error) error {
	if cut == nil {
		return fmt.Errorf("paced: the client took more than %v to take a mebibyte", w.pace)
	}
	return fmt.Errorf("paced: the client took more than %v to take a mebibyte: %w", w.pace, cut)
}

// Send writes src to the client through a [Writer], [sendPiece] at a time,
// so each mebibyte is written under its own [Pace] — and the time spent
// waiting on the store for the next piece is never the client's, because the
// Writer charges only the time inside its writes.
//
// Done only where src answers io.EOF itself ([objstore.Fill]): a source cut
// short is a failure, which the caller turns into a cut response rather than
// one ended as if it were whole. The response is ended through the Writer
// ([Writer.End]) whichever way Send returns.
func Send(w http.ResponseWriter, src io.Reader, now func() time.Time) error {
	out := NewWriter(w, now)
	defer out.End()
	piece, _ := sendPieces.Get().(*[]byte)
	defer sendPieces.Put(piece)
	buf := *piece
	for {
		n, end, err := objstore.Fill(src, buf)
		if err != nil {
			return err
		}
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if end {
			return nil
		}
	}
}

// sendPiece is how much of a stored file [Send] reads before it writes — and
// so the memory every download in flight holds for as long as it runs. A
// client at the floor takes a mebibyte every thirty seconds, so a slow
// download holds its piece for hours, and nothing bounds how many run at
// once.
//
// 64 KiB, the size of the pieces a git response is written in
// (gitproc.CopyBuffer). NOT A MEBIBYTE, which it was while the deadline was
// set afresh at every write and the write therefore had to be the unit the
// pace is given per: the Writer now carries each mebibyte's budget across
// writes of any size, so the piece is chosen on what it costs. Measured in
// the session the package doc's figures come from (BenchmarkConnection/send:
// a 64 MiB file over loopback, the three sizes interleaved over five rounds):
// 1.22–1.42 GB/s in 64 KiB pieces (median 1.35), 1.22–1.32 GB/s in 256 KiB
// (1.27) and 1.41–1.66 GB/s in a mebibyte (1.45). The mebibyte's lead —
// fewer write calls — is under a tenth at the median and inside the spread
// of the runs, and it shows only where nothing but the CPU limits the
// transfer: on loopback every size moves about what a 10 Gb/s interface
// carries, so behind a real link — a client's, which is what paces a
// download — the difference is CPU time and never a download's speed. A
// sixteenth of the memory is worth more: a thousand downloads held open by
// slow clients hold 64 MiB rather than a gibibyte. A node whose downloads
// turn out CPU-bound is the measurement that would argue for the mebibyte
// back.
const sendPiece = 64 << 10

// sendPieces keeps [Send]'s pieces between downloads, so a download — an
// attachment of a few kibibytes as much as a gibibyte's file — allocates
// none. One allocated per download — a mebibyte, as Send was first written
// — cost a 4 KiB file 233 µs and a mebibyte of garbage
// (BenchmarkSend/size=4KiB: 1,048,593 B/op); pooled, 0.28–0.31 µs and 16
// B/op.
var sendPieces = sync.Pool{New: func() any {
	piece := make([]byte, sendPiece)
	return &piece
}}
