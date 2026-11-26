package executor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkAChatCountAtItsBounds measures Qwen Code's and Gemini CLI's count:
// the take's meter (meter) and a collection's of the box's copy, alone (copy)
// or with its summary read on the same walk (summary), against [meterBaseHeldBytes] and [baseTierReadBytes]. A line just within the
// bound passed over (skipped), in words, an answer or a call's arguments, past
// it (past, failing the read), or none (plain); the most ids a count keeps, as
// uuids (ids) and at the word bound (ids_long), and Gemini CLI's at its base
// beside a line of as many (ids_base). One case a process: held is the most
// live heap at GOGC=10 under gctrace, held-MiB as each GC marks it. An unread
// count fails but past the bound; an answer or Qwen Code's calls at it read masked.
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAChatCountAtItsBounds$/^<case>$' -benchtime 1x
func BenchmarkAChatCountAtItsBounds(b *testing.B) {
	line := codexRolloutLineMaxBytes - 128<<10
	q := func(n int) string { return personBenchQuoted("plain", n) }
	for _, h := range chatBenchHarnesses() {
		for _, c := range []struct {
			name string
			big  func() string
		}{
			{"plain", nil},
			{"skipped", func() string { return h.skipped(q(line)) }},
			{"words", func() string { return h.words(q(line)) }},
			{"answer", func() string { return h.answer(q(line)) }},
			{"call", func() string { return h.call(q(line)) }},
			{"past", func() string { return h.skipped(q(2 * codexRolloutLineMaxBytes)) }},
			{"ids", func() string { return h.ids(36) }},
			{"ids_long", func() string { return h.ids(transcriptNameMaxBytes) }},
			{"ids_base", h.idsBase},
		} {
			if c.big == nil && c.name != "plain" {
				continue
			}
			for _, how := range []string{"meter", "copy", "summary"} {
				b.Run(h.name+"_"+how+"_"+c.name, func(b *testing.B) { chatBenchRead(b, h, how, c.name == "past", c.big) })
			}
		}
	}
}

// chatBenchHarness is how the bench writes a harness's chat file: its counter,
// the line that anchors its count, and each case's line around a quoted value,
// or the most ids it keeps of length n; and how its count reads the newest
// summary on its walk (summarize).
type chatBenchHarness struct {
	name                         string
	counter                      func() chatCounter
	member                       string
	isMember                     func(string) bool
	release                      string
	anchor                       string
	skipped, words, answer, call func(quoted string) string
	ids                          func(n int) string
	idsBase                      func() string
	summarize                    func(c chatCounter)
}

// chatBenchID is the i'th id of n bytes.
func chatBenchID(i, n int) string {
	id := fmt.Sprintf("%08d", i)
	return id + strings.Repeat("-", n-len(id))
}

// chatBenchHarnesses is Qwen Code's and Gemini CLI's.
func chatBenchHarnesses() []chatBenchHarness {
	return []chatBenchHarness{
		{name: QwenCodeName, counter: func() chatCounter { return new(qwenCounting) }, member: qwenMember.Name,
			isMember: qwenChatMember, release: qwenRelease,
			anchor: `{"uuid":"a","type":"assistant","message":{"role":"model","parts":[{"text":"ok"}]},` +
				`"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":10,"totalTokenCount":1010}}`,
			skipped: func(q string) string {
				return `{"type":"system","subtype":"slash_command","systemPayload":{"output":` + q + `}}`
			},
			words: func(q string) string {
				return `{"type":"user","message":{"role":"user","parts":[{"text":` + q + `}]}}`
			},
			answer: func(q string) string {
				return `{"type":"tool_result","message":{"role":"user","parts":[{"functionResponse":{"id":"c1",` +
					`"name":"run_shell_command","response":{"output":` + q + `}}}]}}`
			},
			call: func(q string) string {
				return `{"type":"assistant","message":{"role":"model","parts":[{"functionCall":{"id":"c1",` +
					`"name":"run_shell_command","args":{"command":` + q + `}}}]}}`
			},
			summarize: func(c chatCounter) {
				c.(*qwenCounting).summarize(newSummaryClaim(context.Background(), new(passBudget)))
			},
			ids: func(n int) string {
				parts := make([]string, qwenCountIDs)
				for i := range parts {
					parts[i] = `{"functionCall":{"id":"` + chatBenchID(i, n) + `","name":"n","args":{}}}`
				}
				return `{"type":"assistant","message":{"role":"model","parts":[` + strings.Join(parts, ",") + `]}}`
			}},
		{name: GeminiCLIName, counter: func() chatCounter { return newGeminiCounting() }, member: geminiMember.Name,
			isMember: geminiChatMember, release: geminiRelease,
			anchor:  `{"id":"a","type":"user","content":[{"text":"hello"}]}`,
			skipped: func(q string) string { return `{"$set":{"lastUpdated":"t","pad":` + q + `}}` },
			words: func(q string) string {
				return `{"id":"w","type":"user","content":[{"text":` + q + `}]}`
			},
			answer: func(q string) string {
				return `{"id":"m","type":"gemini","content":"","toolCalls":[{"id":"c1","name":"run_shell_command",` +
					`"args":{},"result":[{"functionResponse":{"id":"c1","name":"run_shell_command","response":` +
					`{"output":` + q + `}}}]}]}`
			},
			call: func(q string) string {
				return `{"id":"m","type":"gemini","content":"","toolCalls":[{"id":"c1","name":"run_shell_command",` +
					`"args":{"command":` + q + `}}]}`
			},
			ids: func(n int) string {
				// One record a line, the count's ids to its bound, the anchor's among them.
				lines := make([]string, geminiCountIDs-1)
				for i := range lines {
					lines[i] = `{"id":"` + chatBenchID(i, n) + `","type":"user","content":"x"}`
				}
				return strings.Join(lines, "\n")
			},
			summarize: func(c chatCounter) {
				c.(*geminiCounting).summarize(newSummaryClaim(context.Background(), new(passBudget)))
			},
			idsBase: func() string {
				lines := make([]string, geminiCountBaseIDs-1)
				for i := range lines {
					lines[i] = `{"id":"` + chatBenchID(i, transcriptNameMaxBytes) + `","type":"user","content":"x"}`
				}
				parts := make([]string, geminiCountBaseIDs-1)
				for i := range parts {
					parts[i] = `{"functionCall":{"id":"` + chatBenchID(geminiCountBaseIDs+i, transcriptNameMaxBytes) +
						`","name":"n"}}`
				}
				return strings.Join(lines, "\n") + "\n" + `{"id":"` + chatBenchID(1<<20, transcriptNameMaxBytes) +
					`","type":"gemini","content":[` + strings.Join(parts, ",") + `]}`
			}},
	}
}

// chatBenchRead measures one count of h's chat file around the line big
// writes, none for nil, as the take's meter reads its archive or as a
// collection reads the box's copy; past where it fails the read.
func chatBenchRead(b *testing.B, h chatBenchHarness, how string, past bool, big func() string) {
	file := filepath.Join(b.TempDir(), "chat")
	archive := how == "meter"
	chatBenchWrite(b, h, archive, big, file)
	runtime.GC()
	debug.FreeOSMemory()
	fmt.Fprintf(os.Stderr, "chat-count-bench %s: the read starts\n", b.Name())
	held := watchLiveHeap()
	b.ResetTimer()
	for range b.N {
		f, err := os.Open(file)
		if err != nil {
			b.Fatal(err)
		}
		start := time.Now()
		c := h.counter()
		var read error
		if how == "summary" {
			h.summarize(c)
		}
		if archive {
			_, read = chatMeter(b.Context(), c, f, h.isMember, h.release, h.release, new(passBudget))
		} else {
			_, read = countChat(h.release, h.release, func() error {
				return chatFileLines(b.Context(), f, c, new(passBudget))
			})
		}
		count := c.count(0, 0)
		elapsed := time.Since(start) //fixedwait:measure the read's time is what the case reports
		_ = f.Close()
		fmt.Fprintf(os.Stderr, "chat-count-bench %s: the read ends in %s, count %s\n", b.Name(), elapsed,
			countString(count))
		switch {
		case past && read == nil:
			b.Fatalf("the count read %s, want it failed past the bound", countString(count))
		case !past && (read != nil || count.Mark == sandbox.CountUnread):
			b.Fatalf("the count read %s (%v), want it read", countString(count), read)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(held.stop())/(1<<20), "held-MiB")
}

// chatBenchWrite writes h's chat file to file, the anchor, then the line big
// writes, then the anchor again; packed as the take packs it where archive.
// The file is let go of on return, before the read.
func chatBenchWrite(b *testing.B, h chatBenchHarness, archive bool, big func() string, file string) {
	lines := []string{h.anchor}
	if big != nil {
		lines = append(lines, big())
	}
	chat := []byte(strings.Join(append(lines, h.anchor), "\n") + "\n")
	if archive {
		chat = tarGzOf(b, h.member, chat)
	}
	if err := os.WriteFile(file, bytes.Clone(chat), 0o600); err != nil {
		b.Fatal(err)
	}
}
