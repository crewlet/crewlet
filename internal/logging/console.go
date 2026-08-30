package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// ColorMode is what an operator asked for about ANSI colour.
type ColorMode string

const (
	// ColorAuto colours only when the sink is a live terminal.
	ColorAuto ColorMode = "auto"
	// ColorAlways colours whatever the sink is — for a CI log viewer that
	// renders ANSI but is not a terminal.
	ColorAlways ColorMode = "always"
	// ColorNever never colours.
	ColorNever ColorMode = "never"
)

// ColorModes is the closed set, in the order they are documented.
var ColorModes = []ColorMode{ColorAuto, ColorAlways, ColorNever}

// Valid reports whether m is a colour mode this build understands.
func (m ColorMode) Valid() bool { return oneOfColor(m) }

func oneOfColor(m ColorMode) bool {
	for _, known := range ColorModes {
		if m == known {
			return true
		}
	}
	return false
}

// ParseColorMode reads $CREWLET_LOG_COLOR, with $NO_COLOR as the override
// every other tool in a terminal already honours.
//
// NO_COLOR WINS OVER auto BUT NOT OVER always. The convention
// (https://no-color.org) is that setting it to a non-empty value suppresses
// colour a program would have added on its own initiative — which is exactly
// what `auto` is. `CREWLET_LOG_COLOR=always` is not initiative, it is an
// instruction about this specific program, so it keeps its colour rather
// than being silently ignored by an environment variable the operator may
// not even know their shell exports.
//
// An unrecognised CREWLET_LOG_COLOR resolves to auto rather than failing:
// this is decoration, and no spelling of it may be why a company will not
// boot.
func ParseColorMode(name string) ColorMode {
	switch m := ColorMode(strings.ToLower(strings.TrimSpace(name))); {
	case m == "":
		return ColorAuto
	case m.Valid():
		return m
	default:
		return ColorAuto
	}
}

// colorFromEnv resolves the mode this process runs under.
func colorFromEnv() ColorMode {
	mode := ParseColorMode(os.Getenv("CREWLET_LOG_COLOR"))
	if mode == ColorAuto && os.Getenv("NO_COLOR") != "" {
		return ColorNever
	}
	return mode
}

// isTerminal reports whether w is a live terminal — something a person is
// watching right now, rather than a file, a pipe or a container's captured
// stream.
//
// TERM=dumb is checked because an editor's shell pane and `emacsclient` set
// it precisely to say "I am a terminal that cannot render this".
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// consoleHandler renders one line per record for a PERSON to read.
//
// # Why this exists rather than slog.NewTextHandler
//
// slog's text handler is `time=2026-08-29T19:55:12.482Z level=INFO
// msg=seat_claimed component=seat.host seat=eng.alice` — every field
// self-describing, which is what makes it greppable and what makes it a wall
// of identical prefixes when a person is watching a company boot. The three
// facts an operator scans for (how bad, which subsystem, what happened) are
// the three that sit behind `level=`, `component=` and `msg=` on every
// single line.
//
// This handler puts those three in fixed columns and dims everything else,
// so the eye lands on the event name. `text` and `json` are unchanged and
// remain what a log shipper should be pointed at.
//
// A line with NO component renders that column blank, which is deliberate. It
// happens to anything reaching slog.Default without going through [Get] — the
// stdlib `log` bridge, a dependency that took the default logger — and a
// placeholder there would be indistinguishable from a real component while
// putting punctuation in a column whose only job is alignment.
//
// # Why not a logging library
//
// See adrs/001. The short version: the engine's logger is `*slog.Logger`
// because that is the interface the ecosystem passes around, and a console
// encoder is the only thing zap or zerolog would have been adopted for.
type consoleHandler struct {
	level slog.Level
	// color and dateInTime are decided once, at construction, from the
	// sink — see [newConsoleHandler].
	color      bool
	dateInTime bool
	out        *syncWriter

	// component is hoisted out of the attributes into its own column. It
	// is bound by [Get] on every logger in the tree, so rendering it
	// inline would put the same `component=` in the middle of every line.
	component string

	// groupPrefix is the dotted path opened by WithGroup, applied to every
	// key rendered under it.
	groupPrefix string
	// preformatted holds the attributes bound by WithAttrs, already
	// rendered — the same trick slog's own handlers use.
	preformatted []byte
}

// syncWriter serialises whole records onto one sink.
//
// A handler is called from every goroutine that logs, and derived handlers
// (WithAttrs / WithGroup) share the writer with the one they came from, so
// the lock has to live behind a pointer they all hold rather than in the
// handler value.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// newConsoleHandler builds the human-facing handler over w.
//
// COLOUR AND THE TIMESTAMP SHAPE ARE DECIDED HERE, TOGETHER, from one
// question — is a person watching this stream right now? A live terminal
// gets colour and a bare wall-clock time, because the date is today and the
// width is better spent on the event. Anything else (a file, a pipe, a
// container's captured stream) is read LATER, so it gets no escape codes it
// cannot render and a full date it would otherwise have lost: a redirected
// console log whose lines say only `19:55:12` cannot be correlated with
// anything a day afterwards.
func newConsoleHandler(w io.Writer, level slog.Level, mode ColorMode) *consoleHandler {
	tty := isTerminal(w)
	return &consoleHandler{
		level:      level,
		color:      mode == ColorAlways || (mode == ColorAuto && tty),
		dateInTime: !tty,
		out:        &syncWriter{w: w},
	}
}

// Enabled READS THE LEVEL AND NOTHING ELSE, which is load-bearing beyond
// this type: [lazy.Enabled] answers from the root handler without replaying
// the WithAttrs/WithGroup ops recorded on it, because doing so would
// allocate a handler for every suppressed debug line. That shortcut is only
// correct while every handler Configure can install answers Enabled from its
// level alone. This one does; keep it that way.
func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := h.clone()
	for _, a := range attrs {
		// THE COMPONENT COLUMN, and only at the top level: inside a
		// group, `component` is an ordinary key that happens to share a
		// name, and hoisting it would move a value out of the group it
		// was deliberately put in.
		if next.groupPrefix == "" && a.Key == componentKey && a.Value.Kind() == slog.KindString {
			next.component = a.Value.String()
			continue
		}
		next.preformatted = next.appendAttr(next.preformatted, next.groupPrefix, a)
	}
	return next
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := h.clone()
	next.groupPrefix += name + "."
	return next
}

// clone copies the handler, COPYING the preformatted buffer.
//
// Two derivations of one logger append to it independently, and appending in
// place would let one's attributes appear on the other's lines whenever the
// backing array had spare capacity — the same aliasing bug [lazy.with]
// guards against one layer up.
func (h *consoleHandler) clone() *consoleHandler {
	next := *h
	next.preformatted = make([]byte, len(h.preformatted), len(h.preformatted)+64)
	copy(next.preformatted, h.preformatted)
	return &next
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	bp := bufPool.Get().(*[]byte)
	buf := (*bp)[:0]
	defer func() {
		// A record that ballooned the buffer (a long tool result, a
		// stack in an error) is dropped rather than pooled: keeping it
		// would make one outlier line the resident cost of every line
		// afterwards.
		if cap(buf) <= 8192 {
			*bp = buf
			bufPool.Put(bp)
		}
	}()

	layout := timeLayout
	if h.dateInTime {
		layout = timeLayoutWithDate
	}
	stamp := r.Time
	if stamp.IsZero() {
		stamp = time.Now()
	}
	buf = h.paint(buf, ansiDim, func(b []byte) []byte {
		return stamp.AppendFormat(b, layout)
	})
	buf = append(buf, ' ')

	buf = h.paint(buf, levelColor(r.Level), func(b []byte) []byte {
		return appendPadded(b, r.Level.String(), levelWidth)
	})
	buf = append(buf, ' ')

	buf = h.paint(buf, ansiCyan, func(b []byte) []byte {
		return appendComponent(b, h.component, componentWidth)
	})
	buf = append(buf, ' ')

	// The event name, undimmed and unquoted-if-it-can-be: it is the one
	// field the eye is looking for, and every one in this tree is a
	// snake_case token with nothing in it that needs escaping.
	buf = appendValue(buf, r.Message)

	buf = append(buf, h.preformatted...)
	r.Attrs(func(a slog.Attr) bool {
		buf = h.appendAttr(buf, h.groupPrefix, a)
		return true
	})
	buf = append(buf, '\n')

	_, err := h.out.Write(buf)
	return err
}

// appendAttr renders one attribute as ` key=value`, expanding groups inline
// under a dotted prefix the way slog's text handler does.
func (h *consoleHandler) appendAttr(buf []byte, prefix string, a slog.Attr) []byte {
	// Resolve before anything is decided about it: a LogValuer's own value
	// is what the record means, and its Kind is what says whether this is
	// a group.
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf // slog's contract: an empty Attr is not logged
	}
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		if len(group) == 0 {
			return buf // an empty group renders nothing, not `key=`
		}
		if a.Key != "" {
			prefix += a.Key + "."
		}
		for _, sub := range group {
			buf = h.appendAttr(buf, prefix, sub)
		}
		return buf
	}

	buf = append(buf, ' ')
	buf = h.paint(buf, ansiDim, func(b []byte) []byte {
		b = append(b, prefix...)
		b = appendValue(b, a.Key)
		return append(b, '=')
	})
	// AN ERROR IS THE REASON THE LINE EXISTS. `log.Error("x", "error", err)`
	// is the shape the whole tree uses, so the value behind that key is
	// what an operator is actually reading the line for.
	color := ""
	if isErrorKey(a.Key) {
		color = ansiRed
	}
	return h.paint(buf, color, func(b []byte) []byte {
		return appendValue(b, a.Value.String())
	})
}

// paint wraps whatever render appends in an SGR sequence, and is a plain
// call-through when colour is off — so the uncoloured output has no empty
// escape sequences in it and stays byte-comparable in a test.
func (h *consoleHandler) paint(buf []byte, color string, render func([]byte) []byte) []byte {
	if !h.color || color == "" {
		return render(buf)
	}
	buf = append(buf, color...)
	buf = render(buf)
	return append(buf, ansiReset...)
}

// appendValue writes v, quoting only when it would otherwise be ambiguous.
//
// A value with a space, a quote or a control character in it would run into
// the next `key=` and make the line unparseable by eye; everything else is
// written raw, because quoting every value is what makes a console line hard
// to read in the first place.
func appendValue(buf []byte, v string) []byte {
	if v == "" {
		return append(buf, `""`...)
	}
	if needsQuote(v) {
		return strconv.AppendQuote(buf, v)
	}
	return append(buf, v...)
}

func needsQuote(v string) bool {
	for i := range len(v) {
		if c := v[i]; c <= ' ' || c == '"' || c == '=' || c == 0x7f {
			return true
		}
	}
	return false
}

// isErrorKey reports whether a key holds the failure a line is about. Both
// spellings appear in the tree, and a nested one arrives dotted.
func isErrorKey(key string) bool {
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		key = key[i+1:]
	}
	return key == "error" || key == "err"
}

// appendPadded writes s left-aligned in at least width columns.
//
// A name LONGER than the column is written in full rather than truncated:
// the column is there to make the common case scannable, and a truncated
// name is a fact destroyed to keep a margin straight.
//
// It does NOT escape s, so it is only for strings this process generates —
// today just the level, whose text slog derives from an integer. Anything
// that could carry a caller's bytes goes through [appendComponent] or
// [appendValue].
func appendPadded(buf []byte, s string, width int) []byte {
	buf = append(buf, s...)
	for i := len(s); i < width; i++ {
		buf = append(buf, ' ')
	}
	return buf
}

// appendComponent writes the component column, ESCAPED and then padded to
// what it actually rendered as.
//
// # A column is not a reason to skip escaping
//
// The message and every attribute value go through [appendValue], which
// quotes anything holding a control byte. The component used to be written
// raw because every logging.Get in the tree passes a string literal — but
// the column is reached by any `.With("component", x)`, x is one refactor
// away from being an MCP server name or a role handle out of the company
// config, and a component holding a newline forges an entire log line while
// one holding an ESC repaints an operator's terminal. The defence the rest
// of the line already has should not depend on which column a string
// lands in.
//
// An EMPTY component renders as blank padding rather than as `""` — see the
// type's doc comment for why the empty case is intentional.
func appendComponent(buf []byte, component string, width int) []byte {
	start := len(buf)
	if component != "" {
		buf = appendValue(buf, component)
	}
	for i := len(buf) - start; i < width; i++ {
		buf = append(buf, ' ')
	}
	return buf
}

func levelColor(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return ansiDim
	case level < slog.LevelWarn:
		return ansiGreen
	case level < slog.LevelError:
		return ansiYellow
	default:
		return ansiBoldRed
	}
}

const (
	componentKey = "component"

	// levelWidth fits ERROR, the longest of the four names. A custom level
	// prints wider (slog spells LevelError+1 as "ERROR+1") and pushes its
	// own line's columns right rather than being cut.
	levelWidth = 5

	// componentWidth is the longest component name in the tree
	// ("sandbox.coding_agent" and "providers.credential", both 20), so
	// every one of the 59 names in use today lands in a straight column.
	componentWidth = 20

	// timeLayout is wall-clock only, for a terminal someone is watching:
	// the date is today, and the eight columns it would cost are better
	// spent on the event.
	timeLayout = "15:04:05.000"
	// timeLayoutWithDate is what a redirected stream gets, because it is
	// read later and a bare time cannot be correlated with anything.
	timeLayoutWithDate = "2006-01-02 15:04:05.000"

	ansiReset   = "\x1b[0m"
	ansiDim     = "\x1b[2m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiCyan    = "\x1b[36m"
	ansiBoldRed = "\x1b[1;31m"
)
