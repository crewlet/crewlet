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
// every shape; the keys' corpus ([streamCorpus]'s keys, each in every
// wrapping, a transcript's line between keys), a text of every shape a stage
// carries, and an armour beside password's keys and runs of 32 KiB, as the
// hold shapes set them; the counterexamples past a reach and a megabyte, at
// both parities; chains of keys each decided by the next key's line in every
// wrapping, and long lines of keys before a key pending at their end; random
// short shapes of keys, armours, escaped breaks and gutters, three where a
// password's key follows an END on its line, [keyLineAboveABlock] and
// [structuredText]s; and a key 64 KiB and a byte, and 128 KiB, into a line
// under a pipe's, a word's and a quote's gutter and in a string with its
// breaks escaped.
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
	for _, kname := range slices.Sorted(maps.Keys(keys))[:4] {
		var b strings.Builder
		for _, wname := range slices.Sorted(maps.Keys(forms)) {
			b.WriteString(transcriptLines[r.IntN(len(transcriptLines))] + "\n" + forms[wname].wrap(keys[kname]).text + "\n")
		}
		add("the keys' corpus, "+kname, b.String(), false)
	}
	add("every shape a stage carries", redact.StreamText(r, 256<<10), false)
	head := "k: -----BEGIN RSA PRIVATE KEY-----\n" + keyLine + "\n\n"
	run := strings.Repeat("Ab3Cd4ef", 4<<10)
	ok := strings.Repeat("ok\n", 30)
	add("an armour, password's keys and a run", head+strings.Repeat("pwd:\n", 1200)+"sk-"+run+"\n"+ok, false)
	add("armours among tokens and password's keys", strings.Repeat(head+strings.Repeat("sk-"+run[:40]+" pwd:x\n", 64), 16)+ok, false)
	add("an armour, a run and its END", head+run+"\n-----END RSA PRIVATE KEY-----\n"+ok, false)
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
	for i, text := range []string{
		"\t\"password:\\n\" +\n+-----END RSA PRIVATE KEY-----|passwd:\nAb3f9C2d7E1b pulled\n     2→839201\n[...truncated]\n+-----END RSA PRIVATE KEY-----|passwd:\n",
		" **Password:**\npwd=\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun-----END RSA PRIVATE KEY-----| Password: | \n",
		"\\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gunpwd=\\n\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gunMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun-----END RSA PRIVATE KEY----- password: \n",
	} {
		add(fmt.Sprintf("a password's key after an END on its line, %d", i), text+"hunter2secretvalue9\nok\n", true)
	}
	for i, text := range keyLineAboveABlock {
		add(fmt.Sprintf("a key's line above a block on a password's key's line, %d", i), text, true)
	}
	for i := range 2000 {
		add(fmt.Sprintf("structured text %d", i), structuredText(r), true)
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

// keyLineAboveABlock are texts whose line of a password's key sits above a
// block on another password's key's line, cut apart from it only once the
// line below gives that key its value: until then the line above may read
// the gutter the cut leaves as its value.
var keyLineAboveABlock = []string{
	"passwd:\n-----BEGIN PRIVATE KEY-----RgeLezV6|passwd:\n-RgeLezV6\n",
	"passwd:\n-----BEGIN PRIVATE KEY-----RgeLezV6|passwd:\n-6",
	"Passwd:\n-----BEGIN PRIVATE KEY-----RgeLezV6 passwd:\n-RgeLezV6\n",
	"Passwd:\n-----BEGIN PRIVATE KEY-----RgeLezV6 passwd:\n-V",
	"passwd:\n-----BEGIN PRIVATE KEY-----RgeLezV6|passwd:\n-RgeLezV6\nok\n",
}

// The pieces a [structuredText] is built of.
var (
	structuredGutters  = []string{"", "", "| ", "7\u2192", "   32\t", "> ", "  ", "+", `"`, "- "}
	structuredSuffixes = []string{"", "", "|passwd:", " passwd:", " password: ", "| Password: |", `"`, " pwd=", ` \n`, "|", " Password:"}
	structuredKeys     = []string{"password: ", "password:", `"password": "`, `{"password": "`, "- **Password:**", "| Password: |", "pwd=", "passwd ", "pwd: ",
		"passwd:", "Passwd:", "Password: |", "password: >", `\"password\": \"`, "password:\"", "Password:", "export DB_PASSWORD=", "--password "}
	structuredValues = []string{"hunter2secretvalue9", "Ab3f9C2d7E1b", `""`, "x", "ok", "-", "|", `"hunter2x9"`, "hunter2x9 # rotate", "", "-RgeLezV6", "[REDACTED:password]"}
	structuredBodies = []string{keyLine, "RgeLezV6", keyLine[:56], keyLine[:56] + " " + keyLine[:56]}
	structuredLabels = []string{"RSA PRIVATE KEY", "PRIVATE KEY", "OPENSSH PRIVATE KEY"}
)

// structuredFragment is one piece of a [structuredText], ending in a line
// break: a block closed or open under a gutter, a one-line key or a BEGIN
// after other text on its line, each perhaps with a password's key after it;
// a password's key with its value on its line or below, or above such a
// block; an END alone, a key's line, a chain of keys, a block value, a
// marker as a value, or a line of nothing much.
func structuredFragment(r *rand.Rand) string {
	pick := func(of []string) string { return of[r.IntN(len(of))] }
	g, body, label := pick(structuredGutters), pick(structuredBodies), pick(structuredLabels)
	begin, end := "-----BEGIN "+label+"-----", "-----END "+label+"-----"
	switch r.IntN(14) {
	case 0, 1:
		var b strings.Builder
		b.WriteString(g + begin + "\n")
		for range 1 + r.IntN(3) {
			line := g
			if r.IntN(4) == 0 {
				line = pick(structuredGutters)
			}
			b.WriteString(line + body + "\n")
		}
		return b.String() + g + end + pick(structuredSuffixes) + "\n"
	case 2:
		return g + begin + "\n" + strings.Repeat(g+body+"\n", r.IntN(3))
	case 3:
		return g + begin + body + pick([]string{end, ""}) + pick(structuredSuffixes) + "\n"
	case 4:
		return g + `{"key": "` + begin + `\n` + body + `\n` + end + `\n"}` + "\n"
	case 5, 6:
		k := pick(structuredKeys)
		if r.IntN(2) == 0 {
			return g + k + pick(structuredValues) + "\n"
		}
		return g + k + "\n" + strings.Repeat(g+"\n", r.IntN(2)) + g + pick(structuredValues) + "\n"
	case 7:
		return g + pick(structuredKeys) + "\n" + g + pick([]string{"", "x ", body + " "}) + begin + body + pick([]string{end, ""}) + pick(structuredSuffixes) + "\n" + g + pick(structuredValues) + "\n"
	case 8:
		return g + end + pick(structuredSuffixes) + "\n"
	case 9:
		return g + body + "\n"
	case 10:
		return strings.Repeat(g+pick(structuredKeys)+"\n", 1+r.IntN(4))
	case 11:
		return g + pick([]string{"x ", "KEY=", pick(structuredKeys)}) + begin + "\n" + g + body + "\n" + pick([]string{g + end + "\n", ""})
	case 12:
		return pick([]string{"Password: |\n  hunter2secretvalue9\n  ok\n", "password: [REDACTED:password]\n", "| " + begin + "\n> " + body + "\n7\u2192" + body + "\n   32\t" + end + "|passwd:\n"})
	}
	return pick([]string{"ok\n", "\n", "x\n", "  \n"})
}

// structuredText is a text of [structuredFragment]s, some run together
// mid-line or written with CRLF breaks, ending in a value, a line or not.
func structuredText(r *rand.Rand) string {
	var b strings.Builder
	for range 2 + r.IntN(10) {
		f := structuredFragment(r)
		switch r.IntN(8) {
		case 0:
			f = strings.TrimSuffix(f, "\n")
		case 1:
			f = strings.ReplaceAll(f, "\n", "\r\n")
		}
		b.WriteString(f)
	}
	b.WriteString([]string{"hunter2secretvalue9\n", "ok\n", "\n", "", "ok", "x\n"}[r.IntN(6)])
	return b.String()
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
