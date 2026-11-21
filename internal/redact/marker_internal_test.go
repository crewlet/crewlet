package redact

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// Test texts' parts: a key's armour, its lines, and the alphabets the rules'
// runs are written in.
const (
	testBegin   = "-----BEGIN RSA PRIVATE KEY-----"
	testEnd     = "-----END RSA PRIVATE KEY-----"
	testKeyLine = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
	testB64     = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	testB64url  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	testAlnum   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// testRun is n bytes of alphabet drawn from r.
func testRun(r *rand.Rand, alphabet string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

// decisionsBefore is every decision rule makes in text that begins before
// before, in decide mode, or in find mode where finds is true, as a stage
// makes them one after another.
func decisionsBefore(rl rule, finds bool, text string, before int) [][4]int {
	var out [][4]int
	for from := 0; from <= len(text); {
		if finds {
			s, e, ok := rl.find(text, from)
			if !ok || s >= before {
				break
			}
			out = append(out, [4]int{s, e, e, e})
			from = max(e, beyond(text, s))
			continue
		}
		key, v, ok := rl.decide(text, from, nil)
		if !ok || key[0] >= before {
			break
		}
		out = append(out, [4]int{key[0], key[1], v[0], v[1]})
		if v[1] > v[0] {
			from = v[1]
		} else {
			from = beyond(text, key[0])
		}
	}
	return out
}

// NO TOKEN'S OR AWS SECRET'S DECISION READS PAST A BRACKET, which is what a
// claims stage stepping at a marker rests on ([ruleStage.stepAtMarker]): no
// token's run, nor an AWS secret's value, nor an armour's label takes a `[`;
// every rule but the password's decides what begins before a `[` by the text
// up to it, in decide mode and in find mode alike, whatever follows (another
// marker, the rest of a token, a value, an operator); and no rule's decision
// begins inside a marker a rule writes, an AWS secret's key that a marker
// spells among them, since no separator takes the `]` after it.
//
// Mutation: let `sk-`'s class take a `[`, let the run token's subject take
// one, or let an AWS secret's value take one ([awsSecretByte]), and a decision
// before a bracket here reads on through it.
func TestNoTokenOrAWSDecisionReadsPastABracket(t *testing.T) {
	t.Parallel()
	table := rules()
	for _, rl := range table[:len(table)-1] {
		if rl.run != nil && rl.run.in['['] {
			t.Errorf("%s: its run takes a bracket", rl.with)
		}
		if rl.valueIn != nil && rl.valueIn('[') {
			t.Errorf("%s: its value takes a bracket", rl.with)
		}
	}
	if awsSecretByte('[') {
		t.Error("an AWS secret's value takes a bracket")
	}
	if pemArmourAt().MatchString("-----BEGIN RSA[PRIVATE KEY-----") {
		t.Error("an armour's label takes a bracket")
	}

	r := rand.New(rand.NewPCG(1170, 30))
	pick := func(s ...string) string { return s[r.IntN(len(s))] }
	// Every rule's match and an AWS secret's key, separator and value,
	// each cut at every byte by a bracket below.
	matches := []string{
		"crewlet_rt1.Tq0d_8-x.subj.1792927401." + strings.Repeat("S", 43),
		"ya29." + testRun(r, testB64url, 40) + "...a",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + testRun(r, testB64url, 40),
		"sk-proj-" + testRun(r, testB64url, 30), "sk-ant-" + testRun(r, testB64url, 30), "sk-" + testRun(r, testB64url, 30),
		"xoxb-" + testRun(r, testAlnum, 30), "AKIAABCDEFGHIJKLMNOP", "ASIAABCDEFGHIJKLMNOP",
		"ghp_" + testRun(r, testAlnum, 40), "github_pat_" + testRun(r, testAlnum, 60),
		"glpat-" + testRun(r, testB64url, 30), "glrt-" + testRun(r, testB64url, 30),
		"dop_v1_" + strings.Repeat("0a", 16), "ssh-ed25519-cert-v01@openssh.com " + testRun(r, testB64, 40) + "==",
		"SessionToken=" + testRun(r, testB64, 50), `"aws_secret_access_key": "` + testRun(r, testB64, 40) + `"`,
		"SessionToken\t \"=\" " + testRun(r, testB64, 50), "SECRET_ACCESS_KEY " + testRun(r, testB64, 40),
	}
	brackets := []string{"[", Marker + "api-key]", Marker + "jwt]", Marker + "secret-access-key]", keyMarker}
	tails := func(rest string) []string {
		return []string{rest, "", " ", "= " + testRun(r, testB64, 50), testRun(r, testB64url, 60), pick(matches...)}
	}
	read := 0
	for _, m := range matches {
		for cut := 0; cut <= len(m); cut++ {
			head := pick("", "x ", "the build passed\n", testRun(r, testB64, 20)) + m[:cut]
			bracket := pick(brackets...)
			var first [][][4]int
			for k, tail := range tails(m[cut:]) {
				text := head + bracket + tail
				var got [][][4]int
				for _, rl := range table[:len(table)-1] {
					got = append(got, decisionsBefore(rl, false, text, len(head)))
					if rl.value != nil {
						got = append(got, decisionsBefore(rl, true, text, len(head)))
					}
				}
				read++
				if k == 0 {
					first = got
					continue
				}
				for i := range got {
					if !slices.Equal(got[i], first[i]) {
						t.Fatalf("%q cut at %d by %q: a decision before the bracket reads past it: %v with %q after it, %v with %q", m, cut, bracket, first[i], m[cut:], got[i], tail)
					}
				}
			}
		}
	}

	// And none begins inside a marker a rule writes, whatever is around it.
	markers := append(slices.Clone(ruleMarkers()), keyMarker)
	for _, m := range markers {
		for _, before := range []string{"", "x ", "SessionToken=", `"`, "sk-" + testRun(r, testB64url, 30)} {
			for _, after := range []string{"", " = " + testRun(r, testB64, 50), "=" + testRun(r, testB64, 50), `": "` + testRun(r, testB64, 40) + `"`, "\t\t" + testRun(r, testB64, 40), testRun(r, testB64url, 40)} {
				text := before + m + after
				for _, rl := range table[:len(table)-1] {
					for _, finds := range []bool{false, true} {
						if finds && rl.value == nil {
							continue
						}
						for _, d := range decisionsBefore(rl, finds, text, len(text)) {
							if len(before) <= d[0] && d[0] < len(before)+len(m) {
								t.Errorf("%s (finds %v) decides at %d inside the marker of %q", rl.with, finds, d[0], text)
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d texts read with a bracket at every byte of a match", read)
}

// A MARKER IS REDACTED ALIKE AT EVERY BYTE A STEP CAN END BESIDE IT: a claims
// stage steps at the last marker it holds ([ruleStage.markerToStepAt]), so a
// marker's `[`, its end, and the end of one still open, each set at every
// byte around where a stage's step ends and where its first window ends, beside
// a key's block whose reading asks what the rules claim there: a token glued
// to a marker the text holds either side of it, a run read on past a window
// and a marker straight after it, and a run whose end, a marker of another
// rule glued to it, falls at those bytes too.
//
// Mutation: end a step at a marker's `[` rather than past it
// ([ruleStage.stepAtMarker]), and a stage there decides nothing.
func TestAMarkerIsRedactedAlikeAtEveryByteAStepCanEndBesideIt(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 31))
	block := testBegin + "\n" + testKeyLine + "\n" + testKeyLine + "\n"
	filler := strings.Repeat("the build passed ok\n", stageStep/20)
	shapes := map[string]func(n int) string{
		"a token glued to a marker either side": func(int) string {
			return "sk-" + testRun(r, testB64url, 40) + Marker + "api-key]" + "sk-" + testRun(r, testB64url, 40) + " Password: hunter2x9\n"
		},
		"a run and a marker straight after it": func(n int) string {
			return "sk-ant-" + testRun(r, testB64url, n) + Marker + "jwt]" + " pwd=hunter2x9\n"
		},
		"a run and a token of an earlier rule glued to it": func(n int) string {
			return "x sk-" + testRun(r, testB64url, n) + "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + testRun(r, testB64url, 60) + " |\n"
		},
	}
	checked := 0
	for name, shape := range shapes {
		for _, at := range []int{stageAdvance, stageStep} {
			for d := -2; d <= 1; d++ {
				for _, n := range []int{stageStep - 1, stageStep + 2} {
					text := block + filler[:at+d-len(block)] + shape(n) + block + testEnd + "\n" + filler
					want := Secrets(text)
					if got := staged(text); got != want {
						t.Fatalf("%s, %d bytes before it and a run of %d: the stages wrote another redaction than the passes over it whole %s", name, at+d, n, parted(got, want))
					}
					var out bytes.Buffer
					s := NewStream(&out)
					for i := 0; i < len(text); {
						k := min(len(text)-i, 1+r.IntN(96<<10))
						_, _ = s.Write([]byte(text[i : i+k]))
						i += k
					}
					_ = s.Close()
					if got := out.String(); got != want {
						t.Fatalf("%s, %d bytes before it and a run of %d, cut at random: %s", name, at+d, n, parted(got, want))
					}
					checked++
				}
			}
		}
	}
	t.Logf("%d texts", checked)
}

// A CLAIMS STAGE DECIDES UP TO THE MARKER IT HOLDS ([ruleStage.markerToStepAt]):
// a stream writes [Secrets] of dense random-cut texts of every token, AWS
// secret, key armour and marker, and no token's or AWS secret's claim is made,
// nor a run's end set, further past where it lies than a stage's place in the
// table lets it, so a reader behind every rule stays under the claims' lag
// ([streamBound]). Exempt: the password's reader, whose decisions read past a
// marker, and a Google token's end before the dots it may end on, set only once
// they stop ([ruleStage.carryToken]); a text here runs them past the bound.
//
// Mutation: let the password's reader step at a marker; step at an open marker
// only where the stage holds more than a step; or count a Google token's end
// from where it lies, not where its dots stop.
func TestAClaimsStageDecidesUpToTheMarkerItHolds(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 32))
	units := []func() string{
		func() string { return "sk-" + testRun(r, testB64url, 20+r.IntN(400)) },
		func() string { return "sk-ant-" + testRun(r, testB64url, 20+r.IntN(400)) },
		func() string {
			return "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + testRun(r, testB64url, r.IntN(1200))
		},
		func() string { return "ya29." + testRun(r, testB64url, 30) + strings.Repeat(".", r.IntN(4)) + "a" },
		func() string { return "ya29." + testRun(r, testB64url, 30) + strings.Repeat(".", 1+r.IntN(600)) },
		func() string {
			return "crewlet_rt1.Tq0d_8-x." + testRun(r, testAlnum, r.IntN(4000)) + ".1792927401." + testRun(r, testB64url, 43)
		},
		func() string { return "SessionToken=" + testRun(r, testB64, 40+r.IntN(400)) },
		func() string { return "aws_secret_access_key = " + testRun(r, testB64, 40) },
		func() string {
			return "SessionToken\"" + strings.Repeat(" ", r.IntN(300)) + "= " + testRun(r, testB64, 50)
		},
		func() string { return "ssh-ed25519-cert-v01@openssh.com " + testRun(r, testB64, 40+r.IntN(400)) },
		func() string { return "AKIA" + testRun(r, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", 16) },
		func() string { return "ghp_" + testRun(r, testAlnum, 36+r.IntN(400)) },
		func() string { return "pwd=" + testRun(r, testB64, 8) },
		func() string { return "Password:\n" },
		func() string { return testBegin + "\n" + testKeyLine + "\n" },
		func() string { return testEnd + "\n" },
		func() string { return testBegin + " " + testKeyLine + " " + testEnd },
		func() string { return testKeyLine },
		func() string { return "[" },
		func() string { return Marker + "api-key]" },
	}
	seps := []string{" ", "\n", "", `\n`, "\t", "|", `"`, ".", "-", "=", ":"}
	texts := map[string]string{}
	for i := range 4 {
		var b strings.Builder
		for b.Len() < 1<<20+r.IntN(1<<20) {
			b.WriteString(units[r.IntN(len(units))]())
			b.WriteString(seps[r.IntN(len(seps))])
		}
		texts[fmt.Sprintf("dense text %d", i)] = b.String()
	}
	run := testRun(r, testB64url, 2<<20)
	block := testBegin + "\n" + testKeyLine + "\n" + testKeyLine + "\n" + testKeyLine + "\n" + testEnd + "\n"
	tail := strings.Repeat("ok\n", stageStep/3+1)
	texts["an escaped break before a run"] = "a key: " + testBegin + "\n" + testKeyLine + "\n\n" + `{"pwd": "hunter2x9\n` + "sk-" + run + `"}` + "\n" + tail
	texts["a password's key a block reads, a run, a suffix"] = testBegin + "\n" + testKeyLine + "\n" + testKeyLine + "\nx Password: |\nu1SU1Lf9x sk-" + run + " |\n" + tail
	texts["a run, a key's block, a run of an earlier rule"] = "x sk-" + run[:300<<10] + " |\n" + block + "sk-ant-" + run + "\n" + tail
	var pairs strings.Builder
	for pairs.Len() < 1<<20 {
		pairs.WriteString(testBegin + "\n" + testKeyLine + "\nx Password: |\nu1SU1Lf9x sk-" + testRun(r, testB64url, 20+r.IntN(400)) + " |\n")
	}
	texts["password's keys a block reads, each beside a token and a suffix"] = pairs.String()
	texts["a run, a key on one line, a run of an earlier rule"] = "x sk-" + run[:300<<10] + " |\n" + testBegin + " " + testKeyLine + " " + testKeyLine + " " + testEnd + "\n" + "sk-ant-" + run + "\n" + tail
	texts["a key's block, a Google token whose dots run past the bound"] = block + "ya29." + run[:30] + strings.Repeat(".", 1<<20) + " |\n" + tail

	longest := 0
	for _, m := range ruleMarkers() {
		longest = max(longest, len(m))
	}
	for name, text := range texts {
		want := Secrets(text)
		var out bytes.Buffer
		s := NewStream(&out)
		s.observe = &keyObserver{}
		var late []string
		var places map[*claimSource]int
		// held is the byte a claims stage's token may end before, which
		// it counts while it runs rather than decides: a Google token's
		// dot.
		var held map[*claimSource]byte
		s.observe.changed = func(c *claimSource, place int) {
			if places == nil {
				// The place of each source's stage in the table: a
				// reader is behind every rule.
				places = map[*claimSource]int{}
				held = map[*claimSource]byte{}
				i := 0
				for st := s.p.claims; ; i++ {
					places[st.src] = i
					if st.r.run != nil {
						held[st.src] = st.r.run.held
					}
					next, ok := st.to.(*ruleStage)
					if !ok {
						break
					}
					st = next
				}
				for _, rd := range s.p.claims.lastTo() {
					if rd.mode == reads {
						places[rd.src] = -1
						continue
					}
					places[rd.src] = i + 1
				}
			}
			i := places[c]
			if i < 0 {
				return
			}
			bound := (i + 1) * (stageStep + pieceBytes + longest)
			// From where the dots a token's end lies before stop.
			from := place
			for b := held[c]; b != 0 && from < len(text) && text[from] == b; {
				from++
			}
			if by := s.p.keys.arrived - from; by >= bound && len(late) < 3 {
				late = append(late, fmt.Sprintf("the stage %d in the table changed a claim at %d, %d bytes behind the text, past its %d", i, place, by, bound))
			}
		}
		for at := 0; at < len(text); {
			n := min(len(text)-at, 1+r.IntN(128<<10))
			_, _ = s.Write([]byte(text[at : at+n]))
			at += n
		}
		_ = s.Close()
		if got := out.String(); got != want {
			t.Errorf("%s (%d KiB), cut at random: %s", name, len(text)>>10, parted(got, want))
		}
		for _, l := range late {
			t.Errorf("%s (%d KiB): %s", name, len(text)>>10, l)
		}
	}
}

// lastTo is the readers behind the last claims stage of the chain s begins.
func (s *ruleStage) lastTo() []*ruleStage {
	for {
		next, ok := s.to.(*ruleStage)
		if !ok {
			break
		}
		s = next
	}
	var readers []*ruleStage
	for _, rd := range s.to.(fanout) {
		readers = append(readers, rd.(*ruleStage))
	}
	return readers
}
