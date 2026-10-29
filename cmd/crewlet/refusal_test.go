package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/secrets"
)

// engineAnswered is the headers of an answer the engine wrote: its content
// type and its mark ([httpjson.EngineHeader]).
func engineAnswered(contentType string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", contentType)
	h.Set(httpjson.EngineHeader, httpjson.EngineHeaderValue)
	return h
}

// proxyAnswered is the headers of an answer something in front of the node
// wrote: a content type, and no mark.
func proxyAnswered(contentType string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", contentType)
	return h
}

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
// operator whose token works and lacks `admin` went looking for a typo
// in it; the other three rendered the 403 as a bare code or a bare reason,
// with the grants — the one fact that says whom to ask for what — dropped. Each
// client is asked, so a fifth renderer that skipped the shared one would have
// to be added here to escape it.
func TestEveryClientNamesTheGrantANodeRefusedATokenFor(t *testing.T) {
	t.Parallel()
	refused := []byte(`{"error":"unauthorized","message":"The credential you ` +
		`presented does not carry the access (Admin, Worklet, Doclet or Artifacts) ` +
		`this request needs.","reason":"no_grant","grants":["admin"]}`)
	config := &configClient{base: "http://node"}
	stored := &secretsClient{base: "http://node"}
	clients := map[string]func(status int, raw []byte) error{
		"node": func(status int, raw []byte) error { return nodeError(status, raw, true) },
		"config": func(status int, raw []byte) error {
			return config.refusal(status, engineAnswered("application/json"), raw)
		},
		"secrets": func(status int, raw []byte) error {
			return stored.refusal(status, "/secrets/X", engineAnswered("application/json"), raw)
		},
		"iam": func(status int, raw []byte) error {
			return iamRefusal(status, engineAnswered("application/json"),
				map[string]any{"error": "unauthorized"}, raw, true)
		},
	}
	for name, refusal := range clients {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := refusal(http.StatusForbidden, refused)
			if err == nil || !strings.Contains(err.Error(), "admin") ||
				!strings.Contains(err.Error(), "this token doesn't have") {
				t.Errorf("a token lacking a grant reads %v, want the grant it "+
					"needs and that the token itself was accepted", err)
			}
			err = refusal(http.StatusUnauthorized,
				[]byte(`{"error":"invalid_token"}`))
			if err == nil || !strings.Contains(err.Error(), "didn't accept") ||
				strings.Contains(err.Error(), "this token doesn't have") {
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
		`{"error":"unauthorized","detail":"a machine token may not mint another token"}`,
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

// A 403 IS THE NODE JUDGING A VALID CALLER, NOT A BAD TOKEN. Only a 401 is the
// credential: a gesture the node refuses for a reason that is not a grant — a
// seat the chart no longer holds, a tool that forbids the act — answers 403
// with a detail that is the whole answer, and reporting it as "check your
// token" sends the operator after the one thing that was fine.
func TestAForbiddenAnswerIsTheNodesRefusalNotTheToken(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":"seat_unavailable",` +
		`"detail":"the seat cto is not in the org chart any more"}`)
	err := nodeError(http.StatusForbidden, body, true)
	if err == nil {
		t.Fatal("a 403 was not reported as an error")
	}
	if strings.Contains(err.Error(), "didn't accept") {
		t.Errorf("a 403 carrying the node's own code was reported as a bad token:\n%s", err)
	}
	for _, want := range []string{"seat_unavailable", "not in the org chart"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, err)
		}
	}
	// The credential sentence is still the 401's.
	if err := nodeError(http.StatusUnauthorized, []byte(`{"error":"invalid_token"}`), true); err == nil ||
		!strings.Contains(err.Error(), "didn't accept") {
		t.Errorf("a 401 = %v, want the token refusal", err)
	}
}

// THE IAM CLIENT READS A PAGE THAT IS NOT THE ENGINE'S AS THE OTHER THREE DO —
// its title, never its markup. It printed the whole body, so a proxy's error
// page in front of a node reached the operator as a terminal full of HTML
// around the one sentence it held. Mutation: print the raw body again and the
// markup is in the refusal.
func TestTheIamClientReadsAForeignPageForWhatItSaid(t *testing.T) {
	t.Parallel()
	page := "<html><head><title>502 Bad Gateway</title></head>" +
		strings.Repeat("<div>a gateway error page</div>", 1_000) + "</html>"
	err := iamRefusal(http.StatusBadGateway, proxyAnswered("text/html"), nil, []byte(page), true)
	if err == nil {
		t.Fatal("a gateway page was not reported as an error")
	}
	if !strings.Contains(err.Error(), "502 Bad Gateway") || strings.Contains(err.Error(), "<div>") {
		t.Errorf("the refusal %.300q does not say what the page said", err)
	}
}

// A PROXY'S ANSWER IS NOT THE NODE'S, HOWEVER IT IS SPELLED. Every answer the
// engine writes carries its mark ([httpjson.EngineHeader]), and something in
// front of the node can answer JSON in the engine's own envelope. Read by its
// body, a proxy's 401 was a token the node refused, its 403 a grant to ask
// for, its 409 a lost race, its 404 a secret that does not exist and its 503
// a write the node could not account for. Unmarked, each client reads none of
// its fields and says what stood in front of the node answered. Mutation:
// read the body whatever the headers say, and every case here goes red.
func TestEveryClientReadsAnUnmarkedAnswerAsNotTheNodes(t *testing.T) {
	t.Parallel()
	forged := map[int][]byte{
		http.StatusUnauthorized: []byte(`{"error":"invalid_token"}`),
		http.StatusForbidden: []byte(`{"error":"unauthorized","reason":"no_grant",` +
			`"grants":["admin"]}`),
		http.StatusNotFound: []byte(`{"error":"not_found"}`),
		http.StatusConflict: []byte(`{"error":"revision_advanced",` +
			`"current_revision_id":"r9"}`),
		http.StatusServiceUnavailable: []byte(`{"error":"unavailable",` +
			`"outcome":"unknown","op_id":"op-1"}`),
	}
	config := &configClient{base: "http://node"}
	stored := &secretsClient{base: "http://node"}
	for status, raw := range forged {
		proxy := proxyAnswered("application/json")
		if code := engineCode(proxy, raw); code != "" {
			t.Errorf("node: an unmarked %d read as the engine's %q", status, code)
		}
		if code := engineCode(engineAnswered("application/json"), raw); code == "" {
			t.Errorf("node: the same %d, marked, read as nobody's", status)
		}
		for name, err := range map[string]error{
			"config":  config.refusal(status, proxy, raw),
			"secrets": stored.refusal(status, "/secrets/X", proxy, raw),
			"iam":     iamRefusal(status, proxy, map[string]any{"error": "x"}, raw, true),
		} {
			if err == nil {
				t.Errorf("%s: an unmarked %d was no error", name, status)
				continue
			}
			said := err.Error()
			for _, engines := range []string{
				"didn't accept", "this token doesn't have", "couldn't confirm",
				"could not establish", "another write activated first", "op op-1",
			} {
				if strings.Contains(said, engines) {
					t.Errorf("%s: an unmarked %d was read as the engine's (%q):\n%s",
						name, status, engines, said)
				}
			}
			if errors.Is(err, secrets.ErrNotFound) {
				t.Errorf("%s: an unmarked 404 was read as no such secret", name)
			}
		}
	}
}
