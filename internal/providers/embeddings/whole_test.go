package embeddings_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// A TEXT WITHIN THE BOUND IS ONE PIECE, ITSELF — prepared, and nothing else —
// which is what keeps every short text's vector exactly what it always was.
func TestATextWithinTheBoundIsOnePieceItself(t *testing.T) {
	t.Parallel()
	text := "  the staging deploy\n keeps failing  "
	got := embeddings.Chunks(text, 64)
	if want := []string{embeddings.Prepare(text)}; !slices.Equal(got, want) {
		t.Fatalf("Chunks = %q, want %q", got, want)
	}
	if got := embeddings.Chunks(" \n\t ", 64); got != nil {
		t.Fatalf("a text of nothing chunked to %q, want none", got)
	}
}

// THE PIECES ARE THE TEXT: consecutive, each inside the bound, each valid
// UTF-8 and itself prepared, cut between words — so joined with the single
// spaces they were cut at, they are the prepared text again, and nothing of
// it is dropped.
func TestThePiecesAreTheTextCutBetweenWords(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("the deploy keeps failing on staging — 部署失败 again. ", 40)
	for _, bound := range []int{4, 7, 16, 33, 100, 512} {
		chunks := embeddings.Chunks(text, bound)
		if len(chunks) < 2 {
			t.Fatalf("bound %d: %d pieces for a %d-byte text", bound, len(chunks), len(text))
		}
		for i, chunk := range chunks {
			switch {
			case chunk == "":
				t.Errorf("bound %d: piece %d is empty", bound, i)
			case len(chunk) > bound:
				t.Errorf("bound %d: piece %d is %d bytes", bound, i, len(chunk))
			case !utf8.ValidString(chunk):
				t.Errorf("bound %d: piece %d is not valid UTF-8: %q", bound, i, chunk)
			case embeddings.Prepare(chunk) != chunk:
				t.Errorf("bound %d: piece %d is not itself prepared: %q", bound, i, chunk)
			}
		}
		// EVERY CHARACTER, IN ORDER, at any bound: with the spaces taken
		// out, the pieces are the text.
		bare := func(s string) string { return strings.ReplaceAll(s, " ", "") }
		if bare(strings.Join(chunks, "")) != bare(embeddings.Prepare(text)) {
			t.Errorf("bound %d: the pieces do not carry every character of the text", bound)
		}
		// And where every word fits a piece (the longest here, 部署失败,
		// is twelve bytes), every cut fell between words, so the pieces
		// joined at those spaces are the prepared text exactly.
		if bound >= 12 && strings.Join(chunks, " ") != embeddings.Prepare(text) {
			t.Errorf("bound %d: the pieces joined are not the text", bound)
		}
	}
}

// A WORD LONGER THAN A PIECE IS CUT ON RUNE BOUNDARIES — a URL, a hash, a
// script written without spaces — and the words after it join its last piece.
func TestAWordLongerThanAPieceIsCutOnRuneBoundaries(t *testing.T) {
	t.Parallel()
	// Ten three-byte runes, thirty bytes, against a ten-byte piece: each cut
	// walks back from byte ten to the rune boundary at nine.
	word := strings.Repeat("失败", 5)
	chunks := embeddings.Chunks(word+" ok", 10)
	want := []string{"失败失", "败失败", "失败失", "败 ok"}
	if !slices.Equal(chunks, want) {
		t.Fatalf("Chunks = %q, want %q", chunks, want)
	}
}

// A PIECE IS FILLED TO THE BOUND, not one short of it: words join a piece
// while the piece, the space and the word fit — exactly fitting included — so
// a text takes the fewest pieces the bound allows, and so the fewest inputs.
func TestAPieceIsFilledToTheBound(t *testing.T) {
	t.Parallel()
	got := embeddings.Chunks("aaaa bbbb cccc dddd e", 9)
	want := []string{"aaaa bbbb", "cccc dddd", "e"}
	if !slices.Equal(got, want) {
		t.Fatalf("Chunks = %q, want %q", got, want)
	}
}

// A SHORT TEXT EMBEDDED WHOLE IS HELD TO EMBED'S CEILING, because it IS an
// Embed call — not a batch of one, whose request would carry a corpus's
// ceiling into a turn start.
func TestAShortTextEmbeddedWholeIsASingleCall(t *testing.T) {
	t.Parallel()
	e := newEchoAPI(t, 4)
	p := e.provider(t, small)
	if _, err := embeddings.EmbedWhole(t.Context(), p, "short"); err != nil {
		t.Fatalf("EmbedWhole: %v", err)
	}
	if _, err := embeddings.EmbedWhole(t.Context(), p, "a text longer than sixteen bytes"); err != nil {
		t.Fatalf("EmbedWhole: %v", err)
	}
	want := []string{
		fmt.Sprint(int(embeddings.EmbedTimeout.Seconds())),
		fmt.Sprint(int(embeddings.BatchTimeout.Seconds())),
	}
	if got := e.ceilings(); !slices.Equal(got, want) {
		t.Fatalf("a short and a long text were held to %v seconds, want %v", got, want)
	}
}

// A BOUND THAT CANNOT HOLD A CHARACTER IS A CALLER'S MISTAKE, refused loudly
// rather than looped on or answered with pieces nothing can send.
func TestChunksRefusesABoundThatCannotHoldEveryCharacter(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("a 3-byte bound was accepted")
		}
	}()
	embeddings.Chunks("anything at all", utf8.UTFMax-1)
}

// THE POOL IS A LENGTH-WEIGHTED MEAN OF DIRECTIONS, unit length: each piece
// counts in proportion to how much of the text it carries, and a server's
// magnitude counts for nothing.
func TestThePoolIsALengthWeightedMeanOfDirections(t *testing.T) {
	t.Parallel()
	got, err := embeddings.Pool([][]float32{{1, 0}, {0, 5}}, []int{3, 1}, 2)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	want := []float64{3 / math.Sqrt(10), 1 / math.Sqrt(10)}
	for i := range want {
		if math.Abs(float64(got[i])-want[i]) > 1e-6 {
			t.Fatalf("Pool = %v, want %v", got, want)
		}
	}
	// A MAGNITUDE IS NOT A WEIGHT: the same directions, scaled, pool alike.
	scaled, err := embeddings.Pool([][]float32{{7, 0}, {0, 0.25}}, []int{3, 1}, 2)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if !slices.Equal(got, scaled) {
		t.Errorf("rescaling a piece's vector moved the pool: %v vs %v", got, scaled)
	}
	if norm := math.Sqrt(float64(got[0]*got[0] + got[1]*got[1])); math.Abs(norm-1) > 1e-6 {
		t.Errorf("the pool has magnitude %v, want 1", norm)
	}
}

// WHAT HAS NO DIRECTION IS REFUSED: a zero or non-finite vector contributes
// nothing a cosine can read, a vector of the wrong width is a configuration
// fault, and a set that cancels has no mean to give.
func TestThePoolRefusesWhatHasNoDirection(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		vectors [][]float32
		weights []int
		want    error
	}{
		"nothing":            {nil, nil, nil},
		"weights mismatched": {[][]float32{{1, 0}}, []int{1, 1}, nil},
		"a zero vector":      {[][]float32{{1, 0}, {0, 0}}, []int{1, 1}, nil},
		"a NaN":              {[][]float32{{float32(math.NaN()), 0}}, []int{1}, nil},
		"an infinity":        {[][]float32{{float32(math.Inf(1)), 0}}, []int{1}, nil},
		"a weight of zero":   {[][]float32{{1, 0}}, []int{0}, nil},
		"cancelling":         {[][]float32{{1, 0}, {-1, 0}}, []int{4, 4}, nil},
		"the wrong width":    {[][]float32{{1, 0, 0}}, []int{1}, embeddings.ErrConfiguration},
	} {
		got, err := embeddings.Pool(tc.vectors, tc.weights, 2)
		if err == nil {
			t.Errorf("%s: pooled to %v", name, got)
			continue
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: %v is not %v", name, err, tc.want)
		}
	}
	// A PIECE WITH NO DIRECTION IS NAMED, rather than surfacing as a mean
	// that came out NaN with nothing to say which piece made it so.
	_, err := embeddings.Pool([][]float32{{1, 0}, {0, 0}}, []int{1, 1}, 2)
	if err == nil || !strings.Contains(err.Error(), "piece 1") {
		t.Errorf("a zero piece was refused as %v, without naming it", err)
	}
}

// A SHORT TEXT EMBEDDED WHOLE IS EMBED'S OWN VECTOR, from one Embed call — not
// a pool of one, and not a batch request held to a corpus's ceiling.
func TestEmbeddingAShortTextWholeIsEmbed(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(64)
	text := "the staging deploy keeps failing"
	got, err := embeddings.EmbedWhole(t.Context(), f, text)
	if err != nil {
		t.Fatalf("EmbedWhole: %v", err)
	}
	alone, err := embeddings.NewFake(64).Embed(t.Context(), text)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !slices.Equal(got, alone) {
		t.Fatal("a short text embedded whole is not the vector Embed gives it")
	}
	if requests := f.Requests(); len(requests) != 1 || len(requests[0]) != 1 {
		t.Fatalf("a short text took requests %q, want one of one input", requests)
	}
}

// A LONG TEXT EMBEDDED WHOLE IS NEVER REFUSED FOR ITS LENGTH: its pieces go in
// ONE call, packed by the model's limits, and come back as one unit vector —
// the pool of the pieces' own vectors, which is every piece of the text and
// not its opening.
func TestEmbeddingALongTextWholePoolsItsPiecesInOneCall(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(64)
	f.SetLimits(embeddings.Limits{InputBytes: 32, BatchInputs: 4, BatchBytes: 1024})
	text := strings.Repeat("deploy failing staging runbook rollback ", 10)
	if _, err := f.Embed(t.Context(), text); !errors.Is(err, embeddings.ErrTooLong) {
		t.Fatalf("the fixture is wrong: Embed of the long text = %v", err)
	}
	got, err := embeddings.EmbedWhole(t.Context(), f, text)
	if err != nil {
		t.Fatalf("EmbedWhole: %v", err)
	}
	chunks := embeddings.Chunks(text, 32)
	vectors := make([][]float32, len(chunks))
	weights := make([]int, len(chunks))
	for i, chunk := range chunks {
		v, err := embeddings.NewFake(64).Embed(t.Context(), chunk)
		if err != nil {
			t.Fatalf("Embed piece %d: %v", i, err)
		}
		vectors[i], weights[i] = v, len(chunk)
	}
	want, err := embeddings.Pool(vectors, weights, 64)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatal("the whole text's vector is not the pool of its pieces' vectors")
	}
	var sent int
	for _, request := range f.Requests() {
		if len(request) > 4 {
			t.Errorf("a request carried %d inputs, past the limit of 4", len(request))
		}
		sent += len(request)
	}
	if sent != len(chunks) {
		t.Errorf("%d pieces were sent for a text of %d", sent, len(chunks))
	}
}

// A TEXT OF NOTHING IS ErrEmpty, the answer Embed gives it — not a pool of no
// vectors, and not a provider error.
func TestEmbeddingNothingWholeIsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := embeddings.EmbedWhole(t.Context(), embeddings.NewFake(8), " \n "); !errors.Is(err, embeddings.ErrEmpty) {
		t.Fatalf("EmbedWhole of nothing = %v, want ErrEmpty", err)
	}
}

// A FAILURE IS THE CALL'S: a long text whose batch is refused is refused, in
// the provider's class, rather than pooled from the pieces that came back.
func TestAWholeTextWhoseBatchFailsIsNotPooledFromWhatCameBack(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(16)
	f.SetLimits(embeddings.Limits{InputBytes: 16, BatchInputs: 1, BatchBytes: 16})
	f.Refuse("poison")
	got, err := embeddings.EmbedWhole(context.Background(), f, "a perfectly fine opening and then poison")
	if !errors.Is(err, embeddings.ErrRefused) || got != nil {
		t.Fatalf("EmbedWhole = %v, %v; want no vector and ErrRefused", got, err)
	}
}
