package redact_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// keyLine is one line of a key's body at PEM's width: base64's alphabet, made
// of filler — a shape, not a key.
const keyLine = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"

// sshLine is one line at OpenSSH's width, seventy characters.
const sshLine = "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAABlwAAAAdzc2gtcn"

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
// END closes nothing. The rule paired an END with any BEGIN within 64 KiB
// before it, whatever lay between, so a grep's match for the BEGIN line and a
// later one for the END took every line of the run between them — the test
// run, its failure — out of the record and the live view alike.
//
// Mutation: pair an END with the BEGIN before it without reading what is
// between, and the transcript between the two greps is one marker.
func TestAnEndClosesOnlyABlockOfKey(t *testing.T) {
	for name, text := range map[string]string{
		"a grep's two matches, a run between":   grepOfBothArmours,
		"one command naming both armours":       "grep -e '-----BEGIN RSA PRIVATE KEY-----' -e '-----END RSA PRIVATE KEY-----' key.pem\n",
		"a placeholder between the armours":     "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n",
		"a sentence between the armours":        "-----BEGIN PRIVATE KEY-----\nyour key goes here\n-----END PRIVATE KEY-----\n",
		"the armours one after the other":       "a.pem:1:-----BEGIN RSA PRIVATE KEY-----\na.pem:27:-----END RSA PRIVATE KEY-----\n",
		"prose between, then a key-shaped line": "-----BEGIN RSA PRIVATE KEY-----\nsee below\n" + keyLine + "\n-----END RSA PRIVATE KEY-----\n",
	} {
		if got := redact.Secrets(text); got != text {
			t.Errorf("%s: Secrets =\n%s\nwant it unchanged", name, got)
		}
	}
}

// keyForm is one wrapping a key reaches text in, and what the text redacts to.
type keyForm struct{ text, want string }

// wrappedKeys is a key in every wrapping it reaches a transcript in, closed by
// its END: each is one marker, with the wrapping around it kept.
func wrappedKeys() map[string]keyForm {
	m := redact.Marker + "private-key]"
	numbered := func(from int, lines ...string) string {
		var b strings.Builder
		for i, line := range lines {
			fmt.Fprintf(&b, "%6d\u2192%s\n", from+i, line)
		}
		return b.String()
	}
	return map[string]keyForm{
		"plain": {
			"-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(3) + "u1SU1Lf=\n-----END RSA PRIVATE KEY-----\nnext\n",
			m + "\nnext\n",
		},
		"a reader's line numbers, across a number's width": {
			numbered(8, "-----BEGIN RSA PRIVATE KEY-----", keyLine, keyLine, keyLine, "u1SU1Lf=",
				"-----END RSA PRIVATE KEY-----", "next"),
			"     8\u2192" + m + "\n    14\u2192next\n",
		},
		"a grep's match and context lines": {
			"a.pem:1:-----BEGIN RSA PRIVATE KEY-----\na.pem-2-" + keyLine + "\na.pem-3-" + keyLine +
				"\na.pem-4-u1SU1Lf=\na.pem-5------END RSA PRIVATE KEY-----\nb.pem:9:other\n",
			"a.pem:1:" + m + "\nb.pem:9:other\n",
		},
		"a log's timestamps, the first line with its message": {
			"2026-10-06T09:00:00.120Z loaded key: -----BEGIN RSA PRIVATE KEY-----\n" +
				"2026-10-06T09:00:00.121Z " + keyLine + "\n2026-10-06T09:00:00.121Z " + keyLine +
				"\n2026-10-06T09:00:00.122Z -----END RSA PRIVATE KEY-----\n2026-10-06T09:00:01.000Z started\n",
			"2026-10-06T09:00:00.120Z loaded key: " + m + "\n2026-10-06T09:00:01.000Z started\n",
		},
		"a diff": {
			"+-----BEGIN RSA PRIVATE KEY-----\n+" + keyLine + "\n+" + keyLine + "\n+u1SU1Lf=\n+-----END RSA PRIVATE KEY-----\n context\n",
			"+" + m + "\n context\n",
		},
		"escaped in a JSON string": {
			`{"private_key": "-----BEGIN PRIVATE KEY-----\n` + keyLine + `\n` + keyLine +
				`\nu1SU1Lf=\n-----END PRIVATE KEY-----\n", "client_email": "svc@example.com"}` + "\n",
			`{"private_key": "` + m + `\n", "client_email": "svc@example.com"}` + "\n",
		},
		"escaped twice": {
			`"{\"key\": \"-----BEGIN PRIVATE KEY-----\\n` + keyLine + `\\n` + keyLine +
				`\\n-----END PRIVATE KEY-----\\n\"}"` + "\n",
			`"{\"key\": \"` + m + `\\n\"}"` + "\n",
		},
		"a quoted scalar broken across lines, its breaks kept": {
			"key: \"-----BEGIN RSA PRIVATE KEY-----\\n\n  " + keyLine + "\\n\n  " + keyLine +
				"\\n\n  u1SU1Lf=\\n\n  -----END RSA PRIVATE KEY-----\\n\"\nnext: 1\n",
			"key: \"" + m + "\\n\"\nnext: 1\n",
		},
		"a string concatenated across lines": {
			"const key = \"-----BEGIN RSA PRIVATE KEY-----\\n\" +\n\t\"" + keyLine + "\\n\" +\n\t\"" +
				keyLine + "\\n\" +\n\t\"u1SU1Lf=\\n\" +\n\t\"-----END RSA PRIVATE KEY-----\\n\"\nnext\n",
			"const key = \"" + m + "\\n\"\nnext\n",
		},
		"the same, read with line numbers": {
			numbered(12, "const key = \"-----BEGIN RSA PRIVATE KEY-----\\n\" +", "\t\""+keyLine+"\\n\" +",
				"\t\""+keyLine+"\\n\" +", "\t\"-----END RSA PRIVATE KEY-----\\n\"", "next"),
			"    12\u2192const key = \"" + m + "\\n\"\n    16\u2192next\n",
		},
		"a line echoed into a file at a time": {
			"echo \"-----BEGIN RSA PRIVATE KEY-----\" >> key.pem\necho \"" + keyLine + "\" >> key.pem\necho \"" +
				keyLine + "\" >> key.pem\necho \"-----END RSA PRIVATE KEY-----\" >> key.pem\nchmod 600 key.pem\n",
			"echo \"" + m + "\" >> key.pem\nchmod 600 key.pem\n",
		},
		"a shell's trace of printf, each line quoted": {
			"+ printf '%s\\n' '-----BEGIN RSA PRIVATE KEY-----' '" + keyLine + "' '" + keyLine +
				"' 'u1SU1Lf=' '-----END RSA PRIVATE KEY-----'\n+ next\n",
			"+ printf '%s\\n' '" + m + "'\n+ next\n",
		},
		"on one line": {
			"KEY=-----BEGIN RSA PRIVATE KEY----- " + keyLine + " " + keyLine + " u1SU1Lf= -----END RSA PRIVATE KEY----- set\n",
			"KEY=" + m + " set\n",
		},
		"encrypted, read with line numbers": {
			numbered(1, "-----BEGIN RSA PRIVATE KEY-----", "Proc-Type: 4,ENCRYPTED",
				"DEK-Info: AES-128-CBC,0123456789ABCDEF0123456789ABCDEF", "", keyLine, keyLine,
				"-----END RSA PRIVATE KEY-----"),
			"     1\u2192" + m + "\n",
		},
		"OpenSSH": {
			"-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat(sshLine+"\n", 5) + "AAAEBm9uZQ==\n" +
				"-----END OPENSSH PRIVATE KEY-----\n",
			m + "\n",
		},
	}
}

// A KEY IS READ IN EVERY WRAPPING IT REACHES A TRANSCRIPT IN, now that an END
// closes only a block of key: a reader's line numbers, a grep's context lines,
// a log's timestamps, a diff, a JSON string escaped once or twice, a string
// concatenated across lines, a line echoed at a time, the whole key on one
// line. Each is one marker, with the wrapping around it kept.
//
// Mutation: drop the per-line prefix ([prefixOK] taking nothing but
// punctuation), and every prefixed form survives in clear.
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
		if strings.Contains(redact.Secrets(cut), keyLine) {
			t.Errorf("%s with no END: key material survived:\n%s", name, redact.Secrets(cut))
		}
	}
}

// terminatedKeys is a key whose END was never written and whose body's last
// line ends in the text around it: the quote that closes the string it was
// written into, or the mark of a cut.
func terminatedKeys() map[string]keyForm {
	m := redact.Marker + "private-key]"
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
		"grep":           "a:-----BEGIN RSA PRIVATE KEY-----\nb: nothing here\nc: more\n",
		"password":       "login\npassword:\n  hunter2\nnext\n",
		"password later": "Enter password\n\n= swordfish\nok\n",
		"tokens":         "token sk-" + strings.Repeat("q", 40) + "\nAKIA" + strings.Repeat("Q", 16) + "\n",
		"key after a password": "password:\n-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(2) +
			"-----END RSA PRIVATE KEY-----\ntail\n",
		"grep of both armours": grepOfBothArmours,
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
func settle(text string, sizes []int) (string, []string) {
	var shown strings.Builder
	var steps []string
	pending := ""
	at, i := 0, 0
	for at < len(text) {
		n := min(sizes[i%len(sizes)], len(text)-at)
		i++
		pending += text[at : at+n]
		at += n
		cut := redact.Settled(pending)
		shown.WriteString(redact.Secrets(pending[:cut]))
		pending = pending[cut:]
		steps = append(steps, shown.String())
	}
	shown.WriteString(redact.Secrets(pending))
	return shown.String(), steps
}

// WHAT IS SHOWN AS IT SETTLES IS THE WHOLE TEXT REDACTED, however the text
// arrives — a byte at a time, or in pieces that cut lines anywhere.
//
// Mutation: make [redact.Settled] settle every complete line, and the closed
// key, the prefixed key, the password on a later line and the key after a
// password each come out differently from the whole.
func TestTextShownAsItSettlesIsTheWholeTextRedacted(t *testing.T) {
	for name, text := range keyFixtures() {
		want := redact.Secrets(text)
		for _, sizes := range [][]int{{1}, {7}, {64}, {3, 50, 1, 200}, {len(text)}} {
			if got, _ := settle(text, sizes); got != want {
				t.Errorf("%s in pieces of %v:\n got %q\nwant %q", name, sizes, got, want)
			}
		}
	}
}

// NOTHING SHOWN BEFORE THE END IS A SECRET, at any moment: a key's body shown
// before its END lands is a key in clear on a screen, whatever the record
// redacts afterwards.
func TestNoSecretIsShownBeforeItsShapeIsSettled(t *testing.T) {
	for name, text := range keyFixtures() {
		_, steps := settle(text, []int{1})
		for _, shown := range steps {
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
// grep of the armour line.
//
// Mutation: hold an open BEGIN as far as an END could be paired with it at
// any distance, and the line after the header is never shown.
func TestAHeaderIsHeldOnlyWhileAKeyCouldFollowIt(t *testing.T) {
	header := "x:-----BEGIN RSA PRIVATE KEY-----\n"
	for name, c := range map[string]struct {
		text string
		want int
	}{
		"a header alone, its next line not written yet": {header, 0},
		"a header and a line that is not a key's":       {header + "line\n", len(header + "line\n")},
		"a header and a body line, the next to come":    {header + keyLine + "\n", 0},
		"a header and a body that is over": {header + pemBody(2) + "the run went on\n",
			len(header + pemBody(2) + "the run went on\n")},
		// One word after a body could be its short last line, which an END
		// may still follow.
		"a header, a body and a word": {header + pemBody(2) + "line\n", 0},
		"a header and a closed block": {header + pemBody(2) + "-----END RSA PRIVATE KEY-----\nnext\n",
			len(header + pemBody(2) + "-----END RSA PRIVATE KEY-----\nnext\n")},
		"a grep's match and its next result": {grepOfTheBeginLine, len(grepOfTheBeginLine)},
	} {
		if got := redact.Settled(c.text); got != c.want {
			t.Errorf("%s: Settled = %d of %d; want %d", name, got, len(c.text), c.want)
		}
	}
	// A body-looking stream with no END is held no further than a key could
	// run: past [redact.MaxKeyBlockBytes] it is settled again.
	long := header + strings.Repeat(keyLine+"\n", redact.MaxKeyBlockBytes/len(keyLine)+2) + "line\n"
	if got := redact.Settled(long); got != len(long) {
		t.Errorf("Settled = %d of %d; want everything settled once no key could still be read", got, len(long))
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

// A PASSWORD KEY WAITS FOR ITS VALUE, which the rule takes off the next line.
func TestAPasswordKeyIsHeldUntilItsValueIsWritten(t *testing.T) {
	text := "ok\nEnter the password:\n"
	if got := redact.Settled(text); got != len("ok\n") {
		t.Fatalf("Settled = %d; want the key's line held", got)
	}
	if got := redact.Settled(text + "hunter2\n"); got != len(text+"hunter2\n") {
		t.Fatalf("Settled = %d; want the line settled once its value is written", got)
	}
}
