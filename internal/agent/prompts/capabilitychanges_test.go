package prompts

import (
	"strings"
	"testing"
)

// THE ACCESS CHANGES ARE TOLD AFTER THE CONVERSATION'S ENDINGS AND BEFORE THE
// ASK, as their own section, and a turn with none carries no trace of them.
func TestTheAccessChangesAreToldBeforeTheTask(t *testing.T) {
	t.Parallel()
	changes := "## What changed since your last turn\n- Cloudflare: your Crewlet grant now sets your level to Secure (was Operate)."
	m := BuildPhaseUserMessage(UserMessage{TaskDescription: "Purge the cache.",
		Endings: endingsHeader + "\n\n### Turn t-1", CapabilityChanges: changes})
	endings, told, task := strings.Index(m.Text, endingsHeader), strings.Index(m.Text, changes),
		strings.Index(m.Text, taskHeader)
	if endings < 0 || told < endings || task < told {
		t.Fatalf("order is wrong (endings %d, changes %d, task %d):\n%s", endings, told, task, m.Text)
	}
	if !strings.Contains(m.Text, changes+"\n\n"+taskHeader) {
		t.Errorf("the changes are not their own section before the task:\n%s", m.Text)
	}
	plain := BuildPhaseUserMessage(UserMessage{TaskDescription: "Purge the cache."})
	if plain.Text != taskHeader+"\nPurge the cache." {
		t.Errorf("a turn with no change moved: %q", plain.Text)
	}
}
