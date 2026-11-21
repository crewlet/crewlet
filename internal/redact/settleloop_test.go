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

// heldText is a text the sweep is held to the loop over, and whether every
// line end of it is checked or a sample.
type heldText struct {
	name, text string
	every      bool
}

// heldRunShapes are the lines a long run of holds is made of: each password's
// key decided by the next line, which holds a key of its own.
var heldRunShapes = []string{"| Password: |", "**Password:**", "- **Password:**", "password:", `"password": "`, "pwd="}

// heldTexts are texts Settled is held to at their line ends: transcripts of
// [transcriptLines], random short shapes of keys, armours and password's keys,
// written plainly and inside strings, [heldStructured] texts, the key
// fixtures, real keys in every wrapping, the family of a password's key after
// an END on its line, the gutter and seam shapes and the lines of password's
// keys ([redact.HeldTexts]), [blocksBeforeAKey]; and long runs of holds, alone,
// after an armour, and a run of keys then blank lines. Every other text a test
// asks Settled about is held to the loop where it asks ([settledByBoth]).
func heldTexts(r *rand.Rand) []heldText {
	var texts []heldText
	add := func(name, text string) {
		texts = append(texts, heldText{name, text, strings.Count(text, "\n") <= 64})
	}
	for i := range 300 {
		var b strings.Builder
		for range 1 + r.IntN(30) {
			b.WriteString(transcriptLines[r.IntN(len(transcriptLines))] + "\n")
		}
		add(fmt.Sprintf("transcript %d", i), b.String())
	}
	toks := []string{"password: ", "pwd: ", "x ", "passwd ", `"password": "`, "-----BEGIN RSA PRIVATE KEY-----",
		"-----BEGIN RSA PRIVATE KEY-----" + `\n`, keyLine, `\n`, "7\u2192", " ", ":", `"`, "|", "pwd=",
		"-----END RSA PRIVATE KEY-----", "**Password:**", "| Password: |", "- ", keyLikeRun,
		`\"password\": \"`, `{"log":"{\"password\": \"`, `\\\"password\\\": \\\"`, `\"`}
	for i := range 1000 {
		var b strings.Builder
		for range 1 + r.IntN(6) {
			for range 1 + r.IntN(5) {
				b.WriteString(toks[r.IntN(len(toks))])
			}
			b.WriteString([]string{"\n", "\n", "\n\n", `\n` + "\n"}[r.IntN(4)])
		}
		b.WriteString([]string{"hunter2secretvalue9\n", "ok\n", "\n"}[r.IntN(3)])
		add(fmt.Sprintf("random shape %d", i), b.String())
	}
	for i := range 600 {
		add(fmt.Sprintf("structured text %d", i), heldStructured(r))
	}
	fixtures := keyFixtures()
	for _, name := range slices.Sorted(maps.Keys(fixtures)) {
		add("key fixture "+name, fixtures[name])
	}
	keys, forms := realKeys(), wrappings()
	for _, kname := range slices.Sorted(maps.Keys(keys))[:4] {
		var b strings.Builder
		for _, wname := range slices.Sorted(maps.Keys(forms)) {
			b.WriteString(transcriptLines[r.IntN(len(transcriptLines))] + "\n" + forms[wname].wrap(keys[kname]).text + "\n")
		}
		add("real keys in every wrapping, "+kname, b.String())
	}
	for _, c := range redact.ArmourEndFamily() {
		add("armour's END family, "+c[0], c[1])
	}
	held := redact.HeldTexts()
	for _, name := range slices.Sorted(maps.Keys(held)) {
		add(name, held[name])
	}
	for i, text := range blocksBeforeAKey() {
		add(fmt.Sprintf("a block before a password's key, %d", i), text)
	}
	armour := "k: -----BEGIN RSA PRIVATE KEY-----\n" + keyLine + "\n\n"
	for _, line := range heldRunShapes {
		run := strings.Repeat(line+"\n", 400) + "hunter2secretvalue9\nok\n"
		add(fmt.Sprintf("%q x 400", line), run)
		add(fmt.Sprintf("an armour, then %q x 400", line), armour+run)
	}
	add("key lines, then blank lines", strings.Repeat("password:\n", 200)+strings.Repeat("\n", 400)+"hunter2secretvalue9\nok\n")
	add("an armour, key lines, then blank lines", armour+strings.Repeat("pwd:\n", 200)+strings.Repeat("\n", 400)+"ok\n")
	return texts
}

// SETTLED IN ONE PASS ANSWERS WHAT A HOLD A PASS DOES: over [heldTexts], at
// every line end of a short text and the first and last ten and about ten more
// of a long one, [redact.Settled]'s sweep of the holds settles exactly where a
// loop resolving one hold a pass settles ([redact.SettledTwoWays]).
//
// Mutation: lower no rewritable point for a claim beside a block, move a
// point to the line above a hold's, start a hold's or a block's bar a line
// early, end a block's a byte short, stop the sweep a bar early, or count an
// unread hold at the end's rewritable point rather than the point's, and a
// text settles elsewhere.
func TestSettledInOnePassAnswersWhatAHoldAPassDoes(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 3))
	cuts := 0
	for _, tx := range heldTexts(r) {
		var ends []int
		for i := range len(tx.text) {
			if tx.text[i] == '\n' {
				ends = append(ends, i+1)
			}
		}
		for i, c := range ends {
			if !tx.every && i >= 10 && i < len(ends)-10 && r.IntN(len(ends)) >= 10 {
				continue
			}
			cuts++
			if got, want := redact.SettledTwoWays(tx.text[:c]); got != want {
				t.Fatalf("%s, at %d: Settled settled %d, and a hold a pass %d; the text's end:\n%q", tx.name, c, got, want, tx.text[max(0, c-2000):c])
			}
		}
	}
	t.Logf("%d cuts", cuts)
}

// settledByBoth is [redact.Settled] of text, which it holds to a loop
// resolving one hold a pass ([redact.SettledTwoWays]): every text a test asks
// Settled about holds the sweep to the loop there too.
func settledByBoth(t testing.TB, text string) int {
	t.Helper()
	swept, looped := redact.SettledTwoWays(text)
	if swept != looped {
		t.Errorf("Settled settled %d, and a hold a pass %d; the text's end:\n%q", swept, looped, text[max(0, len(text)-2000):])
	}
	return swept
}

// blocksBeforeAKey are texts whose key's block, open on its BEGIN's line,
// meets a password's key on the next line after a one-byte gutter or an
// escaped break, under a wrapping or none, after a line of a key's END and a
// password's key or none.
func blocksBeforeAKey() []string {
	var texts []string
	for _, above := range []string{"", "  7\u2192passwd -----END RSA PRIVATE KEY-----\"| pwd: | x \n"} {
		for _, g := range []string{"", "  ", "| "} {
			for _, body := range []string{keyLine[:56] + ":", keyLine} {
				for _, lead := range []string{`\n7` + "\u2192", `\n+`, `\n3`, "7\u2192", "+"} {
					for _, key := range []string{"- **Password:**", "| Password: |", "password: "} {
						for _, below := range []string{"7\u2192" + `\r\nPassword: |`, "ok"} {
							texts = append(texts, above+g+"-----BEGIN RSA PRIVATE KEY-----"+body+"\n"+lead+key+`AKIAABCDEFGHIJKLMNOP"\n`+"\n"+below+"\n")
						}
					}
				}
			}
		}
	}
	return texts
}

// The pieces a [heldStructured] text is built of.
var (
	heldGutters  = []string{"", "", "| ", "7\u2192", "   32\t", "> ", "  ", "+", `"`, "- "}
	heldSuffixes = []string{"", "", "|passwd:", " passwd:", " password: ", "| Password: |", `"`, " pwd=", ` \n`, "|", " Password:"}
	heldKeys     = []string{"password: ", "password:", `"password": "`, `{"password": "`, "- **Password:**", "| Password: |", "pwd=", "passwd ", "pwd: ",
		"passwd:", "Passwd:", "Password: |", "password: >", `\"password\": \"`, "password:\"", "Password:", "export DB_PASSWORD=", "--password "}
	heldValues = []string{"hunter2secretvalue9", "Ab3f9C2d7E1b", `""`, "x", "ok", "-", "|", `"hunter2x9"`, "hunter2x9 # rotate", "", "-RgeLezV6", "[REDACTED:password]"}
	heldBodies = []string{keyLine, "RgeLezV6", keyLine[:56], keyLine[:56] + " " + keyLine[:56]}
	heldLabels = []string{"RSA PRIVATE KEY", "PRIVATE KEY", "OPENSSH PRIVATE KEY"}
)

// heldFragment is one piece of a [heldStructured], ending in a line
// break: a block closed or open under a gutter, a one-line key or a BEGIN
// after other text on its line, each perhaps with a password's key after it;
// a password's key with its value on its line or below, or above such a
// block; an END alone, a key's line, a chain of keys, a block value, a
// marker as a value, or a line of nothing much.
func heldFragment(r *rand.Rand) string {
	pick := func(of []string) string { return of[r.IntN(len(of))] }
	g, body, label := pick(heldGutters), pick(heldBodies), pick(heldLabels)
	begin, end := "-----BEGIN "+label+"-----", "-----END "+label+"-----"
	switch r.IntN(14) {
	case 0, 1:
		var b strings.Builder
		b.WriteString(g + begin + "\n")
		for range 1 + r.IntN(3) {
			line := g
			if r.IntN(4) == 0 {
				line = pick(heldGutters)
			}
			b.WriteString(line + body + "\n")
		}
		return b.String() + g + end + pick(heldSuffixes) + "\n"
	case 2:
		return g + begin + "\n" + strings.Repeat(g+body+"\n", r.IntN(3))
	case 3:
		return g + begin + body + pick([]string{end, ""}) + pick(heldSuffixes) + "\n"
	case 4:
		return g + `{"key": "` + begin + `\n` + body + `\n` + end + `\n"}` + "\n"
	case 5, 6:
		k := pick(heldKeys)
		if r.IntN(2) == 0 {
			return g + k + pick(heldValues) + "\n"
		}
		return g + k + "\n" + strings.Repeat(g+"\n", r.IntN(2)) + g + pick(heldValues) + "\n"
	case 7:
		return g + pick(heldKeys) + "\n" + g + pick([]string{"", "x ", body + " "}) + begin + body + pick([]string{end, ""}) + pick(heldSuffixes) + "\n" + g + pick(heldValues) + "\n"
	case 8:
		return g + end + pick(heldSuffixes) + "\n"
	case 9:
		return g + body + "\n"
	case 10:
		return strings.Repeat(g+pick(heldKeys)+"\n", 1+r.IntN(4))
	case 11:
		return g + pick([]string{"x ", "KEY=", pick(heldKeys)}) + begin + "\n" + g + body + "\n" + pick([]string{g + end + "\n", ""})
	case 12:
		return pick([]string{"Password: |\n  hunter2secretvalue9\n  ok\n", "password: [REDACTED:password]\n", "| " + begin + "\n> " + body + "\n7\u2192" + body + "\n   32\t" + end + "|passwd:\n"})
	}
	return pick([]string{"ok\n", "\n", "x\n", "  \n"})
}

// heldStructured is a text of [heldFragment]s, some run together
// mid-line or written with CRLF breaks, ending in a value, a line or not.
func heldStructured(r *rand.Rand) string {
	var b strings.Builder
	for range 2 + r.IntN(10) {
		f := heldFragment(r)
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
