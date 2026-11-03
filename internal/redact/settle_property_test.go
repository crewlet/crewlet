package redact_test

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// transcriptLines are the lines an executor's transcript is made of, as far as
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
// own — and what a cut leaves of a key: a line's head before a tool's mark,
// its tail after one, both with the mark between them and a head with no
// digit, a head narrower than a full line before a bare ellipsis, a window's
// first line, a key on one line cut between its armours and a body a window
// kept before its END, a mark on a line of its own and the tail it kept, a
// head cut short with no mark, a cut between a BEGIN's armour and its break —
// and prose holding what reads like a piece of a key at the start of its line,
// at its end and in its middle, or in brackets; and what another rule claims
// that reads like such a piece: a password's key holding a digit, a value that
// does, an indented one, and a token after a `_` — and a password's key and its
// value a line or two apart in every wrapping a line is written in, with the
// wrapping's blank lines between: echoed into a file, concatenated in a
// string, under a reader's line numbers, a grep's context marks, a diff's
// `+`, a log's timestamps and nl's, in a list of strings, a table's row, a
// JSON line and a YAML string, a key nested under another, digits under a key
// whose line opens with a number, a token on every line, an escaped break
// written at a line's end, a value with one in it and another key after it,
// and a key beside a value naming its own further down.
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
	keyLine + "[...truncated]",
	keyLine[:30] + " (truncated)",
	"[… 812 bytes truncated]" + keyLine[20:],
	keyLine[:20] + "…[… 2,345 characters truncated …]…" + keyLine[30:],
	digitless[:20] + "…" + keyLine[44:],
	"     8\u2192" + keyLine[:25] + " [output truncated]",
	"KEY=-----BEGIN RSA PRIVATE KEY----- " + keyLine[:30] + "[...truncated]" + keyLine[10:] + " -----END RSA PRIVATE KEY-----",
	"…u1SU1Lf9x= -----END RSA PRIVATE KEY-----",
	"=== RUN   TestSHA256Sum",
	"--- FAIL: TestRSA2048Sign (0.01s)",
	keyLine[:24] + "…",
	"TestRSA2048Sign:",
	"Ab3f9C2d7E1b pulled",
	"[...truncated]",
	"…",
	"[...] " + keyLine[44:],
	keyLine[:30],
	"-----BEGIN RSA PRIVATE KEY-----…" + keyLine[40:],
	"[Worker1A] started",
	"s3Password: hunter2-Secret!",
	"db2Password=",
	"  Kk55Lm66NN-secretpart",
	"pwd=Ab12cdEF34gh",
	"OPENAI_API_KEY=sk-abcdefghij_Ab12CdEf34GhIjKlMnOpQr",
	`echo "Password:" >> key.pem`,
	`echo "hunter2x9" >> key.pem`,
	"\t\"password:\\n\" +",
	"\t\"\\n\" +",
	"\t\"hunter2x9\\n\"",
	"     9\u2192Password:",
	"    10\u2192",
	"    11\u2192hunter2x9",
	"a.pem:12:Enter the password:",
	"a.pem-11-hunter2x9",
	"+Password:",
	"+",
	"+hunter2x9",
	"2026-10-06T09:00:00.121Z Password:",
	"2026-10-06T09:00:00.122Z ",
	"2026-10-06T09:00:00.122Z",
	"2026-10-06T09:00:00.123Z hunter2x9",
	`  - "PWD:"`,
	`  - ""`,
	`  - "hunter2x9"`,
	`  - "pwd:\n"`,
	"| Password: |",
	"|  |",
	"| hunter2x9 |",
	`{"log": "password:\n"}`,
	`{"log": "hunter2x9"}`,
	`password: "`,
	`  hunter2x9"`,
	"  password: hunter2x9",
	"    31\tEnter the password:",
	"    32\t",
	"3 password:",
	"4567x9",
	"sk-" + strings.Repeat("Q", 24) + " password:",
	"sk-" + strings.Repeat("Q", 24),
	"sk-" + strings.Repeat("Q", 24) + " hunter2x9",
	`+password:\n`,
	`"password:\n" + pwd: x`,
	`export PASSWORD=Xk9\nQ2#vL8 pwd=abc`,
	"hunter2x9 Retype password:",
	"- password:",
	"Password: *** Retype password:",
	"***",
	`    change_password(old_password="hunter2", new_password=`,
	`                    "hunter3x9")`,
	"hunter2x9  # rotate the password:",
	"+hunter2x9 Retype password:",
	"1. Password:",
	"     1\u21921. Password:",
	"     2\u2192839201",
	"   2024.Summer!",
	`pwd=ab\ncd pwd="`,
	"+u1SU1Lf= fAt0gun+",
	"+-----END RSA PRIVATE KEY-----|passwd:",
	"+- hunter2x9",
	"Enter password:",
	"\ttext := \"Enter password:\\n\"",
	"\tfmt.Fprintf(w, \"done\\n\")",
	`  "Password:",`,
	`  "hunter2x9"`,
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
