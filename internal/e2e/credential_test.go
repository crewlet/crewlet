package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// EVERY REQUEST THIS SUITE MAKES CARRIES A CREDENTIAL, because every request
// to a running engine does.
//
// # What this closes, and what it deliberately does not
//
// This suite used to dial the socket with a nil header and issue bare GETs, on
// a default bootstrap that configured no tokens at all — which worked because
// reads were open by default. That posture is gone: every route needs a
// credential whatever its method, so a fixture that presents none is asserting
// the 401 rather than whatever each case is about.
//
// What it does NOT do is prove anything about authentication. These cases are
// about a turn reaching a dashboard, a parked run reaching a board, a fleet
// converging — and the credential is scaffolding so they can be about that.
// Proving that the guard actually refuses, that a revocation reaches every
// node, that a stalled identity applier answers 503 rather than 401: those are
// their own cases, over a fixture that boots a closed posture deliberately.
//
// # It carries every grant
//
// A case here narrows nothing, so the fixture grants everything the ceiling
// permits and the ceiling is everything. A case whose subject IS authority
// builds its own posture and says what it presents — which is the same
// division internal/api's own fixtures draw.

// e2eToken is the credential every fixture in this suite presents.
//
// LONG ENOUGH FOR THE FLOOR, which Tier A enforces on what a `${VAR}`
// RESOLVES to: a fixture under the 26-character minimum would be refused at
// validation, and the failure would name a config rule rather than the case.
const e2eToken = "an-end-to-end-fixture-credential-long-enough"

// withCredential gives a bootstrap the fixture's token and the ceiling behind
// it.
//
// BOTH HALVES, because a principal's grants are the INTERSECTION of what its
// token declares with the deployment's ceiling: either one missing carries
// none, and every question would refuse for want of a grant rather than
// answering whatever the case is about.
//
// UNEXPORTED AND ONLY REACHED THROUGH [withServingTierA], so a fixture cannot
// take the credential without the keyring or the keyring without the
// credential — the pairing was two calls and the second was already missing
// from one of the two fixtures that needed it.
func withCredential(t *testing.T, boot *config.Bootstrap) {
	t.Helper()
	boot.API.Auth.MaxGrants = iam.AllGrants
	boot.API.Auth.Tokens = []config.APIToken{
		{ID: "e2e", Token: e2eToken, Grants: iam.AllGrants},
	}
}

// withServingTierA gives a bootstrap everything Tier A REQUIRES once a port is
// set, which is what every fixture in this suite serves with.
//
// # One call, because the requirement is one rule
//
// `api.port` non-zero makes four settings mandatory at once — the external
// URL, the grant ceiling, at least one credential, and the keyring — and a
// fixture that took some of them is a fixture that fails validation or answers
// 401 to its own case. They were two helpers and one fixture called only the
// first, which is a pairing nothing could have caught: the case that broke was
// in the SOLO partition, so an ordinary `make test` never reached it.
func withServingTierA(t *testing.T, boot *config.Bootstrap) {
	t.Helper()
	withKeyring(t, boot)
	withCredential(t, boot)
}

// present attaches the fixture's credential to a request.
func present(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer "+e2eToken)
	return r
}

// socketCredential is the query parameter a WebSocket handshake carries.
//
// ?token= AND NOT A HEADER, because a browser cannot set one on a WebSocket
// constructor — which is why that parameter exists at all, and why this suite
// dials the way the dashboard does rather than the way a script would.
func socketCredential() string { return "?token=" + e2eToken }

// THE GUARD ACTUALLY REFUSES, through a real engine on a real listener.
//
// # Why this case is the control for every other one in this suite
//
// Every fixture here now presents a credential, which makes the suite pass
// whether the guard refuses anonymous callers or serves them. That is exactly
// the shape of the bug this whole file exists because of: this suite dialled
// anonymously for as long as reads were open by default, and when that posture
// was deleted the fixtures went on dialling — the failure was invisible in
// `make test` because internal/e2e is in the SOLO partition, so it surfaced
// several commits later.
//
// A guard that stopped guarding is the same class of failure with the opposite
// sign and none of the symptoms: every case here would still pass, and the
// only evidence would be a company serving its LLM transcripts to whoever
// could reach the port.
//
// # It checks both transports, because they are two mounts
//
// A socket handshake and a REST read are refused by the same middleware today,
// and "today" is the word doing the work: the socket carries its credential in
// a query parameter because a browser cannot set a header on a WebSocket
// constructor, so it is the one path that has ever had a credential rule of its
// own. Checking one would leave the other's exemption invisible.
func TestAnAnonymousCallerIsRefusedByARunningEngine(t *testing.T) {
	n := start(t)

	// THE SOCKET, dialled the way the dashboard does and presenting
	// nothing. A refused handshake carries no close code — the connection
	// never opened — so what this reads is the dial failing at all.
	target := "ws" + strings.TrimPrefix(n.server.URL, "http") + "/ws/stream"
	if conn, _, err := websocket.Dial(t.Context(), target, nil); err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Error("a socket opened with no credential: this company's LLM " +
			"transcripts, diary entries and roster are served to whoever can " +
			"reach the port")
	}

	// AND THE READS, over both a guarded question and a plain route. Each
	// is a separate mount, and an exemption is added one path at a time.
	for _, path := range []string{"/query/stream", "/events", "/agents", "/config"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			n.server.URL+path, nil)
		if err != nil {
			t.Fatalf("build the request: %v", err)
		}
		res, err := n.server.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s answered %d with no credential, want 401",
				path, res.StatusCode)
		}
	}

	// THE CONTROL FOR THE CONTROL: the probe is still exempt. Without it
	// this case would pass on an engine that refused every request,
	// including the liveness check an orchestrator makes — which is a node
	// that gets killed in the middle of the turns a drain exists to finish.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		n.server.URL+"/health", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	res, err := n.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /health answered %d with no credential, want 200: an "+
			"orchestrator holds no token, and a liveness check that 401s is a "+
			"liveness check that fails", res.StatusCode)
	}
}
