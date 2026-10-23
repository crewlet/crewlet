package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
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

func TestARefusalWithNothingBesideTheCodeIsLeftAlone(t *testing.T) {
	t.Parallel()
	if got := withRefusalDetail("draining", refusalWords{}); got != "draining" {
		t.Errorf("withRefusalDetail with no extras = %q, want the message unchanged", got)
	}
	if got := withRefusalDetail("draining", refusalWords{Hint: "retry elsewhere"}); got !=
		"draining\n  retry elsewhere" {
		t.Errorf("withRefusalDetail skipping an absent detail = %q", got)
	}
}

// A REFUSAL NOBODY WORDED FOR A PERSON SAYS ITS CODE'S OWN SENTENCE, and one
// the node did word says that instead: the detail is the same refusal said of
// this request, so the two never stand one under the other. Mutation: drop the
// message from [refusalWords.said] and the floor reads the bare code; say both
// and a worded refusal carries its code's general sentence above its own.
func TestARefusalSaysItsDetailOrElseItsCodesSentence(t *testing.T) {
	t.Parallel()
	conflict := httpjson.CodeConflict.Message()
	if got, want := withRefusalDetail("conflict", refusalWords{Message: conflict}),
		"conflict\n  "+conflict; got != want {
		t.Errorf("a refusal at the floor reads %q, want %q", got, want)
	}
	worded := refusalWords{Message: conflict, Detail: "The room is archived.",
		Hint: "Unarchive it first."}
	if got, want := withRefusalDetail("conflict", worded),
		"conflict\n  The room is archived.\n  Unarchive it first."; got != want {
		t.Errorf("a worded refusal reads %q, want %q", got, want)
	}
}

// EVERY CLIENT SAYS THE CODE'S OWN SENTENCE FOR A REFUSAL NOBODY WORDED FOR A
// PERSON, which is ADR-0060's floor: no `detail`, and the code's `message` the
// whole of what the refusal says. Three of the four decoded `error`, `detail`
// and `hint` and never `message`, so `crewlet chatlet post` into an archived
// room printed "the node answered 409: conflict" and nothing more. The bodies
// are the node's own envelope rather than a sentence typed here, and each
// client is asked, so a renderer that skipped the shared rule would have to be
// left out of this list to escape it. Mutation: drop the message from
// [refusalWords.said], or decode a client's body without [refusalWords], and
// that client reads the bare code.
func TestEveryClientSaysTheCodesSentenceForARefusalNobodyWorded(t *testing.T) {
	t.Parallel()
	config := &configClient{base: "http://node"}
	secrets := &secretsClient{base: "http://node"}
	decoded := func(raw []byte) map[string]any {
		var answer map[string]any
		_ = json.Unmarshal(raw, &answer)
		return answer
	}
	clients := map[string]func(status int, raw []byte) error{
		"node": func(status int, raw []byte) error { return nodeError(status, raw, true) },
		"config": func(status int, raw []byte) error {
			return config.refusal(status, "application/json", raw)
		},
		"secrets": func(status int, raw []byte) error {
			return secrets.refusal(status, "/secrets/X", "application/json", raw)
		},
		"iam": func(status int, raw []byte) error {
			return iamRefusal(status, "application/json", decoded(raw), raw, true)
		},
		"iam self": func(status int, raw []byte) error {
			return selfRefusal(status, decoded(raw), raw)
		},
	}
	for name, refusal := range clients {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, c := range []struct {
				status int
				code   httpjson.Code
			}{
				{http.StatusConflict, httpjson.CodeConflict},
				{http.StatusUnprocessableEntity, httpjson.CodeInvalid},
				{http.StatusForbidden, httpjson.CodeForbidden},
			} {
				raw, err := json.Marshal(httpjson.Envelope(c.code, nil))
				if err != nil {
					t.Fatal(err)
				}
				err = refusal(c.status, raw)
				if err == nil || !strings.Contains(err.Error(), string(c.code)) ||
					!strings.Contains(err.Error(), c.code.Message()) {
					t.Errorf("a %d %s nobody worded reads %v, want the code and its "+
						"own sentence", c.status, c.code, err)
				}
			}
		})
	}
}

// THE NODE CLIENT SAYS THE CODE'S SENTENCE IN EVERY ARM it frames a refusal
// in: a refusal it relays as the node's answer, a 503 it frames as a node
// that cannot serve, and a gate's judgement. Mutation: put the bare detail
// back in any one arm and that arm reads the bare code.
func TestTheNodeClientSaysTheCodesSentenceInEveryArm(t *testing.T) {
	t.Parallel()
	judged := func(err error) string {
		var refusal *nodeRefusal
		if !errors.As(err, &refusal) {
			return err.Error()
		}
		return refusal.asJudgement().Error()
	}
	for _, c := range []struct {
		name   string
		status int
		code   httpjson.Code
		read   func(error) string
	}{
		{"answered", http.StatusNotFound, httpjson.CodeNotFound, error.Error},
		{"cannot serve", http.StatusServiceUnavailable, httpjson.CodeUnavailable, error.Error},
		{"judgement", http.StatusServiceUnavailable, httpjson.CodeUnavailable, judged},
	} {
		raw, err := json.Marshal(httpjson.Envelope(c.code, nil))
		if err != nil {
			t.Fatal(err)
		}
		if said := c.read(nodeError(c.status, raw, true)); !strings.Contains(said,
			string(c.code)) || !strings.Contains(said, c.code.Message()) {
			t.Errorf("%s: a %d %s nobody worded reads %q, want the code and its own "+
				"sentence", c.name, c.status, c.code, said)
		}
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
	// THE NODE'S OWN SENTENCE, from the table it answers with, rather than a
	// copy typed here that would go on naming three permissions after the
	// table named a fourth.
	refused, err := json.Marshal(map[string]any{
		"error":   httpjson.CodeUnauthorized,
		"message": httpjson.CodeUnauthorized.Message(),
		"reason":  "no_grant", "grants": []string{"admin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := &configClient{base: "http://node"}
	secrets := &secretsClient{base: "http://node"}
	clients := map[string]func(status int, raw []byte) error{
		"node": func(status int, raw []byte) error { return nodeError(status, raw, true) },
		"config": func(status int, raw []byte) error {
			return config.refusal(status, "application/json", raw)
		},
		"secrets": func(status int, raw []byte) error {
			return secrets.refusal(status, "/secrets/X", "application/json", raw)
		},
		"iam": func(status int, raw []byte) error {
			return iamRefusal(status, "application/json",
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

// A VERB MADE AS A SEAT, REFUSED TO A TOKEN ACTING AS NONE, NAMES THE TWO WAYS
// PAST IT: no grant would lift it, so its bare reason sent an operator holding
// admin looking for a permission they already had. Mutation: drop the
// unseated arm and the refusal reads the bare reason with neither command.
func TestAnUnseatedRefusalNamesTheTokenAndTheBinding(t *testing.T) {
	t.Parallel()
	msg, ok := credentialRefusal(http.StatusForbidden,
		[]byte(`{"error":"unauthorized","reason":"unseated","grants":[]}`), true)
	if !ok {
		t.Fatalf("an unseated refusal was not read as a refusal on authority: %q", msg)
	}
	for _, want := range []string{"acts as no seat", "crewlet iam token -login LOGIN",
		"crewlet iam bind ID SEAT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("an unseated refusal reads %q, want it to say %q", msg, want)
		}
	}
}

// A RULE NO GRANT LIFTS IS SAID IN THE NODE'S OWN WORDS FOR IT, and one a grant
// does lift names the grant.
//
// The renderer answered every refusal with no grants by its reason's bare word
// ("this token isn't allowed to do this (reason)"), and its own comment said
// what was missing was a relation — for an account that is not active and a
// verb only an agent's seat makes, it was neither. The node's `message`
// follows the reason now, so it is what the operator reads. The bodies are the
// node's own envelope, built the way every surface builds one, rather than a
// sentence typed here. Mutation: drop the message arm and the stage and
// seat-only rows read the bare reason; put it before the grants arm and the
// no-grant row loses the grant's own word.
func TestARefusalNoGrantLiftsSaysTheRule(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		reason authz.Reason
		grants []iam.Grant
		want   []string
	}{
		{authz.ReasonStage, nil, []string{"(stage)", "isn't active", "crewlet iam activate"}},
		{authz.ReasonSeatOnly, nil, []string{"(seat_only)", "Only an agent can do this"}},
		{authz.ReasonNotLead, []iam.Grant{iam.GrantAdmin},
			[]string{"(not_lead)", "its lead", "With Admin access"}},
		{authz.ReasonNoGrant, []iam.Grant{iam.GrantWorklet},
			[]string{`doesn't have "worklet". Ask an admin for it`}},
	} {
		_, _, detail := authz.RefusalFields(authz.Decision{Reason: c.reason, Grants: c.grants})
		raw, err := json.Marshal(httpjson.Envelope(httpjson.CodeUnauthorized, detail))
		if err != nil {
			t.Fatal(err)
		}
		msg, ok := credentialRefusal(http.StatusForbidden, raw, true)
		if !ok || msg == "this token isn't allowed to do this ("+string(c.reason)+")" {
			t.Errorf("%s reads %q, %v; want the node's own words for it", c.reason, msg, ok)
			continue
		}
		for _, want := range c.want {
			if !strings.Contains(msg, want) {
				t.Errorf("%s reads %q, want it to say %q", c.reason, msg, want)
			}
		}
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
	err := iamRefusal(http.StatusBadGateway, "text/html", nil, []byte(page), true)
	if err == nil {
		t.Fatal("a gateway page was not reported as an error")
	}
	if !strings.Contains(err.Error(), "502 Bad Gateway") || strings.Contains(err.Error(), "<div>") {
		t.Errorf("the refusal %.300q does not say what the page said", err)
	}
}
