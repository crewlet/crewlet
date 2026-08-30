package jetstream

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/crewlet/crewlet/internal/logging"
)

// natsLogger adapts nats-server's printf-style logger onto the engine's
// structured one.
//
// # Why this exists
//
// nats-server logs through an interface it will not install for you: a
// server built with [server.NewServer] and never handed a logger has
// `s.logging.logger == nil`, and its own `executeLogCall` returns early on
// that — SILENTLY. The embedded broker is the DEFAULT event stream, so
// until this bridge existed every diagnostic the default backend produced
// went nowhere: JetStream write errors, slow-consumer warnings, stream
// recovery after an unclean shutdown, cluster election trouble. Nothing
// looked wrong, because nothing was printed. `opts.NoLog` is left false the
// whole time, so the configuration even reads as though logging were on.
//
// The lines land under their own component, the way `queue.pulsar.client`
// already separates a third-party client's chatter from the engine's own:
// an operator can tell what the BROKER said from what the engine said about
// it.
//
// # Levels
//
// `Noticef` is where nats-server puts its boot narration — "Starting
// nats-server", the JetStream storage line, "Server is ready" — a dozen or
// so lines per start, describing a broker the operator deliberately did not
// deploy. The engine already announces its own stream coming up, so these
// are DEBUG: available when someone is diagnosing a broker, absent when
// nobody asked for one. Everything that reports something WRONG keeps its
// severity, which is the half that was actually missing.
//
// `Fatalf` maps to Error and DOES NOT EXIT, for the same reason
// `Options.NoSigs` is set beside it: the engine owns this process, and a
// library killing it mid-drain loses the running turn and the lease release
// with it. nats-server does not depend on the exit — every `s.Fatalf` call
// site returns immediately after it (see e.g. its route.go listener setup),
// which is what makes a non-exiting Fatalf safe rather than merely polite.
type natsLogger struct{ log *slog.Logger }

// newNATSLogger builds the bridge, and reports whether nats-server should
// bother calling Debugf at all.
//
// The debug flag is read ONCE, here, because that is when the server is
// built. `crewlet run` configures logging from its flags before it brings
// the stream up, so the value is the operator's, not the package default.
func newNATSLogger() (natsLogger, bool) {
	log := logging.Get("queue.jetstream.server")
	return natsLogger{log: log}, log.Enabled(context.Background(), slog.LevelDebug)
}

// The event name is the same for every line and the severity rides on the
// level, because the engine's rule is that an event name is a short
// machine-parsable token rather than a sentence — and nats-server hands us
// nothing but a sentence. It goes under `detail`, where the rest of the
// engine's dynamic data goes, so `event=nats_server` finds every broker
// line and the level narrows it.
func (l natsLogger) emit(level slog.Level, format string, v ...any) {
	// Enabled is checked before Sprintf so a suppressed line does not pay
	// to format itself. nats-server calls Debugf per protocol event when
	// debug is on, and this is the guard that keeps the off case free.
	if !l.log.Enabled(context.Background(), level) {
		return
	}
	l.log.Log(context.Background(), level, "nats_server", "detail", fmt.Sprintf(format, v...))
}

func (l natsLogger) Noticef(format string, v ...any) { l.emit(slog.LevelDebug, format, v...) }
func (l natsLogger) Warnf(format string, v ...any)   { l.emit(slog.LevelWarn, format, v...) }
func (l natsLogger) Errorf(format string, v ...any)  { l.emit(slog.LevelError, format, v...) }
func (l natsLogger) Debugf(format string, v ...any)  { l.emit(slog.LevelDebug, format, v...) }
func (l natsLogger) Tracef(format string, v ...any)  { l.emit(slog.LevelDebug, format, v...) }

// Fatalf is an Error line and RETURNS. See the type doc: exiting here would
// take the engine down from inside a library, which is the exact failure
// `NoSigs` exists to prevent.
func (l natsLogger) Fatalf(format string, v ...any) { l.emit(slog.LevelError, format, v...) }
