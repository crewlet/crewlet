package auth

import (
	"net/http"

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

// readerMoved reports whether a request names a reader ([ReaderHeader]) who is
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
	named := r.Header.Get(ReaderHeader)
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
			"for in " + ReaderHeader + ", and the credential it carries is " +
			"somebody else's, so nothing was done. Ask " + PathAuthSession +
			" who the credential is now, and make the request again for them"})
}
