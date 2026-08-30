package jetstream

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// capture returns a bridge writing JSON into buf at the given level.
func capture(buf *bytes.Buffer, level slog.Level) natsLogger {
	return natsLogger{log: slog.New(slog.NewJSONHandler(buf,
		&slog.HandlerOptions{Level: level}))}
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("nothing was logged")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("decoding %q: %v", line, err)
	}
	return out
}

// The whole point of the bridge: what nats-server reports as WRONG must
// reach the operator, at a severity that says so. Before it existed, the
// embedded server had no logger at all and every one of these went nowhere.
func TestTheBrokersOwnFailuresReachTheOperator(t *testing.T) {
	for _, tc := range []struct {
		name  string
		emit  func(natsLogger)
		level string
	}{
		{"error", func(l natsLogger) { l.Errorf("stream %q: %v", "INBOX", "no space") }, "ERROR"},
		{"warn", func(l natsLogger) { l.Warnf("slow consumer on %s", "INBOX") }, "WARN"},
		// Fatal is an Error line, because the engine owns this process.
		{"fatal", func(l natsLogger) { l.Fatalf("listen on %d failed", 4222) }, "ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.emit(capture(&buf, slog.LevelDebug))
			line := decode(t, &buf)

			if line["level"] != tc.level {
				t.Errorf("level = %v, want %v", line["level"], tc.level)
			}
			// The event name is a token and the broker's sentence is a
			// value — the engine's rule, and what keeps `event=nats_server`
			// a usable filter.
			if line["msg"] != "nats_server" {
				t.Errorf("event = %v, want nats_server", line["msg"])
			}
			detail, _ := line["detail"].(string)
			if detail == "" {
				t.Fatal("the broker's own message must survive, under `detail`")
			}
			if strings.Contains(detail, "%") {
				t.Errorf("detail = %q — the format verbs were never expanded", detail)
			}
		})
	}
}

// Fatalf MUST NOT exit. nats-server returns immediately after every
// s.Fatalf, so the process surviving is what lets the engine run its own
// drain instead of vanishing mid-shutdown from inside a library — the same
// property Options.NoSigs exists to protect.
func TestFatalfDoesNotEndTheProcess(t *testing.T) {
	var buf bytes.Buffer
	capture(&buf, slog.LevelDebug).Fatalf("bind failed")
	// Reaching this line at all is the assertion; the log is the corroboration.
	if got := decode(t, &buf)["detail"]; got != "bind failed" {
		t.Errorf("detail = %v, want the message", got)
	}
}

// The broker's boot narration is a dozen lines about infrastructure the
// operator deliberately did not deploy, so it is DEBUG — present when
// someone is diagnosing the broker, absent otherwise.
func TestBootNarrationIsDebugAndNotInfo(t *testing.T) {
	var buf bytes.Buffer
	capture(&buf, slog.LevelInfo).Noticef("Server is ready")
	if buf.Len() != 0 {
		t.Errorf("a Notice surfaced at info: %s", buf.String())
	}

	buf.Reset()
	capture(&buf, slog.LevelDebug).Noticef("Server is ready")
	if line := decode(t, &buf); line["level"] != "DEBUG" {
		t.Errorf("level = %v, want DEBUG", line["level"])
	}
}

// noisy records whether it was asked to render itself.
type noisy struct{ rendered *bool }

func (n noisy) String() string { *n.rendered = true; return "rendered" }

// A suppressed line must not pay to format itself. nats-server calls Debugf
// per protocol event once debug is on, and Tracef per message; without the
// Enabled check in emit, every one of those would Sprintf its arguments and
// throw the result away.
func TestASuppressedLineDoesNotFormatItsArguments(t *testing.T) {
	var rendered bool
	var buf bytes.Buffer
	capture(&buf, slog.LevelWarn).Debugf("client %v", noisy{&rendered})

	if buf.Len() != 0 {
		t.Fatalf("a debug line surfaced at warn: %s", buf.String())
	}
	if rendered {
		t.Error("the argument was formatted for a line that was never emitted")
	}
}

// The defect this bridge fixes was not that the mapping was wrong — it was
// that NOTHING INSTALLED A LOGGER, and nats-server's executeLogCall returns
// silently on a nil one. Nothing but this connects the bridge to the server,
// so a refactor that drops the call would restore the silence unnoticed.
func TestTheEmbeddedServerIsGivenTheLoggerBeforeItStarts(t *testing.T) {
	raw, err := os.ReadFile("embedded.go")
	if err != nil {
		t.Fatalf("reading embedded.go: %v", err)
	}
	src := string(raw)

	install := strings.Index(src, "SetLoggerV2(")
	if install < 0 {
		t.Fatal("the embedded server is never given a logger; nats-server " +
			"installs none of its own and discards every line in silence")
	}
	start := strings.Index(src, "go ns.Start()")
	if start < 0 {
		t.Fatal("cannot find the server start; this guard has gone stale")
	}
	if install > start {
		t.Error("the logger is installed after Start — the boot, where stream " +
			"recovery and store failures report, would log nowhere")
	}
	// Trace is a line per protocol message and the engine publishes every
	// event through this broker.
	if strings.Contains(src, "SetLoggerV2(natsLog, natsDebug, true") {
		t.Error("protocol tracing must never be enabled by default")
	}
}
