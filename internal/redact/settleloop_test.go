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

// heldTexts are texts Settled is held to at their line ends, beside
// [settlingTexts], which the settling tests hold so at each line end they ask
// ([settlingText.whole]): random short shapes of [settlingToks], key-like runs
// and keys inside escaped strings, the key fixtures, the family of a
// password's key after an END on its line, the gutter and seam shapes and the
// lines of password's keys ([redact.HeldTexts]), [blocksBeforeAKey]; and long
// runs of holds, alone, after an armour, and a run of keys then blank lines.
// Every other text a test asks Settled about is held to the loop where it
// asks ([redact.SettledByBoth]).
func heldTexts(r *rand.Rand) []heldText {
	var texts []heldText
	add := func(name, text string) {
		texts = append(texts, heldText{name, text, strings.Count(text, "\n") <= 64})
	}
	toks := append(slices.Clip(settlingToks), keyLikeRun, `\"password\": \"`, `{"log":"{\"password\": \"`, `\\\"password\\\": \\\"`, `\"`)
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
	fixtures := keyFixtures()
	for _, name := range slices.Sorted(maps.Keys(fixtures)) {
		add("key fixture "+name, fixtures[name])
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
