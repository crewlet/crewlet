package redact_test

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// transcriptLines are the lines an executor's transcript is made of, as far as
// the key reader is concerned: armour lines bare, wrapped and glued to a body's
// line, full and short body lines, blank and wrapping lines, headers, other
// base64, prose, a key already redacted, password keys and their values (in
// every wrapping a line is written in, with the wrapping's blank lines
// between), what a rule writes into a line a key is read across (markers,
// tokens, an AWS session's keys and secrets, in every placement), what a cut
// leaves of a key (heads, tails, marks, a window's first line), and prose
// or another rule's matches that read like a piece of a key.
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
	"ASIAABCDEFGHIJKLMNOP",
	"Authorization: Bearer ya29.c.c0ASRK0GaBcDeFgHiJkLmNoPqRsTuVwXyZ-_12",
	"the token is ya29.a0AfB_byC-Xy3Xy3Xy3Xy3Xy3Xy3.",
	"ya29.a0AbCdEf_GhIj-KlMnOp12-----END RSA PRIVATE KEY-----",
	"export AWS_SESSION_TOKEN=" + strings.Repeat("Ab3+", 12) + "==",
	`  "SecretAccessKey": "` + strings.Repeat("Qw3/", 10) + `",`,
	"SessionToken=Ab12cdEF34ghIJKLmnopQRSTuvwxYZabCDefGHijKLmn",
	"AWS_SESSION_TOKEN=" + strings.Repeat("Ab3+", 12) + "-----END RSA PRIVATE KEY-----",
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
	keyLikeRun + " -----END RSA PRIVATE KEY-----|passwd:",
	keyLikeRun + " -----END RSA PRIVATE KEY-----",
	keyLikeRun + " Password:",
	keyLikeRun + ` "`,
}

// keyLikeRun reads like a key's line, short of an encoder's width, and opens
// a line as a word a gutter can be.
const keyLikeRun = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6"

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
		if got, _ := settle(t, text, sizes); got != want {
			t.Fatalf("trial %d, in pieces of %v:\ntext %q\n got %q\nwant %q", trial, sizes, text, got, want)
		}
	}
}

// A KEY LINE ABOVE A BLOCK WHOSE CUT IS UNDECIDED IS HELD: a block on a
// password key's line is written apart from that line's gutter only once a
// line below gives the key its value, and then a key line above it reads the
// gutter the cut leaves as its own value. Shown before that, the line above
// had no value, and the gutter was shown with the block's line.
//
// Mutation: leave the block's lines out of what [redact.Settled] counts as
// rewritable, and the gutter is shown before the line that decides it lands.
func TestAKeyLineAboveABlockWhoseCutIsUndecidedIsHeld(t *testing.T) {
	t.Parallel()
	end := " -----END RSA PRIVATE KEY-----"
	for name, text := range map[string]string{
		"a prompt above": keyLikeRun + end + "|passwd:\n" + keyLikeRun + " Password:\n" + keyLikeRun + end + "|passwd:\n" +
			keyLikeRun + " \"\n",
		"a key's line above": keyLikeRun + end + "|passwd:\n" + keyLikeRun + end + "\n" + keyLikeRun + end + "|passwd:\n" +
			keyLikeRun + " " + keyLikeRun + "\n",
	} {
		want := redact.Secrets(text)
		for _, sizes := range [][]int{{1}, {7}, {64}, {len(text)}} {
			if got, _ := settle(t, text, sizes); got != want {
				t.Errorf("%s in pieces of %v:\n got %q\nwant %q", name, sizes, got, want)
			}
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

// A BREAK AFTER A TEXT'S LAST LINE CHANGES NO REDACTION: Secrets(t+"\n") is
// Secrets(t)+"\n", whatever t's end decides, for a t not ending in "\r" (the
// "\n" would join it into one break). An executor's live reading writes each
// transcript entry as a line, so it shows the record's text and one break only
// while that break decides nothing. Over transcripts of every shape above and
// every credential shape, whole and cut at a random byte; every prefix of the
// hand and credential shapes, after a line and after a password's key; and a
// password's key followed by escaped breaks, punctuation and words up to the
// text's end. Seeded.
//
// Mutation: read an escaped break that ends the text as the string going on,
// or redact a run reaching the text's end as though its line were unfinished.
func TestABreakAfterATextsLastLineChangesNoRedaction(t *testing.T) {
	t.Parallel()
	var shapes []string
	for _, s := range redacttest.Shapes() {
		shapes = append(shapes, s.Sample, "token: "+s.Sample, "export KEY="+s.Sample,
			"[tool 3] Bash: curl -H 'Authorization: Bearer "+s.Sample)
	}
	hand := append([]string{
		"password: hunter2x9", "**password:**", "| password: |", "passwd=", "secret:", "api_key =",
		`"password": "`, `msg="Enter password:" level=info`, "Sign in with this password:",
		"-----BEGIN OPENSSH PRIVATE KEY-----", "Authorization: Bearer", "Authorization: token ghp_A1b2C3d",
		"[tool 3] Bash: echo $PASSWORD", "[error] boom", "[input 4] mysql -u root -p", "[input 4] password='",
		`msg := "Enter password:\n" +`, `"password:\n" + "hunter2x9"`,
		// Texts ending in an escaped break or just after one.
		`password:\n"\n`, `password:\n!@#$%^&*\n`, `Sign in with this password:\n***\n`,
		`[input 2] printf 'password:\n!@#$%^&*\n`, `pwd=\n-\n`, `passwd:\n\n"`, `pwd:\n\n'`, "PASSWORD=\n\\n+",
		"working\npwd='\n  Ab3xK9q\\n='\\n", "working\n{\"password\":\"\nhunter2x9 ***\\n\\\"\\n",
		"\"password:#\ns3cr3t\\n#\\n", "pwd:-\n  x\\\\n-\\n", "working\n**password:**\" +\n  hunter2x9\\n**\"\\n",
	}, shapes...)
	same := func(how, text string) {
		t.Helper()
		if strings.HasSuffix(text, "\r") {
			return
		}
		if got, want := redact.Secrets(text+"\n"), redact.Secrets(text)+"\n"; got != want {
			t.Fatalf("%s: a break after the text changed its redaction:\ntext %q\n got %q\nwant %q",
				how, text, got, want)
		}
	}
	pool := append(append([]string{}, transcriptLines...), hand...)
	rng := rand.New(rand.NewPCG(20261119, 3))
	for trial := range 5000 {
		var b strings.Builder
		for i := range 1 + rng.IntN(14) {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(pool[rng.IntN(len(pool))])
		}
		text := b.String()
		same(fmt.Sprintf("trial %d", trial), text)
		if len(text) > 1 {
			same(fmt.Sprintf("trial %d, cut", trial), text[:1+rng.IntN(len(text)-1)])
		}
	}
	for _, s := range hand {
		for i := 1; i <= len(s); i++ {
			same("a prefix after a line", "working\n"+s[:i])
			same("a prefix after a password's key", "Sign in with this password:\n"+s[:i])
		}
	}
	keys := []string{"password:", "Password:", "passwd:", "pwd:", "PASSWORD=", "pwd=", "secret:",
		"Sign in with this password:", `"password:`, `msg := "Enter password:`, "| Password: |", "**password:**",
		"password: ", `{"password":"`, "- password:", "+password:", "token: ", "export PASSWORD="}
	bits := []string{`\n`, `\n`, `\n`, `"`, `'`, "+", `" +`, "!@#$%^&*", "***", "|", "-", "#", "=", "hunter2x9",
		" ", "\t", "\r", `\r\n`, `\"`, "\n", "\r\n", "x", `\`, "**", "`", "Ab3xK9q", "\n  "}
	for trial := range 20000 {
		var b strings.Builder
		if rng.IntN(2) == 0 {
			b.WriteString("working\n")
		}
		b.WriteString(keys[rng.IntN(len(keys))])
		for range 1 + rng.IntN(7) {
			b.WriteString(bits[rng.IntN(len(bits))])
		}
		same(fmt.Sprintf("escaped trial %d", trial), b.String())
		same(fmt.Sprintf("escaped trial %d, trimmed", trial), strings.TrimRight(b.String(), " \t\r\n"))
	}
}
