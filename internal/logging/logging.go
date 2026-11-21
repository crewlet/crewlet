// Package logging is the one place a Crewlet logger is built.
//
// Every log line in the engine is structured: a short machine-parsable
// snake_case event name plus key/value attributes. Dynamic data never goes
// into the message string, so a log stream stays greppable by event name and
// filterable by field. The rule that buys that:
//
//	log.Info("agent_phase_started", "task_id", id, "creator", who)   // yes
//	log.Info(fmt.Sprintf("created task %s", id))              // never
//
// Loggers are obtained through Get, which binds a component= attribute so
// every line says which subsystem emitted it.
package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
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
func (f Format) Valid() bool { return slices.Contains(Formats, f) }

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
func (l Level) Valid() bool { return slices.Contains(Levels, l) }

// Slog maps the operator's spelling onto the level the handler compares
// against, via [ParseLevel] — so an unset value is info.
func (l Level) Slog() slog.Level { return ParseLevel(string(l)) }

// root holds the process-wide base handler. It is swapped atomically by
// Configure so a late reconfiguration (CLI flags parsed after some package
// already grabbed a logger) reaches loggers handed out earlier.
var root atomic.Pointer[slog.Logger]

// FileSink is the log file destination and everything that may differ about
// it from the console one.
//
// Both overrides default to "follow the process", because the ordinary case
// is one log written twice. They exist for the two deployments that genuinely
// want a split: a shipper reading `json` out of the file while a person reads
// columns on the terminal, and a durable `debug` record kept behind a `warn`
// console (or the reverse — a small file behind a loud terminal).
type FileSink struct {
	// Writer is the file. Nil removes the destination.
	Writer io.Writer
	// Format is the shape written here. Empty follows the process format,
	// so `-log-format json` reaches both destinations.
	Format Format
	// Level is how loud this destination is. Nil follows the process
	// level.
	//
	// A POINTER because [slog.LevelInfo] is 0: a plain slog.Level could
	// not tell "info" from "nothing was said", and the two must differ —
	// `logging.level: warn` with an unset file level means a warn file,
	// not an info one.
	Level *slog.Level
}

// settings is everything the installers decide between them: how loud this
// process is, in what shape, and WHERE it writes.
type settings struct {
	level   slog.Level
	format  Format
	console io.Writer
	// consoleOff silences the console destination — see [SetConsole]. It
	// is spelled OFF rather than on so the zero value is the default
	// every process starts at, which is what makes an unset field in this
	// struct mean the safe thing.
	consoleOff bool
	file       FileSink
}

// current holds them, behind a mutex rather than an atomic pointer.
//
// Each of [Configure], [SetVerbosity], [SetFile] and [SetConsole] is a
// READ-MODIFY-WRITE of this value — SetFile keeps the level the flags chose,
// SetVerbosity keeps the file the config named — and two of them racing on an
// atomic pointer would silently drop whichever landed first. Nothing on the
// logging path reads it: a record resolves [root], which stays an atomic
// pointer for exactly that reason.
var (
	mu      sync.Mutex
	current settings
)

func init() {
	Configure(slog.LevelInfo, FormatConsole, os.Stderr)
}

// Configure installs the process-wide logging settings, DESTINATION INCLUDED.
//
// The writer is a property of the PROCESS, and this is the only way to set it:
// the level and format have [SetVerbosity], the log file [SetFile]. A command
// decides how loud to be from its flags, not where logs go; installing a
// writer it was handed makes the global depend on its caller, and parallel
// tests then race on whichever buffer was installed last.
//
// IT ALSO CLEARS ANY LOG FILE: it states what the destinations ARE. A caller
// adding a file wants [SetFile]. Called once from the CLI entry point and
// from a TestMain.
func Configure(level slog.Level, format Format, w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	current = settings{level: level, format: format, console: w}
	install(current)
}

// SetVerbosity changes how much is logged and in what shape, on the
// destinations already installed.
//
// FORMAT TRAVELS WITH LEVEL: both come off the same invocation flags and say
// nothing about where bytes go. Writers cannot be changed here ([Configure]).
//
// A file sink given no format or level of its own follows these; one given its
// own keeps it.
//
// A no-op before the first Configure, which cannot happen: this package's own
// init installs os.Stderr.
func SetVerbosity(level slog.Level, format Format) {
	mu.Lock()
	defer mu.Unlock()
	if current.console == nil {
		return
	}
	current.level, current.format = level, format
	install(current)
}

// SetFile installs the log file as a SECOND destination beside the console
// one, or removes it when w is nil.
//
// A file is added to stderr, never instead of it: stderr exists before the
// config is read and is what a container platform captures. A deployment
// wanting the file alone redirects stderr.
//
// Two sinks, not one io.MultiWriter: the console format picks its colour FROM
// the writer ([newConsoleHandler]), and a MultiWriter is not a terminal, so
// colour would vanish or ANSI escapes land in the file. Each destination has
// its own handler, so the file can carry `json`. An empty format means
// "whatever [SetVerbosity] last chose".
func SetFile(s FileSink) {
	mu.Lock()
	defer mu.Unlock()
	if current.console == nil {
		return
	}
	current.file = s
	install(current)
}

// SetConsole turns the console destination on or off.
//
// Off is only meaningful beside a log file, and this is not where that is
// enforced: [install] keeps the console when switching it off would leave no
// destination, and says so once. That is a backstop; the Tier A validator
// refuses `logging.stderr: false` with no file, and `crewlet run` refuses the
// same via `-log-file ""`, because an operator told what is wrong can fix it.
//
// It does NOT silence what reaches stderr directly: lines before the Tier A
// document is read, the seat watchdog's hard-exit notice, and this package's
// report of an unwritable log file. A shell redirect would drop those too.
func SetConsole(enabled bool) {
	mu.Lock()
	defer mu.Unlock()
	if current.console == nil {
		return
	}
	current.consoleOff = !enabled
	install(current)
}

// install builds one handler per destination and publishes them, behind the
// redaction every record passes through ([redacting]).
//
// ONE SINK IS INSTALLED WITHOUT A FAN-OUT, deliberately: the overwhelming
// majority of runs have no log file, and a fan-out around a single handler
// would put an indirection on every record to no purpose, and would hide which
// handler a format installed from the test that asserts each format has its
// own. A node that silenced its console in favour of a file gets the same
// treatment, for the same reason.
func install(s settings) {
	var handlers []slog.Handler
	// admits is the level the ROOT answers Enabled from: the most verbose
	// of the destinations, because a line any destination would take has
	// to get past [lazy.Enabled] to reach the fan-out that filters it —
	// see [fanout].
	admits := slog.Level(0)
	add := func(h slog.Handler, level slog.Level) {
		if len(handlers) == 0 || level < admits {
			admits = level
		}
		handlers = append(handlers, h)
	}

	if !s.consoleOff {
		add(handlerFor(s.format, s.console, s.level, colorFromEnv()), s.level)
	}
	if s.file.Writer != nil {
		format := s.file.Format
		if format == "" {
			format = s.format
		}
		level := s.level
		if s.file.Level != nil {
			level = *s.file.Level
		}
		// NEVER COLOURED, whatever $CREWLET_LOG_COLOR says. `auto`
		// declines on its own — the sink is a [File], not a terminal —
		// but `always` is an instruction about a STREAM SOMEBODY IS
		// WATCHING that a CI viewer renders without being a terminal,
		// and nobody is watching a file. Honouring it here would put
		// escape bytes in front of every line a shipper parses and every
		// line `grep` prints — which is the exact failure [SetFile]'s own
		// doc names as the reason these are two handlers rather than one
		// io.MultiWriter.
		add(handlerFor(format, s.file.Writer, level, ColorNever), level)
	}
	// A PROCESS WITH NO DESTINATION LOGS NOWHERE, which is never what
	// anybody asked for — `logging.stderr: false` is a statement about the
	// file taking over, not about going silent. Both layers that can say
	// so refuse the combination by name (see [SetConsole]); this is the
	// backstop for the path neither of them sees, and it is loud.
	silent := len(handlers) == 0
	if silent {
		add(handlerFor(s.format, s.console, s.level, colorFromEnv()), s.level)
	}

	h := handlers[0]
	if len(handlers) > 1 {
		h = fanout{level: admits, handlers: handlers}
	}
	l := slog.New(redacting{next: h})
	root.Store(l)
	slog.SetDefault(l)

	if silent {
		// NOT THROUGH THE LOGGER. The handler just built filters by
		// s.level, so at `logging.level: error` the one line explaining
		// the override would itself be dropped and the backstop would
		// fire in complete silence — which is the failure it exists to
		// prevent, one level up. Printed directly for the same reason
		// [File.note] and the CLI's stderr handover are.
		fmt.Fprint(s.console, "crewlet: console_kept_open — the console was "+
			"switched off with no log file installed, which would leave this "+
			"process logging nowhere; keeping stderr\n")
	}
}

// handlerFor builds the handler one format writes one destination through.
//
// THE COLOUR MODE IS THE CALLER'S, not this function's. It used to read
// $CREWLET_LOG_COLOR for itself, which gave every destination the same
// answer — and `always` means "colour this stream even though it is not a
// terminal", which is true of a CI-captured stderr and false of a file on
// disk. [install] passes the environment's mode for the console and
// [ColorNever] for the file.
func handlerFor(format Format, w io.Writer, level slog.Level, mode ColorMode) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	switch format {
	case FormatJSON:
		return slog.NewJSONHandler(w, opts)
	case FormatText:
		return slog.NewTextHandler(w, opts)
	default:
		// CONSOLE IS THE FALLBACK as well as the default: an unset format
		// reaches here from this package's own init, and a person is the
		// likeliest reader of a stream nobody has said anything about.
		return newConsoleHandler(w, level, mode)
	}
}

// fanout writes one record to every installed destination.
//
// Enabled answers from the level alone, as every handler here does:
// [lazy.Enabled] skips replaying attribute ops, so anything else would filter
// by how the call site was spelled.
//
// That level is the MOST VERBOSE of its children, never an average: slog asks
// the ROOT first, so root must admit anything ANY destination would take (a
// `debug` file behind a `warn` console, [FileSink.Level]); each child then
// filters in [fanout.Handle]. So `log.Enabled(ctx, LevelDebug)` means
// "recorded anywhere".
type fanout struct {
	level    slog.Level
	handlers []slog.Handler
}

func (f fanout) Enabled(_ context.Context, level slog.Level) bool {
	return level >= f.level
}

// Handle writes the record to every destination, CLONING it for each.
//
// This is the case [slog.Record]'s own doc is about: "Copies of a Record
// share state. Do not modify a Record after handing out a copy to it." One
// record now reaches more than one handler, so the clone stopped being
// defensive and became the contract — see [lazy.Handle], which says so.
//
// EVERY DESTINATION IS TRIED, and the failures are joined rather than
// returned on the first: a full log disk must not be able to stop the same
// line reaching the terminal.
func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	return f.derive(func(h slog.Handler) slog.Handler { return h.WithAttrs(attrs) })
}

func (f fanout) WithGroup(name string) slog.Handler {
	return f.derive(func(h slog.Handler) slog.Handler { return h.WithGroup(name) })
}

// derive builds a new fanout over the derived children, into a NEW slice —
// the same aliasing rule [lazy.with] and [consoleHandler.clone] keep: slog
// hands one handler to every logger derived from it, and writing a child in
// place would put one derivation's attributes on another's lines.
func (f fanout) derive(op func(slog.Handler) slog.Handler) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = op(h)
	}
	return fanout{level: f.level, handlers: next}
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
// Every level and format here fails soft, but not in SILENCE: a soft failure
// nobody is told about leaves the operator with behaviour they did not ask for
// and no pointer to why. A caller that can name the value's source (a flag, an
// environment variable) says so. An empty name is RECOGNISED: the default is
// the right answer to nothing said, not a fallback from a mistake.
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
// AN UNRECOGNISED NAME IS CONSOLE, the default: the flag and environment path
// never fails (see [ParseLevel]), and falling back to JSON, a format the
// operator did not ask for, would hide the typo. A config file's
// `logging.format` is checked against [Formats] and refused outright.
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
// It resolves the root on every record, not once here: callers are almost all
// PACKAGE-LEVEL VARs (`var log = logging.Get("store")`) run at init, before
// main parses a flag. A logger capturing the root here would keep the boot
// default for the process's life, and `-log-level debug` would reach nothing.
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
// This is the hot path, consulted for every suppressed line, and replay would
// allocate a handler per call to answer a question that does not depend on
// attributes. Configure only builds slog's text and JSON handlers,
// [consoleHandler] and the [fanout] over them, all of which answer Enabled
// from their level alone. A HANDLER WHOSE Enabled CONSULTED ITS ATTRIBUTES
// WOULD BREAK THIS, silently. The fan-out answers from the MOST VERBOSE of its
// destinations, so a line any destination would take is admitted here.
func (l lazy) Enabled(ctx context.Context, level slog.Level) bool {
	return root.Load().Handler().Enabled(ctx, level)
}

// Handle injects the trace correlation bound onto ctx, then forwards.
//
// Here and not inside [install]: this is the one point every format passes
// through. Wrapping the handlers in install would give console, text and json
// one concrete type, and TestEveryDeclaredFormatInstallsItsOwnHandler keys on
// %T so a format cannot silently render as another.
//
// [lazy.Enabled] is deliberately NOT touched: it answers from the level alone.
// The record is CLONED before attributes are added, as slog's Record doc
// prescribes ("Copies of a Record share state"): with a log file the chain is
// a [fanout] handing it to several handlers. fanout clones again per child.
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
