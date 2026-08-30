package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

const (
	goodTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	goodSpan  = "00f067aa0ba902b7"
)

func jsonSink(t *testing.T, level slog.Level) (*bytes.Buffer, *slog.Logger) {
	t.Helper()
	var buf bytes.Buffer
	Configure(level, FormatJSON, &buf)
	t.Cleanup(func() { Configure(slog.LevelInfo, FormatConsole, &bytes.Buffer{}) })
	return &buf, Get("t.component")
}

func lastLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("nothing was logged")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &out); err != nil {
		t.Fatalf("decoding %q: %v", lines[len(lines)-1], err)
	}
	return out
}

// The point of the whole carrier: a line logged under a bound trace names it,
// so an operator reading logs and a collector reading spans are looking at the
// same identifier.
func TestABoundTraceReachesTheLine(t *testing.T) {
	buf, log := jsonSink(t, slog.LevelInfo)
	ctx := WithTrace(context.Background(), goodTrace, goodSpan)

	log.InfoContext(ctx, "seat_claimed", "seat", "eng.alice")

	line := lastLine(t, buf)
	if line["trace_id"] != goodTrace {
		t.Errorf("trace_id = %v, want %s", line["trace_id"], goodTrace)
	}
	if line["span_id"] != goodSpan {
		t.Errorf("span_id = %v, want %s", line["span_id"], goodSpan)
	}
	// The line's own content must survive intact beside the injection.
	if line["seat"] != "eng.alice" || line["msg"] != "seat_claimed" {
		t.Errorf("the record was damaged: %v", line)
	}
}

// A line logged with no trace bound must not grow empty fields — a shipper
// indexing trace_id="" is worse than one indexing nothing.
func TestNoTraceMeansNoFields(t *testing.T) {
	buf, log := jsonSink(t, slog.LevelInfo)

	log.InfoContext(context.Background(), "seat_claimed")

	line := lastLine(t, buf)
	if _, present := line["trace_id"]; present {
		t.Errorf("trace_id appeared with nothing bound: %v", line)
	}
	if _, present := line["span_id"]; present {
		t.Errorf("span_id appeared with nothing bound: %v", line)
	}
}

// This is the injection route by which a value from OUTSIDE — a remote
// traceparent on the webhook edge — could reach a terminal. Only real ids get
// through.
func TestAMalformedIdNeverReachesALine(t *testing.T) {
	for _, tc := range []struct{ name, trace, span string }{
		{"escape sequence", "\x1b[31mred", goodSpan},
		{"newline forges a line", "aaaa\nlevel=ERROR msg=forged", goodSpan},
		{"too short", "4bf92f35", goodSpan},
		{"too long", goodTrace + "ff", goodSpan},
		{"uppercase", strings.ToUpper(goodTrace), goodSpan},
		{"non-hex", strings.Repeat("g", 32), goodSpan},
		{"all zero is OTel's invalid", strings.Repeat("0", 32), goodSpan},
		{"bad span", goodTrace, "zzzz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, log := jsonSink(t, slog.LevelInfo)
			ctx := WithTrace(context.Background(), tc.trace, tc.span)

			log.InfoContext(ctx, "an_event")

			line := lastLine(t, buf)
			if got, ok := line["trace_id"].(string); ok && got != goodTrace {
				t.Errorf("a malformed trace id reached the line: %q", got)
			}
			if got, ok := line["span_id"].(string); ok && got != goodSpan {
				t.Errorf("a malformed span id reached the line: %q", got)
			}
			// Whatever was rejected, exactly one line was written.
			if n := strings.Count(strings.TrimSpace(buf.String()), "\n"); n != 0 {
				t.Errorf("the record forged %d extra lines", n)
			}
		})
	}
}

// THE CONSTRAINT decisions/001 states, re-asserted against the new code:
// Enabled reads the level and nothing else. A trace on the context must not
// make a suppressed line emit, nor an emitted line vanish.
func TestABoundTraceDoesNotChangeWhatIsEnabled(t *testing.T) {
	buf, log := jsonSink(t, slog.LevelInfo)
	ctx := WithTrace(context.Background(), goodTrace, goodSpan)

	log.DebugContext(ctx, "suppressed_event")
	if buf.Len() != 0 {
		t.Fatalf("a debug line surfaced at info because a trace was bound: %s", buf.String())
	}
	if log.Enabled(ctx, slog.LevelDebug) {
		t.Error("Enabled answered true for debug at info level")
	}
}

// All three formats carry the ids. d-001 gives each format one reader and one
// SHAPE — not one field set — and a format that silently dropped a field the
// others carry is the same class of bug as a config key nothing reads.
func TestEveryFormatCarriesTheTrace(t *testing.T) {
	for _, format := range Formats {
		t.Run(string(format), func(t *testing.T) {
			var buf bytes.Buffer
			Configure(slog.LevelInfo, format, &buf)
			t.Cleanup(func() { Configure(slog.LevelInfo, FormatConsole, &bytes.Buffer{}) })

			ctx := WithTrace(context.Background(), goodTrace, goodSpan)
			Get("t.component").InfoContext(ctx, "an_event")

			if out := buf.String(); !strings.Contains(out, goodTrace) ||
				!strings.Contains(out, goodSpan) {
				t.Errorf("%s dropped the trace ids: %s", format, out)
			}
		})
	}
}

// The carrier must not allocate on the path that has no trace — every line in
// the engine passes through it.
func TestBindingNothingIsFree(t *testing.T) {
	ctx := context.Background()
	if got := WithTrace(ctx, "", ""); got != ctx {
		t.Error("binding two empty ids allocated a context")
	}
	if got := WithTrace(ctx, "nonsense", "nonsense"); got != ctx {
		t.Error("binding two invalid ids allocated a context")
	}
}

func TestTraceFromContextReadsBackWhatWasBound(t *testing.T) {
	ctx := WithTrace(context.Background(), goodTrace, goodSpan)
	traceID, spanID := TraceFromContext(ctx)
	if traceID != goodTrace || spanID != goodSpan {
		t.Errorf("read back (%q, %q), want (%q, %q)", traceID, spanID, goodTrace, goodSpan)
	}
	if traceID, spanID := TraceFromContext(context.Background()); traceID != "" || spanID != "" {
		t.Errorf("an unbound context reported (%q, %q)", traceID, spanID)
	}
}
