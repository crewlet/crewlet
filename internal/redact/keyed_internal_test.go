package redact

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// A KEYED SCAN FINDS EXACTLY WHAT ITS PATTERN FINDS. A [keyed] pattern is tried
// only where one of its words opens ([keyed.begins]) rather than stepped
// through every byte, which is exact only if no match can begin anywhere else
// and the scan reads a word as (?i) does. Fed every way a word's spellings, its
// case folds, its fragments, the separators and the bytes around them can
// follow one another, the scan's matches, its replacement, its answer to "is
// there one" and where its first match begins are the pattern's own — for both
// patterns the password rule is built from, and for a list whose words open
// with runes that fold outside ASCII (k to the Kelvin sign, s to ſ), so the
// scan is certified for whatever word [passwordKeys] gains rather than for the
// three it holds today.
//
// The texts are built from the list itself, so a word added to it is in them.
// Seeded, so a failure names a text that fails every time.
//
// Mutation: compare a word's runes by their ASCII case alone and the ſ and
// Kelvin spellings are missed; leave the last word out of what [keyed.begins]
// tries and every pwd is missed; resume after a match from its start rather
// than its end and the scan's replacements overlap where the pattern's never
// do.
func TestAKeyedScanFindsWhatItsPatternFinds(t *testing.T) {
	t.Parallel()
	for name, k := range map[string]*keyed{
		"the password rule":           passwordRule,
		"the pending password key":    pendingPassword,
		"words folding outside ASCII": keyedOn([]string{"key", "ſecret", "sk"}, `\s*=\S+`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pieces := []string{
				":", "=", " ", "\t", "\n", "\r\n", "x", "日", "\xff", "hunter2", Marker + "password]",
			}
			for _, word := range k.words {
				pieces = append(pieces, spellings(word)...)
				runes := []rune(word)
				for n := 1; n < len(runes); n++ {
					pieces = append(pieces, string(runes[:n]), string(runes[n:]))
				}
			}
			r := k.rule(Marker + "test]")
			rng := rand.New(rand.NewPCG(20261008, 1))
			for trial := range 2_000 {
				var b strings.Builder
				for range 1 + rng.IntN(12) {
					b.WriteString(pieces[rng.IntN(len(pieces))])
				}
				text := b.String()

				want := k.pattern.FindAllStringIndex(text, -1)
				var got [][]int
				for start, end, found := k.next(text, 0); found; start, end, found = k.next(text, end) {
					got = append(got, []int{start, end})
				}
				if !slices.EqualFunc(got, want, slices.Equal[[]int]) {
					t.Fatalf("trial %d, %q: the scan matched %v, the pattern %v", trial, text, got, want)
				}
				wantIndex := -1
				if len(want) > 0 {
					wantIndex = want[0][0]
				}
				if got := k.index(text); got != wantIndex {
					t.Fatalf("trial %d, %q: the scan's first match begins at %d, the pattern's at %d",
						trial, text, got, wantIndex)
				}
				if got, want := r.apply(text), k.pattern.ReplaceAllLiteralString(text, r.with); got != want {
					t.Fatalf("trial %d, %q: the scan replaced to %q, the pattern to %q", trial, text, got, want)
				}
				if got, want := r.in(text), k.pattern.MatchString(text); got != want {
					t.Fatalf("trial %d, %q: the scan says a match is there %v, the pattern %v", trial, text, got, want)
				}
			}
		})
	}
}

// spellings is word as (?i) reads it in several hands: as written, in capitals,
// alternating case, and with every rune moved one and two steps along its
// [unicode.SimpleFold] orbit — which is how password's s becomes ſ and a k the
// Kelvin sign.
func spellings(word string) []string {
	fold := func(s string) string {
		return strings.Map(unicode.SimpleFold, s)
	}
	alternating := []rune(word)
	for i := range alternating {
		if i%2 == 1 {
			alternating[i] = unicode.ToUpper(alternating[i])
		}
	}
	return []string{word, strings.ToUpper(word), string(alternating), fold(word), fold(fold(word))}
}

// EVERY RULE OPENS WITH A LITERAL OR IS KEYED. Go's regexp skips ahead to a
// pattern's literal prefix with a byte scan and starts its machine only there;
// a pattern with none steps the machine through every byte of every text
// redacted, which is what the password rule cost before it was [keyed] — 42 ms
// of a 45 ms pass over a two-mebibyte transcript. A rule added without either
// would bring that back with every test still passing, so this is what notices.
//
// Mutation: build the password rule as a plain rule over its pattern, with no
// keyed scan, and this fails.
func TestEveryRuleOpensWithALiteralOrIsKeyed(t *testing.T) {
	t.Parallel()
	for _, r := range rules {
		if prefix, _ := r.pattern.LiteralPrefix(); prefix == "" && r.keyed == nil {
			t.Errorf("rule %q opens with no literal for the regexp to skip ahead on and is not keyed: "+
				"it would run over every byte of every text — build it with keyedOn", r.pattern)
		}
	}
}

// A KEYED PATTERN'S TAIL CANNOT ESCAPE ITS WORDS. The scan's whole claim is that
// no match begins anywhere but at a word, and that holds because the words open
// a group the rest of the pattern sits after. A tail that closed that group —
// `)|secret=(\S+` spliced in whole — would compile into a pattern matching
// secret=… where the scan never looks, so the redaction a reader of the pattern
// expects would silently not happen; [keyedOn] refuses it instead.
//
// Mutation: drop the compile of after on its own in [keyedOn] and the tail is
// accepted.
func TestAKeyedPatternsTailCannotEscapeItsWords(t *testing.T) {
	t.Parallel()
	for _, after := range []string{`)|secret=(\S+`, `\s*=\S+)|(?:token`} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("keyedOn accepted the tail %q, which lets a match begin outside the words", after)
				}
			}()
			keyedOn(passwordKeys, after)
		}()
	}
}

// BenchmarkSecrets is the pass over two mebibytes of a coding run's
// transcript, the largest text it is handed — the figure [keyed]'s doc comment
// cites.
//
// Measured on a shared 4-CPU box: 45 ms an op with the password rule stepped
// through every byte (42 ms of it that one rule), 5.7 ms with the scan; under
// the race detector 0.67 s before.
func BenchmarkSecrets(b *testing.B) {
	text := strings.Repeat("[tool] bash: go test ./pkg/000123/... "+strings.Repeat("-", 40)+"\n", 2<<20/80)
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		_ = Secrets(text)
	}
}
