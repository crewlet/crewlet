package jetstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nats-io/nats-server/v2/server"

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
// ONE ERROR LINE IS DEBUG TOO: nats-server's own report of a connection its
// authenticator refused, `authentication error - User "<name>"` or the bare
// sentinel, once PER REFUSED ATTEMPT, carrying whatever username the peer sent
// (server/client.go authViolation, v2.15.1). The authenticator already reports
// every refusal — counted, and logged as broker_peer_refused once a minute a
// class with the key it was presented — so this second report of one event
// would drift from the first, and at ERROR it is an unbounded stream at the
// rate any peer chooses, of text that peer wrote. Recognised by the sentinel,
// [server.ErrAuthentication], among its arguments or as the format's tail —
// never the whole format, which a client's own label precedes. The DIALER'S
// side of a refusal keeps its level ("Route Error 'Authorization Violation'"):
// that is this node failing to join, bounded by the route's own back-off, and
// the operator of this node is the one who needs it.
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
// # Fatalf is a declaration, not a line
//
// `Fatalf` is logged at Error and DOES NOT EXIT, for the same reason
// `Options.NoSigs` is set beside it: the engine owns this process, and a
// library killing it from inside loses whatever the process was in the middle
// of. But it is not merely an Error line either. Stock nats-server's logger
// exits on it, so nats-server never had to make a Fatalf mean anything else:
// it calls one where it cannot go on and RETURNS from the step it was on —
// JetStream that could not take its store, a listener that could not bind —
// leaving a server that is half started and will stay that way. One call site
// does not even return (server.go's "Not allowed to enable JetStream on the
// system account", v2.15.1), so the server carries on past a failure it
// called fatal. Read as an Error line alone, a refused store made the boot
// wait out its whole readiness budget for a server that could never be ready.
//
// So every Fatalf also DECLARES the broker's death ([Death]), and the process
// owner decides what that means: during the boot [startEmbedded] refuses at
// once with nats-server's own reason, and after it the engine ends the node,
// as stock nats-server would have — see [Server.Death] for why a drain is not
// the answer.
type natsLogger struct {
	log *slog.Logger
	// death is where a Fatalf is declared: the server's own, built beside
	// it so the declaration reaches whoever started this server.
	death *Death
}

// newNATSLogger builds the bridge, BOUND TO THE MEMBER IT SPEAKS FOR.
//
// It takes no verbosity of its own: whether nats-server calls Debugf at all
// is [Config.Debug]'s answer, and whether a line that reaches here is
// RECORDED is the sink's — [natsLogger.emit] asks it per line, so a level
// raised after the server was built still takes effect.
//
// # Why the server name is not optional
//
// Because more than one broker can run in ONE process, and when that happens
// every line here was indistinguishable. A fleet test stands up n engines each
// embedding its own member, and both of them logged under one
// `component=queue.nats.server` with nothing naming which — so "route
// connection refused" and "has NO quorum, stalled" could not be attributed to
// a member, and the one question a reader has about a fleet that did not form
// is WHICH MEMBER. It costs one attribute per line and it is the difference
// between a log and a transcript of two servers talking over each other.
//
// Empty is a legitimate value and rendered as such rather than omitted: a
// solo embedded broker has no name to carry, and a blank attribute says "one
// member" where a missing one would say "this build does not report it".
func newNATSLogger(serverName string, death *Death) natsLogger {
	return natsLogger{log: logging.Get("queue.nats.server").With("server", serverName), death: death}
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
func (l natsLogger) Debugf(format string, v ...any)  { l.emit(slog.LevelDebug, format, v...) }
func (l natsLogger) Tracef(format string, v ...any)  { l.emit(slog.LevelDebug, format, v...) }

// Errorf is an Error line, but for the server's own report of a connection its
// authenticator refused, which is DEBUG — see the type doc's "Levels".
func (l natsLogger) Errorf(format string, v ...any) {
	level := slog.LevelError
	if refusedAuthentication(format, v) {
		level = slog.LevelDebug
	}
	l.emit(level, format, v...)
}

// refusedAuthentication reports whether a line is nats-server's report of an
// authentication it refused: [server.ErrAuthentication] handed in as an
// argument, or its text closing the format — a client's Errorf prefixes the
// format with the connection's own label, so the bare form is the TAIL.
func refusedAuthentication(format string, v []any) bool {
	for _, arg := range v {
		if err, ok := arg.(error); ok && errors.Is(err, server.ErrAuthentication) {
			return true
		}
	}
	return strings.HasSuffix(format, server.ErrAuthentication.Error())
}

// Fatalf is an Error line and a declaration of the broker's death, and it
// RETURNS. See the type doc: exiting here would take the engine down from
// inside a library, which is the exact failure `NoSigs` exists to prevent, and
// logging alone left a boot waiting on a server that had already given up.
func (l natsLogger) Fatalf(format string, v ...any) {
	l.emit(slog.LevelError, format, v...)
	l.death.declare(format, v...)
}
