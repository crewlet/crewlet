package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// EVERY ITEM THE ENGINE RENDERS FITS ONE MUSE CODE HOOK ANSWER, whatever its
// words hold: a person's note, a late call's account at the engine's widest
// (4,505 UTF-16 units), a wake and another run's message, each of every C0
// and C1 control, U+2028 and U+2029, tabs or newlines, is handed within the
// 16,384 bytes Muse reads, with no character escaped past two bytes: the
// rendering draws each control as U+FFFD (prompts' carried), three bytes.
//
// Mutation: render an item without its controls replaced, and a late call of
// controls is handed at six bytes each, past the ceiling.
func TestEveryRenderedItemFitsAMuseHookAnswer(t *testing.T) {
	t.Parallel()
	var controls []rune
	for r := rune(0); r <= 0x9f; r++ {
		if (r < 0x20 && r != '\t' && r != '\n') || r >= 0x80 {
			controls = append(controls, r)
		}
	}
	controls = append(controls, ' ', ' ')
	words := map[string]func(n int) string{
		"controls": func(n int) string {
			var b strings.Builder
			for i := range n {
				b.WriteRune(controls[i%len(controls)])
			}
			return b.String()
		},
		"tabs":     func(n int) string { return strings.Repeat("\t", n) },
		"newlines": func(n int) string { return "x" + strings.Repeat("\n", n-2) + "x" },
	}
	label := provenance.Label{Source: provenance.SourceChatThread, Trust: provenance.TrustExternal}
	for name, of := range words {
		head := executor.UTF16Len(prompts.LateCallArrived("error: ", "deploy()", false))
		late := prompts.LateCallArrived("error: "+of(4505-head), "deploy()", false)
		if got := executor.UTF16Len(late); got != 4505 {
			t.Fatalf("%s: the late call's account is %d units, want 4505", name, got)
		}
		wake := steer.Wake{By: "ada", Conversation: "c", Label: label, Text: of(steer.MaxWakeRunes)}
		message := steer.Wake{By: "ada", FromRun: "r1", FromSeat: "dev", Label: label, Text: of(steer.MaxWakeRunes)}
		items := map[string]string{
			"late call":     late,
			"note":          prompts.SteerMessage("ada", of(steer.MaxNoteRunes)),
			"wake":          prompts.WakeMessage(wake, provenance.NewBoundary(), "c"),
			"run's message": prompts.WakeMessage(message, provenance.NewBoundary(), ""),
		}
		for kind, item := range items {
			for _, event := range executor.MuseHookEvents {
				handed := executor.MuseHanded(t, event, item)
				if len(handed) > executor.MuseHookAnswerBytes || strings.Contains(handed, `\u`) {
					t.Errorf("%s of %s at %s: handed %d bytes (escaped %v), past %d", kind, name, event,
						len(handed), strings.Contains(handed, `\u`), executor.MuseHookAnswerBytes)
				}
			}
		}
	}
}
