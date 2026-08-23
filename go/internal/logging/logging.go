// Package logging is the one place a Crewlet logger is built.
//
// Every log line in the engine is structured: a short machine-parsable
// snake_case event name plus key/value attributes. Dynamic data never goes
// into the message string, so a log stream stays greppable by event name and
// filterable by field. This mirrors the discipline the Python engine holds
// with structlog (see src/crewlet/_logging.py) and carries the same rule:
//
//	log.Info("task_created", "task_id", id, "creator", who)   // yes
//	log.Info(fmt.Sprintf("created task %s", id))              // never
//
// Loggers are obtained through Get, which binds a component= attribute so
// every line says which subsystem emitted it.
package logging

import (
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
func Get(component string) *slog.Logger {
	return root.Load().With("component", component)
}
