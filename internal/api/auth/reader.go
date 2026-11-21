package auth

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
)

// ReaderParam is the query parameter a WebSocket UPGRADE names its reader in,
// read only where the request carries no [httpjson.ReaderHeader].
//
// A browser cannot set [httpjson.ReaderHeader] on a WebSocket handshake, so a
// socket redialling under a cookie another tab's sign-in moved would be served
// as the new person. Named in the URL, the dial is refused `409 reader_changed`
// before any frame; the dashboard's refusal probe (a plain GET, which can carry
// the header) reads the 409 and hands the tab over.
//
// Read on upgrades only, so a URL is never a second way for an ordinary request
// to say who it is for; it can only refuse. The value is a principal ID, NOT A
// SECRET (`GET /auth/session` returns it), but a proxy's access log records it.
const ReaderParam = "reader"

// WebSocketUpgrade reports whether a request asks to become a WebSocket: an
// `Upgrade` header naming `websocket`, which a browser's handshake carries and
// the dashboard's plain re-ask of the socket's path does not. ONE RULE, which
// the stream handler's own origin check reads too, so the guard and the
// handler can never disagree about which request is a handshake.
func WebSocketUpgrade(r *http.Request) bool {
	for _, value := range r.Header.Values("Upgrade") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "websocket") {
				return true
			}
		}
	}
	return false
}

// namedReader is the reader a request names: its [httpjson.ReaderHeader], or,
// on a WebSocket upgrade that carries no header, its [ReaderParam]. Empty is a
// request that names nobody.
func namedReader(r *http.Request) string {
	if named := r.Header.Get(httpjson.ReaderHeader); named != "" {
		return named
	}
	if WebSocketUpgrade(r) {
		return r.URL.Query().Get(ReaderParam)
	}
	return ""
}

// readerMoved reports whether a request names a reader ([namedReader]) who is
// not the principal its credential resolved to (the id a tab sends in
// [httpjson.ReaderHeader]). Tabs share ONE session cookie but each draws one
// person's screen. When a sign-in in another tab moves it, closing the old
// session is BEST EFFORT, so a stale window may act as the new person from the
// last one's screen (internal/api/opkey scopes a retry's key to them).
//
// So a guarded route serves a request only to the reader it names: anybody else
// gets `409 reader_changed` before any handler runs and the dashboard hands the
// tab over (ADR-0024). NOT A CREDENTIAL: it can only refuse. A request naming
// no reader (scripts, the CLI) is decided as before, an unguarded route never
// reads it, and a value that is no principal ID is refused, not ignored.
func readerMoved(r *http.Request, p iam.Principal) bool {
	named := namedReader(r)
	if named == "" {
		return false
	}
	id, err := uuid.Parse(named)
	return err != nil || id != p.ID
}

// refuseMovedReader answers a request [readerMoved] says was made for somebody
// else: `409 reader_changed`, saying where to read who the credential is.
func refuseMovedReader(w http.ResponseWriter) {
	httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeReaderChanged,
		httpjson.Detail{"detail": "the request names the reader it was made " +
			"for in " + httpjson.ReaderHeader + " (on a WebSocket handshake, in its `" +
			ReaderParam + "` parameter), and the credential it carries is " +
			"somebody else's, so nothing was done. Ask " + PathAuthSession +
			" who the credential is now, and make the request again for them"})
}
