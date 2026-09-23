package jetstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
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
	end := strings.Index(src[install:], ")\n")
	if end < 0 {
		t.Fatal("cannot read the SetLoggerV2 call; this guard has gone stale")
	}
	call := src[install : install+end+1]

	// THE DEBUG FLAG IS THE OPERATOR'S OWN, not the log level's. Derived
	// from the level, `-debug` — asked for to watch turns — also turned on
	// nats-server's internal Debugf population, which the engine's own KV
	// listings drive at a steady rate. Nothing but this call carries the
	// answer, so a refactor that went back to the level would be silent.
	if !strings.Contains(call, "cfg.Debug") {
		t.Errorf("%s does not pass cfg.Debug — the broker's own debug output "+
			"must be gated on stream.debug rather than on how loud the engine is", call)
	}
	// Trace is a line per protocol message and the engine publishes every
	// event through this broker; sysTrace is the same for the system
	// account. Both are the last two arguments and both must stay off.
	if !strings.HasSuffix(call, ", false, false)") {
		t.Errorf("%s enables protocol tracing — a line per message, on a "+
			"broker every event in the company passes through", call)
	}
}

// A nats-server Logger that records what it was handed.
//
// NOT the engine's logger, and deliberately: logging.Configure installs a
// PROCESS-WIDE sink, this suite runs its cases in parallel, and a test that
// pointed the global at its own buffer would race every other one — which is
// the exact failure logging.Configure's own doc is about.
type recordedLines struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordedLines) add(kind, format string, v ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, kind+": "+fmt.Sprintf(format, v...))
}

func (r *recordedLines) matching(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func (r *recordedLines) Noticef(f string, v ...any) { r.add("notice", f, v...) }
func (r *recordedLines) Warnf(f string, v ...any)   { r.add("warn", f, v...) }
func (r *recordedLines) Errorf(f string, v ...any)  { r.add("error", f, v...) }
func (r *recordedLines) Fatalf(f string, v ...any)  { r.add("fatal", f, v...) }
func (r *recordedLines) Debugf(f string, v ...any)  { r.add("debug", f, v...) }
func (r *recordedLines) Tracef(f string, v ...any)  { r.add("trace", f, v...) }

// THE FLAG GATES Debugf AND NOTHING ELSE. This is the vendor fact
// [Config.Debug] rests on: nats-server checks it inside its own Debugf
// (server/log.go) and on no other severity, so declining the firehose costs
// none of the diagnostics the bridge exists to deliver.
func TestTheBrokersDebugFlagGatesOnlyItsDebugLines(t *testing.T) {
	t.Parallel()
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprintf("debug=%t", debug), func(t *testing.T) {
			t.Parallel()
			srv, err := StartServer(t.Context(), Config{})
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			defer srv.Shutdown()

			rec := &recordedLines{}
			srv.embedded.ns.SetLoggerV2(rec, debug, false, false)
			srv.embedded.ns.Debugf("a debug line")
			srv.embedded.ns.Noticef("a notice")
			srv.embedded.ns.Warnf("a warning")
			srv.embedded.ns.Errorf("an error")

			if got := rec.matching("debug: "); (got > 0) != debug {
				t.Errorf("debug lines recorded = %d with the flag %t", got, debug)
			}
			for _, kind := range []string{"notice: ", "warn: ", "error: "} {
				if rec.matching(kind) == 0 {
					t.Errorf("%q was swallowed with the debug flag %t — the flag "+
						"must gate Debugf alone", kind, debug)
				}
			}
		})
	}
}

// WHERE THE VOLUME ACTUALLY COMES FROM, and the measurement behind
// [Config.Debug] existing at all.
//
// A KV key listing is not a read: the vendored client implements it as an
// ordered ephemeral consumer created and then deleted (jetstream/kv.go), and
// deleting a consumer closes the two internal JetStream clients it was built
// on — each of which logs its own close at DEBUG. The engine's coordination
// layer lists keys from two 15-second duty loops, so on the engine's own
// debug level this was a permanent background stream of "JetStream
// connection closed: Client Closed" for an idle node.
//
// Asserted here because it belongs to the CLIENT rather than to this package:
// a bump that stopped listing keys through a consumer would make this test
// fail with good news, and one that started doing it somewhere else would
// make the same noise appear again with nothing to explain it.
func TestAKeyListingCostsAConsumerAndSaysSoAtDebug(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	srv, err := StartServer(ctx, Config{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Shutdown()

	rec := &recordedLines{}
	srv.embedded.ns.SetLoggerV2(rec, true, false, false)

	// THE QUEUE'S OWN CONNECTION, which is the one the coordination store
	// lists its keys over in production (internal/engine's openNATS).
	q, err := srv.Client(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = q.Stop(context.WithoutCancel(ctx)) }()
	js, err := jetstream.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "probe"})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if _, err = kv.Put(ctx, "a", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}

	const closed = "connection closed: Client Closed"
	before := rec.matching(closed)
	const listings = 5
	for range listings {
		lister, err := kv.ListKeys(ctx)
		if err != nil {
			t.Fatalf("list keys: %v", err)
		}
		for range lister.Keys() {
		}
	}
	// The teardown is the server's own goroutine, so the count settles
	// shortly after the listing returns rather than during it.
	var got int
	for range 200 {
		if got = rec.matching(closed) - before; got >= listings {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got < listings {
		t.Errorf("%d key listings produced %d close lines; the listing no longer "+
			"costs a consumer, or the broker stopped reporting one", listings, got)
	}
	t.Logf("%d key listings produced %d close lines", listings, got)
}
