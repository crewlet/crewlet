package redact

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
)

// asking is a live stream whose every ask of Settled's readers is counted:
// the bytes they were handed to read ([settle]), and the longest text asked of.
type asking struct {
	s             *Stream
	out           bytes.Buffer
	read, longest int
}

func newAsking() *asking {
	a := &asking{}
	a.s = NewLiveStream(&a.out)
	a.s.live.view.ask = func(text string) int {
		a.longest = max(a.longest, len(text))
		n, read := settle(text)
		a.read += read
		return n
	}
	return a
}

// heldRun is n bytes at least of a table's cells, each a password's key
// decided by the next line: a run Settled holds back whole.
func heldRun(n int) string {
	const line = "| Password: |\n"
	return strings.Repeat(line, n/len(line)+1)
}

// boundText is a text a live stream is read over, and whether what it holds
// unsettled passes [liveHeldBytes].
type boundText struct {
	name, text string
	trails     bool
}

// boundTexts are texts a live stream holds past its bound, and texts it never
// does: a plain log, and a run of held lines just short of the bound.
func boundTexts() []boundText {
	ok := strings.Repeat("ok\n", 1000)
	b64 := strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9Q", 1<<13)
	return []boundText{
		{"a run of held lines two bounds long", ok + heldRun(2*liveHeldBytes) + "hunter2secretvalue9\nok\n" + ok, true},
		{"a base64 line of 512 KiB", ok + "data: " + b64 + "\npassword: hunter2x9\n" + ok, true},
		{"an armour, then a run of held lines past the bound", "k: " + testBegin + "\n" + testKeyLine + "\n\n" + heldRun(liveHeldBytes+64<<10) + "hunter2secretvalue9\nok\n", true},
		{"a plain log of 1 MiB", strings.Repeat("2026-11-19T10:00:00Z INFO build step 12 of 40 passed in 0.412s\n", 1<<20/64), false},
		{"a run of held lines just short of the bound", ok + heldRun(liveHeldBytes-80<<10) + "hunter2secretvalue9\nok\n" + ok, false},
	}
}

// A LIVE STREAM HOLDS AND REREADS NO MORE THAN ITS BOUND: over [boundTexts],
// written in pieces of up to 64 KiB and once whole, after every write it holds
// at most [liveHeldBytes] past its settled point, asks Settled of no text
// longer than twice that, and its asks read at most twelve times the write
// and the bound: six reads of each text asked, which holds the held text and
// a piece of the write at most ([Stream.settleIn]). Counted in bytes the
// readers are handed, never timed.
//
// Mutation: never trail, and a run of held lines is held and reread past the
// bound; take a write whole rather than a bound at a time, and a text written
// whole is asked of at its whole length.
func TestALiveStreamHoldsAndRereadsNoMoreThanItsBound(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 50))
	for _, tx := range boundTexts() {
		for _, whole := range []bool{false, true} {
			a := newAsking()
			for at := 0; at < len(tx.text); {
				n := min(len(tx.text)-at, 1+r.IntN(64<<10))
				if whole {
					n = len(tx.text)
				}
				before := a.read
				_, _ = a.s.Write([]byte(tx.text[at : at+n]))
				at += n
				switch {
				case len(a.s.live.raw.text) > liveHeldBytes:
					t.Fatalf("%s (whole %v): after %d bytes the stream holds %d unsettled, past %d", tx.name, whole, at, len(a.s.live.raw.text), liveHeldBytes)
				case a.longest > 2*liveHeldBytes:
					t.Fatalf("%s (whole %v): after %d bytes the stream asked Settled of %d bytes, past twice %d", tx.name, whole, at, a.longest, liveHeldBytes)
				case a.read-before > 12*(n+liveHeldBytes):
					t.Fatalf("%s (whole %v): a write of %d bytes had Settled's readers read %d, past twelve times it and %d", tx.name, whole, n, a.read-before, liveHeldBytes)
				}
			}
			if err := a.s.Close(); err != nil || a.out.String() != Secrets(tx.text) {
				t.Errorf("%s (whole %v): Close answered %v, and the stream wrote another text than Secrets %s", tx.name, whole, err, parted(a.out.String(), Secrets(tx.text)))
			}
		}
	}
}

// A LIVE STREAM PAST ITS BOUND TRAILS, SAYS SO, AND WRITES SECRETS: over
// [boundTexts] in pieces of up to 64 KiB, flushed after every write or never,
// a text held past [liveHeldBytes] trails ([Stream.Trailing]) and no other
// does; untrailing, a flush answers at most the bound held, and trailing, the
// text not yet written, at the first more than none. What a flush answers
// final never falls and is Secrets' prefix, and Close answers nil having
// written [Secrets], all of it final.
//
// Mutation: decide the stages as at an end at a flush while trailing, and a
// held run is written otherwise than Secrets; trail at half the bound, and a
// run short of it trails; answer Trailing false, and the stream says nothing.
func TestALiveStreamPastItsBoundTrailsSaysSoAndWritesSecrets(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 51))
	for _, tx := range boundTexts() {
		want := Secrets(tx.text)
		for _, odds := range []int{1, 0} {
			var out bytes.Buffer
			s := NewLiveStream(&out)
			var final int64
			trailed, saidHeld := false, false
			for at := 0; at < len(tx.text); {
				n := min(len(tx.text)-at, 1+r.IntN(64<<10))
				_, _ = s.Write([]byte(tx.text[at : at+n]))
				at += n
				if trailed && !s.Trailing() {
					t.Fatalf("%s (odds %d): after %d bytes the stream no longer trails", tx.name, odds, at)
				}
				if odds == 0 || r.IntN(odds) != 0 {
					trailed = s.Trailing()
					continue
				}
				f, held, err := s.Flush()
				switch {
				case err != nil:
					t.Fatalf("%s (odds %d): a flush at %d answered %v", tx.name, odds, at, err)
				case f < final || f > int64(out.Len()) || out.String()[:f] != want[:min(int(f), len(want))]:
					t.Fatalf("%s (odds %d): a flush at %d answered %d final after %d, of %d written, not Secrets' prefix", tx.name, odds, at, f, final, out.Len())
				case !s.Trailing() && held > liveHeldBytes:
					t.Fatalf("%s (odds %d): a flush at %d answered %d held without trailing, past %d", tx.name, odds, at, held, liveHeldBytes)
				case s.Trailing() && !saidHeld && held == 0:
					t.Fatalf("%s (odds %d): the first flush at %d after the stream trailed answered nothing held", tx.name, odds, at)
				}
				saidHeld = saidHeld || s.Trailing()
				final, trailed = f, s.Trailing()
			}
			err := s.Close()
			f, held, _ := s.Flush()
			switch {
			case s.Trailing() != tx.trails:
				t.Errorf("%s (odds %d): the stream trailed %v, where its held stretch passes the bound %v", tx.name, odds, s.Trailing(), tx.trails)
			case err != nil || out.String() != want:
				t.Errorf("%s (odds %d): Close answered %v, and the stream wrote another text than Secrets %s", tx.name, odds, err, parted(out.String(), want))
			case f != int64(out.Len()) || held != 0:
				t.Errorf("%s (odds %d): a flush after Close answered %d final and %d held, of %d written", tx.name, odds, f, held, out.Len())
			}
		}
	}
}
