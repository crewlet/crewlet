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
// The lines land under their own component, the way every other client
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
// # The broker's OWN debug is a separate question, and [Config.Debug] asks it
//
// `Debugf` is a different population from the narration above, and a far
// larger one: nats-server calls it per internal-client lifecycle event, and
// the engine's own coordination reads manufacture those by the dozen. One
// `ListKeys` on a KV bucket is an ordered ephemeral consumer created and
// deleted, and deleting a consumer closes the two internal JetStream clients
// it was built on — so every key listing emits exactly two "JetStream
// connection closed: Client Closed" lines. Two of the node's 15-second duty
// loops list keys on every tick, so an idle solo node produces a steady
// stream of them before anything happens at all.
//
// That flag used to be read off this logger's own level, which made
// `-debug` — asked for to watch turns, prompts and tool calls — also
// subscribe the operator to a broker they never deployed. They are two
// questions and they now have two knobs: the level decides whether the
// ENGINE is verbose, `stream.debug` decides whether the BROKER is. Warnings
// and errors are unaffected by both and always reach the sink.
//
// `Fatalf` maps to Error and DOES NOT EXIT, for the same reason
// `Options.NoSigs` is set beside it: the engine owns this process, and a
// library killing it mid-drain loses the running turn and the lease release
// with it. nats-server does not depend on the exit — every `s.Fatalf` call
// site returns immediately after it (see e.g. its route.go listener setup),
// which is what makes a non-exiting Fatalf safe rather than merely polite.
type natsLogger struct{ log *slog.Logger }

// newNATSLogger builds the bridge.
//
// It takes no verbosity of its own: whether nats-server calls Debugf at all
// is [Config.Debug]'s answer, and whether a line that reaches here is
// RECORDED is the sink's — [natsLogger.emit] asks it per line, so a level
// raised after the server was built still takes effect.
func newNATSLogger() natsLogger {
	return natsLogger{log: logging.Get("queue.nats.server")}
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
