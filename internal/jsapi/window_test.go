package jsapi_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
)

// pieceBytes is one object-store piece, 128 KiB, spelled here rather than read
// from the package that cuts objects by it.
const pieceBytes = 128 << 10

// deliveryBytes is what a consumer's delivery of one piece carries: the piece
// and four KiB the broker counts beside it.
const deliveryBytes = pieceBytes + 4<<10

// floorCrossing is how long n bytes take to cross a link at the floor every
// leg is promised ([objstore.MiBPace]).
func floorCrossing(n int) time.Duration {
	return time.Duration(int64(n) * int64(objstore.MiBPace) / objstore.MiB)
}

// A LINK'S WINDOW HOLDS BOTH DIRECTIONS WITHIN A REQUEST'S TERM.
//
// A request waits behind what the node has out on a link both ways. So the
// ONE window crosses the floor within what a request's term leaves after its
// own round trip, spelled here from the two terms and the floor; a put's piece
// and a download's are never out together, since two cross past it; takes are
// let out first come first served whichever way they go; and a take larger
// than the window is let out only alone, nothing beside it or out of turn.
//
// Mutation: let a take larger than the window out beside the others, and this
// goes red.
func TestALinksWindowHoldsBothDirectionsWithinARequestsTerm(t *testing.T) {
	t.Parallel()
	budget := jsapi.RequestTerm - jsprovision.ReadTerm
	w := jsapi.NewLinkWindow()
	if c := floorCrossing(w.Size()); c > budget {
		t.Errorf("a link's window of %d bytes crosses the floor in %v, past the %v a request's %v term "+
			"leaves after its own round trip", w.Size(), c, budget, jsapi.RequestTerm)
	}
	if c := floorCrossing(w.Size() + 1); c <= budget {
		t.Errorf("a link's window of %d bytes is not the widest within %v: a byte more crosses in %v",
			w.Size(), budget, c)
	}
	up, down := jsapi.RequestBytes(pieceBytes), deliveryBytes
	if up > w.Size() || down > w.Size() {
		t.Fatalf("a put's piece (%d bytes) or a download's (%d) does not fit a window of %d alone",
			up, down, w.Size())
	}
	if c := floorCrossing(up + down); c <= budget {
		t.Fatalf("a piece each way crosses the floor in %v, within %v: the case needs two to cross "+
			"past it", c, budget)
	}

	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		order := make(chan string, 8)
		take := func(name string, n int) {
			go func() {
				if err := w.Take(ctx, n); err != nil {
					if ctx.Err() == nil {
						t.Errorf("%s: %v", name, err)
					}
					return
				}
				order <- name
			}()
			synctest.Wait()
		}
		// expect is the takes let out since the last look, in any order:
		// two let out at once race to say so.
		expect := func(want ...string) {
			t.Helper()
			synctest.Wait()
			due := map[string]int{}
			for _, name := range want {
				due[name]++
			}
			for range want {
				select {
				case got := <-order:
					if due[got] == 0 {
						t.Fatalf("%s was let out where %v were due", got, want)
					}
					due[got]--
				default:
					t.Fatalf("of %v, not all were let out", want)
				}
			}
			select {
			case got := <-order:
				t.Fatalf("%s was let out beside what the window holds", got)
			default:
			}
		}

		take("a put's piece", up)
		expect("a put's piece")
		take("a download's piece", down)
		take("a replay pull", w.Size())
		take("a second put's piece", up)
		expect()
		if held, waiting := w.Held(); held != up || waiting != 3 {
			t.Fatalf("the window holds %d bytes with %d waiting, want one put's piece (%d) and three",
				held, waiting, up)
		}
		w.Give(up)
		expect("a download's piece")
		w.Give(down)
		expect("a replay pull")
		w.Give(w.Size())
		expect("a second put's piece")

		// A TAKE LARGER THAN THE WINDOW, a replay pull of one row at the
		// transport's bound: let out once the window is empty, held alone,
		// and never overtaken by a smaller take that would fit beside the
		// first.
		over := 8 << 20
		take("a row at the transport's bound", over)
		take("a walk's pull", 4<<10)
		expect()
		w.Give(up)
		expect("a row at the transport's bound")
		take("a download's piece", down)
		expect()
		w.Give(over)
		expect("a walk's pull", "a download's piece")
		w.Give(4 << 10)
		w.Give(down)
		if held, waiting := w.Held(); held != 0 || waiting != 0 {
			t.Fatalf("every take given back, the window holds %d bytes with %d waiting", held, waiting)
		}
	})
}

// A TAKE WHOSE CALLER GIVES UP LEAVES ITS PLACE TO THE TAKES BEHIND IT, and
// one let out as its context ended is given back, so nothing a withdrawn take
// asked for is held for ever.
func TestATakeWithdrawnLetsTheTakesBehindItOut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := jsapi.NewLinkWindow()
		if err := w.Take(t.Context(), w.Size()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancelCause(t.Context())
		big := make(chan error, 1)
		go func() { big <- w.Take(ctx, 8<<20) }()
		synctest.Wait()
		small := make(chan error, 1)
		go func() { small <- w.Take(t.Context(), 1) }()
		synctest.Wait()
		cause := errors.New("the caller gave up")
		cancel(cause)
		if err := <-big; !errors.Is(err, cause) {
			t.Fatalf("a withdrawn take answered %v, want its context's cause", err)
		}
		w.Give(w.Size())
		synctest.Wait()
		select {
		case err := <-small:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("the take behind a withdrawn one was never let out")
		}
		if err := w.Take(t.Context(), 0); err == nil {
			t.Fatal("a take of no bytes was let out")
		}
	})
	if _, err := jsapi.NewWindow(0); err == nil {
		t.Fatal("a window of no bytes was made")
	}
}

// A TAKE GIVEN BACK TWICE IS A BUG, NEVER ROOM: the window panics rather than
// let out more than its size behind it.
//
// Mutation: clamp what is held at zero, and the second give lets a take out
// beside one already holding the window.
func TestAWindowGivenBackMoreThanItHoldsPanics(t *testing.T) {
	t.Parallel()
	w := jsapi.NewLinkWindow()
	if err := w.Take(t.Context(), deliveryBytes); err != nil {
		t.Fatal(err)
	}
	w.Give(deliveryBytes)
	defer func() {
		if recover() == nil {
			t.Fatal("a take given back twice was taken as room")
		}
	}()
	w.Give(deliveryBytes)
}
