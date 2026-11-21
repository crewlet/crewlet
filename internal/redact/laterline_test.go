package redact_test

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// laterLineWraps are a line's wrapping, opening and closing: strings,
// concatenations, logged JSON, an echo, a list, a table, a quote, a reader's
// number, a timestamp, a diff, a comment.
var laterLineWraps = [][2]string{
	{`"`, `\n" +`}, {`"`, `\n",`}, {`"`, `"`}, {`"`, `" +`}, {`{"text": "> `, `"}`}, {`{"text": "> `, `\n"}`},
	{`{"log": "`, `"}`}, {`{"log": "`, `\n"}`}, {`echo "`, `" >> k`}, {`echo "`, `"`}, {`echo "`, `\n" >> k`},
	{`- "`, `"`}, {`- "`, `\n"`}, {`| `, ` |`}, {`| `, ``}, {`> `, ``}, {`  `, ``}, {`1. `, ``}, {`# `, ``},
	{``, ``}, {`    10→`, ``}, {`+`, ``}, {`2026-11-21T10:00:00Z `, ``}, {`"`, `\n"`}, {`'`, `\n'`},
}

var (
	laterLineKeys = []string{
		`Password:`, `password:`, `passwd: `, `pwd=`, `Password: `, `password="`, `"password": "`, `PASSWORD=`,
		`Retype password:`, `db_password: `, `Password:\n`, `password = `, `Enter password`, `> Password:`,
	}
	laterLineKeyTails = []string{
		``, ``, ` `, `\n"`, `\n" +`, `\n",`, `\n"}`, `\n`, `\n\n`, `\r\n"`, `"`, `" >> k`, `"}`, ` |`,
		`\n" >> k`, `\n"\n"}`, `\n\"`, `\n"\n- "`, `\n"\necho "`, `\n" + "`, `\n\n"`, `\\n`, `\n"\n"`,
	}
	laterLineLeads = []string{`q8Zr4TfL2w== `, `user=bob `, `> `, `"`, `{"a": 1} `, `ok; `}
)

// laterLineTexts is n texts, each one or two keys whose value a line below may
// give (a key's line whose pieces hold no value, its wrapping's lines between,
// a value on a later line in the same or another wrapping), and the texts
// that showed such a value as they settled.
func laterLineTexts(n int) []string {
	texts := []string{
		"{\"text\": \"> Password:\\n\"\\n\"}\n- \"passwd: \n- \"hunter2x9\\n\"}\n",
		"\"passwd: \\n\"hunter2x9\nhunter2x9\" >> k\necho \"Password:\\n\"\\necho \"\necho \"hunter2x9\"}\n| Password:\n  hunter2x9\"}\n| Password:\n",
		"{\"text\": \"> Password:\\n\"\\n\"}\n- \"hunter2x9\"}\n",
		"echo \"Password:\\n\"\\necho \"\necho \"hunter2x9\"}\n",
		"{\"log\": \"passwd: \\n\"\\n\"\\n\"}\n{\"log\": \"placeholder9x\\n\"}\n\n",
	}
	r := rand.New(rand.NewPCG(1819, 1778))
	pick := func(s []string) string { return s[r.IntN(len(s))] }
	value := func() string {
		return pick([]string{"hunter2x9", "Xy7!pq9Lm", "placeholder9x", "s3cr3tPass", "P@ss!w0rd42"})
	}
	snippet := func(b *strings.Builder) {
		nl := "\n"
		if r.IntN(6) == 0 {
			nl = "\r\n"
		}
		w := laterLineWraps[r.IntN(len(laterLineWraps))]
		for range r.IntN(3) {
			b.WriteString(pick([]string{w[0] + "starting job" + w[1], w[0] + value() + w[1], w[0] + w[1], "ok"}))
			b.WriteString(nl)
		}
		b.WriteString(w[0])
		if r.IntN(5) == 0 {
			b.WriteString(pick(laterLineLeads))
		}
		b.WriteString(pick(laterLineKeys) + pick(laterLineKeyTails))
		if r.IntN(3) == 0 {
			b.WriteString(w[1])
		}
		b.WriteString(nl)
		lines, valued := 1+r.IntN(5), false
		for i := range lines {
			k := r.IntN(11)
			if i == lines-1 && !valued {
				k = 4
			}
			v := w
			if r.IntN(4) == 0 {
				v = laterLineWraps[r.IntN(len(laterLineWraps))]
			}
			switch k {
			case 1:
				b.WriteString(w[0] + w[1])
			case 2:
				b.WriteString(w[0] + `\n` + w[1])
			case 3:
				b.WriteString(w[0] + `"`)
			case 4:
				b.WriteString(v[0] + value() + v[1])
				valued = true
			case 5:
				b.WriteString(v[0] + "  " + value() + v[1])
				valued = true
			case 6:
				b.WriteString(v[0] + value() + `\n"`)
				valued = true
			case 7:
				b.WriteString(w[0] + "ok then" + w[1])
			case 8:
				b.WriteString(w[0] + pick(laterLineKeys) + " " + value() + w[1])
			case 9:
				b.WriteString(pick([]string{`"}`, `" +`, `",`, `" >> k`, `|`, `\n"`}))
			case 10:
				b.WriteString(v[0] + `\"` + value() + `\"` + v[1])
				valued = true
			}
			if r.IntN(8) == 0 {
				b.WriteString(nl)
			}
			if i < lines-1 || r.IntN(12) != 0 {
				b.WriteString(nl)
			}
		}
	}
	for range n {
		var b strings.Builder
		snippet(&b)
		if r.IntN(5) == 0 {
			snippet(&b)
		}
		texts = append(texts, b.String())
	}
	return texts
}

// A KEY WHOSE VALUE A LATER LINE GIVES HOLDS THAT LINE AS IT SETTLES: over keys
// whose value a reading of the key's string can take from a line below, what is
// shown as the text settles, asked at every line's end and in pieces of any
// size, is the whole text redacted ([redact.Settled]'s promise: the text past
// what it settles reads as one that starts there).
//
// Mutation: hold a key only for the reading that found its value, and a text cut
// before the line a second reading takes the value from settles that line, and
// the value is shown.
func TestAKeyWhoseValueALaterLineGivesHoldsThatLineAsItSettles(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1819, 7))
	for i, text := range laterLineTexts(3000) {
		want := redact.Secrets(text)
		for _, sizes := range [][]int{{1}, {1 + r.IntN(40), 1 + r.IntN(200)}} {
			if got, _ := settle(text, sizes); got != want {
				t.Fatalf("text %d, in pieces of %v:\ntext %q\n got %q\nwant %q", i, sizes, text, got, want)
			}
		}
	}
}

// A LIVE STREAM WRITES A VALUE A LATER LINE GIVES AS THE WHOLE TEXT DOES: the
// same texts written a line at a time to a [redact.NewLiveStream], flushed at
// every line's end, write [redact.Secrets], and Close answers nil.
//
// Mutation: as [TestAKeyWhoseValueALaterLineGivesHoldsThatLineAsItSettles]'s,
// and a flush writes the value's line before the line that decides it.
func TestALiveStreamWritesAValueALaterLineGivesAsTheWholeTextDoes(t *testing.T) {
	t.Parallel()
	for i, text := range laterLineTexts(3000) {
		var out bytes.Buffer
		s := redact.NewLiveStream(&out)
		for line := range strings.SplitAfterSeq(text, "\n") {
			_, _ = s.Write([]byte(line))
			if _, _, err := s.Flush(); err != nil {
				t.Fatalf("text %d: Flush: %v", i, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatalf("text %d: Close: %v\ntext %q", i, err, text)
		}
		if got, want := out.String(), redact.Secrets(text); got != want {
			t.Fatalf("text %d:\ntext %q\n got %q\nwant %q", i, text, got, want)
		}
	}
}
