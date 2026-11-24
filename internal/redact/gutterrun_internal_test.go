package redact

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// gutterRunKL, gutterRunLast and gutterRunEnd are a key's body line, its last
// run and its END, as the shapes below write them.
const (
	gutterRunKL   = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
	gutterRunLast = "q8Zr4TfL2w=="
	gutterRunEnd  = "-----END RSA PRIVATE KEY-----"
)

// loggedToo is each text, and each written as a logged JSON string.
func loggedToo(texts map[string]string) map[string]string {
	all := map[string]string{}
	for name, text := range texts {
		q, err := json.Marshal(text)
		if err != nil {
			panic(err)
		}
		all[name], all[name+", logged"] = text, `{"log": `+string(q)+"}\n"
	}
	return all
}

// A KEY'S LAST RUN IS NO GUTTER: where a key's END line goes on to a password's
// key and a line below repeats the key's last run as the gutter, read back from
// the END or under a body line, an echo, a table row, a quoted string or a
// JSON array's strings, the run is a block of the key's on each line that
// spells it, and the text writes as Secrets does.
//
// Mutation: write no block over the runs ([claims.keyRuns]), note none where
// no value is read below ([valueRead.decided]), drop the decoded reading's
// ([decodedReading]), or leave the gutter seam past the run
// ([valueRead.withRuns]), and the run or the value is shown, or the key's line
// is two blocks.
func TestAKeysLastRunIsNoGutter(t *testing.T) {
	t.Parallel()
	begin, last, kl, end := "-----BEGIN RSA PRIVATE KEY-----\n", gutterRunLast, gutterRunKL, gutterRunEnd
	for name, text := range loggedToo(map[string]string{
		"glued, back from END":       kl + "\n" + last + end + "|passwd:\n" + last + "-placeholder9x\n",
		"a body line before the END": begin + kl + "\n" + kl + " " + end + "|passwd:\n" + kl + " placeholder9x\n",
		"under an echo": kl + "\n" + last + "\necho \"u1SU1Lf=-----END DSA PRIVATE KEY-----\" ; pwd=\" >> k\n" +
			"echo \"u1SU1Lf=\" >> k\n",
		"under an echo, a value after the run": kl + "\necho \"" + last + " -----END DSA PRIVATE KEY-----\" ; pwd=\" >> k\n" +
			"echo \"" + last + " placeholder9x\" >> k\n",
		"in a quoted string": kl + "\n\"" + last + " " + end + "\" ; password: \"\n\"" + last + " placeholder9x\"\n",
		"in a table": kl + "\n| " + last + "-----END EC PRIVATE KEY-----\" ; password =  |\n| " + last +
			"-placeholder9x |\n| " + kl[:56] + " -----END EC PRIVATE KEY----- |\n",
		"in a JSON array": begin + kl + "\n" + last + end + "\n  \"" + last + end + "\" ; password = \",\n  \"" + last +
			"-placeholder9x\",\n  \"" + strings.TrimSuffix(begin, "\n") + "\",\n",
	}) {
		once := Secrets(text)
		for _, w := range []string{"placeholder9x", last, kl[:40], "u1SU1Lf="} {
			if strings.Contains(once, w) {
				t.Errorf("%s: %q is shown:\n text %q\n once %q", name, w, text, once)
			}
		}
		if strings.Contains(once, keyMarker+" "+keyMarker) {
			t.Errorf("%s: the key's line is written as two blocks, its run apart:\n text %q\n once %q", name, text, once)
		}
		writesAsSecrets(t, name, text, once)
	}
}

// A KEY'S LAST RUN IS NO GUTTER WHATEVER STANDS BETWEEN ITS END AND THE KEY:
// nothing, a `|`, a space, a tab or two spaces, under no gutter or a log's
// timestamp, the run glued to the END or a space before it, and repeated glued
// to the value below or alone on a blank line above it. The run is hidden on
// every line, the value too, the key's line is one block, and the text writes
// as Secrets does ([writesAsSecrets]).
//
// Mutation: let a marker matched whole end no word of both lines ([gutterLen]),
// and under a key's line that spaces its END from the key, the value glued to
// the run's marker is shown; or note no run on a blank line
// ([valueRead.runOn]), and the run alone above the value is.
func TestAKeysLastRunIsNoGutterWhateverStandsBeforeItsKey(t *testing.T) {
	t.Parallel()
	begin, last, kl, end := "-----BEGIN RSA PRIVATE KEY-----\n", gutterRunLast, gutterRunKL, gutterRunEnd
	for _, sep := range []struct{ name, s string }{{"nothing", ""}, {"a bar", "|"}, {"a space", " "}, {"a tab", "\t"}, {"two spaces", "  "}} {
		for _, gutter := range []struct{ name, s string }{{"no gutter", ""}, {"a timestamp", "2026-10-06T09:00:00.123Z "}} {
			for _, run := range []struct{ name, key, below string }{
				{"glued to the END and the value", last, last + "-placeholder9x\n"},
				{"glued to the END, alone above the value", last, last + "-\n" + gutter.s + "placeholder9x\n"},
				{"a space before the END", last + " ", last + " placeholder9x\n"},
			} {
				name := sep.name + " before the key, " + gutter.name + ", the run " + run.name
				text := begin + kl + "\n" + gutter.s + run.key + end + sep.s + "passwd:\n" + gutter.s + run.below + gutter.s + "done\n"
				for form, text := range loggedToo(map[string]string{name: text}) {
					once := Secrets(text)
					for _, w := range []string{"placeholder9x", last, kl[:40]} {
						if strings.Contains(once, w) {
							t.Errorf("%s: %q is shown:\n text %q\n once %q", form, w, text, once)
						}
					}
					if strings.Contains(once, keyMarker+" "+keyMarker) {
						t.Errorf("%s: the key's line is written as two blocks:\n text %q\n once %q", form, text, once)
					}
					writesAsSecrets(t, form, text, once)
				}
			}
		}
	}
}

// A GUTTER THE LINE AFTER REPEATS IS THE WRAPPING'S, though it is a key's last
// run before its END: a wrapping that writes the run on every line writes it
// on the line after the value's too, and there it stays the gutter, shown on
// every line, the value after it hidden.
//
// Mutation: ask nothing of the line after ([wraps]), and the gutter is taken
// for a key's.
func TestAGutterTheLineAfterRepeatsIsTheWrappings(t *testing.T) {
	t.Parallel()
	kl, end := gutterRunKL, gutterRunEnd
	for name, c := range map[string]struct{ gutter, text string }{
		"a key's line on every line": {kl, kl + " " + kl + "\n" + kl + " " + end + "|password: \"\n" + kl +
			" \"placeholder9x\"\n" + kl + " done\n"},
		"a short run on every line": {"u1SU1Lf=", "u1SU1Lf= hello\nu1SU1Lf= " + end + "|password: \"\nu1SU1Lf= " +
			"\"placeholder9x\"\nu1SU1Lf= done\n"},
	} {
		for form, text := range loggedToo(map[string]string{name: c.text}) {
			once := Secrets(text)
			if strings.Contains(once, "placeholder9x") || !strings.Contains(once, c.gutter+" "+Marker) &&
				!strings.Contains(once, c.gutter+` "`+Marker) && !strings.Contains(once, c.gutter+` \"`+Marker) {
				t.Errorf("%s: the value is not hidden after its gutter:\n text %q\n once %q", form, text, once)
			}
			writesAsSecrets(t, form, text, once)
		}
	}
}

// A WRAPPING'S GUTTER BEFORE A KEY'S RUN STAYS THE WRAPPING'S: a diff's `+`
// before the key's run on its END line is no part of the run, and a gutter a
// wrapping glues to the run on every line, blank lines among them, is read on
// each line below as the gutter; under both the value is hidden, and the text
// writes as Secrets does ([writesAsSecrets]).
//
// Mutation: take the `+` into the run ([bodyRunAt]), and the diff's `+` is
// taken with it; or ask the line after for a space after the run ([wraps]),
// and the value is shown.
func TestAWrappingsGutterBeforeAKeysRunStaysTheWrappings(t *testing.T) {
	t.Parallel()
	kl, last, m56 := gutterRunKL, gutterRunLast, gutterRunKL[:56]
	glued := last + "-"
	for name, text := range loggedToo(map[string]string{
		"a diff's +": "-----BEGIN RSA PRIVATE KEY-----\n" + kl + "\n+" + m56 + " " + gutterRunEnd + " Password:\n+\n" +
			"+placeholder9x\n }\n",
		"glued to the run on every line": "u1SU1Lf=\n" + last + gutterRunEnd + "\ttoken: abc PASSWORD=\n" + glued + "\n" +
			glued + "\n" + glued + glued + "placeholder9x\n" + glued + "-----BEGIN RSA PRIVATE KEY-----\n" + glued + kl + "\n",
	}) {
		once := Secrets(text)
		if strings.Contains(once, "placeholder9x") {
			t.Errorf("%s: the value is shown:\n text %q\n once %q", name, text, once)
		}
		if strings.HasPrefix(name, "a diff's +") && !strings.Contains(once, "+"+Marker) {
			t.Errorf("%s: the diff's + is taken with the run:\n text %q\n once %q", name, text, once)
		}
		writesAsSecrets(t, name, text, once)
	}
	// Logged: a key's line under a wrapping that glues its gutter to every
	// line below it.
	q, err := json.Marshal("-----BEGIN EC PRIVATE KEY-----\r\n" + glued + kl + "\r\n" + kl + " -----END EC PRIVATE KEY-----" +
		"|token: abc Root password: \r\n" + glued + "\r\n" + glued + "\r\n" + glued + "placeholder9x\r\n" + glued + "done\r\n")
	if err != nil {
		t.Fatal(err)
	}
	text := `{"log": ` + string(q) + "}\n"
	if once := Secrets(text); strings.Contains(once, "placeholder9x") {
		t.Errorf("logged, glued below a key's line: the value is shown:\n text %q\n once %q", text, once)
	} else {
		writesAsSecrets(t, "logged, glued below a key's line", text, once)
	}
}

// A CERTIFICATE'S RUN IS NO KEY'S: before a certificate's END on the line of a
// password's key, under a key's block, a run the value's line repeats is the
// wrapping's gutter, as it always was, shown on both lines, and no block of a
// key's is written over it; the value is hidden, and the text writes as
// Secrets does ([writesAsSecrets]).
//
// Mutation: take a run before any armour for a key's ([bodyRunAt]), and a
// certificate's run is written as a key's block.
func TestACertificatesRunIsNoKeys(t *testing.T) {
	t.Parallel()
	m56 := gutterRunKL[:56]
	for name, text := range loggedToo(map[string]string{
		"a space before the END": "-----BEGIN RSA PRIVATE KEY-----\n" + gutterRunKL + "\n" + gutterRunEnd + "\n" + gutterRunKL +
			"\n" + m56 + " -----END CERTIFICATE-----|passwd:\n" + m56 + " placeholder9x\n",
	}) {
		once := Secrets(text)
		if strings.Contains(once, "placeholder9x") || strings.Count(once, keyMarker) != 1 || !strings.Contains(once, m56+" "+Marker) {
			t.Errorf("%s: the value is not hidden after its gutter, or a key's block is written:\n text %q\n once %q", name, text, once)
		}
		writesAsSecrets(t, name, text, once)
	}
}

// A KEY'S RUN A VALUE'S LINE OPENS WITH IS THE KEYS': the claim names the value
// alone, and the run on the key's line and on the value's is a block of the
// key's, so a block read back from an END below covers what it always did;
// the value and every copy of it are hidden, and the text writes as Secrets
// does ([writesAsSecrets]).
//
// Mutation: write no block over the runs ([claims.keyRuns]), and the run opens
// the key's line and the value's.
func TestAKeysRunAValuesLineOpensWithIsTheKeys(t *testing.T) {
	t.Parallel()
	kl, m56, end := gutterRunKL, gutterRunKL[:56], gutterRunEnd
	for name, text := range loggedToo(map[string]string{
		"a key's END below": m56 + " " + kl + " " + end + "\n" + m56 + " " + end + "|passwd:\n" + m56 + " hunter2x9\n" +
			"hunter2x9 \" " + m56 + " " + end + "\n",
		"a second key below": m56 + " " + end + "|passwd:\n" + m56 + " hunter2x9 fAt0gun+\nPassword: |\n" + m56 + " \" " +
			m56 + "\n" + m56 + " " + end + "|passwd:\n" + m56 + " \" " + m56 + "\n",
	}) {
		once := Secrets(text)
		if strings.Contains(once, "hunter2x9") {
			t.Errorf("%s: the value is shown:\n text %q\n once %q", name, text, once)
		}
		for _, line := range strings.Split(strings.ReplaceAll(once, `\n`, "\n"), "\n") {
			if strings.HasPrefix(strings.TrimPrefix(line, `{"log": "`), m56) {
				t.Errorf("%s: a line opens with the key's run:\n text %q\n once %q", name, text, once)
			}
		}
		writesAsSecrets(t, name, text, once)
	}
}

// A VALUE WHOSE GUTTER REACHES NO KEY'S RUN READS AS IT DID: under a key on
// one line, a value alone on its line under a log's timestamp, shaped like a
// key's run, has a gutter that ends before any run, so no block is added and
// the text redacts as it always did, the next key's label its own. A value is
// never taken for a key's run by its shape: one read so passed the value and
// took that label for its second range.
func TestAValueWhoseGutterReachesNoKeysRunReadsAsItDid(t *testing.T) {
	t.Parallel()
	text := "{\"log\": \"2026-10-06T09:00:00.000Z -----BEGIN DSA PRIVATE KEY----- " + gutterRunKL +
		" -----END DSA PRIVATE KEY-----passwd:\\n2026-10-06T09:00:01.037Z u1SU1Lf=\\n2026-10-06T09:00:02.074Z Retype " +
		"password:\\n2026-10-06T09:00:03.111Z placeholder9y\\n\"}\n"
	want := "{\"log\": \"2026-10-06T09:00:00.000Z [REDACTED:private-key]passwd:\\n[REDACTED:password] [REDACTED:password]\\n" +
		"[REDACTED:password] Retype password:\\n[REDACTED:password] placeholder9y\\n\"}\n"
	if once := Secrets(text); once != want {
		t.Errorf("the text redacts otherwise:\n text %q\n once %q\n want %q", text, once, want)
	}
	writesAsSecrets(t, "a value under a one-line key", text, want)
}

// A KEY'S RUNS ARE WRITTEN BY THE WINDOW THAT READS ITS KEY, AND ONE THAT READ
// PAST THE KEY BEFORE THEM REFUSES: a stream whose stages run writes each run as
// a block where the whole text's keys do ([keyStage.runsIn]), so at its own
// hold it writes Secrets; at a hold the claims trail past, the window that
// read the key decided early and wrote none, and the stream says so
// ([ErrDecidedEarly]) rather than write another text than Secrets
// ([checkReading]), with a block of the key's on that line or none.
//
// Mutation: write no run from the stream's claims ([keyStage.runsIn]), or let
// a late claim's runs pass unchecked ([keyStage.handed]), and the stream
// writes a run Secrets hides.
func TestAKeysRunsAreWrittenByTheWindowThatReadsItsKey(t *testing.T) {
	t.Parallel()
	filler := strings.Repeat("a line of a log that names nothing at all\n", 8<<10)
	last, end := gutterRunLast, gutterRunEnd
	for form, text := range loggedToo(map[string]string{
		"a body line above":    filler + gutterRunKL + "\n" + last + " " + end + "|passwd:\n" + last + " placeholder9x\n" + filler,
		"no block on the line": filler + last + " " + end + "|passwd:\n" + last + " placeholder9x\n" + filler,
	}) {
		if len(text) <= wholeBytes {
			t.Fatalf("%s: %d bytes is redacted whole, so this shape tests no stage", form, len(text))
		}
		once := Secrets(text)
		if strings.Contains(once, last) {
			t.Fatalf("%s: Secrets shows the run, so this shape tests nothing:\n %q", form, once[len(filler)-1:])
		}
		got := readCut(text, holdBytes(), cuts(nil, len(text), 0))
		if got.err != nil || got.out != once {
			t.Errorf("%s at the stream's own hold: %v, %s", form, got.err, parted(got.out, once))
		}
		got = readCut(text, 200<<10, cuts(nil, len(text), 0))
		if !errors.Is(got.err, ErrDecidedEarly) {
			t.Errorf("%s at a hold of 200 KiB: Close answered %v, %d windows decided early", form, got.err, got.early)
		}
		checkReading(t, form+" at a hold of 200 KiB", text, got)
	}
}
