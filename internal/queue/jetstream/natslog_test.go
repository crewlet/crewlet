package jetstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// capture returns a bridge writing JSON into buf at the given level, declaring
// a death of its own.
func capture(buf *bytes.Buffer, level slog.Level) natsLogger {
	return natsLogger{log: slog.New(slog.NewJSONHandler(buf,
		&slog.HandlerOptions{Level: level})), death: newDeath()}
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

// What nats-server reports as WRONG must reach the operator, at a severity
// that says so.
func TestTheBrokersOwnFailuresReachTheOperator(t *testing.T) {
	for _, tc := range []struct {
		name  string
		emit  func(natsLogger)
		level string
	}{
		{"error", func(l natsLogger) { l.Errorf("stream %q: %v", "INBOX", "no space") }, "ERROR"},
		{"warn", func(l natsLogger) { l.Warnf("slow consumer on %s", "INBOX") }, "WARN"},
		// Fatal is an Error line (the engine owns this process) and a
		// declaration, which the next test holds.
		{"fatal", func(l natsLogger) { l.Fatalf("listen on %d failed", 4222) }, "ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.emit(capture(&buf, slog.LevelDebug))
			line := decode(t, &buf)

			if line["level"] != tc.level {
				t.Errorf("level = %v, want %v", line["level"], tc.level)
			}
			// The event name is a token and the broker's sentence a value,
			// so `event=nats_server` stays a usable filter.
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

// Fatalf MUST NOT exit, AND MUST NOT BE ONLY A LINE.
//
// Not exit: the engine owns this process (what Options.NoSigs protects). Not
// only a line: nats-server returns from the failed step and leaves the server
// half started, so a merely logged Fatalf leaves a boot waiting on a server
// that has given up. The bridge declares the death; its owner acts on it.
func TestFatalfDeclaresTheBrokersDeathAndDoesNotEndTheProcess(t *testing.T) {
	var buf bytes.Buffer
	l := capture(&buf, slog.LevelDebug)
	l.Fatalf("bind failed on %d", 6222)
	// Reaching this line is the first assertion.
	if got := decode(t, &buf)["detail"]; got != "bind failed on 6222" {
		t.Errorf("detail = %v, want the message", got)
	}
	select {
	case <-l.death.Done():
	default:
		t.Fatal("a Fatalf was logged and declared nothing, so whoever started " +
			"the server waits on one that has given up")
	}
	if err := l.death.Err(); !errors.Is(err, ErrBrokerDied) ||
		!strings.Contains(err.Error(), "bind failed on 6222") {
		t.Errorf("the declared cause is %v, want nats-server's own sentence", err)
	}
}

// The broker's boot narration is about infrastructure the operator did not
// deploy, so it is DEBUG.
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

// A suppressed line must not pay to format itself: nats-server calls Debugf
// per protocol event and Tracef per message, and without emit's Enabled check
// each would Sprintf its arguments for nothing.
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

// nats-server's executeLogCall returns silently on a nil logger, and nothing
// but this test connects the bridge to the server: dropping the SetLoggerV2
// call, or moving it after Start, would silence the broker unnoticed.
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
	// The call statement alone: Start runs on a goroutine startEmbedded joins.
	where := regexp.MustCompile(`(?m)^\s*ns\.Start\(\)\s*$`).FindStringIndex(src)
	if where == nil {
		t.Fatal("cannot find the server start; this guard has gone stale")
	}
	start := where[0]
	if install > start {
		t.Error("the logger is installed after Start — the boot, where stream " +
			"recovery and store failures report, would log nowhere")
	}
	end := strings.Index(src[install:], ")\n")
	if end < 0 {
		t.Fatal("cannot read the SetLoggerV2 call; this guard has gone stale")
	}
	call := src[install : install+end+1]

	// THE DEBUG FLAG IS THE OPERATOR'S OWN, not the log level's: `-debug`,
	// asked for to watch turns, must not also turn on nats-server's Debugf,
	// which the engine's KV listings drive at a steady rate.
	if !strings.Contains(call, "cfg.Debug") {
		t.Errorf("%s does not pass cfg.Debug — the broker's own debug output "+
			"must be gated on stream.debug rather than on how loud the engine is", call)
	}
	// trace and sysTrace (the last two arguments) are a line per protocol
	// message, on a broker every event passes through: both stay off.
	if !strings.HasSuffix(call, ", false, false)") {
		t.Errorf("%s enables protocol tracing — a line per message, on a "+
			"broker every event in the company passes through", call)
	}
}

// A nats-server Logger that records what it was handed.
//
// Not the engine's logger: logging.Configure installs a process-wide sink, and
// a parallel test pointing it at its own buffer would race every other one.
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

// THE FLAG GATES Debugf AND NOTHING ELSE: the vendor fact [Config.Debug]
// rests on (server/log.go), so declining the firehose costs no diagnostics.
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

// Where the broker's debug volume comes from, and why [Config.Debug] exists.
//
// A KV key listing is an ordered ephemeral consumer created then deleted
// (jetstream/kv.go), and the deletion closes two internal JetStream clients,
// each logging "connection closed: Client Closed" at DEBUG. The coordination
// layer lists keys from two 15-second duty loops, so an idle node logs this
// forever. It is the CLIENT's behaviour, asserted so a client bump that stops
// (or moves) it is noticed.
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

	nc, err := srv.embedded.connect(watched{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
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
	// The teardown runs on the server's own goroutine, so the count settles
	// after the listing returns. Ten seconds is this package's bound on a wait
	// for the broker's goroutines (awaitAcknowledged, settledBacklog); it only
	// bounds how long a client that no longer logs the closes is given.
	var got int
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got = rec.matching(closed) - before; got >= listings || time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			break
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
	if got < listings {
		t.Errorf("%d key listings produced %d close lines; the listing no longer "+
			"costs a consumer, or the broker stopped reporting one", listings, got)
	}
	t.Logf("%d key listings produced %d close lines", listings, got)
}
