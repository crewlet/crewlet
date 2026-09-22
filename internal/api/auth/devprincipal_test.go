package auth_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/version"
)

// devBootstrap is a Tier A that would accept the flag: a loopback bind and a
// ceiling for it to be cut to.
func devBootstrap(t *testing.T) config.Bootstrap {
	t.Helper()
	b := config.DefaultBootstrap()
	b.API.Host = "127.0.0.1"
	b.API.Auth.MaxGrants = []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite}
	return b
}

// THE DEVELOPMENT PRINCIPAL IS REFUSED OFF LOOPBACK.
//
// It authenticates every request with no credential at all, so the bind is the
// only thing keeping it to one machine — which is the whole difference from
// `api.auth.disabled`, whose one failure was that it checked nothing.
func TestDevPrincipalIsRefusedOffLoopback(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"0.0.0.0", "", "10.0.0.4", "::", "example.com"} {
		b := devBootstrap(t)
		b.API.Host = host
		dev, err := auth.NewDevPrincipal("alice", &b)
		if err == nil {
			t.Errorf("api.host %q accepted the development principal", host)
			continue
		}
		if dev != nil {
			t.Errorf("api.host %q was refused AND built one: %v", host, dev)
		}
		// THE REFUSAL NAMES THE SETTING. "refused" alone sends somebody
		// looking at their credentials, which is the one thing that is
		// not wrong here.
		if !strings.Contains(err.Error(), "api.host") {
			t.Errorf("the refusal does not name api.host: %v", err)
		}
	}
	// THE CONTROL: a loopback bind is accepted, or every assertion above
	// would pass on a function that refused everything.
	b := devBootstrap(t)
	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		b.API.Host = host
		dev, err := auth.NewDevPrincipal("alice", &b)
		if err != nil {
			t.Errorf("api.host %q was refused: %v", host, err)
		}
		if dev == nil {
			t.Errorf("api.host %q built no principal and no error", host)
		}
	}
}

// AND IT IS REFUSED BY ANYTHING THAT IS NOT A DEVELOPMENT BUILD.
//
// A bind is a setting, and a setting reaches production by being copied — a
// compose file, a Helm value, a forked base image. The build check is what no
// copied setting can carry with it.
//
// ASSERTED THROUGH THE PREDICATE rather than by stamping a version into this
// process, which no test can do: [version.IsDevelopment] is resolved once per
// process from link-time state. So this case pins the two halves that ARE
// reachable — that the predicate is what decides, and that it answers true for
// the build running this test and false for every release shape.
func TestDevPrincipalIsRefusedByAReleasedBinary(t *testing.T) {
	t.Parallel()
	// The test binary is built from source, so the flag is available here
	// — which is also what makes the loopback case above meaningful.
	if !version.IsDevelopment() {
		t.Fatalf("the test binary reports %q, which this build does not read "+
			"as a development build: the loopback case above is then passing "+
			"for the wrong reason", version.String())
	}
	b := devBootstrap(t)
	if _, err := auth.NewDevPrincipal("alice", &b); err != nil {
		t.Fatalf("a development build refused the flag: %v", err)
	}
	// AND THE PREDICATE FAILS CLOSED, which is the half that protects a
	// release: anything it does not positively recognise as a development
	// build is treated as one somebody could be running.
	for _, released := range []string{
		"v1.2.3", "v0.1.0", "v0.0.0-20260101120000-abcdef123456",
		"1.2.3", "", "unknown", "(development)",
	} {
		if version.IsDevelopmentFor(released) {
			t.Errorf("%q is read as a development build, so a binary reporting "+
				"it would authenticate every request with no credential", released)
		}
	}
	for _, dev := range []string{"dev", "(devel)"} {
		if !version.IsDevelopmentFor(dev) {
			t.Errorf("%q is not read as a development build, so the flag is "+
				"refused on a build from source", dev)
		}
	}
}

// AN EMPTY FLAG BUILDS NOTHING, which is what an ordinary run passes, and it
// is not an error: `crewlet run` wires this unconditionally.
func TestAnAbsentDevPrincipalIsNotAnError(t *testing.T) {
	t.Parallel()
	b := devBootstrap(t)
	for _, login := range []string{"", "   "} {
		dev, err := auth.NewDevPrincipal(login, &b)
		if err != nil || dev != nil {
			t.Errorf("%q built %v, %v; want nothing and no error", login, dev, err)
		}
	}
}

// A LOGIN CARRYING A NAMESPACE SEPARATOR IS REFUSED.
//
// internal/iam keeps its three identity namespaces apart by the SHAPE of the
// name — a seat handle is one segment, a person's login joins with dots, a
// machine's with a colon — so a development login carrying either would land
// in somebody else's namespace rather than beside it.
func TestADevPrincipalLoginMayNotCarryASeparator(t *testing.T) {
	t.Parallel()
	b := devBootstrap(t)
	for _, login := range []string{"a.b", "token:ops", "a.b:c"} {
		if _, err := auth.NewDevPrincipal(login, &b); err == nil {
			t.Errorf("%q was accepted as a development login", login)
		}
	}
}

// IT RESOLVES AN UNAUTHENTICATED REQUEST, and only an unauthenticated one.
func TestTheDevPrincipalResolvesARequestThatPresentedNothing(t *testing.T) {
	t.Parallel()
	b := devBootstrap(t)
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "founder", Token: "a-real-credential-long-enough", Grants: []iam.Grant{iam.GrantStateRead}},
	}
	dev, err := auth.NewDevPrincipal("alice", &b)
	if err != nil {
		t.Fatalf("NewDevPrincipal: %v", err)
	}
	g := auth.New(&b).WithDevPrincipal(dev)

	var seen iam.Principal
	var how iam.Resolution
	handler := g.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, how = iam.From(r.Context())
	}))
	// RESET EACH TIME, because a refused request never reaches the handler:
	// left over, the previous run's answer reads as this one's, and the
	// anonymous arm below would pass on whatever the arm before it saw.
	run := func(header string) int {
		seen, how = iam.Principal{}, ""
		req := httptest.NewRequest(http.MethodGet, "/events", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if status := run(""); status != http.StatusOK {
		t.Fatalf("an unauthenticated request answered %d, want it served as "+
			"the development principal", status)
	}
	if how != iam.Resolved {
		t.Fatalf("an unauthenticated request resolved %v, want the development principal", how)
	}
	if seen.Login != "dev:alice" {
		t.Errorf("login = %q, want dev:alice", seen.Login)
	}
	if seen.Kind != iam.KindMachine {
		t.Errorf("kind = %q: it is a credential belonging to this process, "+
			"not a person who enrolled", seen.Kind)
	}
	// IT CARRIES THE CEILING AND NO MORE. `api.auth.disabled` granted
	// everything unconditionally, which made a laptop's convenience and
	// the deployment's authority the same thing.
	for _, want := range b.API.Auth.MaxGrants {
		if !seen.Can(want) {
			t.Errorf("the development principal does not carry %s", want)
		}
	}
	for _, withheld := range []iam.Grant{iam.GrantSecretWrite, iam.GrantFleetOperate} {
		if seen.Can(withheld) {
			t.Errorf("the development principal carries %s, which the ceiling "+
				"withholds", withheld)
		}
	}

	// A REAL CREDENTIAL STILL WINS, so a developer testing what a narrow
	// token can reach is not silently answered as the wide principal.
	run("Bearer a-real-credential-long-enough")
	if seen.Login != auth.TokenLogin("founder") {
		t.Errorf("a presented credential resolved as %q, want the token's own "+
			"principal", seen.Login)
	}

	// AND A CREDENTIAL THAT IS PRESENT AND WRONG IS STILL ANONYMOUS. The
	// person typed something; being told it worked would hide exactly the
	// typo they are about to spend an afternoon on.
	// Read from the STATUS rather than from the handler, which a refusal
	// never reaches — that is what makes a 401 the observable here.
	if status := run("Bearer not-the-configured-one"); status != http.StatusUnauthorized {
		t.Errorf("a wrong credential answered %d, want 401: it was served as "+
			"the development principal, which hides the typo somebody is "+
			"about to spend an afternoon on", status)
	}
}

// AND WITHOUT THE FLAG NOTHING CHANGES, which is the control for all of the
// above: a guard built with no development principal refuses an
// unauthenticated request exactly as it always did.
func TestWithoutTheFlagAnUnauthenticatedRequestIsStillRefused(t *testing.T) {
	t.Parallel()
	b := devBootstrap(t)
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "founder", Token: "a-real-credential-long-enough"},
	}
	g := auth.New(&b).WithDevPrincipal(nil)
	res, _ := serve(t, g, http.MethodGet, "/events", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d without the flag, want 401", res.StatusCode)
	}
}

// THE BUILD CHECK IS ASSERTED AGAINST THE SOURCE, because nothing a running
// test can do reaches it.
//
// [version.IsDevelopment] resolves once per process from link-time state, and
// the binary running this IS a development build — so the check passes
// whatever it is, and deleting it changes nothing any case above can see. That
// is precisely the shape of a guard that rots: it protects the one deployment
// no test is ever run on.
//
// Asked of the source, it is a small claim and an exact one — this function
// consults that predicate — which is what makes deleting the check a failing
// build rather than a silent bypass on every release from then on. It is the
// same idiom resolution_test.go uses for the same reason.
func TestTheDevPrincipalConsultsTheBuildPredicate(t *testing.T) {
	t.Parallel()
	parsed, err := parser.ParseFile(token.NewFileSet(), "devprincipal.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found, consults bool
	ast.Inspect(parsed, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "NewDevPrincipal" {
			return true
		}
		found = true
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			sel, ok := inner.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "IsDevelopment" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			consults = consults || (ok && pkg.Name == "version")
			return true
		})
		return false
	})
	// The control: a walk that did not find the function passes every
	// assertion below and protects nothing.
	if !found {
		t.Fatal("devprincipal.go no longer declares NewDevPrincipal; this case " +
			"is reading the wrong file")
	}
	if !consults {
		t.Error("NewDevPrincipal does not consult version.IsDevelopment: a " +
			"released binary would then authenticate every request with no " +
			"credential wherever somebody copied a loopback bind, which is " +
			"exactly how api.auth.disabled reached production")
	}
}
