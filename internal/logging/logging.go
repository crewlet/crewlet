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
// # The writer is a property of the PROCESS, and this is the only way to set it
//
// Which is why the level and format have [SetVerbosity] of their own, and the
// log file has [SetFile]. A command decides how loud it should be from its own
// flags; it does not decide where a process's logs go, and one that installed
// a writer it had been handed made the global depend on its caller.
//
// That is not a hypothetical tidiness argument. `crewlet`'s own `run` took
// `stderr` as an argument — so it could be tested — and then installed that
// argument as the process-wide sink, which under `go test` meant 29 parallel
// tests each pointing the global at their own bytes.Buffer. Every test's log
// lines went to whichever buffer was installed last, racing that test's own
// writes to it. Under -race it was a hard failure; without it, one test
// asserting on another's output.
//
// IT ALSO CLEARS ANY LOG FILE, because it is the statement of what this
// process's destinations ARE, not an adjustment to them. A caller adding a
// file to the console sink wants [SetFile].
//
// Called once from the CLI entry point — and from a TestMain, which is the
// other legitimate owner of a process.
func Configure(level slog.Level, format Format, w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	current = settings{level: level, format: format, console: w}
	install(current)
}

// SetVerbosity changes how much is logged and in what shape, on the
// destinations already installed.
//
// FORMAT TRAVELS WITH LEVEL because both are invocation properties: they come
// off the same flags (`-log-level`, `-log-format`, `$CREWLET_LOG_LEVEL`) and
// neither says anything about where the bytes go. The writers deliberately
// cannot be changed here — see [Configure] for what that cost.
//
// A file sink given no format or level of its own follows these, so a node
// that switches to `json` for a shipper, or turns `-debug` on, switches both
// destinations at once. One given its own keeps it.
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
// # Why a second sink rather than a replacement, or an io.MultiWriter
//
// A file is added to stderr, never instead of it: stderr is the only sink
// that exists before the config has been read, it is what a container
// platform captures and what an operator watching a boot is looking at, and
// a node that went quiet there the moment a file was configured would look
// like one that had stopped. A deployment that genuinely wants the file
// alone redirects stderr, which is a decision it can make and this package
// cannot.
//
// And two sinks rather than one io.MultiWriter over both, because the
// console format decides its colour and its timestamp shape FROM the writer
// (see [newConsoleHandler]): a MultiWriter is not a terminal, so tee-ing
// would silently take the colour off an operator's terminal — or, forced
// back on, write ANSI escapes into the file. Each destination gets its own
// handler, which is also what lets the file carry `json` for a shipper while
// the terminal keeps its columns.
//
// format empty means "whatever [SetVerbosity] last chose", which is what
// makes `-log-format` reach both sinks.
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
// # Off is only meaningful beside a log file, and this is not where that is enforced
//
// A process with no destination at all logs nowhere, which is never what
// anybody meant, so [install] keeps the console when switching it off would
// leave nothing — and says so, once, on the console it just kept. That is a
// backstop rather than the rule: the Tier A validator refuses
// `logging.stderr: false` with no file by name, and `crewlet run` refuses the
// same combination arrived at through `-log-file ""`, because an operator
// told what is wrong with their document can fix it and one silently
// overridden cannot.
//
// # What it does NOT silence
//
// Three things reach stderr without passing through here, and all three stay:
// the lines emitted before the Tier A document has been read (the log file is
// named BY that document, so it cannot be open yet), the seat watchdog's
// hard-exit notice, which writes to os.Stderr directly because a wedged
// process has not earned a configured handler, and this package's own report
// when the log file cannot be written. That is the whole reason this is a
// config field rather than advice to redirect stderr: a shell redirect throws
// those away too, and they are the three an operator most needs.
func SetConsole(enabled bool) {
	mu.Lock()
	defer mu.Unlock()
	if current.console == nil {
		return
	}
	current.consoleOff = !enabled
	install(current)
}

// install builds one handler per destination and publishes them.
//
// ONE SINK IS INSTALLED UNWRAPPED, deliberately: the overwhelming majority of
// runs have no log file, and a fan-out around a single handler would put an
// indirection on every record to no purpose — and would hide which handler a
// format installed from the test that asserts each format has its own. A node
// that silenced its console in favour of a file gets the same unwrapped
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
	l := slog.New(h)
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
// # Enabled answers from the level and nothing else
//
// Which is the same contract every other handler this package installs
// keeps, and for the same reason: [lazy.Enabled] consults the root handler
// directly, without replaying the recorded attribute ops, so a handler whose
// Enabled depended on anything but the level would filter different lines
// depending on how the call site was spelled.
//
// # And the level is the MOST VERBOSE of its children, never an average
//
// Destinations may differ in level — a `debug` file behind a `warn` console
// is the point of [FileSink.Level] — and slog asks the ROOT handler first:
// a record that root refuses never reaches Handle at all. So root has to
// admit anything ANY destination would take, and each child then filters
// with its own [slog.Handler.Enabled] inside [fanout.Handle]. Taking the
// quieter level here would silently make the verbose destination a lie.
//
// The visible consequence, and it is the right one: `log.Enabled(ctx,
// LevelDebug)` now answers "will this be recorded anywhere", so a call site
// guarding an expensive debug computation does the work when only the file
// wants it. That is what the operator asked for by asking for a debug file.
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
// JSON handlers, this package's [consoleHandler], and the [fanout] over them
// when more than one destination is installed — all four of which answer
// Enabled from their level and nothing else. A HANDLER WHOSE Enabled
// CONSULTED ITS ATTRIBUTES WOULD BREAK THIS, silently and only for the lines
// it was supposed to filter.
//
// The fan-out answers from the MOST VERBOSE of its destinations, which is
// what keeps this shortcut correct once destinations differ in level: a line
// refused here never reaches Handle, so anything any destination would take
// has to be admitted here and filtered there.
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
// It costs one allocation on traced lines only, and it is no longer merely
// the documented contract: with a log file installed the resolved chain is a
// [fanout], which hands the record to more than one handler — the exact
// situation that doc paragraph names. fanout clones again per destination,
// for its own half of the same reason.
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
