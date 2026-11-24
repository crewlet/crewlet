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
// whole-line decode keeps raw (raw), one past the bound (past: after the
// request where a later line would leave the read undecided), or none
// (plain). One case a process under GODEBUG=gctrace=1: held is the largest
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
			b.Run(h.name+"_"+c.name, func(b *testing.B) { notesBenchRead(b, h, c.name == "past", c.big) })
		}
	}
}

// notesBenchHarness is how the bench writes a harness's record: the lines
// before and after the one it measures at, that one with its value quoted in a
// member the read passes over or in one a whole-line decode keeps raw, and the
// read; pastLast where a line past the bound with a later line after it leaves
// the read undecided, so the bench writes it last.
type notesBenchHarness struct {
	name          string
	before, after string
	skipped, raw  func(quoted string) string
	held          func(ctx context.Context, r io.Reader, takes []ServedTake) ([]string, error)
	pastLast      bool
}

// notesBenchInbox is the inbox id of the one note each record holds, and
// notesBenchTake the take a harness that names a take by its token
// ([claudeTakeToken]) holds it under.
const (
	notesBenchInbox = "inbox1"
	notesBenchTake  = "0f0e0d0c-0000-4000-8000-000000000001"
)

// notesBenchTokens is the bench's note where held, a harness's reading of the
// take tokens a record shows held, holds its take's.
func notesBenchTokens(held map[string]bool, err error) ([]string, error) {
	if err != nil || !held[claudeTakeToken(notesBenchTake)] {
		return nil, err
	}
	return []string{notesBenchInbox}, nil
}

// notesBenchHarnesses is every harness whose record a collection's read walks
// a line at a time.
func notesBenchHarnesses() []notesBenchHarness {
	return []notesBenchHarness{
		{name: KimiName, held: heldAlone(kimiNotesHeld),
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
		{name: CopilotName, held: heldAlone(copilotNotesHeld),
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
		{name: PiName, held: heldAlone(piNotesHeld),
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
		{name: GrokName, pastLast: true,
			held: func(ctx context.Context, r io.Reader, _ []ServedTake) ([]string, error) {
				return notesBenchTokens(grokHeld(ctx, r, nil))
			},
			before: `{"type":"user","content":[{"type":"text","text":` + benchStringOf(grokHookHeads[0]+
				"## A note from ada\n\nUse main.\n\n"+claudeTakeToken(notesBenchTake)+grokReminderTail) +
				`}],"synthetic_reason":"system_reminder"}`,
			after: `{"type":"assistant","content":"ok","model_id":"m"}`,
			skipped: func(q string) string {
				return `{"type":"tool_result","tool_call_id":"c1","content":"ok","pad":` + q + `}`
			},
			raw: func(q string) string { return `{"type":"tool_result","tool_call_id":"c1","content":` + q + `}` }},
		{name: MuseName, pastLast: true,
			held: func(ctx context.Context, r io.Reader, _ []ServedTake) ([]string, error) {
				return notesBenchTokens(museHeld(ctx, r, museHooksDigest,
					map[string]bool{claudeTakeToken(notesBenchTake): true}, nil))
			},
			before: `{"payload_type":"tool_batch.effect.terminal","payload":{"record":{"call_id":"c1",` +
				`"kind":"terminal"}}}` + "\n" + `{"payload_type":"runtime.session","payload":{"event":{"effects":` +
				`["context"],"event":"PostToolUse","hook_key":"` + museHookKey(museNoteHooks[0]) + `","kind":` +
				`"hook_run_terminal","origin":{"source_digest":"` + museHooksDigest + `"},"status":"completed",` +
				`"stderr":"` + claudeTakeToken(notesBenchTake) + `\n"}}}` + "\n" + `{"payload_type":` +
				`"runtime.session","payload":{"event":{"id":"hook:post_tool_use:c1:0","kind":` +
				`"context_block_updated","source":"runtime_hook","text":"a note"}}}`,
			after: `{"payload_type":"runtime.session","payload":{"event":{"kind":"model_completed"}}}`,
			skipped: func(q string) string {
				return `{"payload_type":"runtime.session","payload":{"kind":"x","record":{"kind":"x"},"pad":` + q + `}}`
			},
			raw: func(q string) string {
				return `{"payload_type":"runtime.session","payload":{"event":{"chunk":` + q + `,"kind":"output"}}}`
			}},
	}
}

// notesBenchRead measures one read of h's record around the line big writes,
// none for nil, past where it is past the bound.
func notesBenchRead(b *testing.B, h notesBenchHarness, past bool, big func() string) {
	file := filepath.Join(b.TempDir(), "record.jsonl")
	notesBenchWrite(b, h, past && h.pastLast, big, file)
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
// note and its request, or after them where last; the line is let go of on
// return, before the read.
func notesBenchWrite(b *testing.B, h notesBenchHarness, last bool, big func() string, file string) {
	lines := []string{h.before}
	if big != nil && !last {
		lines = append(lines, big())
	}
	lines = append(lines, h.after)
	if big != nil && last {
		lines = append(lines, big())
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		b.Fatal(err)
	}
}
