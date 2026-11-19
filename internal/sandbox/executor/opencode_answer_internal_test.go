package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// openCodeText is one OpenCode text event of message's.
func openCodeText(t *testing.T, message, text string) []byte {
	t.Helper()
	line, err := json.Marshal(map[string]any{"type": "text", "part": map[string]any{"messageID": message, "type": "text", "text": text}})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// keptAndNote is a result's text split at the note a bound leaves.
func keptAndNote(text string) (string, string) {
	if at := strings.LastIndex(text, "\n("); at >= 0 {
		return text[:at], text[at+1:]
	}
	if strings.HasPrefix(text, "(") {
		return "", text
	}
	return text, ""
}

// AN OPENCODE ANSWER PAST ITS BOUND KEEPS ITS SETTLED OPENING: an answer, or a
// run's plain output, longer than the bound a run's report is read to keeps
// its head only as far as its redaction settles, so what it keeps redacts to
// an opening of what the whole redacts to, and says how much more there was;
// prose keeps every whole line within the bound; and a later message lets the
// earlier one's chunks and counts go, so its own answer is kept whole.
//
// Mutation: keep counting a message's dropped chunks into the next message,
// and the later answer is dropped for the earlier one's length.
func TestAnOpenCodeAnswerPastItsBoundKeepsItsSettledOpening(t *testing.T) {
	t.Parallel()
	const bound = 4 << 10
	const keyLine = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
	key := append(append([]string{"build log"}, strings.Split(strings.Repeat(keyLine+"\n", 200), "\n")[:200]...),
		"-----END RSA PRIVATE KEY-----", "done")
	var prose []string
	for i := range 400 {
		prose = append(prose, strings.Repeat("word ", 4+i%7)+"end")
	}

	for name, lines := range map[string][]string{"a key's body before its END": key, "prose": prose} {
		whole := redact.Secrets(strings.Join(lines, "\n") + "\n")
		answer := &openCodeEvents{readBound: bound}
		plain := &openCodeEvents{readBound: bound}
		for _, l := range lines {
			answer.Line(openCodeText(t, "msg_1", l))
			plain.Line([]byte(l))
		}
		for read, d := range map[string]*openCodeEvents{"answer": answer, "plain output": plain} {
			kept, note := keptAndNote(d.Result().Text)
			if !strings.Contains(note, "not kept: past the 4 KiB") {
				t.Errorf("%s, %s: no note of what is not kept in %q", name, read, note)
			}
			if len(kept) > bound {
				t.Errorf("%s, %s: kept %d bytes, past the %d bound", name, read, len(kept), bound)
			}
			if shown := redact.Secrets(kept); !strings.HasPrefix(whole, shown) {
				t.Errorf("%s, %s: what is kept redacts otherwise than the whole: …%q", name, read, shown[max(0, len(shown)-200):])
			}
			if name == "prose" {
				n := 0
				for at := 0; at+len(lines[n])+1 <= bound; n++ {
					at += len(lines[n]) + 1
				}
				if want := strings.Join(lines[:n], "\n"); kept != want {
					t.Errorf("prose, %s: kept %d bytes, want the %d whole lines within the bound (%d bytes)", read, len(kept), n, len(want))
				}
			}
		}
	}

	d := &openCodeEvents{readBound: bound}
	for _, l := range prose {
		d.Line(openCodeText(t, "msg_1", l))
	}
	d.Line(openCodeText(t, "msg_2", "the later answer"))
	if got := d.Result().Text; got != "the later answer" {
		t.Errorf("a later message after an answer past the bound: %q, want the later answer alone", got)
	}
	if cap(d.answers) > 1 {
		t.Errorf("the later message's one chunk is held in a slice of %d, the earlier message's chunks kept reachable", cap(d.answers))
	}
}
