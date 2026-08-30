// Package logging is the one place a Crewlet logger is built.
//
// Every log line in the engine is structured: a short machine-parsable
// snake_case event name plus key/value attributes. Dynamic data never goes
// into the message string, so a log stream stays greppable by event name and
// filterable by field. The rule that buys that:
//
//	log.Info("task_created", "task_id", id, "creator", who)   // yes
//	log.Info(fmt.Sprintf("created task %s", id))              // never
//
// Loggers are obtained through Get, which binds a component= attribute so
// every line says which subsystem emitted it.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// Format selects the handler a configured logger writes through.
type Format string

const (
	// FormatConsole emits fixed columns — time, level, component, event —
	// with the rest dimmed, and colours them when the sink is a terminal.
	// The default, because the default reader of a `crewlet run` is a
	// person watching it.
	FormatConsole Format = "console"
	// FormatJSON emits one JSON object per line — the machine-readable
	// choice for anything shipping logs somewhere.
	FormatJSON Format = "json"
	// FormatText emits slog's key=value text: every field self-describing
	// on one line, which is what makes it greppable with no parser.
	FormatText Format = "text"
)

// Formats is the closed set, shared by the config validator and the
// generated JSON Schema so an editor cannot offer a format the engine
// refuses.
var Formats = []Format{FormatConsole, FormatText, FormatJSON}

// Valid reports whether f is a format this build can install.
func (f Format) Valid() bool {
	for _, known := range Formats {
		if f == known {
			return true
		}
	}
	return false
}

// Level is a log level as an OPERATOR writes it — the spelling that appears
// in a config file, a flag or an environment variable.
//
// It exists beside [slog.Level] because that type decodes from text through
// encoding.TextUnmarshaler, which yaml.v3 does not consult: a config field
// typed as slog.Level would silently decode to zero (info) for every value
// including "debug", which is the exact bug this package is being changed to
// fix.
type Level string

// The four levels the engine emits at.
const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Levels is the closed set, shared by the config validator and the schema.
var Levels = []Level{LevelDebug, LevelInfo, LevelWarn, LevelError}

// Valid reports whether l is a level this build understands. "warning" is
// deliberately NOT valid here even though [ParseLevel] accepts it: this is
// the set a config file is checked against and a schema offers, and two
// spellings of one level in an editor's completion list is a choice nobody
// benefits from making.
func (l Level) Valid() bool {
	for _, known := range Levels {
		if l == known {
			return true
		}
	}
	return false
}

// Slog maps the operator's spelling onto the level the handler compares
// against, via [ParseLevel] — so an unset value is info.
func (l Level) Slog() slog.Level { return ParseLevel(string(l)) }

// root holds the process-wide base handler. It is swapped atomically by
// Configure so a late reconfiguration (CLI flags parsed after some package
// already grabbed a logger) reaches loggers handed out earlier.
var root atomic.Pointer[slog.Logger]

// sink is where the process writes its logs, held so [SetVerbosity] can
// rebuild the handler over the SAME writer.
var sink atomic.Pointer[io.Writer]

func init() {
	Configure(slog.LevelInfo, FormatConsole, os.Stderr)
}

// Configure installs the process-wide logging settings, DESTINATION INCLUDED.
//
// # The writer is a property of the PROCESS, and this is the only way to set it
//
// Which is why the level and format have [SetVerbosity] of their own. A
// command decides how loud it should be from its own flags; it does not
// decide where a process's logs go, and one that installed a writer it had
// been handed made the global depend on its caller.
//
// That is not a hypothetical tidiness argument. `crewlet`'s own `run` took
// `stderr` as an argument — so it could be tested — and then installed that
// argument as the process-wide sink, which under `go test` meant 29 parallel
// tests each pointing the global at their own bytes.Buffer. Every test's log
// lines went to whichever buffer was installed last, racing that test's own
// writes to it. Under -race it was a hard failure; without it, one test
// asserting on another's output.
//
// Called once from the CLI entry point — and from a TestMain, which is the
// other legitimate owner of a process.
func Configure(level slog.Level, format Format, w io.Writer) {
	sink.Store(&w)
	install(level, format, w)
}

// SetVerbosity changes how much is logged and in what shape, on the
// destination already installed.
//
// FORMAT TRAVELS WITH LEVEL because both are invocation properties: they come
// off the same flags (`-log-level`, `-log-format`, `$CREWLET_LOG_LEVEL`) and
// neither says anything about where the bytes go. The writer deliberately
// cannot be changed here — see [Configure] for what that cost.
//
// A no-op before the first Configure, which cannot happen: this package's own
// init installs os.Stderr.
func SetVerbosity(level slog.Level, format Format) {
	w := sink.Load()
	if w == nil {
		return
	}
	install(level, format, *w)
}

func install(level slog.Level, format Format, w io.Writer) {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch format {
	case FormatJSON:
		h = slog.NewJSONHandler(w, opts)
	case FormatText:
		h = slog.NewTextHandler(w, opts)
	default:
		// CONSOLE IS THE FALLBACK as well as the default: an unset format
		// reaches here from this package's own init, and a person is the
		// likeliest reader of a stream nobody has said anything about.
		// Whether it colours is decided from w — see [newConsoleHandler].
		h = newConsoleHandler(w, level, colorFromEnv())
	}
	l := slog.New(h)
	root.Store(l)
	slog.SetDefault(l)
}

// ParseLevel maps an operator-supplied level name onto a slog.Level.
// Unknown names resolve to info rather than failing: a typo in a log level
// must never be the reason a company will not boot.
func ParseLevel(name string) slog.Level {
	level, _ := ParseLevelName(name)
	return level
}

// ParseLevelName is [ParseLevel], and additionally reports whether the name
// was one this build knows.
//
// # Falling back is right; falling back in SILENCE is not
//
// Every level and format this package parses fails soft, and that is
// deliberate — a misspelled log level must never be why a company will not
// boot. But a soft failure nobody is told about is how `debug: true` spent
// its whole life doing nothing: the operator sees the behaviour they did not
// ask for and has nothing pointing at the reason. A caller that can name the
// source of the value (a flag, an environment variable) says so instead.
//
// An empty name is RECOGNISED: nothing was said, and the default is the
// correct answer to that rather than a fallback from a mistake.
func ParseLevelName(name string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, true
	case "warn", "warning":
		// "warning" is accepted here and refused in a config file — see
		// [Level.Valid]. This is the path that may not fail.
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	case "info", "":
		return slog.LevelInfo, true
	default:
		return slog.LevelInfo, false
	}
}

// ParseFormat maps an operator-supplied format name onto a Format.
//
// AN UNRECOGNISED NAME IS CONSOLE, the default — the same never-fail rule
// [ParseLevel] follows, and for the same reason: this is the flag and
// environment-variable path, where a typo must never be why a company will
// not boot. It resolves to the default rather than to JSON because falling
// back to a format the operator did NOT ask for and cannot read is how a
// typo goes unnoticed; a config file's `logging.format` is checked against
// [Formats] and refused outright, which is where a typo should surface.
func ParseFormat(name string) Format {
	format, _ := ParseFormatName(name)
	return format
}

// ParseFormatName is [ParseFormat], and additionally reports whether the
// name was one this build knows — see [ParseLevelName] for why a caller
// wants to be told.
func ParseFormatName(name string) (Format, bool) {
	switch f := Format(strings.ToLower(strings.TrimSpace(name))); {
	case f == "":
		return FormatConsole, true
	case f.Valid():
		return f, true
	default:
		return FormatConsole, false
	}
}

// Get returns a logger bound to component. The name is a dotted subsystem
// path — "queue.memory", "seat.host", "agent.turn". The vocabulary is stable
// on purpose: operator runbooks and log queries are written against these
// names, so renaming one breaks a query nothing in this repo can see.
//
// # It resolves the root on every record, not once here
//
// Almost every caller is a PACKAGE-LEVEL VAR:
//
//	var log = logging.Get("store")
//
// which runs at package init — before main has parsed a flag. A logger that
// captured the root here would be bound to the boot default for the life of
// the process, so `-log-level debug` would reach nothing: the only lines
// affected would be the ones emitted by loggers obtained after Configure,
// which is a handful of them. Every subsystem would keep logging at info
// with no indication why.
func Get(component string) *slog.Logger {
	return slog.New(lazy{}).With("component", component)
}

// lazy is a handler that forwards to whatever root is current.
//
// It records the WithAttrs / WithGroup calls made on it and replays them
// onto the current root's handler per record, rather than binding one. That
// is what makes Configure's swap reach a logger handed out at init — which
// is what [root] has always claimed to do.
type lazy struct {
	ops []func(slog.Handler) slog.Handler
}

func (l lazy) resolve() slog.Handler {
	h := root.Load().Handler()
	for _, op := range l.ops {
		h = op(h)
	}
	return h
}

// Enabled asks the root handler DIRECTLY, without replaying the ops.
//
// This is the hot path — it is consulted for every suppressed line, so a
// debug call in a loop pays it whether or not anything is emitted — and the
// replay would allocate a handler per call to answer a question that does
// not depend on attributes. Configure only ever builds slog's own text and
// JSON handlers and this package's [consoleHandler], all three of which
// answer Enabled from their level and nothing else. A HANDLER WHOSE Enabled
// CONSULTED ITS ATTRIBUTES WOULD BREAK THIS, silently and only for the
// lines it was supposed to filter.
func (l lazy) Enabled(ctx context.Context, level slog.Level) bool {
	return root.Load().Handler().Enabled(ctx, level)
}

// Handle injects the trace correlation bound onto ctx, then forwards.
//
// # Why here and not inside install
//
// This is the one point every format passes through, so all three carry the
// ids and none of them has to know about tracing. Wrapping the handlers
// inside [install] instead would give console, text and json the same
// concrete type, and TestEveryDeclaredFormatInstallsItsOwnHandler asserts
// they do not — it keys on the handler's %T precisely so a format cannot
// silently render as another.
//
// [lazy.Enabled] is deliberately NOT touched: it must answer from the level
// alone (see its doc), and a level that varied by whether a
// trace happened to be bound would filter different lines depending on which
// spelling the call site used.
//
// The record is CLONED before attributes are added, which is what slog's
// Record doc prescribes for a handler that adds any: "Copies of a Record share
// state. Do not modify a Record after handing out a copy to it. Use
// Record.Clone to create a copy with no shared state."
//
// Be honest about what that buys TODAY: nothing observable. This handler
// forwards to exactly one resolved chain, and the caller (slog.Logger.log)
// discards its record afterwards, so mutating in place cannot currently be
// seen — removing the Clone breaks no test, which was checked rather than
// assumed. It is kept because it is the documented contract for this exact
// operation and it costs one allocation on traced lines only; it becomes
// load-bearing the moment anything hands the record to more than one place.
func (l lazy) Handle(ctx context.Context, r slog.Record) error {
	if attrs := attrsFor(ctx); len(attrs) > 0 {
		r = r.Clone()
		r.AddAttrs(attrs...)
	}
	return l.resolve().Handle(ctx, r)
}

func (l lazy) WithAttrs(attrs []slog.Attr) slog.Handler {
	return l.with(func(h slog.Handler) slog.Handler { return h.WithAttrs(attrs) })
}

func (l lazy) WithGroup(name string) slog.Handler {
	return l.with(func(h slog.Handler) slog.Handler { return h.WithGroup(name) })
}

// with appends one op, COPYING the slice.
//
// slog hands the same handler to several derived loggers — every `log.With`
// on a shared package logger starts from this one — so appending in place
// would let one derivation's attributes appear on another's lines whenever
// the backing array had spare capacity.
func (l lazy) with(op func(slog.Handler) slog.Handler) slog.Handler {
	ops := make([]func(slog.Handler) slog.Handler, len(l.ops), len(l.ops)+1)
	copy(ops, l.ops)
	return lazy{ops: append(ops, op)}
}
