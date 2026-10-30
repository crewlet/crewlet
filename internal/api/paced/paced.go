// Package paced bounds a file's bytes on the API listener: an upload's body
// read, and a download's body written, each mebibyte inside the object
// store's one pace.
//
// # Why every streamed body needs it
//
// The server's ReadTimeout and WriteTimeout are deliberately unset (cmd/crewlet's
// apiIdleTimeout says why), so a route that streams a body bounds it itself:
// unbounded, a client could trickle a gibibyte upload a byte at a time, or open
// a download and never read it, and hold a handler and a connection slot as
// long as it liked. One deadline over the whole body cannot be the bound
// either, because a file may be a gibibyte and any single figure is either a
// cap on real uploads or no bound on a trickle.
//
// # The pace is the object store's, and only the client's time is charged
//
// [Pace] is [objstore.MiBPace], THE object store's one floor, which every leg a
// file's bytes cross is held to, the client's included, so no leg can be
// retuned out from under another. And it is CHARGED ONLY FOR THE TIME THE
// CLIENT IS WAITED ON, never the server's own: storing what arrived can take a
// broker's publish acknowledgements or a bucket's part upload, and fetching
// what a download sends can wait on the store, and neither is the client's
// time to spend.
//
// ONE PACKAGE for every surface that streams a file (a project's files, an
// artifact's uploads and downloads), so two surfaces cannot hold one client to
// two paces.
package paced

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// Pace is how long each mebibyte of a body is given to cross the connection,
// in either direction: an upload's to arrive, a download's to be taken by the
// client.
const Pace = objstore.MiBPace

// ErrBody is an upload whose body could not be read to its end: the client's
// connection, not the store, so a surface answers it 400 rather than 503.
var ErrBody = errors.New("the body could not be read")

// Unread is [ErrBody] as the person whose upload it was reads it, on every
// surface that streams a file in.
//
// NEVER THE ERROR ITSELF, whose words after the sentinel are the read's own
// failure as Go and net/http spell it — "unexpected EOF", "read tcp
// 10.0.0.4:443->203.0.113.9:51234: i/o timeout", "a mebibyte of it took more
// than 30s to arrive" — which a surface logs. Every one has the same remedy.
const Unread = "The upload didn't arrive in full, so nothing was stored. " +
	"Send it again."

// Body is an upload's body, every mebibyte of it read inside a budget of
// [Pace]; see [Body.Read].
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

// NewBody paces r at [Pace] a mebibyte, measured on now; rc carries the read
// deadlines to the request's connection.
func NewBody(r io.Reader, rc *http.ResponseController, now func() time.Time) *Body {
	return newBody(r, rc, Pace, now)
}

// newBody is [NewBody] at any pace, for a case that shortens it.
func newBody(r io.Reader, rc *http.ResponseController, pace time.Duration, now func() time.Time) *Body {
	return &Body{r: r, rc: rc, pace: pace, now: now}
}

// Read reads the body, inside what is left of the current mebibyte's budget.
//
// THE STORE DRIVES THE READS (it asks for the next piece once it has done
// whatever it does with the last one), so the pace is kept HERE, on the time
// spent inside Read, which is the only time the client is the one being waited
// on. Each Read is given the rest of its mebibyte's budget as its deadline,
// and what it took is charged against it; a mebibyte that arrives opens the
// next one's whole budget. A deadline re-armed whole at every Read would admit
// a byte every twenty-nine seconds for ever, and one charged by the wall clock
// would bill the client for the store's own work.
//
// http.ErrNotSupported is ignored, as httpjson.ReadBody ignores it: a writer
// that cannot carry a deadline has no connection to bound, and the budget is
// still kept, so a trickle is refused here even where no deadline could be
// set.
//
// A failure is tagged [ErrBody], the client's (a body cut short, which net/http
// answers as io.ErrUnexpectedEOF, included). The end of the body, io.EOF
// itself, is passed through untouched: the store ends an object only there
// ([objstore.Fill]), and a tagged one would read as a failure.
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

// sendUnit is how much of a download one write hands the client, each under
// its own [Pace] deadline: a mebibyte, the unit the pace is given per.
const sendUnit = objstore.MiB

// Send writes src to the client [sendUnit] at a time, each under its own
// [Pace] write deadline, set from now once the unit is in hand, so the time
// spent waiting on the store for it is never the client's.
//
// Done only where src answers io.EOF itself ([objstore.Fill]): a source cut
// short is a failure, which the caller turns into a cut response rather than
// one ended as if it were whole.
func Send(w http.ResponseWriter, src io.Reader, now func() time.Time) error {
	rc := http.NewResponseController(w)
	buf := make([]byte, sendUnit)
	for {
		n, end, err := objstore.Fill(src, buf)
		if err != nil {
			return err
		}
		if n > 0 {
			if derr := rc.SetWriteDeadline(now().Add(Pace)); derr != nil &&
				!errors.Is(derr, http.ErrNotSupported) {
				return derr
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if end {
			return nil
		}
	}
}
