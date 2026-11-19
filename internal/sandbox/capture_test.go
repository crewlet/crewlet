package sandbox

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

const (
	captureBegin   = "-----BEGIN RSA PRIVATE KEY-----"
	captureEnd     = "-----END RSA PRIVATE KEY-----"
	captureKeyLine = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
)

// captureShapes are outputs whose redaction a line written later decides: a
// block a later line closes, a key's block beside a password's key, a
// password's key before its value, and the redact package's hold shapes (a
// key's armour beside a long run, a value or blanks).
func captureShapes() map[string]string {
	r := rand.New(rand.NewPCG(1170, 9))
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	run := make([]byte, 48<<10)
	for i := range run {
		run[i] = alphabet[r.IntN(len(alphabet))]
	}
	long := string(run)
	tail := strings.Repeat("ok\n", 2000)
	armour := "a key: " + captureBegin + "\n" + captureKeyLine + "\n\n"
	body := strings.Repeat(captureKeyLine+"\n", 200)
	return map[string]string{
		"a block closed by a later line":    "build log\n" + body + captureEnd + "\n" + tail,
		"a key's body after prose":          "build log\n" + captureBegin + "\nsee below\n" + body + captureEnd + "\n" + tail,
		"a password's key before its value": strings.Repeat("ok\n", 50) + "login password:\n\n  hunter2x9\n" + tail,
		"an armour beside a token's run":    armour + "data: " + long[:1000] + "sk-" + long + "\n" + tail,
		"an armour beside a password":       armour + "pwd=" + long + "\n" + tail,
		"an armour beside blanks":           armour + "SessionToken\"" + strings.Repeat(" ", len(long)) + "= x\n" + tail,
		"a key's block beside a password":   captureBegin + "\n" + captureKeyLine + "\n" + captureKeyLine + "\nx Password: |\nu1SU1Lf9x sk-" + long + " |\n" + tail,
		"a password inside a key's block":   captureBegin + "\n" + captureKeyLine + "\nx Password: |\nu1SU1Lf9x sk-" + long + "\n" + captureKeyLine + "\n" + captureEnd + "\n" + tail,
	}
}

// fillerLines is n bytes of short lines, ending in a line break where n > 0.
func fillerLines(n int) string {
	if n == 0 {
		return ""
	}
	m := (n - 1) / 3
	return strings.Repeat("k", n-3*m-1) + "\n" + strings.Repeat("ok\n", m)
}

// A CUT COMMAND'S OUTPUT KEEPS ONLY WHAT ITS REDACTION SETTLES: wherever the
// limit falls in an output whose redaction a later line decides, the head a
// [capture] keeps redacts to an opening of the whole output's redaction, so a
// reader that redacts what it was handed shows what the whole output's
// redaction shows, no more; an output within the limit is kept as written.
//
// Mutation: keep the head without asking [redact.Settled], and a shape goes
// red.
func TestACutCommandOutputKeepsOnlyWhatItsRedactionSettles(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1170, 10))
	for name, shape := range captureShapes() {
		seed := r.Uint64()
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := rand.New(rand.NewPCG(1170, seed))
			// The limit falls at some of the shape's line breaks, a byte
			// either side, and at places spread over its first 24 KiB.
			var at []int
			for i := 0; i < len(shape) && len(at) < 30; i++ {
				if shape[i] == '\n' && r.IntN(4) == 0 {
					at = append(at, i, i+1, i+2)
				}
			}
			for p := 0; p < 24<<10; p += 3 << 10 {
				at = append(at, p+r.IntN(3<<10))
			}
			for _, p := range at {
				if p >= len(shape) {
					continue
				}
				text := fillerLines(captureLimit-p) + shape
				var c capture
				for rest := text; rest != ""; {
					n := min(len(rest), 1+r.IntN(64<<10))
					c.Write([]byte(rest[:n]))
					rest = rest[n:]
				}
				got := c.String()
				note := strings.LastIndex(got, "(")
				if note < 0 || !strings.Contains(got[note:], "more bytes of output not kept") {
					t.Fatalf("the limit %d bytes into the shape: no note in …%q", p, got[max(0, len(got)-120):])
				}
				kept := strings.TrimSuffix(got[:note], "\n")
				if !strings.HasPrefix(text, kept) {
					t.Fatalf("the limit %d bytes into the shape: what is kept is not the output's head", p)
				}
				if shown, whole := redact.Secrets(kept), redact.Secrets(text); !strings.HasPrefix(whole, shown) {
					j := 0
					for j < len(shown) && shown[j] == whole[j] {
						j++
					}
					t.Errorf("the limit %d bytes into the shape: what is kept redacts otherwise than the whole output from byte %d:\n kept  …%q\n whole …%q",
						p, j, shown[max(0, j-60):min(len(shown), j+60)], whole[max(0, j-60):min(len(whole), j+60)])
				}
			}
		})
	}
	within := "an error\n" + captureBegin + "\n" + captureKeyLine + "\n"
	var c capture
	c.Write([]byte(within))
	if got := c.String(); got != within {
		t.Errorf("output within the limit changed: %s", fmt.Sprintf("%q", got))
	}
}
