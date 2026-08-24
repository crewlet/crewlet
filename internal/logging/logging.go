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
	// FormatJSON emits one JSON object per line — the machine-readable
	// default for anything shipping logs somewhere.
	FormatJSON Format = "json"
	// FormatText emits slog's key=value text, which is easier to read
	// while developing.
	FormatText Format = "text"
)

// root holds the process-wide base handler. It is swapped atomically by
// Configure so a late reconfiguration (CLI flags parsed after some package
// already grabbed a logger) reaches loggers handed out earlier.
var root atomic.Pointer[slog.Logger]

func init() {
	Configure(slog.LevelInfo, FormatText, os.Stderr)
}

// Configure installs the process-wide logging settings. It is called once
// from the CLI entry point, before the engine starts.
func Configure(level slog.Level, format Format, w io.Writer) {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch format {
	case FormatJSON:
		h = slog.NewJSONHandler(w, opts)
	default:
		h = slog.NewTextHandler(w, opts)
	}
	l := slog.New(h)
	root.Store(l)
	slog.SetDefault(l)
}

// ParseLevel maps an operator-supplied level name onto a slog.Level.
// Unknown names resolve to info rather than failing: a typo in a log level
// must never be the reason a company will not boot.
func ParseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ParseFormat maps an operator-supplied format name onto a Format,
// defaulting to JSON for anything unrecognised.
func ParseFormat(name string) Format {
	if strings.EqualFold(strings.TrimSpace(name), string(FormatText)) {
		return FormatText
	}
	return FormatJSON
}

// Get returns a logger bound to component. The name is a dotted subsystem
// path — "queue.memory", "seat.host", "agent.turn" — matching the
// get_logger() names the Python engine uses, so operator runbooks and log
// queries keep working across the rewrite.
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
// JSON handlers, whose Enabled reads the level from their options and
// nothing else.
func (l lazy) Enabled(ctx context.Context, level slog.Level) bool {
	return root.Load().Handler().Enabled(ctx, level)
}

func (l lazy) Handle(ctx context.Context, r slog.Record) error {
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
