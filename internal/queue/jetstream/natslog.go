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
// structured one, under its own component so the broker's lines stand apart
// from the engine's. Without it nats-server logs NOTHING: a server never
// handed a logger returns early from every log call, silently, though
// `opts.NoLog` reads false.
//
// Noticef is boot narration the engine already announces, so DEBUG; whatever
// reports something wrong keeps its severity, but one refused-authentication
// line ([natsLogger.Errorf]). Whether nats-server calls Debugf at all is
// [Config.Debug] (`stream.debug`), a knob apart from the engine's level: its
// lines run two per KV key listing, a steady stream on an idle node. Fatalf
// does not exit ([natsLogger.Fatalf]).
type natsLogger struct {
	log *slog.Logger
	// death is where a Fatalf is declared: the server's own, built beside
	// it so the declaration reaches whoever started this server.
	death *Death
}

// newNATSLogger builds the bridge, BOUND TO THE MEMBER IT SPEAKS FOR. It takes
// no verbosity: [natsLogger.emit] asks the sink per line, so a level raised
// after the server was built still takes effect.
//
// The server name is required because several brokers can share a process (a
// fleet test's members), and the question about a fleet that did not form is
// WHICH MEMBER said "has NO quorum". Empty is rendered, not omitted: a blank
// says "one member" (solo), a missing attribute "this build does not report it".
func newNATSLogger(serverName string, death *Death) natsLogger {
	return natsLogger{log: logging.Get("queue.nats.server").With("server", serverName), death: death}
}

// Every line is event `nats_server` with nats-server's sentence under
// `detail`, since an event name is a short token, never a sentence; the level
// carries the severity.
func (l natsLogger) emit(level slog.Level, format string, v ...any) {
	// Enabled before Sprintf: with debug on nats-server calls Debugf per
	// protocol event, and a suppressed line must not pay to format.
	if !l.log.Enabled(context.Background(), level) {
		return
	}
	l.log.Log(context.Background(), level, "nats_server", "detail", fmt.Sprintf(format, v...))
}

func (l natsLogger) Noticef(format string, v ...any) { l.emit(slog.LevelDebug, format, v...) }
func (l natsLogger) Warnf(format string, v ...any)   { l.emit(slog.LevelWarn, format, v...) }
func (l natsLogger) Debugf(format string, v ...any)  { l.emit(slog.LevelDebug, format, v...) }
func (l natsLogger) Tracef(format string, v ...any)  { l.emit(slog.LevelDebug, format, v...) }

// Errorf is an Error line, but for nats-server's own report of a connection its
// authenticator refused (server/client.go authViolation), which is DEBUG: it
// repeats per attempt, at a rate and with a username the peer chooses, and
// the authenticator already counts every refusal and logs it as
// broker_peer_refused once a minute a class. The dialer's side ("Route Error
// 'Authorization Violation'") keeps its level: that is this node failing to
// join, bounded by the route's back-off.
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

// Fatalf is an Error line and a declaration of the broker's death ([Death]),
// and it RETURNS: exiting would take the engine down from inside a library,
// which `NoSigs` exists to prevent. Logging alone is not enough: stock
// nats-server exits on Fatalf, so it calls one and returns from the step it
// was on (a store it could not take, a listener it could not bind), leaving a
// half-started server a boot would wait its whole readiness budget on. During
// boot [startEmbedded] refuses with nats-server's reason; after it the engine
// ends the node ([Server.Death]).
func (l natsLogger) Fatalf(format string, v ...any) {
	l.emit(slog.LevelError, format, v...)
	l.death.declare(format, v...)
}
