package redact_test

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// settlingText is a text a settling is read over, and whether every line end
// of it is checked against Settled read whole, or a sample of them.
type settlingText struct {
	name, text string
	every      bool
}

// settlingWraps are the lines a password's key ends in under every wrapping
// a value is read below its key in, and settlingSeps the separators between
// a key's line and the next: each key's value decided by the line after it,
// which holds a key of its own.
var (
	settlingWraps = []string{"password: ", "password:", `"password": "`, `{"password": "`, "- **Password:**", "| Password: |", "pwd=", "passwd ", "7→pwd: ", "   32\tpassword: ", "a.pem-3-password: ", `\"password\": \"`, "Password: |", "password: >"}
	settlingSeps  = []string{"\n", "\n\n", "\n-\n", `\n` + "\n", "\n  \n"}
)

// counterexample is a line too long for a window ending in a password's key
// that awaits its separator, a key pending or a quoted key, then a line of k
// keys whose pairing that line's end decides, then a line that is the last
// key's value or not: k's parity decides which, and so whether the whole
// text settles from its start or from the long line's end.
func counterexample(filler string, sh [3]string, k int) string {
	return filler + sh[0] + "\n" + sh[1] + strings.Repeat(sh[2], k) + "\nhunter2secretvalue9\n" + strings.Repeat("ok\n", 5)
}

// counterexampleShapes are [counterexample]'s long line's end, the next
// line's head and its key.
var counterexampleShapes = [][3]string{
	{"passwd ", ": ", "password: "}, {"passwd ", ": ", `"password": "`}, {"passwd ", ": ", "pwd: "},
	{"password: ", "", "password: "}, {`"password": "`, "", `"password": "`},
}

// settlingTexts is every text Settled is read in pieces over: transcripts of
// every shape; the keys' corpus ([streamCorpus], each wrapping once and its
// text between keys cut short), a text of every shape a stage carries and six
// hold shapes, their lines of `ok` cut to 30; the counterexamples past a reach
// and a megabyte, at both parities; chains of keys each decided by the next
// key's line in every wrapping, and long lines of keys before a key pending
// at their end; random short shapes of keys, armours, escaped breaks and
// gutters; and a key 64 KiB and a byte, and 128 KiB, into a line under a
// pipe's, a word's and a quote's gutter and in a string with its breaks
// escaped.
func settlingTexts(r *rand.Rand) []settlingText {
	var texts []settlingText
	add := func(name, text string, every bool) { texts = append(texts, settlingText{name, text, every}) }
	for i := range 300 {
		var b strings.Builder
		for range 1 + r.IntN(30) {
			b.WriteString(transcriptLines[r.IntN(len(transcriptLines))])
			b.WriteByte('\n')
		}
		add(fmt.Sprintf("transcript %d", i), b.String(), true)
	}
	keys, forms := realKeys(), wrappings()
	for i, kname := range slices.Sorted(maps.Keys(keys))[:4] {
		var b strings.Builder
		for j, wname := range slices.Sorted(maps.Keys(forms)) {
			if j%4 == i {
				b.WriteString(redact.StreamText(r, r.IntN(2<<10)) + "\n" + forms[wname].wrap(keys[kname]).text + "\n")
			}
		}
		add("the keys' corpus, "+kname, b.String(), false)
	}
	add("every shape a stage carries", redact.StreamText(r, 256<<10), false)
	for _, s := range redact.HoldShapes(300 << 10) {
		if slices.Contains([]string{"a1", "b1", "e1", "g1", "p1", "pa1"}, s[0]) {
			text := s[1]
			for strings.HasSuffix(text, "\nok\n") {
				text = strings.TrimSuffix(text, "ok\n")
			}
			add("hold shape "+s[0], text+strings.Repeat("ok\n", 30), false)
		}
	}
	for _, sh := range counterexampleShapes {
		for _, k := range []int{7000, 7001} {
			add(fmt.Sprintf("a 70 KiB line ending %q, then %q and %q x %d", sh[0], sh[1], sh[2], k),
				counterexample("filler "+strings.Repeat("x", 70<<10)+" ", sh, k), true)
		}
		for _, k := range []int{30000, 30001} {
			add(fmt.Sprintf("a 1.1 MiB line ending %q, then %q and %q x %d", sh[0], sh[1], sh[2], k),
				counterexample("filler "+strings.Repeat("x", 1100<<10)+" ", sh, k), false)
		}
	}
	for _, w := range settlingWraps {
		for _, sep := range settlingSeps {
			for _, n := range []int{1, 3, 40} {
				add(fmt.Sprintf("a chain of %q x %d over %q", w, n, sep), strings.Repeat(w+sep, n)+"hunter2secretvalue9\nok\nok\n", true)
			}
		}
		for _, k := range []int{3000, 3001} {
			add(fmt.Sprintf("a line of %q x %d", w, k), "x "+strings.Repeat(w+" ", k)+"\nhunter2secretvalue9\n"+strings.Repeat(w+"\n", 5)+"ok\n", true)
		}
	}
	toks := []string{"password: ", "pwd: ", "x ", "passwd ", `"password": "`, "-----BEGIN RSA PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----" + `\n`, "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun", `\n`, "7→", " ", ":", `"`, "|", "pwd=", "-----END RSA PRIVATE KEY-----", "**Password:**", "| Password: |", "- "}
	for i := range 2000 {
		var b strings.Builder
		for range 1 + r.IntN(6) {
			for range 1 + r.IntN(5) {
				b.WriteString(toks[r.IntN(len(toks))])
			}
			b.WriteString([]string{"\n", "\n", "\n\n", `\n` + "\n"}[r.IntN(4)])
		}
		b.WriteString([]string{"hunter2secretvalue9\n", "ok\n", "\n"}[r.IntN(3)])
		add(fmt.Sprintf("random shape %d", i), b.String(), true)
	}
	for _, at := range []int{64<<10 + 1, 128 << 10} {
		pad := func(prefix string) string { return prefix + strings.Repeat("x", at-len(prefix)-1) + " " }
		add(fmt.Sprintf("a key %d bytes into a line under a pipe", at), pad("| ")+"password: \n| hunter2secretvalue9\n| ok\n", true)
		add(fmt.Sprintf("a key %d bytes into a line under a word's gutter", at), pad("   32\t")+"password: \n   33\thunter2secretvalue9\n   34\tok\n", true)
		add(fmt.Sprintf("a key %d bytes into a line under a quote", at), pad(`"`)+"password: \n\"hunter2secretvalue9\n\"ok\n", true)
		add(fmt.Sprintf("a key %d bytes into a string with its breaks escaped", at), pad(`{"log": "`)+`password:\n hunter2secretvalue9\n ok"}`+"\n", true)
	}
	return texts
}

// sampled is whether the i-th of n cuts of tx is checked: every one on a text
// marked so, else the first and last k and about k of the rest.
func (tx settlingText) sampled(r *rand.Rand, i, n, k int) bool {
	return tx.every || i < k || i >= n-k || r.IntN(n) < k
}

// SETTLED CHAINS AT ITS OWN POINT, AND NEVER FALLS: over [settlingTexts], at
// every line end in order (on a long text the first and last 30 and about 30
// more), Settled of the text so far read whole is never less than it was at
// the line end before, and it is that point and Settled of the text past it.
// A settling rests on both.
//
// Mutation: hold no line for a key's block still open, and Settled falls once
// its END arrives; hold a password's key from the line its value is decided
// on rather than its own, and a text settles further than its point and the
// text past it do.
func TestSettledChainsAtItsOwnPointAndNeverFalls(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 42))
	for _, tx := range settlingTexts(r) {
		var ends []int
		for i := range len(tx.text) {
			if tx.text[i] == '\n' {
				ends = append(ends, i+1)
			}
		}
		prev := 0
		for i, c := range ends {
			if !tx.sampled(r, i, len(ends), 30) {
				continue
			}
			got := redact.SettledWhole(tx.text[:c])
			if got < prev {
				t.Fatalf("%s, at %d: Settled fell to %d from %d", tx.name, c, got, prev)
			}
			if chained := prev + redact.SettledWhole(tx.text[prev:c]); got != chained {
				t.Fatalf("%s, at %d: Settled answered %d, and %d chained from %d", tx.name, c, got, chained, prev)
			}
			prev = got
		}
	}
}

// A TEXT READ IN PIECES SETTLES AS IT SETTLES WHOLE: over [settlingTexts],
// written to a settling whole, a line at a time and in random pieces
// ([settlingPlans]), after every write (on a long text the first and last ten
// and about ten more) what it has answered settled is exactly Settled of the
// text so far read whole, and it holds only what is past that.
//
// Mutation: ask Settled of only the last four reaches of the text past the
// point, and a text settles otherwise than whole.
func TestATextReadInPiecesSettlesAsItSettlesWhole(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 41))
	for _, tx := range settlingTexts(r) {
		for p, plan := range settlingPlans(r, tx) {
			var s redact.Settling
			settled, at := 0, 0
			for i, n := range plan {
				s.Write([]byte(tx.text[at : at+n]))
				at += n
				settled += s.Settle()
				if !tx.sampled(r, i, len(plan), 10) {
					continue
				}
				if want := redact.SettledWhole(tx.text[:at]); settled != want || s.Held() != at-settled {
					t.Fatalf("%s, plan %d, after %d bytes: settled %d and held %d; Settled of the text so far is %d", tx.name, p, at, settled, s.Held(), want)
				}
			}
		}
	}
}

// settlingPlans are the writes tx is read in: whole, in random pieces of up
// to 200 bytes and of up to 96 KiB, and a line at a time. On a long text the
// small pieces run to 8 KiB, and it is read a line at a time only if it has
// few lines: a settling asks at every line end, and reads again at each a
// stretch Settled holds back.
func settlingPlans(r *rand.Rand, tx settlingText) [][]int {
	plans := [][]int{{len(tx.text)}}
	for _, most := range []int{200, 96 << 10} {
		if most == 200 && !tx.every {
			most = 8 << 10
		}
		var plan []int
		for at := 0; at < len(tx.text); {
			n := min(len(tx.text)-at, 1+r.IntN(most))
			plan = append(plan, n)
			at += n
		}
		plans = append(plans, plan)
	}
	if !tx.every && strings.Count(tx.text, "\n") > 64 {
		return plans
	}
	var lines []int
	for at := 0; at < len(tx.text); {
		n := strings.IndexByte(tx.text[at:], '\n') + 1
		if n == 0 {
			n = len(tx.text) - at
		}
		lines = append(lines, n)
		at += n
	}
	return append(plans, lines)
}

// A SETTLING ASKS ONLY ONCE A LINE HAS ENDED: over [settlingTexts] in random
// pieces (of up to 200 bytes, or 8 KiB on a long text), it asks
// Settled's readers once for each write that brings a line break and for none
// other, so a line of any length written in any number of pieces is read once
// its line ends, and not again on every piece.
//
// Mutation: look for a line break in all it holds rather than in what came
// since it last asked, and a write with none asks again.
func TestASettlingAsksOnlyOnceALineHasEnded(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 43))
	for _, tx := range settlingTexts(r) {
		plan := settlingPlans(r, tx)[1]
		var s redact.Settling
		breaks, at := 0, 0
		for _, n := range plan {
			piece := tx.text[at : at+n]
			at += n
			if strings.IndexByte(piece, '\n') >= 0 {
				breaks++
			}
			s.Write([]byte(piece))
			_ = s.Settle()
			if s.Asks() != breaks {
				t.Fatalf("%s, after %d bytes: asked %d times, where %d writes brought a line break", tx.name, at, s.Asks(), breaks)
			}
		}
	}
}

// A SETTLING LETS A LONG LINE'S BUFFER GO ONCE IT SETTLES: a line of 2 MiB,
// written in 4 KiB pieces among short lines, is held whole until it ends,
// and once Settled settles it the settling keeps no more buffer than an
// ordinary text needs ([redact.SettlingKeeps]).
//
// Mutation: keep the buffer however large it grew, and the settling holds
// the long line's size until it is gone.
func TestASettlingLetsALongLinesBufferGo(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("ok\n", 100) + "data " + strings.Repeat("x", 2<<20) + "\n" + strings.Repeat("ok\n", 100)
	var s redact.Settling
	most := 0
	for at := 0; at < len(text); at += 4 << 10 {
		s.Write([]byte(text[at:min(len(text), at+4<<10)]))
		_ = s.Settle()
		most = max(most, s.Held())
	}
	if most < 2<<20-4<<10 || s.Held() != 0 || s.Kept() > redact.SettlingKeeps {
		t.Errorf("a 2 MiB line held %d bytes at most; settled, it holds %d and keeps %d bytes of buffer, past %d", most, s.Held(), s.Kept(), redact.SettlingKeeps)
	}
}

// BenchmarkSettled is what Settled costs over a text read whole, and read as
// it arrives in 16 KiB writes, each asked of: a plain log, a mix of every
// shape a stage carries, password lines, and lines dense with tokens. Its
// MB/s is the text's.
func BenchmarkSettled(b *testing.B) {
	r := rand.New(rand.NewPCG(1666, 9))
	const size = 8 << 20
	repeat := func(line string) string { return strings.Repeat(line, size/len(line)+1)[:size] }
	for _, c := range []struct{ name, text string }{
		{"plain log", repeat("2026-11-19T10:00:00Z INFO build step 12 of 40 passed in 0.412s\n")},
		{"mixed", redact.StreamText(r, size)},
		{"passwords", repeat("export DB_PASSWORD=hunter2x9 # rotate the password: monthly\n")},
		{"token-dense", repeat("Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJlLXZhbHVl sk-" + strings.Repeat("Ab3Cd4", 8) + "\n")},
	} {
		b.Run(c.name+"/whole", func(b *testing.B) {
			b.SetBytes(int64(len(c.text)))
			for b.Loop() {
				_ = redact.Settled(c.text)
			}
		})
		b.Run(c.name+"/as it arrives", func(b *testing.B) {
			text := []byte(c.text)
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			for b.Loop() {
				var s redact.Settling
				for at := 0; at < len(text); at += 16 << 10 {
					s.Write(text[at:min(len(text), at+16<<10)])
					_ = s.Settle()
				}
			}
		})
	}
}
