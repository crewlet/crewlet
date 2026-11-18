package redact

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// lagBytes is the claims' lag the stream's memory figure counts for the keys'
// stage waiting on its claims ([streamBound]), worked here from the table
// rather than read from [holdBytes], so a hold that drifts from it is caught
// ([TestNoTextHoldsAStreamPastItsWindowsAndItsHold] holds the two equal, and
// holds the stream's own hold to the figure with this one).
func lagBytes() int {
	return len(rules()) * (stageStep + pieceBytes + longestMarker())
}

// keysBytes is what the keys' stage holds besides what it waits on: a piece,
// four blocks' reach behind where it reads and a step past it ([streamBound]).
const keysBytes = pieceBytes + 4*keyReach + stageAdvance + keyAhead

// holdShape is a text built to hold a stream, built when asked for: a key's
// armour beside a run, a collapsed text, or blanks that a reader decides a
// claim past.
type holdShape struct {
	name string
	text func() string
}

// holdShapes is every shape a stream was found to hold, each with its run, or
// the text it repeats, n bytes long: an armour beside a run of every kind, a
// password's key or an AWS secret's quoted key decided past a run or blanks,
// runs and keys' blocks in turn, keys' blocks among tokens the first rules
// collapse, the password's keys a block's reading asks of beside them, and a
// Google token beside a key whose dots run on, its end before them set only
// once they stop ([ruleStage.carryToken]).
func holdShapes(n int) []holdShape {
	r := rand.New(rand.NewPCG(1170, 40))
	long := testRun(r, testB64url, n)
	longB64 := testRun(r, testB64, n)
	block := testBegin + "\n" + testKeyLine + "\n" + testKeyLine + "\n" + testKeyLine + "\n" + testEnd + "\n"
	open := testBegin + "\n" + testKeyLine + "\n" + testKeyLine + "\n" + testKeyLine + "\n"
	one := testBegin + " " + testKeyLine + " " + testKeyLine + " " + testEnd + "\n"
	tail := strings.Repeat("ok\n", stageStep/3+1)
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + long + "\n"
	jwt237 := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + long[:200] + " "
	head := "k: " + testBegin + "\n" + testKeyLine + "\n\n"
	armour := "a key: " + testBegin + "\n" + testKeyLine + "\n\n"
	repeat := func(unit string, n int) string { return strings.Repeat(unit, n/len(unit)+1) }
	sk200 := "sk-" + testRun(r, testB64url, 200) + " "
	rt := "crewlet_rt1.Tq0d_8-x." + testRun(r, testAlnum, 60<<10) + ".1792927401." + testRun(r, testB64url, 43) + " "
	ghp := "ghp_" + testRun(r, testAlnum, 2000) + "\n"
	jwt1k := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + testRun(r, testB64url, 1000)
	dots := strings.Repeat(".", n)
	// The first run of the p shapes is longer than a stage's window, so
	// what claims it is read on across steps, whatever n is.
	first := 300 << 10
	pairs := ""
	for range 8 {
		pairs += testBegin + "\n" + testKeyLine + "\nx Password: |\nu1SU1Lf9x sk-" + long[:n/8] + " |\n"
	}
	return []holdShape{
		{"a1 an armour, then a run token's body", func() string { return armour + "data: " + long[:1000] + "sk-" + long + "\n" + tail }},
		{"a2 an armour, then a JSON Web Token's signature", func() string { return armour + jwt + tail }},
		{"a3 an armour, then an AWS secret's value", func() string { return armour + "SessionToken=" + longB64 + "\n" + tail }},
		{"a4 an armour, then an SSH certificate", func() string { return armour + "ssh-ed25519-cert-v01@openssh.com " + longB64 + "\n" + tail }},
		{"a5 an armour, then a password's value", func() string { return armour + "pwd=" + long + "\n" + tail }},
		{"a6 an armour, then a password's key's blanks", func() string { return armour + "password:" + strings.Repeat(" ", n) + "x\n" + tail }},
		{"a7 an armour, a password's line, a run", func() string {
			return "a key: " + testBegin + "\n" + testKeyLine + "\n" + testEnd + "\nPassword: hunter2x9\nsk-" + long + "\n" + tail
		}},
		{"b1 an armour, then an AWS secret's quoted key and blanks", func() string { return armour + "SessionToken\"" + strings.Repeat(" ", n) + "= x\n" + tail }},
		{"b2 an armour, an AWS secret's quoted key, tabs and a value", func() string {
			return armour + "\"aws_secret_access_key\"" + strings.Repeat("\t", n) + ": " + longB64[:60] + "\n" + tail
		}},
		{"b3 an armour, then an AWS secret's bare key and blanks", func() string { return armour + "SessionToken" + strings.Repeat(" ", n) + "= x\n" + tail }},
		{"c1 an armour, a run, an armour, a run", func() string {
			return head + "sk-" + long[:n/2] + "\n" + testBegin + "\n" + testKeyLine + "\n\nsk-" + long[:n/2] + "\n" + tail
		}},
		{"c6 an escaped break before a run", func() string { return armour + `{"pwd": "hunter2x9\n` + "sk-" + long + `"}` + "\n" + tail }},
		{"d1 a password's key a block reads, a run, a suffix", func() string {
			return testBegin + "\n" + testKeyLine + "\n" + testKeyLine + "\nx Password: |\nu1SU1Lf9x sk-" + long + " |\n" + tail
		}},
		{"d2 a password's key a block reads, a run", func() string {
			return testBegin + "\n" + testKeyLine + "\n" + testKeyLine + "\nx Password: |\nu1SU1Lf9x sk-" + long + "\n" + tail
		}},
		{"d3 a password's key a block reads, two runs, an END", func() string {
			return testBegin + "\n" + testKeyLine + "\n" + testKeyLine + "\nx Password: |\nu1SU1Lf9x sk-" + long[:n/2] + " sk-" + long[:n/2] + " |\n" + testEnd + "\n" + tail
		}},
		{"d4 a password's key a block reads, a run, a key's line and END", func() string {
			return testBegin + "\n" + testKeyLine + "\nx Password: |\nu1SU1Lf9x sk-" + long + "\n" + testKeyLine + "\n" + testEnd + "\n" + tail
		}},
		{"e1 an armour, password's keys, a run", func() string { return head + strings.Repeat("pwd:\n", 12000) + "sk-" + long + "\n" + tail }},
		{"f1 an armour, 10 KB tokens", func() string { return head + repeat("sk-"+long[:10000]+" ", n) + "\n" + tail }},
		{"f3 an armour, tokens and password's keys", func() string { return head + repeat("sk-"+long[:30]+" pwd:x\n", n) + "\n" + tail }},
		{"g1 armours among tokens and password's keys", func() string { return repeat(head+repeat("sk-"+long[:40]+" pwd:x\n", 64<<10), n) + "\n" + tail }},
		{"g2 armours among 1 KB JSON Web Tokens and password's keys", func() string { return repeat(head+repeat(jwt1k+" pwd:x\n", 256<<10), n) + "\n" + tail }},
		{"gd1 a key on one line, a Google token on it and its dots", func() string {
			return testBegin + " " + testKeyLine + " ya29." + long[:30] + dots + " " + testEnd + "\n" + tail
		}},
		{"gd2 an armour, its key's line, a Google token and its dots on the next", func() string {
			return "a key: " + testBegin + "\n" + testKeyLine + "\nya29." + long[:30] + dots + " \n" + tail
		}},
		{"gd3 a key on one line, a Google token on it and dots that end with a byte of its own", func() string {
			return testBegin + " " + testKeyLine + " ya29." + long[:30] + dots + "a " + testEnd + "\n" + tail
		}},
		{"gd4 a key on one line, a Google token on it and dots to the text's end", func() string {
			return testBegin + " " + testKeyLine + " ya29." + long[:30] + dots
		}},
		{"gd5 a key's line, a Google token and its dots, the key's END read back after them", func() string {
			return testKeyLine + "\n" + testKeyLine + " ya29." + long[:30] + dots + "\n" + testKeyLine + "\n" + testEnd + "\n" + tail
		}},
		{"gd6 a key on one line, two Google tokens on it, the first's dots stopping", func() string {
			return testBegin + " " + testKeyLine + " ya29." + long[:30] + "... ya29." + long[30:60] + dots + " " + testEnd + "\n" + tail
		}},
		{"hc1 an armour, 200 B tokens", func() string { return head + repeat(sk200, n) }},
		{"hc2 an armour, 237 B JSON Web Tokens", func() string { return head + repeat(jwt237, n) }},
		{"hc3 an armour, run tokens of 60 KiB subjects", func() string { return head + repeat(rt, n) }},
		{"hc5 an armour, 2 KB tokens a line", func() string { return head + repeat(ghp, n) }},
		{"hc7 an armour, 1 KB JSON Web Tokens, a password's line", func() string { return head + repeat(jwt1k+"\n", n) + "Password: hunter2x9\n" + tail }},
		{"n1 a run and no armour", func() string { return "data: " + long[:1000] + "sk-" + long + "\n" + tail }},
		{"p1 eight password's keys blocks read, beside runs", func() string { return pairs + tail }},
		{"pa1 a run, a key's block, a JSON Web Token's signature", func() string { return "x sk-" + long[:first] + " |\n" + block + jwt + tail }},
		{"pa2 a run, a key with no END, a JSON Web Token's signature", func() string { return "x sk-" + long[:first] + " |\n" + open + jwt + tail }},
		{"pa3 an AWS secret's value, a key's block, a run", func() string { return "SessionToken=" + longB64[:first] + " |\n" + block + "sk-" + long + "\n" + tail }},
		{"pa4 an SSH certificate, a key's block, a JSON Web Token's signature", func() string {
			return "ssh-ed25519-cert-v01@openssh.com " + longB64[:first] + " |\n" + block + jwt + tail
		}},
		{"pb1 a run, a key's block, a run of an earlier rule", func() string { return "x sk-" + long[:first] + " |\n" + block + "sk-ant-" + long + "\n" + tail }},
		{"pb2 a run, a key with no END, a run of an earlier rule", func() string { return "x sk-" + long[:first] + " |\n" + open + "sk-ant-" + long + "\n" + tail }},
		{"pb3 a JSON Web Token, a key's block, a Google token", func() string {
			return "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + long[:first] + " |\n" + block + "ya29." + long + "\n" + tail
		}},
		{"pb4 an AWS secret's value, a key's block, an SSH certificate", func() string {
			return "SessionToken=" + longB64[:first] + " |\n" + block + "ssh-ed25519-cert-v01@openssh.com " + longB64 + "\n" + tail
		}},
		{"pb6 a long run, a key's block, a run of an earlier rule", func() string { return "x sk-" + long[:n/6] + " |\n" + block + "sk-ant-" + long + "\n" + tail }},
		{"pc1 a run, a key on one line, a run of an earlier rule", func() string { return "x sk-" + long[:first] + " |\n" + one + "sk-ant-" + long + "\n" + tail }},
		{"pc4 a run past what claims it reads, a key on one line, a run of an earlier rule", func() string { return "x sk-" + long + " |\n" + one + "sk-ant-" + long + "\n" + tail }},
		{"pc3 a JSON Web Token, a key on one line, a Google token", func() string {
			return "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + long[:first] + "\n" + one + "ya29." + long + "\n" + tail
		}},
		{"pd2 a run, a key on one line, dense JSON Web Tokens", func() string {
			return "x sk-" + long[:first] + " |\n" + one + strings.Repeat(jwt237, n/len(jwt237)) + "\n" + tail
		}},
	}
}

// earlyText is a text of about size bytes of [streamText]'s shapes with the
// hold shapes put in it at random: a key's armour and then a long run of each
// kind, a password's shapes before the run and a suffix after it, password's
// keys after an armour, AWS secrets' quoted keys whose blanks run on, and an
// END after a run.
func earlyText(r *rand.Rand, size, runMax int) string {
	pick := func(s ...string) string { return s[r.IntN(len(s))] }
	runLen := func() int { return runMax/4 + r.IntN(runMax) }
	runs := []func() string{
		func() string { return "sk-" + testRun(r, testB64url, runLen()) },
		func() string { return "sk-ant-" + testRun(r, testB64url, runLen()) },
		func() string { return "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + testRun(r, testB64url, runLen()) },
		func() string { return "ssh-ed25519-cert-v01@openssh.com " + testRun(r, testB64, runLen()) },
		func() string { return "SessionToken=" + testRun(r, testB64, runLen()) },
		func() string { return "aws_secret_access_key = " + testRun(r, testB64, runLen()) },
		func() string {
			return "ya29." + testRun(r, testB64url, 30) + strings.Repeat(".", runLen()) + pick("a", " ", "")
		},
		func() string { return "pwd=" + testRun(r, testB64, runLen()) },
		func() string {
			return "SessionToken\"" + strings.Repeat(" ", runLen()) + pick("= x", "x", "", ": "+testRun(r, testB64, 60))
		},
		func() string { return "sk-" + strings.Repeat(testRun(r, testB64url, 200)+" sk-", runLen()/205) },
	}
	armours := []string{
		testBegin + "\n" + testKeyLine + "\n\n",
		"a key: " + testBegin + "\n" + testKeyLine + "\n",
		testBegin + "\n" + testKeyLine + "\n" + testKeyLine + "\n",
		`"` + testBegin + `\n` + testKeyLine + `\n`,
		testKeyLine + "\n" + testKeyLine + "\n",
		"",
	}
	before := []string{
		"x Password: |\nu1SU1Lf9x ", "| Password: |\n| ", "echo \"Password:\" >> f\necho \"", "Password:\n",
		"pwd=", "pwd= ", `{"pwd": "hunter2x9\n`, "a Password: |\nAb3dEf9hK2 ", "password: \"", "Passwd:\n  ",
		strings.Repeat("pwd:\n", 1+r.IntN(300)), "",
	}
	after := []string{
		" |\n", "\n", "\" >> f\n", "\n" + testEnd + "\n", " |\n" + testEnd + "\n", ".pwd:x\n", `\n"` + "\n",
		" tail |\n", "\n" + testKeyLine + "\n" + testEnd + "\n", "",
	}
	var b strings.Builder
	for b.Len() < size {
		switch r.IntN(5) {
		case 0, 1:
			b.WriteString(streamText(r, 4<<10+r.IntN(160<<10)))
		default:
			b.WriteString(pick(armours...))
			b.WriteString(pick(before...))
			b.WriteString(runs[r.IntN(len(runs))]())
			b.WriteString(pick(after...))
		}
		b.WriteString(pick("\n", " ", `\n`, "", "\n\n"))
	}
	return b.String()
}

// reading is what a stream that holds h did with a text: what it wrote and
// answered, the blocks its keys' stage wrote, the windows it decided early,
// and the most its stages held between two writes.
type reading struct {
	out             string
	err             error
	blocks          [][2]int
	early, keysMost int
	most            streamHeld
	// decidedAt is how much of the text the stages had been handed when
	// each window was decided early.
	decidedAt []int
}

// readHolding is text written to a stream that holds h, in pieces of 1 to
// most bytes drawn from r, or of 64 KiB where r is nil.
func readHolding(text string, h int, r *rand.Rand, most int) reading {
	var got reading
	var out bytes.Buffer
	s := newStreamHolding(&out, h)
	s.observe = &keyObserver{
		wrote: func(b [2]int) { got.blocks = append(got.blocks, b) },
		decided: func(int, int) {
			got.early++
			got.decidedAt = append(got.decidedAt, s.p.keys.arrived)
		},
	}
	for at := 0; at < len(text); {
		n := 64 << 10
		if r != nil {
			n = 1 + r.IntN(most)
		}
		n = min(n, len(text)-at)
		_, _ = s.Write([]byte(text[at : at+n]))
		at += n
		if s.p != nil {
			h := s.p.held()
			got.most = streamHeld{max(got.most.text, h.text), max(got.most.entries, h.entries), max(got.most.stageText, h.stageText), max(got.most.stageEdits, h.stageEdits)}
			got.keysMost = max(got.keysMost, s.p.keys.in.held())
		}
	}
	got.err = s.Close()
	got.out = out.String()
	return got
}

// checkReading fails t where a reading is neither [Secrets] of text with Close
// nil nor refused, or where its keys' stage wrote a block otherwise than the
// whole text's before the byte the refusal names.
func checkReading(t *testing.T, name, text string, got reading) {
	t.Helper()
	whole := keySpans(text)
	if got.err == nil {
		if want := Secrets(text); got.out != want {
			t.Errorf("%s: Close answered nil, and the stream wrote another text than Secrets %s", name, parted(got.out, want))
		}
		if !slices.Equal(got.blocks, whole) {
			t.Errorf("%s: Close answered nil, and the keys' stage wrote %d blocks where the whole text has %d", name, len(got.blocks), len(whole))
		}
		return
	}
	var early *decidedEarly
	if !errors.Is(got.err, ErrDecidedEarly) || !errors.As(got.err, &early) {
		t.Errorf("%s: Close answered %v, neither nil nor ErrDecidedEarly", name, got.err)
		return
	}
	for i, b := range got.blocks {
		if i < len(whole) && b == whole[i] {
			continue
		}
		at := b[0]
		if i < len(whole) {
			at = min(at, whole[i][0])
		}
		if at < early.at {
			t.Errorf("%s: the keys' stage wrote %v, otherwise than the whole text's block, at %d, before the %d the refusal names", name, b, at, early.at)
		}
		return
	}
}

// NO TEXT HOLDS A STREAM PAST ITS WINDOWS AND ITS HOLD: whatever the text, the
// keys' stage holds no more than its own bytes plus the hold ([holdBytes]),
// every stage no more than its window, and all together no more than
// [streamBound] derives with the hold in place of the claims' lag. Checked over
// a shape of every hold a stream was found to have, at a small hold, with a run
// of two lengths (the longer holds no more); and at the stream's own hold over
// a run longer than it beside an armour, held to the figure [lagBytes] gives.
//
// Mutation: decide no window before its claims are known ([keyStage.step]), or
// let the text run twice the hold past a window, and a shape is held past the
// figure; or hold the text further past a window than the claims' lag
// ([holdBytes]), and the stream's own hold is not the figure's.
func TestNoTextHoldsAStreamPastItsWindowsAndItsHold(t *testing.T) {
	t.Parallel()
	boundText, boundEntries := streamBound()
	const h = 128 << 10
	check := func(name string, hold int, got reading) {
		t.Helper()
		text := boundText - lagBytes() + hold
		switch {
		case got.keysMost > keysBytes+hold:
			t.Errorf("%s at a hold of %d KiB: the keys' stage held %d KiB, past the %d KiB it holds besides its hold and the hold", name, hold>>10, got.keysMost>>10, (keysBytes+hold)>>10)
		case got.most.stageText > streamWindow:
			t.Errorf("%s at a hold of %d KiB: a stage held %d bytes of its input, past the %d of its window", name, hold>>10, got.most.stageText, streamWindow)
		case got.most.text > text || got.most.entries > boundEntries:
			t.Errorf("%s at a hold of %d KiB: the stages held %d bytes of text and %d of entries, past the %d and %d derived", name, hold>>10, got.most.text, got.most.entries, text, boundEntries)
		}
	}
	// A shape of each hold the stream was found to have: an armour beside
	// a run of each kind, blanks, a collapsed text, and claims the
	// password's reader decides past a run.
	each := []string{"a1", "a3", "a5", "a6", "b1", "b2", "b3", "c1", "c6", "d1", "e1", "f3", "g1", "g2", "hc1", "hc3", "p1", "pb1", "pc1", "pd2"}
	short, long := holdShapes(640<<10), holdShapes(1<<20)
	for i := range short {
		if !slices.Contains(each, short[i].name[:strings.IndexByte(short[i].name, ' ')]) {
			continue
		}
		a := readHolding(short[i].text(), h, nil, 0)
		b := readHolding(long[i].text(), h, nil, 0)
		check(short[i].name+", a run of 640 KiB", h, a)
		check(long[i].name+", a run of 1 MiB", h, b)
		if b.keysMost > a.keysMost+pieceBytes {
			t.Errorf("%s: the keys' stage held %d KiB with a run of 640 KiB and %d KiB with one of 1 MiB, growing with the run", short[i].name, a.keysMost>>10, b.keysMost>>10)
		}
	}
	if holdBytes() != lagBytes() {
		t.Errorf("the stream decides a window once the text has run %d bytes past it, where the figure counts the claims' lag of %d", holdBytes(), lagBytes())
	}
	for _, s := range holdShapes(lagBytes() + 2<<20) {
		if strings.HasPrefix(s.name, "a1 ") {
			text := s.text()
			got := readHolding(text, holdBytes(), nil, 0)
			check(s.name+", a run past the hold", lagBytes(), got)
			t.Logf("%s (%d KiB): the keys' stage held at most %d KiB, every stage %d KiB", s.name, len(text)>>10, got.keysMost>>10, got.most.text>>10)
		}
	}
}

// A STREAM THAT DECIDES EARLY SAYS SO: Close answers nil, and the stream wrote
// [Secrets] of the text; or Close answers [ErrDecidedEarly], naming the first
// byte from which what it wrote may differ, with no earlier block of the keys'
// stage differing from the whole text's. Over random texts at a low hold, a
// shape of every hold at 256 KiB (exactly those named refuse), and at the
// stream's own hold, where a key with a Google token whose dots run past the
// hold still writes Secrets. Mutation: step no claims stage at a marker
// ([ruleStage.markerToStepAt]); check an end set late ([keyStage.ended]); read
// a late end in an early window as no change; or write a span moved before its
// start ([keyStage.readArmours]).
func TestAStreamThatDecidesEarlySaysSo(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 41))
	refused := 0
	for i := range 3 {
		text := earlyText(r, 1<<20, 512<<10)
		got := readHolding(text, 256<<10, r, 96<<10)
		if got.err != nil {
			refused++
		}
		checkReading(t, fmt.Sprintf("text %d at a hold of 256 KiB", i), text, got)
	}
	// A shape of every hold, and those that read a claim otherwise: at
	// this hold a token's claim too can come past it, and these refuse.
	each := []string{"a1", "b1", "c1", "d4", "e1", "g2", "gd1", "gd2", "gd3", "gd4", "gd5", "gd6", "hc1", "pb1", "pc4", "pd2"}
	refuse := []string{"c1", "e1", "g2", "pb1", "pc4"}
	for _, s := range holdShapes(768 << 10) {
		if !slices.Contains(each, s.name[:strings.IndexByte(s.name, ' ')]) {
			continue
		}
		text := s.text()
		got := readHolding(text, 256<<10, nil, 0)
		checkReading(t, s.name, text, got)
		if (got.err != nil) != slices.Contains(refuse, s.name[:strings.IndexByte(s.name, ' ')]) {
			t.Errorf("%s at a hold of 256 KiB: Close answered %v, %d windows decided early", s.name, got.err, got.early)
		}
	}
	for _, s := range holdShapes(lagBytes() + 256<<10) {
		name := s.name[:strings.IndexByte(s.name, ' ')]
		if name != "pb1" && name != "gd1" {
			continue
		}
		text := s.text()
		got := readHolding(text, holdBytes(), nil, 0)
		if got.err != nil || got.early == 0 {
			t.Errorf("%s at the stream's own hold: Close answered %v, %d windows decided early", s.name, got.err, got.early)
		}
		checkReading(t, s.name+" at the stream's own hold", text, got)
	}
	t.Logf("%d of 3 random texts refused", refused)
}

// AN EARLY WINDOW'S FOOTPRINT HOLDS EVERY CLAIM ITS ANSWERS READ ([claims.asked]):
// over named ranges and spans drawn at random, a claim added that meets no
// part of the footprint changes no answer [claims.any], [claims.names] or
// [claims.shortOfNames] gave: not where it merges with a range an answer
// read, which moves the range's start without covering what was asked of.
//
// Mutation: take a span's footprint in [claims.shortOfNames] at the place
// asked of alone, and a claim merging with the range there changes where the
// span ends unseen.
func TestAnEarlyWindowsFootprintHoldsEveryClaimItsAnswersRead(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 42))
	const size = 400
	text := strings.Repeat("x", size)
	draw := func(n int) [][2]int {
		var c [][2]int
		for range n {
			a := r.IntN(size - 20)
			c = append(c, [2]int{a, a + 1 + r.IntN(40)})
		}
		return c
	}
	answers := func(tokens [][2]int, spans [][2]int, queries [][2]int) ([][2]int, []bool) {
		c := &claims{text: text, read: true, tokens: slices.Clone(tokens)}
		c.build()
		var got []bool
		for _, q := range queries {
			got = append(got, c.any(q[0], q[1]), c.names(q[0], q[1]))
		}
		return c.shortOfNames(slices.Clone(spans)), got
	}
	for it := range 20000 {
		tokens := draw(1 + r.IntN(6))
		spans, queries := draw(1+r.IntN(3)), draw(r.IntN(3))
		c := &claims{text: text, read: true, tokens: slices.Clone(tokens)}
		c.build()
		foot := [2]int{size + 1, -1}
		c.footprint = &foot
		for _, q := range queries {
			c.any(q[0], q[1])
			c.names(q[0], q[1])
		}
		c.shortOfNames(slices.Clone(spans))
		added := draw(1)[0]
		if foot[0] < foot[1] && added[0] < foot[1] && foot[0] < added[1] {
			continue
		}
		s1, a1 := answers(tokens, spans, queries)
		s2, a2 := answers(append(slices.Clone(tokens), added), spans, queries)
		if !slices.Equal(s1, s2) || !slices.Equal(a1, a2) {
			t.Fatalf("draw %d: claims %v, then %v added outside the footprint %v: spans %v became %v, answers %v became %v", it, tokens, added, foot, s1, s2, a1, a2)
		}
	}
}

// A LATE CHANGE TO WHAT AN EARLY WINDOW READ IS A MISS, and only that: a keys'
// stage that decided a window early, its footprint known, refuses a claim
// handed more than the hold past where it lies that meets the footprint; an
// end set inside the window on an open claim whose stretch to the window's end
// meets it, to the byte; and a late word whose key lies before a block let go,
// naming the key. Each is named from the earliest window still waiting; a
// window with all claims known stops waiting ([keyStage.settle]); past
// [earlyWindows] waiting, the two oldest merge ([keyStage.wait]). Mutation:
// check an end from where its claim begins or a byte off ([keyStage.ended]);
// drop the late-word check ([keyStage.handed]); name a miss from where found
// alone ([keyStage.miss]); never settle a window; merge other than the oldest.
func TestALateChangeToWhatAnEarlyWindowReadIsAMiss(t *testing.T) {
	t.Parallel()
	const h = 1 << 20
	stage := func() (*keyStage, *claimSource) {
		k := &keyStage{hold: h, early: [][4]int{{100 << 10, 300 << 10, 150 << 10, 160 << 10}}, floor: 90 << 10, forgot: 120 << 10}
		src := &claimSource{keys: k}
		k.sources = []*claimSource{src}
		return k, src
	}
	cases := []struct {
		name   string
		change func(k *keyStage, src *claimSource)
		at     int
	}{
		{"a claim over the footprint, a piece past the hold", func(k *keyStage, src *claimSource) {
			k.arrived = 155<<10 + h + pieceBytes/2
			src.token([2]int{155 << 10, 156 << 10})
		}, 100 << 10},
		{"a claim, open, from before the footprint", func(k *keyStage, src *claimSource) {
			k.arrived = 140<<10 + 2*h
			src.token([2]int{140 << 10, -1})
		}, 100 << 10},
		{"an end set inside the window on a claim it read open", func(k *keyStage, src *claimSource) {
			src.tokens = [][2]int{{140 << 10, -1}}
			k.arrived = 2 * h
			src.endToken(145 << 10)
		}, 100 << 10},
		{"an end set on the footprint's last byte, on a claim it read open from before it", func(k *keyStage, src *claimSource) {
			src.tokens = [][2]int{{140 << 10, -1}}
			k.arrived = 2 * h
			src.endToken(160<<10 - 1)
		}, 100 << 10},
		{"a late word behind a block let go", func(k *keyStage, src *claimSource) {
			k.early, k.arrived = nil, 4*h
			src.word([3]int{110 << 10, 111 << 10, 95 << 10})
		}, 110 << 10},
		{"a late word behind a block let go, its value open", func(k *keyStage, src *claimSource) {
			k.early, k.arrived = nil, 4*h
			src.word([3]int{110 << 10, 111 << 10, -1})
		}, 110 << 10},
		{"a late word behind a block let go, a window decided early before it waiting", func(k *keyStage, src *claimSource) {
			k.arrived = 4 * h
			src.word([3]int{110 << 10, 111 << 10, -1})
		}, 100 << 10},
		{"a late word behind a block let go, the window before it let go once its claims are known", func(k *keyStage, src *claimSource) {
			src.frontier = 301 << 10
			k.take("")
			k.arrived = 4 * h
			src.word([3]int{110 << 10, 111 << 10, -1})
		}, 110 << 10},
		{"a late claim between the two oldest of more windows than wait", func(k *keyStage, src *claimSource) {
			waitMore(k)
			k.arrived = 200 * h
			src.token([2]int{h/2 + 1, h/2 + 2})
		}, 0},
		{"a late claim between two later windows of more than wait", func(k *keyStage, src *claimSource) {
			waitMore(k)
			k.arrived = 200 * h
			src.token([2]int{2*h + h/2, 2*h + h/2 + 1})
		}, -1},
		{"a claim beside the footprint", func(k *keyStage, src *claimSource) {
			k.arrived = 4 * h
			src.token([2]int{160 << 10, 170 << 10})
		}, -1},
		{"a claim over the footprint within the hold", func(k *keyStage, src *claimSource) {
			k.arrived = 155<<10 + h
			src.token([2]int{155 << 10, 156 << 10})
		}, -1},
		{"an end set past the footprint, on a claim it read open from before it", func(k *keyStage, src *claimSource) {
			src.tokens = [][2]int{{140 << 10, -1}}
			k.arrived = 2 * h
			src.endToken(165 << 10)
		}, -1},
		{"an end set at the footprint's end, on a claim it read open from inside it", func(k *keyStage, src *claimSource) {
			src.tokens = [][2]int{{155 << 10, -1}}
			k.arrived = 2 * h
			src.endToken(160 << 10)
		}, -1},
		{"an end set past the window", func(k *keyStage, src *claimSource) {
			src.tokens = [][2]int{{140 << 10, -1}}
			k.arrived = 4 * h
			src.endToken(301 << 10)
		}, -1},
		{"a late word whose value ends before where the next window begins", func(k *keyStage, src *claimSource) {
			k.arrived = 4 * h
			src.word([3]int{20 << 10, 21 << 10, 30 << 10})
		}, -1},
	}
	for _, c := range cases {
		k, src := stage()
		c.change(k, src)
		switch {
		case c.at < 0 && k.missed != nil:
			t.Errorf("%s: refused at %d", c.name, k.missed.at)
		case c.at >= 0 && k.missed == nil:
			t.Errorf("%s: no miss", c.name)
		case c.at >= 0 && k.missed.at != c.at:
			t.Errorf("%s: the miss names %d, not %d", c.name, k.missed.at, c.at)
		}
	}
}

// waitMore has k wait on 65 windows decided early, one more than the 64
// [earlyWindows] lets wait, counted here rather than read from it so that a
// cap that drifts merges other windows: window j the 100 KiB from j MiB on,
// its footprint the bytes from 16 to 32 KiB into it, so no two windows meet.
func waitMore(k *keyStage) {
	k.early = nil
	for j := range 65 {
		lo := j << 20
		k.wait([4]int{lo, lo + 100<<10, lo + 16<<10, lo + 32<<10})
	}
}

// WHETHER A STREAM DECIDES EARLY IS ITS TEXT'S ALONE: a shape of every hold a
// stream was found to have, those that refuse among them, at a hold of 256
// KiB, and an armour beside a run past the stream's own hold, at that hold,
// answers alike and writes alike cut at random, in small pieces and in
// large ones, as in pieces of 64 KiB, and decides early the windows it does
// with the same text handed to it, at an offset a piece ends at: when it
// decides a window early, and so whether it refuses, turns on what each stage
// holds when it is handed a piece, which the text's own offsets decide
// ([pipeline.feed]).
//
// Mutation: cut each write into pieces from the write's own start, and a shape
// here decides a window early where a write ended.
func TestWhetherAStreamDecidesEarlyIsItsTextsAlone(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 43))
	alike := func(name, text string, hold int) {
		t.Helper()
		first := readHolding(text, hold, nil, 0)
		if first.early == 0 {
			t.Errorf("%s at a hold of %d KiB: no window decided early, so how it is cut decides nothing here", name, hold>>10)
		}
		for _, most := range []int{7 << 10, 100 << 10} {
			got := readHolding(text, hold, r, most)
			if got.out != first.out || fmt.Sprint(got.err) != fmt.Sprint(first.err) || !slices.Equal(got.decidedAt, first.decidedAt) {
				t.Errorf("%s at a hold of %d KiB, in pieces of at most %d bytes: windows decided early with %v of the text handed, and %v, where in pieces of 64 KiB with %v, and %v", name, hold>>10, most, got.decidedAt, got.err, first.decidedAt, first.err)
			}
			for _, at := range got.decidedAt {
				if at%pieceBytes != 0 && at != len(text) {
					t.Errorf("%s at a hold of %d KiB, in pieces of at most %d bytes: a window decided early with %d bytes of the text handed, no offset a piece ends at", name, hold>>10, most, at)
				}
			}
		}
	}
	each := []string{"a1", "b1", "c1", "d4", "e1", "g1", "gd1", "hc5", "p1", "pb1", "pc1"}
	for _, s := range holdShapes(768 << 10) {
		if slices.Contains(each, s.name[:strings.IndexByte(s.name, ' ')]) {
			alike(s.name, s.text(), 256<<10)
		}
	}
	for _, s := range holdShapes(lagBytes() + 1<<20) {
		if strings.HasPrefix(s.name, "a1 ") {
			alike(s.name, s.text(), holdBytes())
		}
	}
}

// EarlyReach is the least text in which a [Stream] at its own hold
// ([holdBytes]) can decide a window early: its first window ends
// stageAdvance and keyAhead into the text ([keyStage.step]), and is decided
// before its claims are known only once the text has run the hold past that.
func EarlyReach() int {
	return stageAdvance + keyAhead + holdBytes()
}

// EarlyWindows is how many windows a [Stream] at its own hold ([holdBytes])
// decides early reading text in writes of 64 KiB, and what its Close
// answers, for the package's external tests.
func EarlyWindows(text string) (int, error) {
	got := readHolding(text, holdBytes(), nil, 0)
	return got.early, got.err
}

// A SHAPE IS READ ALIKE AT EVERY BYTE THE HOLD ENDS BESIDE IT: the keys' stage
// decides its first window once the text has run the hold past the window's
// end, and the stages are handed the text a piece at a time, so a hold a byte
// either side of where a piece ends past the window decides the window early
// at that piece or at the next; over a marker glued to a token, a run and a
// marker after it, and a run and a token of an earlier rule glued to it, each
// beside a key's block, every such reading writes [Secrets] with Close nil or
// refuses with no block written otherwise before what it names.
func TestAShapeIsReadAlikeAtEveryByteTheHoldEndsBesideIt(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 45))
	block := testBegin + "\n" + testKeyLine + "\n" + testKeyLine + "\n"
	filler := strings.Repeat("the build passed ok\n", stageStep/20)
	run := testRun(r, testB64url, 3*stageStep)
	shapes := map[string]string{
		"a token glued to a marker either side": "sk-" + run[:40] + Marker + "api-key]" + "sk-" + run[:40] + " Password: hunter2x9\n",
		"a run and a marker straight after it":  "sk-ant-" + run + Marker + "jwt]" + " pwd=hunter2x9\n",
		"a run and a token of an earlier rule":  "x sk-" + run + "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + run[:60] + " |\n",
	}
	// Where the first window ends ([keyStage.step]), and the piece the
	// text first runs a hold of 256 KiB and more past it at.
	hi := stageAdvance + keyAhead
	edge := (hi+256<<10)/pieceBytes*pieceBytes + pieceBytes
	for name, shape := range shapes {
		text := block + filler + shape + block + testEnd + "\n" + filler
		for d := -1; d <= 1; d++ {
			h := edge - hi + d
			checkReading(t, fmt.Sprintf("%s, a hold of %d bytes", name, h), text, readHolding(text, h, nil, 0))
		}
	}
}
