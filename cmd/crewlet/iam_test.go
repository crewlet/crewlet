package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
)

// A BEARER WRITE PASSES THE NODE'S OWN CROSS-SITE CHECK.
//
// The CLI dials the node on the address it can reach — loopback, on the host —
// and a deployment behind a proxy is reached by browsers at `api.external_url`,
// which is the only origin the node's check admits. The client used to send
// its dialled address as the Origin, so every `crewlet iam` write to such a
// deployment was refused as a cross-site request. The node here runs the REAL
// check, configured for an external URL the test server is not. Mutation:
// send the base URL as the Origin again and the suspension answers 403.
func TestAnIamWritePassesTheNodesCrossSiteCheck(t *testing.T) {
	var reached bool
	var boot config.Bootstrap
	boot.API.ExternalURL = "https://crewlet.example.com"
	node := httptest.NewServer(auth.NewCSRF(&boot).Middleware(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"position":"1:1"}`))
		})))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")

	var out, errs bytes.Buffer
	if err := run([]string{"iam", "suspend", "p-1", "-config", cfg,
		"-api", node.URL}, &out, &errs); err != nil {
		t.Fatalf("iam suspend through the node's cross-site check: %v\n%s",
			err, errs.String())
	}
	if !reached {
		t.Error("the write never reached the handler behind the check")
	}
}

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

// A CREATE WITH NO LOGIN IS REFUSED HERE, NAMING THE FLAG.
//
// Every principal enrols with a login — it is the name their changes are
// recorded under while they hold no seat, and a person created by address
// alone was recorded as `anonymous`. The node refuses one too; this is the
// sentence an operator who left the flag off needs, before a config file is
// read or a request is sent.
func TestAnIamCreateWithNoLoginNamesTheFlag(t *testing.T) {
	var out, errs bytes.Buffer
	err := run([]string{"iam", "create", "-email", "jane@example.com",
		"-config", "/nonexistent/crewlet.yaml"}, &out, &errs)
	if err == nil {
		t.Fatal("`crewlet iam create` with no -login was accepted")
	}
	if !strings.Contains(err.Error(), "-login") {
		t.Errorf("the refusal is %q, want it to name -login", err)
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

// THE TRAIL PRINTS A TOKEN'S GESTURE AS ONE.
//
// A token acts as its owner, so an entry's actor is the owner either way, and
// the credential beside it is what says their token did it — printed in the
// words every other trail this command prints uses. Mutation: print the actor
// alone and the two rows read the same.
func TestTheTrailPrintsATokensGestureAsOne(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := &iamPrinter{w: &out}
	if err := p.audit(map[string]any{"events": []any{
		map[string]any{"position": float64(2), "op": "status",
			"actor": "ana.admin", "operator_id": "pat:0192f00d-0000-7000-8000-00000000000a"},
		map[string]any{"position": float64(1), "op": "status", "actor": "ana.admin"},
	}}, nil); err != nil {
		t.Fatalf("audit: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("printed %d line(s), want a header and two rows:\n%s",
			len(lines), out.String())
	}
	if !strings.Contains(lines[1],
		"ana.admin (through pat:0192f00d-0000-7000-8000-00000000000a)") {
		t.Errorf("the token's gesture printed as %q", lines[1])
	}
	if strings.Contains(lines[2], "through") {
		t.Errorf("the owner's own gesture printed a credential: %q", lines[2])
	}
}

// A KEY NOBODY COULD JUDGE IS SAID BEFORE NOTHING TO REPORT, for the binding
// rule's reason: a node behind the identity log names no unowned key and
// counts them instead, and printing "nothing to report" over that count would
// read as a company with no key outliving its owner.
func TestAnUnjudgedKeyIsSaidBeforeNothingToReport(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := &iamPrinter{w: &out}
	if err := p.check(map[string]any{
		"findings": []any{}, "position": "1:40", "keys_unchecked": float64(3),
	}, nil); err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.Contains(out.String(), "3 key(s) no row here owns could not be judged") {
		t.Errorf("the unjudged keys went unsaid:\n%s", out.String())
	}
	out.Reset()
	if err := p.check(map[string]any{
		"findings": []any{}, "position": "1:40", "keys_unchecked": float64(0),
	}, nil); err != nil {
		t.Fatalf("check: %v", err)
	}
	if strings.Contains(out.String(), "could not be judged") {
		t.Errorf("a node that judged every key carried the caveat:\n%s", out.String())
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

// `iam link` PINS THE SUBJECT THE OPERATOR TYPED, AND `iam unlink` TAKES IT OFF.
//
// Both are the one edit the directory has for it — `oidc_subject` on the
// person — and the empty string is the unlink, so a command that sent nothing
// for `unlink` would be an edit that changes nothing and reports success. And
// a link with no subject is refused before the config is read, naming what is
// missing.
func TestIamLinkPinsTheSubjectTheOperatorTyped(t *testing.T) {
	type seen struct {
		method, path string
		body         map[string]any
	}
	var got []seen
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		one := seen{method: r.Method, path: r.URL.Path}
		_ = json.NewDecoder(r.Body).Decode(&one.body)
		got = append(got, one)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"p-1","position":"iam@9"}`))
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")

	var out, errs bytes.Buffer
	if err := run([]string{"iam", "link", "p-1", "00u1abcd", "-reason", "okta",
		"-config", cfg, "-api", node.URL}, &out, &errs); err != nil {
		t.Fatalf("iam link: %v\n%s", err, errs.String())
	}
	if err := run([]string{"iam", "unlink", "p-1", "-config", cfg, "-api",
		node.URL}, &out, &errs); err != nil {
		t.Fatalf("iam unlink: %v\n%s", err, errs.String())
	}
	if len(got) != 2 {
		t.Fatalf("the node saw %d requests, want 2", len(got))
	}
	for i, want := range []string{"00u1abcd", ""} {
		call := got[i]
		if call.method != http.MethodPatch || call.path != "/iam/people/p-1" {
			t.Errorf("request %d was %s %s", i, call.method, call.path)
		}
		subject, present := call.body["oidc_subject"]
		if !present || subject != want {
			t.Errorf("request %d carried oidc_subject %v (present %v), want %q",
				i, subject, present, want)
		}
	}

	err := run([]string{"iam", "link", "p-1"}, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "sub claim") {
		t.Errorf("a link with no subject answered %v, want the subject named", err)
	}
}

// A WRITE SAYS WHETHER IT LANDED HERE, read off the answer's outcome.
//
// The printer said "applied at" for every 2xx, so a write the node had made
// durable and not yet applied — a 202, which a read there does not show yet —
// was reported as applied. Mutation: print "applied" for every success again
// and the pending case reads as applied.
func TestAnIamWriteSaysWhetherItLandedHere(t *testing.T) {
	for _, tc := range []struct {
		status  int
		outcome string
	}{
		{http.StatusOK, "applied"},
		{http.StatusAccepted, "pending"},
	} {
		node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "p-1",
				"outcome": tc.outcome, "position": "CREWLET_IAM_LOG@0:9",
				"op_id": "people:update:p-1:k"})
		}))
		t.Setenv(apiTokenEnv, "a-tier-a-token")
		cfg := bootstrapWithKeyring(t, "k1")
		var out, errs bytes.Buffer
		err := run([]string{"iam", "suspend", "p-1", "-config", cfg, "-api",
			node.URL}, &out, &errs)
		node.Close()
		if err != nil {
			t.Fatalf("a %s suspension failed: %v", tc.outcome, err)
		}
		want := tc.outcome + " at CREWLET_IAM_LOG@0:9 (op people:update:p-1:k)"
		if !strings.Contains(out.String(), want) {
			t.Errorf("a %s write printed %q, want it to say %q", tc.outcome,
				out.String(), want)
		}
		if tc.outcome == "pending" && strings.Contains(out.String(), "applied") {
			t.Errorf("a pending write was reported as applied: %q", out.String())
		}
	}
}

// AN UNKNOWN WRITE NAMES ITS RETRY, AND THE RETRY CAN BE MADE.
//
// The node answers an unknown outcome 503 with the op id and says the only
// safe retry is the same operation, sent back as the Idempotency-Key. The CLI
// dropped the op id from the refusal and had no way to send a key, so the
// retry an operator could actually run was a fresh operation — for a create,
// a second person. Mutation: drop the op id from the refusal, or the header
// from the request, and each half goes red.
func TestAnUnknownIamWriteNamesItsRetryAndCanMakeIt(t *testing.T) {
	var keys []string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "unavailable",
			"detail": "this node cannot establish what happened to this change",
			"op_id":  "people:update:p-1:k7", "landed": []string{"seat"}})
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")

	var out, errs bytes.Buffer
	err := run([]string{"iam", "bind", "p-1", "sre", "-config", cfg, "-api",
		node.URL}, &out, &errs)
	if err == nil {
		t.Fatal("an unknown write was reported as a success")
	}
	for _, want := range []string{"-idempotency-key people:update:p-1:k7",
		"these changes DID land before it: seat"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	_ = run([]string{"iam", "bind", "p-1", "sre", "-idempotency-key",
		"people:update:p-1:k7", "-config", cfg, "-api", node.URL}, &out, &errs)
	if len(keys) != 2 || keys[0] != "" || keys[1] != "people:update:p-1:k7" {
		t.Errorf("the node saw keys %q, want none and then the retried op id",
			keys)
	}

	// A KEY ON A COMMAND THAT READS NONE is refused before anything is
	// sent, saying why: the node would ignore it and the operator would be
	// told nothing.
	for _, args := range [][]string{
		{"iam", "token", "-person", "svc-1"},
		{"iam", "bootstrap-code"},
		{"iam", "show", "p-1"},
	} {
		err := run(append(args, "-idempotency-key", "k", "-config", cfg,
			"-api", node.URL), &out, &errs)
		if err == nil || !strings.Contains(err.Error(), "idempotency-key") {
			t.Errorf("%v with a key answered %v, want it refused naming the flag",
				args, err)
		}
	}
	if len(keys) != 2 {
		t.Errorf("a refused key still reached the node: %q", keys)
	}
}
