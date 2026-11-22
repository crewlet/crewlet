package executor

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// A FOLLOWED TEXT SHOWS NO VALUE A LATER LINE GIVES: a password's key whose
// value a reading of the key's string takes from a line below, the text
// followed a line at a time as a run's transcript is ([pendingText.settle]),
// shows what the whole text redacted shows, and so never that value.
//
// Mutation: hold a key in [redact.Settled] only for the reading that found its
// value, and the line holding the value is settled and shown before the line
// that decides it.
func TestAFollowedTextShowsNoValueALaterLineGives(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"{\"text\": \"> Password:\\n\"\\n\"}\n- \"hunter2x9\"}\n",
		"echo \"Password:\\n\"\\necho \"\necho \"hunter2x9\"}\n",
		"{\"log\": \"passwd: \\n\"\\n\"\\n\"}\n{\"log\": \"hunter2x9\\n\"}\n\n",
		"{\"text\": \"> Password:\\n\"\\n\"}\r\n- \"hunter2x9\"}\r\n",
		"\"passwd: \\n\"hunter2x9\nok\n",
	} {
		var p pendingText
		var shown strings.Builder
		for line := range strings.SplitAfterSeq(text, "\n") {
			p.add(line)
			shown.WriteString(p.settle(false))
		}
		shown.WriteString(p.settle(true))
		if got, want := shown.String(), redact.Secrets(text); got != want {
			t.Errorf("followed a line at a time:\ntext %q\n got %q\nwant %q", text, got, want)
		}
		if strings.Contains(shown.String(), "hunter2x9") && !strings.Contains(redact.Secrets(text), "hunter2x9") {
			t.Errorf("the value is shown:\ntext %q\n got %q", text, shown.String())
		}
	}
}
