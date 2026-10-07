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
// wrapped, blank lines bare and wrapped, the wrapping's own punctuation,
// headers, another block's armour, base64 that is not a key's, prose and a
// line another process wrote, a password key and its value.
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
	strings.Repeat("9f86d081884c7d65", 4),
	foreignLine,
	"[tool] bash: go test ./...",
	"ok  \tgithub.com/acme/api\t0.412s",
	"password:",
	"  hunter2",
	"token sk-" + strings.Repeat("q", 40),
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
