package executor

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// buildLog is the lines every [runShape] ends on.
var buildLog = strings.Repeat("ok\n", 4000)

// runShape is a stream around one run that the whole redaction reads as one
// unit, longer than [redactContext]: what is written before it, the run, and
// what follows, which ends on lines of a build's log.
type runShape struct {
	name, before, run, after string
}

// runsPastTheContext is the redact package's hold shapes, each run n bytes or
// more: tokens, values and blanks after a key's armour or a password's key,
// password's keys a key's block reads, a chain of password's keys, and a
// dump of key-shaped lines.
func runsPastTheContext(n int) []runShape {
	r := rand.New(rand.NewPCG(1170, 41))
	run := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[r.IntN(len(alphabet))]
		}
		return string(b)
	}
	const (
		begin = "-----BEGIN RSA PRIVATE KEY-----"
		end   = "-----END RSA PRIVATE KEY-----"
		b64   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	)
	long, longB64 := run(b64[:62]+"-_", n), run(b64, n)
	tail := "\n" + buildLog
	armour := "a key: " + begin + "\n" + keyLine + "\n\n"
	pair := begin + "\n" + keyLine + "\n" + keyLine + "\nx Password: |\nu1SU1Lf9x "
	return []runShape{
		{"a token's body after an armour", armour + "data: " + long[:1000], "sk-" + long, tail},
		{"a JSON Web Token's signature after an armour", armour, "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0." + long, tail},
		{"an AWS secret's value after an armour", armour, "SessionToken=" + longB64, tail},
		{"a password's value after an armour", armour, "pwd=" + long, tail},
		{"a password's key's blanks after an armour", armour, "password:" + strings.Repeat(" ", n), "x" + tail},
		{"an AWS secret's quoted key and blanks", armour, "SessionToken\"" + strings.Repeat(" ", n), "= x" + tail},
		{"an AWS secret's quoted key and tabs", armour, "\"aws_secret_access_key\"" + strings.Repeat("\t", n),
			": " + longB64[:60] + tail},
		{"a token after a key and a password's line",
			"a key: " + begin + "\n" + keyLine + "\n" + end + "\nPassword: hunter2x9\n", "sk-" + long, tail},
		{"a token after an escaped break", armour + `{"pwd": "hunter2x9\n`, "sk-" + long, `"}` + tail},
		{"a token under a password's key a block reads", pair, "sk-" + long, " |" + tail},
		{"a token under a password's key, a key's line and END", pair, "sk-" + long,
			"\n" + keyLine + "\n" + end + tail},
		{"password's keys and a token", "k: " + begin + "\n" + keyLine + "\n\n",
			strings.Repeat("pwd:\n", n/5) + "sk-" + long, tail},
		{"a token and no armour", "data: " + long[:1000], "sk-" + long, tail},
		{"key-shaped lines after a BEGIN", "build log\n" + begin + "\nsee below\n",
			strings.Repeat(keyLine+"\n", n/len(keyLine)), end + tail},
	}
}

// readLive is what a live reading shows of text[at:], given what the stream
// holds before it as its lead: settled after each piece the stream is written
// in (random groups of its lines, or random byte cuts), and once it is done.
func readLive(r *rand.Rand, text string, at int, byLines bool) string {
	var p pendingText
	p.lead(text[:at])
	rest := text[at:]
	var pieces []string
	if byLines {
		lines := strings.SplitAfter(rest, "\n")
		for i := 0; i < len(lines); {
			k := 1 + r.IntN(max(1, len(lines)/3))
			pieces = append(pieces, strings.Join(lines[i:min(i+k, len(lines))], ""))
			i += k
		}
	} else {
		for i := 0; i < len(rest); {
			k := 1 + r.IntN(max(1, len(rest)/3))
			pieces = append(pieces, rest[i:min(i+k, len(rest))])
			i += k
		}
	}
	var shown strings.Builder
	for _, piece := range pieces {
		p.add(piece)
		shown.WriteString(p.settle(false))
	}
	shown.WriteString(p.settle(true))
	return shown.String()
}

// A LIVE READING BEGUN INSIDE A RUN IS THE END OF THE WHOLE STREAM'S
// REDACTION: inside a run the whole redaction reads as one unit, longer than
// its context and begun before it, on a line after that run, or inside a line
// of the log after it. What it shows, settled piece by piece and once the stream is done,
// is how the whole stream redacts, and it shows the lines after the run: from
// a cut in the log, every line the text opens.
//
// Mutation: show the text from its first byte when its context ends inside a
// line (pendingText.add moving nothing to the context), and the cuts inside a
// run show what the whole redaction does not end on.
func TestALiveReadingBegunInsideARunIsTheEndOfTheWholeRedaction(t *testing.T) {
	t.Parallel()
	filler := strings.Repeat("compiling a package of the build\n", 100)
	for i, shape := range runsPastTheContext(100 << 10) {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			r := rand.New(rand.NewPCG(1170, uint64(i)))
			text := filler + shape.before + shape.run + shape.after
			whole := redact.Secrets(text)
			runAt := len(filler) + len(shape.before)
			afterAt := runAt + len(shape.run)
			lastLine := len(text) - len("ok\n")
			cuts := []struct {
				at   int
				want string // what is shown exactly, where the cut is in the log
			}{
				{runAt + redactContext + r.IntN(len(shape.run)-redactContext), ""},
				{runAt + redactContext + r.IntN(len(shape.run)-redactContext), ""},
				{afterAt + strings.IndexByte(shape.after, '\n') + 1, ""},
				{lastLine - 3*99, text[lastLine-3*99:]},
				{lastLine - 3*99 - 2, text[lastLine-3*99:]},
			}
			for _, cut := range cuts {
				for _, byLines := range []bool{true, false} {
					shown := readLive(r, text, cut.at, byLines)
					switch {
					case !strings.HasSuffix(whole, shown):
						t.Errorf("read from %d (run at %d, by lines %v): shown %.80q…, which the whole "+
							"redaction does not end on", cut.at, runAt, byLines, shown)
					case cut.want != "" && shown != cut.want:
						t.Errorf("read from %d in the log (by lines %v): shown %d bytes; want the %d of every "+
							"line the text opens", cut.at, byLines, len(shown), len(cut.want))
					case cut.want == "" && !strings.HasSuffix(shown, buildLog):
						t.Errorf("read from %d (run at %d, by lines %v): shown %d bytes; want every line "+
							"of the log after the run", cut.at, runAt, byLines, len(shown))
					}
				}
			}
		})
	}
}

// A LIVE READING BEGUN INSIDE A KEY'S BLOCK IS THE END OF THE WHOLE STREAM'S
// REDACTION: its context holds the key's BEGIN and ends inside a line of the
// body, and what it shows, settled piece by piece and once done, is how the
// whole stream redacts, with no line of the key's body, and ends on the line
// after the block.
//
// Mutation: drop the context where the text's first line moves into it, or
// keep that line alone as the context, and the key's lines show.
func TestALiveReadingBegunInsideAKeysBlockIsTheEndOfTheWholeRedaction(t *testing.T) {
	t.Parallel()
	const begin, end = "-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----"
	lead := "the build began\n" + begin + "\n" + keyLine + "\n" + keyLine[:20]
	body := keyLine[20:] + "\n" + keyLine + "\n" + keyLine + "\n"
	for _, after := range []string{"\nthe build went on\n", end + "\nthe build went on\n"} {
		text := lead + body + after
		whole := redact.Secrets(text)
		for seed := range uint64(8) {
			r := rand.New(rand.NewPCG(1170, seed))
			for _, byLines := range []bool{true, false} {
				shown := readLive(r, text, len(lead), byLines)
				if !strings.HasSuffix(whole, shown) || strings.Contains(shown, keyLine[:16]) ||
					!strings.HasSuffix(shown, "the build went on\n") {
					t.Errorf("after %.12q (seed %d, by lines %v): shown %q; want the end of the whole "+
						"redaction %q, ending on the line after the block", after, seed, byLines, shown, whole)
				}
			}
		}
	}
}

// A LIVE READING BEGUN INSIDE A LINE KEEPS ALL ITS LEAD: the rest of the line
// its context ended in joins that context whole, so the first line its text
// opens is redacted with every byte the lead kept before the reading began and
// the rest of that line, as the first line a stream's end shows is.
//
// Mutation: trim the context to its last redactContext bytes when that rest
// joins it, or keep the rest alone, and the context loses the lead's start.
func TestALiveReadingBegunInsideALineKeepsAllItsLead(t *testing.T) {
	t.Parallel()
	given := strings.Repeat("compiling a package of the build\n", 3000) + "linking the"
	kept := given[len(given)-redactContext:]
	var p pendingText
	p.lead(given)
	if p.ctx != kept {
		t.Fatalf("the lead kept %d bytes; want its last %d", len(p.ctx), redactContext)
	}
	rest := strings.Repeat(" binary", 3000)
	p.add(rest)
	p.add(" of the run\nok\n")
	if want := kept + rest + " of the run\n"; p.ctx != want || p.text != "ok\n" {
		t.Fatalf("context %d bytes, text %q; want the %d the lead kept with the rest of its line (%d), "+
			"and text %q", len(p.ctx), p.text, len(kept), len(want), "ok\n")
	}
	if shown := p.settle(true); shown != "ok\n" {
		t.Fatalf("shown %q; want the line the text opens", shown)
	}
}
