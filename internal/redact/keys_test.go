package redact_test

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// keyLine is one line of a key's body at PEM's width: base64's alphabet, made
// of filler — a shape, not a key.
const keyLine = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"

// sshLine is one line at OpenSSH's width, seventy characters.
const sshLine = "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAABlwAAAAdzc2gtcn"

// mimeLine is one line at MIME's width, seventy-six characters: what Java's
// MIME encoder and Python's base64.encodebytes wrap a body at.
const mimeLine = keyLine + "Q8vN2xRk7TpZ"

func pemBody(lines int) string { return strings.Repeat(keyLine+"\n", lines) }

// A KEY WHOSE END WAS NEVER WRITTEN — a run that crashed mid-print — is still
// a key. It used to match nothing, because the rule needed the END line, so
// its whole body reached the record.
//
// Mutation: drop the structural reading of an unclosed block, and the body
// survives.
func TestAKeyWhoseEndWasNeverWrittenIsRedactedToItsBody(t *testing.T) {
	text := "cloning\n-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(5) + "u1SU1Lf=\npanic: the run crashed\n"
	got := redact.Secrets(text)
	if strings.Contains(got, "MIIEowIBAAKCAQEA") || strings.Contains(got, "u1SU1Lf=") {
		t.Fatalf("key material survived: %q", got)
	}
	if got != "cloning\n"+redact.Marker+"private-key]\npanic: the run crashed\n" {
		t.Fatalf("got %q; want the block replaced and the lines around it kept whole", got)
	}
}

// A GREP OF THE ARMOUR LINE IS NOT A KEY: the header has no body under it, and
// what follows it — the next result, a word — is the reader's, not the key's.
func TestAGrepOfTheArmourLineSwallowsNothing(t *testing.T) {
	for _, text := range []string{
		"a.pem:1:-----BEGIN RSA PRIVATE KEY-----\nb.pem:1:-----BEGIN RSA PRIVATE KEY-----\nsrc/main.go:3: func main() {\n",
		"-----BEGIN RSA PRIVATE KEY-----\nDone\n",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"echo '-----BEGIN EC PRIVATE KEY-----' > key.pem\nls -la\n",
	} {
		if got := redact.Secrets(text); got != text {
			t.Errorf("Secrets(%q) = %q; want it unchanged", text, got)
		}
		if redact.Contains(text) {
			t.Errorf("Contains(%q) = true for a header with no key under it", text)
		}
	}
}

// AN ENCRYPTED KEY in the traditional form carries RFC 1421 headers between
// its armour and its body; they are part of the block, closed or not.
func TestAnEncryptedKeyIsRedactedWithItsHeaders(t *testing.T) {
	block := "-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\n" +
		"DEK-Info: AES-128-CBC,0123456789ABCDEF0123456789ABCDEF\n\n" + pemBody(4)
	for name, text := range map[string]string{
		"closed":   "before\n" + block + "-----END RSA PRIVATE KEY-----\nafter\n",
		"unclosed": "before\n" + block + "after the crash\n",
	} {
		got := redact.Secrets(text)
		if strings.Contains(got, "DEK-Info") || strings.Contains(got, keyLine) {
			t.Errorf("%s: the encrypted block survived: %q", name, got)
		}
		if !strings.HasPrefix(got, "before\n"+redact.Marker+"private-key]") || !strings.Contains(got, "after") {
			t.Errorf("%s: got %q; want the block replaced and the lines around it kept", name, got)
		}
	}
}

// AN OPENSSH KEY wraps at seventy, and one with no END is read by the same
// structure. Its body's last line is shorter than the rest and is taken with
// it — which is also why a single word on the line after a body would be: a
// block with no END has nothing but its alphabet to stop at.
func TestAnOpenSSHKeyWithNoEndIsRedacted(t *testing.T) {
	text := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat(sshLine+"\n", 6) +
		"AAAEBm9uZQ==\nthe next step\n"
	got := redact.Secrets(text)
	if strings.Contains(got, sshLine) || got != redact.Marker+"private-key]\nthe next step\n" {
		t.Fatalf("got %q; want the body replaced and the next line kept", got)
	}
}

// PKCS#8's ENCRYPTED form is still the key, under a passphrase that is often
// in the same environment. It was no shape this pass knew.
func TestAnEncryptedPKCS8KeyIsRedacted(t *testing.T) {
	text := "-----BEGIN ENCRYPTED PRIVATE KEY-----\n" + pemBody(3) + "-----END ENCRYPTED PRIVATE KEY-----"
	if got := redact.Secrets(text); got != redact.Marker+"private-key]" {
		t.Fatalf("got %q; want exactly one marker", got)
	}
}

// A KEY IN A JSON STRING has its line breaks escaped, and is the commonest way
// one reaches a log — a service account's file printed by a tool. Without an
// END, its escaped lines are read as lines.
func TestAKeyEscapedInAStringWithNoEndIsRedacted(t *testing.T) {
	text := `{"private_key": "-----BEGIN PRIVATE KEY-----\n` + keyLine + `\n` + keyLine + `\n` + keyLine
	got := redact.Secrets(text)
	if strings.Contains(got, keyLine) {
		t.Fatalf("an escaped key's body survived: %q", got)
	}
	if !strings.HasPrefix(got, `{"private_key": "`+redact.Marker+"private-key]") {
		t.Fatalf("got %q; want the string's prefix kept", got)
	}
}

// AN END FAR PAST A HEADER DOES NOT CLOSE IT. The rule ran from a BEGIN to
// whatever END came next, at any distance, and took everything between.
//
// Mutation: drop the [redact.MaxKeyBlockBytes] bound, and the log between is
// eaten.
func TestAnArmourLineDoesNotReachAnEndFarPastIt(t *testing.T) {
	log := strings.Repeat("ordinary log line that is not a key\n", (redact.MaxKeyBlockBytes/36)+10)
	text := "x:-----BEGIN RSA PRIVATE KEY-----\n" + log + "y:-----END RSA PRIVATE KEY-----\n"
	if got := redact.Secrets(text); got != text {
		t.Fatalf("a header and an END %d bytes apart were redacted as one key", len(log))
	}
}

// A HEADER WITH NO KEY UNDER IT stops at the next BEGIN: the key after it is
// redacted on its own, and the lines between are kept. They were swallowed
// from the first header to the second key's END.
func TestAHeaderWithNoKeyKeepsTheLinesBeforeTheNextKey(t *testing.T) {
	text := "grep: -----BEGIN RSA PRIVATE KEY-----\nIMPORTANT LOG\n" +
		"-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(2) + "-----END RSA PRIVATE KEY-----\n"
	got := redact.Secrets(text)
	if !strings.Contains(got, "IMPORTANT LOG") || strings.Contains(got, keyLine) {
		t.Fatalf("got %q; want the log kept and the key redacted", got)
	}
	if strings.Count(got, redact.Marker+"private-key]") != 1 {
		t.Fatalf("got %q; want one marker, for the key", got)
	}
}

// grepOfTheBeginLine is a run's transcript in which a grep matched the BEGIN
// line, and the run then went on with other work.
const grepOfTheBeginLine = "[tool] bash: grep -rn BEGIN certs\n" +
	"certs/a.pem:1:-----BEGIN RSA PRIVATE KEY-----\n" +
	"[tool] bash: go test ./...\n" +
	"ok  \tpkg\t0.1s\n" +
	"FAIL\tother\t0.2s\n"

// grepOfBothArmours is the same transcript once a later grep matched the END.
const grepOfBothArmours = grepOfTheBeginLine +
	"[tool] bash: grep -rn END certs\n" +
	"certs/a.pem:27:-----END RSA PRIVATE KEY-----\n" +
	"[tool] bash: done\n"

// AN END CLOSES ONLY A BLOCK OF KEY: everything between it and its BEGIN has to
// be a key's — headers, a blank line, base64 at the encoder's width — or the
// END closes nothing, and read back from it, the same prose is where the read
// stops. The rule paired an END with any BEGIN within 64 KiB before it,
// whatever lay between, so a grep's match for the BEGIN line and a later one
// for the END took every line of the run between them — the test run, its
// failure — out of the record and the live view alike.
//
// A grep's match for the END line is no key either, whatever path it is
// under: a path in mixed case is a run as wide as a key's line, and read back
// across the `.pem:28:` before the armour as the key's head across a cut, it
// was redacted with the END.
//
// Lines of a key under another wrapping are a key's whatever wraps them: each
// ends in a piece of one where the END's wrapping puts no prefix, and is read
// back as a line a cut wrote into, as far up as such lines go.
//
// Mutation: pair an END with the BEGIN before it without reading what is
// between, and the transcript between the two greps is one marker; cross a cut
// written straight before an END to any run at a full line's width, and the
// paths are redacted; read a line whose end holds a piece back as no line a
// reading goes on from, and the key's lines under the other wrapping above the
// last are published.
func TestAnEndClosesOnlyABlockOfKey(t *testing.T) {
	for name, text := range map[string]string{
		"a grep's two matches, a run between": grepOfBothArmours,
		"one command naming both armours":     "grep -e '-----BEGIN RSA PRIVATE KEY-----' -e '-----END RSA PRIVATE KEY-----' key.pem\n",
		"a placeholder between the armours":   "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n",
		"a sentence between the armours":      "-----BEGIN PRIVATE KEY-----\nyour key goes here\n-----END PRIVATE KEY-----\n",
		"the armours one after the other":     "a.pem:1:-----BEGIN RSA PRIVATE KEY-----\na.pem:27:-----END RSA PRIVATE KEY-----\n",
		"a grep's match for the END, under a path in mixed case": "src/main/resources/certs/ServerPrivateKey.pem:28:-----END PRIVATE KEY-----\n" +
			"internal/Redact/testdata/fixtures/rsaprivatekey.pem:27:-----END PRIVATE KEY-----\n",
		"a sentence naming the END after a path": "Delete " + mixedCasePath + " first; the file ends at -----END PRIVATE KEY----- and nothing follows.\n",
		// Base64 at a body's width that does not read like a key's — a
		// hash's hex, a path — is no key's body read back from an END.
		"hex before an END": strings.Repeat(strings.Repeat("9f86d081884c7d65", 4)+"\n", 3) + "-----END RSA PRIVATE KEY-----\n",
	} {
		if got := redact.Secrets(text); got != text {
			t.Errorf("%s: Secrets =\n%s\nwant it unchanged", name, got)
		}
	}
	text := "-----BEGIN RSA PRIVATE KEY-----\nsee below\nx:" + keyLine + "\nx:" + keyLine + "\nx:" + keyLine +
		"\ny:-----END RSA PRIVATE KEY-----\n"
	want := "-----BEGIN RSA PRIVATE KEY-----\nsee below\nx:" + marker + "\n"
	if got := redact.Secrets(text); got != want {
		t.Errorf("a key's lines under another wrapping than its END's:\n got %q\nwant %q", got, want)
	}
}

// A KEY IS READ BACK FROM ITS END where no BEGIN's block reached it: a body cut
// off from its BEGIN by lines another process wrote — two, which the read
// crosses back to the part the BEGIN's block holds, or three, which no read
// crosses — by prose straight after the armour, or whose BEGIN is not in the
// text at all, a stream's end read from inside the key. Its body used to be published whole once the
// structure stopped a forward read short of it, because nothing read back.
//
// Mutation: read a key only forward from its BEGIN, and every one of these
// shows its body.
func TestAKeyIsReadBackFromItsEnd(t *testing.T) {
	m := redact.Marker + "private-key]"
	numbered := func(from int, lines ...string) string {
		var b strings.Builder
		for i, line := range lines {
			fmt.Fprintf(&b, "%6d\u2192%s\n", from+i, line)
		}
		return b.String()
	}
	for name, c := range map[string]keyForm{
		"its BEGIN not in the text": {
			pemBody(3) + "u1SU1Lf=\n-----END RSA PRIVATE KEY-----\nnext\n",
			m + "\nnext\n",
		},
		"its BEGIN not in the text, under line numbers": {
			numbered(10, keyLine, keyLine, "u1SU1Lf=", "-----END RSA PRIVATE KEY-----", "next"),
			"    10\u2192" + m + "\n    14\u2192next\n",
		},
		"cut off from its BEGIN by prose": {
			"-----BEGIN RSA PRIVATE KEY-----\nsee below\n" + keyLine + "\n-----END RSA PRIVATE KEY-----\n",
			"-----BEGIN RSA PRIVATE KEY-----\nsee below\n" + m + "\n",
		},
		// Two lines in a row end the block read forward, and the END reads
		// back across them to the key's line above: one key, one marker.
		"broken by two lines in a row": {
			"-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(2) + "[worker 3] connected\n[worker 4] connected\n" +
				pemBody(2) + "u1SU1Lf=\n-----END RSA PRIVATE KEY-----\nnext\n",
			m + "\nnext\n",
		},
		"broken twice, read from both ends": {
			"-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(2) + "[worker 3] connected\n[worker 4] connected\n" +
				pemBody(2) + "[worker 5] connected\n" + pemBody(2) + "-----END RSA PRIVATE KEY-----\n",
			m + "\n",
		},
		// Three no read crosses: each part is read from its own armour.
		"broken by three lines in a row": {
			"-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(2) + "[worker 3] connected\n[worker 4] connected\n" +
				"[worker 5] connected\n" + pemBody(2) + "u1SU1Lf=\n-----END RSA PRIVATE KEY-----\nnext\n",
			m + "\n[worker 3] connected\n[worker 4] connected\n[worker 5] connected\n" + m + "\nnext\n",
		},
		"escaped in a string, its BEGIN not in the text": {
			`"` + keyLine + `\n` + keyLine + `\nu1SU1Lf=\n-----END PRIVATE KEY-----\n"}` + "\n",
			`"` + m + `\n"}` + "\n",
		},
		"blank lines between its lines, any number of them": {
			keyLine + "\n\n\n" + keyLine + "\n\n\nu1SU1Lf=\n\n\n-----END RSA PRIVATE KEY-----\nnext\n",
			m + "\nnext\n",
		},
		"its last line glued to the END": {
			pemBody(2) + "u1SU1Lf=-----END RSA PRIVATE KEY-----\n",
			m + "\n",
		},
		"a body of one line glued to its END": {
			"cat key.pem | tail -c 90\n" + keyLine + "-----END PRIVATE KEY-----\n",
			"cat key.pem | tail -c 90\n" + m + "\n",
		},
		// A line number with nothing after it reads back as a line
		// interrupting the key, one on each side of its short last line.
		"double-spaced under line numbers, its BEGIN not in the text": {
			numbered(10, keyLine, "", keyLine, "", "u1SU1Lf=", "", "-----END RSA PRIVATE KEY-----", "next"),
			"    10\u2192" + m + "\n    17\u2192next\n",
		},
	} {
		got := redact.Secrets(c.text)
		if strings.Contains(got, keyLine) || strings.Contains(got, "u1SU1Lf=") {
			t.Errorf("%s: key material survived:\n%s", name, got)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}

// A KEY READ FROM INSIDE IS READ BACK ACROSS TWO LINES. With its BEGIN not in
// the text — a stream's end, a window of a log — a key another process broke
// with two lines in a row was published above them: two lines end every
// reading of a block, and the part above had no armour of its own. Its END now
// reads back across exactly two lines to a line of the key at its encoder's
// width, wherever the two fell: between two full lines, before the short last
// line — with a line or a word below it too — before the END, with a line
// between the body's last line and the full one above it too, between an EC
// key's only two full lines, between a body of one line and its END, at
// MIME's width, and under a diff's `+` or a reader's line numbers. OpenPGP's
// checksum line is read back as its END is, so its key is read whole broken
// by two lines on either side of the checksum, and by one between the
// checksum and the body's last line. Each is one marker, and is shown so as it
// arrives.
//
// What a read does not cross is held here too: three lines in a row, two below
// a line at no encoder's width — a Go test's name, a path one character wider
// than a key's line but with no diff's `+` to make it so — two below a full
// line that lie under one at the other width, and two breaks of two lines with
// nothing but the body's last line between them: each leaves the lines above
// as they were.
//
// Mutation: start no bridge at the END, and the cases broken just before their
// END publish the lines above; none at a line that may be the body's last, and
// the one broken before its short last line does, and none after a line or a
// word below it, and those two do; let the END's bridge take no line between
// the body's last line and the line it lands on, and the case with one there
// does; read OpenPGP's checksum as a short line alone, and its cases do; leave
// MIME's width out, and its case does; cross three lines, land at any width,
// take one wider for no `+`, or chain a bridge from the END into one from the
// body's last line, and a case that should be left is redacted; land from a
// full line at the other width, and its case is; hold nothing a bridge lands
// on in [redact.Settled], and the cases are shown before their END redacts
// them.
func TestAKeyReadFromInsideIsReadBackAcrossTwoLines(t *testing.T) {
	const testName = "TestAKeyReadFromInsideIsReadBackAcrossTwoLinesWhenItsBroken"
	m := marker
	end := "-----END RSA PRIVATE KEY-----"
	w3, w4, w5, w6 := foreignLine, "[worker 4] connected", "[worker 5] connected", "[worker 6] connected"
	pgpEnd := "-----END PGP PRIVATE KEY BLOCK-----"
	lines := func(l ...string) string { return strings.Join(l, "\n") + "\n" }
	numbered := func(from int, lines ...string) string {
		var b strings.Builder
		for i, line := range lines {
			fmt.Fprintf(&b, "%6d\u2192%s\n", from+i, line)
		}
		return b.String()
	}
	for name, c := range map[string]keyForm{
		"broken between two of its full lines": {
			lines(keyLine, keyLine, w3, w4, keyLine, keyLine, "u1SU1Lf=", end, "next"), m + "\nnext\n",
		},
		"broken just before its short last line": {
			lines(keyLine, keyLine, w3, w4, "u1SU1Lf=", end, "next"), m + "\nnext\n",
		},
		"broken just before its END, its last line of forty-four": {
			lines(keyLine, digitless[:44], w3, w4, end, "next"), m + "\nnext\n",
		},
		"an EC key's two full lines, broken between them": {
			lines(keyLine, w3, w4, digitless, "u1SU1Lf=", "-----END EC PRIVATE KEY-----", "next"), m + "\nnext\n",
		},
		"a body of one line, broken from its END": {
			lines(keyLine, w3, w4, "-----END PRIVATE KEY-----", "next"), m + "\nnext\n",
		},
		"an OpenSSH key's lines, at seventy": {
			lines(sshLine, sshLine, w3, w4, sshLine, "AAAEBm9uZQ==", "-----END OPENSSH PRIVATE KEY-----", "next"), m + "\nnext\n",
		},
		"in a diff, a line wider by its +": {
			lines("+"+keyLine, "+"+keyLine, "+"+w3, "+"+w4, "+"+keyLine, "+u1SU1Lf=", "+"+end, " context"), m + "\n context\n",
		},
		"under a reader's line numbers": {
			numbered(10, keyLine, keyLine, w3, w4, keyLine, "u1SU1Lf=", end, "next"), "    10\u2192" + m + "\n    17\u2192next\n",
		},
		"broken before its short last line, and by a line below it": {
			lines(keyLine, keyLine, w3, w4, "u1SU1Lf=", w5, end, "next"), m + "\nnext\n",
		},
		"broken before its short last line, and by a word below it": {
			lines(keyLine, keyLine, w3, w4, "u1SU1Lf=", "Done", end, "next"), m + "\nnext\n",
		},
		"broken just before its END, and by a line above its short last line": {
			lines(keyLine, keyLine, w5, "u1SU1Lf=", w3, w4, end, "next"), m + "\nnext\n",
		},
		"at MIME's width": {
			lines(mimeLine, mimeLine, w3, w4, mimeLine, "u1SU1Lf=", end, "next"), m + "\nnext\n",
		},
		"OpenPGP's, broken just before its END": {
			lines(keyLine, keyLine, "u1SU1Lf=", "=nE4V", w3, w4, pgpEnd, "next"), m + "\nnext\n",
		},
		"OpenPGP's, broken just before its checksum": {
			lines(keyLine, keyLine, "u1SU1Lf=", w3, w4, "=nE4V", pgpEnd, "next"), m + "\nnext\n",
		},
		"OpenPGP's, broken by one line before its checksum": {
			lines(keyLine, keyLine, "u1SU1Lf=", w3, "=nE4V", pgpEnd, "next"), m + "\nnext\n",
		},
		"OpenPGP's, broken just before its END and by one line before its checksum": {
			lines(keyLine, keyLine, "u1SU1Lf=", w5, "=nE4V", w3, w4, pgpEnd, "next"), m + "\nnext\n",
		},
		"OpenPGP's, broken just before its checksum and by one line before its END": {
			lines(keyLine, keyLine, "u1SU1Lf=", w3, w4, "=nE4V", w5, pgpEnd, "next"), m + "\nnext\n",
		},
		"OpenPGP's, broken just before its checksum and by a word before its END": {
			lines(keyLine, keyLine, "u1SU1Lf=", w3, w4, "=nE4V", "Done", pgpEnd, "next"), m + "\nnext\n",
		},
	} {
		redactsWhole(t, name, c, keyLine, sshLine, mimeLine, digitless[:44], "u1SU1Lf=")
	}
	for name, c := range map[string]keyForm{
		"three lines in a row": {
			lines(keyLine, w3, w4, w5, keyLine, "u1SU1Lf=", end, "next"),
			lines(keyLine, w3, w4, w5, m, "next"),
		},
		"two, below a Go test's name at no encoder's width": {
			lines(testName, w3, w4, end, "next"), lines(testName, w3, w4, end, "next"),
		},
		"two, below a path one wider than a key's line, with no diff's +": {
			lines(mixedCasePath, w3, w4, end, "next"), lines(mixedCasePath, w3, w4, end, "next"),
		},
		"two below a full line, under one at the other width": {
			lines(sshLine, w3, w4, keyLine, keyLine, "u1SU1Lf=", end, "next"), lines(sshLine, w3, w4, m, "next"),
		},
		"two and two, only its short last line between them": {
			lines(keyLine, w3, w4, "u1SU1Lf=", w5, w6, end, "next"), lines(keyLine, w3, w4, "u1SU1Lf=", w5, w6, end, "next"),
		},
	} {
		redactsWhole(t, name, c)
	}
}

// digitless is a line of a key's body at PEM's width with no digit in it, as
// about one line of random bytes in fifty thousand is, and a key's last line of
// forty-odd characters far more often.
const digitless = "MIIEowIBAAKCAQEAuqSUqLfVLPHCozMxHxMoalgOEePzNmatRgeLezVaffAtagun"

// A LINE OF A KEY'S BODY WITH NO DIGIT IN IT IS READ BACK AS A LINE OF IT, a
// full line like the rest. Read back, a full line had to carry a digit, and one
// without was a line interrupting the key — which the one interruption a block
// may carry between two of its lines was meant to pay for. But printed
// double-spaced under a reader's line numbers, a grep's context marks or a
// log's timestamps, every blank line between two lines of the key is already
// that interruption, and so is a line another process wrote. The line with no
// digit made a second, the read stopped there, and every line above it was
// published — a random 1298-byte PKCS#8 body did this in CI — and where no
// other line of the body was read back, the whole of it was.
//
// Mutation: ask a line read back for a digit as well, and each of these but the
// last publishes a line of the key, two of them all of it; take a line that
// reads like a key's only as a full line, never as one interrupting it, and
// the last does.
func TestAKeysLineWithNoDigitIsReadBackAsALineOfIt(t *testing.T) {
	m := marker
	end := "-----END RSA PRIVATE KEY-----"
	numbered := func(from int, lines ...string) string {
		var b strings.Builder
		for i, line := range lines {
			fmt.Fprintf(&b, "%6d\u2192%s\n", from+i, line)
		}
		return b.String()
	}
	for name, c := range map[string]keyForm{
		"read from inside it, double-spaced under line numbers": {
			numbered(10, keyLine, "", digitless, "", keyLine, "", "u1SU1Lf=", "", end, "next"),
			"    10\u2192" + m + "\n    19\u2192next\n",
		},
		"read from inside it, double-spaced under a grep's context lines": {
			"a.pem-10-" + keyLine + "\na.pem-11-\na.pem-12-" + digitless + "\na.pem-13-\na.pem-14-" + keyLine +
				"\na.pem-15-\na.pem-16-u1SU1Lf=\na.pem-17-\na.pem-18-" + end + "\nb.pem:9:other\n",
			"a.pem-10-" + m + "\nb.pem:9:other\n",
		},
		"broken by two lines in a row, the line after them": {
			"-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(2) + "[worker 3] connected\n[worker 4] connected\n" +
				digitless + "\n" + keyLine + "\nu1SU1Lf=\n" + end + "\nnext\n",
			m + "\nnext\n",
		},
		"read from inside it, broken by one line beside it": {
			keyLine + "\n" + foreignLine + "\n" + digitless + "\n" + keyLine + "\nu1SU1Lf=\n" + end + "\nnext\n",
			m + "\nnext\n",
		},
		"read from inside it, its first line": {
			digitless + "\n" + keyLine + "\n" + keyLine + "\nu1SU1Lf=\n" + end + "\nnext\n",
			m + "\nnext\n",
		},
		// The tail before the END is already full: the line number on
		// each side of the short last line.
		"its last full line, double-spaced under line numbers": {
			numbered(10, keyLine, "", keyLine, "", digitless, "", "u1SU1Lf=", "", end, "next"),
			"    10\u2192" + m + "\n    19\u2192next\n",
		},
		// RSA's traditional encrypted body: 1232 bytes end on a line of 44.
		"its last line of forty-four, double-spaced under line numbers": {
			numbered(10, keyLine, "", keyLine, "", digitless[:44], "", end, "next"),
			"    10\u2192" + m + "\n    17\u2192next\n",
		},
		// An EC key's second line opens with twelve characters of its
		// encoding and no digit, and broken after its first line, its
		// second and its short last line are all the read back has.
		"broken by two lines after its first, an EC key's": {
			"-----BEGIN EC PRIVATE KEY-----\n" + keyLine + "\n[worker 3] connected\n[worker 4] connected\n" +
				digitless + "\nu1SU1Lf=\n-----END EC PRIVATE KEY-----\nnext\n",
			m + "\nnext\n",
		},
		// A line that reads like a key's may as well be one another
		// process wrote: here, one on each side of the short last line,
		// the second at a body's width.
		"broken on each side of its last line, once at a body's width": {
			keyLine + "\n" + foreignLine + "\nu1SU1Lf=\n" + digitless + "\n" + end + "\nnext\n",
			m + "\nnext\n",
		},
	} {
		redactsWhole(t, name, c, keyLine, digitless[:44], "u1SU1Lf=")
	}
	// What a key's line has is both cases: base64 at a body's width in one
	// case before an END is no key.
	for name, text := range map[string]string{
		"lines in upper case before an END":  strings.ToUpper(keyLine) + "\n" + strings.ToUpper(keyLine) + "\n" + end + "\n",
		"lines in lower case, double-spaced": numbered(10, strings.ToLower(keyLine), "", strings.ToLower(keyLine), "", end),
	} {
		if got := redact.Secrets(text); got != text {
			t.Errorf("%s: Secrets =\n%s\nwant it unchanged", name, got)
		}
	}
}

// mixedCasePath is a path as wide as a key's line, in upper and lower case and
// with no digit: base64's alphabet throughout, and so, read back, a line that
// reads like a key's.
const mixedCasePath = "/Users/ExampleUser/Library/Application/Support/Code/User/settings"

// AN END'S OWN LINE IS READ BOTH WAYS, and the block reaches as far back as
// either reading does. What comes before an END's armour may be the body's last
// line written up to it, or the wrapping's prefix — a diff's `+` is a line of
// base64 and a prefix both — and the line does not say which. Read as the short
// last line, the `+` spends the tail on the END's own line, so a line that
// reads like a key's written between the key's real last line and its END is
// taken for the block's one full line and the read stops just above it. Taking
// whichever reading found a body first published every line above that, which
// the other reading — the `+` as the diff's prefix — redacts.
//
// Mutation: read the END's line as the body's last line first and keep that
// reading whenever it finds a body, and each case publishes the lines of the
// key above its last.
func TestAnEndsOwnLineIsReadBothWays(t *testing.T) {
	m := marker
	end := "-----END RSA PRIVATE KEY-----"
	diff := func(lines ...string) string { return "+" + strings.Join(lines, "\n+") + "\n context\n" }
	for name, c := range map[string]keyForm{
		"a diff read from inside a key, a path in mixed case before its END": {
			diff(keyLine, keyLine, foreignLine, "u1SU1Lf=", mixedCasePath, end),
			m + "\n context\n",
		},
		"a diff read from inside a key, a line of another key's before its END": {
			diff(keyLine, keyLine, foreignLine, "u1SU1Lf=", sshLine, end),
			m + "\n context\n",
		},
		// A run of base64 before the armour, which may be the body's last
		// line or a prefix, and a line that reads like a key's above it.
		"base64 before the armour, a path in mixed case above it": {
			keyLine + "\n" + keyLine + "\n" + foreignLine + "\nDone\n" + mixedCasePath + "\nu1SU1Lf=" + end + "\nnext\n",
			m + "\nnext\n",
		},
	} {
		redactsWhole(t, name, c, keyLine)
	}
}

// A KEY ALREADY REDACTED READS AS THE ARMOUR IT REPLACED: its marker ends a
// block read forward or back where the key's own BEGIN or END line did, so a
// text holding one redacts as the text it was redacted from does — and a
// second pass over redacted text finds nothing the first left. Read as any
// other line, the marker let an END after the key read back over it, and a
// BEGIN before it read on through it, to lines no read of the text it came
// from had reached. And a BEGIN written straight after the key's END, on its
// line, holds no key once the marker stands before it: the first pass read
// the lines after that BEGIN in the wrapping the END's line gave them — the
// key's own text before its armour — and the second read them in what the
// marker left, which took a line the first had read as that wrapping's
// prefix alone for a line of a body.
//
// Mutation: drop the marker from [armoured], or from a BEGIN line's search for
// the next armour on it, or read a BEGIN line holding a marker before its
// armour, and a case here redacts more than the text it was redacted from
// does.
func TestARedactedKeyReadsAsTheArmourItReplaced(t *testing.T) {
	inner := "-----BEGIN PRIVATE KEY-----\n" + keyLine + "\n-----END PRIVATE KEY-----"
	oneLine := "-----BEGIN PRIVATE KEY----- " + keyLine + " -----END PRIVATE KEY-----"
	for name, text := range map[string]string{
		"a path in mixed case before it, an END after it": mixedCasePath + "\n" + inner + "\nu1SU1Lf=-----END RSA PRIVATE KEY-----\n",
		"a key's line before it, an END after it":         keyLine + "\n" + inner + "\nu1SU1Lf=-----END RSA PRIVATE KEY-----\n",
		"a BEGIN before it on its line":                   "-----BEGIN RSA PRIVATE KEY----- " + oneLine + "\n" + keyLine + "\n" + keyLine + "\n",
		"an END after it on its line":                     oneLine + " " + keyLine + " -----END RSA PRIVATE KEY-----\n",
		"inside a block read forward":                     "-----BEGIN RSA PRIVATE KEY-----\n" + keyLine + "\n" + inner + "\n" + keyLine + "\n" + keyLine + "\n",
		"a BEGIN written up to the END of a key read back": mixedCasePath + "-----END RSA PRIVATE KEY----------BEGIN RSA PRIVATE KEY-----\n" +
			mixedCasePath + " :\n",
		"a BEGIN written up to the END of a key read forward": "a.pem-3-x -----BEGIN RSA PRIVATE KEY-----\n" + keyLine +
			"\n-----END RSA PRIVATE KEY----------BEGIN RSA PRIVATE KEY-----\na.pem-3-" + keyLine + "\"\n",
	} {
		once := redact.Secrets(text)
		redacted := strings.Replace(strings.Replace(text, inner, marker, 1), oneLine, marker, 1)
		if got := redact.Secrets(redacted); got != once {
			t.Errorf("%s, the key redacted beforehand:\n got %q\nwant %q", name, got, once)
		}
		if twice := redact.Secrets(once); twice != once {
			t.Errorf("%s, redacted twice:\n got %q\nwant %q", name, twice, once)
		}
	}
}

// WHAT A RULE REPLACES CHANGES NO LINE A KEY IS READ ACROSS, so a second pass
// over redacted text reads no key the first did not. The rules run after the
// key blocks are read, and each case here is text whose redaction a second pass
// used to read a key in — the lines above an END that the first pass had read
// nothing back from, or the lines after a BEGIN, redacted the second time: a
// password's value taken off the next line with its line break, so two lines
// were one; one taken with an escaped break in it, past an armour or into a key
// already redacted; a token taken on into an END written straight after it;
// tokens a wrapping's prefix or an armour line's suffix held, read apart the
// first time and alike once they were one marker; a value written up to a
// BEGIN, the prefix of a wrapping the line after it repeats — read as that
// prefix alone the first time, and as a line of a body under the marker the
// second; a token between a key's line and an END on one line, which a read
// back across a cut went past to the key's line only once it was a marker; and
// a key on one line between a run at a key's width and an END, whose BEGIN's
// armour stopped that read the first time and whose marker, words, did not stop
// it the second.
//
// Mutation: make the password's rule replace its whole match again, or its
// value whole, escaped breaks and all, or let the value run on through an
// armour or a marker, or a token's into an armour; or read an armour line
// holding a marker, before its armour or after it, or a key on one line back
// across a marker: each changes its own case's second pass.
func TestARuleReplacesNothingAKeyIsReadAcross(t *testing.T) {
	escaped := func(lines ...string) string { return `{"log": "` + strings.Join(lines, `\n`) + `\n"}` + "\n" }
	for name, text := range map[string]string{
		"a password's value on the line after its key": strings.Join([]string{
			mixedCasePath, foreignLine, "  hunter2", "password:", "a.pem-3-", "xyz -----END RSA PRIVATE KEY-----",
		}, "\n") + "\n",
		"a password's value in a string, on the line after its key": escaped(
			"starting", mixedCasePath, foreignLine, "  hunter2", "password:", "a.pem-3-", "xyz -----END RSA PRIVATE KEY-----"),
		"a password's value written up to an armour": strings.Join([]string{
			"+" + keyLine, "password:", `-----BEGIN RSA PRIVATE KEY-----" sk-` + strings.Repeat("F", 24),
			"2026-10-06T09:00:00.121Z -----END RSA PRIVATE KEY-----",
		}, "\n") + "\n",
		"a password's value a key on one line": strings.Join([]string{
			"+" + digitless, "password:", "KEY=-----BEGIN RSA PRIVATE KEY----- " + keyLine + " -----END RSA PRIVATE KEY-----",
			"sk-" + strings.Repeat("C", 24) + " -----END RSA PRIVATE KEY-----",
		}, "\n") + "\n",
		"a token written up to an END": strings.Join([]string{
			"     8\u2192" + digitless, "sk-abcdefghijklmnopqrstu-----END RSA PRIVATE KEY-----",
			"2026-10-06T09:00:00.121Z -----END RSA PRIVATE KEY-----",
		}, "\n") + "\n",
		"a token the END's prefix": "[R " + keyLine + "\nsk-" + strings.Repeat("C", 24) + " -----END RSA PRIVATE KEY-----\n",
		"a token after a BEGIN":    "-----BEGIN RSA PRIVATE KEY----- sk-" + strings.Repeat("E", 24) + "\n" + keyLine + "\n",
		"a token after an END":     mixedCasePath + "\n-----END RSA PRIVATE KEY----- sk-" + strings.Repeat("G", 24) + "\n",
		"a password's value written up to a BEGIN, the next line repeating it": "pwd:\n" + keyLine +
			"-----BEGIN RSA PRIVATE KEY-----\n" + keyLine + "\"\n",
		"a token between a key's line and an END on its line": keyLine + " x ghp_" + strings.Repeat("Q7wE9", 7) + "A" +
			" xyz Done -----END RSA PRIVATE KEY-----\n",
		"a key on one line between a run at a key's width and an END": "ZZ " + keyLine + " -----BEGIN RSA PRIVATE KEY----- " +
			keyLine + " -----END RSA PRIVATE KEY----- x -----END RSA PRIVATE KEY-----\n",
	} {
		once := redact.Secrets(text)
		if twice := redact.Secrets(once); twice != once {
			t.Errorf("%s: a second pass changed\n%q\nto\n%q", name, once, twice)
		}
	}
}

// redactsWhole checks that c's text redacts to c's want, with none of pieces
// left in it, and is shown exactly so when it arrives a byte at a time and in
// pieces of 9 and 70 bytes ([redact.Settled]).
func redactsWhole(t *testing.T, name string, c keyForm, pieces ...string) {
	t.Helper()
	got := redact.Secrets(c.text)
	for _, piece := range pieces {
		if strings.Contains(got, piece) {
			t.Errorf("%s: key material survived:\n%s", name, got)
			return
		}
	}
	if got != c.want {
		t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
	}
	for _, sizes := range [][]int{{1}, {9, 70}} {
		if shown, _ := settle(c.text, sizes); shown != got {
			t.Errorf("%s in pieces of %v: shown as it settled\n%q\nwant\n%q", name, sizes, shown, got)
		}
	}
}

// keyForm is one wrapping a key reaches text in, and what the text redacts to.
type keyForm struct{ text, want string }

// wrapping is one way a key's lines reach a transcript, and what that text
// redacts to.
type wrapping struct {
	// oneLine is whether the whole block is written on one line, where no
	// other line can come between two of its own.
	oneLine bool
	wrap    func(lines []string) keyForm
}

// marker is what a private key's block redacts to.
const marker = redact.Marker + "private-key]"

// wrappings is every wrapping a key reaches a transcript in, as a function of
// its lines from armour to armour: each wraps them and the text around them,
// and redacts to one marker with that text kept.
func wrappings() map[string]wrapping {
	m := marker
	numbered := func(from int, lines []string) string {
		var b strings.Builder
		for i, line := range lines {
			fmt.Fprintf(&b, "%6d\u2192%s\n", from+i, line)
		}
		return b.String()
	}
	concatenated := func(l []string) []string {
		out := []string{"const key = \"" + l[0] + "\\n\" +"}
		for _, line := range l[1 : len(l)-1] {
			out = append(out, "\t\""+line+"\\n\" +")
		}
		return append(out, "\t\""+l[len(l)-1]+"\\n\"")
	}
	each := func(l []string, f func(i int, line string) string) string {
		var b strings.Builder
		for i, line := range l {
			b.WriteString(f(i, line))
		}
		return b.String()
	}
	return map[string]wrapping{
		"plain": {wrap: func(l []string) keyForm {
			return keyForm{strings.Join(l, "\n") + "\nnext\n", m + "\nnext\n"}
		}},
		"a reader's line numbers, across a number's width": {wrap: func(l []string) keyForm {
			return keyForm{numbered(8, append(slices.Clone(l), "next")), fmt.Sprintf("%6d\u2192%s\n%6d\u2192next\n", 8, m, 8+len(l))}
		}},
		"a grep's match and context lines": {wrap: func(l []string) keyForm {
			return keyForm{each(l, func(i int, line string) string {
				if i == 0 {
					return "a.pem:1:" + line + "\n"
				}
				return fmt.Sprintf("a.pem-%d-%s\n", i+1, line)
			}) + "b.pem:9:other\n", "a.pem:1:" + m + "\nb.pem:9:other\n"}
		}},
		"a log's timestamps, the first line with its message": {wrap: func(l []string) keyForm {
			return keyForm{each(l, func(i int, line string) string {
				if i == 0 {
					return "2026-10-06T09:00:00.120Z loaded key: " + line + "\n"
				}
				return "2026-10-06T09:00:00.121Z " + line + "\n"
			}) + "2026-10-06T09:00:01.000Z started\n",
				"2026-10-06T09:00:00.120Z loaded key: " + m + "\n2026-10-06T09:00:01.000Z started\n"}
		}},
		"a diff": {wrap: func(l []string) keyForm {
			return keyForm{each(l, func(_ int, line string) string { return "+" + line + "\n" }) + " context\n", "+" + m + "\n context\n"}
		}},
		"escaped in a JSON string": {wrap: func(l []string) keyForm {
			return keyForm{`{"private_key": "` + strings.Join(l, `\n`) + `\n", "client_email": "svc@example.com"}` + "\n",
				`{"private_key": "` + m + `\n", "client_email": "svc@example.com"}` + "\n"}
		}},
		"escaped twice": {wrap: func(l []string) keyForm {
			return keyForm{`"{\"key\": \"` + strings.Join(l, `\\n`) + `\\n\"}"` + "\n", `"{\"key\": \"` + m + `\\n\"}"` + "\n"}
		}},
		"a quoted scalar broken across lines, its breaks kept": {wrap: func(l []string) keyForm {
			return keyForm{"key: \"" + each(l, func(i int, line string) string {
				switch {
				case i == len(l)-1:
					return "  " + line + "\\n\"\n"
				case i == 0:
					return line + "\\n\n"
				}
				return "  " + line + "\\n\n"
			}) + "next: 1\n", "key: \"" + m + "\\n\"\nnext: 1\n"}
		}},
		"a string concatenated across lines": {wrap: func(l []string) keyForm {
			return keyForm{strings.Join(concatenated(l), "\n") + "\nnext\n", "const key = \"" + m + "\\n\"\nnext\n"}
		}},
		"the same, read with line numbers": {wrap: func(l []string) keyForm {
			return keyForm{numbered(12, append(concatenated(l), "next")),
				fmt.Sprintf("%6d\u2192const key = \"%s\\n\"\n%6d\u2192next\n", 12, m, 12+len(l))}
		}},
		"a line echoed into a file at a time": {wrap: func(l []string) keyForm {
			return keyForm{each(l, func(_ int, line string) string { return "echo \"" + line + "\" >> key.pem\n" }) + "chmod 600 key.pem\n",
				"echo \"" + m + "\" >> key.pem\nchmod 600 key.pem\n"}
		}},
		"a shell's trace of printf, each line quoted": {oneLine: true, wrap: func(l []string) keyForm {
			return keyForm{"+ printf '%s\\n' '" + strings.Join(l, "' '") + "'\n+ next\n", "+ printf '%s\\n' '" + m + "'\n+ next\n"}
		}},
		"on one line": {oneLine: true, wrap: func(l []string) keyForm {
			return keyForm{"KEY=" + strings.Join(l, " ") + " set\n", "KEY=" + m + " set\n"}
		}},
		"a list of its lines": {oneLine: true, wrap: func(l []string) keyForm {
			return keyForm{"lines = ['" + strings.Join(l, "', '") + "']\n", "lines = ['" + m + "']\n"}
		}},
		"its line breaks removed": {oneLine: true, wrap: func(l []string) keyForm {
			return keyForm{"KEY=" + strings.Join(l, "") + "\n", "KEY=" + m + "\n"}
		}},
	}
}

// fixtureKeys are keys' lines, armour to armour, made of filler: shapes, not
// keys. One of each layout a body has — PEM's width with a short last line,
// RFC 1421's headers before it, OpenSSH's width, a body of one line.
func fixtureKeys() map[string][]string {
	return map[string][]string{
		"RSA": {"-----BEGIN RSA PRIVATE KEY-----", keyLine, keyLine, keyLine, "u1SU1Lf=", "-----END RSA PRIVATE KEY-----"},
		"encrypted": {"-----BEGIN RSA PRIVATE KEY-----", "Proc-Type: 4,ENCRYPTED",
			"DEK-Info: AES-128-CBC,0123456789ABCDEF0123456789ABCDEF", "", keyLine, keyLine, "-----END RSA PRIVATE KEY-----"},
		"OpenSSH": {"-----BEGIN OPENSSH PRIVATE KEY-----", sshLine, sshLine, sshLine, sshLine, sshLine, "AAAEBm9uZQ==",
			"-----END OPENSSH PRIVATE KEY-----"},
		"a body of one line": {"-----BEGIN PRIVATE KEY-----", keyLine, "-----END PRIVATE KEY-----"},
		// As GnuPG writes one: no armour headers, the blank line that
		// ends them anyway, and the CRC-24 after the body.
		"OpenPGP": {"-----BEGIN PGP PRIVATE KEY BLOCK-----", "", keyLine, keyLine, keyLine, "u1SU1Lf=", "=nE4V",
			"-----END PGP PRIVATE KEY BLOCK-----"},
	}
}

// foreignLine is a line another process writes into the middle of a key.
const foreignLine = "[worker 3] connected"

// variant is a key's lines as they reach a transcript: whole, or with
// something else written into them.
type variant struct {
	// lines reads to the variant's lines from the key's.
	lines func(lines []string) []string
	// lineByLine is whether it can only happen to a key written a line at a
	// time, never to one on one line.
	lineByLine bool
}

// bodyAt is where a key's body begins in its lines: past its armour, its
// headers and its blank line.
func bodyAt(lines []string) int {
	i := 1
	for i < len(lines) && (strings.Contains(lines[i], ":") || lines[i] == "") {
		i++
	}
	return i
}

// inserted is lines with more lines put in at i.
func inserted(lines []string, i int, more ...string) []string {
	return slices.Insert(slices.Clone(lines), i, more...)
}

// variants is every way a key's lines come between its armours that a block
// still reads as ONE key, one marker: printed double-spaced, or broken by one
// line another process wrote.
func variants() map[string]variant {
	return map[string]variant{
		"whole": {lines: slices.Clone[[]string]},
		"double-spaced": {lines: func(l []string) []string {
			var out []string
			for i, line := range l {
				if i > 0 {
					out = append(out, "")
				}
				out = append(out, line)
			}
			return out
		}},
		"broken by one line": {lineByLine: true, lines: func(l []string) []string {
			// After the body's first half: a line of the body before
			// it, as a forward read needs.
			at := bodyAt(l)
			return inserted(l, at+max(1, (len(l)-1-at)/2), foreignLine)
		}},
		"broken by one line before its END": {lineByLine: true, lines: func(l []string) []string {
			return inserted(l, len(l)-1, foreignLine)
		}},
	}
}

// wrappedKeys is every fixture key, in every variant a block reads whole, in
// every wrapping: each is one marker, with the wrapping around it kept.
func wrappedKeys() map[string]keyForm {
	forms := map[string]keyForm{}
	for kname, key := range fixtureKeys() {
		for vname, v := range variants() {
			for wname, w := range wrappings() {
				if v.lineByLine && w.oneLine {
					continue
				}
				forms[kname+", "+vname+", "+wname] = w.wrap(v.lines(key))
			}
		}
	}
	return forms
}

// A KEY IS READ IN EVERY WRAPPING IT REACHES A TRANSCRIPT IN, now that an END
// closes only a block of key: a reader's line numbers, a grep's context lines,
// a log's timestamps, a diff, a JSON string escaped once or twice, a string
// concatenated across lines, a line echoed at a time, the whole key on one
// line — its encrypted headers too, with spaces for its breaks or none at all
// — and printed double-spaced, or broken by a line another process wrote. Each
// is one marker, with the wrapping around it kept.
//
// Mutation: drop the per-line prefix ([prefixOK] taking nothing but
// punctuation), and every prefixed form survives in clear; end a block at a
// blank line, and every double-spaced one does; at the first line that is not
// the key's, and every broken one does.
func TestAKeyIsReadInEveryWrapping(t *testing.T) {
	for name, form := range wrappedKeys() {
		got := redact.Secrets(form.text)
		if strings.Contains(got, keyLine) || strings.Contains(got, sshLine) || strings.Contains(got, "DEK-Info") {
			t.Errorf("%s: key material survived:\n%s", name, got)
			continue
		}
		if got != form.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, form.want)
		}
		// AND WITH NO END: the same block, cut where its END would be,
		// is read by the same structure — its body taken, its wrapping
		// left.
		cut := form.text[:strings.Index(form.text, "-----END")]
		if got := redact.Secrets(cut); strings.Contains(got, keyLine) || strings.Contains(got, sshLine) {
			t.Errorf("%s with no END: key material survived:\n%s", name, got)
		}
	}
}

// AN OPENPGP PRIVATE KEY BLOCK IS A KEY, read by the structure a PEM one is:
// armour headers, the blank line after them, base64 lines, and the checksum
// line after the body. It matched neither armour pattern, so the whole block
// — what `gpg --export-secret-keys --armor` prints — reached the record and
// the live view in clear. A mention of its armour is no key, and a public key
// block is none at all.
//
// Mutation: take OpenPGP's armour out of the patterns, and every block here
// is left whole.
func TestAnOpenPGPPrivateKeyBlockIsRedacted(t *testing.T) {
	m := redact.Marker + "private-key]"
	block := "-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: GnuPG v2\nComment: exported for the deploy\n\n" +
		pemBody(3) + "u1SU1Lf=\n=nE4V\n"
	for name, c := range map[string]keyForm{
		"closed, with armour headers": {"before\n" + block + "-----END PGP PRIVATE KEY BLOCK-----\nafter\n", "before\n" + m + "\nafter\n"},
		"its END never written":       {"before\n" + block + "gpg: signal 2 caught\n", "before\n" + m + "\ngpg: signal 2 caught\n"},
		"a mention of its armour": {"grep -n 'BEGIN PGP' notes.md\nnotes.md:4:-----BEGIN PGP PRIVATE KEY BLOCK-----\nComment: x\nsee the vault\n",
			"grep -n 'BEGIN PGP' notes.md\nnotes.md:4:-----BEGIN PGP PRIVATE KEY BLOCK-----\nComment: x\nsee the vault\n"},
		"a public key block": {"-----BEGIN PGP PUBLIC KEY BLOCK-----\n\n" + pemBody(2) + "=nE4V\n-----END PGP PUBLIC KEY BLOCK-----\n",
			"-----BEGIN PGP PUBLIC KEY BLOCK-----\n\n" + pemBody(2) + "=nE4V\n-----END PGP PUBLIC KEY BLOCK-----\n"},
	} {
		if got := redact.Secrets(c.text); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}

// BLANK LINES ARE A KEY'S, ANY NUMBER OF THEM, anywhere in its block — a key
// printed double-spaced or more, its escaped breaks doubled in a string. The
// structural reader ended a block at its first blank line after the body, and
// every body line after it was published.
//
// Mutation: end a block at a blank line after its body, and the lines after
// the first one survive.
func TestBlankLinesAnywhereInAKeyAreTheKeys(t *testing.T) {
	m := redact.Marker + "private-key]"
	for name, c := range map[string]keyForm{
		"real breaks": {
			"-----BEGIN RSA PRIVATE KEY-----\n\n\n" + keyLine + "\n\n\n" + keyLine + "\n\n\nu1SU1Lf=\n\n\n" +
				"-----END RSA PRIVATE KEY-----\nnext\n",
			m + "\nnext\n",
		},
		"escaped breaks": {
			`{"k": "-----BEGIN PRIVATE KEY-----\n\n` + keyLine + `\n\n\n` + keyLine + `\n\n-----END PRIVATE KEY-----\n"}` + "\n",
			`{"k": "` + m + `\n"}` + "\n",
		},
		"no END, a trailing blank still the key's": {
			"-----BEGIN RSA PRIVATE KEY-----\n" + keyLine + "\n\n\n" + keyLine + "\n\n",
			m + "\n\n",
		},
	} {
		if got := redact.Secrets(c.text); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}

// AN ENCRYPTED KEY ON ONE LINE is still a key: its RFC 1421 headers come
// before its body there too — with spaces for its line breaks, as a list of its
// lines, or with its breaks removed, the IV glued to the body. The one-line
// reader took the header's first word for a word of prose and the line for no
// key at all.
//
// Mutation: drop the one-line header skip, and each of these is left whole.
func TestAnEncryptedKeyOnOneLineIsRedacted(t *testing.T) {
	m := redact.Marker + "private-key]"
	lines := []string{"-----BEGIN RSA PRIVATE KEY-----", "Proc-Type: 4,ENCRYPTED",
		"DEK-Info: AES-128-CBC,0123456789ABCDEF0123456789ABCDEF", "", keyLine, keyLine, "u1SU1Lf=",
		"-----END RSA PRIVATE KEY-----"}
	for name, form := range map[string]keyForm{
		"spaces for its breaks": {"KEY=" + strings.Join(lines, " ") + " set\n", "KEY=" + m + " set\n"},
		"a list of its lines":   {"lines = ['" + strings.Join(lines, "', '") + "']\n", "lines = ['" + m + "']\n"},
		"its breaks removed":    {"KEY=" + strings.Join(lines, "") + "\n", "KEY=" + m + "\n"},
		"a shell's trace of printf": {"+ printf '%s\\n' '" + strings.Join(lines, "' '") + "'\n",
			"+ printf '%s\\n' '" + m + "'\n"},
		"no END, its breaks removed": {"KEY=" + strings.Join(lines[:len(lines)-1], ""), "KEY=" + m},
	} {
		if got := redact.Secrets(form.text); got != form.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, form.want)
		}
	}
}

// terminatedKeys is a key whose END was never written and whose body's last
// line ends in the text around it: the quote that closes the string it was
// written into, or the mark of a cut.
func terminatedKeys() map[string]keyForm {
	m := marker
	short := keyLine[:30]
	return map[string]keyForm{
		"a closing quote after a full line": {
			`{"private_key": "-----BEGIN PRIVATE KEY-----\n` + keyLine + `\n` + keyLine + `"}`,
			`{"private_key": "` + m + `"}`,
		},
		"a closing quote after a short last line": {
			`{"private_key": "-----BEGIN PRIVATE KEY-----\n` + keyLine + `\n` + keyLine + `\nabcd1234=="}`,
			`{"private_key": "` + m + `"}`,
		},
		"a closing quote on a real line": {
			"{\"private_key\": \"-----BEGIN PRIVATE KEY-----\n" + keyLine + "\n" + keyLine + "\"}",
			"{\"private_key\": \"" + m + "\"}",
		},
		"a body of one line, Ed25519's": {
			`"-----BEGIN PRIVATE KEY-----\n` + keyLine + `"`,
			`"` + m + `"`,
		},
		"a cut's mark": {
			"-----BEGIN PRIVATE KEY-----\n" + keyLine + "\n" + short + "\u2026",
			m + "\u2026",
		},
	}
}

// A KEY WITH NO END WHOSE LAST LINE ENDS IN THE TEXT AROUND IT is still a key.
// Its body line counted only when the WHOLE line was base64, so the line that
// carried the closing quote of the string the key was written into — or the
// mark of a cut — was left in clear, and a body of one line (an Ed25519 key's
// whole body) was not redacted at all.
//
// Mutation: read a body line as all-or-nothing again, and every one of these
// shows a line of the key.
func TestAKeyWhoseLastLineEndsInTheTextAroundItIsRedacted(t *testing.T) {
	for name, form := range terminatedKeys() {
		got := redact.Secrets(form.text)
		if strings.Contains(got, keyLine) || strings.Contains(got, keyLine[:30]) {
			t.Errorf("%s: key material survived: %q", name, got)
			continue
		}
		if got != form.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, form.want)
		}
	}
}

// A KEY IS REDACTED WHEREVER A CUT LANDS in it: at the end of a line, the head
// a tool kept before its mark; at the start of one, the tail a window kept
// after its mark — the window's first line, the body's last or the line before
// it, OpenPGP's above its checksum; and in the middle, a head and a tail with
// the mark between them or on a line of its own, with the END below it, with
// no END and with no BEGIN, and between the BEGIN's armour and its line break;
// on every line, as a viewer cuts each at its width or keeps each one's end,
// to a full line or narrower, before or after a mark in letters, a bare
// ellipsis or none; beside two lines another process wrote; and on a key
// written on one line. Each mark is one a tool writes and none is listed
// anywhere: what is read is the piece of the key beside it, and the line's text
// at its other end that reads like a key's.
//
// What a cut leaves that reads like no key's is left as it is: a piece with no
// digit, a piece shorter than eight characters, a piece between two marks, the
// first word of the prose after a key with no END, the word above a key read
// from inside, a blank line of a wrapping whose prefix reads like a piece, a
// key on one line between two cuts, read back no further than the second, or
// across a cut to prose or another block's armour — and a line of prose beside
// a key that holds a run reading like a piece of one anywhere but at an end of
// the line, since a cut leaves its piece at one. Prose whose lines each open
// in such a run, straight above an END, is redacted with it as a key cut on
// every line: what reading a line a cut wrote into as a full one costs.
//
// Mutations: tell a line a cut wrote into by its head alone, or by its tail
// alone, or read it back as no full line; end its key's text at its first or
// its last piece; read no cut between the BEGIN's armour and its line break;
// take no short piece forward as the body's first line, or before text on its
// line, or after another; read a short piece back as no full line, or a line a
// cut wrote into, or a short piece, back at the width of its run; start no
// bridge from a line whose width a cut hid, or land none on one; read a blank
// line of a wrapping by its prefix's run; read a key on one line back across a
// cut only to a run as wide as an encoder writes one, or forward or back past
// no OpenPGP checksum, or its short last run alone as nothing; ask a piece for
// seven characters or nine: each fails a case.
func TestAKeyIsRedactedWhereverACutLands(t *testing.T) {
	m := marker
	const testFailed = "--- FAIL: TestRSA2048Sign (0.01s)"
	begin, end := "-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----"
	lines := func(l ...string) string { return strings.Join(l, "\n") + "\n" }
	// rot is the i-th line of a key: keyLine turned i characters, so no
	// two lines of a case share a run that one cut could keep of another.
	rot := func(i int) string { return keyLine[i:] + keyLine[:i] }
	every := func(n int, line func(i int) string) []string {
		var l []string
		for i := 1; i <= n; i++ {
			l = append(l, line(i))
		}
		return l
	}
	head, tail, narrow := keyLine[:30], keyLine[40:], keyLine[:24]
	middle := keyLine[:20] + "…[… 2,345 characters truncated …]…" + keyLine[44:]
	numbered := func(from int, lines ...string) string {
		var b strings.Builder
		for i, line := range lines {
			fmt.Fprintf(&b, "%6d→%s\n", from+i, line)
		}
		return b.String()
	}
	for name, c := range map[string]keyForm{
		// The head kept, nothing after it.
		"a full line, then a mark in letters": {
			lines(begin, keyLine, keyLine+"[...truncated]", "$ git status"), m + "[...truncated]\n$ git status\n",
		},
		"half a line, then a mark spaced from it": {
			lines(begin, keyLine, head+" (truncated)"), m + " (truncated)\n",
		},
		"the body's first line cut short": {
			lines(begin, head+"<truncated>"), m + "<truncated>\n",
		},
		"the same with no mark": {
			lines(begin, head, "$ git status"), m + "\n$ git status\n",
		},
		"under a reader's line numbers": {
			numbered(10, begin, keyLine, head+"…[1234 more bytes]"), "    10→" + m + "…[1234 more bytes]\n",
		},
		"escaped in a JSON string": {
			`{"private_key": "` + begin + `\n` + keyLine + `\n` + head + `[...truncated]` + "\n",
			`{"private_key": "` + m + `[...truncated]` + "\n",
		},
		"a piece of eight characters": {
			lines(begin, keyLine, "u1SU1LfV(truncated)"), m + "(truncated)\n",
		},
		// The tail kept: a window's first line.
		"a window opened inside a line": {
			lines("[… 812 bytes truncated]"+keyLine[34:], keyLine, "u1SU1Lf=", end, "next"), "[… 812 bytes truncated]" + m + "\nnext\n",
		},
		"a window opened on the body's last line": {
			lines("<truncated>u1SU1Lf9x=", end, "next"), "<truncated>" + m + "\nnext\n",
		},
		"a window opened on the line before it": {
			lines("… (5 KiB more)"+tail, "u1SU1Lf=", end), "… (5 KiB more)" + m + "\n",
		},
		"a window opened with no mark": {
			lines("$ tail -c 120 key.pem", tail, "u1SU1Lf=", end), "$ tail -c 120 key.pem\n" + m + "\n",
		},
		"a window's first line, then a line another process wrote": {
			lines("[truncated]"+tail, foreignLine, end), "[truncated]" + m + "\n",
		},
		"OpenPGP's, opened on the line above its checksum": {
			lines("[truncated]"+keyLine[50:], "=nE4V", "-----END PGP PRIVATE KEY BLOCK-----"), "[truncated]" + m + "\n",
		},
		// A head and a tail with the mark between them.
		"a mark between a head and a tail": {
			lines(begin, keyLine, middle, keyLine, "u1SU1Lf=", end, "next"), m + "\nnext\n",
		},
		"the same, read from inside": {
			lines(keyLine, middle, keyLine, "u1SU1Lf=", end, "next"), m + "\nnext\n",
		},
		"the same, with no END": {
			lines(begin, keyLine, middle, keyLine, "u1SU1Lf="), m + "\n",
		},
		"the same, the mark's line the last": {
			lines(begin, keyLine, middle), m + "\n",
		},
		"the same, its head with no digit": {
			lines(begin, keyLine, digitless[:20]+"…[… 2,345 characters truncated …]…"+keyLine[44:]), m + "\n",
		},
		"the same, its tail with no digit": {
			lines(begin, keyLine, head+"[… 2,345 characters truncated …]"+digitless[30:], "$ git status"), m + "\n$ git status\n",
		},
		"an RSA key's first line, its head the encoding's, read from inside": {
			lines("MIIEogIBAAKCAQEA…"+tail, keyLine, "u1SU1Lf=", end), m + "\n",
		},
		"the mark on a line of its own between two pieces": {
			lines(begin, keyLine, head, "[... 42 lines truncated ...]", tail, "u1SU1Lf=", end), m + "\n",
		},
		"a mark on a line of its own, read from inside": {
			lines(rot(1), rot(2), rot(3)[:30], "... [42 lines truncated] ...", rot(6)[34:], rot(7), "u1SU1Lf=", end, "next"),
			m + "\nnext\n",
		},
		"a cut between the BEGIN's armour and its line break": {
			"-----BEGIN EC PRIVATE KEY-----[... 2,345 characters truncated ...]" + mimeLine[40:] + "==\n-----END EC PRIVATE KEY-----\nnext\n",
			m + "\nnext\n",
		},
		"the same in a JSON string, inside its escaped break": {
			`{"private_key": "-----BEGIN PRIVATE KEY-----\\…[… 1,500 characters truncated …]…` + mimeLine[40:] +
				`==\n-----END PRIVATE KEY-----\n", "x": 1}` + "\n",
			`{"private_key": "` + m + `\n", "x": 1}` + "\n",
		},
		// Every line cut at a viewer's width, or each one's end kept.
		"every line cut at fifty, with no END": {
			lines(begin, keyLine[:50]+"…", keyLine[:50]+"…", keyLine[:50]+"…", "u1SU1Lf="), m + "\n",
		},
		"every line cut at fifty, read from inside": {
			lines(keyLine[:50]+"[...truncated]", keyLine[:50]+"[...truncated]", "u1SU1Lf=", end), m + "\n",
		},
		"every line cut at thirty, a window on the last of them": {
			lines("$ tail -n 2 key.pem", head+"[...truncated]", end), "$ tail -n 2 key.pem\n" + m + "\n",
		},
		"every line cut at twenty-four, read from inside": {
			lines(narrow+"[...truncated]", narrow+"[...truncated]", narrow+"[...truncated]", narrow+"[...truncated]", "u1SU1Lf=", end),
			m + "\n",
		},
		"every line cut at twenty-four before an ellipsis, with no END": {
			lines(begin, narrow+"…", narrow+"…", narrow+"…", narrow+"…", "$ git status"), m + "…\n$ git status\n",
		},
		"every line cut at twenty-four before an ellipsis, read from inside": {
			lines(narrow+"…", narrow+"…", narrow+"…", narrow+"…", "u1SU1Lf=", end), m + "\n",
		},
		"every line cut at twenty-four with no mark, below a BEGIN": {
			lines(begin, narrow, narrow, narrow, narrow, narrow, "$ git status"), m + "\n$ git status\n",
		},
		"every line cut at twenty-four with no mark, read from inside": {
			lines(narrow, narrow, narrow, narrow, narrow, "u1SU1Lf=", end), m + "\n",
		},
		"every line's start cut behind a mark, read from inside": {
			lines(append(every(6, func(i int) string { return "[...truncated]" + rot(i)[24:] }), "u1SU1Lf=", end, "next")...),
			"[...truncated]" + m + "\nnext\n",
		},
		"every line's start cut behind an ellipsis, with no END": {
			lines(append(append([]string{begin}, every(8, func(i int) string { return "…" + rot(i)[34:] })...), "$ git status")...),
			m + "\n$ git status\n",
		},
		"every line's start cut behind an ellipsis, read from inside": {
			lines(append(every(8, func(i int) string { return "…" + rot(i)[34:] }), "u1SU1Lf=", end, "next")...),
			"…" + m + "\nnext\n",
		},
		// Beside two lines another process wrote.
		"two lines another process wrote above a line cut short, read from inside": {
			lines(rot(1), rot(2), rot(3), foreignLine, "[worker 4] connected", rot(4)[:30]+"[...truncated]", rot(5), rot(6), "u1SU1Lf=", end, "next"),
			m + "\nnext\n",
		},
		"the same below it": {
			lines(rot(1), rot(2), rot(3)[:30]+"[...truncated]", foreignLine, "[worker 4] connected", rot(4), rot(5), "u1SU1Lf=", end, "next"),
			m + "\nnext\n",
		},
		"the same below a full line cut, read from inside": {
			lines(keyLine+"[...truncated]", foreignLine, "[worker 4] connected", keyLine, "u1SU1Lf=", end, "next"), m + "\nnext\n",
		},
		"the same below lines cut short with no mark, read from inside": {
			lines(rot(1)[:24], rot(2)[:24], rot(3)[:24], foreignLine, "[worker 4] connected", rot(4), rot(5), "u1SU1Lf=", end, "next"),
			m + "\nnext\n",
		},
		// A key on one line.
		"a key on one line, a mark between its head and its tail": {
			"KEY=" + begin + " " + keyLine + " " + head + "[...truncated]" + tail + " " + keyLine + " u1SU1Lf= " + end + " set\n",
			"KEY=" + m + " set\n",
		},
		"a key on one line, a mark between its head and its tail, with no END": {
			"KEY=" + begin + " " + keyLine + " " + head + "[...truncated]" + tail + " " + keyLine + " u1SU1Lf= set\n",
			"KEY=" + m + " set\n",
		},
		"the same, its last run short": {
			"KEY=" + begin + " " + keyLine + " " + head + "[...truncated]" + tail + " " + keyLine + " AbCdEf== set\n",
			"KEY=" + m + " set\n",
		},
		"the same, OpenPGP's checksum after it": {
			"KEY=-----BEGIN PGP PRIVATE KEY BLOCK----- " + keyLine + " " + head + "[...truncated]" + tail + " " + keyLine + " AbCdEf== =nE4V set\n",
			"KEY=" + m + " set\n",
		},
		"a key on one line read from inside, across its cut to a run narrower than a line": {
			"…" + keyLine[14:] + "[...truncated]" + keyLine + " u1SU1Lf= " + end + "\n", "…" + m + "\n",
		},
		"a key on one line read from inside, a mark between its head and its tail": {
			"…" + tail + " " + keyLine + " " + head + "[...truncated]" + tail + " u1SU1Lf= " + end + "\n", "…" + m + "\n",
		},
		"the same, OpenPGP's checksum before its END": {
			"…" + tail + " " + keyLine + " " + head + "[...truncated]" + tail + " " + keyLine + " u1SU1Lf= =nE4V -----END PGP PRIVATE KEY BLOCK-----\n",
			"…" + m + "\n",
		},
		"a key on one line, read from inside": {
			"…" + tail + " " + keyLine + " u1SU1Lf= " + end + " set\n", "…" + m + " set\n",
		},
		"a key on one line cut short at its first run": {
			"KEY=" + begin + " " + head + "[...truncated]\n", "KEY=" + m + "[...truncated]\n",
		},
		"a body's last run alone, written up to its END": {
			"key: u1SU1Lf9x=" + end + "\n", "key: " + m + "\n",
		},
		// What is across a cut from the tail a window kept is read only
		// to a run at a full line's width: a word with a digit in it is
		// what a piece reads like as well.
		"a window on one line, a word with a digit across its cut": {
			"see TestRSA2048Sign for it [… 812 bytes truncated]" + tail + " u1SU1Lf= " + end + "\n",
			"see TestRSA2048Sign for it [… 812 bytes truncated]" + m + "\n",
		},
	} {
		redactsWhole(t, name, c, keyLine[:24], head, tail, keyLine[34:], keyLine[44:], keyLine[50:], "u1SU1Lf9x=", "u1SU1LfV",
			"MIIEogIBAAKCAQEA", digitless[30:54], mimeLine[40:64], rot(1)[24:48], rot(3)[:24], rot(6)[34:58], "AbCdEf==", "nE4V")
	}
	for name, c := range map[string]keyForm{
		"a piece with no digit": {
			lines(begin, keyLine, digitless[:30]+"[...truncated]"), m + "\n" + digitless[:30] + "[...truncated]\n",
		},
		"a piece of seven characters": {
			lines(begin, keyLine, keyLine[16:23]+"(truncated)"), m + "\n" + keyLine[16:23] + "(truncated)\n",
		},
		"a piece between two marks": {
			lines(begin, "MI…"+keyLine[20:44]+"[truncated]", rot(5), rot(6), "$ ls"),
			lines(begin, "MI…"+keyLine[20:44]+"[truncated]", rot(5), rot(6), "$ ls"),
		},
		"the prose after a key with no END": {
			lines(begin, keyLine, "u1SU1Lf=", "Traceback (most recent call last):", `  File "x.py", line 3`),
			m + "\nTraceback (most recent call last):\n  File \"x.py\", line 3\n",
		},
		"a word above a key read from inside": {
			lines("Done in 3s", keyLine, "u1SU1Lf=", end), "Done in 3s\n" + m + "\n",
		},
		"a blank line in a wrapping whose prefix reads like a piece, above a bare END": {
			lines("[Worker1A] some prose", "[Worker1A]", "[Worker1A] "+end),
			// The END's own gutter is read as the piece a cut kept
			// before it: what reading the END's line alone costs.
			lines("[Worker1A] some prose", "[Worker1A]", "["+m),
		},
		// Prose holding a run that reads like a piece of a key, at neither
		// end of its line: a test's name with a digit.
		"a line of prose holding a piece, after a key with no END": {
			lines(begin, keyLine, "u1SU1Lf=", testFailed), m + "\n" + testFailed + "\n",
		},
		"the same between two armours": {
			lines(begin, testFailed, end), lines(begin, testFailed, end),
		},
		"the same above a bare END": {
			lines("see TestRSA2048Sign for the case", end), lines("see TestRSA2048Sign for the case", end),
		},
		// What reading a line a cut wrote into as a full line costs: prose
		// whose lines each open in such a run, straight above an END, is
		// a key cut on every line as far up as it goes.
		"lines of prose that each open in a piece, above a bare END": {
			lines("$ make test", "Alice42Smith logged in", "Ab3f9C2d7E1b pulled", "TestRSA2048Sign passed", end), "$ make test\n" + m + "\n",
		},
		"a key on one line between two cuts": {
			"…" + tail + " " + keyLine + " " + head + "[...truncated]" + tail + " " + keyLine + " " + head + "[...truncated]" +
				tail + " u1SU1Lf= " + end + "\n",
			"…" + tail + " " + keyLine + " " + head + "[...truncated]" + m + "\n",
		},
		"a certificate's END across a cut from a key's tail on one line": {
			keyLine + " -----END CERTIFICATE----- [… 812 bytes truncated]" + tail + " u1SU1Lf= " + end + "\n",
			keyLine + " -----END CERTIFICATE----- [… 812 bytes truncated]" + m + "\n",
		},
		"a key on one line, a piece of prose across its cut": {
			"KEY=" + begin + " " + keyLine + "[...truncated] see TestRSA2048Sign for it\n",
			// The mark's word is read as the body's short last run.
			"KEY=" + m + "] see TestRSA2048Sign for it\n",
		},
	} {
		redactsWhole(t, name, c)
	}
}

// WHAT ANOTHER RULE REPLACES IS NO PIECE OF A KEY: a password's key, its
// separator and its value, and a token, beside a key a cut was read in. Each
// holds runs that read like a piece of a key — `s3Password`,
// `Password=hunter2`, the `Ab12…` after a token's `_` — and taken as one, the
// key's span ended or began inside the password or the token: its marker took
// the key word or the token's head, and what was left matched no rule, so the
// value and the token's other characters were published.
//
// Mutations: judge a piece, a full line's run, the key's text at a cut line's
// other end or a run across a cut on one line by its run alone, and a case
// publishes the value or the token's tail; count only a claim the run lies
// inside, and the run that holds a password's key is a piece again; claim a
// password's key and separator only with a value after them, and the key a pass
// left beside its value's marker is read as a piece the second time; read a
// password's value on past a token, or a password's key inside a token as one,
// and the piece of the key after the token is published; tell a line a cut
// wrote into by its tail alone, and the head a cut kept before a password or a
// token is published.
func TestARunAnotherRuleClaimsIsNoPieceOfAKey(t *testing.T) {
	m, pw, api := marker, redact.Marker+"password]", redact.Marker+"api-key]"
	begin, end := "-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----"
	noEnd := begin + "\n" + keyLine + "\n" + keyLine + "\n"
	// wide is a value as wide as a key's line, in both cases with digits.
	const wide = "Ab12cdEF34ghIJKLmnopQRSTuvwxYZabCDefGHijKLmn"
	for name, c := range map[string]keyForm{
		"a password's key holding a digit, under a key with no END": {
			noEnd + "s3Password: hunter2-Secret!\n", m + "\ns3Password: " + pw + "\n",
		},
		"its value on the next line": {
			noEnd + "s3Password:\nhunter2 is not it\n", m + "\ns3Password:\n" + pw + " is not it\n",
		},
		"the same straight after a BEGIN": {
			begin + "\ns3Password: hunter2-Secret!\n", begin + "\ns3Password: " + pw + "\n",
		},
		"a value holding a piece": {
			noEnd + "password: Summer2024!\n", m + "\npassword: " + pw + "\n",
		},
		"a key and its value in one run": {
			noEnd + "user1Password=hunter-two\n", m + "\nuser1Password=" + pw + "\n",
		},
		"a value on the next line opening in a piece": {
			noEnd + "password:\nKk55Lm66NN-secretpart\n", m + "\npassword:\n" + pw + "\n",
		},
		"a password on the BEGIN's line": {
			begin + " Password=hunter2-Secret! set\n", begin + " Password=" + pw + " set\n",
		},
		"a token after a piece, under a key with no END": {
			begin + "\n" + keyLine + "\nAb12cdEF34gh sk-proj-Xy98ZzWw76Vv_abcdefghijklmnop\n", m + " " + api + "\n",
		},
		"a token above an END": {
			"OPENAI_API_KEY=sk-abcdefghij_Ab12CdEf34GhIjKlMnOpQr\n" + end + "\n", "OPENAI_API_KEY=" + api + "\n" + end + "\n",
		},
		"a token above a key read from inside": {
			"[... truncated]\nOPENAI_API_KEY=sk-proj-Ab1_xY-Q7wE9Q7wE9Q7wE9Q7wE9Q7wE9\n" + keyLine + "\n" + keyLine + "\nu1SU1Lf=\n" + end + "\n",
			"[... truncated]\nOPENAI_API_KEY=" + api + "\n" + m + "\n",
		},
		"a token before an END on its line": {
			"sk-ant-api03-Ab12Cd34Ef56Gh78_ijklmnopq " + end + "\n", api + " " + end + "\n",
		},
		"a password's value as wide as a key's line, before an END on its line": {
			"password=" + wide + " " + end + "\n", "password=" + pw + " " + end + "\n",
		},
		"a token as wide as one": {
			"sk-proj-" + wide + " " + end + "\n", api + " " + end + "\n",
		},
		"a password after the mark on a line a cut wrote into, under a key with no END": {
			noEnd + keyLine[:30] + "[...truncated] db_password=SuperSecretValue\n", m + "[...truncated] db_password=" + pw + "\n",
		},
		"a key on one line, a password across its cut": {
			"KEY=" + begin + " " + keyLine + "[...truncated] password=" + wide + " set\n",
			"KEY=" + m + "] password=" + pw + " set\n",
		},
		"a token that ends in a password's key, before an END": {
			"sk-AAAAAAAAAAAAAAAAAAAApwd: Kk55Lm66NN " + end + "\n", api + ": " + m + "\n",
		},
		"in a string, a password's value running on to a token and a piece before an END": {
			`{"log": "pwd=\nOPENAI_API_KEY=sk-abcdefghij_Ab12CdEf34GhIjKlMnOpQr\n…u1SU1Lf9x= ` + end + `\n"}` + "\n",
			`{"log": "pwd=\n` + pw + api + `\n…` + m + `\n"}` + "\n",
		},
	} {
		redactsWhole(t, name, c, "hunter2", "Secret", "Summer2024", "-two", "secretpart", "abcdefghij", "Ab12CdEf34", "Xy98ZzWw", "ijklmnopq",
			"u1SU1Lf9x", wide[:20], "SuperSecret", "Kk55Lm66NN")
		if twice := redact.Secrets(c.want); twice != c.want {
			t.Errorf("%s: a second pass changed\n%q\nto\n%q", name, c.want, twice)
		}
	}
}

// A PASSWORD'S KEY UNDER A KEY WITH NO END IS NO LINE OF IT. Read as the body's
// last line written up to the text around it, `Password:` gave the key's block
// its word and left the `:` outside it, so the value on the next line was
// published — under a key whose END was never written, which is what a run
// that crashed mid-print leaves, and in every wrapping a key is read in. Read
// as a line interrupting the key, the label and its value are redacted with
// it when the value can be its last line, and left to the password's rule
// when it cannot. So is a key whose separator is an `=` ending its line,
// `pwd=` and `PASSWORD=`, which was read as base64's padding: the line was the
// key's last, and the value on the next one published. And a token is no line
// of the key either, so it is redacted as the token it is. What that costs is
// a key's last line that ends in a password's key and `=`, when no END
// follows it: it is published, and what follows it on its line is taken for
// the password.
//
// Mutation: read no rule's match as keeping a run off the body, and every
// value is published; read a password's key only where its separator is past
// the run, and the values after `pwd=` and `PASSWORD=` are; name no token, and
// the token is taken into the key's marker.
func TestAPasswordsKeyUnderAKeyWithNoEndIsNoLineOfIt(t *testing.T) {
	m := marker
	begin := "-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(2)
	for name, form := range map[string]keyForm{
		"after the body's full lines": {
			begin + "Password:\nhunter2\n",
			m + "\n",
		},
		"after its short last line": {
			begin + "u1SU1Lf=\nPassword:\nhunter2\n",
			m + "\nPassword:\n" + redact.Marker + "password]\n",
		},
		"spaced from its separator": {
			begin + "Password :\nhunter2\n",
			m + "\n",
		},
		"indented, in capitals": {
			begin + "  PASSWD:\n  hunter2\n",
			m + "\n",
		},
		"its value after a blank line": {
			begin + "Password:\n\nhunter2\n",
			m + "\n",
		},
		"its value no line of the key": {
			begin + "Password:\nhunter2 is not it\n",
			m + "\nPassword:\n" + redact.Marker + "password] is not it\n",
		},
		"under a reader's line numbers": {
			"12:-----BEGIN RSA PRIVATE KEY-----\n13:" + keyLine + "\n14:" + keyLine + "\n15:Password:\n16:hunter2\n",
			"12:" + m + "\n",
		},
		"in a string, its breaks escaped": {
			`"-----BEGIN RSA PRIVATE KEY-----\n` + keyLine + `\n` + keyLine + `\nPassword:\nhunter2\n"`,
			`"` + m + `\n"`,
		},
		"a second key in the run": {
			begin + "u1pwd=Xpassword:\nhunter2\n",
			m + "\n",
		},
		"its key ending its line in =": {
			begin + "pwd=\nhunter2 is it\n",
			m + "\npwd=\n" + redact.Marker + "password] is it\n",
		},
		"the same in capitals, its value indented": {
			begin + "PASSWORD=\n  hunter2-x y\n",
			m + "\nPASSWORD=\n  " + redact.Marker + "password] y\n",
		},
		"a token where its last line would be": {
			begin + "AKIAABCDEFGHIJKLMNOP\nnext line\n",
			m + "\n" + redact.Marker + "aws-key]\nnext line\n",
		},
	} {
		got := redact.Secrets(form.text)
		if strings.Contains(got, "hunter2") || strings.Contains(got, "u1") {
			t.Errorf("%s: a secret survived: %q", name, got)
			continue
		}
		if got != form.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, form.want)
		}
	}
	text := begin + "u1SU1Lpwd= \u2026"
	if want := m + "\nu1SU1Lpwd= " + redact.Marker + "password]"; redact.Secrets(text) != want {
		t.Errorf("a last line ending in pwd=, then a cut's mark:\n got %q\nwant %q", redact.Secrets(text), want)
	}
}

// oneLineKey is a whole RSA key written on one line, its breaks turned to
// spaces — one of the ways two keys end up flattened onto one line (a shell
// `echo $A $B`, an env dump with the newlines stripped).
func oneLineKey() string {
	return "-----BEGIN RSA PRIVATE KEY----- " + keyLine + " " + keyLine + " u1SU1Lf= -----END RSA PRIVATE KEY-----"
}

// flatKey is the same with no separator at all between the armours and the
// body — the newlines removed rather than spaced.
func flatKey() string {
	return "-----BEGIN RSA PRIVATE KEY-----" + keyLine + keyLine + "u1SU1Lf=-----END RSA PRIVATE KEY-----"
}

// keyFixtures are the texts the idempotence and the settling cases run over:
// every private-key shape above, beside the shapes that are not keys.
func keyFixtures() map[string]string {
	fixtures := map[string]string{
		"closed key": "start\n-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(8) +
			"-----END RSA PRIVATE KEY-----\ndone\n",
		"unclosed key": "start\n-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(8) + "abc=\npanic\n",
		"prefixed key": "1→-----BEGIN RSA PRIVATE KEY-----\n2→" + keyLine + "\n3→" + keyLine +
			"\n4→-----END RSA PRIVATE KEY-----\n5→done\n",
		"encrypted key": "-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,00FF\n\n" +
			pemBody(3) + "-----END RSA PRIVATE KEY-----\n",
		"two keys": "-----BEGIN EC PRIVATE KEY-----\n" + pemBody(2) + "-----END EC PRIVATE KEY-----\nbetween\n" +
			"-----BEGIN EC PRIVATE KEY-----\n" + pemBody(2) + "-----END EC PRIVATE KEY-----\n",
		"two keys flattened onto one line, spaces for breaks": "KEYS=" + oneLineKey() + " " + oneLineKey() + "\n",
		"two keys flattened onto one line, breaks removed":    "KEYS=" + flatKey() + flatKey() + "\n",
		"two keys echoed on one line":                         "+ echo " + oneLineKey() + " " + oneLineKey() + "\n",
		"a one-line key, then a bare BEGIN mention": "KEY=" + oneLineKey() +
			" # -----BEGIN PRIVATE KEY----- format\n",
		"a one-line key with no END, then a certificate": "A=-----BEGIN RSA PRIVATE KEY----- " + keyLine + " " + keyLine +
			" u1SU1Lf= -----BEGIN CERTIFICATE----- MIIB\n",
		"a one-line key with no END, then a whole key": "A=-----BEGIN RSA PRIVATE KEY----- " + keyLine + " " + keyLine +
			" u1SU1Lf= " + oneLineKey() + "\n",
		"a one-line key whose END follows two words": "A=-----BEGIN RSA PRIVATE KEY----- " + keyLine + " " + keyLine +
			" u1SU1Lf= two words -----END RSA PRIVATE KEY-----\n",
		"grep":           "a:-----BEGIN RSA PRIVATE KEY-----\nb: nothing here\nc: more\n",
		"password":       "login\npassword:\n  hunter2\nnext\n",
		"password later": "Enter password\n\n= swordfish\nok\n",
		// A key whose string goes on to the next line after escaped breaks:
		// a quoted scalar broken across lines, its breaks kept, and a JSON
		// string continued onto the next line.
		"password in a string broken after its key":         "key: \"password:\\n\n  hunter2\\n\"\nnext: 1\n",
		"password in a string broken after an escaped CRLF": "{\"log\": \"password:\\r\\n\n hunter2\"}\n",
		"tokens": "token sk-" + strings.Repeat("q", 40) + "\nAKIA" + strings.Repeat("Q", 16) + "\n",
		"key after a password": "password:\n-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(2) +
			"-----END RSA PRIVATE KEY-----\ntail\n",
		"grep of both armours": grepOfBothArmours,
		"a body, then the run going on": "-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(3) +
			"the run went on\nand on\nand on\nand on\ndone\n",
		// The line before the END is key-shaped, but not in the END's
		// wrapping: it is a line interrupting the key, and the three lines
		// before the full ones are the tail's, not the body's one.
		"a key-shaped line its END's wrapping reads as an interruption": "echo \"" + keyLine + "\" >> key.pem\n" +
			"echo \"" + keyLine + "\" >> key.pem\nthe run printed this\nDone\na.pem-2-" + keyLine + "\n" +
			"echo \"-----END RSA PRIVATE KEY-----\" >> key.pem\nnext\n",
	}
	for name, form := range wrappedKeys() {
		fixtures["wrapped: "+name] = form.text
	}
	for name, form := range terminatedKeys() {
		fixtures["terminated: "+name] = form.text + "\n"
	}
	return fixtures
}

// IDEMPOTENT over every key shape too: a marker matches nothing, so a second
// pass changes nothing and finds nothing.
func TestRedactingAKeyTwiceIsRedactingItOnce(t *testing.T) {
	for name, text := range keyFixtures() {
		once := redact.Secrets(text)
		if twice := redact.Secrets(once); twice != once {
			t.Errorf("%s: a second pass changed the text:\n once: %q\ntwice: %q", name, once, twice)
		}
		if redact.Contains(once) {
			t.Errorf("%s: the redacted text still holds a shape: %q", name, once)
		}
	}
}

// settle feeds text to a reader the way a live view receives it — in pieces of
// the given sizes — and shows each piece's settled part as it lands, and the
// rest once the text is finished. It returns everything shown, and what had
// been shown after each piece.
//
// A PIECE WITH NO LINE BREAK, landing on text none of which settled, is not
// asked about again. Settled reads a text only up to its last line break, so
// its answer is the 0 it just gave, and redacting nothing shows nothing — the
// step it records is the one a reader asking would have seen. Asking anyway
// re-read a held key's whole text once per byte of a one-byte schedule, which
// made that schedule quadratic in the key's length and the slowest thing in
// this suite. It is exact only for a Settled that keeps its promise to ignore
// an unfinished line, so [TestAnUnfinishedLineChangesNothingSettled] holds it
// to that promise directly, over every fixture.
func settle(text string, sizes []int) (string, []string) {
	var shown strings.Builder
	var steps []string
	pending := ""
	// held is whether the last answer settled none of pending.
	held := false
	at, i := 0, 0
	for at < len(text) {
		n := min(sizes[i%len(sizes)], len(text)-at)
		i++
		piece := text[at : at+n]
		pending += piece
		at += n
		if !held || strings.Contains(piece, "\n") {
			cut := redact.Settled(pending)
			shown.WriteString(redact.Secrets(pending[:cut]))
			pending = pending[cut:]
			held = cut == 0
		}
		steps = append(steps, shown.String())
	}
	shown.WriteString(redact.Secrets(pending))
	return shown.String(), steps
}

// AN UNFINISHED LINE CHANGES NOTHING SETTLED: what Settled answers for a text
// is what it answers for that text up to its last line break, however much of
// the next line has been written after it.
//
// [settle] relies on it to stop asking while a line is still arriving, so the
// one-byte schedules no longer probe it at every byte; it is held here
// instead, at every line break of every fixture, with the next line begun,
// half written, and written all but its break.
//
// Mutation: let Settled read past its last line break, and a key's armour
// half written on the next line moves the answer.
func TestAnUnfinishedLineChangesNothingSettled(t *testing.T) {
	t.Parallel()
	for name, text := range keyFixtures() {
		for at := 0; at < len(text); {
			next := strings.IndexByte(text[at:], '\n')
			if next < 0 {
				next = len(text) - at
			}
			line := text[at : at+next]
			want := redact.Settled(text[:at])
			for _, partial := range []string{line[:min(1, len(line))], line[:len(line)/2], line} {
				if got := redact.Settled(text[:at] + partial); got != want {
					t.Errorf("%s: %d bytes of the line after byte %d moved Settled from %d to %d: %q",
						name, len(partial), at, want, got, partial)
				}
			}
			at += next + 1
		}
	}
}

// WHAT IS SHOWN AS IT SETTLES IS THE WHOLE TEXT REDACTED, however the text
// arrives — a byte at a time, or in pieces that cut lines anywhere.
//
// Mutation: make [redact.Settled] settle every complete line, and the closed
// key, the prefixed key, the password on a later line and the key after a
// password each come out differently from the whole.
func TestTextShownAsItSettlesIsTheWholeTextRedacted(t *testing.T) {
	t.Parallel()
	byByte := byteAtATime()
	for name, text := range keyFixtures() {
		want := redact.Secrets(text)
		if got := byByte[name].shown; got != want {
			t.Errorf("%s a byte at a time:\n got %q\nwant %q", name, got, want)
		}
		for _, sizes := range [][]int{{7}, {64}, {3, 50, 1, 200}, {len(text)}} {
			if got, _ := settle(text, sizes); got != want {
				t.Errorf("%s in pieces of %v:\n got %q\nwant %q", name, sizes, got, want)
			}
		}
	}
}

// byteAtATime is [settle] over every key fixture a byte at a time: the
// costliest schedule there is, and the one both the case above and the case
// below read, so it is worked out once for the two.
var byteAtATime = sync.OnceValue(func() map[string]settled {
	out := map[string]settled{}
	for name, text := range keyFixtures() {
		shown, steps := settle(text, []int{1})
		out[name] = settled{shown: shown, steps: steps}
	}
	return out
})

// settled is what [settle] returns: everything shown, and what had been shown
// after each piece.
type settled struct {
	shown string
	steps []string
}

// NOTHING SHOWN BEFORE THE END IS A SECRET, at any moment: a key's body shown
// before its END lands is a key in clear on a screen, whatever the record
// redacts afterwards.
func TestNoSecretIsShownBeforeItsShapeIsSettled(t *testing.T) {
	t.Parallel()
	for name, run := range byteAtATime() {
		checked := -1
		for _, shown := range run.steps {
			// What is shown only ever grows, so a step as long as the last
			// one checked is that step again.
			if len(shown) == checked {
				continue
			}
			checked = len(shown)
			if strings.Contains(shown, keyLine) || strings.Contains(shown, sshLine) ||
				strings.Contains(shown, keyLine[:30]) || strings.Contains(shown, "hunter2") ||
				strings.Contains(shown, "swordfish") {
				t.Errorf("%s: shown before the text was whole: %q", name, shown)
				break
			}
		}
	}
}

// A HEADER WITH NO KEY UNDER IT IS HELD ONLY UNTIL A LINE THAT IS NOT A KEY'S:
// an END after that closes nothing, so nothing after it can change how the
// header redacts. It used to be held until 64 KiB had been written after it,
// over ordinary lines and all — the rest of the run, on a live view, for a
// grep of the armour line. A body is held a little longer: until no END
// written next could still read it back with a short last line and a line
// interrupting the key on either side of that.
//
// Mutation: hold an open BEGIN as far as an END could be paired with it at
// any distance, and the line after the header is never shown.
func TestAHeaderIsHeldOnlyWhileAKeyCouldFollowIt(t *testing.T) {
	header := "x:-----BEGIN RSA PRIVATE KEY-----\n"
	over := header + pemBody(2) + "the run went on\nand on\nand on\nand on\nand on\n"
	for name, c := range map[string]struct {
		text string
		want int
	}{
		"a header alone, its next line not written yet": {header, 0},
		"a header and a line that is not a key's":       {header + "line\n", len(header + "line\n")},
		"a header and a body line, the next to come":    {header + keyLine + "\n", 0},
		// One word after a body could be its short last line, which an END
		// may still follow.
		"a header, a body and a word": {header + pemBody(2) + "line\n", 0},
		// Three lines after it could still be an interruption, the short
		// last line and another before an END; four, an interruption and
		// the short last line below two an END reads back across to a
		// line as wide as the body's.
		"a header, a body and three lines": {header + pemBody(2) + "the run went on\nand on\nand on\n", 0},
		"a header, a body and four lines":  {header + pemBody(2) + "the run went on\nand on\nand on\nand on\n", 0},
		"a header and a body that is over": {over, len(over)},
		"a header and a closed block": {header + pemBody(2) + "-----END RSA PRIVATE KEY-----\nnext\n",
			len(header + pemBody(2) + "-----END RSA PRIVATE KEY-----\nnext\n")},
		"a grep's match and its next result": {grepOfTheBeginLine, len(grepOfTheBeginLine)},
	} {
		if got := redact.Settled(c.text); got != c.want {
			t.Errorf("%s: Settled = %d of %d; want %d", name, got, len(c.text), c.want)
		}
	}
}

// A BEGIN ON A LINE LONGER THAN THE BOUND HOLDS NOTHING AFTER THAT LINE: a
// block runs no further than [redact.MaxKeyBlockBytes], so no line after it can
// be the block's, and what the line held of a body is redacted with the line.
// The scan of the BEGIN's line stops at the bound, and what it found there was
// read as the text running out — an OPEN block — so a minified bundle or a run
// of padding that mentioned the armour held every line after it back from a
// live view for good.
//
// Mutation: answer a BEGIN line the scan stopped on at the bound as one the
// text ran out on, and none of these settles past its first line.
func TestABeginOnALineLongerThanTheBoundHoldsNothingAfterIt(t *testing.T) {
	padding := strings.Repeat(" ", redact.MaxKeyBlockBytes+5000)
	for name, c := range map[string]struct{ line, want string }{
		"a minified bundle naming the armour": {
			`var a="-----BEGIN PRIVATE KEY-----",` + strings.Repeat("b.c(d);", 12000),
			`var a="-----BEGIN PRIVATE KEY-----",` + strings.Repeat("b.c(d);", 12000),
		},
		"the armour, then padding": {
			"-----BEGIN PRIVATE KEY-----" + padding + "tail",
			"-----BEGIN PRIVATE KEY-----" + padding + "tail",
		},
		"a one-line key, then padding": {
			"-----BEGIN RSA PRIVATE KEY----- " + keyLine + " " + keyLine + padding + "tail",
			marker + padding + "tail",
		},
	} {
		text := c.line + "\nnext line\nmore\n"
		if got := redact.Settled(text); got != len(text) {
			t.Errorf("%s: Settled = %d of %d; want all of it", name, got, len(text))
		}
		if got := redact.Secrets(text); got != c.want+"\nnext line\nmore\n" {
			t.Errorf("%s: Secrets = %.80q…; want %.80q…", name, got, c.want)
		}
	}
}

// A STREAM OF KEY-SHAPED LINES IS HELD NO FURTHER BACK THAN AN END COULD READ:
// an END written next reads back at most [redact.MaxKeyBlockBytes], so what is
// further back than that is settled while the stream goes on, and all of it
// once lines that are not a key's follow.
func TestAStreamOfKeyShapedLinesIsHeldOnlyAsFarAsAnEndCouldRead(t *testing.T) {
	stream := "x:-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat(keyLine+"\n", 2*redact.MaxKeyBlockBytes/len(keyLine)+4)
	got := redact.Settled(stream)
	if got == 0 || len(stream)-got > redact.MaxKeyBlockBytes {
		t.Errorf("Settled = %d of %d; want all but the last %d bytes settled", got, len(stream), redact.MaxKeyBlockBytes)
	}
	over := stream + "the run\nwent\non\nand on\nand on\n"
	if got := redact.Settled(over); got != len(over) {
		t.Errorf("Settled = %d of %d; want all of it once no key could still be read", got, len(over))
	}
}

// A LINE THAT READS LIKE A KEY'S BODY WAITS, because an END written next could
// read it back — and nothing else does. Read back, base64 at a body's width
// counts only when it carries upper and lower case letters, as the encoding of
// random bytes does, digit or none; a path in lower case and a commit's hash
// are in one case, and a live view shows them the moment they are whole. What
// releases a line that waits is two lines in a row that are no key's with two
// more after them, whatever those are: a key's line among the last two may yet
// be read as one interrupting the key, and holds the line before it. A line
// exactly as wide as a key's waits for one more, and so does one two lines
// above another of its width that waits, since an END can read back across two
// lines to it; and a line that may be OpenPGP's checksum, with no more than two
// lines after it, starts the count again above it, since an END reads back from
// one as from itself.
//
// Mutation: read any run at a body's width as a key's line back from an END,
// and the path and the hash wait for four more lines; ask it for a digit as
// well, and the line with none is shown before an END could still read it back;
// read a held key's line only as a full one, never as one interrupting a key,
// and the line two lines before it is shown although an END can still reach it;
// read on past a key's marker, and the line before it waits for an END that
// could never reach it; hold nothing a read could cross two lines to, and a
// key's line is shown after four although an END could still reach it; cross
// to a line at any width, and the ones at no key's width wait for five; leave
// MIME's width out, and its line is shown after four; hold nothing a checksum
// starts a read again from, and the line above one is shown after five.
func TestOnlyALineThatReadsLikeAKeysBodyWaitsForAnEnd(t *testing.T) {
	for name, line := range map[string]string{
		"a long path":     "[tool] Read: /home/user/crewlet/internal/sandbox/executor/claudestream.go",
		"a commit's hash": "[tool] Bash: git show 9fceb02d0ae598e95dc970b74767f19372d61af8",
		"prose":           "the build passed and the run went on",
	} {
		if got := redact.Settled(line + "\n"); got != len(line)+1 {
			t.Errorf("%s: Settled = %d of %d; want it shown at once", name, got, len(line)+1)
		}
	}
	for name, c := range map[string]struct {
		line string
		wait int
	}{
		"a key's line":               {keyLine, 5},
		"a key's line with no digit": {digitless, 5},
		"an OpenSSH key's line":      {sshLine, 5},
		"a line at MIME's width":     {mimeLine, 5},
		"a path in mixed case":       {mixedCasePath, 4},
		"a key's last line":          {keyLine[:56], 4},
	} {
		text := "start\n" + c.line + "\n"
		for i := range c.wait {
			if got := redact.Settled(text); got != len("start\n") {
				t.Errorf("%s, after %d lines: Settled = %d; want it held", name, i, got)
			}
			text += fmt.Sprintf("line %d\n", i)
		}
		if got := redact.Settled(text); got != len(text) {
			t.Errorf("%s: Settled = %d of %d; want it shown once %d lines no key's follow it", name, got, len(text), c.wait)
		}
	}
	for name, c := range map[string]struct {
		after []string
		held  bool
	}{
		"two lines, then a key's line":                         {[]string{"one", "two", keyLine}, true},
		"two lines, a key's line, then a line":                 {[]string{"one", "two", keyLine, "three"}, true},
		"two lines, a line at no key's width, then two lines":  {[]string{"one", "two", keyLine[:56], "three", "four"}, false},
		"three lines, a key's line, then a line":               {[]string{"one", "two", "three", keyLine, "four"}, false},
		"a line, a key's line, then four lines":                {[]string{"one", keyLine, "two", "three", "four", "five"}, true},
		"a line, a key's line, then five lines":                {[]string{"one", keyLine, "two", "three", "four", "five", "six"}, false},
		"two lines, then a blank and a glue line":              {[]string{"one", "two", "", `" +`}, true},
		"two lines, then two blank ones, then three":           {[]string{"one", "two", "", "", "three", "four", "five"}, false},
		"two lines, then two blank ones, then two":             {[]string{"one", "two", "", "", "three", "four"}, true},
		"two lines, a checksum, then two":                      {[]string{"one", "two", "=nE4V", "three", "four"}, true},
		"two lines, a checksum, then three":                    {[]string{"one", "two", "=nE4V", "three", "four", "five"}, false},
		"a key's line cut short, then four lines":              {[]string{keyLine[:56], "one", "two", "three", "four"}, false},
		"two lines, a line at the other width, then two lines": {[]string{"one", "two", sshLine, "three", "four"}, false},
		// A line a cut wrote into is at no width a read can tell, so a
		// read crosses two lines from it to one at any encoder's width.
		"two lines, a key's line cut short, then two lines":  {[]string{"one", "two", keyLine[:30] + "[...truncated]", "three", "four"}, true},
		"two lines, a key's line cut short, then four lines": {[]string{"one", "two", keyLine[:30] + "[...truncated]", "three", "four", "five", "six"}, false},
		// Read as the body's last line, with a line between it and an END
		// written next, a line at any width is one a read crosses two
		// lines from.
		"two lines, a line at the other width, then a line": {[]string{"one", "two", sshLine, "three"}, true},
		// No END after another block's armour, or a key already redacted
		// in its place, reads back past it.
		"a certificate's END":  {[]string{"-----END CERTIFICATE-----"}, false},
		"a key redacted after": {[]string{marker}, false},
	} {
		text := "start\n" + keyLine + "\n" + strings.Join(c.after, "\n") + "\n"
		if got := redact.Settled(text); (got == len("start\n")) != c.held {
			t.Errorf("%s: Settled = %d; want the first key's line held: %v", name, got, c.held)
		}
	}
}

// HALF A LINE IS NEVER SETTLED: a credential is matched by its whole shape, and
// a token half written is a line that changes when its other half lands.
func TestHalfALineIsNeverSettled(t *testing.T) {
	if got := redact.Settled("done\nsk-" + strings.Repeat("q", 10)); got != len("done\n") {
		t.Fatalf("Settled = %d; want only the complete line", got)
	}
	if got := redact.Settled("no line break yet"); got != 0 {
		t.Fatalf("Settled = %d; want nothing settled before a line ends", got)
	}
}

// A KEY A BLOCK TAKES NAMES NO VALUE PAST IT. A password's key on a line a
// key's block read from its BEGIN takes — `3 password:` interrupting a body
// `printf` wrote in pieces — is inside the block's marker by the time the
// rules run, so it names nothing; and its value, read past the wrapping's
// blank line to the END's own line, is that END's to read back. Claimed as
// the password's value, `u1SU1Lf=` was no part of the key the END read, and
// it was published — until a second pass, the key gone into the marker, read
// it back and redacted it.
//
// Mutation: claim the value of a password whose key a block took, and the
// END's line keeps u1SU1Lf= in clear.
func TestAKeyABlockTakesNamesNoValuePastIt(t *testing.T) {
	t.Parallel()
	key := redact.Marker + "private-key]"
	text := "'-----BEGIN RSA PRIVATE KEY-----' '" + keyLine + "' '\n3 password:\n    32\t\n     9\u2192u1SU1Lf=-----END RSA PRIVATE KEY-----\n"
	want := "'" + key + "\t\n     9\u2192" + key + "\n"
	got := redact.Secrets(text)
	if got != want {
		t.Fatalf("Secrets:\n got %q\nwant %q", got, want)
	}
	if again := redact.Secrets(got); again != got {
		t.Errorf("a second pass changed %q to %q", got, again)
	}
	for _, sizes := range [][]int{{1}, {5, 400}} {
		if shown, _ := settle(text, sizes); shown != want {
			t.Errorf("in pieces of %v: shown as it settled\n%q\nwant\n%q", sizes, shown, want)
		}
	}
}

// AN END READS BACK ACROSS A PASSWORD'S PROMPTS as it does across blank lines:
// a line that ends in a password's key and its separator is one a key's block
// can take whole, since what it names is on a line between it and the END,
// which the block takes too — whatever the line holds before the key, another
// password and its value among it. Read as lines interrupting the key, three
// prompts — another process asking for a password three times — stopped the
// END's read, and a key's tail above them, its BEGIN not in the text, was
// published. A live view holds a key's line across prompts as an END could read
// back across them, so what it shows is what the whole text redacts. A line
// holding a password and its value and no prompt still interrupts the key, and
// three of them end the read as any three other lines do.
//
// Mutation: read a prompt back as a line interrupting the key, and every case
// but the last publishes the key's lines; let a live view count a prompt
// toward letting a line it holds go, and shown in pieces the six prompts'
// key is published before its END; read a line holding a second password's
// key and its value before the prompt's as no prompt, and the key above three
// of them is published; ask a prompt whether what comes before its key could
// be a key's line, and the second pass of a value beside a prompt, a marker
// there, reads back further than the first; read a key that is the value of
// a key before it as a prompt, or one a token takes, and the line a first
// pass never read back from, its key replaced, is a line of a key's text to
// the second; read a prompt's key bare — no quote or markup closing it, no
// operator after it — and the key above three prompts so written is
// published; read a string that ends in the key's word as a prompt, and the
// END reads back across three of them, which name no value a block could
// take.
func TestAnEndReadsBackAcrossPasswordPrompts(t *testing.T) {
	t.Parallel()
	key := redact.Marker + "private-key]"
	end := "-----END RSA PRIVATE KEY-----"
	for name, c := range map[string]struct{ text, want string }{
		"three prompts": {
			keyLine + "\n" + keyLine + "\npassword:\npassword:\npassword:\n" + end + "\n", key + "\n",
		},
		"six prompts": {
			keyLine + "\n" + keyLine + "\n" + strings.Repeat("Password:\n", 6) + end + "\nnext\n", key + "\nnext\n",
		},
		"two prompts under a cut, words before the key": {
			"[… 812 bytes truncated]" + keyLine[20:] + "\nEnter password:\nPassword:\n" + end + "\n",
			"[… 812 bytes truncated]" + key + "\n",
		},
		"two prompts under a labelled line": {
			"key: " + keyLine + "\nPassword:\nPassword:\n" + end + "\n", "key: " + key + "\n",
		},
		"three prompts each after another password and its value": {
			keyLine + "\n" + keyLine + "\npwd=x password:\npwd=y password:\npwd=z password:\n" + end + "\n", key + "\n",
		},
		"three prompts a quote closes": {
			keyLine + "\n" + keyLine + "\n\"password\":\n'password' =>\n**Password**:\n" + end + "\n", key + "\n",
		},
		"three prompts an operator separates": {
			keyLine + "\n" + keyLine + "\npassword :=\npassword ==\npassword =>\n" + end + "\n", key + "\n",
		},
		"three prompts under a diff's marks": {
			// The `+` is base64, so read back the block starts with it.
			"+" + keyLine + "\n+" + keyLine + "\n+password:\n+Password :\n+pwd=\n+" + end + "\n", key + "\n",
		},
		// A string ending in the key's word is no key and no prompt, so
		// three of them part a key as any three lines do.
		"still published: three strings that end in the key's word": {
			keyLine + "\n" + keyLine + "\n" + strings.Repeat("\"POST /auth/password\":\n", 3) + end + "\n",
			keyLine + "\n" + keyLine + "\n" + strings.Repeat("\"POST /auth/password\":\n", 3) + end + "\n",
		},
		"still published: three lines holding a password beside its value": {
			keyLine + "\n" + keyLine + "\npassword: a1\npassword: a2\npassword: a3\n" + end + "\n",
			keyLine + "\n" + keyLine + "\npassword: " + redact.Marker + "password]\npassword: " + redact.Marker + "password]\npassword: " +
				redact.Marker + "password]\n" + end + "\n",
		},
	} {
		got := redact.Secrets(c.text)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		if again := redact.Secrets(got); again != got {
			t.Errorf("%s: a second pass changed %q to %q", name, got, again)
		}
		for _, sizes := range [][]int{{1}, {5, 400}} {
			if shown, _ := settle(c.text, sizes); shown != c.want {
				t.Errorf("%s in pieces of %v: shown as it settled\n%q\nwant\n%q", name, sizes, shown, c.want)
			}
		}
	}
	// What comes before a prompt's key is replaced by a pass — a value
	// another key named, a second key's, the prompt's own key as that value
	// or inside a token — and a line reads back alike on every pass.
	for name, text := range map[string]string{
		"a line with two keys":                               keyLine + "\n" + keyLine + "\npwd=x password:\npassword:\npassword:\n" + end + "\n",
		"a prompt whose key is the value of a key before it": keyLine[:30] + "\npwd= password:\npwd= password:\npwd= password:\n" + end + "\n",
		"a prompt beside a value a prompt above it named": "u1SU1Lf= fAt0gun+\nPassword:\nSEKRIT2x9  # rotate the password:\n" +
			"pwd=Ab12cdEF34gh\n" + end + "\n",
		"a key's piece and a prompt whose key is the value of a key before it": "ffAt0gun passwd:Pwd:\n" + end + "\n",
		"a key's piece and a prompt whose key a token takes":                   "ffAt0gun sk-" + strings.Repeat("q", 17) + "Pwd:\n" + end + "\n",
	} {
		once := redact.Secrets(text)
		if twice := redact.Secrets(once); twice != once {
			t.Errorf("%s: a second pass changed\n%q\nto\n%q", name, once, twice)
		}
	}
}

// A VALUE DECIDED ON A LINE A KEY STILL HOLDS WAITS WITH IT. A password's key
// whose line ends in an `echo`'s quote is decided by the line after it, and
// where that line is a key's BEGIN its END is not written yet: the END turns
// the lines between into one marker that ends in the `echo`'s quote too, and
// the value is read off it. Released on the line as first written — the
// BEGIN's own marker, no line of the wrapping — the live view showed the
// quote replaced, which the whole text keeps.
//
// Mutation: release a key whose line after it decided what its own line
// alone gives while that line is held only by a block an END closes, and
// the live view replaces the quote.
func TestAValueDecidedOnALineAKeyHoldsWaitsWithIt(t *testing.T) {
	t.Parallel()
	lines := []string{
		"echo \"Password:\" >> key.pem",
		"KEY=-----BEGIN RSA PRIVATE KEY----- Proc-Type: 4,ENCRYPTED DEK-Info: AES-128-CBC,0123456789ABCDEF0123456789ABCDEF " + keyLine,
		keyLine[:30],
		"| hunter2x9 |",
		"echo \"-----END RSA PRIVATE KEY-----\" >> key.pem",
	}
	text := strings.Join(lines, "\n") + "\n"
	if got := redact.Settled(strings.Join(lines[:4], "\n") + "\n"); got != 0 {
		t.Errorf("before the END is written, Settled = %d; want the key's line held", got)
	}
	want := redact.Secrets(text)
	for _, sizes := range [][]int{{1}, {3, 16, 3}, {5, 400}} {
		if shown, _ := settle(text, sizes); shown != want {
			t.Errorf("in pieces of %v: shown as it settled\n%q\nwant\n%q", sizes, shown, want)
		}
	}
}

// A KEY WAITS WITH THE VALUE THE KEYS WERE READ BESIDE. A key's block is read
// with every password's value claimed ([claims]), so a value on the line
// after its key is no line of the key; but cut between the two, the text after
// the cut is read with no key naming that value, and an END reads the value
// back as the key's. Where the two agree on what is redacted, it is the same
// line either way; where they do not — a block joins the value's line to an
// END's line that a password's key follows, and the rule, reading that key on
// the joined line, takes the key's own punctuation for the first one's value —
// the live view showed the key's line redacted that the whole text keeps.
//
// Mutation: hold a key for the value the rule reads alone, as the keys are
// not read beside it, and shown in pieces the value's line is the key's
// marker.
func TestAKeyWaitsWithTheValueTheKeysWereReadBeside(t *testing.T) {
	t.Parallel()
	text := "const key = \"Password:\\n\" +\n\t\"u1SU1Lf= fAt0gun+\\n\" +\n" +
		"\t\"-----END RSA PRIVATE KEY-----|passwd:\\n\" +\n\t\"u1SU1Lf= fAt0gun+\\n\"\n"
	lines := strings.SplitAfter(text, "\n")
	if got := redact.Settled(strings.Join(lines[:3], "")); got != 0 {
		t.Errorf("before the value's key is decided, Settled = %d; want the key's line held", got)
	}
	want := redact.Secrets(text)
	for _, sizes := range [][]int{{1}, {5, 400}} {
		if shown, _ := settle(text, sizes); shown != want {
			t.Errorf("in pieces of %v: shown as it settled\n%q\nwant\n%q", sizes, shown, want)
		}
	}
}

// A PASSWORD KEY WAITS FOR ITS VALUE, which the rule takes off a later line:
// its line is held while every line after it is one the value may still come
// after — a blank line, the wrapping's blank line, the punctuation that closes
// a concatenated string's line — and while a line ending in what the key's
// line ends in may still say it was the wrapping's suffix, and released once
// the line that decides is written: the value's, or one that holds a key of
// its own or does not repeat the suffix, which leaves the key's line as the
// whole text redacts it. Never for ever: a line that is none of those ends
// the wait. And not while the next key waits: a line holding a key of its own
// leaves the key above it the value its own line gives, which no line written
// later changes — the quote of an `echo`'s or a string's that no line repeats,
// or none — so a run of such keys is shown as it is written, its last line
// alone held. The lines are read as the rule reads them, the tokens already
// replaced: a key a token runs into is none.
//
// Mutation: hold a key only while nothing but whitespace follows it, as
// Settled did, and the live view shows the `echo`'s quote, the blank
// numbered line and the concatenation's glue replaced, then the value in
// clear on the line after; hold a key whatever follows it, and the cases that
// release it never settle; hold a key while the line that decided it waits,
// whatever it decided, and the key above a key of its own waits with it; hold
// one whose line's own quote is what it decided, and a run of `password:"` or
// of `echo`ed keys is held back whole; hold one decided to name nothing where
// its own line would name nothing either, and the key over a value already
// redacted waits with the key beside that value; read a value of digits under
// a numbered list item as a blank numbered line, and the key waits for as
// long as such lines go on; pass over a blank line of the wrapping before
// asking whether it holds a key, and the key above it waits for the line
// below; read the next key from the end of the key rather than of its value,
// and a key inside a value is held; read the values off the
// text as written rather than as the rule reads it, and the key a token runs
// into holds its line; hold a key a quote closes whatever string the quote
// closes, and a route that ends in the key's word waits for the next line.
func TestAPasswordKeyIsHeldUntilItsValueIsWritten(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		// lines are written one after another; held is how many of them,
		// counted from the key's, are held after each is written.
		lines []string
		held  []int
	}{
		"plain":                       {[]string{"ok", "Enter the password:", "hunter2"}, []int{0, 1, 0}},
		"echoed, a blank line echoed": {[]string{"echo \"Password:\" >> key.pem", "echo \"\" >> key.pem", "echo \"hunter2x9\" >> key.pem"}, []int{1, 2, 0}},
		"under line numbers":          {[]string{"     9\u2192Password:", "    10\u2192", "    11\u2192hunter2x9"}, []int{1, 2, 0}},
		"concatenated":                {[]string{"\t\"password:\\n\" +", "\t\"\\n\" +", "\t\"hunter2x9\\n\""}, []int{1, 2, 0}},
		"an echo's quote no line repeats": {
			[]string{"echo \"Password:\" >> key.pem", "chmod 600 key.pem"}, []int{1, 0},
		},
		"a key of its own on the next line": {[]string{"password:", "  password:", "  - x"}, []int{1, 1, 0}},
		"a run of keys, each decided by the next": {
			[]string{`password:"`, `password:"`, `password:"`}, []int{1, 1, 1},
		},
		"a run of echoed keys": {
			[]string{"echo \"Password:\" >> key.pem", "echo \"Password:\" >> key.pem", "echo \"Password:\" >> key.pem"}, []int{1, 1, 1},
		},
		"a key over a value already redacted beside a key of its own": {
			[]string{"password:", redact.Marker + "password] password:"}, []int{1, 1},
		},
		"a key over a value already redacted behind the gutter, beside a key of its own": {
			[]string{"x password:", "x " + redact.Marker + "password] password:"}, []int{1, 1},
		},
		"a blank line of the wrapping holding a key of its own": {
			[]string{"a pwd= b password:", "a pwd=", "hunter2x9"}, []int{1, 1, 0},
		},
		"a value of digits under a numbered list item": {
			[]string{"     1\u21921. Password:", "     2\u2192839201"}, []int{1, 0},
		},
		"a key inside a value": {[]string{"pwd=password:"}, []int{0}},
		// A quote closes a key only where it closes the key's own string:
		// a route ending in the word is no key, and holds nothing.
		"a string that ends in the key's word": {[]string{`"POST /auth/password"`, "next"}, []int{0, 0}},
		// An operator's second character is the separator's, so the value
		// is the word after it, on the key's own line.
		"Go's short declaration": {[]string{"\tpassword := hunter2x9", "next"}, []int{0, 0}},
		// Punctuation before another key on the rest of the line an
		// escaped break ended the key's on is the value, as it is on the
		// key's own line, and no line after decides anything.
		"punctuation and a key after an escaped break": {[]string{"\"password:\\n\" + pwd: x"}, []int{0}},
		// The token's marker on the line after ends it as the key's line
		// does, so the value is none there — and no key holds that line,
		// since the one written there is inside the token.
		"a key a token runs into on the next line": {
			[]string{"password: ]", "sk-" + strings.Repeat("A", 20) + "password: ]"}, []int{1, 0},
		},
	} {
		var text string
		for i, line := range c.lines {
			text += line + "\n"
			want := len(text)
			for n := 0; n < c.held[i]; n++ {
				want = strings.LastIndexByte(text[:want-1], '\n') + 1
			}
			if got := redact.Settled(text); got != want {
				t.Errorf("%s, %d lines written: Settled = %d; want %d (%d lines held) of %q",
					name, i+1, got, want, c.held[i], text)
			}
		}
	}
}
