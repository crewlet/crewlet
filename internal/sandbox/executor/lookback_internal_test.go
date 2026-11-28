package executor

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// farBackShape is a stream whose line at at is decided by text far before it,
// what the whole redaction takes out there, and the log it ends on.
type farBackShape struct {
	name, text, secret, log string
	at                      int
}

// readsFarBack is cases whose first shown line is decided by text up to
// [redact.LookBack] before it (a key's armour and the stages' reach), each
// after more text than the context, so a reading of the line at at begins
// inside the stream.
func readsFarBack() []farBackShape {
	filler := strings.Repeat("compiling a package of the build\n", 2*redact.LookBack/33+1)
	log := strings.Repeat("ok\n", 2000)
	var out []farBackShape
	const begin = "-----BEGIN RSA PRIVATE KEY-----\n"
	for _, x := range []int{1, 16, 24} {
		body := strings.Repeat(keyLine+"\n", 1007) + (keyLine + keyLine)[:48+x] + "\n"
		if len(begin)+len(body) != redact.MaxKeyBlockBytes+x {
			panic("the block's arithmetic")
		}
		head := filler + "a key:\n" + begin + body
		out = append(out, farBackShape{fmt.Sprintf("a key's block %d bytes past a block's reach", x),
			head + "Zq9XwV\n\nthe build went on\n" + log, "Zq9XwV", log, len(head)})
	}
	label := func(name, key, between, value string) {
		head := filler + key + between
		out = append(out, farBackShape{name, head + value + "\n" + log, "hunter2x9", log, len(head)})
	}
	a := strings.Repeat("a", 6000)
	label("a bold label over blank lines", "**"+a+" password:**\n", strings.Repeat("\n", 60000), "hunter2x9")
	label("a table's row over blank rows", "| "+a+" password: |\n", strings.Repeat("|  |\n", 12000),
		"| hunter2x9 |")
	label("a value read past the keys' stage's reach", "sk-"+strings.Repeat("A", 30)+" password:\n",
		strings.Repeat("sk-"+strings.Repeat("B", 50000)+"\n", 4), "sk-"+strings.Repeat("C", 30)+" hunter2x9")
	return out
}

// readLiveFrom is what a live view shows of text[at:] with text[:at] its
// lead, added in pieces cut by mode and settled after each and once done.
func readLiveFrom(r *rand.Rand, text string, at int, mode string) string {
	var p pendingText
	p.lead(text[:at])
	rest := text[at:]
	var pieces []string
	switch mode {
	case "whole":
		pieces = []string{rest}
	case "lines":
		lines := strings.SplitAfter(rest, "\n")
		for i := 0; i < len(lines); {
			k := 1 + r.IntN(max(1, len(lines)/3))
			pieces = append(pieces, strings.Join(lines[i:min(i+k, len(lines))], ""))
			i += k
		}
	default:
		for i := 0; i < len(rest); {
			k := 1 + r.IntN(8192)
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

// followFrom is what the error stream's reading shows of text through
// [Runner.Follow] over a fake box, its window opening on the line at at.
func followFrom(t *testing.T, text string, at int) string {
	t.Helper()
	runner := NewClaudeCode()
	b := sandbox.NewFakeSandbox("box-far-back")
	if err := runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	p := PathsFor(b)
	reading := runner.Follow(sandbox.RunHandle{})
	first := at - 1 + liveWindow
	var shown strings.Builder
	for _, n := range []int{first, first + (len(text)-first)/2, len(text)} {
		b.Put(p.Err(), text[:n])
		got, err := reading.Read(t.Context(), b)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		shown.WriteString(got.Text)
	}
	b.Put(p.Done(), "0")
	got, err := reading.Read(t.Context(), b)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return shown.String() + got.Text
}

// A READING HOLDS THE REDACTION'S LOOK-BACK BEFORE ITS FIRST LINE
// ([redact.LookBack]: a key's armour and the stages' reach). The live view in
// three piecings and the error stream's reading through [Runner.Follow] each
// show the end of the whole stream's redaction and the log after it, as a
// failure's tail, read from the stream's start, does.
//
// Mutation: hold a block's reach (redact.MaxKeyBlockBytes) as the context, or
// the keys' stage's reach alone, and the case goes red.
func TestAReadingHoldsTheRedactionsLookBackBeforeItsFirstLine(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, s farBackShape, whole, how, shown string) {
		t.Helper()
		if !strings.HasSuffix(whole, shown) || strings.Contains(shown, s.secret) || !strings.HasSuffix(shown, s.log) {
			t.Errorf("%s shows %d bytes opening %.60q; want the end of the whole redaction, through the log",
				how, len(shown), shown)
		}
	}
	for i, s := range readsFarBack() {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			whole := redact.Secrets(s.text)
			if strings.Contains(whole, s.secret) || !strings.HasSuffix(whole, s.log) {
				t.Fatalf("the whole redaction keeps %q; the shape needs one that takes it out", s.secret)
			}
			r := rand.New(rand.NewPCG(1635, uint64(i)))
			for _, mode := range []string{"whole", "lines", "pieces"} {
				check(t, s, whole, "the live view in "+mode, readLiveFrom(r, s.text, s.at, mode))
			}
			check(t, s, whole, "a failure's tail", failureTailOf(t, s.text, len(s.text)-s.at).shown)

			text := s.text + strings.Repeat("ok\n", liveWindow/3+1)
			check(t, s, redact.Secrets(text), "the error stream's reading", followFrom(t, text, s.at))
		})
	}
}
