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
// A fixture presenting none asserts the 401 rather than its own case. The
// credential is scaffolding, not a proof of authentication: the guard's
// refusals, a revocation reaching every node, a stalled identity applier
// answering 503 not 401 are their own cases, over a closed posture. It carries
// admin, and every app, on a node whose `api.auth.admin` serves it, since a
// case here narrows nothing; a case whose subject IS authority builds its own.

// e2eToken is the credential every fixture in this suite presents.
//
// LONG ENOUGH FOR THE FLOOR, which Tier A enforces on what a `${VAR}`
// RESOLVES to: a fixture under the 26-character minimum would be refused at
// validation, and the failure would name a config rule rather than the case.
const e2eToken = "an-end-to-end-fixture-credential-long-enough"

// withCredential gives a bootstrap the fixture's token and the admin posture
// behind it.
//
// BOTH HALVES, because a principal's grants are what its token declares as
// the node's admin posture lets it carry them: an unset posture confers
// nothing and a token declaring nothing carries nothing, and every question
// past the overview would refuse for want of a grant.
//
// UNEXPORTED AND ONLY REACHED THROUGH [withServingTierA], so a fixture cannot
// take the credential without the keyring or the keyring without the
// credential.
func withCredential(t *testing.T, boot *config.Bootstrap) {
	t.Helper()
	boot.API.Auth.Admin = iam.AdminServe
	boot.API.Auth.Tokens = []config.APIToken{
		{ID: "e2e", Token: e2eToken, Grants: []iam.Grant{iam.GrantAdmin}},
	}
}

// withServingTierA gives a bootstrap everything Tier A REQUIRES once a port is
// set, which is what every fixture in this suite serves with.
//
// # One call, because the requirement is one rule
//
// `api.port` non-zero makes three settings mandatory at once — the external
// URL, the admin posture and at least one credential — and the keyring is
// mandatory on every node whatever it serves; a fixture that took some of
// them is a fixture that fails validation or answers 401 to its own case. They were two helpers and one fixture called only the
// first, which is a pairing nothing could have caught: the case that broke was
// in the SOLO partition, so an ordinary `make test` never reached it.
func withServingTierA(t *testing.T, boot *config.Bootstrap) {
	t.Helper()
	withKeyring(t, boot)
	withCredential(t, boot)
	withPasswordAlone(boot)
}

// present attaches the fixture's credential to a request.
func present(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer "+e2eToken)
	return r
}

// socketOptions attaches the fixture's credential to a WebSocket handshake.
//
// A HEADER, because nothing reads a credential off a URL: a query string is
// written into every proxy's access log, and the engine's socket stopped
// taking `?token=` once the dashboard's handshake carried its session cookie
// instead. This suite dials as a script does, with the deployment's token.
func socketOptions() *websocket.DialOptions {
	return &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + e2eToken}},
	}
}

// THE GUARD ACTUALLY REFUSES, through a real engine on a real listener.
//
// # Why this case is the control for every other one in this suite
//
// Every fixture here presents a credential, so the suite passes whether the
// guard refuses anonymous callers or serves them: a guard that stopped
// guarding shows no symptom elsewhere. It checks both transports, because they
// are two mounts: a socket handshake and a REST read share a middleware only
// today, and the socket has had a credential rule of its own.
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
