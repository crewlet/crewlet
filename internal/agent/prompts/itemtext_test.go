package prompts

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/provenance"
)

// itemControls is every character an item draws as U+FFFD ([carried]): each
// control character but tab and newline (DEL included), U+2028 and U+2029.
func itemControls() []rune {
	var out []rune
	for r := rune(0); r <= 0x9f; r++ {
		if (r < 0x20 && r != '\t' && r != '\n') || r >= 0x7f {
			out = append(out, r)
		}
	}
	return append(out, ' ', ' ')
}

// writtenAndDrawn is text holding every character from U+0000 to U+009F,
// U+2028 and U+2029, between two marks no renderer trims, and that text as an
// item shows it.
func writtenAndDrawn() (written, drawn string) {
	var w, d strings.Builder
	w.WriteString("<<")
	d.WriteString("<<")
	replaced := map[rune]bool{}
	for _, r := range itemControls() {
		replaced[r] = true
	}
	for r := rune(0); r <= 0x9f; r++ {
		w.WriteRune(r)
		if replaced[r] {
			d.WriteRune('�')
		} else {
			d.WriteRune(r)
		}
	}
	w.WriteString("  >>")
	d.WriteString("��>>")
	return w.String(), d.String()
}

// EVERY ITEM KEEPS TO CHARACTERS EVERY HARNESS CARRIES: a person's note (and
// its sender), a late call's account and its error line, a wake and another
// run's message each come out with every control character but tab and
// newline, U+2028 and U+2029 drawn as U+FFFD, and every other character as
// written, tab and newline included.
//
// Mutation: leave carriage return, DEL, a C1 control or U+2028 as it is, or draw
// tab or newline as U+FFFD.
func TestEveryItemKeepsToCharactersEveryHarnessCarries(t *testing.T) {
	t.Parallel()
	written, drawn := writtenAndDrawn()
	b := provenance.NewBoundary()
	label := provenance.Label{Source: provenance.SourceChatThread, Trust: provenance.TrustExternal}
	items := map[string]string{
		"note":          SteerMessage("ada"+written, written),
		"note before":   SteerMessageSentBefore("ada"+written, written),
		"late call":     LateCallArrived("error: "+written, "deploy("+written+")", false),
		"late submit":   LateCallArrived("success", "submit_work("+written+")", true),
		"wake":          WakeMessage(steer.Wake{By: "ada", Conversation: "chat:C1", Label: label, Text: written}, b, "chat:C1"),
		"run's message": WakeMessage(steer.Wake{By: "ada", FromRun: "r1", FromSeat: "dev", Label: label, Text: written}, b, ""),
	}
	for kind, item := range items {
		for _, r := range itemControls() {
			if strings.ContainsRune(item, r) {
				t.Errorf("%s: holds %U", kind, r)
			}
		}
		if !strings.Contains(item, drawn) {
			t.Errorf("%s: does not show the text with exactly its controls replaced:\n%q", kind, item)
		}
	}
}

// THE WIDEST ITEM FITS A HOOK'S 16,384 BYTES: a late call's account of 4,505
// UTF-16 units (the engine's item bound), its error line all controls,
// U+2028 and U+2029 that are three bytes each as U+FFFD, JSON-encoded with
// only the escapes JSON requires inside the widest hook answer's framing,
// stays within 16,384 bytes, the least any harness reads of a hook.
//
// Mutation: hand the controls over as written, which JSON escapes at six
// bytes each.
func TestTheWidestItemFitsAHooksBytesOnceEncoded(t *testing.T) {
	t.Parallel()
	const units, ceiling = 4505, 16384
	head := LateCallArrived("error: ", "deploy()", false)
	controls := itemControls()
	var pad strings.Builder
	for i := 0; len(utf16.Encode([]rune(head)))+i < units; i++ {
		pad.WriteRune(controls[i%len(controls)])
	}
	item := LateCallArrived("error: "+pad.String(), "deploy()", false)
	if got := len(utf16.Encode([]rune(item))); got != units {
		t.Fatalf("the item is %d units, want %d", got, units)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "PostToolUse", "additionalContext": item}}); err != nil {
		t.Fatal(err)
	}
	if n := len(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))); n > ceiling || bytes.Contains(buf.Bytes(), []byte(`\u`)) {
		t.Fatalf("the answer is %d bytes (escaped %v), past %d", n, bytes.Contains(buf.Bytes(), []byte(`\u`)), ceiling)
	}
}
