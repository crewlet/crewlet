package executor

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// stillClock is a runner's clock that never moves: a read on it is never past
// [sandbox.FailureReadWait], however long the race detector makes it.
func stillClock() time.Time { return time.Time{} }

// readCounter is an error stream's body that hands its text in pieces of a
// size drawn from r, and counts what it was asked for: the most one read
// asked and every byte it gave.
type readCounter struct {
	text      io.Reader
	r         *rand.Rand
	most, all int
}

func (c *readCounter) Read(p []byte) (int, error) {
	c.most = max(c.most, len(p))
	if c.r != nil && len(p) > 1 {
		p = p[:1+c.r.IntN(len(p))]
	}
	n, err := c.text.Read(p)
	c.all += n
	return n, err
}

func (c *readCounter) Close() error { return nil }

// failureTailOf is what a collection shows of text as its error stream, kept
// to keep bytes, on a clock that never passes the read's wait.
func failureTailOf(t *testing.T, text string, keep int) failureTail {
	t.Helper()
	body := &readCounter{text: strings.NewReader(text), r: rand.New(rand.NewPCG(uint64(len(text)), uint64(keep)))}
	tail, err := (&Runner{now: stillClock}).readFailureTail(t.Context(), &piece{path: "err",
		what: "the error stream", body: body, size: int64(len(text))}, keep, time.Time{})
	if err != nil {
		t.Fatalf("readFailureTail: %v", err)
	}
	return tail
}

// tailShapes is error streams whose end a reading begun inside them, with
// [redact.LookBack] before it, does not show as the whole stream's redaction
// does: a key's armour opened before the end, its block running past the
// place the reading settles, so the reading redacted with and without what
// came before parts to the end and shows nothing; a long line run to the end,
// and a token whose body runs past the end, of which it shows nothing either. And a key's block begun before the end
// and a password's value past its label, beyond a key block's reach, which it
// showed only past their context.
func tailShapes() map[string]string {
	filler := func(n int) string { return strings.Repeat("compiling a package of the build\n", n/33+1) }
	body := "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
	begin, end := "-----BEGIN RSA PRIVATE KEY-----\n", "-----END RSA PRIVATE KEY-----\n"
	label := "**" + strings.Repeat("a", 6000) + " password:**\n" + strings.Repeat("\n", 60000)
	return map[string]string{
		"a key's armour opened before the end, its END lost": filler(2*redact.LookBack) + begin +
			strings.Repeat(body+"\n", 790),
		"a key's armour opened before the end": filler(2*redact.LookBack) + begin +
			strings.Repeat(body+"\n", 790) + end + "done\n",
		"a key's block begun before the end": filler(2*redact.LookBack) + begin + strings.Repeat(body+"\n", 40) +
			strings.Repeat("retrying the upload in two seconds\n", 3) + strings.Repeat(body+"\n", 5) + end + "done\n",
		"a password's value past its label": filler(2*redact.LookBack) + label + "hunter2x9\n" +
			strings.Repeat("sk-"+strings.Repeat("B", 50000)+"\n", 8) + strings.Repeat("ok\n", 100),
		"one line longer than the end": "starting\n" + strings.Repeat("e", 3*redact.LookBack) + "FATAL: the last line",
		"a token's body running past the end": "the key was sk-ant-api03-" +
			strings.Repeat("Ab0x9Z", redact.LookBack) + "\nFATAL: the key was refused\n",
		"one line, ended": "starting\n" + strings.Repeat("e", 3*redact.LookBack) + "\n",
	}
}

// A FAILURE'S TAIL IS THE END OF ITS WHOLE STREAM'S REDACTION: read from the
// stream's first byte through one redaction stream, in pieces of any size, it
// is [sandbox.KeepEnd] of [redact.Secrets] of the whole stream, and what it
// leaves out before that end is counted in redacted bytes. Over the shapes
// whose redaction is decided far before the end ([tailShapes], [readsFarBack],
// [holdShapes]), where a window read with [redact.LookBack] before it showed
// none, or only past what its context settled.
//
// Mutations, each red: redact only the stream's last keep+LookBack bytes; keep
// the ring at keep bytes, not keep+1, so KeepEnd cannot see the byte before
// its cut; count the stream's bytes rather than the redaction's before the end.
func TestAFailuresTailIsTheEndOfItsWholeStreamsRedaction(t *testing.T) {
	t.Parallel()
	texts := tailShapes()
	for _, s := range readsFarBack() {
		texts[s.name] = s.text
	}
	for name, text := range holdShapes(512 << 10) {
		texts["hold shape "+name] = text
	}
	for name, text := range texts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			whole := redact.Secrets(text)
			for _, keep := range []int{4 << 10, 64 << 10, len(whole)} {
				want, at := sandbox.KeepEnd(whole, keep)
				got := failureTailOf(t, text, keep)
				if got.shown != want || got.before != int64(at) || got.withheld != "" {
					t.Errorf("at %d: the tail is %d bytes opening %.60q after %d, withheld %q; want the whole "+
						"redaction's end, %d bytes opening %.60q after %d", keep, len(got.shown), got.shown,
						got.before, got.withheld, len(want), want, at)
				}
			}
		})
	}
}

// A FAILURE'S TAIL IS NOT SHOWN PAST THE READ'S BOUND: a stream whose
// redaction has not reached its end within [sandbox.FailureReadWait] on the
// runner's clock, or by the bound its box read handed it
// ([sandbox.RunHandle.StreamsReadBy]), or once its read's context ended, shows
// none of it and says why and the time it was given, since an end the
// redaction has not reached is no end of the stream's; one that reached it in
// time is shown.
//
// Mutations, each red: show what the ring holds at the bound; read the clock
// only before the first piece; read on past the bound; ignore the handle's
// bound or the context's end.
func TestAFailuresTailIsNotShownPastTheReadsBound(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("compiling a package of the build\n", 4*failureReadPiece/33) + "FATAL: the last line\n"
	ended, end := context.WithCancel(context.Background())
	end()
	for _, c := range []struct {
		name   string
		step   time.Duration
		readBy time.Duration
		ctx    context.Context
		given  string
	}{
		{"a read that ends within the wait", sandbox.FailureReadWait / 16, 0, nil, ""},
		{"a read the wait ends", sandbox.FailureReadWait / 3, 0, nil, "32s"},
		{"a read its box read's bound ends", sandbox.FailureReadWait / 16, sandbox.FailureReadWait / 8, nil, "4s"},
		{"a read handed a bound already passed", sandbox.FailureReadWait / 16, -time.Second, nil, "0s"},
		{"a read whose context ended", sandbox.FailureReadWait / 16, 0, ended, "32s"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var now time.Time
			clock := func() time.Time {
				now = now.Add(c.step)
				return now
			}
			var readBy time.Time
			if c.readBy != 0 {
				// The first reading of the clock is the read's start.
				readBy = time.Time{}.Add(c.step + c.readBy)
			}
			ctx := c.ctx
			if ctx == nil {
				ctx = t.Context()
			}
			body := &readCounter{text: strings.NewReader(text)}
			tail, err := (&Runner{now: clock}).readFailureTail(ctx, &piece{path: "err",
				what: "the error stream", body: body, size: int64(len(text))}, 1<<10, readBy)
			if err != nil {
				t.Fatal(err)
			}
			withheld := "ran past the collection's bound on its read, " + c.given + ","
			switch {
			case c.given == "" && (tail.withheld != "" || !strings.HasSuffix(tail.shown, "FATAL: the last line\n")):
				t.Errorf("the tail is %q, withheld %q; want the stream's end", tail.shown, tail.withheld)
			case c.given != "" && (tail.shown != "" || !strings.Contains(tail.note(), withheld)):
				t.Errorf("the tail shows %q with the note %q; want none of it, and %q", tail.shown, tail.note(),
					withheld)
			case c.given != "" && body.all >= len(text):
				t.Errorf("the read went on to the stream's end, %d bytes, past its bound", body.all)
			}
		})
	}
}

// A FAILURE'S TAIL THE REDACTION DECLINES IS NOT SHOWN: a stream the redaction
// stream refuses ([redact.ErrDecidedEarly]: a key's armour, then password's
// keys and a run past the stream's hold) shows none of what the stream wrote,
// which may hold what the redaction takes out, and says why, whether the
// refusal comes at a write or at the close.
//
// Mutation: show what the ring holds on a refusal, at a write or at the close.
func TestAFailuresTailTheRedactionDeclinesIsNotShown(t *testing.T) {
	t.Parallel()
	for when, tail := range map[string]int{"at its close": 256 << 10, "at a write": 2 << 20} {
		t.Run(when, func(t *testing.T) {
			t.Parallel()
			text := "k: -----BEGIN RSA PRIVATE KEY-----\n" +
				"MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n\n" +
				strings.Repeat("pwd:\n", 12000) + "sk-" + strings.Repeat("Ab0_-x9Z", 1<<20) + "\n" +
				strings.Repeat("ok\n", tail)
			got := failureTailOf(t, text, 64<<10)
			if got.shown != "" || !strings.Contains(got.note(), redactionRefused) {
				t.Errorf("a declined stream shows %d bytes with the note %q; want none, and why", len(got.shown),
					got.note())
			}
		})
	}
}

// A FAILURE'S TAIL HOLDS ITS BOUND, NEVER ITS STREAM: the read takes the
// stream a piece at a time ([failureReadPiece]) and its end in a ring of
// keep+1 bytes, so a stream of tens of mebibytes is read whole, to the size
// it was weighed at, through buffers that do not grow with it.
//
// Mutations, each red: read the stream whole (io.ReadAll); grow the ring by
// append; keep everything written.
func TestAFailuresTailHoldsItsBoundNeverItsStream(t *testing.T) {
	t.Parallel()
	const keep = 64 << 10
	line := "compiling a package of the build, step 0000000\n"
	text := strings.Repeat(line, (24<<20)/len(line))
	body := &readCounter{text: io.MultiReader(strings.NewReader(text), strings.NewReader("grown after the open"))}
	tail, err := (&Runner{now: stillClock}).readFailureTail(t.Context(), &piece{path: "err",
		what: "the error stream", body: body, size: int64(len(text))}, keep, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if body.most > failureReadPiece || body.all != len(text) {
		t.Errorf("the read asked for %d bytes at most and read %d; want pieces of %d, to the %d it was weighed at",
			body.most, body.all, failureReadPiece, len(text))
	}
	if want, at := sandbox.KeepEnd(text, keep); tail.shown != want || tail.before != int64(at) {
		t.Errorf("the tail is %d bytes after %d; want the stream's last %d after %d", len(tail.shown),
			tail.before, len(want), at)
	}

	ring := &endRing{keep: keep + 1}
	r := rand.New(rand.NewPCG(7, 11))
	var all bytes.Buffer
	for all.Len() < 8<<20 {
		piece := bytes.Repeat([]byte{byte('a' + r.IntN(26))}, 1+r.IntN(3*keep))
		all.Write(piece)
		if _, err := ring.Write(piece); err != nil {
			t.Fatal(err)
		}
		if cap(ring.buf) > ring.keep {
			t.Fatalf("the ring holds a buffer of %d bytes, past its %d", cap(ring.buf), ring.keep)
		}
	}
	if got, want := ring.String(), all.String()[all.Len()-ring.keep:]; got != want || ring.dropped() != int64(all.Len()-ring.keep) {
		t.Errorf("the ring holds %d bytes after %d; want the last %d written after %d", len(got), ring.dropped(),
			len(want), all.Len()-ring.keep)
	}
}

// EVERY PIECE A COLLECTION REFUSES OR DOES NOT SHOW IS COUNTED, WITH WHY
// ([sandbox.CountingWithheld]): an error stream past its bound, a report
// past the whole-read cap and a session record cut while it was read, each
// once, by its piece and reason; a collection that gives up nothing counts
// nothing.
//
// Mutation: leave any of the counts out, or count a refusal at its open twice.
func TestEveryPieceACollectionWithholdsIsCounted(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		box  func(b *sandbox.FakeSandbox, p Paths)
		want []sandbox.Withholding
	}{
		{"nothing given up", func(b *sandbox.FakeSandbox, p Paths) { b.Put(p.Err(), "a warning\n") }, nil},
		{"each withheld", func(b *sandbox.FakeSandbox, p Paths) {
			b.Put(p.Findings(), strings.Repeat("r", b.ReadCap()+1))
			b.Streams = func(path string, r io.Reader) io.Reader {
				if path == p.SessionRecord() {
					b.TruncateInPlace(path, 10)
				}
				return r
			}
		}, []sandbox.Withholding{
			{Piece: sandbox.WithheldWholePiece, Reason: sandbox.WithheldTooLarge},
			{Piece: sandbox.WithheldWholePiece, Reason: sandbox.WithheldChanged},
			{Piece: sandbox.WithheldErrorStream, Reason: sandbox.WithheldPastBound},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var now time.Time
			runner := New(OpenCode{})
			runner.now = func() time.Time {
				now = now.Add(sandbox.FailureReadWait / 2)
				return now
			}
			b := sandbox.NewFakeSandbox("box-1")
			p := PathsFor(b)
			b.Put(p.Result(), "not a result\n")
			b.Put(p.Err(), strings.Repeat("noise line\n", 3*failureReadPiece/11))
			b.Put(p.ExitCode(), "1")
			b.Put(p.SessionRecord(), "9a8b7c6d-0000-4000-8000-000000000002 0\n")
			c.box(b, p)
			meter := &withholdings{}
			_, err := runner.Collect(sandbox.CountingWithheld(t.Context(), meter), b, sandbox.RunHandle{},
				queue.NewAnswerBudget(nil))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if got := meter.all(); !slices.Equal(got, c.want) {
				t.Errorf("the collection counted %v, want %v", got, c.want)
			}
		})
	}
}

// withholdings is a meter that keeps what it is told, in order.
type withholdings struct {
	mu  sync.Mutex
	got []sandbox.Withholding
}

func (m *withholdings) Withheld(w sandbox.Withholding) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.got = append(m.got, w)
}

func (m *withholdings) all() []sandbox.Withholding {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.got)
}
