package embeddings_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// THE FAKE REFUSES WHAT THE PROVIDER REFUSES, by the same rule: an input past
// the bound before anything is sent, and a batch packed into requests by the
// model's limits — so a caller certified against the twin is certified against
// what production does with the same input.
func TestTheFakeEnforcesTheLimitsTheProviderDoes(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(8)
	f.SetModel("tiny-model")
	f.SetLimits(embeddings.Limits{InputBytes: 8, BatchInputs: 2, BatchBytes: 64})

	_, err := f.Embed(t.Context(), "nine bytes")
	var long *embeddings.TooLongError
	if !errors.As(err, &long) || long.Model != "tiny-model" || long.Limit != 8 {
		t.Fatalf("Embed past the bound = %v, want a TooLongError for tiny-model at 8", err)
	}
	_, err = f.EmbedBatch(t.Context(), []string{"ok", "much too long"})
	if !errors.As(err, &long) || long.Index != 1 || long.Model != "tiny-model" {
		t.Fatalf("EmbedBatch past the bound = %v, want input 1 named", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests were recorded for inputs refused before sending", n)
	}

	got, err := f.EmbedBatch(t.Context(), []string{"a", "", "b", "c", "d"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(got) != 5 || got[1] != nil || got[0] == nil || got[4] == nil {
		t.Fatalf("the batch did not keep every slot: %v", got)
	}
	want := [][]string{{"a", "b"}, {"c", "d"}}
	if requests := f.Requests(); !slices.EqualFunc(requests, want, slices.Equal) {
		t.Fatalf("the fake sent %q, want %q", requests, want)
	}
}

// THE FAKE CAN BE TOLD TO REFUSE AN INPUT, every time it is sent and with the
// whole request it rides in, as a provider refuses a request over one input it
// cannot take — which is what a caller isolating a poison input needs to be
// tested against.
func TestTheFakeRefusesAMarkedInputEveryTime(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(8)
	f.Refuse("POISON")
	for range 2 {
		got, err := f.EmbedBatch(t.Context(), []string{"fine", "a POISON document"})
		if !errors.Is(err, embeddings.ErrRefused) || got != nil {
			t.Fatalf("a batch carrying the marked input = %v, %v; want ErrRefused and no vectors", got, err)
		}
	}
	if _, err := f.Embed(t.Context(), "POISON alone"); !errors.Is(err, embeddings.ErrRefused) {
		t.Fatalf("Embed of the marked input = %v, want ErrRefused", err)
	}
	if _, err := f.EmbedBatch(t.Context(), []string{"fine", "also fine"}); err != nil {
		t.Fatalf("a batch without the marked input was refused: %v", err)
	}
}

// THE FAKE CAN FAIL TRANSIENTLY AND RECOVER, so a caller's retry is tested
// against a failure that clears rather than one that never does.
func TestTheFakeFailsTransientlyThenRecovers(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(8)
	f.FailTransiently("flaky", 2)
	for i := range 2 {
		if _, err := f.Embed(t.Context(), "a flaky one"); !errors.Is(err, embeddings.ErrTransient) {
			t.Fatalf("call %d = %v, want ErrTransient", i, err)
		}
	}
	if _, err := f.Embed(t.Context(), "a flaky one"); err != nil {
		t.Fatalf("the third call still failed: %v", err)
	}
}

// A DONE CONTEXT FAILS THE FAKE AS IT FAILS THE PROVIDER: a deadline is
// transient, a cancellation is the caller's own — because a twin that answered
// a caller who had stopped waiting would certify a caller that never checks.
func TestTheFakeHonoursADoneContext(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(8)
	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if _, err := f.Embed(ctx, "late"); !errors.Is(err, embeddings.ErrTransient) {
		t.Errorf("Embed past a deadline = %v, want ErrTransient", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	_, err := f.EmbedBatch(ctx, []string{"stopped"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, embeddings.ErrTransient) {
		t.Errorf("EmbedBatch after a cancellation = %v, want context.Canceled and no class", err)
	}
}

// LIMITS NOTHING COULD BE SENT UNDER ARE A MISTAKE IN THE TEST, not a fake
// that refuses everything.
func TestTheFakeRefusesLimitsNothingFits(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "SetLimits") {
			t.Fatalf("SetLimits with no bound did not panic naming itself: %v", r)
		}
	}()
	embeddings.NewFake(8).SetLimits(embeddings.Limits{})
}
