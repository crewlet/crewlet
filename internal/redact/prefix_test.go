package redact_test

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// AN OPENING IS THE WHOLE REDACTION'S OPENING: over a text of the stream's
// corpus ([streamCorpus]), texts of every shape a stage carries across its
// window and texts a byte either side of the length a stream redacts whole,
// the opening [redact.Prefix] answers is the first n bytes of
// [redact.Secrets] and states its whole length, for n at nothing, a byte,
// either side of a stream's whole length and of the redaction's, and at
// random. Each reading costs a pass over the text, so over the long texts it
// is held at fewer n, and read through a stream at the random n alone.
//
// Mutation: stop feeding the stream once n bytes are out, and the length it
// states is short of the whole redaction's.
func TestAnOpeningIsTheWholeRedactionsOpening(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 60))
	texts := map[string]string{}
	corpus := streamCorpus(r)
	for _, name := range slices.Sorted(maps.Keys(corpus))[:1] {
		texts[name] = corpus[name]
	}
	texts["shapes"] = redact.StreamText(r, 3*redact.MaxReach+r.IntN(8*redact.MaxReach))
	short := redact.StreamText(r, 4*redact.MaxReach)
	streamWhole := 3*redact.MaxReach + 1536
	for _, n := range []int{streamWhole, streamWhole + 1} {
		texts[fmt.Sprintf("%d bytes", n)] = short[:min(len(short), n)]
	}
	for name, text := range texts {
		seed := r.Uint64()
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := redact.Secrets(text)
			r := rand.New(rand.NewPCG(1170, seed))
			random := r.IntN(len(want) + 1)
			ns := []int{-1, 0, 1, streamWhole - 1, streamWhole, streamWhole + 1, random, len(want) - 1, len(want), len(want) + 1}
			if len(text) > streamWhole+1 {
				ns = []int{0, streamWhole + 1, random, len(want), len(want) + 1}
			}
			for _, n := range ns {
				reads := map[string]func(string, int) (string, int){"whole": redact.Prefix}
				if len(text) <= streamWhole+1 || n == random {
					reads["streamed"] = redact.StreamedPrefix
				}
				wantOpening := want[:min(max(n, 0), len(want))]
				for read, prefix := range reads {
					opening, whole := prefix(text, n)
					if whole != len(want) {
						t.Errorf("%d bytes, %s, an opening of %d: states a whole of %d bytes, want %d", len(text), read, n, whole, len(want))
					}
					if opening != wantOpening {
						t.Errorf("%d bytes, %s, an opening of %d: %s", len(text), read, n, streamParted(opening, wantOpening))
					}
				}
			}
		})
	}
}
