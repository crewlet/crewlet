package redact_test

import (
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

// keyFixtures are the texts the idempotence and the settling cases run over:
// every private-key shape above, beside the shapes that are not keys.
func keyFixtures() map[string]string {
	return map[string]string{
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
	}
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
			if strings.Contains(shown, keyLine) || strings.Contains(shown, "hunter2") ||
				strings.Contains(shown, "swordfish") {
				t.Errorf("%s: shown before the text was whole: %q", name, shown)
				break
			}
		}
	}
}

// A HEADER NO END FOLLOWS IS HELD ONLY AS FAR AS AN END COULD STILL CLOSE IT:
// past [redact.MaxKeyBlockBytes] the lines after it are settled again, so a
// grep for the armour does not stop a live view for the rest of the run.
func TestAHeaderNoEndFollowsIsHeldOnlyAsFarAsAnEndCouldClose(t *testing.T) {
	header := "x:-----BEGIN RSA PRIVATE KEY-----\n"
	if got := redact.Settled(header + "line\n"); got != 0 {
		t.Fatalf("Settled = %d; want the header held while an END could still close it", got)
	}
	long := header + strings.Repeat("line\n", redact.MaxKeyBlockBytes/5+1)
	if got := redact.Settled(long); got != len(long) {
		t.Fatalf("Settled = %d of %d; want everything settled once no END could close the header",
			got, len(long))
	}
	closed := header + pemBody(2) + "-----END RSA PRIVATE KEY-----\nnext\n"
	if got := redact.Settled(closed); got != len(closed) {
		t.Fatalf("Settled = %d of %d; want a closed block settled at once", got, len(closed))
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
