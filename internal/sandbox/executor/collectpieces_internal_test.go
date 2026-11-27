package executor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// collectedBox is a fake box holding every piece a Claude Code collection
// reads, the report findings bytes long and the error stream errBytes, each
// other piece a line; with what each piece holds, by path.
func collectedBox(findings, errBytes int) (*sandbox.FakeSandbox, map[string]int) {
	b := sandbox.NewFakeSandbox("box-1")
	p := PathsFor(b)
	files := map[string]string{
		p.Stream():        `{"type":"result","subtype":"success","is_error":false,"result":"done"}` + "\n",
		p.Result():        `{"type":"result","subtype":"success","is_error":false,"result":"done"}` + "\n",
		p.SessionRecord(): "9a8b7c6d-0000-4000-8000-000000000002 0\n",
		p.ExitCode():      "0\n",
		p.MemoryBefore():  "oom_kill 0\n",
		p.MemoryAfter():   "oom_kill 0\n",
		p.JobEnded():      "",
		p.Findings():      strings.Repeat("r", findings),
		p.BaseResult():    "{}\n",
		p.Err():           strings.Repeat("e", errBytes),
	}
	sizes := map[string]int{}
	for path, content := range files {
		b.Put(path, content)
		sizes[path] = len(content)
	}
	return b, sizes
}

// A COLLECTION WEIGHS WHAT ITS OPENS HOLD: five times every piece it reads
// but the error stream, at least the fixed per-read buffers, and beside them
// the error stream's read, one redaction stream and its end's ring, or five
// times a stream short enough to be redacted whole, and the event stream's
// kept last line; at most what one collection can hold.
//
// Mutations, each red: leave a piece out of the weight; count the error stream
// among the pieces; leave its read's stream or ring out; drop the multiple,
// the floor or the cap.
func TestACollectionWeighsWhatItsOpensHold(t *testing.T) {
	t.Parallel()
	r := NewClaudeCode()
	for _, errBytes := range []int{1 << 10, 64 << 20} {
		b, sizes := collectedBox(3<<20, errBytes)
		p := PathsFor(b)
		c, err := r.openCollection(t.Context(), b, p, r.harness.Output(p))
		if err != nil {
			t.Fatal(err)
		}
		var want int64
		for path, size := range sizes {
			if path != p.Err() {
				want += int64(size)
			}
		}
		read := min(collectHeldMultiple*int64(errBytes), streamHeldBytes+3*int64(r.failureBound)+2)
		// And the event stream's last line, which Claude Code's collection
		// keeps to stand in for an empty result file: here the whole stream.
		last := int64(sizes[p.Stream()])
		if got := c.weight(); got != collectHeldMultiple*want+read+last {
			t.Errorf("with an error stream of %d bytes the collection weighs %d, want five times the %d bytes "+
				"its other pieces hold, the %d its read of the stream holds and the %d of its kept last line",
				errBytes, got, want, read, last)
		}
		c.close()
	}
	if got, want := stderrHeld(64<<20, 2<<20), int64(streamHeldBytes+6<<20+2); got != want {
		t.Errorf("a long error stream's read weighs %d, want one redaction stream and its ring, %d", got, want)
	}
	for _, tc := range []struct{ sizes, want int64 }{
		{0, baseTierReadBytes},
		{1 << 20, baseTierReadBytes},
		{4 << 20, 20 << 20},
		{1 << 40, sandbox.BoxReadHeldBytes},
	} {
		if got := collectionWeight(tc.sizes); got != tc.want {
			t.Errorf("pieces of %d bytes weigh %d, want %d", tc.sizes, got, tc.want)
		}
	}
}

// A COLLECTION HOLDS ITS WEIGHT BEFORE IT READS A BYTE: with the node's answer
// budget held by others for the whole wait, it reads nothing of the box and
// fails naming the budget.
//
// Mutations, each red: read a piece before holding; read on after the hold is
// refused.
func TestACollectionHoldsItsWeightBeforeItReadsAByte(t *testing.T) {
	t.Parallel()
	b, _ := collectedBox(1<<20, 1<<10)
	answers := queue.NewAnswerBudget(nil)
	release, err := answers.Acquire(t.Context(), queue.AnswerHistory, queue.AnswerBudgetBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = NewClaudeCode().Collect(ended, b, sandbox.RunHandle{}, answers)
	if !errors.Is(err, queue.ErrAnswerBudget) {
		t.Fatalf("a collection behind a held budget: %v, want ErrAnswerBudget", err)
	}
	if got := b.Delivered(); got != 0 {
		t.Errorf("a collection that never held its weight read %d bytes of the box", got)
	}
}

// grownReader is a piece's bytes as its open sized them, then more the file
// gained after the open, counting what is read past the size.
type grownReader struct {
	opened io.Reader
	more   int
	past   *int
}

func (g *grownReader) Read(buf []byte) (int, error) {
	n, err := g.opened.Read(buf)
	if n > 0 || !errors.Is(err, io.EOF) {
		return n, err
	}
	if g.more == 0 {
		return 0, io.EOF
	}
	n = min(len(buf), g.more)
	g.more -= n
	*g.past += n
	return n, nil
}

// A PIECE THAT GREW AFTER ITS OPEN IS READ NO FURTHER THAN THE SIZE IT WAS
// WEIGHED AT: the bytes it gained are not read (one byte tells it grew), and it
// is described as a piece past its bound, the run no success.
//
// Mutations, each red: read a piece to its end; keep a piece that grew; read
// the next byte past the size and keep reading.
func TestAPieceThatGrewAfterItsOpenIsReadNoFurtherThanItsSize(t *testing.T) {
	t.Parallel()
	const report, gained = 64 << 10, 4 << 20
	b, _ := collectedBox(report, 1<<10)
	findings := PathsFor(b).Findings()
	past := 0
	b.Streams = func(path string, r io.Reader) io.Reader {
		if path != findings {
			return r
		}
		return &grownReader{opened: r, more: gained, past: &past}
	}
	result, err := NewClaudeCode().Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatal(err)
	}
	if past > 1 {
		t.Errorf("the collection read %d bytes the report gained after its open, want at most the one "+
			"that tells it grew", past)
	}
	if result.Success || !strings.Contains(result.Error, "grew past the 64 KiB it held when the collection "+
		"opened it") || result.Text == strings.Repeat("r", report) {
		t.Errorf("a report that grew after its open collected success %v, text of %d bytes, failure %q; "+
			"want it described and not read", result.Success, len(result.Text), result.Error)
	}
}

// A COLLECTION READS NO MORE PIECES WHOLE THAN A BOX READ'S BUDGET HOLDS ROOM
// FOR ([sandbox.CollectWholePieces], each at its cap at the box-read floor),
// for every harness: a piece added past it would be read on time the budget
// never gave, out of the session take's.
//
// Mutation: open one more piece whole, or lower the figure, and a harness
// collects more than it holds.
func TestACollectionReadsNoMorePiecesWholeThanItsBudgetHolds(t *testing.T) {
	t.Parallel()
	most := 0
	for _, h := range Registered() {
		r := New(h)
		b, _ := collectedBox(1<<10, 1<<10)
		p := PathsFor(b)
		c, err := r.openCollection(t.Context(), b, p, h.Output(p))
		if err != nil {
			t.Fatalf("%s: %v", h.Name(), err)
		}
		whole := 0
		for _, piece := range c.all() {
			if piece != nil && piece != c.stream && piece != c.stderr {
				whole++
			}
		}
		c.close()
		if whole > sandbox.CollectWholePieces {
			t.Errorf("%s's collection reads %d pieces whole, past the %d a box read's budget holds room for",
				h.Name(), whole, sandbox.CollectWholePieces)
		}
		most = max(most, whole)
	}
	if most != sandbox.CollectWholePieces {
		t.Errorf("the most pieces any harness's collection reads whole is %d; sandbox.CollectWholePieces "+
			"holds room for %d", most, sandbox.CollectWholePieces)
	}
}

// A PIECE READ WHOLE STANDS FOR ONE VERSION OF ITS FILE: read short of the
// size its open found, or cut, or cut and grown short again while it was
// read, it is refused as a piece that changed while it was read; grown in
// place, as one past its bound; a file written anew over its path while it
// was read leaves the one opened whole, and it is read.
//
// Mutations, each red: accept a read short of the size; leave out the check of
// the file after the read; take an append for no change.
func TestAPieceReadWholeStandsForOneVersionOfItsFile(t *testing.T) {
	t.Parallel()
	const report = 64 << 10
	for _, c := range []struct {
		name   string
		during func(b *sandbox.FakeSandbox, path string)
		short  bool
		want   string
	}{
		{"read short of its size", nil, true, "changed while the collection read it"},
		{"cut while read", func(b *sandbox.FakeSandbox, path string) { b.TruncateInPlace(path, 100) }, false,
			"changed while the collection read it"},
		{"cut and grown short", func(b *sandbox.FakeSandbox, path string) {
			b.TruncateInPlace(path, 0)
			b.AppendInPlace(path, []byte(strings.Repeat("s", report/2)))
		}, false, "changed while the collection read it"},
		{"grown in place", func(b *sandbox.FakeSandbox, path string) { b.AppendInPlace(path, []byte("more")) },
			false, "grew past the"},
		{"written anew over its path", func(b *sandbox.FakeSandbox, path string) { b.Put(path, "another") },
			false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			b, _ := collectedBox(report, 1<<10)
			findings := PathsFor(b).Findings()
			b.Streams = func(path string, r io.Reader) io.Reader {
				if path != findings {
					return r
				}
				if c.short {
					return io.LimitReader(r, report/2)
				}
				return &firstRead{r: r, then: func() {
					if c.during != nil {
						c.during(b, findings)
					}
				}}
			}
			r := NewClaudeCode()
			p := PathsFor(b)
			pieces, err := r.openCollection(t.Context(), b, p, r.harness.Output(p))
			if err != nil {
				t.Fatal(err)
			}
			defer pieces.close()
			raw, refusal, err := pieces.findings.whole()
			switch {
			case err != nil:
				t.Fatal(err)
			case c.want == "" && (refusal != "" || len(raw) != report):
				t.Errorf("the piece read %d bytes, refused %q; want the file opened, whole", len(raw), refusal)
			case c.want != "" && (raw != nil || !strings.Contains(refusal, c.want)):
				t.Errorf("the piece read %d bytes, refused %q; want none, refused as %q", len(raw), refusal, c.want)
			}
		})
	}
}

// firstRead is r, calling then once its first read has handed something over.
type firstRead struct {
	r    io.Reader
	then func()
	done bool
}

func (f *firstRead) Read(p []byte) (int, error) {
	n, err := f.r.Read(p[:min(len(p), 1024)])
	if n > 0 && !f.done {
		f.done = true
		f.then()
	}
	return n, err
}

// A SESSION'S RECORD IS READ AS ONE VERSION OF IT, for the harnesses whose
// notes and newest summary are read off it (Copilot's events, Pi's session
// file): a record written to while its walk read it is refused, neither held
// nor not, since what was read may join two versions of it; one left alone
// reads as it did.
//
// Mutation: leave the check of the record after its walk out of either.
func TestASessionsRecordIsReadAsOneVersionOfIt(t *testing.T) {
	t.Parallel()
	const session = "9a8b7c6d-0000-4000-8000-000000000002"
	for name, c := range map[string]struct {
		path func(Paths) string
		read func(context.Context, sandbox.Sandbox) error
	}{
		"Copilot": {func(p Paths) string { return p.CopilotSessionDir(session) + "/events.jsonl" },
			func(ctx context.Context, b sandbox.Sandbox) error {
				_, err := Copilot{}.notesHeld(ctx, b, Session{ID: session}, nil, nil)
				return err
			}},
		"Pi": {func(p Paths) string { return p.PiSessionFile(session) },
			func(ctx context.Context, b sandbox.Sandbox) error {
				_, err := Pi{}.notesHeld(ctx, b, Session{ID: session}, nil, nil)
				return err
			}},
	} {
		for _, written := range []bool{false, true} {
			b := sandbox.NewFakeSandbox("box-1")
			path := c.path(PathsFor(b))
			b.Put(path, "")
			if written {
				b.Streams = func(p string, r io.Reader) io.Reader {
					if p != path {
						return r
					}
					b.AppendInPlace(path, []byte("{}\n"))
					return r
				}
			}
			err := c.read(t.Context(), b)
			if refused := err != nil && strings.Contains(err.Error(), "while it was read"); refused != written {
				t.Errorf("%s's record, written to while read %v, read with %v", name, written, err)
			}
		}
	}
}
