package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"
)

// BenchmarkACollectionsReadAtItsBounds measures a collection's read of a
// session's record for the receipts of the notes its run served, each harness
// whose record such a read walks a line at a time here ([notesBenchHarnesses]),
// against the base tier it runs in ([baseTierReadBytes]): a record holding a
// note and the request that carried it, around one line of plain words at the
// line bound in a member the read passes over (skipped), one in a member a
// whole-line decode keeps raw (raw), one past the bound (past; before the
// note where such a line drops a pending one), or none (plain). One case a process under GODEBUG=gctrace=1: held is the largest
// live heap at GOGC=10. A read that holds no note fails.
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkACollectionsReadAtItsBounds$/^<case>$' -benchtime 1x
func BenchmarkACollectionsReadAtItsBounds(b *testing.B) {
	line := maxLineBytes - 128<<10
	for _, h := range notesBenchHarnesses() {
		for _, c := range []struct {
			name string
			big  func() string
		}{
			{"plain", nil},
			{"skipped", func() string { return h.skipped(personBenchQuoted("plain", line)) }},
			{"raw", func() string { return h.raw(personBenchQuoted("plain", line)) }},
			{"past", func() string { return h.skipped(personBenchQuoted("plain", 2*maxLineBytes)) }},
		} {
			first := c.name == "past" && h.pastFirst
			b.Run(h.name+"_"+c.name, func(b *testing.B) { notesBenchRead(b, h, c.big, first) })
		}
	}
}

// notesBenchHarness is how the bench writes a harness's record: the lines
// before and after the one it measures at, that one with its value quoted in a
// member the read passes over or in one a whole-line decode keeps raw, and the
// read; pastFirst writes a line past the bound before the note, as one
// between drops it.
type notesBenchHarness struct {
	name          string
	pastFirst     bool
	before, after string
	skipped, raw  func(quoted string) string
	held          func(ctx context.Context, r io.Reader, takes []ServedTake) ([]string, error)
}

// notesBenchInbox is the inbox id of the one note each record holds.
const notesBenchInbox = "inbox1"

// notesBenchHarnesses is every harness whose record a collection's read walks
// a line at a time.
func notesBenchHarnesses() []notesBenchHarness {
	return []notesBenchHarness{
		{name: KimiName, held: kimiNotesHeld,
			before: `{"type":"context.append_message","message":{"role":"user","content":[],"id":"` +
				KimiNoteID(notesBenchInbox) + `"}}`,
			after: `{"type":"usage.record","usageScope":"turn"}`,
			skipped: func(q string) string {
				return `{"type":"context.append_loop_event","time":1,"event":{"type":"tool.result",` +
					`"toolCallId":"c1","result":{"output":` + q + `,"isError":false}}}`
			},
			raw: func(q string) string {
				return `{"type":"context.append_message","message":{"role":"user","id":"m1","content":` +
					`[{"type":"text","text":` + q + `}]}}`
			}},
		{name: CopilotName, held: copilotNotesHeld,
			before: `{"type":"user.message","timestamp":"2026-01-01T00:00:00.000Z","data":{"clientTurnId":"` +
				CopilotClientTurnID(notesBenchInbox) + `","delivery":"steering","content":"a note"}}`,
			after: `{"type":"session.usage_record","timestamp":"2026-01-01T00:00:01.000Z","data":{"usage":` +
				`{"interactionType":"` + copilotMainRequest + `","duration":500}}}`,
			skipped: func(q string) string {
				return `{"type":"tool.execution_complete","timestamp":"2026-01-01T00:00:00.100Z","data":` +
					`{"toolCallId":"c1","success":true,"result":{"content":` + q + `}}}`
			},
			raw: func(q string) string {
				return `{"type":"hook.end","timestamp":"2026-01-01T00:00:00.100Z","data":` +
					`{"hookInvocationId":"h1","hookType":"postToolUse","output":` + q + `}}`
			}},
		{name: PiName, held: piNotesHeld,
			before: `{"type":"custom_message","id":"n1","parentId":"","customType":"` + piNoteType +
				`","content":"a note","details":{"inbox":"` + notesBenchInbox + `"}}`,
			after: `{"type":"message","id":"a1","parentId":"n1","message":{"role":"assistant","content":[],` +
				`"stopReason":"stop"}}`,
			skipped: func(q string) string {
				return `{"type":"message","id":"m1","parentId":"n1","note":` + q + `}`
			},
			raw: func(q string) string {
				return `{"type":"message","id":"m1","parentId":"n1","message":{"role":"toolResult",` +
					`"toolCallId":"c1","content":[{"type":"text","text":` + q + `}]}}`
			}},
		{name: QwenCodeName, held: qwenHeldOf(qwenNotesHeld), pastFirst: true,
			before: `{"type":"user","subtype":"mid_turn_user_message","message":{"role":"user","parts":[{"text":` +
				benchStringOf("\n"+qwenNoteFraming+notesBenchInbox) + `}]}}`,
			after: `{"type":"assistant","message":{"role":"model","parts":[{"text":"ok"}]}}`,
			skipped: func(q string) string {
				return `{"type":"tool_result","message":{"role":"user","parts":[{"functionResponse":{"id":"c1",` +
					`"name":"run_shell_command","response":{"output":` + q + `}}}]}}`
			},
			raw: func(q string) string {
				return `{"type":"system","subtype":"slash_command","systemPayload":{"output":` + q + `}}`
			}},
		{name: GeminiCLIName, held: geminiHeldOf(geminiConfirmed),
			before: `{"id":"u1","type":"user","content":[{"text":"` + notesBenchInbox + `"}]}`,
			after:  `{"id":"m1","type":"gemini","content":"ok"}`,
			skipped: func(q string) string {
				return `{"id":"m1","type":"gemini","content":"","toolCalls":[{"id":"c1","name":"x","args":{},` +
					`"result":[{"functionResponse":{"response":{"output":` + q + `}}}]}]}`
			},
			raw: func(q string) string { return `{"id":"m1","type":"gemini","content":` + q + `}` }},
	}
}

// notesBenchRead measures one read of h's record around the line big writes,
// none for nil.
func notesBenchRead(b *testing.B, h notesBenchHarness, big func() string, first bool) {
	file := filepath.Join(b.TempDir(), "record.jsonl")
	notesBenchWrite(b, h, big, first, file)
	takes := []ServedTake{{InboxID: notesBenchInbox}}
	runtime.GC()
	debug.FreeOSMemory()
	fmt.Fprintf(os.Stderr, "collection-read-bench %s: the read starts\n", b.Name())
	b.ResetTimer()
	for range b.N {
		f, err := os.Open(file)
		if err != nil {
			b.Fatal(err)
		}
		start := time.Now()
		held, err := h.held(b.Context(), f, takes)
		elapsed := time.Since(start) //fixedwait:measure the read's time is what the case reports
		_ = f.Close()
		fmt.Fprintf(os.Stderr, "collection-read-bench %s: the read ends in %s, held %v\n", b.Name(), elapsed, held)
		if err != nil || !slices.Equal(held, []string{notesBenchInbox}) {
			b.Fatalf("the read held %v (%v), want its note", held, err)
		}
	}
	b.StopTimer()
}

// notesBenchWrite writes h's record to file, the line big writes between the
// note and its request, or before the note (first); the line is let go of on
// return, before the read.
func notesBenchWrite(b *testing.B, h notesBenchHarness, big func() string, first bool, file string) {
	var lines []string
	if big != nil && first {
		lines = append(lines, big())
	}
	lines = append(lines, h.before)
	if big != nil && !first {
		lines = append(lines, big())
	}
	lines = append(lines, h.after)
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		b.Fatal(err)
	}
}
