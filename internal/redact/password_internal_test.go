package redact

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// THE PASSWORD SCAN FINDS EXACTLY WHAT THE PATTERN FINDS. [passwordRule] and
// [pendingPassword] are tried only where a key could begin ([passwordKey])
// rather than stepped through every byte, which is exact only if no match can
// begin anywhere else. Fed every way the key's spellings, their case folds (ſ
// for s, which the pattern's (?i) matches), its separators and the bytes around
// them can follow one another, the scan's matches, its replacement, its answer
// to "is there one" and its pending key are the pattern's own.
//
// Seeded, so a failure names a text that fails every time.
//
// Mutation: leave w and W out of what [passwordKey] accepts after a p, and
// every pwd is missed; resume after a match from its start rather than its
// end, and the scan's replacements overlap where the pattern's never do.
func TestThePasswordScanFindsWhatThePatternFinds(t *testing.T) {
	t.Parallel()
	pieces := []string{
		"password", "PASSWORD", "PaSsWoRd", "paſſword", "PAſſWD", "passwd", "pwd", "PWD",
		"pass", "word", "pW", "Pa", "p", "P", "a", "w", "d", "s", "ſ",
		":", "=", " ", "\t", "\n", "\r\n", "x", "日", "hunter2", Marker + "password]",
	}
	r := rules[slices.IndexFunc(rules, func(r rule) bool { return r.pattern == passwordRule })]
	rng := rand.New(rand.NewPCG(20261008, 1))
	for trial := range 5_000 {
		var b strings.Builder
		for range 1 + rng.IntN(12) {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		text := b.String()

		want := passwordRule.FindAllStringIndex(text, -1)
		var got [][]int
		for start, end, found := r.next(text, 0); found; start, end, found = r.next(text, end) {
			got = append(got, []int{start, end})
		}
		if !slices.EqualFunc(got, want, slices.Equal[[]int]) {
			t.Fatalf("trial %d, %q: the scan matched %v, the pattern %v", trial, text, got, want)
		}
		if got, want := r.apply(text), passwordRule.ReplaceAllString(text, r.with); got != want {
			t.Fatalf("trial %d, %q: the scan replaced to %q, the pattern to %q", trial, text, got, want)
		}
		if got, want := r.in(text), passwordRule.MatchString(text); got != want {
			t.Fatalf("trial %d, %q: the scan says a match is there %v, the pattern %v", trial, text, got, want)
		}
		wantPending := -1
		if loc := pendingPassword.FindStringIndex(text); loc != nil {
			wantPending = loc[0]
		}
		if got := pendingPasswordIndex(text); got != wantPending {
			t.Fatalf("trial %d, %q: the scan's pending key is at %d, the pattern's at %d",
				trial, text, got, wantPending)
		}
	}
}

// BenchmarkSecrets is the pass over two mebibytes of a coding run's
// transcript, the largest text it is handed — the figure the password scan's
// doc comment cites.
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
