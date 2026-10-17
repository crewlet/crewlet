package auth

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
)

// ReaderHeader names the principal a request is made FOR: the id the
// dashboard's tab recorded as who it is read by (`GET /auth/session`'s
// `person`), sent beside the session cookie on every request the tab makes on
// that person's behalf.
//
// # Why a request has to say who it is for
//
// A browser holds ONE session cookie and every tab of it shares that cookie,
// while each tab holds a screen drawn for one person: their company, their
// draft, the toast offering to retry their change. When somebody signs in as
// somebody else in another tab, the cookie under every other tab moves, and
// the sign-in's close of the session those tabs were opened with — what
// closes their sockets `4401` and hands them over — is BEST EFFORT: a sign-in
// is never refused over the close of somebody's other session. Where it does
// not land, a window left open beside the one that signed in keeps the last
// person's screen and socket, and every request it sends goes out under the
// new cookie. A press there was a change made as the new person from a screen
// drawn for the last one, and a retry of the last person's change sent their
// operation key as the new person's — whom the operation key's scope then
// treats as a write of theirs (internal/api/opkey).
//
// So the request names its reader, and a guarded route serves it only to that
// principal: anybody else is `409 reader_changed`, written HERE, before any
// handler runs, so nothing was read on their behalf, nothing was written and
// no operation key was scoped. The dashboard hands the tab to whoever the
// session is now on that answer, exactly as a sign-in by them in this tab
// would (ADR-0024: one call is made by one principal, and it is the one the
// screen was drawn for).
//
// # What it is not
//
// NOT A CREDENTIAL, and it widens nothing: it can only refuse a request its
// credential would have been served. A request that names no reader — every
// script, the command line, an assistant, the dashboard's own read of who the
// session is and its token exchange, whose whole point is to learn or change
// who that is — is decided exactly as it always was. And an unguarded route
// never reads it: a sign-in names no reader and needs none.
const ReaderHeader = "X-Crewlet-Reader"

// ReaderParam is the query parameter a WebSocket UPGRADE names its reader in,
// read only where the request carries no [ReaderHeader].
//
// # Why the socket's handshake needs a second spelling
//
// A browser sets no header on a WebSocket handshake — it is the one request a
// tab makes that cannot carry [ReaderHeader]. Without it, a socket that
// dropped for an unrelated reason and dialled again under a cookie a sign-in
// in another tab had moved was served as the new person, and the tab drew
// their company until its next REST request or session keepalive was refused,
// up to two keepalive intervals later. Named in the handshake's URL, the dial
// is refused `409 reader_changed` HERE, before the stream handler runs and
// before any frame of anybody's company is written; the browser reports the
// refused handshake as 1006, and the dashboard's refusal probe — a plain GET
// of the same path, which can carry the header — reads the 409 and hands the
// tab over.
//
// # On upgrades only, and never a credential
//
// Read on nothing else, so a URL is never a second way for an ordinary
// request to say who it is for; and it is compared exactly as the header is,
// so it can only refuse a handshake its credential would have been served.
// The value is a principal ID, which is NOT A SECRET — every answer of
// `GET /auth/session` carries it — but it does appear wherever the socket's
// URL is written down: a proxy's or a balancer's access log of the socket
// path. The engine's own logs name the path alone.
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

// namedReader is the reader a request names: its [ReaderHeader], or — on a
// WebSocket upgrade that carries no header — its [ReaderParam]. Empty is a
// request that names nobody.
func namedReader(r *http.Request) string {
	if named := r.Header.Get(ReaderHeader); named != "" {
		return named
	}
	if WebSocketUpgrade(r) {
		return r.URL.Query().Get(ReaderParam)
	}
	return ""
}

// readerMoved reports whether a request names a reader ([namedReader]) who is
// not the principal its credential resolved to.
//
// A VALUE THAT IS NOT A PRINCIPAL ID NAMES NOBODY THE CREDENTIAL IS, and is
// refused for that reason rather than ignored: the only sender is a tab
// repeating the id the engine answered it, so a value no principal carries is
// a tab whose record of its reader is not this credential's — and a request
// that said who it was for and was served to somebody else anyway is exactly
// what this exists to stop. Compared as an id, so a spelling of the same id
// in another case is the same reader.
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
			"for in " + ReaderHeader + " (on a WebSocket handshake, in its `" +
			ReaderParam + "` parameter), and the credential it carries is " +
			"somebody else's, so nothing was done. Ask " + PathAuthSession +
			" who the credential is now, and make the request again for them"})
}
