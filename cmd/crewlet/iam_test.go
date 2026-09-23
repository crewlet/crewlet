package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// THE CLI CARRIES ITS OWN CREDENTIAL, AND NEVER THE CONFIG'S FIRST TOKEN.
//
// `api.auth.tokens` is what a node ACCEPTS; it is not a wallet this command
// helps itself from. The fallback that took the first entry meant an
// operator's write landed under a name they had not chosen and might not
// hold — and it read a resolved `${VAR}` out of the config file in the clear,
// in a process with no other reason to hold one.
//
// The refusal NAMES THE VARIABLE, because "unauthorized" from the far end is
// the answer an operator spends an afternoon on.
func TestTheFirstTokenFallbackIsGone(t *testing.T) {
	t.Setenv(apiTokenEnv, "")
	if got := nodeTokenOrEmpty(); got != "" {
		t.Errorf("with no environment variable the CLI produced a token (%q), "+
			"so it is reading one out of somewhere it should not", got)
	}
	_, err := nodeAPIToken("/iam")
	if err == nil {
		t.Fatal("a command with no credential was allowed to proceed")
	}
	if !strings.Contains(err.Error(), apiTokenEnv) {
		t.Errorf("the refusal %q does not name %s, so an operator has to "+
			"guess where the credential comes from", err, apiTokenEnv)
	}
	// AND THE ENVIRONMENT IS STILL READ, which is the control: without it
	// the case above would pass on a CLI that could never authenticate.
	t.Setenv(apiTokenEnv, "a-token")
	if got := nodeTokenOrEmpty(); got != "a-token" {
		t.Errorf("the environment token resolved to %q", got)
	}
}

// `iam` IS DISPATCHED AND ADVERTISED.
//
// Nothing connects the switch to usage(), so a subcommand can ship reachable
// and undocumented or documented and unreachable. main_test.go walks that in
// general; this is the one line that says which command this change added.
func TestIamIsDispatchedAndAdvertised(t *testing.T) {
	var out, errs bytes.Buffer
	if err := run([]string{"iam"}, &out, &errs); err == nil {
		t.Error("`crewlet iam` with no subcommand did not ask for one")
	}
	if !strings.Contains(out.String(), "crewlet iam invite") {
		t.Errorf("the usage does not name the invite gesture:\n%s", out.String())
	}
}

// A COMMAND THAT NAMES NOTHING IS REFUSED BY NAME.
//
// "crewlet iam show" is an operator who typed half a command, and what they
// need is which half — never a usage dump they have to find their own line
// in.
func TestAnIamCommandWithNoSubjectSaysWhatIsMissing(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{"show", "a person id"},
		{"invite", "an address"},
		{"bind", "a person id"},
		{"revoke-credential", "a credential id"},
	} {
		t.Run(tc.args, func(t *testing.T) {
			var out, errs bytes.Buffer
			err := run([]string{"iam", tc.args}, &out, &errs)
			if err == nil {
				t.Fatalf("`crewlet iam %s` was accepted with no subject",
					tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal is %q, want it to name %q",
					err, tc.want)
			}
		})
	}
}

// `iam token` ASKS FOR WHAT THE ROUTE READS: the owner on the QUERY, which is
// what the authority table decides on, and the grants, reach and lifetime in
// the body — and it prints the value with what it carries.
//
// It used to name the owner in a body field the route no longer reads, and to
// drop -colleague on the floor, so every token it minted was the caller's own
// at the caller's own reach whatever the command said. Mutation: send the
// person in the body again and the node sees no `person` query.
func TestIamTokenAsksForWhatTheRouteReads(t *testing.T) {
	var seen struct {
		method, path, person, auth string
		body                       map[string]any
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.method, seen.path = r.Method, r.URL.Path
		seen.person = r.URL.Query().Get("person")
		seen.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&seen.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"c-1","person":"p-1","token":"cwl_pat_the-value",` +
			`"grants":["state:read"],"colleague":"read",` +
			`"expires_at":"2026-07-01T00:00:00Z"}`))
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")

	var out, errs bytes.Buffer
	if err := run([]string{"iam", "token", "-person", "p-1", "-label", "ci",
		"-days", "30", "-grants", "state:read", "-colleague", "read",
		"-config", cfg, "-api", node.URL}, &out, &errs); err != nil {
		t.Fatalf("iam token: %v\n%s", err, errs.String())
	}
	if seen.method != http.MethodPost || seen.path != "/iam/credentials" {
		t.Fatalf("the node saw %s %s", seen.method, seen.path)
	}
	if seen.person != "p-1" {
		t.Errorf("the owner reached the node as ?person=%q, want p-1", seen.person)
	}
	if _, inBody := seen.body["person"]; inBody {
		t.Errorf("the owner travelled in the body too: %v", seen.body)
	}
	if seen.body["colleague"] != "read" || seen.body["label"] != "ci" ||
		seen.body["expires_in_days"] != float64(30) {
		t.Errorf("the body was %v", seen.body)
	}
	if grants, _ := seen.body["grants"].([]any); len(grants) != 1 ||
		grants[0] != "state:read" {
		t.Errorf("the grants reached the node as %v", seen.body["grants"])
	}
	if seen.auth != "Bearer a-tier-a-token" {
		t.Errorf("authenticated as %q", seen.auth)
	}
	for _, want := range []string{"cwl_pat_the-value", "state:read", apiTokenEnv} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output does not carry %q:\n%s", want, out.String())
		}
	}
}

// `none` IS A VALUE AND AN OMITTED FLAG IS NOT.
//
// Stripping somebody's last grant and not mentioning grants at all are
// opposite intentions, and an empty list cannot carry both: a body with
// `grants: []` strips, and one with no `grants` key leaves them alone. Without
// the word, the strip silently does not happen.
func TestStrippingGrantsIsSpeltAndNotImplied(t *testing.T) {
	empty, err := iamGrantsBody("", "")
	if err != nil {
		t.Fatalf("no flags: %v", err)
	}
	if _, mentioned := empty["grants"]; mentioned {
		t.Errorf("an omitted -grants sent a grants field: %v, which would "+
			"strip somebody nobody asked to strip", empty)
	}
	stripped, err := iamGrantsBody("none", "")
	if err != nil {
		t.Fatalf("-grants none: %v", err)
	}
	held, ok := stripped["grants"].([]string)
	if !ok || len(held) != 0 {
		t.Errorf("-grants none sent %v, want an empty list", stripped["grants"])
	}
	listed, err := iamGrantsBody(" state:read , audit:read ", "write")
	if err != nil {
		t.Fatalf("a list: %v", err)
	}
	if got := listed["grants"].([]string); len(got) != 2 ||
		got[0] != "state:read" || got[1] != "audit:read" {
		t.Errorf("the list parsed to %v", got)
	}
	if listed["colleague"] != "write" {
		t.Errorf("the colleague level parsed to %v", listed["colleague"])
	}
	if _, err := iamGrantsBody("", "boss"); err == nil {
		t.Error("`boss` was accepted as a colleague level")
	}
}

// A DIRECTORY NOBODY COULD CHECK IS NOT A CLEAN ONE.
//
// A node whose chart applier has stalled cannot say whether a bound person's
// seat exists, and the report counts those rather than guessing. Printing
// "nothing to report" over that count would tell an operator the directory is
// clean during exactly the stall that hides a dangling binding.
func TestAnUncheckedBindingIsSaidBeforeNothingToReport(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := &iamPrinter{w: &out}
	if err := p.check(map[string]any{
		"findings": []any{}, "position": "1:40", "bindings_unchecked": float64(2),
	}, nil); err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.Contains(out.String(), "2 seat binding(s) could not be checked") {
		t.Errorf("the unchecked bindings went unsaid:\n%s", out.String())
	}

	// AND THE CONTROL: a node that checked everything prints no caveat.
	out.Reset()
	if err := p.check(map[string]any{
		"findings": []any{}, "position": "1:40", "bindings_unchecked": float64(0),
	}, nil); err != nil {
		t.Fatalf("check: %v", err)
	}
	if strings.Contains(out.String(), "could not be checked") {
		t.Errorf("a fully checked directory carried the caveat:\n%s", out.String())
	}
}

// A DUPLICATED CLAIM NAMES EVERYBODY HOLDING IT, because the report exists so
// an operator can decide who keeps it — and a row naming one holder, or none,
// leaves them to find the rest by hand. An address is named by its kind alone:
// the report carries no form of it, so there is nothing to print.
func TestTheCheckNamesEveryHolderOfADuplicatedClaim(t *testing.T) {
	var out bytes.Buffer
	answer := map[string]any{
		"position": "iam@12",
		"findings": []any{
			map[string]any{
				"kind": "claim_duplicated", "claim": "login",
				"login": "ada.lovelace", "people": []any{"p-1", "p-2"},
				"detail": "held twice",
			},
			map[string]any{
				"kind": "claim_duplicated", "claim": "email",
				"people": []any{"p-3", "p-4"}, "detail": "held twice",
			},
			map[string]any{
				"kind": "person_without_credential", "person": "p-5",
				"login": "grace.hopper", "detail": "never signed in",
			},
		},
	}
	if err := (&iamPrinter{w: &out}).check(answer, nil); err != nil {
		t.Fatalf("print: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"login ada.lovelace: p-1, p-2",
		"email: p-3, p-4",
		"grace.hopper",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not say %q:\n%s", want, got)
		}
	}
}
