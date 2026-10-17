package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
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
	if got := nodeTokenOrEmpty(config.EnvSource{}); got != "" {
		t.Errorf("with no environment variable the CLI produced a token (%q), "+
			"so it is reading one out of somewhere it should not", got)
	}
	_, err := nodeAPIToken(config.EnvSource{}, "/iam")
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
	if got := nodeTokenOrEmpty(config.EnvSource{}); got != "a-token" {
		t.Errorf("the environment token resolved to %q", got)
	}
}

// EVERY `iam` SUBCOMMAND IS DISPATCHED AND ADVERTISED, in both directions.
//
// Nothing connects runIAM's switch to iamUsage, so a subcommand can ship
// reachable and undocumented or documented and unreachable — and `create`'s
// -name flag had already gone unadvertised. So the switch is read out of the
// source, as run()'s is, and held against the usage text's lines: every case
// named by a `crewlet iam <word>` line, and every such line a case. Mutation:
// drop the `seats` line from the usage, or its case from the switch, and one
// half fails.
func TestEveryIamSubcommandIsDispatchedAndAdvertised(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	if err := runIAM(noEnviron, nil, nil, &out, &errs); err == nil {
		t.Error("`crewlet iam` with no subcommand did not ask for one")
	}
	usage := out.String()
	dispatched := switchCases(t, "iam.go", "runIAM", "sub")
	advertised := advertisedWords(t, usage, "crewlet iam ", "\nFlags:")
	for _, sub := range dispatched {
		if !slices.Contains(advertised, sub) {
			t.Errorf("runIAM dispatches %q but the usage never names `crewlet "+
				"iam %s`, so it can only be found by reading the source", sub, sub)
		}
	}
	for _, sub := range advertised {
		if !slices.Contains(dispatched, sub) {
			t.Errorf("the usage names `crewlet iam %s` and runIAM dispatches no "+
				"such subcommand, so it answers `unknown subcommand`", sub)
		}
	}
}

// AN INVITATION AND A CREATE ARE TOLD WHAT THEY LACK BEFORE ANYTHING IS SENT,
// naming the flag — before a config file is read, so the operator hears about
// the flag rather than about a file they were never going to need.
//
// A person holds a human seat for as long as they are here, so an invitation
// and a person's create each name one; a person is found by their address,
// and their login may be left to the node, which proposes it from that
// address; a service account has no address to propose from, so its login is
// required and its seat is not — and nobody is behind it to have meant a
// default, so what it carries is required too. The node refuses each the same
// way — this is
// the sentence an operator who left a flag off needs. The controls are the
// complete commands, which pass every check here and stop only at the config
// file nobody wrote. Mutation: drop any one check and its row reaches the
// config file instead of naming its flag.
func TestAnIamNewcomerIsToldWhatItLacksBeforeAnythingIsSent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		// want is what the refusal names, or nil for a command complete
		// enough to reach the config file.
		want []string
	}{
		{"an invitation with no seat",
			[]string{"invite", "jane@example.com"},
			[]string{"-seat", "crewlet iam seats -unheld"}},
		{"a person with no address",
			[]string{"create", "-seat", "founder"},
			[]string{"-email"}},
		{"a person with no seat",
			[]string{"create", "-email", "jane@example.com"},
			[]string{"-seat", "crewlet iam seats -unheld", "service accounts"}},
		{"a person named as one, with no seat",
			[]string{"create", "-kind", "person", "-email", "jane@example.com",
				"-login", "jane.doe"},
			[]string{"-seat"}},
		{"a service account with no login",
			[]string{"create", "-kind", "machine"},
			[]string{"-login"}},
		{"a service account naming nothing it carries",
			[]string{"create", "-kind", "machine", "-login", "ci:release"},
			[]string{"-grants", "or none"}},
		{"an invitation onto a seat", []string{"invite", "jane@example.com",
			"-seat", "founder"}, nil},
		{"a person with no login", []string{"create", "-email",
			"jane@example.com", "-seat", "founder"}, nil},
		{"a service account with no seat", []string{"create", "-kind",
			"machine", "-login", "ci:release", "-grants", "none"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			err := runIAM(noEnviron, append(tc.args, "-config",
				"/nonexistent/crewlet.yaml"), nil, &out, &errs)
			if err == nil {
				t.Fatal("the command went through with no config file at all")
			}
			if tc.want == nil {
				if !strings.Contains(err.Error(), "/nonexistent/crewlet.yaml") {
					t.Errorf("a complete command was refused before its "+
						"config was read: %v", err)
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is %q, want it to name %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "/nonexistent/crewlet.yaml") {
				t.Errorf("the refusal came from the config file, not the "+
					"missing flag: %v", err)
			}
		})
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
// what the authority table decides on, and the grants and lifetime in the body
// — and it prints the value with what it carries.
//
// It used to name the owner in a body field the route no longer reads, so
// every token it minted was the caller's own whatever the command said.
// Mutation: send the person in the body again and the node sees no `person`
// query.
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
			`"grants":["worklet"],` +
			`"expires_at":"2026-07-01T00:00:00Z"}`))
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")

	var out, errs bytes.Buffer
	if err := run([]string{"iam", "token", "-person", "p-1", "-label", "ci",
		"-days", "30", "-grants", "worklet",
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
	if seen.body["label"] != "ci" || seen.body["expires_in_days"] != float64(30) {
		t.Errorf("the body was %v", seen.body)
	}
	if grants, _ := seen.body["grants"].([]any); len(grants) != 1 ||
		grants[0] != "worklet" {
		t.Errorf("the grants reached the node as %v", seen.body["grants"])
	}
	if seen.auth != "Bearer a-tier-a-token" {
		t.Errorf("authenticated as %q", seen.auth)
	}
	for _, want := range []string{"cwl_pat_the-value", "worklet", apiTokenEnv} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output does not carry %q:\n%s", want, out.String())
		}
	}
}

// `none` IS A VALUE AND AN OMITTED FLAG IS NOT.
//
// Stripping somebody's last permission and not mentioning permissions at all
// are opposite intentions, and an empty list cannot carry both: a body with
// `grants: []` strips, and one with no `grants` key leaves them alone. Without
// the word, the strip silently does not happen.
func TestStrippingGrantsIsSpeltAndNotImplied(t *testing.T) {
	t.Parallel()
	read := func(flagged string) iamGrantList {
		t.Helper()
		granted, err := iamGrants(flagged)
		if err != nil {
			t.Fatalf("-grants %q: %v", flagged, err)
		}
		return granted
	}
	empty := read("").body()
	if _, mentioned := empty["grants"]; mentioned {
		t.Errorf("an omitted -grants sent a grants field: %v, which would "+
			"strip somebody nobody asked to strip", empty)
	}
	stripped := read("none").body()
	held, ok := stripped["grants"].([]string)
	if !ok || len(held) != 0 {
		t.Errorf("-grants none sent %v, want an empty list", stripped["grants"])
	}
	// TRIMMED, AND IN THE ENGINE'S ORDER whatever order it was typed in,
	// each name once.
	listed := read(" doclet , worklet,doclet ").body()
	if got := listed["grants"].([]string); !slices.Equal(got,
		[]string{"worklet", "doclet"}) {
		t.Errorf("the list parsed to %v, want worklet, doclet", got)
	}
}

// -grants ADMIN STORES EVERY APP BESIDE IT, as the dashboard's picker does.
//
// Admin includes every app whether or not a row says so, so what this buys is
// what happens when admin is taken AWAY: a row that listed the apps beside it
// keeps them, and an administrator stepping down (a `remove_grants: [admin]`
// from the dashboard, say) goes on using Worklet. A command that sent
// admin alone would leave that row holding nothing past the overview. So the
// expansion is the command's, on every subcommand that sends a set, and it
// reaches the node as the stored list.
//
// Mutation: send admin alone and every row below fails.
func TestTheCLIStoresEveryAppWithAdmin(t *testing.T) {
	t.Parallel()
	every := []string{string(iam.GrantAdmin)}
	for _, app := range iam.Apps {
		every = append(every, string(app))
	}
	for _, flagged := range []string{"admin", "doclet,admin", " admin , worklet "} {
		granted, err := iamGrants(flagged)
		if err != nil {
			t.Fatalf("-grants %q: %v", flagged, err)
		}
		if got, _ := granted.body()["grants"].([]string); !slices.Equal(got, every) {
			t.Errorf("-grants %q sends %v, want %v", flagged, got, every)
		}
	}

	// THROUGH THE COMMAND, onto the wire, on each subcommand that sends a
	// set: an invitation, a create, a grant and a mint.
	var bodies []map[string]any
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x-1","outcome":"applied","position":"p"}`))
	}))
	defer node.Close()
	cfg := bootstrapWithKeyring(t, "k1")
	for _, args := range [][]string{
		{"invite", "lead@example.com", "-seat", "lead", "-grants", "admin"},
		{"create", "-email", "lead@example.com", "-seat", "lead", "-grants", "admin"},
		{"create", "-kind", "machine", "-login", "ci:release", "-grants", "admin"},
		{"grant", "p-1", "-grants", "admin"},
		{"token", "-person", "svc-1", "-grants", "admin"},
	} {
		var out, errs bytes.Buffer
		err := runIAM(tokenEnviron("a-tier-a-token"), append(args, "-config", cfg,
			"-api", node.URL), nil, &out, &errs)
		if err != nil {
			t.Fatalf("iam %s: %v\n%s", strings.Join(args, " "), err, errs.String())
		}
	}
	if len(bodies) != 5 {
		t.Fatalf("the node was asked %d times, want 5", len(bodies))
	}
	for i, body := range bodies {
		got := make([]string, 0, 3)
		raw, _ := body["grants"].([]any)
		for _, g := range raw {
			s, _ := g.(string)
			got = append(got, s)
		}
		if !slices.Equal(got, every) {
			t.Errorf("request %d carried grants %v, want %v", i, body["grants"], every)
		}
	}
}

// A PERSON LEFT WITHOUT -grants IS GIVEN EVERY APP, AND NEVER ADMIN.
//
// The node requires `grants` on an invitation and on a create — `[]` is a real
// answer there, the overview alone — so what a person is given when the
// command says nothing has to be the command's, and it is the colleague's:
// every app, which is what a teammate needs to be sent work and to read what
// the company wrote down. Admin is never a default. A token left without
// -grants is the opposite case: it sends no field at all, which the node reads
// as everything the owner holds — the default the help states for each.
//
// Mutation: send no grants on a person's invitation or create and the first
// two rows fail; send the apps on a mint and the third does.
func TestAPersonsInviteDefaultsToEveryApp(t *testing.T) {
	t.Parallel()
	var bodies []map[string]any
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"x-1","outcome":"applied","position":"p"}`))
	}))
	defer node.Close()
	cfg := bootstrapWithKeyring(t, "k1")
	for _, args := range [][]string{
		{"invite", "lead@example.com", "-seat", "lead"},
		{"create", "-email", "lead@example.com", "-seat", "lead"},
		{"token", "-person", "svc-1"},
	} {
		var out, errs bytes.Buffer
		if err := runIAM(tokenEnviron("a-tier-a-token"), append(args, "-config", cfg,
			"-api", node.URL), nil, &out, &errs); err != nil {
			t.Fatalf("iam %s: %v\n%s", strings.Join(args, " "), err, errs.String())
		}
	}
	if len(bodies) != 3 {
		t.Fatalf("the node was asked %d times, want 3", len(bodies))
	}
	apps := make([]any, 0, len(iam.Apps))
	for _, g := range iam.Apps {
		apps = append(apps, string(g))
	}
	for i, what := range []string{"an invitation", "a person's create"} {
		if got, _ := bodies[i]["grants"].([]any); !slices.Equal(got, apps) {
			t.Errorf("%s naming no -grants sent %v, want every app %v and not "+
				"admin", what, bodies[i]["grants"], apps)
		}
	}
	if grants, sent := bodies[2]["grants"]; sent {
		t.Errorf("a mint naming no -grants sent %v, want no field — the node "+
			"reads its absence as everything the owner holds", grants)
	}

	// AND A GRANT NAMING NOTHING IS REFUSED rather than sent: it is the one
	// subcommand whose whole job is the set.
	var out, errs bytes.Buffer
	err := runIAM(tokenEnviron("a-tier-a-token"), []string{"grant", "p-1",
		"-config", cfg, "-api", node.URL}, nil, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "-grants") {
		t.Errorf("a grant naming no -grants answered %v, want a refusal "+
			"naming the flag", err)
	}
	if len(bodies) != 3 {
		t.Errorf("the refused grant reached the node: %v", bodies[3:])
	}
}

// A SERVICE ACCOUNT IS TOLD WHAT IT CARRIES, EVERY TIME — and every -grants is
// held to the engine's vocabulary — before a config file is read.
//
// A person left without -grants is given every app, but nobody is behind a
// service account to have meant a default, so its create without -grants is
// refused naming the flag; `-grants none` is how to decide nothing. A name the
// engine does not know, `none` beside a permission, and -grants on a command
// that sends no set are each refused naming what to do — and every refusal
// lands before the config is read, which this case proves by naming a config
// file that does not exist.
//
// Mutation: default a service account's grants, accept an unknown name, or
// ignore -grants on suspend, and a row is answered by the missing config
// instead.
func TestAMachineCreateNamesItsGrants(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-such-config.yaml")
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"create", "-kind", "machine", "-login", "ci:release"},
			[]string{"-grants", "or none"}},
		{[]string{"create", "-kind", "machine", "-login", "ci:release",
			"-grants", "trackr"},
			[]string{`"trackr"`, "admin, worklet, doclet", "none"}},
		{[]string{"invite", "lead@example.com", "-seat", "lead",
			"-grants", "billing"},
			[]string{`"billing"`, "admin, worklet, doclet"}},
		{[]string{"grant", "p-1", "-grants", "none,admin"},
			[]string{"none", "on its own"}},
		{[]string{"suspend", "p-1", "-grants", "admin"},
			[]string{"-grants only works with", "iam suspend"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			var out, errs bytes.Buffer
			err := runIAM(tokenEnviron("a-tier-a-token"),
				append(tc.args, "-config", missing), nil, &out, &errs)
			if err == nil {
				t.Fatal("accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q", err, want)
				}
			}
			if strings.Contains(err.Error(), missing) {
				t.Errorf("the refusal came from reading the config, after the "+
					"flags should have been refused: %v", err)
			}
		})
	}

	// THE CONTROL: the same create naming its grants — `none` among them —
	// is refused only for the config this case withholds.
	var out, errs bytes.Buffer
	err := runIAM(tokenEnviron("a-tier-a-token"), []string{"create", "-kind",
		"machine", "-login", "ci:release", "-grants", "none", "-config", missing},
		nil, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("a service account's create naming -grants none answered %v, "+
			"want it to get as far as reading the config", err)
	}
}

// THE COMMAND NEVER SPELLS THE PERMISSION LIST BY HAND.
//
// The help, the -grants flag's own line and the refusals that tell an
// operator what to type all list the permissions, and four of them had the
// list typed out while [iamGrantWords] — written so that list would follow
// [iam.AllGrants] — was used by two. A typed copy is right on the day it is
// written and goes on naming three permissions after a fourth exists, which
// no behavioural case can see while the copy still matches. So this reads
// every string this package's source builds, a constant concatenation folded
// into one sentence, and refuses one that names every permission, or every
// app, as a list.
//
// Mutation: type "admin, worklet and doclet" back into the grant refusal, or
// "(worklet, doclet)" back into the help, and this fails naming the line.
func TestTheCLINeverSpellsTheGrantListByHand(t *testing.T) {
	t.Parallel()
	// ANY LIST OF THE VOCABULARY'S WORDS, and not only the whole of it: a
	// list typed when the vocabulary was smaller names fewer words than it
	// holds now, and a pattern built from today's vocabulary alone passes
	// exactly that copy -- the defaults, "worklet and doclet", went on
	// standing in the help after the artifacts app arrived.
	if len(iam.AllGrants) < 2 {
		t.Fatal("the permissions are not two words long, so there is no list " +
			"to spell and this guard asserts nothing")
	}
	words := make([]string, 0, len(iam.AllGrants))
	for _, g := range iam.AllGrants {
		words = append(words, regexp.QuoteMeta(string(g)))
	}
	word := `(?:` + strings.Join(words, "|") + `)`
	list := regexp.MustCompile(`(?i)\b` + word + `(?:(?:,? (?:and|or) |, )` + word + `)+\b`)
	vocabulary := regexp.MustCompile(`(?i)\b` + word + `\b`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		read++
		for _, built := range stringsBuilt(parsed) {
			text := strings.Join(strings.Fields(built.text), " ")
			for _, at := range list.FindAllStringIndex(text, -1) {
				spelled := text[at[0]:at[1]]
				distinct := map[string]bool{}
				for _, w := range vocabulary.FindAllString(spelled, -1) {
					distinct[strings.ToLower(w)] = true
				}
				if len(distinct) < 2 {
					continue
				}
				t.Errorf("%s spells the list %q by hand: build it from "+
					"iamGrantWords or iamAppWords, which follow the "+
					"engine's own, so a permission a later build adds is "+
					"named there too", fset.Position(built.pos), spelled)
			}
		}
	}
	if read == 0 {
		t.Fatal("no source file was read, so this guard asserts nothing")
	}
}

// builtString is one string a source file builds out of literals: a lone
// literal, or a `+` chain of them folded into one, with a NUL where an operand
// that is not a literal sits — so a sentence wrapped across lines is read as
// the sentence it prints, and a word supplied by a call never joins the words
// either side of it.
type builtString struct {
	pos  token.Pos
	text string
}

func stringsBuilt(file *ast.File) []builtString {
	var built []builtString
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BasicLit:
			if s, ok := stringLiteral(e); ok {
				built = append(built, builtString{e.Pos(), s})
			}
			return false
		case *ast.BinaryExpr:
			if e.Op != token.ADD {
				return true
			}
			var b strings.Builder
			literal := false
			var operand func(ast.Expr)
			operand = func(x ast.Expr) {
				x = ast.Unparen(x)
				if chain, ok := x.(*ast.BinaryExpr); ok && chain.Op == token.ADD {
					operand(chain.X)
					operand(chain.Y)
					return
				}
				if s, ok := stringLiteral(x); ok {
					b.WriteString(s)
					literal = true
					return
				}
				b.WriteByte(0)
				ast.Inspect(x, visit)
			}
			operand(e.X)
			operand(e.Y)
			if literal {
				built = append(built, builtString{e.Pos(), b.String()})
			}
			return false
		}
		return true
	}
	ast.Inspect(file, visit)
	return built
}

// A DIRECTORY NOBODY COULD CHECK IS NOT A CLEAN ONE.
//
// A node running no company yet cannot say whether a bound person's seat
// exists, and the report counts those rather than guessing. Printing
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
	if !strings.Contains(out.String(), "2 seat binding(s) couldn't be checked") {
		t.Errorf("the unchecked bindings went unsaid:\n%s", out.String())
	}

	// AND THE CONTROL: a node that checked everything prints no caveat.
	out.Reset()
	if err := p.check(map[string]any{
		"findings": []any{}, "position": "1:40", "bindings_unchecked": float64(0),
	}, nil); err != nil {
		t.Fatalf("check: %v", err)
	}
	if strings.Contains(out.String(), "couldn't be checked") {
		t.Errorf("a fully checked directory carried the caveat:\n%s", out.String())
	}
}

// A FINDING NAMES WHO IT IS ABOUT BY THEIR LOGIN, and by their id where the
// finding carries none, since the remedy is a command naming them.
// Mutation: print the id whatever the row carries and the login row fails.
func TestAFindingNamesWhoItIsAbout(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := (&iamPrinter{w: &out}).check(map[string]any{"position": "1:40",
		"findings": []any{
			map[string]any{"kind": "binding_dangling", "person": "p-1",
				"login": "jane.doe", "seat": "platform-lead",
				"detail": "the seat is gone"},
			map[string]any{"kind": "person_without_credential", "person": "p-2",
				"detail": "cannot sign in"},
		}}, nil); err != nil {
		t.Fatalf("check: %v", err)
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) < 3 || !strings.Contains(lines[1], "binding_dangling") ||
		!strings.Contains(lines[1], "jane.doe") || strings.Contains(lines[1], "p-1") {
		t.Errorf("a finding about somebody with a login printed %q", lines)
	}
	if !strings.Contains(lines[2], "p-2") {
		t.Errorf("a finding about somebody with no login printed %q", lines[2])
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
	if err := p.audit(nil, map[string]any{"events": []any{
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
// The node answers an unknown outcome 503 with `outcome: "unknown"` and the
// op id, written by the engine's one writer of that answer
// (httpjson.UnknownOutcome), and says the only safe retry is the same
// operation, sent back as the Idempotency-Key. The CLI dropped the op id from
// the refusal and had no way to send a key, so the retry an operator could
// actually run was a fresh operation — for a create, a second person. And it
// read the answer as `unavailable: …`, a node that did nothing, of a write
// that may have landed. Mutation: drop the op id from the refusal, the header
// from the request, or the outcome branch, and each half goes red.
func TestAnUnknownIamWriteNamesItsRetryAndCanMakeIt(t *testing.T) {
	// AN ID IN THE ENGINE'S GRAMMAR, as the node answers with: the command
	// holds the one sent back to that rule before anything is sent.
	op := statelog.NewOpID(time.Now(), "")
	var keys []string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		httpjson.UnknownOutcome(w, 2, op, false, httpjson.Detail{
			"detail": "this node cannot establish what happened to this change",
			"landed": []string{"seat"}})
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
	for _, want := range []string{"-op-id " + op,
		"these changes were saved before it: seat",
		"couldn't confirm whether this change was saved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	// ON THIS NODE: nothing in the answer says another would do better.
	if strings.Contains(err.Error(), "another node") {
		t.Errorf("an unknown this node can settle was sent elsewhere: %v", err)
	}
	_ = run([]string{"iam", "bind", "p-1", "sre", "-op-id",
		op, "-config", cfg, "-api", node.URL}, &out, &errs)
	if len(keys) != 2 || keys[0] != "" || keys[1] != op {
		t.Errorf("the node saw keys %q, want none and then the retried op id",
			keys)
	}

	// A KEY ON A COMMAND THAT READS NONE is refused before anything is
	// sent, saying why: the node would ignore it and the operator would be
	// told nothing.
	for _, args := range [][]string{
		{"iam", "token", "-person", "svc-1"},
		{"iam", "show", "p-1"},
	} {
		err := run(append(args, "-op-id", "k", "-config", cfg,
			"-api", node.URL), &out, &errs)
		if err == nil || !strings.Contains(err.Error(), "-op-id") {
			t.Errorf("%v with a key answered %v, want it refused naming the flag",
				args, err)
		}
	}
	if len(keys) != 2 {
		t.Errorf("a refused key still reached the node: %q", keys)
	}

	// THE CONTROL: a 503 that wrote nothing carries the op id too, and is
	// not said to have maybe landed.
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, 0, httpjson.Detail{
			"detail": "the log is full", "op_id": op})
	}))
	defer refused.Close()
	err = run([]string{"iam", "bind", "p-1", "sre", "-config", cfg, "-api",
		refused.URL}, &out, &errs)
	if err == nil || strings.Contains(err.Error(), "could not establish") {
		t.Errorf("a refusal that wrote nothing reads %v, want no unknown outcome", err)
	}
}

// AN UNKNOWN MINT IS RETRIED AS A NEW MINT, NEVER UNDER A KEY.
//
// A mint's route reads no Idempotency-Key and this command refuses the flag on
// `token`, so the retry every other unknown prescribes here — the same command
// with -op-id — sent an operator to a flag they would be refused.
// The node's own sentence says to mint again; the op id is named for the
// trail. Mutation: prescribe the key for every 503 again and this goes red.
func TestAnUnknownMintIsNotRetriedUnderAKey(t *testing.T) {
	op := statelog.NewOpID(time.Now(), "")
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpjson.UnknownOutcome(w, 2, op, false, httpjson.Detail{
			"detail": "no token was issued: mint again for one you hold"})
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")

	var out, errs bytes.Buffer
	err := run([]string{"iam", "token", "-person", "svc-1", "-config", cfg, "-api",
		node.URL}, &out, &errs)
	if err == nil {
		t.Fatal("an unknown mint was reported as a success")
	}
	for _, want := range []string{"couldn't confirm whether this change was saved",
		"mint again", "op " + op} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "-op-id") {
		t.Errorf("an unknown mint prescribes a flag `iam token` refuses: %v", err)
	}
}

// AN UNKNOWN THIS NODE CANNOT VOUCH FOR IS RETRIED THROUGH ANOTHER NODE.
//
// The node says `unvouched` when its operation ledger may have lost the row
// the operation needs: it published nothing, and asked again it answers the
// same way until the change reaches it. The command prescribed the same retry
// here for every 503, which for this one is a loop. Mutation: drop the case
// and the advice names no other node.
func TestAnUnvouchedIamWriteIsRetriedThroughAnotherNode(t *testing.T) {
	op := statelog.NewOpID(time.Now(), "")
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "unavailable",
			"detail":    "this node's operation ledger cannot vouch for this change",
			"op_id":     op,
			"unvouched": true})
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")

	var out, errs bytes.Buffer
	err := run([]string{"iam", "suspend", "p-1", "-config", cfg, "-api", node.URL},
		&out, &errs)
	if err == nil {
		t.Fatal("an unvouched write was reported as a success")
	}
	for _, want := range []string{"another node with -api",
		"-op-id " + op, "Don't use a new id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
}

// A KEY THE ENGINE COULD NOT HAVE MINTED IS REFUSED BEFORE ANYTHING IS SENT,
// naming the flag: the node refuses it `400 op_id_invalid` anyway, and an id
// with no instant is one no ledger could vouch for. Mutation: drop the check
// and the node is asked.
func TestAnIamKeyTheEngineNeverMintedIsRefusedBeforeItIsSent(t *testing.T) {
	var asked bool
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked = true
		w.WriteHeader(http.StatusOK)
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")

	var out, errs bytes.Buffer
	err := run([]string{"iam", "suspend", "p-1", "-op-id",
		"people:update:p-1:k7", "-config", cfg, "-api", node.URL}, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "-op-id") {
		t.Fatalf("a hand-made key answered %v, want a refusal naming the flag", err)
	}
	if asked {
		t.Error("a hand-made key still reached the node")
	}
}

// AN INVITATION AND A CREATE TAKE THE SEAT THE OPERATOR NAMED, and only they
// take the flag.
//
// A person holds a human seat for as long as they are here, so `-seat` has to
// reach the body the node reads on an invitation and on a create alike — and a
// person's create answers their first password link, which the command prints
// once. A service account's create names no seat and answers no link: the
// command says how to give it a token instead. On any other command `-seat`
// is refused rather than ignored: `iam grant ID -seat lead` read as accepted
// would say a seat was taken that nothing sent — a seat is MOVED with `bind`,
// which trims it as the flag is. Mutation: drop the seat from either body and
// the node sees none; drop the link block and the url goes unprinted; send
// bind's seat untrimmed and the node is asked for a padded handle; drop the
// refusal and the grant is sent.
func TestAnIamInviteAndCreateTakeTheSeatTheOperatorNamed(t *testing.T) {
	var asked []string
	var bodies []map[string]any
	// THE NODE'S OPERATION IDS ARE THE GRAMMAR'S, as a real node mints them:
	// the retry the create prints is one this command accepts, which a
	// hand-written `op-1` is not.
	personOp, machineOp := statelog.NewOpID(time.Now(), ""), statelog.NewOpID(time.Now(), "")
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		switch {
		case r.URL.Path == "/iam/invitations":
			_, _ = w.Write([]byte(`{"id":"inv-1","outcome":"applied",` +
				`"url":"https://crewlet.example.com/dashboard#/invite/inv-1.s3cr3t",` +
				`"expires_at":"2026-06-21T12:00:00Z","seat":"platform-lead"}`))
		case body["kind"] == "machine":
			_, _ = w.Write([]byte(`{"id":"svc-1","kind":"machine",` +
				`"login":"ci:release","outcome":"applied","position":"p",` +
				`"op_id":"` + machineOp + `"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"p-1","kind":"person",` +
				`"login":"lead","seat":"platform-lead","outcome":"applied",` +
				`"position":"p","op_id":"` + personOp + `","credential":"c-1",` +
				`"url":"https://crewlet.example.com/dashboard#/reset/c-1.f1rst",` +
				`"expires_at":"2026-06-21T12:00:00Z",` +
				`"detail":"a retry under the same Idempotency-Key hands back this same link"}`))
		}
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")
	cli := func(args ...string) (string, error) {
		var out, errs bytes.Buffer
		err := run(append(append([]string{"iam"}, args...), "-config", cfg,
			"-api", node.URL), &out, &errs)
		return out.String(), err
	}

	printed, err := cli("invite", "lead@example.com", "-seat", " platform-lead ")
	if err != nil {
		t.Fatalf("iam invite: %v", err)
	}
	if len(asked) != 1 || asked[0] != "POST /iam/invitations" {
		t.Fatalf("the node saw %v, want one POST /iam/invitations", asked)
	}
	if bodies[0]["seat"] != "platform-lead" || bodies[0]["email"] != "lead@example.com" {
		t.Errorf("the invitation carried %v, want the address and seat platform-lead",
			bodies[0])
	}
	if !strings.Contains(printed, "#/invite/inv-1.s3cr3t") {
		t.Errorf("the invitation's link was not printed:\n%s", printed)
	}

	// A PERSON'S CREATE, onto the seat, with the login left to the node.
	printed, err = cli("create", "-email", "lead@example.com", "-seat",
		"platform-lead")
	if err != nil {
		t.Fatalf("iam create of a person: %v", err)
	}
	if len(asked) != 2 || asked[1] != "POST /iam/people" {
		t.Fatalf("the node saw %v, want a POST /iam/people", asked)
	}
	if bodies[1]["seat"] != "platform-lead" || bodies[1]["email"] != "lead@example.com" {
		t.Errorf("the create carried %v, want the address and seat platform-lead",
			bodies[1])
	}
	for _, want := range []string{"p-1", "login lead, on seat platform-lead",
		"applied at p (op " + personOp + ")", "password link for p-1",
		"https://crewlet.example.com/dashboard#/reset/c-1.f1rst",
		"shown once", "-op-id " + personOp} {
		if !strings.Contains(printed, want) {
			t.Errorf("the person's create printed no %q:\n%s", want, printed)
		}
	}
	// THE PRINTED RETRY RUNS: the key after -op-id is one the
	// command's own check admits, so the retry is not refused before it is
	// sent.
	_, after, _ := strings.Cut(printed, "-op-id ")
	if key, _, _ := strings.Cut(strings.TrimSpace(after), " "); statelog.CheckCallerOpID(
		strings.Trim(key, "`'\".,;")) != nil {
		t.Errorf("the create printed a retry under %q, which the command refuses", key)
	}
	// THE NODE'S SENTENCE NAMES A HEADER, which nobody at a terminal sends.
	if strings.Contains(printed, "Idempotency-Key") {
		t.Errorf("the create printed the node's header sentence:\n%s", printed)
	}

	// A SERVICE ACCOUNT'S, with no seat: sent with none, and no link —
	// and with the grants it was told to carry, here nothing past the
	// overview, which nobody would have meant by leaving the flag out.
	printed, err = cli("create", "-kind", "machine", "-login", "ci:release",
		"-grants", "none")
	if err != nil {
		t.Fatalf("iam create of a service account: %v", err)
	}
	if len(asked) != 3 || bodies[2]["seat"] != "" || bodies[2]["kind"] != "machine" {
		t.Errorf("the service account's create carried %v (%v)", bodies[2], asked)
	}
	if grants, ok := bodies[2]["grants"].([]any); !ok || len(grants) != 0 {
		t.Errorf("-grants none reached the node as %v, want an empty list",
			bodies[2]["grants"])
	}
	if strings.Contains(printed, "first password link") ||
		!strings.Contains(printed, "crewlet iam token -person svc-1") {
		t.Errorf("the service account's create printed:\n%s", printed)
	}

	// A MOVE NAMES ITS SEAT POSITIONALLY, trimmed as the flag is: a handle
	// typed with a stray space is a seat the company does not have.
	if _, err = cli("bind", "p-1", " platform-lead "); err != nil {
		t.Fatalf("iam bind: %v", err)
	}
	if len(asked) != 4 || asked[3] != "PATCH /iam/people/p-1" ||
		bodies[3]["seat"] != "platform-lead" {
		t.Errorf("the move sent %v to %v, want seat platform-lead", bodies[3], asked)
	}

	// AND NOWHERE ELSE.
	if _, err = cli("grant", "p-1", "-grants", "worklet", "-seat",
		"platform-lead"); err == nil || !strings.Contains(err.Error(), "iam bind") {
		t.Errorf("a grant given -seat answered %v, want a refusal naming `iam bind`",
			err)
	}
	if len(asked) != 4 {
		t.Errorf("the refused grant was sent anyway: %v", asked)
	}
}

// A CREATE SAYS WHAT BECAME OF ITS LINK, however the node answered.
//
// The link is printed on `pending` too — the record is durable, and the link
// opens wherever it has been applied — and a retry that finds the link spent,
// revoked or aged out is answered without one: the command says so and names
// the command that issues another, rather than printing an empty link block.
// Mutation: print the link block only on `applied`, or drop the closed arm,
// and a row fails.
func TestACreateSaysWhatBecameOfItsLink(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer map[string]any
		want   []string
		not    []string
	}{
		{"pending", map[string]any{"id": "p-1", "kind": "person",
			"login": "jane.doe", "seat": "founder", "outcome": "pending",
			"position": "CREWLET_IAM_LOG@0:9", "op_id": statelog.NewOpID(time.Now(), ""),
			"url":        "https://crewlet.example.com/dashboard#/reset/c-1.s",
			"expires_at": "2026-06-21T12:00:00Z"},
			[]string{"pending at CREWLET_IAM_LOG@0:9", "password link for",
				"#/reset/c-1.s"}, []string{"applied"}},
		{"closed", map[string]any{"id": "p-1", "kind": "person",
			"login": "jane.doe", "seat": "founder", "outcome": "applied",
			"position": "x", "op_id": statelog.NewOpID(time.Now(), ""),
			"detail": "the first password link no longer opens"},
			[]string{"no longer opens", "crewlet iam reset-password p-1"},
			[]string{"first password link for"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := (&iamPrinter{w: &out}).created(tc.answer, nil); err != nil {
				t.Fatalf("created: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("printed no %q:\n%s", want, out.String())
				}
			}
			for _, not := range tc.not {
				if strings.Contains(out.String(), not) {
					t.Errorf("printed %q:\n%s", not, out.String())
				}
			}
		})
	}
}

// A CREATE WHOSE OUTCOME NOBODY KNOWS IS RETRIED UNDER ITS KEY, AND THE RETRY
// HANDS BACK THE LINK.
//
// A person's first password link is derived from the operation, so the retry
// an unknown answer prescribes — the same command with -op-id — is
// one that can hand the link back: the command has to name it, send it, and
// print what the retry answers. Mutation: drop create from the keyed
// subcommands and the retry is refused before it is sent.
func TestAnUnknownCreateIsRetriedUnderItsKeyForTheSameLink(t *testing.T) {
	op := statelog.NewOpID(time.Now(), "")
	var keys []string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		keys = append(keys, key)
		if key == "" {
			httpjson.UnknownOutcome(w, 2, op, false, httpjson.Detail{
				"detail": "this node cannot establish what happened to this change",
				"id":     "p-1"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "p-1",
			"kind": "person", "login": "jane.doe", "seat": "founder",
			"outcome": "applied", "position": "x", "op_id": op,
			"url":        "https://crewlet.example.com/dashboard#/reset/c-1.same",
			"expires_at": "2026-06-21T12:00:00Z"})
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")
	args := []string{"iam", "create", "-email", "jane@example.com", "-seat",
		"founder", "-config", cfg, "-api", node.URL}

	var out, errs bytes.Buffer
	err := run(args, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "-op-id "+op) {
		t.Fatalf("an unknown create answered %v, want the retry named", err)
	}
	out.Reset()
	if err := run(append(args, "-op-id", op), &out, &errs); err != nil {
		t.Fatalf("the retry under the key: %v", err)
	}
	if len(keys) != 2 || keys[1] != op {
		t.Errorf("the node saw keys %q, want none and then %s", keys, op)
	}
	if !strings.Contains(out.String(), "#/reset/c-1.same") {
		t.Errorf("the retry printed no link:\n%s", out.String())
	}
}

// A REFUSAL ABOUT A SEAT NAMES THE COMMAND THAT CLEARS IT.
//
// The node's sentences name routes, which an operator at a terminal does not
// hold. Three refusals each have a remedy this command can spell: a seat or an
// address an OPEN INVITATION holds, which `cancel-invite` withdraws; a PERSON
// being unbound (`seat_required` about somebody who exists), whom `bind`
// moves and `remove` frees the seat from — `unbind` being a service
// account's; and an invitation or a create that named no seat
// (`seat_required` about nobody yet), which `seats -unheld` finds one for.
// Keyed on the answer, never the subcommand. Mutation: drop any remedy and its
// row fails; key the person's on the subcommand and the create's row names
// `bind`.
func TestARefusalAboutASeatNamesTheCommandThatClearsIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		status int
		answer map[string]any
		want   []string
		not    []string
	}{
		{"a seat an invitation holds", []string{"bind", "p-1", "founder"},
			http.StatusConflict, map[string]any{"error": "invalid",
				"field": "seat", "invitation": "inv-7", "id": "p-1",
				"detail": "the seat founder is held by an open invitation"},
			[]string{"held by an open invitation",
				"crewlet iam cancel-invite inv-7"}, nil},
		{"a person unbound", []string{"unbind", "p-1"},
			http.StatusBadRequest, map[string]any{"error": "seat_required",
				"field": "seat", "id": "p-1",
				"detail": "a person holds a human seat for as long as they are here"},
			[]string{"seat_required: a person holds a human seat",
				"crewlet iam bind p-1 SEAT", "crewlet iam remove p-1",
				"people always hold a seat"}, []string{"seats -unheld"}},
		{"a newcomer with no seat", []string{"invite", "jane@example.com",
			"-seat", "founder"},
			http.StatusBadRequest, map[string]any{"error": "seat_required",
				"field": "seat", "detail": "name a vacant one"},
			[]string{"crewlet iam seats -unheld"}, []string{"iam bind"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(tc.answer)
			}))
			defer node.Close()
			t.Setenv(apiTokenEnv, "a-tier-a-token")
			cfg := bootstrapWithKeyring(t, "k1")
			var out, errs bytes.Buffer
			err := run(append(append([]string{"iam"}, tc.args...), "-config",
				cfg, "-api", node.URL), &out, &errs)
			if err == nil {
				t.Fatal("a refusal was reported as a success")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(err.Error(), not) {
					t.Errorf("the refusal %q says %q", err, not)
				}
			}
		})
	}
}

// `iam seats` ASKS FOR THE SEATS AND SAYS WHO HOLDS EACH.
//
// It is what an operator about to invite or create somebody reads first, so
// it says which seats are free: a holder by their login (a service account
// said to be one, since it is freed by an unbind rather than a move), an open
// invitation by the id `cancel-invite` takes, and neither as a vacancy.
// `-unheld` asks the node for the vacant ones and is refused on every other
// command, and an empty answer says why it is empty. Mutation: drop the query,
// print the holder's id, or drop the refusal, and a check fails.
func TestIamSeatsSaysWhoHoldsEachSeat(t *testing.T) {
	var queries []string
	answer := `{"seats":[` +
		`{"handle":"founder","name":"Founder","holder":{"person":"p-1",` +
		`"kind":"person","login":"jane.doe","stage":"active"}},` +
		`{"handle":"ops-bot","name":"Ops","unit":"ops","holder":{"person":"s-1",` +
		`"kind":"machine","login":"ci:ops","stage":"active"}},` +
		`{"handle":"cto-human","name":"CTO","invitation":{"id":"inv-9",` +
		`"email":"cto@example.com","expires_at":"2026-06-21T12:00:00Z"}},` +
		`{"handle":"vacant","name":"Vacant"}]}`
	// fullFails makes the unfiltered listing a 503, for the -unheld case
	// whose count could not be read.
	fullFails := false
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("unheld") == "true" {
			_, _ = w.Write([]byte(`{"seats":[]}`))
			return
		}
		if fullFails {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"unavailable","message":"not now"}`))
			return
		}
		_, _ = w.Write([]byte(answer))
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")
	cli := func(args ...string) (string, error) {
		var out, errs bytes.Buffer
		err := run(append(append([]string{"iam"}, args...), "-config", cfg,
			"-api", node.URL), &out, &errs)
		return out.String(), err
	}

	listed, err := cli("seats")
	if err != nil {
		t.Fatalf("iam seats: %v", err)
	}
	if len(queries) != 1 || queries[0] != "GET /iam/seats?" {
		t.Fatalf("iam seats asked %v", queries)
	}
	rows := map[string]string{}
	for _, line := range strings.Split(listed, "\n") {
		if handle, _, ok := strings.Cut(line, " "); ok {
			rows[handle] = line
		}
	}
	for handle, want := range map[string][]string{
		"founder":   {"jane.doe", "active"},
		"ops-bot":   {"ci:ops (service account)", "ops"},
		"cto-human": {"inv-9 (cto@example.com)"},
		"vacant":    {"Vacant"},
	} {
		for _, w := range want {
			if !strings.Contains(rows[handle], w) {
				t.Errorf("the row for %s is %q, want it to say %q", handle,
					rows[handle], w)
			}
		}
	}
	if strings.Contains(rows["founder"], "p-1") {
		t.Errorf("the holder was named by id beside a login: %q", rows["founder"])
	}

	// AN EMPTY -unheld LISTING HAS TWO CAUSES, told apart by asking for
	// every seat once: here the company declares four and each is taken, so
	// the remedy names what FREES a seat — and never `bind`, which moves
	// somebody onto the very vacancy the listing says is not there.
	before := len(queries)
	vacant, err := cli("seats", "-unheld")
	if err != nil {
		t.Fatalf("iam seats -unheld: %v", err)
	}
	if got := strings.Join(queries[before:], " | "); got !=
		"GET /iam/seats?unheld=true | GET /iam/seats?" {
		t.Errorf("an empty iam seats -unheld asked %s", got)
	}
	for _, want := range []string{
		"no vacant human seats: the company's 4 human seats are taken",
		"Free one", "add a `kind: human` seat",
	} {
		if !strings.Contains(vacant, want) {
			t.Errorf("an empty -unheld listing printed %q, want it to say %q",
				vacant, want)
		}
	}
	if strings.Contains(vacant, "iam bind") || strings.Contains(vacant, "has no human seats") {
		t.Errorf("an every-seat-taken listing printed %q", vacant)
	}

	// -json PRINTS THE NODE'S ANSWER and asks nothing more.
	before = len(queries)
	if _, err := cli("seats", "-unheld", "-json"); err != nil {
		t.Fatalf("iam seats -unheld -json: %v", err)
	}
	if len(queries)-before != 1 {
		t.Errorf("iam seats -unheld -json asked %v", queries[before:])
	}

	// A COUNT THAT COULD NOT BE READ claims neither cause.
	fullFails = true
	unsure, err := cli("seats", "-unheld")
	if err != nil {
		t.Fatalf("iam seats -unheld with the count refused: %v", err)
	}
	if !strings.Contains(unsure, "couldn't list every seat") ||
		strings.Contains(unsure, "has no human seats") ||
		strings.Contains(unsure, "are taken") {
		t.Errorf("an uncounted empty -unheld listing printed %q", unsure)
	}
	fullFails = false

	// A COMPANY THAT DECLARES NO HUMAN SEAT says so, with -unheld or without,
	// and sends the reader to the builder by the name the dashboard draws it
	// under: it has no Agents item, so a line naming one leads nowhere.
	answer = `{"seats":[]}`
	for _, args := range [][]string{{"seats"}, {"seats", "-unheld"}} {
		none, err := cli(args...)
		if err != nil {
			t.Fatalf("iam %v: %v", args, err)
		}
		if !strings.Contains(none, "the company has no human seats") ||
			strings.Contains(none, "no vacant") {
			t.Errorf("iam %v over no human seat printed %q", args, none)
		}
		if !strings.Contains(none, "Org chart › Edit org") ||
			strings.Contains(none, "Agents ›") {
			t.Errorf("iam %v over no human seat named the builder as %q, "+
				"want Org chart › Edit org", args, none)
		}
	}

	before = len(queries)
	if _, err := cli("people", "-unheld"); err == nil ||
		!strings.Contains(err.Error(), "-unheld only works with seats") {
		t.Errorf("iam people -unheld answered %v, want a refusal", err)
	}
	if len(queries) != before {
		t.Errorf("the refused -unheld was sent: %v", queries[before:])
	}
}

// A HELD SEAT NEVER READS AS VACANT, WHOEVER ASKS.
//
// `GET /iam/seats` is everybody's, and it answers a reader who is neither an
// admin nor the holder only the name the holder goes by and their kind — no
// login, id or stage. The HOLDER column used to read the login or the id
// alone, so for every such reader a held seat printed a dash where its holder
// was, exactly as a vacant seat prints. Each masked shape the node sends is
// held here: a name, a name this node's keyring did not open, a holder
// enrolled with no name, and a service account; the vacant seat beside them is
// the control. Mutation: drop the name from seatHolder's fallbacks and the
// first row reads as vacant.
func TestIamSeatsNamesAMaskedHolderRatherThanReadingItVacant(t *testing.T) {
	answer := `{"seats":[` +
		`{"handle":"a","name":"A","holder":{"name":"Jane Doe","kind":"person"}},` +
		`{"handle":"b","name":"B","holder":{"sealed":true,"kind":"person"}},` +
		`{"handle":"c","name":"C","holder":{"kind":"person"}},` +
		`{"handle":"d","name":"D","holder":{"name":"CI","kind":"machine"}},` +
		`{"handle":"v","name":"V"}]}`
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")
	var out, errs bytes.Buffer
	if err := run([]string{"iam", "seats", "-config", cfg, "-api", node.URL}, &out,
		&errs); err != nil {
		t.Fatalf("iam seats: %v", err)
	}
	rows := map[string]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if handle, _, ok := strings.Cut(line, " "); ok {
			rows[handle] = line
		}
	}
	// A vacant row dashes its unit, holder, stage, invitation and expiry; a
	// held one names its holder, so it dashes one fewer.
	if got := strings.Count(rows["v"], "–"); got != 5 {
		t.Fatalf("the vacant control row is %q, with %d dashes, want 5", rows["v"], got)
	}
	for handle, want := range map[string]string{
		"a": "Jane Doe",
		"b": "(name unavailable)",
		"c": "(held)",
		"d": "CI (service account)",
	} {
		if !strings.Contains(rows[handle], want) || strings.Count(rows[handle], "–") != 4 {
			t.Errorf("the row for held seat %s is %q, want its holder said as %q "+
				"rather than a dash", handle, rows[handle], want)
		}
	}
}

// A LISTING'S "MORE" LINE IS A COMMAND THAT RUNS, AND ASKS FOR THE SAME LISTING.
//
// Every listing ends a page by printing the command for the next one. Two
// things have to hold of it. The cursor flag it names has to exist: neither
// `-after` nor `-before` was defined once, so following the line answered "flag
// provided but not defined". And it has to repeat the filters the page was
// asked with: a line naming the cursor alone paged through the WHOLE listing
// from that row, printing rows the operator had filtered out as though they
// matched. The node here answers a page with a cursor; the printed line is run
// as written, and the node must be asked exactly what it was asked the first
// time with the cursor moved. Mutation: drop either cursor flag and its row
// fails to parse; compose the line from the cursor alone and every row fails
// on its filters.
func TestAListingsMoreLineIsACommandThatRuns(t *testing.T) {
	for _, tc := range []struct {
		args           []string
		answer, cursor string
		next           string
	}{
		{[]string{"people", "-q", "jane", "-stage", "active", "-limit", "5"},
			`{"people":[],"next":"0192f00d-0000-7000-8000-0000000000aa",` +
				`"position":"CREWLET_IAM_LOG@0:9"}`, "after",
			"0192f00d-0000-7000-8000-0000000000aa"},
		{[]string{"audit", "-person", "0192f00d-0000-7000-8000-0000000000bb",
			"-event", "enrol", "-at", "2026-06-01T00:00:00Z", "-since", "12"},
			`{"events":[],"next":"4096"}`, "before", "4096"},
		{[]string{"invitations", "-all", "-limit", "2"},
			`{"invitations":[],"next":"0192f00d-0000-7000-8000-0000000000cc",` +
				`"position":"CREWLET_IAM_LOG@0:9"}`, "after",
			"0192f00d-0000-7000-8000-0000000000cc"},
	} {
		var asked []url.Values
		node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked = append(asked, r.URL.Query())
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tc.answer))
		}))
		t.Setenv(apiTokenEnv, "a-tier-a-token")
		cfg := bootstrapWithKeyring(t, "k1")
		var out, errs bytes.Buffer
		if err := run(append(append([]string{"iam"}, tc.args...), "-config", cfg,
			"-api", node.URL), &out, &errs); err != nil {
			node.Close()
			t.Fatalf("iam %v: %v", tc.args, err)
		}
		var more string
		for _, line := range strings.Split(out.String(), "\n") {
			if rest, ok := strings.CutPrefix(line, "more: crewlet "); ok {
				more = rest
			}
		}
		if more == "" {
			node.Close()
			t.Fatalf("iam %v printed no next page:\n%s", tc.args, out.String())
		}
		args := append(strings.Fields(more), "-config", cfg, "-api", node.URL)
		err := run(args, &out, &errs)
		node.Close()
		if err != nil {
			t.Errorf("the printed line %q does not run: %v", more, err)
			continue
		}
		if len(asked) != 2 {
			t.Fatalf("following %q asked the node %d times", more, len(asked))
		}
		want := url.Values{}
		for name, values := range asked[0] {
			want[name] = values
		}
		want.Set(tc.cursor, tc.next)
		if got := asked[1].Encode(); got != want.Encode() {
			t.Errorf("following %q asked the node for\n  %s\nwant the first "+
				"page's own listing from the cursor:\n  %s", more, got,
				want.Encode())
		}
	}
}

// A NEXT-PAGE LINE SURVIVES BEING PASTED INTO A SHELL. A value with nothing a
// shell reads specially is printed as it is; anything else is single-quoted,
// a quote inside it included. Mutation: print every value bare and the
// spaced and quoted rows fail.
func TestANextPageLineQuotesWhatAShellWouldSplit(t *testing.T) {
	for value, want := range map[string]string{
		"jane.doe":             "jane.doe",
		"2026-06-01T00:00:00Z": "2026-06-01T00:00:00Z",
		"ci:release":           "ci:release",
		"two words":            "'two words'",
		"it's":                 `'it'\''s'`,
		"":                     "''",
	} {
		if got := shellWord(value); got != want {
			t.Errorf("shellWord(%q) = %s, want %s", value, got, want)
		}
	}
}
