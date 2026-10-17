package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// someoneElse is a principal id that is not [sessionPerson]: the reader a tab
// recorded before the browser's cookie moved to the rig's person.
const someoneElse = "018f3a9c-0000-7000-8000-0000000000bb"

// reached is what one request through the guard came to, and whether the
// route behind it ran at all.
type reached struct {
	status int
	code   string
	ran    bool
}

// send runs one request through rig's guard, naming `reader` in the header
// where it is not empty.
func (s *signedIn) send(g *auth.Guard, method, path, reader string,
	prepare func(*http.Request)) reached {

	s.t.Helper()
	var out reached
	handler := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		out.ran = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, path, nil)
	if reader != "" {
		req.Header.Set(auth.ReaderHeader, reader)
	}
	if prepare != nil {
		prepare(req)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	out.status = rec.Code
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	out.code, _ = body["error"].(string)
	return out
}

// A REQUEST MADE FOR ANOTHER READER IS REFUSED BEFORE IT RUNS.
//
// The window left open beside the one somebody signed in as somebody else
// with: its screen is the last person's, its cookie is the new person's, and
// the sign-in's close of the session it was opened with did not land. Every
// request it sends names the reader it was drawn for, and a guarded route
// serves it only to that principal — on a read, a change and the act route
// alike, and under /auth too, where a sign-out everywhere pressed on the last
// person's Account screen would otherwise end the new person's sessions.
// Nothing runs, so nothing is written and no operation key is scoped to the
// person the browser now belongs to.
//
// Mutation: drop the guard's reader check and every refused row runs.
func TestARequestMadeForAnotherReaderIsRefusedBeforeItRuns(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	g := rig.guard()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/agents"},
		{http.MethodPost, "/operator/act/create_work_item"},
		{http.MethodPatch, "/config"},
		{http.MethodPost, "/auth/logout/all"},
		{http.MethodPost, auth.PathAuthStepUp},
	} {
		got := rig.send(g, route.method, route.path, someoneElse, rig.withCookie)
		if got.ran {
			t.Errorf("%s %s ran for a request made for somebody else",
				route.method, route.path)
		}
		if got.status != http.StatusConflict || got.code != string(httpjson.CodeReaderChanged) {
			t.Errorf("%s %s answered %d %q, want 409 %q", route.method, route.path,
				got.status, got.code, httpjson.CodeReaderChanged)
		}
	}
	// A VALUE THAT NAMES NO PRINCIPAL is a tab whose record is not this
	// credential's, and is refused as one rather than ignored.
	if got := rig.send(g, http.MethodGet, "/agents", "not-an-id", rig.withCookie); got.ran ||
		got.code != string(httpjson.CodeReaderChanged) {
		t.Errorf("a reader that is no id ran=%v and answered %d %q",
			got.ran, got.status, got.code)
	}
}

// A SOCKET'S HANDSHAKE NAMES ITS READER IN ITS URL, AND IS REFUSED BEFORE IT
// IS SERVED.
//
// A browser sets no header on a WebSocket handshake, so a socket that dropped
// and dialled again under a cookie a sign-in in another tab had moved was
// served as the new person until the tab's next request was refused. Named in
// the `reader` parameter, that dial is refused `409 reader_changed` before the
// stream handler runs. The parameter is read on an upgrade ONLY, and only where
// no header names the reader: on any other request a URL is never a second way
// to say who it is for, and a request naming its reader in the header is
// decided by the header.
//
// Mutation: read the parameter on every request, or ahead of the header, and a
// served row below is refused; drop it and the refused rows run.
func TestASocketHandshakeIsRefusedForAnotherReaderNamedInItsURL(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	g := rig.guard()
	upgrade := func(r *http.Request) {
		rig.withCookie(r)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
	}
	at := func(reader string) string {
		return "/ws/stream?" + auth.ReaderParam + "=" + reader
	}
	for _, c := range []struct {
		name, path, header string
		prepare            func(*http.Request)
		refused            bool
	}{
		{name: "a handshake for somebody else", path: at(someoneElse),
			prepare: upgrade, refused: true},
		{name: "a handshake naming no principal", path: at("not-an-id"),
			prepare: upgrade, refused: true},
		{name: "a handshake whose header names somebody else", path: at(sessionPerson),
			header: someoneElse, prepare: upgrade, refused: true},
		{name: "a handshake for its own reader", path: at(sessionPerson), prepare: upgrade},
		{name: "a handshake naming nobody", path: "/ws/stream", prepare: upgrade},
		{name: "a handshake whose header names its own reader", path: at(someoneElse),
			header: sessionPerson, prepare: upgrade},
		{name: "the socket's plain re-ask", path: at(someoneElse), prepare: rig.withCookie},
		{name: "an ordinary read", path: "/agents?" + auth.ReaderParam + "=" + someoneElse,
			prepare: rig.withCookie},
	} {
		got := rig.send(g, http.MethodGet, c.path, c.header, c.prepare)
		switch {
		case c.refused && (got.ran || got.status != http.StatusConflict ||
			got.code != string(httpjson.CodeReaderChanged)):
			t.Errorf("%s ran=%v and answered %d %q, want 409 %q before it runs",
				c.name, got.ran, got.status, got.code, httpjson.CodeReaderChanged)
		case !c.refused && (!got.ran || got.status != http.StatusOK):
			t.Errorf("%s ran=%v and answered %d %q, want served", c.name, got.ran,
				got.status, got.code)
		}
	}
}

// THE PRINCIPAL'S OWN TAB, AND A CALLER THAT NAMES NOBODY, ARE SERVED.
//
// The controls for the case above, each a separate way the check could be
// wrong while that case stays red: a tab naming the person the cookie is —
// spelled as the engine answered it or in capitals, since it is compared as
// an id — and a request naming no reader at all, which is every script, the
// command line and the dashboard's own read of who the session is.
func TestARequestForItsOwnReaderOrForNobodyIsServed(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	g := rig.guard()
	for _, reader := range []string{sessionPerson, strings.ToUpper(sessionPerson), ""} {
		got := rig.send(g, http.MethodPost, "/operator/act/create_work_item",
			reader, rig.withCookie)
		if !got.ran || got.status != http.StatusOK {
			t.Errorf("a request naming reader %q ran=%v and answered %d %q, "+
				"want served", reader, got.ran, got.status, got.code)
		}
	}
}

// A READER IS ONLY EVER COMPARED WITH SOMEBODY THE GUARD RESOLVED.
//
// Nobody, a node that cannot tell, and an unguarded route each answer as they
// always did whatever the header says: there is no principal to hold the
// reader against in the first two — and a 409 there would hand a tab to a
// session that does not exist, or one this node cannot read — and the
// sign-in routes, which a tab changing hands goes through, never read it. A
// resolved refusal of the credential itself is decided AFTER the reader,
// because a tab that is somebody else's is put right first and whoever it is
// handed to meets their own refusal.
func TestOnlyAResolvedPrincipalIsHeldToTheReaderItWasSentFor(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	g := rig.guard()

	if got := rig.send(g, http.MethodGet, "/agents", someoneElse, nil); got.ran ||
		got.status != http.StatusUnauthorized ||
		got.code != string(httpjson.CodeInvalidToken) {
		t.Errorf("nobody naming a reader answered ran=%v %d %q, want 401 %q",
			got.ran, got.status, got.code, httpjson.CodeInvalidToken)
	}
	if got := rig.send(g, http.MethodPost, auth.PathAuthLogout, someoneElse,
		rig.withCookie); !got.ran {
		t.Errorf("the unguarded sign-out was refused (%d %q) over a reader it "+
			"never reads", got.status, got.code)
	}

	unread := newSignedIn(t)
	unread.dir.err = context.DeadlineExceeded
	if got := unread.send(unread.guard(), http.MethodGet, "/agents", someoneElse,
		unread.withCookie); got.status != http.StatusServiceUnavailable ||
		got.code != string(httpjson.CodeIdentityUnavailable) {
		t.Errorf("a node that cannot tell answered %d %q, want 503 %q",
			got.status, got.code, httpjson.CodeIdentityUnavailable)
	}

	seatless := newSignedIn(t)
	delete(seatless.chart.seats, sessionSeat)
	sg := seatless.guard()
	if got := seatless.send(sg, http.MethodGet, "/agents", someoneElse,
		seatless.withCookie); got.code != string(httpjson.CodeReaderChanged) {
		t.Errorf("a seat refusal beside a moved reader answered %d %q, want %q "+
			"first", got.status, got.code, httpjson.CodeReaderChanged)
	}
	// THE CONTROL: their own tab meets their own refusal.
	if got := seatless.send(sg, http.MethodGet, "/agents", sessionPerson,
		seatless.withCookie); got.code != string(httpjson.CodeSeatUnavailable) {
		t.Errorf("their own tab answered %d %q, want %q", got.status, got.code,
			httpjson.CodeSeatUnavailable)
	}
}

// THE DASHBOARD'S SOCKET NAMES ITS READER IN THE PARAMETER THE GUARD READS.
//
// `contract/identity.ts` `READER_PARAM` is the query parameter the socket's
// handshake carries its reader in. Spelled differently there, every handshake
// would name nobody and be served to whoever the cookie is — by design, as
// any request naming nobody is — and nothing would go red.
func TestTheDashboardsSocketNamesItsReaderInTheGuardsParameter(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "READER_PARAM")
	if err != nil {
		t.Fatal(err)
	}
	if got != auth.ReaderParam {
		t.Errorf("the dashboard's socket names its reader in %q and the guard reads %q",
			got, auth.ReaderParam)
	}
}

// THE DASHBOARD NAMES ITS READER IN THE HEADER THE GUARD READS.
//
// `contract/identity.ts` `READER_HEADER` is the header every request the tab
// makes for its reader carries. Spelled differently there, no request would
// ever name its reader, every one would be served to whoever the cookie is,
// and nothing would go red: a request naming nobody is served by design.
func TestTheDashboardNamesItsReaderInTheGuardsHeader(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "READER_HEADER")
	if err != nil {
		t.Fatal(err)
	}
	if got != auth.ReaderHeader {
		t.Errorf("the dashboard names its reader in %q and the guard reads %q",
			got, auth.ReaderHeader)
	}
}
