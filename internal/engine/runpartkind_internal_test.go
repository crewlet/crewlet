package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY PART OF A RUN'S ACCOUNT IS CONDENSED AS WHAT IT IS. A question
// rewritten as though it were a report keeps findings and loses the options a
// person has to choose between, and an unknown kind is refused outright — so
// each part names a kind of its own, and one the compactor has instructions
// for.
func TestEveryRunPartIsCondensedAsWhatItIs(t *testing.T) {
	t.Parallel()
	want := map[sandbox.RunPart]compact.Kind{
		sandbox.PartReport:   compact.KindReport,
		sandbox.PartFailure:  compact.KindToolError,
		sandbox.PartQuestion: compact.KindQuestion,
	}
	for _, part := range sandbox.RunParts {
		kind := runPartKind(part)
		if !kind.Valid() {
			t.Errorf("%s is condensed as %q, which the compactor has no instructions for", part, kind)
		}
		if w, ok := want[part]; !ok || kind != w {
			t.Errorf("%s is condensed as %q; want %q", part, kind, w)
		}
	}
}

// THE SANDBOX COMPOSES WHAT ONE CONDENSATION READS. The coordinator holds a
// run's failure to sandbox.MaxCondenseBytes before handing it to this
// engine's compactor, and a runner composes its own to what is left of that
// — a figure the sandbox package declares because it cannot import the
// compactor, and that is the compactor's own: its MaxChunks first-pass pieces
// of ChunkBytes. Here the two meet, so the figure is held to the compactor in
// both directions: a text of exactly that size is taken (it reaches the
// model), and one byte more is refused before any model is asked.
func TestTheSandboxComposesWhatOneCondensationReads(t *testing.T) {
	t.Parallel()
	if want := compact.MaxChunks * compact.ChunkBytes; sandbox.MaxCondenseBytes != want {
		t.Fatalf("sandbox.MaxCondenseBytes = %d; want compact.MaxChunks × compact.ChunkBytes = %d",
			sandbox.MaxCondenseBytes, want)
	}
	c := compact.New(refusingAuxiliary{}, compact.NewCache())
	fit := func(n int) error {
		_, err := c.Fit(t.Context(), compact.Request{
			Seat: &org.Role{Name: "Developer"}, Kind: runPartKind(sandbox.PartFailure),
			Text: strings.Repeat("x", n), Budget: sandbox.MaxRunTextBytes,
		})
		return err
	}
	if err := fit(sandbox.MaxCondenseBytes); err == nil || errors.Is(err, compact.ErrTooLarge) {
		t.Errorf("a failure of exactly what one condensation reads = %v; want it taken, and "+
			"refused only by the model this test stands in", err)
	}
	if err := fit(sandbox.MaxCondenseBytes + 1); !errors.Is(err, compact.ErrTooLarge) {
		t.Errorf("a failure one byte past it = %v; want ErrTooLarge", err)
	}
}

// refusingAuxiliary resolves every seat's auxiliary model to one that refuses,
// so a compaction that gets as far as asking it says so.
type refusingAuxiliary struct{}

func (refusingAuxiliary) Auxiliary(*org.Role, auxspend.Use) (chain.Member, error) {
	return chain.Member{Key: "refusing", Provider: refusingProvider{}}, nil
}
