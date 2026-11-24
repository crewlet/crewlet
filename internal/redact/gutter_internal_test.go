package redact

import (
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// gutterValue is the password's value every shape of [gutterShapes] holds once.
const gutterValue = "placeholder9x"

// gutterShape is a text in which a key's block holds the opening of a password
// key's line, and, where its blocks are read or written in two windows, where
// in the text the keys' stage's read boundary falls between the two.
type gutterShape struct {
	name, text string
	joins      []int
}

// gutterShapes is every way a key's block comes to hold the opening of a
// password key's line (the package doc's "A key's block beside a password"),
// under seven wrappings, with gutterValue below the key: a short line or the
// wrapping's lines above, a piece before the END, a bare BEGIN, a prompt above
// a value and a piece, an escaped string, a BEGIN no END closes with and
// without a body, a blank line between, and one key read as two blocks that
// join, its BEGIN's line and its body's opening differently; then the shapes
// the wrappings do not vary, a key's line with no gutter under a block whose
// first line opens as the value's line does among them.
func gutterShapes() []gutterShape {
	pc := testKeyLine[:56]
	wrap := func(g, s string, lines ...string) string {
		var b strings.Builder
		for _, l := range lines {
			b.WriteString(g + l + s + "\n")
		}
		return b.String()
	}
	var shapes []gutterShape
	add := func(name, text string, joins ...int) {
		shapes = append(shapes, gutterShape{name: name, text: text, joins: joins})
	}
	for _, w := range []struct{ name, g, s string }{
		{"echo", `echo "`, `" >> k`}, {"timestamp", "2026-10-06T09:00:00.123Z ", ""}, {"quote", "> ", ""},
		{"table", "| ", " |"}, {"numbered", "     7\u2192", ""}, {"key-piece", pc + " ", ""}, {"short", "u1SU1Lf= ", ""},
	} {
		g, s := w.g, w.s
		add("above bare, "+w.name, "u1SU1Lf=\n"+wrap(g, s, testEnd+"|passwd:", gutterValue))
		add("above wrapped, "+w.name, wrap(g, s, testKeyLine, testEnd+"|passwd:", gutterValue))
		add("piece on the line, "+w.name, wrap(g, s, pc+" "+testEnd+"|passwd:", gutterValue))
		add("begin bare, "+w.name, testBegin+"\n"+wrap(g, s, testKeyLine, testEnd+" Password:", gutterValue))
		add("prompt above, "+w.name, wrap(g, s, "Password:", "hunter2x9 "+pc, testKeyLine, testEnd+"|passwd:", gutterValue))
		add("escaped, "+w.name, `{"log": "u1SU1Lf=\n`+strings.ReplaceAll(wrap(g, "", testEnd+"|passwd:", gutterValue), "\n", `\n`)+"\"}\n")
		add("begin, no end, "+w.name, testBegin+"\n"+wrap(g, s, "passwd:", gutterValue))
		add("begin, a body, no end, "+w.name, testBegin+"\n"+wrap(g, s, testKeyLine, testKeyLine+" passwd:", gutterValue))
		add("a blank line between, "+w.name, wrap(g, s, pc+" "+testEnd+"|passwd:", "", gutterValue))
		// One key read as two blocks: the BEGIN's, ended by two lines that
		// interrupt its body, and the END's, read back across them into it.
		body := wrap(g, s, testKeyLine, testKeyLine[8:]+testKeyLine[:8]) + "some log line\nanother log line\n" + wrap(g, s, testKeyLine)
		for _, begin := range []string{testBegin + "\n", wrap(g, s, testBegin)} {
			name := "joined, the BEGIN bare, "
			text, lines := begin+body, body
			if begin != testBegin+"\n" {
				name = "joined, the body bare, "
				lines = strings.ReplaceAll(strings.ReplaceAll(body, g, ""), s+"\n", "\n")
				text = begin + lines
			}
			text += wrap(g, s, testEnd+"|passwd:", gutterValue, "done")
			end := strings.Index(text, testEnd)
			add(name+w.name, text, len(begin)+1, len(begin)+len(lines)/2, end-1)
		}
	}
	// The same key's two blocks each most of a reach long, its BEGIN's line
	// opening with the gutter and its body bare: the joined block straddles
	// what the keys' stage writes to, which waits at its start, and its cut
	// reads the opening of its first line, a window later.
	ts := "2026-10-06T09:00:00.123Z "
	long := ts + testBegin + "\n" + strings.Repeat(testKeyLine+"\n", 900) + "some log line\nanother log line\n" +
		strings.Repeat(testKeyLine+"\n", 800) + ts + testEnd + "|passwd:\n" + ts + gutterValue + "\n" + ts + "done\n"
	add("joined across a reach, the body bare, timestamp", long+strings.Repeat("ok\n", 60<<10), len(ts)+1)
	// Two keys on one line, the second's BEGIN after the first's END, and the
	// second's END opening the password key's line as the first's opens the
	// BEGIN's: the first is written a step before the second, whose cut still
	// reads where the first ends ([keyStage.wrote]); and with a key before
	// them written in that step too, where the last it wrote ends.
	colons := strings.ReplaceAll(testEnd, "-", ":")
	two := testBegin + "\n" + testKeyLine + "\n" + testEnd + " " + testBegin + "\n" + testKeyLine + "\n" +
		testEnd + " passwd:\n" + colons + " " + gutterValue + "\ndone\n"
	add("a block written a step before, its END on the line", two+strings.Repeat("ok\n", 60<<10), strings.LastIndex(two, testBegin)+3+keyReach)
	three := testBegin + "\n" + testKeyLine + "\n" + testEnd + "\n" + two
	add("two blocks written a step before, the last's END on the line", three+strings.Repeat("ok\n", 60<<10), strings.LastIndex(three, testBegin)+3+keyReach)
	add("an armour in the gutter, its colons on the value's line", wrap(pc+" ", "", testBegin, testKeyLine, testEnd+" passwd:", colons+" "+gutterValue)+"done\n")
	add("concatenation, begun mid-line", "\t\"v1 "+pc+"\\n\" +\n\t\""+testEnd+"|passwd:\\n\" +\n\t\""+gutterValue+"\\n\" +\n")
	add("no gutter under a stamped block", "2026-10-06T09:00:00.123Z hunter2x9 u1SU1Lf9x+\n"+testEnd+"|passwd:\n"+gutterValue+"\n")
	add("an AWS secret's key after", "u1SU1Lf=\necho \""+testEnd+"|passwd:\" >> k\necho \""+gutterValue+"\" >> k\naws_secret_access_key = z6jmCqAkeYL1pRDxqWFVRmPZf+tiEOTNLBkMi2Yu\n")
	add("a blank line of the wrapping, then none", "u1SU1Lf=\n     7\u2192"+testEnd+"|passwd:\n     7\u2192\n"+gutterValue+"\n")
	add("a gutter on the value's line alone", ts+"hunter2x9 u1SU1Lf9x+\n"+testEnd+"|passwd:\n"+ts+gutterValue+"\n")
	add("a stamped BEGIN over a bare body and END", ts+testBegin+"\n"+testKeyLine+"\n"+testEnd+"|passwd:\n"+ts+gutterValue+"\n")
	add("a table's row over a bare END", "| "+testKeyLine+" |\n"+testEnd+"|passwd:\n| "+gutterValue+" fAt0gun+ |\n")
	add("stamps over a bare END, a prompt above", ts+testKeyLine[:30]+"\n"+ts+"Password: |\n"+testEnd+"|passwd:\n"+ts+gutterValue+"\n"+ts+testBegin+"\n")
	add("an armour after the gutter on the value's line", testKeyLine+"\n"+wrap(pc+" ", "", testEnd+" "+testBegin, testKeyLine, testEnd+" passwd:", testEnd+" "+gutterValue)+"done\n")
	add("a value before the block on its line", "Password:\n"+wrap("2026-10-06T09:00:00.123Z ", "", testKeyLine[:30], testEnd+"|passwd:", gutterValue+" "+testEnd))
	return shapes
}

// NO VALUE UNDER A KEY LINE WHOSE OPENING A BLOCK HOLDS IS PUBLISHED, and a
// second pass changes nothing: over every shape of [gutterShapes], Secrets
// shows neither gutterValue nor a value the text holds once that the rule
// reads in it as written ([gutterValues]), and Secrets of its answer is its
// answer.
//
// Mutation: write a key's block whole ([claims.apart]), or cut it at a key's
// line with no gutter ([valueRead.gutterOf]) or whose value the claims do not
// read ([claims.readsAValue]), ignore what the line a block begins on opens
// with ([claims.opensAlike]), or let a block no END closes take a key without
// its value ([claims.shortOfValues]), and a shape here publishes the value;
// read no value where a pass wrote its marker, and a second pass changes one.
func TestNoValueUnderAKeyLineWhoseOpeningABlockHoldsIsPublished(t *testing.T) {
	t.Parallel()
	for _, c := range gutterShapes() {
		if strings.Count(c.text, gutterValue) != 1 {
			t.Fatalf("%s: the shape holds gutterValue %d times, not once", c.name, strings.Count(c.text, gutterValue))
		}
		once := Secrets(c.text)
		for _, v := range append(gutterValues(c.text), gutterValue) {
			if strings.Count(c.text, v) == 1 && strings.Contains(once, v) {
				t.Errorf("%s: the value %q is published:\n text %q\n once %q", c.name, v, c.text, once)
			}
		}
		if twice := Secrets(once); twice != once {
			t.Errorf("%s: a second pass changed\n%q\nto\n%q", c.name, once, twice)
		}
	}
}

// gutterValues is every value the password's rule reads in text as written
// ([claimed]), as the text it is.
func gutterValues(text string) []string {
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
		values = append(values, text[w.at(value[0]):w.at(value[1])])
		prior, from = append(prior, value), value[1]
	}
}

// gutterStream is shape's text with its offset at placed at the keys' stage's
// first read boundary, and plain lines after it: a text the stream reads in
// windows, the one past that boundary not its last.
func gutterStream(text string, at int) string {
	line, n := "the build passed ok\n", max(1, stageAdvance-at)
	before := strings.Repeat(line, (n-1)/len(line))
	before += strings.Repeat("x", n-len(before)-1) + "\n"
	return before + text + strings.Repeat(line, (keyAhead+1<<10)/len(line))
}

// A STREAM CUTS A BLOCK AS THE WHOLE TEXT DOES WHEREVER ITS WINDOWS END: over
// every shape of [gutterShapes], placed at random, and every shape read as two
// blocks placed with the keys' stage's read boundary, or where it writes to,
// between them, at the stream's own hold Close answers nil and the stream
// wrote Secrets and the whole text's blocks, its keys' stage within its figure
// ([keysBytes]).
//
// Mutation: cut a window's blocks before they join those read before them
// ([keyStage.write]), and a joined shape is written otherwise; let go of the
// text before the next block's line ([keyStage.opening]), and the shape
// joined across a reach is too; forget the block written last, or keep the
// first a step wrote, and the block written a step after it is written whole.
func TestAStreamCutsABlockAsTheWholeTextDoesWhereverItsWindowsEnd(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 70))
	for _, c := range gutterShapes() {
		places := append([]int{r.IntN(len(c.text))}, c.joins...)
		for _, at := range places {
			text := gutterStream(c.text, at)
			got := readHolding(text, holdBytes(), r, 96<<10)
			if got.err != nil {
				t.Errorf("%s, the boundary at %d: Close answered %v at the stream's own hold", c.name, at, got.err)
				continue
			}
			if got.keysMost > keysBytes+holdBytes() {
				t.Errorf("%s, the boundary at %d: the keys' stage held %d KiB, past its %d KiB", c.name, at, got.keysMost>>10, (keysBytes+holdBytes())>>10)
			}
			checkReading(t, c.name, text, got)
		}
	}
}

// A KEY LINE'S OPENING IS SHOWN AS IT SETTLES AS THE WHOLE TEXT HAS IT: over
// every shape of [gutterShapes], a live view in pieces of 1, of 5 and 400, and
// of 1, 28 and 103 bytes, each redacted as far as Settled says, shows what
// Secrets shows of the whole.
//
// Mutation: leave the claims in the order the rules read them, an AWS
// secret's before the passwords ([claims.build]), and the shape with an AWS
// secret's key after is shown otherwise.
func TestAKeyLinesOpeningIsShownAsItSettlesAsTheWholeTextHasIt(t *testing.T) {
	t.Parallel()
	for _, c := range gutterShapes() {
		if len(c.text) > 4<<10 {
			// Shown a byte at a time, a text is read once a byte: the
			// shapes this long are the stream's ([keyStage.opening],
			// [keyStage.wrote]).
			continue
		}
		want := Secrets(c.text)
		for _, sizes := range [][]int{{1}, {5, 400}, {1, 28, 103}} {
			var shown strings.Builder
			pending := ""
			for at := 0; at < len(c.text); {
				for _, n := range sizes {
					n = min(n, len(c.text)-at)
					pending += c.text[at : at+n]
					at += n
					cut := SettledByBoth(t, pending)
					shown.WriteString(Secrets(pending[:cut]))
					pending = pending[cut:]
				}
			}
			shown.WriteString(Secrets(pending))
			if shown.String() != want {
				t.Errorf("%s in pieces of %v: shown\n%q\nwant\n%q", c.name, sizes, shown.String(), want)
			}
		}
	}
}

// A LATE KEY ON THE LINE A BLOCK ENDS ON IS A MISS: a password's key on an
// END's line below a short line, whose value under the wrapping's gutter is a
// run of 600 KiB (read open, [ruleStage.open]) or a word before one, so its
// claim and its gutter come past a hold of 256 KiB; the END read and written in
// one window, and read at a window's end and written by the next, which reads
// no armour. At 256 KiB the stream writes Secrets or refuses; at its own hold,
// Secrets.
//
// Mutation: claim an open value without its gutter ([ruleStage.open]), ask
// nothing of where claims begin, or check a late claim against the windows
// that asked what claims cover alone ([keyStage.beginsEarly]), or write a block
// without its claims ([keyStage.step]), and a stream here writes another text.
func TestALateKeyOnTheLineABlockEndsOnIsAMiss(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 71))
	for _, g := range []string{"2026-10-06T09:00:00.123Z ", "> "} {
		for _, run := range []string{strings.Repeat("a", 600<<10), "x " + strings.Repeat(".", 600<<10)} {
			shape := "u1SU1Lf=\n" + g + testEnd + "|passwd:\n" + g + run + "\n" + g + "done\n"
			for _, at := range []int{0, strings.Index(shape, testEnd) + 100} {
				text := gutterStream(shape, at)
				for _, h := range []int{256 << 10, holdBytes()} {
					got := readHolding(text, h, r, 96<<10)
					if h == holdBytes() && got.err != nil {
						t.Errorf("gutter %q, a run %q, the boundary at %d: Close answered %v at the stream's own hold", g, run[:3], at, got.err)
						continue
					}
					checkReading(t, "a late key on the END's line", text, got)
				}
			}
		}
	}
}

// A VALUE THAT BEGINS PAST A WINDOW READS THERE AS A VALUE: a BEGIN no END
// closes, read on through a password's key by lines its block reads as cut
// lines and the rule as the wrapping's blank lines, the value a reach below
// the key, past the keys' window that reads the BEGIN; at the stream's own
// hold it writes Secrets, its block ended short of the key. And the same key's
// value run on across escaped breaks past that window, which no read slices.
//
// Mutation: clip a value's start past a window to one past it, as its end is
// ([keyStage.claimsIn]), and the window reads the value as none and its block
// takes the key.
func TestAValueThatBeginsPastAWindowReadsThereAsAValue(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 72))
	hi := stageAdvance + keyAhead
	for _, g := range []string{testKeyLine[:56] + " ", "u1SU1Lf= "} {
		var b strings.Builder
		b.WriteString(gutterStream("", 100)[:stageAdvance-100])
		b.WriteString(testBegin + "\n")
		for range 12 {
			b.WriteString(g + testKeyLine + "\n")
		}
		b.WriteString(g + "passwd:\n")
		for b.Len()+len(g)+1 <= hi+200 {
			b.WriteString(g + "\n")
		}
		far := b.String() + g + gutterValue + "\n" + g + "done\n"
		escaped := gutterStream(`{"log": "`+testBegin+`\n`+g+testKeyLine+`\n`+g+`passwd:\n`+g+gutterValue+strings.Repeat(`\ny-y.y-y.y`, 9000)+`\n"}`+"\n", 300)
		far += gutterStream("", 0)[:wholeBytes]
		if strings.Contains(Secrets(far), gutterValue) {
			t.Fatalf("gutter %q: Secrets reads no value under the key, so the window's reading of it is not tested", g)
		}
		for _, text := range []string{far, escaped} {
			got := readHolding(text, holdBytes(), r, 96<<10)
			if got.err != nil {
				t.Errorf("gutter %q: Close answered %v at the stream's own hold", g, got.err)
				continue
			}
			checkReading(t, "a value past the window", text, got)
		}
	}
}

// A STREAM CLAIMS A VALUE IT READS OPEN WITH ITS GUTTER, as [claimed] claims
// every value: a password's key on an END's line, under the same gutter of
// base64 on every line, its value on the line below and longer than a stage's
// window, so the password's reader reads it on past each window
// ([ruleStage.open]). At the stream's own hold Close answers nil and the
// stream wrote Secrets and its blocks, the END's cut apart from the gutter.
//
// Mutation: claim an open value from where its separator ends, with no gutter
// ([claimSource.word] in [ruleStage.open]), and the stream writes the END's
// block whole, where Secrets cuts it apart from the gutter.
func TestAStreamClaimsAValueItReadsOpenWithItsGutter(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 63))
	filler := strings.Repeat("the build passed ok\n", stageStep/20+1)
	for _, g := range []string{testKeyLine[:56] + " ", "u1SU1Lf= "} {
		text := filler + g + testEnd + "|passwd:\n" + g + "placeholder9x" + strings.Repeat("y", 300<<10) + "\n" +
			g + "done\n" + filler
		got := readHolding(text, holdBytes(), r, 96<<10)
		if got.err != nil {
			t.Errorf("gutter %q: Close answered %v at the stream's own hold", g, got.err)
			continue
		}
		checkReading(t, "an open value under a gutter "+g, text, got)
	}
}

// A CLAIM READS IN A WINDOW AS NAMING WHAT IT NAMES, wherever the window ends:
// a key whose separator ends past the window names no value there, whether
// that end is known or still to come, and a value that begins past the window,
// decided or read open, reads as one ([keyStage.claimsIn]).
//
// Mutation: read every claim whose value begins past the window as a value,
// and a key with none reads as one, so a block no END closes would end before
// it ([claims.shortOfValues]).
func TestAClaimReadsInAWindowAsNamingWhatItNames(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("x", 100)
	for _, c := range []struct {
		name  string
		w     [12]int
		value bool
	}{
		{"no value, its separator's end past the window", [12]int{90, 150, 150, 150}, false},
		{"no value, its separator's end to come", [12]int{90, -1, -1, -1}, false},
		{"no value, its separator's end set past the window since", [12]int{90, 150, -1, 150}, false},
		{"no value, its separator's end set in the window since", [12]int{90, 95, -1, 95}, false},
		{"a value past the window", [12]int{90, 95, 150, 160}, true},
		{"a value read open past the window", [12]int{90, 95, 150, -1}, true},
	} {
		k := &keyStage{}
		k.sources = []*claimSource{{keys: k, words: [][12]int{c.w}}}
		p := k.claimsIn(text, 0).passwords[0]
		if got := p[3] > p[2]; got != c.value {
			t.Errorf("%s: the window reads %v as naming a value: %v, want %v", c.name, p, got, c.value)
		}
	}
}

// A KEY A BLOCK HOLDS CUTS NO OTHER BLOCK: a password's key inside one block,
// on the line another ends on, its gutter's end on that line too, leaves the
// other block whole ([claims.apartIn]). The rule reads no key inside a block,
// so its line's gutter is no gutter of a value's.
//
// Mutation: cut a block for a key inside another, and the first block is
// written as two markers.
func TestAKeyABlockHoldsCutsNoOtherBlock(t *testing.T) {
	t.Parallel()
	g := "> "
	text := testBegin + "\n" + g + testKeyLine + "\n" + g + testEnd + " " + testBegin + " passwd:\n" + g + testKeyLine + "\n" + g + testEnd + "\n"
	line := strings.Index(text, g+testEnd)
	key := strings.Index(text, "passwd:")
	value := strings.LastIndex(text, g+testKeyLine) + len(g)
	spans := [][2]int{{0, line + len(g+testEnd)}, {strings.LastIndex(text, testBegin), len(text) - 1}}
	c := &claims{text: text, read: true, passwords: [][12]int{{key, key + len("passwd:"), value, value + len(testKeyLine), line + len(g)}}}
	c.build()
	if got := c.apartIn(slices.Clone(spans), spans); !slices.Equal(got, spans) {
		t.Errorf("the blocks %v were written as %v", spans, got)
	}
}

// A STEP THAT WRITES A BLOCK AND READS NO ARMOUR READS THE CLAIMS AS THE WHOLE
// TEXT DOES: a block read forward takes a password's key and its value's first
// line, a key's line below it, and a value whose key a block takes is no claim
// past that block ([claims.take]), so the opening of the line a later block
// begins on, which the value runs into, reads as the gutter, and the later
// block is written whole. The keys' stage's first read boundary falls past the
// last END, and plain lines follow it, so at every placement the text is
// longer than a stream redacts whole ([wholeBytes]) and the step after it
// writes that block and reads no armour.
//
// Mutation: take no forward block into such a step ([keyStage.step]), and the
// stream cuts a block the whole text writes whole.
func TestAStepThatOnlyWritesReadsTheClaimsAsTheWholeTextDoes(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 1))
	tested := false
	for _, g := range []string{"KL20 ", "Ab3x "} {
		shape := `{"log": "` + testBegin + `\n` + testKeyLine + ` password: \nu1SU1Lf9x.Ab3x\n` + testKeyLine[:20] + `\n` + g + testBegin + `\n` +
			g + testKeyLine + `\n` + g + testEnd + `|passwd:\n` + g + gutterValue + `\n"}` + "\n" + strings.Repeat("ok\n", 60<<10)
		spans, _, c := readKeys(shape)
		bare := &claims{text: shape}
		tested = tested || c != nil && !slices.Equal(bare.apart(spans), c.apart(spans))
		end := strings.LastIndex(shape, testEnd) + len(testEnd)
		for _, at := range []int{end + 64, end + 4<<10, end + 32<<10} {
			text := gutterStream(shape, at)
			got := readHolding(text, holdBytes(), r, 96<<10)
			if got.err != nil {
				t.Errorf("the gutter %q, the boundary at %d: Close answered %v at the stream's own hold", g, at, got.err)
				continue
			}
			checkReading(t, "the gutter "+g, text, got)
		}
	}
	if !tested {
		t.Fatal("no shape's cut reads a block taken forward, so the test tests nothing")
	}
}

// A CLAIM ENDED NAMES WHAT IT NAMED OPEN: a value read open and then ended
// reads in a window as a value, and a key claimed with no value and then
// ended reads as none ([claimSource.endWord]).
//
// Mutation: let an ended claim's value begin where it ends, and the value
// read open reads as none.
func TestAClaimEndedNamesWhatItNamedOpen(t *testing.T) {
	t.Parallel()
	k := &keyStage{}
	src := &claimSource{keys: k}
	k.sources = []*claimSource{src}
	src.valued([3]int{10, 15, -1}, 16, [6]int{}, [2]int{}, nil)
	src.endWord(-1, 30)
	src.word([3]int{40, -1, -1})
	src.endWord(45, 45)
	got := k.claimsIn(strings.Repeat("x", 100), 0).passwords
	if len(got) != 2 || got[0][3] <= got[0][2] || got[1][3] > got[1][2] {
		t.Errorf("the claims read %v: want a value from 16 to 30 for the first key and none for the second", got)
	}
}

// seamShapes is every way a key's block comes to change a line a password's
// value is read by other than at its key line's opening (the package doc's "A
// key's block beside a password"), gutterValue the value: a block that runs on
// from the value's line, from a key's piece or a BEGIN, under four wrappings
// that end every line alike; and a table's row whose key's label a block
// stands in, or whose opening one takes, the value below it.
func seamShapes() []gutterShape {
	pc := testKeyLine[:56]
	wrap := func(g, s string, lines ...string) string {
		var b strings.Builder
		for _, l := range lines {
			b.WriteString(g + l + s + "\n")
		}
		return b.String()
	}
	var shapes []gutterShape
	add := func(name, text string) {
		shapes = append(shapes, gutterShape{name: name, text: text, joins: []int{strings.Index(text, gutterValue), strings.Index(text, testEnd) - 1}})
	}
	for _, w := range []struct{ name, g, s string }{
		{"concatenation", "\t\"", `\n" +`}, {"echo", `echo "`, `" >> k`}, {"table", "| ", " |"}, {"list", `"`, `",`},
	} {
		add("a piece on the value's line, "+w.name, wrap(w.g, w.s, "Password:", gutterValue+" "+pc, testEnd+"|passwd:", "hunter2x9"))
		add("a body between, "+w.name, wrap(w.g, w.s, "Password:", gutterValue+" "+pc, testKeyLine, testEnd+"|passwd:", "hunter2x9"))
		add("a BEGIN on the value's line, "+w.name, wrap(w.g, w.s, "Password:", gutterValue+" "+testBegin, testKeyLine, testEnd+"|passwd:", "hunter2x9"))
	}
	// A block that takes a value's line whole, the value a key above it
	// reads, is not cut at that line's ending: the rule reads no value
	// there, and reads on to the one below.
	add("a block holding the value's line, echo", wrap(`echo "`, `" >> k`, "fAt0gun+", "passwd:", testEnd+"|passwd:", testKeyLine, pc+" "+testEnd, gutterValue))
	plus := testKeyLine[:20] + "+" + testKeyLine[21:]
	add("a row, a key's END before its key", "| "+testKeyLine+" "+testEnd+" pwd= |\n     7\u2192"+gutterValue+"\n")
	add("a row, a key's text holding a +", "| "+plus+" "+testEnd+" pwd= |\n     7\u2192"+gutterValue+"\n")
	add("a row, a key's piece below it", "| "+testKeyLine+" "+testEnd+"\tpwd= |\n    \n    "+pc+" "+testEnd+"\n    Retype password: "+gutterValue+"\n")
	add("a row opening a BEGIN's body", testBegin+"\n| "+testKeyLine+" pwd= |\n     7\u2192"+gutterValue+"\n")
	add("rows ending in escaped breaks, a key's text holding =", "| u1SU1Lf="+testEnd+"\tPassword: |\\n\n|  |\\n\n| \""+gutterValue+"\" |\\n\n")
	add("a row opening after a BEGIN's body", testBegin+"\n"+testKeyLine+" | "+testKeyLine[8:]+testKeyLine[:8]+" pwd= |\n     7\u2192"+gutterValue+"\n")
	return shapes
}

// NO VALUE IS PUBLISHED WHERE A BLOCK RUNS ON FROM ITS LINE OR STANDS IN ITS
// KEY'S ROW, and a second pass changes nothing: over every shape of
// [seamShapes], Secrets shows neither gutterValue nor a value the text holds
// once that the rule reads in it as written ([gutterValues]), nor, on a row,
// the middle of a key's line, and Secrets of its answer is its answer.
//
// Mutation: write a block whole across a value's line's ending, or cut one
// that holds the value's line ([claims.endingApart]), stop a label at a key's
// marker ([labelStart]), or write a block whole across a row's opening
// ([claims.openingApart]), and a shape publishes its value.
func TestNoValueIsPublishedWhereABlockRunsOnFromItsLineOrStandsInItsKeysRow(t *testing.T) {
	t.Parallel()
	for _, c := range seamShapes() {
		if strings.Count(c.text, gutterValue) != 1 {
			t.Fatalf("%s: the shape holds gutterValue %d times, not once", c.name, strings.Count(c.text, gutterValue))
		}
		once := Secrets(c.text)
		for _, v := range append(gutterValues(c.text), gutterValue) {
			if strings.Count(c.text, v) == 1 && strings.Contains(once, v) {
				t.Errorf("%s: the value %q is published:\n text %q\n once %q", c.name, v, c.text, once)
			}
		}
		if strings.HasPrefix(c.name, "a row") && strings.Contains(once, testKeyLine[8:40]) {
			t.Errorf("%s: a key's line is published:\n text %q\n once %q", c.name, c.text, once)
		}
		if twice := Secrets(once); twice != once {
			t.Errorf("%s: a second pass changed\n%q\nto\n%q", c.name, once, twice)
		}
	}
}

// A STREAM WRITES A BLOCK APART FROM A VALUE'S LINE AS THE WHOLE TEXT DOES:
// over every shape of [seamShapes], placed at random, with the keys' stage's
// read boundary at the value, and at the END the block is read back from, at
// the stream's own hold Close answers nil and the stream wrote Secrets and the
// whole text's blocks.
//
// Mutation: hand a value's claim without where its line's ending begins
// ([ruleStage.seamsAt]), or leave it out of the window's claims
// ([keyStage.claimsIn]), and a shape is written otherwise.
func TestAStreamWritesABlockApartFromAValuesLineAsTheWholeTextDoes(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 73))
	for _, c := range seamShapes() {
		for _, at := range append([]int{r.IntN(len(c.text))}, c.joins...) {
			text := gutterStream(c.text, max(0, at))
			got := readHolding(text, holdBytes(), r, 96<<10)
			if got.err != nil {
				t.Errorf("%s, the boundary at %d: Close answered %v at the stream's own hold", c.name, at, got.err)
				continue
			}
			checkReading(t, c.name, text, got)
		}
	}
}

// A BLOCK WRITTEN APART FROM A ROW'S OPENING LEAVES THE KEY'S LINE A LINE OF
// ITS OWN: a BEGIN's block that takes a table row's opening, the row only
// spaces before it, is written apart from the line break before the row too,
// so the rule reads the key's line from where the claims read it, and not
// from the BEGIN's line.
//
// Mutation: keep the row's markup alone apart from the block
// ([claims.openingApart]), and the row is joined to the BEGIN's line.
func TestABlockWrittenApartFromARowsOpeningLeavesTheKeysLineALineOfItsOwn(t *testing.T) {
	t.Parallel()
	for _, open := range []string{"| ", "  | ", "|"} {
		text := testBegin + "\n" + open + testKeyLine + " pwd= |\n     7\u2192" + gutterValue + "\n"
		once := Secrets(text)
		if !strings.Contains(once, "\n"+open[:len(open)-len(strings.TrimLeft(open, " "))]+"|"+keyMarker) {
			t.Errorf("the row opening %q: the key's line is not a line of its own:\n text %q\n once %q", open, text, once)
		}
		if strings.Contains(once, gutterValue) {
			t.Errorf("the row opening %q: the value is published:\n text %q\n once %q", open, text, once)
		}
	}
}

// A BLOCK WHOSE FIRST LINE OPENS AS THE KEY'S ROW STAYS ONE MARKER: a table's
// rows from a BEGIN's down to an END's before a password's key, the value's
// row below, the key's row's opening inside the gutter the value's row
// spells: the key's row opens, to the rule, with the BEGIN row's opening, so
// the block is written whole ([claims.opensAlike]) and the value read.
//
// Mutation: write a block apart from a row's opening the gutter holds
// ([claims.openingApart]), and the block is two markers.
func TestABlockWhoseFirstLineOpensAsTheKeysRowStaysOneMarker(t *testing.T) {
	t.Parallel()
	text := "| " + testBegin + " |\n| " + testKeyLine + " |\n| " + testKeyLine + " " + testEnd + " pwd= |\n| " + gutterValue + " |\n"
	once := Secrets(text)
	if strings.Count(once, keyMarker) != 1 || strings.Contains(once, gutterValue) {
		t.Errorf("want one key's marker and the value redacted:\n text %q\n once %q", text, once)
	}
}

// A KEY WHOSE BLANKS RUN TO THE TEXT'S END NAMES NO VALUE, as the whole text
// reads it: under a BEGIN no END closes, a password's key on the base64's
// line, then blanks to the end, at the byte that takes the text through the
// stages and past a window. The keys' stage places such a word one past its
// window ([keyStage.claimsIn]), however the text is cut: the stream writes
// Secrets of it or refuses, and never panics.
//
// Mutation: read the line where such a value starts ([claims.readsAValue]),
// and Close slices past the window's end.
func TestAKeyWhoseBlanksRunToTheEndNamesNoValue(t *testing.T) {
	t.Parallel()
	head := testBegin + "\n" + testKeyLine + testKeyLine + testKeyLine[:32] + "\tPassword:\n"
	for _, size := range []int{wholeBytes + 1, 1 << 20} {
		text := head + strings.Repeat(" ", size-len(head))
		want := Secrets(text)
		for _, cut := range []int{7 << 10, 64 << 10, size} {
			got, panicked, err := streamedOrPanic(text, cut)
			switch {
			case panicked != nil:
				t.Errorf("%d bytes in writes of %d: the stream panicked: %v", size, cut, panicked)
			case err != nil && !errors.Is(err, ErrDecidedEarly):
				t.Errorf("%d bytes in writes of %d: Close answered %v", size, cut, err)
			case err == nil && got != want:
				t.Errorf("%d bytes in writes of %d: the stream wrote other than Secrets %s", size, cut, parted(got, want))
			}
		}
	}
}

// streamedOrPanic is what a [Stream] writes for text in writes of cut bytes,
// what it panicked with, if it did, and what Close answered.
func streamedOrPanic(text string, cut int) (out string, panicked any, err error) {
	var b strings.Builder
	defer func() {
		if panicked = recover(); panicked == nil {
			out = b.String()
		}
	}()
	s := NewStream(&b)
	for at := 0; at < len(text); at += cut {
		_, _ = s.Write([]byte(text[at:min(len(text), at+cut)]))
	}
	return "", nil, s.Close()
}

// A STRING'S LINES ARE READ AS THE LINES THEY STAND FOR: each text below,
// written with real line breaks and then as a JSON log's string, its breaks
// escaped, shows gutterValue in neither ([gutterValues]'s values neither),
// and Secrets of each answer is its answer: rows a markup closes, a blank row
// among them, and a key's text in a row's label; a quote before the key,
// escaped or on a later piece; and a key's piece no markup closes, which a
// key's block joins to the string's first.
//
// Mutation: read no markup-only suffix before an escaped break ([readValue]),
// or read a string's first piece from its real line's start, past an escaped
// quote, a later piece past a quote on it, or a piece no markup closes
// ([stringOpening]), and a string here publishes the value.
func TestAStringsLinesAreReadAsTheLinesTheyStandFor(t *testing.T) {
	t.Parallel()
	pc := testKeyLine[:56]
	v := gutterValue
	var texts []string
	for _, lines := range []string{
		"| Password: |\n| " + v + " |\n",
		"| x pwd= |\n|  |\n| " + v + " |\n",
		"| **Password:** |\n|  |\n| " + v + " |\n",
		"| " + testKeyLine + " " + testEnd + " pwd= |\n     7→" + v + "\ndone\n",
		"| u1SU1Lf=" + testEnd + "\tPassword: |\n|  |\n| " + v + " |\n",
		"| " + testKeyLine + " " + testEnd + "\tpwd= |\n|  |\n| " + pc + " " + testEnd + " |\n| Retype password: " + v + " |\n",
	} {
		texts = append(texts, lines, `{"log": "`+strings.ReplaceAll(lines, "\n", `\n`)+`"}`+"\n")
	}
	texts = append(texts, `{"log": "> \"| Password: |\n> `+v+` |\n"}`+"\n", `{"log": "a\necho "| Password: |\necho "`+v+` |\n"}`+"\n",
		`{"log": "3f9a7c0e1b2d u1SU1Lf=\n`+testKeyLine+testEnd+`\" ; pwd:\n3f9a7c0e1b2d u1SU1Lf=\n3f9a7c0e1b2d Retype password:\n3f9a7c0e1b2d `+v+`\n"}`+"\n")
	for _, text := range texts {
		once := Secrets(text)
		for _, value := range append(gutterValues(text), v) {
			if strings.Count(text, value) == 1 && strings.Contains(once, value) {
				t.Errorf("the value %q is published:\n text %q\n once %q", value, text, once)
			}
		}
		if twice := Secrets(once); twice != once {
			t.Errorf("a second pass changed\n%q\nto\n%q", once, twice)
		}
	}
}

// A VALUE ON THE NEXT LINE OF A STRING THE KEY OPENS IS READ PAST THE STRING'S
// WRAPPING: a key on a string's first piece after its quote, under no markup
// closing it, and the value on the string's next piece behind the same gutter,
// redacted by Secrets and by a stream fed the text a line at a time, every
// stage past its first window, whatever the gutter.
//
// Mutation: read the rest of the key's line past an escaped break as its
// suffix even where it spells the key line's gutter
// ([valueRead.restOfLine]), and the value is shown.
func TestAValueOnAStringsNextLineIsReadPastItsWrapping(t *testing.T) {
	t.Parallel()
	filler := strings.Repeat("the build passed ok\n", (stageStep+2*MaxReach)/20+2)
	for _, gutter := range []string{"> ", "- ", "# ", "// ", "| ", "* ", "1. "} {
		text := `{"text": "` + gutter + `Password:\n` + gutter + `hunter2x9"}` + "\n"
		if got := Secrets(text); strings.Contains(got, "hunter2x9") {
			t.Errorf("gutter %q: Secrets shows the value: %q", gutter, got)
		}
		whole := filler + text
		var lines []int
		for i := range len(whole) {
			if whole[i] == '\n' {
				lines = append(lines, i+1)
			}
		}
		if got := streamed(whole, lines); strings.Contains(got, "hunter2x9") || got != Secrets(whole) {
			t.Errorf("gutter %q: a stream a line at a time shows the value or parts from Secrets: %q", gutter,
				got[len(got)-min(len(got), 120):])
		}
	}
}
