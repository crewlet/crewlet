package executor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/redact"
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

// A COLLECTION WEIGHS WHAT ITS OPENS HOLD: five times every piece it reads,
// the error stream's end only as far as it asks for it, at least the fixed
// per-read buffers and at most what one collection can hold.
//
// Mutations, each red: leave a piece out of the weight; count the error stream
// whole; drop the multiple, the floor or the cap.
func TestACollectionWeighsWhatItsOpensHold(t *testing.T) {
	t.Parallel()
	r := NewClaudeCode()
	errWant := r.failureBound + redact.LookBack
	b, sizes := collectedBox(3<<20, errWant*4)
	p := PathsFor(b)
	c, err := r.openCollection(t.Context(), b, p, r.harness.Output(p))
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	var want int64
	for path, size := range sizes {
		if path == p.Err() {
			size = errWant
		}
		want += int64(size)
	}
	if got := c.weight(); got != collectHeldMultiple*want {
		t.Errorf("the collection weighs %d, want five times the %d bytes its pieces hold", got, want)
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
