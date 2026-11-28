//go:build unix

package executor

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkACollectionAtItsBounds measures what one collection ([Runner.Collect])
// holds at once with each piece near [sandbox.MaxFileBytes], the figure
// [sandbox.BoxReadHeldBytes] states. A case is a real local box holding a
// harness's files: Claude Code's stream (the final message's line, then the
// result line) and the result file `tail -n 1` copies, or OpenCode's stream;
// each text holds a credential, so redaction rewrites it. An `_and_errors`
// case adds an error stream of [benchCollectErrBytes] its failure shows.
// held-MiB is the most heap in use above the heap before, sampled throughout
// at GOGC=1, so it counts what is live; x-input is that over the bytes the box
// held, x-file over the file bound.
//
//	go test -run '^$' -bench '^BenchmarkACollectionAtItsBounds$/^<case>$' -benchtime 1x ./internal/sandbox/executor/
func BenchmarkACollectionAtItsBounds(b *testing.B) {
	// files is what a case's box holds: OpenCode's stream is its result file.
	type files struct{ stream, result, findings, exit, errs string }
	// Each case's files are made inside it, written and dropped before it
	// measures, so the heap it measures against holds none of them.
	text := func() string { return benchCollectText(benchCollectTextBytes, "\n") }
	line := func() string { return benchCollectText(benchCollectTextBytes, " ") }
	errs := func() string { return benchCollectText(benchCollectErrBytes, "\n") }
	result := func(b *testing.B, failed string) string {
		return `{"type":"result","subtype":"success","is_error":` + failed + `,"result":` + benchQuoted(b, text()) +
			`,"session_id":"9a8b7c6d-0000-4000-8000-000000000002","num_turns":3,"total_cost_usd":0.5,` +
			`"usage":{"input_tokens":10,"output_tokens":20}}`
	}
	claude := func(failed string, findings bool) func(b *testing.B) files {
		return func(b *testing.B) files {
			last := result(b, failed)
			f := files{stream: `{"type":"assistant","message":{"id":"msg_01","type":"message","role":"assistant",` +
				`"model":"claude-sonnet-4-5","content":[{"type":"text","text":` + benchQuoted(b, text()) + `}],` +
				`"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":8}},"parent_tool_use_id":null,` +
				`"session_id":"9a8b7c6d-0000-4000-8000-000000000002"}` + "\n" + last + "\n",
				result: last + "\n", exit: "0"}
			if findings {
				f.findings = text()
			}
			return f
		}
	}
	answer := func(findings bool) func(b *testing.B) files {
		return func(b *testing.B) files {
			f := files{result: `{"type":"text","part":{"messageID":"msg_01","text":` + benchQuoted(b, text()) + `}}` + "\n",
				exit: "0"}
			if findings {
				f.findings = text()
			}
			return f
		}
	}
	for _, bc := range []struct {
		name   string
		runner func() *Runner
		files  func(b *testing.B) files
	}{
		{"cc_result", NewClaudeCode, claude("false", false)},
		{"cc_failed_result", NewClaudeCode, claude("true", false)},
		{"cc_unparseable", NewClaudeCode, func(*testing.B) files {
			l := line() + "\n"
			return files{stream: l, result: l, exit: "1"}
		}},
		{"cc_result_and_findings", NewClaudeCode, claude("false", true)},
		{"cc_failed_result_and_findings", NewClaudeCode, claude("true", true)},
		{"oc_answer", NewOpenCode, answer(false)},
		{"oc_plain", NewOpenCode, func(*testing.B) files { return files{result: line() + "\n", exit: "1"} }},
		{"oc_answer_and_findings", NewOpenCode, answer(true)},
		{"cc_unparseable_and_errors", NewClaudeCode, func(*testing.B) files {
			l := line() + "\n"
			return files{stream: l, result: l, exit: "1", errs: errs()}
		}},
		{"oc_plain_and_errors", NewOpenCode, func(*testing.B) files {
			return files{result: line() + "\n", exit: "1", errs: errs()}
		}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			runner := bc.runner()
			local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: b.TempDir()})
			if err != nil {
				b.Fatalf("NewLocal: %v", err)
			}
			box, err := local.Create(b.Context(), sandbox.Spec{})
			if err != nil {
				b.Fatalf("Create: %v", err)
			}
			b.Cleanup(func() { _ = box.Close(b.Context()) })
			p := PathsFor(box)
			f := bc.files(b)
			input := 0
			for at, body := range map[string]string{p.Stream(): f.stream, p.Result(): f.result,
				p.Findings(): f.findings, p.ExitCode(): f.exit, p.Err(): f.errs} {
				if body == "" {
					continue
				}
				if len(body) > sandbox.MaxFileBytes && at != p.Stream() && at != p.Err() {
					b.Fatalf("the case's %s is %d bytes, past the %d a box reads whole", at, len(body),
						sandbox.MaxFileBytes)
				}
				if err := box.WriteFile(b.Context(), at, []byte(body)); err != nil {
					b.Fatalf("write %s: %v", at, err)
				}
				input += len(body)
			}
			var held uint64
			b.ResetTimer()
			for range b.N {
				var got sandbox.Result
				held = max(held, benchHeldDuring(func() {
					got, err = runner.Collect(b.Context(), box, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
				}))
				if err != nil {
					b.Fatalf("Collect: %v", err)
				}
				if got.Text == "" && got.Error == "" {
					b.Fatal("the collection read nothing of the case")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(held)/(1<<20), "held-MiB")
			b.ReportMetric(float64(held)/float64(input), "x-input")
			b.ReportMetric(float64(held)/sandbox.MaxFileBytes, "x-file")
			b.ReportMetric(float64(input)/(1<<20), "input-MiB")
		})
	}
}

// benchCollectTextBytes is each case's text: what a line holding it quoted
// leaves of [sandbox.MaxFileBytes] for the line's own keys, with room to spare.
const benchCollectTextBytes = sandbox.MaxFileBytes - sandbox.MaxFileBytes/64 - 64<<10

// benchCollectErrBytes is a case's error stream: twice what a box reads whole,
// so its read holds a redaction stream at its stages' windows, never the text.
const benchCollectErrBytes = 2 * sandbox.MaxFileBytes

// benchCollectText is n bytes of a report, numbered lines of words each ending
// in sep, with one credential the redaction knows near its end.
func benchCollectText(n int, sep string) string {
	var key string
	for _, s := range redacttest.Shapes() {
		if s.Name == "anthropic-key" {
			key = s.Sample
		}
	}
	const words = "the harness read the change and wrote down what it found there"
	var s strings.Builder
	s.Grow(n)
	tail := "the key it used was " + key + sep
	for i := 0; s.Len()+len(words)+16+len(tail) <= n; i++ {
		fmt.Fprintf(&s, "line %07d: %s%s", i, words, sep)
	}
	s.WriteString(tail)
	return s.String()
}

// benchQuoted is text as a JSON string.
func benchQuoted(b *testing.B, text string) string {
	b.Helper()
	out, err := json.Marshal(text)
	if err != nil {
		b.Fatal(err)
	}
	return string(out)
}

// benchHeldDuring is the most heap in use at once while f runs, above the
// heap in use before it: read throughout by a goroutine of its own, at GOGC=1
// so what is no longer reachable is soon collected and the reading is of what
// f holds, not what it left.
func benchHeldDuring(f func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	runtime.GC()
	metrics.Read(sample)
	before := sample[0].Value.Uint64()
	var stop atomic.Bool
	most := make(chan uint64)
	go func() {
		s := []metrics.Sample{{Name: sample[0].Name}}
		top := before
		for !stop.Load() {
			metrics.Read(s)
			top = max(top, s[0].Value.Uint64())
			runtime.Gosched()
		}
		most <- top
	}()
	f()
	stop.Store(true)
	return <-most - before
}
