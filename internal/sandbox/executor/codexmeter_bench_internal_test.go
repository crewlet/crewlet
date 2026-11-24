package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// BenchmarkACodexMeterAtItsBounds measures the take's meter of a Codex
// archive ([codexMeter]), which a collection's count reads too, against the
// meter's base ([meterBaseHeldBytes]): a token count, then one line of plain
// words just within the rollout's line bound in a member the count passes over
// (skipped), in a call's output (output), or past the bound (past, which fails
// the read), or none (plain), then a token count. One case a process under
// GODEBUG=gctrace=1: held is the largest live heap at GOGC=10. A read that
// reads no figure fails, but past the bound.
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkACodexMeterAtItsBounds$/^<case>$' -benchtime 1x
func BenchmarkACodexMeterAtItsBounds(b *testing.B) {
	line := codexRolloutLineMaxBytes - 128<<10
	for _, c := range []struct {
		name string
		big  func() string
	}{
		{"plain", nil},
		{"skipped", func() string {
			return `{"type":"event_msg","payload":{"type":"agent_message","message":` +
				personBenchQuoted("plain", line) + `}}`
		}},
		{"output", func() string {
			return `{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":` +
				personBenchQuoted("plain", line) + `}}`
		}},
		{"past", func() string {
			return `{"type":"event_msg","payload":{"type":"agent_message","message":` +
				personBenchQuoted("plain", 2*codexRolloutLineMaxBytes) + `}}`
		}},
	} {
		b.Run(c.name, func(b *testing.B) { codexMeterBenchRead(b, c.name == "past", c.big) })
	}
}

// codexMeterBenchRead measures one meter of the archive around the line big
// writes, none for nil, which fails the read where past.
func codexMeterBenchRead(b *testing.B, past bool, big func() string) {
	file := filepath.Join(b.TempDir(), "archive")
	codexMeterBenchWrite(b, big, file)
	runtime.GC()
	debug.FreeOSMemory()
	fmt.Fprintf(os.Stderr, "codex-meter-bench %s: the read starts\n", b.Name())
	b.ResetTimer()
	for range b.N {
		f, err := os.Open(file)
		if err != nil {
			b.Fatal(err)
		}
		start := time.Now()
		read, err := codexMeter(b.Context(), f, codexCarryRelease)
		elapsed := time.Since(start) //fixedwait:measure the read's time is what the case reports
		_ = f.Close()
		fmt.Fprintf(os.Stderr, "codex-meter-bench %s: the read ends in %s, figure %d\n", b.Name(), elapsed,
			read.tokens)
		switch {
		case past && !errors.Is(err, errCodexRolloutLine):
			b.Fatalf("the meter read %d (%v), want it failed past the bound", read.tokens, err)
		case !past && (err != nil || read.tokens != 200):
			b.Fatalf("the meter read %d (%v), want the newest figure", read.tokens, err)
		}
	}
	b.StopTimer()
}

// codexMeterBenchWrite writes the archive to file; the line is let go of on
// return, before the read.
func codexMeterBenchWrite(b *testing.B, big func() string, file string) {
	count := func(input int) string {
		return fmt.Sprintf(`{"type":"event_msg","ordinal":1,"payload":{"type":"token_count","info":{"last_token_usage":`+
			`{"input_tokens":%d,"total_tokens":%d},"model_context_window":190000}}}`, input, input+10)
	}
	lines := []string{count(100)}
	if big != nil {
		lines = append(lines, big())
	}
	lines = append(lines, count(200))
	if err := os.WriteFile(file, codexArchiveOf(b, strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		b.Fatal(err)
	}
}
