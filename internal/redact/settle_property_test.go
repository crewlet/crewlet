package redact_test

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// transcriptLines are the lines a coding run's transcript is made of, as far as
// the key reader is concerned: the armour lines bare and wrapped, glued to a
// body's line or on one line with it, a body's full and short lines bare and
// wrapped — one with no digit among them — blank lines bare and wrapped, the
// wrapping's own punctuation, headers, another block's armour, base64 that is
// not a key's in one case and in both (a path in mixed case, a Go test's
// name), prose and a line another process wrote, a key already redacted, a
// password key (spaced from its separator too, or an `=` ending its line) and
// its value, and what a rule writes into a line a key is read across: a
// password's key with its value on the next line, in a string — broken after
// its key, its breaks kept — or written up to an armour, a key's line written
// up to a BEGIN and repeated with a quote after it, and tokens before a key's
// line or an armour, after one, or written up to one, and one on a line of its
// own.
var transcriptLines = []string{
	"-----BEGIN RSA PRIVATE KEY-----",
	"-----END RSA PRIVATE KEY-----",
	"a.pem:1:-----BEGIN RSA PRIVATE KEY-----",
	"a.pem-9------END RSA PRIVATE KEY-----",
	"     7\u2192-----BEGIN PRIVATE KEY-----",
	"     8\u2192" + keyLine,
	"     9\u2192-----END PRIVATE KEY-----",
	keyLine,
	"+" + keyLine,
	"a.pem-2-" + keyLine,
	sshLine,
	"u1SU1Lf=",
	"Done",
	"Proc-Type: 4,ENCRYPTED",
	"DEK-Info: AES-128-CBC,00FF",
	"",
	`" +`,
	`"` + keyLine + `\n" +`,
	`{"k": "-----BEGIN PRIVATE KEY-----\n` + keyLine + `\n` + keyLine + `"}`,
	`{"k": "-----BEGIN PRIVATE KEY-----\n` + keyLine + `\n-----END PRIVATE KEY-----\n"}`,
	"KEY=-----BEGIN RSA PRIVATE KEY----- " + keyLine + " -----END RSA PRIVATE KEY-----",
	"+ printf '%s\\n' '-----BEGIN RSA PRIVATE KEY-----' '" + keyLine + "' '",
	"     9\u2192",
	"a.pem-3-",
	`echo "-----BEGIN RSA PRIVATE KEY-----" >> key.pem`,
	`echo "` + keyLine + `" >> key.pem`,
	`echo "" >> key.pem`,
	`echo "-----END RSA PRIVATE KEY-----" >> key.pem`,
	"u1SU1Lf=-----END RSA PRIVATE KEY-----",
	keyLine + "-----END PRIVATE KEY-----",
	"KEY=-----BEGIN RSA PRIVATE KEY----- Proc-Type: 4,ENCRYPTED DEK-Info: AES-128-CBC,0123456789ABCDEF0123456789ABCDEF " + keyLine,
	"-----END CERTIFICATE-----",
	"-----BEGIN PGP PRIVATE KEY BLOCK-----",
	"Comment: exported for the deploy",
	"=nE4V",
	"-----END PGP PRIVATE KEY BLOCK-----",
	strings.Repeat("9f86d081884c7d65", 4),
	foreignLine,
	"[tool] bash: go test ./...",
	"ok  \tgithub.com/acme/api\t0.412s",
	digitless,
	"+" + digitless,
	strings.ToUpper(keyLine),
	mixedCasePath,
	"=== RUN   TestAKeysLineWithNoDigitIsReadBackAsALineOfIt",
	"+u1SU1Lf=",
	"+-----END RSA PRIVATE KEY-----",
	"+" + foreignLine,
	marker,
	"KEY=" + marker + " set",
	"password:",
	"Password :",
	"  hunter2",
	"token sk-" + strings.Repeat("q", 40),
	"pwd=",
	"  PASSWORD=",
	"AKIAABCDEFGHIJKLMNOP",
	`"password:\n` + keyLine + `\n" +`,
	`key: "password:\n`,
	`  hunter2\n"`,
	keyLine + "-----BEGIN RSA PRIVATE KEY-----",
	keyLine + `"`,
	"pwd=hunter2-----END RSA PRIVATE KEY-----",
	"password: " + redact.Marker + "password]",
	"sk-" + strings.Repeat("q", 21) + "-----END RSA PRIVATE KEY-----",
	"sk-" + strings.Repeat("B", 24) + " " + keyLine,
	"sk-" + strings.Repeat("C", 24) + " -----END RSA PRIVATE KEY-----",
	"-----BEGIN RSA PRIVATE KEY----- sk-" + strings.Repeat("E", 24),
	"-----END RSA PRIVATE KEY----- sk-" + strings.Repeat("G", 24),
	"[R " + keyLine,
	"xyz -----END RSA PRIVATE KEY-----",
}

// WHAT IS SHOWN AS IT SETTLES IS THE WHOLE TEXT REDACTED, over transcripts no
// one wrote by hand: lines drawn from every shape the key reader tells apart,
// in any order, fed in pieces of any size. The fixtures above are the shapes a
// reader can name; this is every way they can follow one another, which is
// where a reading that depends on what comes NEXT goes wrong.
//
// Seeded, so a failure names a text that fails every time.
func TestSettledIsExactOverTranscriptsOfEveryShape(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(20261006, 7))
	for trial := range 3000 {
		var b strings.Builder
		for range 1 + rng.IntN(14) {
			b.WriteString(transcriptLines[rng.IntN(len(transcriptLines))])
			b.WriteByte('\n')
		}
		text := b.String()
		want := redact.Secrets(text)
		sizes := []int{1 + rng.IntN(5), 1 + rng.IntN(80), 1 + rng.IntN(400)}
		if got, _ := settle(text, sizes); got != want {
			t.Fatalf("trial %d, in pieces of %v:\ntext %q\n got %q\nwant %q", trial, sizes, text, got, want)
		}
	}
}

// A SECOND PASS REDACTS NOTHING THE FIRST LEFT, over transcripts of every shape
// above, as lines and as the lines of a string with their breaks escaped. A
// key's marker ends a block where the armour it replaced did, and nothing a
// rule replaces takes what a key is read across, so an END the first pass read
// nothing back from reads back no further the second time
// ([TestARuleReplacesNothingAKeyIsReadAcross] names each way it used to).
//
// Mutation: read a key's marker as any other line, and an END after a key the
// first pass redacted reads back across it the second time; make the password
// rule replace its whole match again, and a value taken off the next line
// takes the break with it.
func TestASecondPassRedactsNothingTheFirstLeft(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(20261018, 11))
	for trial := range 4000 {
		var lines []string
		for range 1 + rng.IntN(14) {
			lines = append(lines, transcriptLines[rng.IntN(len(transcriptLines))])
		}
		for form, text := range map[string]string{
			"lines":            strings.Join(lines, "\n") + "\n",
			"a string's lines": `{"log": "` + strings.Join(lines, `\n`) + `\n"}` + "\n",
		} {
			once := redact.Secrets(text)
			if twice := redact.Secrets(once); twice != once {
				t.Fatalf("trial %d, %s:\ntext  %q\nonce  %q\ntwice %q", trial, form, text, once, twice)
			}
		}
	}
}
