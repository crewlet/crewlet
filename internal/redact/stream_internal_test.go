package redact

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

// StreamText is [streamText] for the package's external tests.
var StreamText = streamText

// streamText is a text of about size bytes of every shape a stage carries
// across its window, drawn from r: a token's run through long base64 and
// base64url, a Google token's dots and an SSH certificate's padding, an AWS
// secret's value and its separator's spaces, a password's value through
// escaped breaks and backslashes, chains of keys, keys' blocks forward and back
// and on one line, a token's run inside a block, values below keys at
// [MaxReach] and a byte past it, and the words and punctuation between them.
// Its lengths run around a reach and around a stage's advance, so what a
// window decides falls on every side of where it ends.
func streamText(r *rand.Rand, size int) string {
	const (
		b64    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
		b64url = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		line   = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
		begin  = "-----BEGIN RSA PRIVATE KEY-----"
		end    = "-----END RSA PRIVATE KEY-----"
	)
	run := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[r.IntN(len(alphabet))]
		}
		return string(b)
	}
	long := func() int {
		switch r.IntN(4) {
		case 0:
			return 20 + r.IntN(200)
		case 1:
			return MaxReach - 600 + r.IntN(1200)
		case 2:
			return stageAdvance - 2000 + r.IntN(4000)
		}
		return r.IntN(3 * stageAdvance)
	}
	pick := func(s ...string) string { return s[r.IntN(len(s))] }
	shapes := []func() string{
		func() string { return "sk-" + run(b64url, long()) },
		func() string { return "sk-proj-" + run(b64url, long()) + end },
		func() string {
			return "ya29." + run(b64url, 30) + strings.Repeat(".", long()) + pick("a", " ", end, "")
		},
		func() string { return "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + run(b64url, long()) },
		func() string { return "eyJ" + run(b64url, long()) + ".eyJ" + run(b64url, 20) + "." + run(b64url, 10) },
		func() string {
			return "ssh-ed25519-cert-v01@openssh.com " + run(b64, long()) + strings.Repeat("=", r.IntN(5))
		},
		func() string { return "aws_secret_access_key = " + run(b64, long()) },
		func() string {
			return "SessionToken" + strings.Repeat(" ", long()) + pick("= ", "x", "") + run(b64, 50)
		},
		func() string { return "SessionToken\"" + strings.Repeat(" ", long()) + pick("=", "x", "") },
		func() string { return "AKIA" + run("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", 16) },
		func() string { return "crewlet_rt1.Tq0d_8-x.subj.1792927401." + run(b64url, 43) },
		func() string { return "ghp_" + run("abcdefABCDEF0123456789", 36+r.IntN(long()+1)) },
		func() string { return "pwd=" + run(b64, long()) },
		func() string {
			return "password: " + run(b64, 10) + `\n` + run(b64, long()) + strings.Repeat(`\`, r.IntN(3)*long()) + pick(`\n"`, `\nabc`, " ", "")
		},
		func() string { return "pwd=x" + strings.Repeat(`\`, long()) + pick(`\n"`, `\nabc`, "x") },
		func() string {
			return "Password:" + strings.Repeat("\n", r.IntN(3)) + strings.Repeat(" ", long()) + "\nhunter2"
		},
		func() string {
			return "password:" + strings.Repeat(" ", long()) + `\n` + strings.Repeat(`\n `, r.IntN(100))
		},
		func() string { return strings.Repeat("pwd= ", 1+r.IntN(2000)) + "x" },
		func() string { return begin + "\n" + strings.Repeat(line+"\n", 1+r.IntN(30)) + end },
		func() string { return `"` + begin + `\n` + strings.Repeat(line+`\n`, 1+r.IntN(30)) + end + `\n"` },
		func() string { return strings.Repeat(line+"\n", 1+r.IntN(40)) + end },
		func() string { return begin + "\n" + run(b64, long()) },
		func() string { return begin + " " + line + " " + line + " " + end },
		func() string { return begin + "\n" + line + "\nsk-" + run(b64url, long()) + "\n" + line + "\n" + end },
		func() string {
			return "sk-AAAAAAAAAAAAAAAAAAAAAAAA Password:\nsk-" + run(b64url, long()) + " \nsk-AAAAAAAAAAAAAAAAAAAAAAAA hunter2\n" + end
		},
		func() string {
			return "Password:" + strings.Repeat("\n", 1+r.IntN(3)) + strings.Repeat(" ", MaxReach-30+r.IntN(60)) + "hunter2"
		},
		func() string { return `echo "Password:" >> f` + "\n" + `echo "hunter2" >> f` },
		func() string { return "| Password: |\n| hunter2 |" },
		func() string { return run(b64, long()) },
		func() string { return run(b64url, long()) },
		func() string { return strings.Repeat(" ", long()) },
		func() string { return strings.Repeat(`\n`, long()/2) },
	}
	words := []string{"the", "a", "file", "pwd", "ok", "done", "{", "}", `"`, ":", "=", "-", "\t", " ", "\n", `\n`, Marker + "api-key]", "-----", "PRIVATE KEY"}
	var b strings.Builder
	for b.Len() < size {
		if r.IntN(3) == 0 {
			for n := r.IntN(40); n > 0; n-- {
				b.WriteString(words[r.IntN(len(words))])
				b.WriteByte(' ')
			}
		} else {
			b.WriteString(shapes[r.IntN(len(shapes))]())
		}
		b.WriteString(pick("\n", `\n`, " ", "", "\n\n", "\t"))
	}
	return b.String()
}

// streamed is text written to a [Stream] in the pieces cuts end, in order.
func streamed(text string, cuts []int) string {
	var out bytes.Buffer
	s := NewStream(&out)
	at := 0
	for _, c := range append(cuts, len(text)) {
		_, _ = s.Write([]byte(text[at:c]))
		at = c
	}
	_ = s.Close()
	return out.String()
}

// parted is where got and want part, and each around it.
func parted(got, want string) string {
	j := 0
	for j < len(got) && j < len(want) && got[j] == want[j] {
		j++
	}
	lo := max(0, j-80)
	return fmt.Sprintf("at byte %d (%d against %d bytes):\n got  …%q…\n want …%q…", j, len(got), len(want),
		got[lo:min(len(got), j+80)], want[lo:min(len(want), j+80)])
}

// staged is text redacted by the stages fed it whole, a piece at a time: what a
// [Stream] writes for it, every stage read past its first window.
//
// Where the text holds no key's armour, no block is read and no claim asked
// of, so the rules over it are all the stages there are: the keys' stage
// writes the text as it is handed it, and the claims are read by nothing.
func staged(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	p := newPipeline(func(s string) { b.WriteString(s) }, strings.Contains(text, keyArmour), holdBytes())
	for at := 0; at < len(text); at += pieceBytes {
		p.write(text[at:min(len(text), at+pieceBytes)])
	}
	p.close()
	return b.String()
}

// A TEXT LONGER THAN A WINDOW IS REDACTED BY ITS STAGES AS IT IS WHOLE: fed
// whole, the stages write what [Secrets] writes, every rule's decisions
// trusted only where the window holds what they read, every run read on past
// it, and the keys' blocks read once the rules' claims are decided; so does a
// [Stream] with every stage.
//
// Mutation: trust a value whose end the window read back over backslashes only
// ([ruleStage.trusted]), write a value's backslashes before an escaped break
// ([ruleStage.carryValue]), let the keys' stage read a block before the claims
// are decided ([keyStage.step]), or read a token's held dots as its body
// ([ruleStage.carryToken]), and texts here part.
func TestALongTextIsRedactedByItsStagesAsWhole(t *testing.T) {
	t.Parallel()
	check := func(name, text string) {
		t.Helper()
		want := Secrets(text)
		if got := staged(text); got != want {
			t.Fatalf("%s, %d bytes: the stages wrote another redaction than the passes over it whole %s", name, len(text), parted(got, want))
		}
		if got := streamed(text, nil); got != want {
			t.Fatalf("%s, %d bytes: a stream wrote another redaction than the passes over it whole %s", name, len(text), parted(got, want))
		}
	}
	r := rand.New(rand.NewPCG(1170, 1))
	for i := range 6 {
		check(fmt.Sprintf("text %d", i), streamText(r, wholeBytes+r.IntN(4*stageAdvance)))
	}
	// And the runs whose end a window decides by reading past it, each set
	// at every place a stage's window can end inside or beside it: a
	// password's value whose backslashes an escaped break ends it before or
	// reads on across, a Google token's dots, and a value a reach below its
	// key that runs on past the next window.
	b64 := strings.Repeat("Ab3+", 3*stageStep/4)
	filler := strings.Repeat("the build passed\n", 2*stageStep/17)
	for name, run := range map[string]string{
		"backslashes a break reads on across": "pwd=x" + strings.Repeat(`\`, 3*stageReach/2) + `\n` + strings.Repeat("!", MaxReach) + "abc ",
		"backslashes a break ends it before":  "password: " + strings.Repeat("x", 2*stageStep) + strings.Repeat(`\`, stageStep) + `\n" `,
		"a Google token's dots":               "ya29." + strings.Repeat("a", 30) + strings.Repeat(".", 3*stageReach/2) + "a ",
		"a value a reach below its key":       "Password:\n" + strings.Repeat(" ", MaxReach-100) + b64 + "\n",
	} {
		for at := stageAdvance - stageReach; at < stageAdvance+stageReach; at += stageReach / 3 {
			check(fmt.Sprintf("%s %d bytes in", name, at), filler[:at]+run+filler)
		}
	}
}

// A SHAPE IS REDACTED ALIKE AT EVERY BYTE A WINDOW CAN END BESIDE IT: the
// shapes whose reads run furthest, each set so the end of the first window of a
// rule's stage and of the keys' stage ([stageAdvance]) falls a byte or two past
// where it is read from, or at it: a run token whose subject is [MaxReach]
// long, an AWS secret's quoted key with nearly that many spaces, a key's block
// read forward from its BEGIN, alone and holding a token's run, and one read
// back from its END. Random texts place none of these to the byte.
// Mutation: take the margin out of [stageReach], look for an armour in the
// keys' window no further than it reads ([keyStage.step]), or keep a block's
// reach behind the writes shorter than a block ([keyReach]).
func TestAShapeIsRedactedAlikeAtEveryByteAWindowCanEndBesideIt(t *testing.T) {
	t.Parallel()
	const (
		line  = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
		begin = "-----BEGIN RSA PRIVATE KEY-----"
		end   = "-----END RSA PRIVATE KEY-----"
	)
	b64 := strings.Repeat("Ab3+", 16)
	b64url := strings.Repeat("Ab3_x-9Z", stageReach/8)
	// Each shape, and where in it the read that decides it begins.
	shapes := map[string]string{
		"a run token's longest subject": "crewlet_rt1.Tq0d_8-x." + strings.Repeat("s", MaxReach-2) + ".1792927401." + strings.Repeat("S", 43) + " ",
		"an AWS key's quoted spaces":    "SessionToken\"" + strings.Repeat(" ", MaxReach-5) + "= " + b64 + " ",
		"a key's block":                 begin + "\n" + strings.Repeat(line+"\n", 20) + end + "\n",
		"a key's block holding a token": begin + "\n" + line + "\nsk-" + b64url + "\n" + line + "\n" + end + "\n",
		"a key's block read back":       strings.Repeat(line+"\n", 30) + end + "\n",
	}
	filler := strings.Repeat("the build passed ok\n", 2*stageStep/20)
	for name, shape := range shapes {
		from := strings.Index(shape, end)
		if strings.HasPrefix(shape, begin) || !strings.Contains(shape, end) {
			from = 0
		}
		for _, d := range []int{-2, -1, 0} {
			at := stageAdvance + d - from
			text := filler[:at] + shape + filler
			if got, want := staged(text), Secrets(text); got != want {
				t.Errorf("%s, a window's end %d bytes past its start: the stages wrote another redaction than the passes over it whole %s", name, -d, parted(got, want))
			}
		}
	}
}

// A RUN OF BYTES THAT BEGIN NO RUNE IS DECIDED WINDOW BY WINDOW, as any text
// is: invalid UTF-8 longer than a window, each byte of it a place a scan reads
// alone, is written on as the window decides it ([ruleStage.commit]), and what
// stands past it is redacted as in the whole text.
//
// Mutation: move a cut back to where a rune begins over every byte that
// continues one, and a window inside the run decides nothing.
func TestARunOfBytesThatBeginNoRuneIsDecidedWindowByWindow(t *testing.T) {
	t.Parallel()
	filler := strings.Repeat("ok done\n", 2*stageStep/8)
	for _, run := range []string{"\x80", "\xbf\x80\x9f"} {
		text := "pwd=hunter2x9 the build passed\n" + strings.Repeat(run, 3*stageStep/len(run)) +
			"\nsk-" + strings.Repeat("A", 40) + " Password: hunter3x9\n" + filler
		if got, want := staged(text), Secrets(text); got != want {
			t.Errorf("%q a window's length and more: the stages wrote another redaction than the passes over it whole %s", run, parted(got, want))
		}
	}
}

// A TOKEN'S RUN IS READ ON AS ITS PATTERN READS IT ([tokenRunOf]): for every
// rule whose match runs on, and every byte, the byte continues the run past the
// window exactly where the pattern's match goes on through it, a byte the run
// may not end on is held ([tokenRun.held]), and the padding after it is the
// pattern's. Derived from the pattern rather than spelled again, and held to it
// here byte by byte.
//
// Mutation: derive a run from a bounded repeat, or drop the held byte, and a
// rule here parts from its pattern.
func TestATokensRunIsReadOnAsItsPatternReadsIt(t *testing.T) {
	t.Parallel()
	runs := 0
	for _, rule := range rules() {
		if rule.value != nil {
			continue
		}
		run := rule.run
		// A match long enough that its every bound part is behind it:
		// the rule's first match in a long run of its own bytes.
		sample := ruleSample(t, rule)
		if run == nil {
			if loc := rule.pattern.FindStringIndex(sample + strings.Repeat("A", 4*MaxReach)); loc != nil && loc[1]-loc[0] > 2*MaxReach {
				t.Errorf("%s: its match runs on past %d bytes, but no run is read on for it", rule.with, 2*MaxReach)
			}
			continue
		}
		runs++
		anchored := regexp.MustCompile(`^(?:` + rule.pattern.String() + `)`)
		for c := range 256 {
			loc := anchored.FindStringIndex(sample + string([]byte{byte(c)}) + "|")
			if loc == nil {
				t.Fatalf("%s: its sample is no match", rule.with)
			}
			through := loc[1] > len(sample)
			if c == int(run.pad) && run.padMax > 0 {
				continue
			}
			if c == int(run.held) && run.held != 0 {
				if !run.in[c] || through {
					t.Errorf("%s: %q is held, but the pattern reads it as %v", rule.with, rune(c), through)
				}
				continue
			}
			if through != run.in[c] {
				t.Errorf("%s: after its run the pattern's match goes on through %q: %v; the run read on: %v", rule.with, rune(c), through, run.in[c])
			}
		}
	}
	if runs == 0 {
		t.Fatal("no rule's match runs on; the table lost its runs")
	}
}

// ruleSample is a match of rule whose bounded parts are all behind it and whose
// last byte is one its run ends on.
func ruleSample(t *testing.T, rule rule) string {
	t.Helper()
	for _, s := range []string{
		"crewlet_rt1.Tq0d_8-x.subj.1792927401." + strings.Repeat("S", 43),
		"ya29." + strings.Repeat("a", 100),
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + strings.Repeat("a", 100),
		"sk-proj-" + strings.Repeat("a", 100), "sk-ant-" + strings.Repeat("a", 100), "sk-" + strings.Repeat("a", 100),
		"xoxb-" + strings.Repeat("a", 100), "AKIAABCDEFGHIJKLMNOP", "ASIAABCDEFGHIJKLMNOP",
		"ghp_" + strings.Repeat("a", 100), "github_pat_" + strings.Repeat("a", 100),
		"glpat-" + strings.Repeat("a", 100), "glrt-" + strings.Repeat("a", 100),
		"dop_v1_" + strings.Repeat("a", 100), "doo_v1_" + strings.Repeat("a", 100), "dor_v1_" + strings.Repeat("a", 100),
		"-cert-v01@openssh.com " + strings.Repeat("a", 100),
	} {
		if loc := rule.pattern.FindStringIndex(s); loc != nil && loc[0] == 0 && loc[1] == len(s) {
			return s
		}
	}
	t.Fatalf("%s: no sample is its match whole", rule.with)
	return ""
}

// A VALUE WRITTEN A STRETCH AT A TIME IS WRITTEN AS IT IS WHOLE ([valueWriter]):
// a marker for each piece between its escaped breaks, the breaks as they
// stand, however the value is cut: a cut between the `\` and the `n` of a
// break, inside the `\r` before one, or between two backslashes.
//
// Mutation: hold back no `\r` at a stretch's end, and a `\r\n` cut after its
// `\r` writes the `\r` as a piece of its own.
func TestAValueWrittenAStretchAtATimeIsWrittenAsWhole(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		`abc`, `a\nb`, `a\r\nb`, `\nab\n`, `\r\n\r\n`, `a\\nb`, `a\\\nb`, `\\\\`, `ab\r`, `ab\r\`, `\r\nx\r`, `x\\r\nb`,
	} {
		var whole strings.Builder
		writeValue(&whole, passwordMarker, value)
		for cut := 0; cut <= len(value); cut++ {
			for cut2 := cut; cut2 <= len(value); cut2++ {
				var b strings.Builder
				w := valueWriter{with: passwordMarker}
				w.write(&b, value[:cut])
				w.write(&b, value[cut:cut2])
				w.write(&b, value[cut2:])
				w.close(&b)
				if b.String() != whole.String() {
					t.Errorf("%q cut at %d and %d: written %q, whole %q", value, cut, cut2, b.String(), whole.String())
				}
			}
		}
	}
}

// streamHeld is what a pipeline's stages hold between two writes: the text in
// their feeds, what they have yet to hand on and the runs they read on, and
// beside it the entries they keep (values, edits, claims and blocks) at the
// bytes each takes; and the most any one rule's stage holds of its input, and
// of edits.
type streamHeld struct {
	text, entries, stageText, stageEdits int
}

// held is what p's stages hold.
func (p *pipeline) held() streamHeld {
	var h streamHeld
	var walk func(s sink)
	walk = func(s sink) {
		switch st := s.(type) {
		case *ruleStage:
			h.text += st.in.held() + st.out.Len() + len(st.run.head) + len(st.run.w.pending)
			h.stageText = max(h.stageText, st.in.held())
			h.entries += 16 * len(st.prior)
			if st.raw != nil {
				h.entries += 32*len(st.raw.edits) + 16*len(st.src.tokens) + 40*len(st.src.words)
				h.stageEdits = max(h.stageEdits, len(st.raw.edits))
			}
			if st.to != nil {
				walk(st.to)
			}
		case fanout:
			for _, r := range st {
				walk(r)
			}
		}
	}
	walk(p.rules)
	h.text += len(p.carry)
	if p.keys != nil {
		walk(p.claims)
		h.text += p.keys.in.held()
		h.entries += 16*(len(p.keys.pending)+len(p.keys.forward)) + 8*len(p.keys.closed) + 32*(len(p.keys.early)+len(p.keys.beginsEarly))
	}
	return h
}

// streamWindow is the most a rule's stage holds of its input between two
// writes: [stageReach] behind where it resumes, less than [stageStep] past it,
// and the piece handed last, [pieceBytes] and a marker past it at most.
var streamWindow = stageReach + stageStep + pieceBytes + longestMarker()

// longestMarker is the longest marker a rule writes ([ruleMarkers]), read
// from the table so that a rule added with a longer one moves the figure: a
// DigitalOcean token's, a byte longer than an AWS secret's.
func longestMarker() int {
	n := 0
	for _, m := range ruleMarkers() {
		n = max(n, len(m))
	}
	return n
}

// streamBound is the most a [Stream]'s stages hold between two writes, as text
// and as entries (values, edits, claims, blocks), derived from the code: the
// piece carried short of the next offset ([pipeline.feed]), under
// [pieceBytes]; every rule's stage, claims stage and reader holds
// [streamWindow]; the keys' stage holds what lies behind where it reads (four
// blocks' reach) and a step past it, and while it waits on claims the text
// every claims stage holds past its resume. An edit stands for a marker,
// fifteen bytes of input at least, at 32 bytes; a value, six bytes, at 16, as
// many again while trimmed; a claim, five bytes (`pwd:` and a break), at 40;
// and the early windows, [earlyWindows] at 32 in each of two lists.
func streamBound() (text, entries int) {
	table := len(rules())
	readers := 1
	for _, r := range rules() {
		if r.value != nil && r.keyed != passwordRule {
			readers++
		}
	}
	tracking := table - 1 + readers
	keys := pieceBytes + 4*keyReach + stageAdvance + keyAhead
	lag := (table - 1 + 1) * (stageStep + pieceBytes + longestMarker())
	text = (table+tracking)*streamWindow + keys + lag + pieceBytes
	entries = tracking*streamWindow*32/15 + 2*2*16*((stageReach+8)/6+1) + (keys+stageStep)*40/5 + 2*32*earlyWindows
	return text, entries
}

// A STREAM HOLDS NO MORE THAN ITS STAGES' WINDOWS, whatever the text and
// however it is cut: each rule's stage no more of its input than
// [streamWindow] and of edits than the markers that fit in it, and all
// together no more than [streamBound] derives, between every two writes, over
// texts of every shape, the densest entries (a JSON Web Token every fifteen
// bytes), values that run on past a window, and the runs a stage counts rather
// than holds (a Google token's dots, a value's backslashes), each a MiB long.
// Texts holding a stream past its windows before the keys' stage decided early
// ([holdBytes]) are TestNoTextHoldsAStreamPastItsWindowsAndItsHold's.
// Mutation: hand output on a step at a time ([ruleStage.pass]), write counted
// runs at once ([ruleStage.repeat]), keep joined stretches ([feed.window]) or
// edits behind the resume ([rawMap.prune]), or shrink [stageAdvance].
func TestAStreamHoldsNoMoreThanItsStagesWindows(t *testing.T) {
	t.Parallel()
	boundText, boundEntries := streamBound()
	marker := longestMarker()
	boundEdits := (stageStep+pieceBytes+2*marker)/15 + 1
	r := rand.New(rand.NewPCG(1170, 2))
	// Each stage trails its input by a step, so a text reaches the last of
	// them only past the stages' steps together: each text runs some steps
	// past that.
	size := (len(rules()) + 3) * stageStep
	dense := func(unit string) string { return strings.Repeat(unit, size/len(unit)) }
	texts := map[string]string{
		"every shape": streamText(r, size),
		"jwts":        dense("eyJaaaaaaaa.eyJaaaaaaaa.!"),
		"values a reach below their keys": dense("Password:\n" + strings.Repeat(" ", MaxReach-100) +
			strings.Repeat("Ab3+", stageStep/2) + "\n"),
		"a Google token's dots": "ya29." + strings.Repeat("a", 30) + strings.Repeat(".", 1<<20) + " \n" +
			dense("the build passed\n")[:1<<20],
		"backslashes a break ends a value before": "pwd=x" + strings.Repeat(`\`, 1<<20) + `\n" ` + "\n" +
			dense("the build passed\n")[:1<<20],
	}
	for name, text := range texts {
		s := NewStream(io.Discard)
		var most streamHeld
		for at := 0; at < len(text); {
			n := 1 + r.IntN(64<<10)
			_, _ = s.Write([]byte(text[at:min(len(text), at+n)]))
			at += n
			if s.p != nil {
				h := s.p.held()
				most = streamHeld{max(most.text, h.text), max(most.entries, h.entries), max(most.stageText, h.stageText), max(most.stageEdits, h.stageEdits)}
			}
		}
		_ = s.Close()
		switch {
		case most.stageText > streamWindow:
			t.Errorf("%s: a stage held %d bytes of its input, past the %d of its window", name, most.stageText, streamWindow)
		case most.stageEdits > boundEdits:
			t.Errorf("%s: a stage kept %d edits, past the %d markers that fit past where it resumes", name, most.stageEdits, boundEdits)
		case most.text > boundText || most.entries > boundEntries:
			t.Errorf("%s: the stages held %d bytes of text and %d of entries, past the %d and %d derived", name, most.text, most.entries, boundText, boundEntries)
		}
		t.Logf("%s: held %d KiB of text, %d KiB of entries; a stage at most %d KiB and %d edits", name, most.text>>10, most.entries>>10, most.stageText>>10, most.stageEdits)
	}
	t.Logf("bound: %d KiB of text, %d KiB of entries; a stage %d KiB and %d edits", boundText>>10, boundEntries>>10, streamWindow>>10, boundEdits)
}

// A STREAM HANDS ITS STAGES THE SAME PIECES HOWEVER THE TEXT IS CUT: after
// every write, its stages have been handed the text up to the last offset
// that is a multiple of [pieceBytes] and no further, a piece of exactly that
// length at a time, and the stream carries the rest ([pipeline.feed]); so
// where each stage's input ends when it is handed a piece, and what it holds
// when it steps, is the text's alone, whether it arrives a byte at a time, in
// pieces of any size, or whole.
//
// Mutation: cut each write into pieces from the write's own start
// ([pipeline.feed]), and the stages are handed the text up to where each
// write ends, in pieces of the writes' making.
func TestAStreamHandsItsStagesTheSamePiecesHoweverTheTextIsCut(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 6))
	text := streamText(r, wholeBytes+5*pieceBytes+r.IntN(pieceBytes))
	for _, most := range []int{1, 4 << 10, pieceBytes - 1, pieceBytes + 1, 3 * pieceBytes, len(text)} {
		var out bytes.Buffer
		s := NewStream(&out)
		written := 0
		for written < len(text) {
			n := min(len(text)-written, 1+r.IntN(most))
			_, _ = s.Write([]byte(text[written : written+n]))
			written += n
			if s.p == nil {
				continue
			}
			handed := written - written%pieceBytes
			for name, in := range map[string]*feed{"the keys' stage": &s.p.keys.in, "the first claims stage": &s.p.claims.in} {
				if in.end != handed {
					t.Fatalf("pieces of at most %d bytes, %d written: %s was handed %d bytes, not the %d up to the last multiple of a piece", most, written, name, in.end, handed)
				}
				for _, c := range in.chunks {
					if len(c) != pieceBytes {
						t.Fatalf("pieces of at most %d bytes, %d written: %s was handed a piece of %d bytes, not %d", most, written, name, len(c), pieceBytes)
					}
				}
			}
			if len(s.p.carry) != written-handed {
				t.Fatalf("pieces of at most %d bytes, %d written: the stream carries %d bytes, not the %d past the last multiple of a piece", most, written, len(s.p.carry), written-handed)
			}
		}
		_ = s.Close()
		if got, want := out.String(), Secrets(text); got != want {
			t.Fatalf("pieces of at most %d bytes: %s", most, parted(got, want))
		}
	}
}
