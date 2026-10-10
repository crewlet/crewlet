package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// guard builds a guard over a Tier A shaped by the mutator, on the default
// posture (`anonymous: public`) unless the mutator says otherwise.
func guard(t *testing.T, mutate func(*config.APIAuth)) *auth.Guard {
	t.Helper()
	b := config.DefaultBootstrap()
	if mutate != nil {
		mutate(&b.API.Auth)
	}
	return auth.New(&b)
}

func withKeys(keys ...config.APIToken) func(*config.APIAuth) {
	return func(a *config.APIAuth) { a.Tokens = keys }
}

// member and admin are one accepted key of each role.
func member(id, value string) config.APIToken {
	return config.APIToken{ID: id, Role: config.RoleMember, Token: value}
}

func admin(id, value string) config.APIToken {
	return config.APIToken{ID: id, Role: config.RoleAdmin, Token: value}
}

// serve runs one request through the guard's middleware and a route mounted at
// need, and returns the response and the principal the handler saw — the zero
// Principal when the handler never ran.
func serve(t *testing.T, g *auth.Guard, need auth.Reach, method, path, header string) (*http.Response, auth.Principal) {
	t.Helper()
	var seen auth.Principal
	handler := g.Middleware(g.Require(need, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = auth.PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(method, path, nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result(), seen
}

// code reads the `error` a refusal carried.
func code(t *testing.T, res *http.Response) string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("a refusal that is not JSON: %v", err)
	}
	return body["error"]
}

// --- the reach order ----------------------------------------------------- //

// REACH IS ORDERED, and a reach covers exactly itself and what is below it —
// which is the whole of how a surface decides whether to serve.
func TestEachReachCoversItselfAndEveryReachBelowIt(t *testing.T) {
	t.Parallel()
	for i, have := range auth.Reaches {
		if !have.Valid() {
			t.Errorf("%q is listed in Reaches and is not Valid", have)
		}
		for j, need := range auth.Reaches {
			if got, want := have.Covers(need), i >= j; got != want {
				t.Errorf("%q covers %q = %v, want %v", have, need, got, want)
			}
		}
	}
	want := []auth.Reach{auth.ReachOpen, auth.ReachPublic, auth.ReachMember, auth.ReachAdmin}
	if strings.Join(reachStrings(auth.Reaches), ",") != strings.Join(reachStrings(want), ",") {
		t.Errorf("Reaches = %v, want lowest first %v", auth.Reaches, want)
	}
}

// AN UNKNOWN REACH OPENS NOTHING AND IS OPENED BY NOTHING, the zero value
// included: a principal built around the guard, or a surface that forgot to
// declare, must fail closed in both directions.
func TestAnUnknownReachCoversNothingAndIsCoveredByNothing(t *testing.T) {
	t.Parallel()
	for _, unknown := range []auth.Reach{"", "owner", "Admin"} {
		if unknown.Valid() {
			t.Errorf("%q is Valid", unknown)
		}
		for _, known := range auth.Reaches {
			if unknown.Covers(known) {
				t.Errorf("unknown %q covers %q", unknown, known)
			}
			if known.Covers(unknown) {
				t.Errorf("%q covers unknown %q", known, unknown)
			}
		}
	}
}

func reachStrings(rs []auth.Reach) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}

// A ROLE CARRIES ITS REACH, and nothing else does: a role this build does not
// know carries the open reach, never a keyed one.
func TestARoleCarriesItsReachAndAnUnknownOneCarriesNone(t *testing.T) {
	t.Parallel()
	for role, want := range map[config.TokenRole]auth.Reach{
		config.RoleMember: auth.ReachMember,
		config.RoleAdmin:  auth.ReachAdmin,
		"":                auth.ReachOpen,
		"owner":           auth.ReachOpen,
	} {
		if got := auth.ReachOfRole(role); got != want {
			t.Errorf("ReachOfRole(%q) = %q, want %q", role, got, want)
		}
	}
	for posture, want := range map[config.AnonymousAccess]auth.Reach{
		config.AnonymousPublic: auth.ReachPublic,
		config.AnonymousNone:   auth.ReachOpen,
		"":                     auth.ReachOpen,
		"members":              auth.ReachOpen,
	} {
		if got := auth.ReachOfAnonymous(posture); got != want {
			t.Errorf("ReachOfAnonymous(%q) = %q, want %q", posture, got, want)
		}
	}
}

// --- the key comparison -------------------------------------------------- //

func TestAKeyAuthenticatesAsItsIDItsRoleAndItsReach(t *testing.T) {
	t.Parallel()
	g := guard(t, withKeys(admin("founder", "secret-a"), member("ada", "secret-b")))
	for token, want := range map[string]auth.Principal{
		"secret-a": {ID: "founder", Role: config.RoleAdmin, Reach: auth.ReachAdmin},
		"secret-b": {ID: "ada", Role: config.RoleMember, Reach: auth.ReachMember},
	} {
		got, ok := g.Principal(token)
		if !ok || got != want {
			t.Errorf("key %q resolved to %+v/%v, want %+v", token, got, ok, want)
		}
		if !got.Authenticated() {
			t.Errorf("%+v is not Authenticated", got)
		}
	}
	if p, _ := g.Principal("secret-a"); !p.IsAdmin() {
		t.Error("an admin key is not IsAdmin")
	}
	if p, _ := g.Principal("secret-b"); p.IsAdmin() {
		t.Error("a member key is IsAdmin")
	}
}

func TestAWrongOrMissingKeyAuthenticatesAsNobody(t *testing.T) {
	t.Parallel()
	g := guard(t, withKeys(admin("founder", "secret")))
	for _, candidate := range []string{"", "wrong", "secre", "secrett", "SECRET"} {
		if p, ok := g.Principal(candidate); ok || p.Authenticated() {
			t.Errorf("%q authenticated as %+v", candidate, p)
		}
	}
}

func TestTheBearerSchemeIsCaseInsensitiveAndTrimmed(t *testing.T) {
	t.Parallel()
	// Clients spell it every way, and a scheme comparison that was
	// case-sensitive would reject a conforming client for its
	// capitalisation.
	g := guard(t, withKeys(member("ada", "secret")))
	for _, header := range []string{"Bearer secret", "bearer secret", "BEARER secret", "Bearer   secret  "} {
		res, seen := serve(t, g, auth.ReachMember, "GET", "/work", header)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%q: status = %d", header, res.StatusCode)
		}
		if seen.ID != "ada" {
			t.Errorf("%q: principal = %+v", header, seen)
		}
	}
}

func TestANonBearerHeaderIsRefused(t *testing.T) {
	t.Parallel()
	g := guard(t, withKeys(member("ada", "secret")))
	for _, header := range []string{"secret", "Basic c2VjcmV0", "Bearer", "bearertoken"} {
		res, _ := serve(t, g, auth.ReachMember, "GET", "/work", header)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%q: status = %d, want 401", header, res.StatusCode)
		}
	}
}

// --- what a route needs -------------------------------------------------- //

// EVERY ROLE REACHES EXACTLY WHAT ITS REACH COVERS, through the one
// enforcement point every route is mounted with.
func TestAKeyIsServedEveryRouteItsRoleReachesAndNoOther(t *testing.T) {
	t.Parallel()
	g := guard(t, withKeys(admin("founder", "a-key"), member("ada", "m-key")))
	for _, tc := range []struct {
		key  string
		need auth.Reach
		want int
	}{
		{"a-key", auth.ReachOpen, http.StatusOK},
		{"a-key", auth.ReachPublic, http.StatusOK},
		{"a-key", auth.ReachMember, http.StatusOK},
		{"a-key", auth.ReachAdmin, http.StatusOK},
		{"m-key", auth.ReachOpen, http.StatusOK},
		{"m-key", auth.ReachPublic, http.StatusOK},
		{"m-key", auth.ReachMember, http.StatusOK},
		{"m-key", auth.ReachAdmin, http.StatusForbidden},
	} {
		res, _ := serve(t, g, tc.need, "GET", "/somewhere", "Bearer "+tc.key)
		if res.StatusCode != tc.want {
			t.Errorf("%s on a %s route = %d, want %d", tc.key, tc.need, res.StatusCode, tc.want)
		}
	}
}

// 401 AND 403 ARE DIFFERENT ANSWERS, because they send a person to opposite
// places: 401 is "sign in", and 403 is "you are signed in, and this is not
// yours to see" — where signing in again would send them in a circle.
func TestAKeyThatReachesLessIsForbiddenAndNobodyIsUnauthorized(t *testing.T) {
	t.Parallel()
	g := guard(t, withKeys(member("ada", "m-key")))

	res, seen := serve(t, g, auth.ReachAdmin, "GET", "/config", "Bearer m-key")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a member key on an admin route = %d, want 403", res.StatusCode)
	}
	if got := code(t, res); got != "forbidden" {
		t.Errorf("a member key on an admin route answered %q, want forbidden", got)
	}
	if seen.Authenticated() {
		t.Error("the handler ran for a caller below its reach")
	}

	res, seen = serve(t, g, auth.ReachAdmin, "GET", "/config", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key on an admin route = %d, want 401", res.StatusCode)
	}
	if got := code(t, res); got != "invalid_token" {
		t.Errorf("no key on an admin route answered %q, want invalid_token", got)
	}
	if seen != (auth.Principal{}) {
		t.Error("the handler ran for a caller with no key")
	}
}

// THE ANONYMOUS POSTURE IS A CEILING, and the default one reaches the public
// face and nothing a member reads.
func TestTheAnonymousPostureDecidesWhatAKeylessCallerReaches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		posture config.AnonymousAccess
		need    auth.Reach
		want    int
	}{
		{config.AnonymousPublic, auth.ReachOpen, http.StatusOK},
		{config.AnonymousPublic, auth.ReachPublic, http.StatusOK},
		{config.AnonymousPublic, auth.ReachMember, http.StatusUnauthorized},
		{config.AnonymousPublic, auth.ReachAdmin, http.StatusUnauthorized},
		{config.AnonymousNone, auth.ReachOpen, http.StatusOK},
		{config.AnonymousNone, auth.ReachPublic, http.StatusUnauthorized},
		{config.AnonymousNone, auth.ReachMember, http.StatusUnauthorized},
	} {
		g := guard(t, func(a *config.APIAuth) {
			a.Anonymous = tc.posture
			a.Tokens = []config.APIToken{admin("founder", "secret")}
		})
		res, seen := serve(t, g, tc.need, "GET", "/somewhere", "")
		if res.StatusCode != tc.want {
			t.Errorf("anonymous %s on a %s route = %d, want %d",
				tc.posture, tc.need, res.StatusCode, tc.want)
		}
		if res.StatusCode == http.StatusOK && (seen.Authenticated() || seen.Reach != auth.ReachOfAnonymous(tc.posture)) {
			t.Errorf("anonymous %s was served as %+v", tc.posture, seen)
		}
	}
}

// A KEY THE GUARD REJECTED IS ANSWERED FOR THE KEY IT SENT where a route needs
// one — never served as though nothing had been presented — and an open route
// stays open whatever was presented.
func TestARejectedKeyIsRefusedWhereAKeyIsNeededAndNowhereElse(t *testing.T) {
	t.Parallel()
	g := guard(t, withKeys(member("ada", "secret")))
	res, _ := serve(t, g, auth.ReachMember, "GET", "/work", "Bearer wrong")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a rejected key on a member route = %d, want 401", res.StatusCode)
	}
	res, seen := serve(t, g, auth.ReachOpen, "GET", "/health", "Bearer wrong")
	if res.StatusCode != http.StatusOK {
		t.Errorf("a rejected key on an open route = %d: a bad key closed an open route", res.StatusCode)
	}
	if seen.Authenticated() {
		t.Errorf("a rejected key was attributed: %+v", seen)
	}
}

// A ROUTE THAT CANNOT SAY WHO MAY REACH IT IS A WIRING MISTAKE, refused when it
// is mounted rather than served to anybody — or to nobody — at request time.
func TestRequirePanicsOnAReachItDoesNotKnow(t *testing.T) {
	t.Parallel()
	g := guard(t, nil)
	for _, reach := range []auth.Reach{"", "owner"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Require(%q) mounted a route", reach)
				}
			}()
			g.Require(reach, http.NotFoundHandler())
		}()
	}
}

func TestARefusalSaysSoInJSONAndNothingElse(t *testing.T) {
	t.Parallel()
	g := guard(t, withKeys(admin("founder", "secret")))
	res, seen := serve(t, g, auth.ReachMember, "POST", "/work", "Bearer wrong")

	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q", got)
	}
	if seen != (auth.Principal{}) {
		t.Error("the handler ran despite the refusal")
	}
}

// --- who every request is ------------------------------------------------ //

// EVERY REQUEST CARRIES A PRINCIPAL, an anonymous one included, and its reach
// is the posture's — so no handler has to tell "nothing attached" from
// "nobody".
func TestEveryRequestIsAPrincipalAndAKeylessOneIsTheAnonymousPosture(t *testing.T) {
	t.Parallel()
	_, seen := serve(t, guard(t, nil), auth.ReachOpen, "GET", "/health", "")
	if seen.Authenticated() || seen.Role != "" || seen.Reach != auth.ReachPublic {
		t.Errorf("a keyless caller under the default posture is %+v, want anonymous at public", seen)
	}
	closed := guard(t, func(a *config.APIAuth) {
		a.Anonymous = config.AnonymousNone
		a.Tokens = []config.APIToken{admin("founder", "secret")}
	})
	_, seen = serve(t, closed, auth.ReachOpen, "GET", "/health", "")
	if seen.Authenticated() || seen.Reach != auth.ReachOpen {
		t.Errorf("a keyless caller under anonymous: none is %+v, want anonymous at open", seen)
	}
}

// ATTRIBUTION IS NOT AUTHORIZATION: an open route is still told who presented
// a valid key, which is what lets the open `viewer` question answer "you".
func TestAValidKeyIsAttributedEvenOnAnOpenRoute(t *testing.T) {
	t.Parallel()
	g := guard(t, withKeys(member("ada", "secret")))
	if _, seen := serve(t, g, auth.ReachOpen, "GET", "/health", "Bearer secret"); seen.ID != "ada" {
		t.Errorf("principal = %+v on an open route, want ada", seen)
	}
}

// A CONTEXT THE GUARD NEVER SAW CARRIES A PRINCIPAL THAT REACHES NOTHING, so a
// surface reached around the guard is refused rather than opened.
func TestAContextTheGuardNeverSawReachesNothing(t *testing.T) {
	t.Parallel()
	bare := auth.PrincipalFrom(t.Context())
	if bare != (auth.Principal{}) {
		t.Errorf("a bare context carries %+v", bare)
	}
	for _, need := range auth.Reaches {
		if bare.Reach.Covers(need) {
			t.Errorf("the zero principal covers %q", need)
		}
	}
	// And a principal attached by hand — the socket's handshake, a frame's
	// own key — reads back whole.
	want := auth.Principal{ID: "ada", Role: config.RoleMember, Reach: auth.ReachMember}
	if got := auth.PrincipalFrom(auth.WithPrincipal(t.Context(), want)); got != want {
		t.Errorf("WithPrincipal round trip = %+v, want %+v", got, want)
	}
}

// PRESENTED SAYS WHETHER A KEY WAS REJECTED, which is the one case where the
// anonymous answer would hide what the caller tried to do.
func TestPresentedTellsARejectedKeyFromNoKey(t *testing.T) {
	t.Parallel()
	g := guard(t, withKeys(member("ada", "secret")))
	for _, tc := range []struct {
		header   string
		id       string
		rejected bool
	}{
		{"", "", false},
		{"Bearer secret", "ada", false},
		{"Bearer wrong", "", true},
	} {
		r := httptest.NewRequest(http.MethodGet, "/work", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		p, rejected := g.Presented(r)
		if p.ID != tc.id || rejected != tc.rejected {
			t.Errorf("%q: Presented = %+v/%v, want id %q rejected %v",
				tc.header, p, rejected, tc.id, tc.rejected)
		}
	}
}

// --- the postures -------------------------------------------------------- //

func TestNoKeysServesWhatAnonymousOpensAndNothingKeyed(t *testing.T) {
	t.Parallel()
	// A real posture, not an oversight: a public-face deployment has no
	// credential to manage, which is strictly safer than being made to mint
	// one it will never use.
	g := guard(t, nil)
	if res, _ := serve(t, g, auth.ReachPublic, "GET", "/org", ""); res.StatusCode != http.StatusOK {
		t.Errorf("the public face was refused with no keys configured: %d", res.StatusCode)
	}
	for _, need := range []auth.Reach{auth.ReachMember, auth.ReachAdmin} {
		res, _ := serve(t, g, need, "GET", "/somewhere", "Bearer anything")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("a %s route = %d, want 401: no key can match", need, res.StatusCode)
		}
	}
}

func TestNoBootstrapAtAllReachesOnlyWhatIsOpenToAnyone(t *testing.T) {
	t.Parallel()
	// Tier A supplies the POSTURE, never the existence of a check — and with
	// no Tier A nobody opened even the public face.
	g := auth.New(nil)
	if res, _ := serve(t, g, auth.ReachOpen, "GET", "/health", ""); res.StatusCode != http.StatusOK {
		t.Errorf("an open route = %d with no Tier A, want it served", res.StatusCode)
	}
	for _, need := range []auth.Reach{auth.ReachPublic, auth.ReachMember, auth.ReachAdmin} {
		res, _ := serve(t, g, need, "GET", "/somewhere", "Bearer anything")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("a %s route = %d with no Tier A, want 401", need, res.StatusCode)
		}
	}
}

func TestADisabledGuardServesEverythingAsAnAdminNamedForIt(t *testing.T) {
	t.Parallel()
	g := guard(t, func(a *config.APIAuth) { a.Disabled = true })

	res, seen := serve(t, g, auth.ReachAdmin, "POST", "/config/revisions", "")
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want a disabled guard to serve an admin route", res.StatusCode)
	}
	// The explicit label is what keeps a disabled-mode write
	// distinguishable in an audit row from a real key's.
	want := auth.Principal{ID: auth.AnonymousOperator, Role: config.RoleAdmin, Reach: auth.ReachAdmin}
	if seen != want {
		t.Errorf("principal = %+v, want %+v", seen, want)
	}
	if got, ok := g.Principal(""); !ok || got != want {
		t.Errorf("Principal(\"\") = %+v/%v", got, ok)
	}
}

func TestTheReservedIDIsTheOneTheAPIStamps(t *testing.T) {
	t.Parallel()
	// Config refuses it as a token id, the chart refuses it as a seat
	// binding, and the API stamps it. Two copies would disagree silently,
	// each side staying self-consistent while the reservation stopped
	// covering what is actually written.
	if auth.AnonymousOperator != org.ReservedOperatorID {
		t.Errorf("the API stamps %q but the chart reserves %q",
			auth.AnonymousOperator, org.ReservedOperatorID)
	}
}

// --- the bind posture ---------------------------------------------------- //

func TestLoopbackBindsAreRecognised(t *testing.T) {
	t.Parallel()
	// A laptop, against a deployment, on the startup line.
	for _, host := range []string{"127.0.0.1", "::1", "[::1]", "localhost", "LOCALHOST", " localhost "} {
		if !auth.BindIsLoopback(host) {
			t.Errorf("%q was not recognised as loopback", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "", "10.0.0.1", "example.com", "localhost.evil.com"} {
		if auth.BindIsLoopback(host) {
			t.Errorf("%q was treated as loopback", host)
		}
	}
}

func TestTheGuardReportsItsOwnPosture(t *testing.T) {
	t.Parallel()
	// The startup line has to be able to state what was loaded, which is
	// the difference between an operator knowing their posture and
	// assuming it.
	g := guard(t, withKeys(admin("founder", "a"), member("ada", "m")))
	if g.Tokens() != 2 || g.Anonymous() != config.AnonymousPublic || g.Disabled() {
		t.Errorf("posture = keys %d / anonymous %q / disabled %v",
			g.Tokens(), g.Anonymous(), g.Disabled())
	}
	if g.RoleOf("founder") != config.RoleAdmin || g.RoleOf("ada") != config.RoleMember || g.RoleOf("nobody") != "" {
		t.Errorf("roles = founder %q / ada %q / nobody %q",
			g.RoleOf("founder"), g.RoleOf("ada"), g.RoleOf("nobody"))
	}
}

func TestAnEmptyConfiguredKeyIsNotABypass(t *testing.T) {
	t.Parallel()
	// Config refuses an empty key value, but Bootstrap is an exported struct
	// an embedder can build directly — and a key that resolved to "" because
	// its environment variable was unset would otherwise match a request
	// presenting no credential at all.
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{admin("founder", "")}
	g := auth.New(&b)

	if _, ok := g.Principal(""); ok {
		t.Error("an empty candidate authenticated against an empty key")
	}
	res, seen := serve(t, g, auth.ReachAdmin, "POST", "/config/revisions", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
	if seen.Authenticated() {
		t.Errorf("principal = %+v", seen)
	}
	// And the counterfactual: the empty value is still refused when it is
	// presented explicitly.
	if res, _ := serve(t, g, auth.ReachMember, "POST", "/work", "Bearer "); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an explicit empty bearer = %d, want 401", res.StatusCode)
	}
}

func TestConfigRefusesTheShapesThatWouldLockEveryoneOut(t *testing.T) {
	t.Parallel()
	// Checked in config rather than at API startup, so `crewlet validate`
	// catches them on a laptop rather than a deployment catching them at
	// bind time.
	for _, tc := range []struct {
		name string
		auth config.APIAuth
		want string
	}{
		{
			// Nothing past the sign-in page and no key to sign in with:
			// a process that starts cleanly, binds its port, and shows
			// its own dashboard as a door nobody can open.
			name: "no keys with anonymous none",
			auth: config.APIAuth{Anonymous: config.AnonymousNone},
			want: "no keys are configured",
		},
		{
			name: "the reserved attribution as a key id",
			auth: config.APIAuth{
				Anonymous: config.AnonymousPublic,
				Tokens:    []config.APIToken{admin(org.ReservedOperatorID, "t")},
			},
			want: "reserved",
		},
		{
			// A key that cannot say what it is for: defaulting either way
			// guesses about who may read the company's secrets.
			name: "a key with no role",
			auth: config.APIAuth{
				Anonymous: config.AnonymousPublic,
				Tokens:    []config.APIToken{{ID: "founder", Token: "t"}},
			},
			want: "role",
		},
	} {
		b := config.DefaultBootstrap()
		b.API.Auth = tc.auth
		err := b.Validate()
		if err == nil {
			t.Errorf("%s: validated", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error does not say why: %v", tc.name, err)
		}
	}
}

func TestTheOrdinaryPosturesStillValidate(t *testing.T) {
	t.Parallel()
	// The counterfactual to the refusals above: no keys with the public
	// face open is a real deployment, and so is a closed one with a key.
	for _, tc := range []struct {
		name string
		auth config.APIAuth
	}{
		{"public face, no credential to manage", config.APIAuth{Anonymous: config.AnonymousPublic}},
		{"closed with an admin key", config.APIAuth{
			Anonymous: config.AnonymousNone,
			Tokens:    []config.APIToken{admin("founder", "secret")},
		}},
		{"closed with a member key", config.APIAuth{
			Anonymous: config.AnonymousNone,
			Tokens:    []config.APIToken{member("ada", "secret")},
		}},
	} {
		b := config.DefaultBootstrap()
		b.API.Auth = tc.auth
		if err := b.Validate(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// --- the socket's query key ---------------------------------------------- //

// THE SOCKET PATH TAKES ITS KEY FROM THE QUERY, AND NOTHING ELSE DOES.
//
// A browser cannot set a header on a WebSocket constructor, so the dashboard
// sends its key as ?token= on /ws/stream. Read anywhere else, a key in a URL
// lands in proxy logs and browser history for a route that had an
// alternative.
func TestTheSocketPathTakesItsKeyFromTheQuery(t *testing.T) {
	t.Parallel()
	g := guard(t, func(a *config.APIAuth) {
		a.Anonymous = config.AnonymousNone
		a.Tokens = []config.APIToken{member("ada", "secret")}
	})

	res, seen := serve(t, g, auth.ReachMember, http.MethodGet, auth.SocketPath+"?token=secret", "")
	if res.StatusCode != http.StatusOK || seen.ID != "ada" {
		t.Fatalf("a valid query key on the socket path = %d as %+v, want 200 as ada",
			res.StatusCode, seen)
	}
	res, _ = serve(t, g, auth.ReachMember, http.MethodGet, auth.SocketPath+"?token=wrong", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong query key on the socket path = %d, want 401", res.StatusCode)
	}
	// The header still works there, and wins over a stale query.
	res, seen = serve(t, g, auth.ReachMember, http.MethodGet, auth.SocketPath+"?token=stale", "Bearer secret")
	if res.StatusCode != http.StatusOK || seen.ID != "ada" {
		t.Errorf("a header beside a stale query = %d as %+v, want 200 as ada", res.StatusCode, seen)
	}
	// And a key in the URL of any OTHER route authenticates nobody.
	for _, path := range []string{"/events?token=secret", "/config?token=secret", "/ws/streams?token=secret"} {
		res, seen := serve(t, g, auth.ReachMember, http.MethodGet, path, "")
		if res.StatusCode != http.StatusUnauthorized || seen.Authenticated() {
			t.Errorf("%s = %d as %+v, want 401 as nobody", path, res.StatusCode, seen)
		}
	}
}

// THE LABELS THE GUARD ACCEPTS, in order, and none under a disabled guard —
// which accepts every caller as anonymous and no listed key at all, so a
// listing read off the document would name credentials that authenticate
// nobody.
func TestTokenIDsAreTheLabelsTheGuardAccepts(t *testing.T) {
	t.Parallel()
	g := guard(t, withKeys(admin("ops", "t-ops"), member("ci", "t-ci")))
	if got := g.TokenIDs(); strings.Join(got, ",") != "ci,ops" {
		t.Errorf("TokenIDs = %v, want the labels in order", got)
	}
	off := guard(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{admin("ops", "t-ops")}
		a.Disabled = true
	})
	if got := off.TokenIDs(); len(got) != 0 {
		t.Errorf("a disabled guard lists %v, want none", got)
	}
	if off.RoleOf("ops") != "" {
		t.Error("a disabled guard reports a role for a key it does not accept")
	}
}
