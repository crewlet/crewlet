package redact_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// readFrom is what a reader that begins at the line at at shows of text,
// holding [redact.LookBack] before it: the rest redacted alone, from the line
// after the last place it parts from the rest redacted after what it holds.
func readFrom(text string, at int) string {
	part := redact.Secrets(text[at:])
	held := redact.Secrets(text[max(0, at-redact.LookBack):])
	start := len(part)
	for i := 0; i < len(part) && i < len(held); i++ {
		if part[len(part)-1-i] != held[len(held)-1-i] {
			break
		}
		start--
	}
	if start > 0 && part[start-1] != '\n' {
		if i := strings.IndexByte(part[start:], '\n'); i >= 0 {
			start += i + 1
		} else {
			start = len(part)
		}
	}
	return part[start:]
}

// LOOKBACK HOLDS THE READS ACROSS LINES AT THEIR BOUNDS: it covers a key's
// armour and the stages' reach, so a reader that begins at any line near the
// end of each case, after more text than LookBack, shows the end of the whole
// redaction and the log after it.
//
// Mutation: LookBack = MaxKeyBlockBytes, or keyBehind, goes red.
func TestLookBackHoldsTheReadsAcrossLinesAtTheirBounds(t *testing.T) {
	t.Parallel()
	filler := strings.Repeat("compiling a package of the build\n", redact.LookBack/33+100)
	log := strings.Repeat("ok\n", 200)
	const begin = "-----BEGIN RSA PRIVATE KEY-----\n"
	const line = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
	type shape struct {
		name, head, rest, secret string
	}
	var shapes []shape
	for _, x := range []int{1, 16} {
		body := strings.Repeat(line+"\n", 1007) + (line + line)[:48+x] + "\n"
		if len(begin)+len(body) != redact.MaxKeyBlockBytes+x {
			t.Fatal("the block's arithmetic")
		}
		shapes = append(shapes, shape{fmt.Sprintf("a key's block %d bytes past a block's reach", x),
			"a key:\n" + begin + body, "Zq9XwV\n\nthe build went on\n", "Zq9XwV"})
	}
	shapes = append(shapes, shape{"a password's value and label at their reach",
		"**" + strings.Repeat("a", redact.MaxReach-30) + " password:**\n" + strings.Repeat("\n", redact.MaxReach-100),
		"hunter2x9\n", "hunter2x9"})
	shapes = append(shapes, shape{"a password's value read past the keys' stage's reach",
		"sk-" + strings.Repeat("A", 30) + " password:\n" + strings.Repeat("sk-"+strings.Repeat("B", 50000)+"\n", 4),
		"sk-" + strings.Repeat("C", 30) + " hunter2x9\n", "hunter2x9"})
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			text := filler + s.head + s.rest + log
			whole := redact.Secrets(text)
			if strings.Contains(whole, s.secret) {
				t.Fatalf("the whole redaction keeps %q; the shape needs one that takes it out", s.secret)
			}
			at := len(filler) + len(s.head)
			for _, from := range []int{at - 1, at} {
				from = strings.LastIndexByte(text[:from], '\n') + 1
				shown := readFrom(text, from)
				if !strings.HasSuffix(whole, shown) || strings.Contains(shown, s.secret) ||
					!strings.HasSuffix(shown, log) {
					t.Errorf("read from byte %d (%d before the secret's line): %d bytes opening %.60q; want the "+
						"end of the whole redaction, through the log", from, at-from, len(shown), shown)
				}
			}
		})
	}
}
