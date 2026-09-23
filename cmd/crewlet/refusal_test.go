package main

import (
	"net/http"
	"strings"
	"testing"
)

// EVERY CLIENT RENDERS THE HINT, because the hint is the half that says what
// to do. A drain's refusal is `draining` plus a detail and a hint, and a
// client that decodes the code alone reports "this node cannot serve that:
// draining" — true, and useless to the operator holding it.
func TestANodeRefusalCarriesItsDetailAndHintToTheOperator(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":"draining",` +
		`"detail":"this node is draining for a shutdown: the turns already ` +
		`running finish, and nothing new is started here",` +
		`"hint":"retry against another node, or once this one has restarted; ` +
		`/ready answers 503 for as long as the drain lasts"}`)
	err := nodeError(http.StatusServiceUnavailable, body, true)
	if err == nil {
		t.Fatal("a 503 was not reported as an error")
	}
	for _, want := range []string{
		"draining",
		"nothing new is started here",
		"retry against another node",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, err)
		}
	}
	// AND NOT THE GUESS. The fallback names a build without a backend,
	// which is the other thing a 503 means here and the wrong one to
	// print when the body said which.
	if strings.Contains(err.Error(), "without the backend") {
		t.Errorf("the refusal guessed a reason the body had already given:\n%s", err)
	}
}

// The guess survives for the answer that carries no code, which is the case it
// was written for: a node built without the coordination store a route needs.
func TestANodeRefusalWithNoCodeStillNamesTheLikelyCause(t *testing.T) {
	t.Parallel()
	err := nodeError(http.StatusServiceUnavailable, []byte("{}"), true)
	if err == nil || !strings.Contains(err.Error(), "without the backend the route needs") {
		t.Errorf("a 503 with no code = %v, want the fallback cause", err)
	}
}

func TestARefusalWithNothingBesideTheCodeIsLeftAlone(t *testing.T) {
	t.Parallel()
	if got := withRefusalDetail("draining", "", ""); got != "draining" {
		t.Errorf("withRefusalDetail with no extras = %q, want the message unchanged", got)
	}
	if got := withRefusalDetail("draining", "", "retry elsewhere"); got != "draining\n  retry elsewhere" {
		t.Errorf("withRefusalDetail skipping an absent detail = %q", got)
	}
}

// A TOKEN THE NODE ACCEPTED IS NOT A TOKEN IT REFUSED, and every client says
// which — naming the grant the node named.
//
// The node client answered a 401 and a 403 alike with "the node refused the
// token: check it against the api.auth.tokens entry you meant to use", so an
// operator whose token works and lacks `fleet:operate` went looking for a typo
// in it; the other four rendered the 403 as a bare code or a bare reason, with
// the grants — the one fact that says whom to ask for what — dropped. Each
// client is asked, so a sixth renderer that skipped the shared one would have
// to be added here to escape it.
func TestEveryClientNamesTheGrantANodeRefusedATokenFor(t *testing.T) {
	t.Parallel()
	refused := []byte(`{"error":"unauthorized","message":"The credential you ` +
		`presented does not carry the grant this request needs.",` +
		`"reason":"no_grant","grants":["fleet:operate"]}`)
	config := &configClient{base: "http://node"}
	secrets := &secretsClient{base: "http://node"}
	clients := map[string]func(status int, raw []byte) error{
		"node": func(status int, raw []byte) error { return nodeError(status, raw, true) },
		"config": func(status int, raw []byte) error {
			return config.refusal(status, raw)
		},
		"secrets": func(status int, raw []byte) error {
			return secrets.refusal(status, "/secrets/X", raw)
		},
		"chart": func(status int, raw []byte) error {
			return chartRefusal("PATCH /chart/units/x", status, raw)
		},
		"iam": func(status int, raw []byte) error {
			return iamRefusal(status, map[string]any{"error": "unauthorized"}, raw)
		},
	}
	for name, refusal := range clients {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := refusal(http.StatusForbidden, refused)
			if err == nil || !strings.Contains(err.Error(), "fleet:operate") ||
				!strings.Contains(err.Error(), "accepted the token") {
				t.Errorf("a token lacking a grant reads %v, want the grant it "+
					"needs and that the token itself was accepted", err)
			}
			err = refusal(http.StatusUnauthorized,
				[]byte(`{"error":"invalid_token"}`))
			if err == nil || !strings.Contains(err.Error(), "did not accept") ||
				strings.Contains(err.Error(), "accepted the token") {
				t.Errorf("a token the node did not accept reads %v", err)
			}
		})
	}
}

// A 403 THAT IS NOT ABOUT AUTHORITY stays the client's own, because reading it
// as a missing grant would send an operator to ask for one they hold.
func TestAForbiddenThatNamesNoGrantIsNotReadAsOne(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"error":"seat_unavailable","seat":"cto"}`,
		`{"error":"unauthorized","detail":"fleet:operate cannot be minted onto a token"}`,
		`not json`,
	} {
		if msg, ok := credentialRefusal(http.StatusForbidden, []byte(raw), true); ok {
			t.Errorf("%s was read as a refusal on authority: %q", raw, msg)
		}
	}
	msg, ok := credentialRefusal(http.StatusForbidden,
		[]byte(`{"error":"unauthorized","reason":"not_lead","grants":[]}`), true)
	if !ok || !strings.Contains(msg, "not_lead") {
		t.Errorf("a refusal no grant would lift = %q, %v; want the reason named", msg, ok)
	}
}
