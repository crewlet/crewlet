package redact

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
)

// ValuesBelowKeys is [valuesBelowKeys] for the package's external tests.
var ValuesBelowKeys = valuesBelowKeys

// ArmourEndFamily is [armourEndFamily] written out for the package's external
// tests: each case's name and text.
func ArmourEndFamily() [][2]string {
	var out [][2]string
	for _, c := range armourEndFamily() {
		out = append(out, [2]string{c.name(), c.text()})
	}
	return out
}

// armourEnd is one text of the family a value after a key, on a line ending
// in an armour's END: a password's key whose value is quoted, followed on its
// line by a piece of base64 and a key's END that ends the line or goes on to
// what follows it; and on the next line a word, the same quote, and a piece
// and an END. Every part of it is a field, so a case names what it varies.
type armourEnd struct {
	// key and sep are the password's key and its separator, quote what
	// follows the separator, piece how long the piece after the quote is,
	// label the armour's, and after what follows its END on the line: "",
	// a tail, a token's run and a tail, or an escaped line break.
	key, sep, quote string
	piece           int
	label, after    string
	// word is the value's line's word, and gutter what that line opens
	// with, the key's line too where both is true.
	word, gutter string
	both         bool
	// differs is how the value's line goes on past its quote otherwise than
	// the key's line does past its separator, "" where alike: another
	// piece, another label, no piece, or another tail.
	differs string
	// pairs is how many key's lines there are, each with its value's line
	// under it, and further whether a line alike to the value's follows
	// each.
	pairs   int
	further bool
}

// armourEndDefault is the probe's shape: a password's key and a quote, a piece
// of 56 bytes and an RSA key's END ending the line, and a word on the line
// below.
var armourEndDefault = armourEnd{
	key: "Password", sep: ": ", quote: `"`, piece: 56, label: "RSA PRIVATE KEY",
	word: "plainvalue", pairs: 1,
}

// armourEndRun is the token's run a case writes after the END: an API key's
// shape, a placeholder.
var armourEndRun = "sk-ant-" + strings.Repeat("Ab3Cd4", 8)

// armourEndPiece is n bytes of a key's line, repeated, starting at its
// offset from: a piece of a key's base64 as long as a case asks.
func armourEndPiece(n, from int) string {
	line := strings.Repeat(testKeyLine, n/len(testKeyLine)+2)
	return line[from : from+n]
}

// text is the case written out.
func (c armourEnd) text() string {
	rest := func(piece int, from int, label, after string) string {
		s := c.quote
		if piece > 0 {
			s += " " + armourEndPiece(piece, from)
		}
		return s + " -----END " + label + "-----" + after
	}
	keyRest := rest(c.piece, 0, c.label, c.after)
	valueRest := keyRest
	switch c.differs {
	case "another piece":
		valueRest = rest(max(c.piece, 8), 7, c.label, c.after)
	case "another label":
		valueRest = rest(c.piece, 0, "EC PRIVATE KEY", c.after)
		if c.label == "EC PRIVATE KEY" {
			valueRest = rest(c.piece, 0, "RSA PRIVATE KEY", c.after)
		}
	case "no piece":
		valueRest = rest(0, 0, c.label, c.after)
	case "another tail":
		valueRest = rest(c.piece, 0, c.label, " other tail |")
	}
	keyGutter := ""
	if c.both {
		keyGutter = c.gutter
	}
	var b strings.Builder
	for i := range c.pairs {
		word := c.word
		if i > 0 {
			word += strconv.Itoa(i)
		}
		b.WriteString(keyGutter + c.key + c.sep + keyRest + "\n")
		b.WriteString(c.gutter + word + " " + valueRest + "\n")
		if c.further {
			b.WriteString(c.gutter + "further " + valueRest + "\n")
		}
	}
	return b.String()
}

// name says how the case differs from [armourEndDefault].
func (c armourEnd) name() string {
	d := armourEndDefault
	var parts []string
	add := func(differs bool, f string, a ...any) {
		if differs {
			parts = append(parts, fmt.Sprintf(f, a...))
		}
	}
	add(c.key != d.key || c.sep != d.sep, "key %q", c.key+c.sep)
	add(c.quote != d.quote, "quote %q", c.quote)
	add(c.piece != d.piece, "a piece of %d", c.piece)
	add(c.label != d.label, "label %q", c.label)
	add(c.after != d.after, "after the END %s", c.afterName())
	add(c.word != d.word, "word %q", c.word)
	add(c.gutter != d.gutter, "gutter %q (the key's line too: %v)", c.gutter, c.both)
	add(c.differs != "", "the value's line with %s", c.differs)
	add(c.pairs != d.pairs, "%d pairs", c.pairs)
	add(c.further, "a further line")
	if len(parts) == 0 {
		return "the probe's shape"
	}
	return strings.Join(parts, ", ")
}

// afterName is what follows the END, a token's run in it named by its length.
func (c armourEnd) afterName() string {
	i := strings.Index(c.after, "sk-ant-")
	if i < 0 {
		return strconv.Quote(c.after)
	}
	n := strings.IndexByte(c.after[i:]+" ", ' ')
	return fmt.Sprintf("%q, a token's run of %d bytes, %q", c.after[:i], n, c.after[i+n:])
}

// armourEndFamily is the family a value after a key, on a line ending in an
// armour's END, varied in every part: the key's spelling and its separator,
// the quote, the piece's length from none to past a stage's step, the armour's
// label, what follows the END (nothing, a tail, a token's run, an escaped line
// break), the value's line's word and its gutter, whether that line goes on
// past its quote as the key's line does, and one or several such lines.
//
// The probe's three texts first, then each part varied alone from the probe's
// shape, then combinations drawn at random, seeded.
func armourEndFamily() []armourEnd {
	d := armourEndDefault
	cases := []armourEnd{d}
	for _, after := range []string{" " + armourEndRun + " tail |", " tail |"} {
		c := d
		c.after = after
		cases = append(cases, c)
	}
	keys := [][2]string{
		{"Password", ": "}, {"password", "="}, {"passwd", " = "}, {"PWD", ":"}, {"db_password", ":\t"},
		{"Root password", ": "}, {"DB_PASSWORD", "="},
	}
	quotes := []string{`"`, `'`, "`"}
	pieces := []int{0, 1, 8, 40, 56, 64, 77, 300, MaxReach - 40, MaxReach + 40, stageStep + 64}
	labels := []string{"RSA PRIVATE KEY", "PRIVATE KEY", "EC PRIVATE KEY", "OPENSSH PRIVATE KEY", "ENCRYPTED PRIVATE KEY", "PGP PRIVATE KEY BLOCK"}
	afters := []string{"", " tail |", " " + armourEndRun + " tail |", " " + armourEndRun, `\n`}
	words := []string{"plainvalue", "Xk9vAlue7Q", "va1ue", "the-value", "4567890", "UPPERVALUE"}
	gutters := []string{"", "  ", "\t", "+", "> ", "2026-10-06T09:00:00.123Z ", "     7→", "a.pem-3-", "u1SU1Lf9x= "}
	differs := []string{"", "another piece", "another label", "no piece", "another tail"}
	vary := func(f func(c *armourEnd)) {
		c := d
		f(&c)
		cases = append(cases, c)
	}
	for _, k := range keys {
		vary(func(c *armourEnd) { c.key, c.sep = k[0], k[1] })
	}
	for _, q := range quotes {
		vary(func(c *armourEnd) { c.quote = q })
	}
	for _, p := range pieces {
		vary(func(c *armourEnd) { c.piece = p })
	}
	for _, l := range labels {
		vary(func(c *armourEnd) { c.label = l })
	}
	for _, a := range afters {
		vary(func(c *armourEnd) { c.after = a })
	}
	for _, w := range words {
		vary(func(c *armourEnd) { c.word = w })
	}
	for _, g := range gutters {
		vary(func(c *armourEnd) { c.gutter, c.both = g, true })
		vary(func(c *armourEnd) { c.gutter = g })
	}
	for _, v := range differs {
		vary(func(c *armourEnd) { c.differs = v })
	}
	for n := 1; n <= 3; n++ {
		vary(func(c *armourEnd) { c.pairs = n })
		vary(func(c *armourEnd) { c.pairs, c.further = n, true })
	}
	r := rand.New(rand.NewPCG(1170, 1000))
	pick := func(n int) int { return r.IntN(n) }
	for range 400 {
		k := keys[pick(len(keys))]
		piece := pieces[pick(len(pieces)-3)]
		if pick(8) == 0 {
			// The longest pieces, rarely: each costs a reach or a step.
			piece = pieces[len(pieces)-3+pick(3)]
		}
		cases = append(cases, armourEnd{
			key: k[0], sep: k[1], quote: quotes[pick(len(quotes))], piece: piece,
			label: labels[pick(len(labels))], after: afters[pick(len(afters))],
			word: words[pick(len(words))], gutter: gutters[pick(len(gutters))], both: pick(2) == 0,
			differs: differs[pick(len(differs))], pairs: 1 + pick(3), further: pick(4) == 0,
		})
	}
	return cases
}

// valuesBelowKeys is every value the password's rule reads in text as
// written ([claimed]) on a line below its key, as the text it is: what a
// text of the family must not show once redacted.
func valuesBelowKeys(text string) []string {
	w := passwordInput(text, nil)
	password := rules()[len(rules())-1]
	var values []string
	var prior [][2]int
	for from := 0; ; {
		key, value, ok := password.decide(w.text, from, prior)
		if !ok {
			return values
		}
		if value[1] <= value[0] {
			from = beyond(w.text, key[0])
			continue
		}
		if strings.Contains(w.text[key[0]:value[0]], "\n") {
			values = append(values, text[w.at(value[0]):w.at(value[1])])
		}
		prior, from = append(prior, value), value[1]
	}
}

// A STREAM WRITES WHAT SECRETS DOES AFTER A KEY ON A LINE ENDING IN AN ARMOUR'S
// END, its value claimed apart from its key as [claimed] claims it: over the
// probe's three texts and every eightieth case of the rest of the family,
// after a stretch longer than a step and again with a token's run after the
// END longer than a step, cut at random, at the stream's own hold Close
// answers nil and the stream wrote Secrets, and at a hold of 64 KiB it wrote
// Secrets or refused ([checkReading]); and so over the probe's shape after a
// run longer than a window, a run past the stream's own hold after it.
//
// Mutation: claim a value as running from where its separator ends
// ([claimSource.word] for [claimSource.valued]), and the stream reads the
// piece on the key's line as no key's where Secrets reads it as one.
func TestAStreamWritesWhatSecretsDoesAfterAKeyOnALineEndingInAnArmoursEnd(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 62))
	filler := strings.Repeat("the build passed ok\n", stageStep/20+1)
	exact := func(name, text string, got reading) {
		t.Helper()
		if got.err != nil {
			t.Errorf("%s at the stream's own hold: Close answered %v", name, got.err)
			return
		}
		if want := Secrets(text); got.out != want {
			t.Errorf("%s at the stream's own hold: the stream wrote another text than Secrets %s", name, parted(got.out, want))
		}
	}
	for i, c := range armourEndFamily() {
		if i >= 3 && i%80 != 0 {
			// The probe's three, and every eightieth of the rest.
			continue
		}
		run := c
		run.after = " sk-ant-" + testRun(r, testB64url, stageStep+r.IntN(stageStep)) + " tail |"
		for _, c := range []armourEnd{c, run} {
			text := filler + c.text() + "done\n"
			exact(c.name(), text, readHolding(text, holdBytes(), r, 96<<10))
			checkReading(t, c.name()+" at a hold of 64 KiB", text, readHolding(text, 64<<10, r, 96<<10))
		}
	}
	// The probe's shape after a run, and a run past the hold a step and a
	// window on: straight after it, the password's reader holds a step of
	// its input past the key, which the run's marker makes as long as the
	// run, so its claims come only past the hold, and the stream refuses
	// ([ErrDecidedEarly]).
	before := "x sk-" + testRun(r, testB64url, 300<<10) + " |\n" + armourEndDefault.text()
	past := "sk-ant-" + testRun(r, testB64url, holdBytes()+2*stageStep) + "\n" + filler
	text := before + strings.Repeat("the build passed ok\n", (stageStep+streamWindow)/20) + past
	exact("the probe's shape after a run, a run past the hold a step on", text, readHolding(text, holdBytes(), r, 256<<10))
}

// A STREAM CLAIMS A VALUE IT READS OPEN APART FROM ITS KEY, as [claimed]
// claims every value: below a BEGIN no END closes, a password's key on a line
// a cut wrote into, its value longer than a stage's window, so the password's
// reader reads it on past each window ([ruleStage.open]); between them three
// of the wrapping's blank lines, or the value on a string's escaped line. A
// block read forward asks each gutter between key and value whether a rule
// claims it, and at the stream's own hold the stream wrote Secrets.
//
// Mutation: claim an open value from where its separator ends
// ([claimSource.word] in [ruleStage.open]), and the block ends a line short
// of where Secrets ends it.
func TestAStreamClaimsAValueItReadsOpenApartFromItsKey(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 64))
	filler := strings.Repeat("the build passed ok\n", stageStep/20+1)
	for _, g := range []string{testKeyLine[:56] + " ", "u1SU1Lf= "} {
		blanks := testBegin + "\n" + g + "Password:\n" + strings.Repeat(g+"\n", 3) + g + "placeholder9x" +
			strings.Repeat("y", 300<<10) + "\n" + g + "done\n"
		escaped := `{"log": "` + testBegin + `\n` + g + testKeyLine + `\n` + g + `passwd:\n` + g + "placeholder9x" +
			strings.Repeat(`\ny-y.y-y.y`, 30000) + `\n` + g + `done\n"}` + "\n"
		for _, shape := range []string{blanks, escaped} {
			text := filler + shape + filler
			got := readHolding(text, holdBytes(), r, 96<<10)
			if got.err != nil {
				t.Errorf("gutter %q: Close answered %v at the stream's own hold", g, got.err)
				continue
			}
			if want := Secrets(text); got.out != want {
				t.Errorf("gutter %q: the stream wrote another text than Secrets %s", g, parted(got.out, want))
			}
		}
	}
}
