package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// console builds a handler over buf with colour forced one way or the other,
// bypassing the terminal probe a test process cannot satisfy.
func console(buf *bytes.Buffer, mode ColorMode) *consoleHandler {
	h := newConsoleHandler(buf, slog.LevelDebug, mode)
	// A bytes.Buffer is never a terminal, so dateInTime is already true;
	// pinning it keeps the column assertions below independent of that.
	h.dateInTime = false
	return h
}

func line(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	return strings.TrimSuffix(buf.String(), "\n")
}

// THE THREE FACTS AN OPERATOR SCANS FOR GET COLUMNS. Everything this
// handler exists for is that level, component and event name land in fixed
// positions rather than behind `level=` / `component=` / `msg=` on every
// line — see the type's doc comment.
func TestAConsoleLinePutsLevelComponentAndEventInColumns(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(console(&buf, ColorNever)).With("component", "seat.host")
	log.Info("seat_claimed", "seat", "eng.alice", "epoch", 7)

	got := line(t, &buf)
	// time, level, component, event, then the attributes.
	rest, ok := strings.CutPrefix(got, time.Now().Format("15:04:05")[:2])
	if !ok {
		t.Fatalf("the line does not start with a wall-clock hour: %q", got)
	}
	_ = rest
	if !strings.Contains(got, "INFO  seat.host            seat_claimed") {
		t.Errorf("level/component/event are not in their columns: %q", got)
	}
	if !strings.Contains(got, "seat=eng.alice") || !strings.Contains(got, "epoch=7") {
		t.Errorf("the attributes were lost: %q", got)
	}
	// The component is a COLUMN, not an attribute: rendering it both ways
	// would put the same `component=` in the middle of every line, which is
	// the wall this handler exists to break up.
	if strings.Contains(got, "component=") {
		t.Errorf("the component was also rendered inline: %q", got)
	}
}

// A SINK THAT IS NOT A TERMINAL GETS NO ESCAPE CODES. A file, a pipe or a
// container's captured stream renders `\x1b[32m` literally, and a log full
// of them is worse than one with no colour at all.
func TestAutoDoesNotColourANonTerminal(t *testing.T) {
	var buf bytes.Buffer
	// Through the real probe this time — a bytes.Buffer is not an *os.File
	// and an *os.File that is a pipe is not a terminal.
	h := newConsoleHandler(&buf, slog.LevelInfo, ColorAuto)
	slog.New(h).With("component", "cli").Error("boom", "error", "nope")

	if strings.Contains(buf.String(), "\x1b[") {
		t.Fatalf("auto coloured a buffer: %q", buf.String())
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	if isTerminal(w) {
		t.Error("a pipe reported itself a terminal")
	}
}

// AND A REDIRECTED STREAM KEEPS ITS DATE. It is read later, and a line that
// says only 19:55:12 cannot be correlated with anything a day afterwards.
func TestARedirectedConsoleStreamCarriesTheDate(t *testing.T) {
	var buf bytes.Buffer
	h := newConsoleHandler(&buf, slog.LevelInfo, ColorNever)
	if !h.dateInTime {
		t.Fatal("a buffer is not a terminal, so its lines must carry the date")
	}
	slog.New(h).Info("an_event")
	if !strings.HasPrefix(buf.String(), time.Now().Format("2006-01-02")) {
		t.Fatalf("the date is missing from a redirected line: %q", buf.String())
	}
}

// COLOUR IS AN INSTRUCTION WHEN IT IS EXPLICIT. `always` is for a CI log
// viewer that renders ANSI without being a terminal, which auto-detection
// can never discover on its own.
func TestColourModes(t *testing.T) {
	for _, tc := range []struct {
		mode ColorMode
		want bool
	}{
		{ColorAlways, true},
		{ColorNever, false},
		{ColorAuto, false}, // a buffer is not a terminal
	} {
		var buf bytes.Buffer
		slog.New(newConsoleHandler(&buf, slog.LevelInfo, tc.mode)).Info("an_event")
		if got := strings.Contains(buf.String(), "\x1b["); got != tc.want {
			t.Errorf("%s: coloured = %v, want %v (%q)", tc.mode, got, tc.want, buf.String())
		}
	}
}

// AN ERROR IS PAINTED, because `log.Error("x", "error", err)` is the shape
// the whole tree uses and that value is what the line is being read for.
func TestTheErrorValueIsPainted(t *testing.T) {
	var buf bytes.Buffer
	slog.New(console(&buf, ColorAlways)).Error("mcp_server_failed", "error", "dial tcp")
	if !strings.Contains(buf.String(), ansiRed+`"dial tcp"`+ansiReset) {
		t.Fatalf("the error value was not painted: %q", buf.String())
	}
	for _, key := range []string{"error", "err", "turn.error"} {
		if !isErrorKey(key) {
			t.Errorf("%q should read as an error key", key)
		}
	}
	for _, key := range []string{"errors", "terror", "error_count"} {
		if isErrorKey(key) {
			t.Errorf("%q should not read as an error key", key)
		}
	}
}

// GROUPS RENDER DOTTED, the same shape slog's text handler produces, so a
// runbook's grep for `turn.id=` works whichever format is installed.
func TestAGroupRendersDotted(t *testing.T) {
	var buf bytes.Buffer
	slog.New(console(&buf, ColorNever)).WithGroup("turn").Info("a_line", "id", "t1")
	if !strings.Contains(buf.String(), "turn.id=t1") {
		t.Fatalf("the group was dropped: %q", buf.String())
	}
}

// A COMPONENT KEY INSIDE A GROUP IS NOT THE COLUMN. Hoisting it would move
// a value out of the group it was deliberately put in, and blank the column
// of the logger that owns it.
func TestAComponentInsideAGroupIsNotHoisted(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(console(&buf, ColorNever)).
		With("component", "seat.host").
		WithGroup("peer").
		With("component", "seat.host@n2")
	log.Info("peer_seen")

	got := line(t, &buf)
	if !strings.Contains(got, "peer.component=seat.host@n2") {
		t.Errorf("the grouped component lost its group: %q", got)
	}
	if !strings.Contains(got, "seat.host  ") {
		t.Errorf("the real component column was overwritten: %q", got)
	}
}

// AN EMPTY GROUP AND AN EMPTY ATTR RENDER NOTHING, which is slog's own
// contract — a handler that emitted `key=` for them would put a field in the
// stream that the JSON handler does not.
func TestEmptyGroupsAndAttrsAreSkipped(t *testing.T) {
	var buf bytes.Buffer
	slog.New(console(&buf, ColorNever)).Info("a_line",
		slog.Group("empty"), slog.Attr{}, slog.String("kept", "yes"))

	got := line(t, &buf)
	if strings.Contains(got, "empty") {
		t.Errorf("an empty group was rendered: %q", got)
	}
	if !strings.Contains(got, "kept=yes") {
		t.Errorf("the surviving attribute was dropped: %q", got)
	}
}

// A VALUE THAT WOULD RUN INTO THE NEXT KEY IS QUOTED, and one that would not
// is left alone — quoting everything is what makes a console line unreadable.
func TestValuesAreQuotedOnlyWhenAmbiguous(t *testing.T) {
	var buf bytes.Buffer
	slog.New(console(&buf, ColorNever)).Info("a_line",
		"plain", "eng.alice", "spaced", "two words", "empty", "")

	got := line(t, &buf)
	for _, want := range []string{`plain=eng.alice`, `spaced="two words"`, `empty=""`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %q", want, got)
		}
	}
}

// DERIVED LOGGERS DO NOT LEAK INTO EACH OTHER. The preformatted buffer is
// appended to by every derivation, so sharing its backing array would put
// one logger's attributes on another's lines.
//
// Three things make this able to fail, and a version missing any of them
// passed with clone's copy deleted:
//
//   - THE SHARED PARENT CARRIES AN ATTRIBUTE, so there is something for a
//     child to append after.
//   - ITS BUFFER HAS SPARE CAPACITY, reserved below rather than inherited
//     from an append. Whether an append happens to leave room is an allocator
//     detail: with none, both children reallocate and a sharing clone looks
//     correct. A test that relied on that luck proves nothing.
//   - BOTH DERIVATIONS EXIST BEFORE EITHER LOGS. Deriving and logging one at
//     a time renders each line before the other can overwrite the bytes
//     behind it — the corruption happening and going unobserved.
func TestDerivedConsoleHandlersDoNotShareAttributes(t *testing.T) {
	var buf bytes.Buffer
	base := console(&buf, ColorNever).
		WithAttrs([]slog.Attr{slog.String("component", "shared"), slog.String("node", "n1")}).(*consoleHandler)
	if len(base.preformatted) == 0 {
		t.Fatal("the parent carries no attribute, so nothing here can alias")
	}
	base.preformatted = append(make([]byte, 0, 256), base.preformatted...)

	left := slog.New(base.WithAttrs([]slog.Attr{slog.String("side", "left")}))
	right := slog.New(base.WithAttrs([]slog.Attr{slog.String("side", "right")}))
	left.Info("left_line")
	right.Info("right_line")

	for got := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		switch {
		case strings.Contains(got, "left_line"):
			if !strings.Contains(got, "node=n1 side=left") {
				t.Errorf("the left logger lost or corrupted its own attribute: %q", got)
			}
		case strings.Contains(got, "right_line"):
			if !strings.Contains(got, "node=n1 side=right") {
				t.Errorf("the right logger lost or corrupted its own attribute: %q", got)
			}
		}
	}
}

// ENABLED READS THE LEVEL AND NOTHING ELSE, which [lazy.Enabled] depends on:
// it answers from the ROOT handler without replaying the WithAttrs/WithGroup
// ops, so a handler whose Enabled consulted its attributes would filter the
// wrong lines and only the suppressed ones would show it.
func TestConsoleEnabledDependsOnTheLevelAlone(t *testing.T) {
	var buf bytes.Buffer
	base := newConsoleHandler(&buf, slog.LevelWarn, ColorNever)
	derived := base.WithAttrs([]slog.Attr{slog.String("component", "x")}).WithGroup("g")

	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		want := level >= slog.LevelWarn
		if got := base.Enabled(context.Background(), level); got != want {
			t.Errorf("base.Enabled(%v) = %v, want %v", level, got, want)
		}
		if got := derived.Enabled(context.Background(), level); got != want {
			t.Errorf("derived.Enabled(%v) = %v, want %v", level, got, want)
		}
	}
}

// A LONG COMPONENT NAME IS WRITTEN IN FULL. The column exists to make the
// common case scannable; truncating is a fact destroyed to keep a margin
// straight.
func TestALongComponentIsNotTruncated(t *testing.T) {
	var buf bytes.Buffer
	long := "a.very.long.component.name.beyond.the.column"
	slog.New(console(&buf, ColorNever)).With("component", long).Info("an_event")
	if !strings.Contains(buf.String(), long+" an_event") {
		t.Fatalf("a long component was cut: %q", buf.String())
	}
}

// WHOLE RECORDS REACH THE SINK INTACT under concurrency: two handlers over
// one writer do not serialise each other, so the lock lives behind a pointer
// every derivation shares.
func TestConcurrentConsoleWritesDoNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(console(&buf, ColorNever))

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			log := base.With("component", "worker", "n", i)
			for range 50 {
				log.Info("a_line", "k", "v")
			}
		})
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 400 {
		t.Fatalf("got %d lines, want 400", len(lines))
	}
	for _, got := range lines {
		if !strings.HasSuffix(got, "k=v") {
			t.Fatalf("a record was torn: %q", got)
		}
	}
}

// NO_COLOR SUPPRESSES INITIATIVE, NOT AN INSTRUCTION. It is the convention
// for "do not add colour on your own"; an operator who typed
// CREWLET_LOG_COLOR=always has said something about THIS program, and an
// environment variable their shell may export without their knowledge must
// not silently win over it.
func TestNoColorSuppressesAutoButNotAlways(t *testing.T) {
	for _, tc := range []struct {
		noColor, crewlet string
		want             ColorMode
	}{
		{"", "", ColorAuto},
		{"1", "", ColorNever},
		{"1", "always", ColorAlways},
		{"1", "auto", ColorNever},
		{"", "never", ColorNever},
		{"", "nonesuch", ColorAuto},
		{"", "  ALWAYS  ", ColorAlways},
	} {
		t.Setenv("NO_COLOR", tc.noColor)
		t.Setenv("CREWLET_LOG_COLOR", tc.crewlet)
		if got := colorFromEnv(); got != tc.want {
			t.Errorf("NO_COLOR=%q CREWLET_LOG_COLOR=%q: got %s, want %s",
				tc.noColor, tc.crewlet, got, tc.want)
		}
	}
}

// TERM=dumb IS A TERMINAL SAYING IT CANNOT RENDER THIS — an editor's shell
// pane sets it precisely to say so.
func TestTermDumbIsNotAColourableTerminal(t *testing.T) {
	t.Setenv("TERM", "dumb")
	if isTerminal(os.Stdout) {
		t.Fatal("TERM=dumb should never be treated as a colourable terminal")
	}
}

// EVERY DECLARED FORMAT HAS ITS OWN CASE IN install.
//
// The switch falls through to console for anything it does not recognise,
// which is right for a typo and wrong for a format this package advertises:
// a fourth entry in [Formats] with no case beside it would validate, pass a
// config round-trip, appear in the generated schema and in an editor's
// completion list, and then render as console with nothing saying so. The
// closed set and the constructor that serves it are asserted against each
// other here, the way providers.sandbox.type's already are.
func TestEveryDeclaredFormatInstallsItsOwnHandler(t *testing.T) {
	t.Cleanup(func() { Configure(slog.LevelInfo, FormatConsole, io.Discard) })

	byHandler := map[string]Format{}
	for _, format := range Formats {
		install(slog.LevelInfo, format, io.Discard)
		name := fmt.Sprintf("%T", root.Load().Handler())
		if other, dup := byHandler[name]; dup {
			t.Errorf("formats %q and %q both install %s — install's switch "+
				"has no case for one of them, so it renders as the fallback",
				format, other, name)
		}
		byHandler[name] = format
	}

	install(slog.LevelInfo, FormatConsole, io.Discard)
	if _, ok := root.Load().Handler().(*consoleHandler); !ok {
		t.Errorf("the console format installed %T", root.Load().Handler())
	}
}

// NOTHING A CALLER SUPPLIES REACHES THE TERMINAL RAW.
//
// Log content is not all first-party: an MCP server's stderr, a webhook
// payload, an LLM error string and a vendor API message all end up as values
// on these lines. A control byte that survived would let that content repaint
// an operator's terminal, and a newline would let it forge a whole log line —
// a fake ERROR, or a fake "seat_released", written by whatever the engine was
// talking to.
//
// The component column is in here because it was the one that DIDN'T escape.
// Every logging.Get in the tree passes a literal today, so it was not
// reachable — but the column is reached by any `.With("component", x)`, and x
// is one refactor away from being a server name out of the company config.
func TestNothingReachesTheTerminalRaw(t *testing.T) {
	for _, tc := range []struct{ name, component, message, value string }{
		{"escape in the component", "seat\x1b[31m.host", "an_event", "ok"},
		{"newline in the component", "seat\n20:00:00.000 ERROR fake", "an_event", "ok"},
		{"escape in the event name", "seat.host", "an_event\x1b[31m", "ok"},
		{"newline in the event name", "seat.host", "an\nevent", "ok"},
		{"escape in a value", "seat.host", "an_event", "\x1b[2J"},
		{"newline in a value", "seat.host", "an_event", "line1\nline2"},
		{"escape in a key", "seat.host", "an_event", "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			slog.New(console(&buf, ColorNever)).
				With("component", tc.component).
				Info(tc.message, "k\x1b[0m", tc.value)

			got := buf.Bytes()
			if bytes.ContainsRune(got, 0x1b) {
				t.Errorf("an escape byte reached the sink: %q", got)
			}
			if n := bytes.Count(got, []byte{'\n'}); n != 1 {
				t.Errorf("one record produced %d lines, so content forged one: %q", n, got)
			}
		})
	}
}

// AND THE COLUMN STILL LINES UP once a component has been escaped: the
// padding counts what was RENDERED, not what was handed in, or a quoted name
// pushes its own line's event two columns right.
func TestTheComponentColumnPadsWhatItRendered(t *testing.T) {
	var buf bytes.Buffer
	slog.New(console(&buf, ColorNever)).With("component", "a b").Info("an_event")
	// `a b` renders as `"a b"` — 5 columns — so it is padded out to
	// componentWidth, and then the usual single separator space follows.
	if !strings.Contains(buf.String(), `"a b"`+strings.Repeat(" ", componentWidth-5)+" an_event") {
		t.Fatalf("the escaped component did not pad to its rendered width: %q", buf.String())
	}
}
