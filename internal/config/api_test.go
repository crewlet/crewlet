package config

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
)

// serving is a Tier A that binds a port and satisfies every rule that comes
// with doing so, for a case to break exactly one of them.
//
// THE CONTROL IS ASSERTED BEFORE EVERY TABLE that uses it — see
// [TestEveryPostureAServedApiCannotBeReachedUnderIsRefused]. Without that, a
// case breaking one rule could be passing on a second one this helper left
// unsatisfied, which is how a table ends up asserting that validation fails
// rather than that it fails for the stated reason.
func serving() Bootstrap {
	b := DefaultBootstrap()
	b.API.Port = DefaultAPIPort
	b.API.ExternalURL = "https://crewlet.example.com"
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.Auth.Tokens = []APIToken{{
		ID: "founder", Token: strings.Repeat("k", 32),
		Grants: []iam.Grant{iam.GrantConfigWrite},
	}}
	b.Secrets = Secrets{
		ActiveKeyID: "k1",
		Keys: []SecretKey{{
			ID: "k1", Material: base64.StdEncoding.EncodeToString(make([]byte, 32)),
		}},
	}
	return b
}

// refuses runs Validate and reports the error, failing when it validated.
func refuses(t *testing.T, b Bootstrap, want string) {
	t.Helper()
	err := b.Validate()
	if err == nil {
		t.Fatalf("validated, want a refusal naming %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// --- what a served API must state ---------------------------------------- //

// EVERY RULE THAT COMES WITH SERVING, in one table, because they are one
// decision: the moment this process binds a port it is a thing people
// authenticate to, and each of these is a way for that to be impossible.
//
// They are refused HERE rather than at bind time so `crewlet validate` catches
// them on a laptop, which is the difference between a typo and an outage.
func TestEveryPostureAServedApiCannotBeReachedUnderIsRefused(t *testing.T) {
	t.Parallel()
	control := serving()
	if err := control.Validate(); err != nil {
		t.Fatalf("the complete posture does not validate, so every case below "+
			"may be failing on something else: %v", err)
	}
	for _, tc := range []struct {
		name   string
		break_ func(*Bootstrap)
		want   string
	}{
		{"no address a browser reaches this on", func(b *Bootstrap) {
			b.API.ExternalURL = ""
		}, "api.external_url"},
		{"no ceiling on what the directory may confer", func(b *Bootstrap) {
			b.API.Auth.MaxGrants = nil
		}, "max_grants"},
		{"no credential at all", func(b *Bootstrap) {
			b.API.Auth.Tokens = nil
		}, "at least one token is required"},
		{"no keyring to sign a session with", func(b *Bootstrap) {
			b.Secrets = Secrets{}
		}, "signs every session cookie"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := serving()
			tc.break_(&b)
			refuses(t, b, tc.want)
		})
	}
}

// THE COUNTERFACTUAL, and it is what keeps the four rules above from being
// ceremony every node pays. A worker or a seats-only node binds nothing, so it
// needs no address, no ceiling, no credential and no keyring — and the default
// Tier A, which is what an empty file decodes to, is exactly that node.
func TestANodeThatServesNoApiNeedsNoneOfIt(t *testing.T) {
	t.Parallel()
	b := DefaultBootstrap()
	if b.API.Serving() {
		t.Fatal("the default binds a port, so this case is not the one it names")
	}
	if err := b.Validate(); err != nil {
		t.Errorf("a node serving no API was refused: %v", err)
	}
}

// A DEPLOYMENT WITH NO TIER A TOKEN IS A FAULT ON EVERY BACKEND, and the
// per-backend arm is the point: each one leaves a different way to be locked
// out of your own company, and `none` is the one where the token is the only
// credential that exists at all.
func TestADeploymentWithNoTierATokenIsAFaultOnEveryBackend(t *testing.T) {
	t.Parallel()
	for _, backend := range AuthBackends {
		t.Run(string(backend), func(t *testing.T) {
			t.Parallel()
			b := serving()
			b.API.Auth.Backend = backend
			switch backend {
			case AuthBackendLocal:
				b.API.Auth.Local = &APILocal{TOTP: iam.SecondFactorRequired}
			case AuthBackendOIDC:
				b.API.Auth.OIDC = &APIOIDC{
					Issuer:       "https://acme.example.com",
					ClientID:     "client",
					ClientSecret: "secret",
				}
			}
			// The control: with a token this posture is legal, so the
			// refusal below is about the token and not the backend.
			if err := b.Validate(); err != nil {
				t.Fatalf("backend %s with a token was refused: %v", backend, err)
			}
			b.API.Auth.Tokens = nil
			refuses(t, b, "at least one token is required")
		})
	}
}

// --- the credential itself ----------------------------------------------- //

// THE ENTROPY FLOOR IS CHECKED ON THE RESOLVED VALUE, which is the whole of
// why it is checked here at all.
//
// A rule enforced on the LITERAL only is one a `${VAR}` walks straight past:
// that is exactly how a 16-byte webhook signing key once got in, and Tier A is
// where the same mistake is cheapest to make, because every credential in it
// is written as a reference. Tier A expands before it decodes, so a reference
// to a short value arrives here as a short value.
func TestATokenBelowTheEntropyFloorIsRefused(t *testing.T) {
	t.Parallel()
	t.Run("as a literal", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.Auth.Tokens[0].Token = "short"
		refuses(t, b, "at least")
	})
	t.Run("and the value never reaches the message", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.Auth.Tokens[0].Token = "hunter2"
		err := b.Validate()
		if err == nil {
			t.Fatal("a weak token validated")
		}
		// THE REFUSAL IS PRINTED, LOGGED AND ANSWERED OVER /config, so
		// a message carrying the value hands the credential to every
		// one of those readers — and its LENGTH narrows a guess at
		// exactly the credential this rule calls too short.
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the refusal carries the token value: %v", err)
		}
	})
}

// AND A REFERENCE IS NOT THE WAY AROUND IT, which is the arm that matters:
// every credential in Tier A is written as a `${VAR}`, so a rule enforced on
// the literal alone would be enforced on almost nothing. It is a case of its
// own rather than a subtest because t.Setenv cannot run under t.Parallel.
func TestATokenBehindAReferenceIsCheckedOnWhatItResolvesTo(t *testing.T) {
	t.Setenv("CREWLET_TEST_WEAK_TOKEN", "short")
	_, err := ParseBootstrap([]byte(`
api:
  port: 8000
  external_url: "https://crewlet.example.com"
  auth:
    max_grants: [config:write]
    tokens:
      - id: founder
        token: "${CREWLET_TEST_WEAK_TOKEN}"
        grants: [config:write]
secrets:
  active_key_id: k1
  keys:
    - id: k1
      material: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
`), EnvOnly())
	if err == nil {
		t.Fatal("a ${VAR} resolving to a five-character token was accepted, " +
			"so a reference is the way around the floor")
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Errorf("the refusal does not name the floor: %v", err)
	}
}

// A CREDENTIAL'S BLAST RADIUS IS STATED WHERE IT IS PINNED. Before this, one
// token was the whole operator surface — so the CI pipeline that needed to
// file a work item held the credential that reads every key the company owns.
func TestATokenMustStateWhatItMayDo(t *testing.T) {
	t.Parallel()
	t.Run("no grants at all", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.Auth.Tokens[0].Grants = nil
		refuses(t, b, "stated where it is pinned")
	})
	t.Run("a grant this build does not know", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.Auth.Tokens[0].Grants = []iam.Grant{"secrets:exfiltrate"}
		refuses(t, b, "not a grant this build knows")
	})
	t.Run("a grant the ceiling withholds", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.Auth.MaxGrants = []iam.Grant{iam.GrantStateRead}
		b.API.Auth.Tokens[0].Grants = []iam.Grant{iam.GrantSecretWrite}
		refuses(t, b, "outside `api.auth.max_grants`")
	})
	t.Run("the unattributable name", func(t *testing.T) {
		t.Parallel()
		// [iam.AnonymousActor] is what an audit row records for a write
		// nobody could be identified for. A real credential under that
		// name is indistinguishable from one in the single trail that
		// exists to tell them apart, and a reader filtering on it gets
		// both.
		b := serving()
		b.API.Auth.Tokens[0].ID = iam.AnonymousActor
		refuses(t, b, "reserved")
	})
	t.Run("a colleague level that is not one", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.Auth.Tokens[0].Colleague = "admin"
		refuses(t, b, "colleague")
	})
}

// THE DEFAULT COLLEAGUE LEVEL IS THE CLOSED END. A pipeline is nobody's
// colleague, and an unset field must not make it one — which is the opposite
// of how `allow_anonymous_read`'s unset value behaved.
func TestAnUnsetColleagueLevelIsNone(t *testing.T) {
	t.Parallel()
	if got := (APIToken{}).Level(); got != iam.ColleagueNone {
		t.Errorf("an unstated colleague level = %q, want %q", got, iam.ColleagueNone)
	}
	if got := (APIToken{Colleague: iam.ColleagueWrite}).Level(); got != iam.ColleagueWrite {
		t.Errorf("a stated level was not honoured: %q", got)
	}
}

// --- the retired keys ---------------------------------------------------- //

// A RETIRED KEY IS REFUSED BY NAME, never ignored, and the control is what
// makes that matter: ignoring `allow_anonymous_read: false` runs the OPPOSITE
// posture from the one the file asks for, and ignoring `disabled: true` runs
// the opposite of that.
func TestEveryRetiredAuthKeyIsANamedRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		{
			"allow_anonymous_read",
			"api:\n  auth:\n    allow_anonymous_read: false\n",
			"is retired with no replacement setting",
		},
		{
			"api.auth.disabled",
			"api:\n  auth:\n    disabled: true\n",
			"-dev-principal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseBootstrap([]byte(tc.doc), EnvOnly())
			if err == nil {
				t.Fatalf("`%s` was accepted, so the file asks for one posture "+
					"and the engine runs the other", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say what to do instead: %v", err)
			}
		})
	}
	t.Run("integrations.public_base_url", func(t *testing.T) {
		t.Parallel()
		_, err := ParseCompany([]byte(
			"name: Acme\nintegrations:\n  public_base_url: https://x.example.com\n"))
		if err == nil {
			t.Fatal("the retired Tier B address key was accepted")
		}
		if !strings.Contains(err.Error(), "api.external_url") {
			t.Errorf("the refusal does not name where it went: %v", err)
		}
	})
}

// --- the external URL ---------------------------------------------------- //

func TestTheExternalURLIsRefusedWhenItCannotBeAnAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, url, want string }{
		{"no scheme", "crewlet.example.com", "must start with http://"},
		{"a scheme a browser never sends", "ftp://crewlet.example.com", "must start with http://"},
		{"no host", "https://", "names no host"},
		{"a query", "https://crewlet.example.com?a=b", "query or a fragment"},
		{"a fragment", "https://crewlet.example.com#x", "query or a fragment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := serving()
			b.API.ExternalURL = tc.url
			refuses(t, b, tc.want)
		})
	}
	// A PATH IS ACCEPTED, and that is the deliberate asymmetry: a
	// path-routing proxy in front of this engine needs one, and every
	// consumer concatenates onto the value, so a path survives the
	// concatenation where a query or a fragment cannot.
	b := serving()
	b.API.ExternalURL = "https://ops.example.com/crewlet"
	if err := b.Validate(); err != nil {
		t.Errorf("a path prefix was refused, which breaks a path-routing proxy: %v", err)
	}
}

// --- trusted proxies ----------------------------------------------------- //

// A CIDR LIST, NEVER A BOOL, and every refusal here is a way the bool would
// have been wrong: the question is not whether a proxy exists but whether THIS
// peer is one.
func TestTrustedProxiesRefusesWhatCannotMeanWhatItSays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, block, want string }{
		{"everything", "0.0.0.0/0", "trusts every peer's forwarded header"},
		{"everything, v6", "::/0", "trusts every peer's forwarded header"},
		{"a bare address", "10.0.0.7", "is an address rather than a block"},
		{"not a block at all", "not-a-network", "is not a CIDR block"},
		{"host bits below the prefix", "10.1.2.3/8", "host bits set below its prefix"},
		{"nothing", "", "an empty entry matches nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := serving()
			b.API.TrustedProxies = []string{tc.block}
			refuses(t, b, tc.want)
		})
	}
	// The control: a real block is accepted, so the rule is a check
	// rather than a refusal of everything.
	b := serving()
	b.API.TrustedProxies = []string{"10.0.0.0/8", "2001:db8::/32"}
	if err := b.Validate(); err != nil {
		t.Errorf("a real proxy block was refused: %v", err)
	}
}

// --- which backend, and which block ------------------------------------- //

// AN UNSET BACKEND DERIVES FROM WHICH BLOCK IS PRESENT, and with neither it is
// `none` — never a password backend nobody asked for.
func TestTheBackendDerivesFromWhichBlockIsPresent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		auth APIAuth
		want AuthBackend
	}{
		{"neither block", APIAuth{}, AuthBackendNone},
		{"a local block", APIAuth{Local: &APILocal{}}, AuthBackendLocal},
		{"an oidc block", APIAuth{OIDC: &APIOIDC{}}, AuthBackendOIDC},
		{"a declaration wins", APIAuth{Backend: AuthBackendNone, Local: &APILocal{}}, AuthBackendNone},
	} {
		if got := tc.auth.Resolved(); got != tc.want {
			t.Errorf("%s resolved to %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A BLOCK NO BACKEND READS IS REFUSED rather than ignored, because ignoring it
// is how a deployment runs with sign-in switched off while its file carries a
// fully configured identity provider and everybody believes it is set up.
func TestABlockNoBackendReadsIsRefused(t *testing.T) {
	t.Parallel()
	b := serving()
	b.API.Auth.Backend = AuthBackendNone
	b.API.Auth.OIDC = &APIOIDC{
		Issuer: "https://acme.example.com", ClientID: "c", ClientSecret: "s",
	}
	refuses(t, b, "reads nothing under `oidc`")
}

// AND A BACKEND WITH NO BLOCK IS REFUSED TOO, on the one that has no safe
// default to fall back on: `local` exists to say whether a password alone is
// enough, and a block that defaulted would answer that on the operator's
// behalf.
func TestALocalBackendNeedsItsBlock(t *testing.T) {
	t.Parallel()
	b := serving()
	b.API.Auth.Backend = AuthBackendLocal
	refuses(t, b, "has no safe default")
}

// --- the second factor --------------------------------------------------- //

func TestTheSecondFactorMustBeStatedAndIsJudgedOnTheExternalURL(t *testing.T) {
	t.Parallel()
	local := func(f iam.SecondFactor, insecure bool, url string) Bootstrap {
		b := serving()
		b.API.ExternalURL = url
		b.API.Auth.Backend = AuthBackendLocal
		b.API.Auth.Local = &APILocal{TOTP: f, AcceptInsecure: insecure}
		return b
	}
	t.Run("the zero value is refused", func(t *testing.T) {
		t.Parallel()
		refuses(t, local("", false, "https://crewlet.example.com"), "there is no default")
	})
	t.Run("optional off loopback is refused", func(t *testing.T) {
		t.Parallel()
		refuses(t, local(iam.SecondFactorOptional, false, "https://crewlet.example.com"),
			"signs somebody in over the network")
	})
	t.Run("optional on loopback is fine", func(t *testing.T) {
		t.Parallel()
		for _, url := range []string{"http://localhost:8000", "http://127.0.0.1:8000"} {
			b := local(iam.SecondFactorOptional, false, url)
			if err := b.Validate(); err != nil {
				t.Errorf("%s was refused: %v", url, err)
			}
		}
	})
	t.Run("and off loopback with the acknowledgement", func(t *testing.T) {
		t.Parallel()
		b := local(iam.SecondFactorOptional, true, "https://crewlet.example.com")
		if err := b.Validate(); err != nil {
			t.Errorf("an acknowledged insecure posture was refused: %v", err)
		}
	})
	// THE JUDGEMENT IS ON THE EXTERNAL URL AND NOT THE BIND ADDRESS, and
	// this is the case that separates them: a hardened node binds loopback
	// behind its proxy and is reached over the internet, so a bind check
	// would permit the insecure posture in exactly the deployment that
	// must refuse it.
	t.Run("a loopback bind behind a public address is still refused", func(t *testing.T) {
		t.Parallel()
		b := local(iam.SecondFactorOptional, false, "https://crewlet.example.com")
		b.API.Host = "127.0.0.1"
		refuses(t, b, "signs somebody in over the network")
	})
}

func TestAPasswordFloorBelowTheEngineOwnIsRefusedRatherThanRaised(t *testing.T) {
	t.Parallel()
	b := serving()
	b.API.Auth.Backend = AuthBackendLocal
	b.API.Auth.Local = &APILocal{TOTP: iam.SecondFactorRequired, MinPasswordLength: 8}
	refuses(t, b, "below the engine's own floor")

	b.API.Auth.Local.MinPasswordLength = MaxPasswordLength + 1
	refuses(t, b, "nobody can satisfy")

	b.API.Auth.Local.MinPasswordLength = 0
	if got := b.API.Auth.Local.Passwords(); got != iam.MinPasswordChars {
		t.Errorf("an unset floor = %d, want the engine's own %d", got, iam.MinPasswordChars)
	}
}

// --- the identity provider ----------------------------------------------- //

func TestTheOIDCBlockRefusesWhatWouldLetSomebodyElseIssueIdentities(t *testing.T) {
	t.Parallel()
	oidc := func(mutate func(*APIOIDC)) Bootstrap {
		b := serving()
		b.API.Auth.Backend = AuthBackendOIDC
		o := &APIOIDC{
			Issuer: "https://acme.example.com", ClientID: "c", ClientSecret: "${S}",
		}
		mutate(o)
		b.API.Auth.OIDC = o
		return b
	}
	control := oidc(func(*APIOIDC) {})
	if err := control.Validate(); err != nil {
		t.Fatalf("a complete oidc block was refused: %v", err)
	}
	// AN HTTP ISSUER IS THE WHOLE ATTACK. The discovery document read from
	// there names the key set every id token is verified against, so
	// anyone who can rewrite it in flight can sign in as anybody.
	refuses(t, oidc(func(o *APIOIDC) { o.Issuer = "http://acme.example.com" }),
		"must be https://")
	refuses(t, oidc(func(o *APIOIDC) { o.Issuer = "" }), "issuer")
	refuses(t, oidc(func(o *APIOIDC) { o.ClientID = "" }), "client_id")
	refuses(t, oidc(func(o *APIOIDC) { o.ClientSecret = "" }), "client_secret")
	// A GROUP MAPPING IS AUTHORITY WRITTEN AT THE PROVIDER, so the ceiling
	// clamps it exactly as it clamps a token's.
	refuses(t, oidc(func(o *APIOIDC) {
		o.GroupGrants = map[string][]iam.Grant{"ops": {"not-a-grant"}}
	}), "not a grant this build knows")
	b := oidc(func(o *APIOIDC) {
		o.GroupGrants = map[string][]iam.Grant{"ops": {iam.GrantSecretWrite}}
	})
	b.API.Auth.MaxGrants = []iam.Grant{iam.GrantStateRead, iam.GrantConfigWrite}
	refuses(t, b, "outside `api.auth.max_grants`")
	refuses(t, oidc(func(o *APIOIDC) {
		o.GroupGrants = map[string][]iam.Grant{"ops": nil}
	}), "confers nothing")
}

// `openid` IS ADDED RATHER THAN REFUSED, because a scopes list that forgets it
// is a list nobody meant to break — and an authorization request without it is
// not an OpenID Connect request at all.
func TestTheOpenIDScopeIsAlwaysRequested(t *testing.T) {
	t.Parallel()
	o := &APIOIDC{Scopes: []string{"email", "profile"}}
	got := o.RequestedScopes()
	if len(got) == 0 || got[0] != ScopeOpenID {
		t.Errorf("scopes = %v, want %q first", got, ScopeOpenID)
	}
	o = &APIOIDC{Scopes: []string{ScopeOpenID, "email"}}
	if got := o.RequestedScopes(); len(got) != 2 {
		t.Errorf("scopes = %v: openid was added to a list that already had it", got)
	}
	// UNSET IS NOT `openid` ALONE: it asks for the engine's own set, which
	// is the one carrying offline_access — so it must reach the flow as
	// nothing rather than as a one-element list that drops the refresh
	// token the deactivation probe asks with.
	if got := (&APIOIDC{}).RequestedScopes(); got != nil {
		t.Errorf("an unset list requests %v, want nil so the engine's own "+
			"set applies", got)
	}
}

// AN UNSET SCOPE LIST WARNS ABOUT NOTHING, because it asks for the engine's own
// set and that set carries offline_access. The warning is for a list somebody
// WROTE without it; firing on the default told every deployment that its
// deactivation probe had nothing to exchange when it had.
func TestAnUnsetScopeListIsNotWarnedAboutAsMissingARefreshToken(t *testing.T) {
	t.Parallel()
	b := serving()
	b.API.Auth.Backend = AuthBackendOIDC
	b.API.Auth.OIDC = &APIOIDC{
		Issuer: "https://acme.example.com", ClientID: "c", ClientSecret: "s",
	}
	for _, w := range b.Warnings() {
		if strings.Contains(w.Path, "oidc.scopes") {
			t.Errorf("an unset scope list warned: %s", w.Message)
		}
	}
}

// --- the horizons -------------------------------------------------------- //

func TestTheSessionWindowsRefuseAnOrderNobodyMeant(t *testing.T) {
	t.Parallel()
	// A SENSITIVE WINDOW LONGER THAN THE ORDINARY ONE is refused rather
	// than swapped: the file says the opposite of what its writer meant,
	// and a silent swap leaves the document and the engine disagreeing.
	b := serving()
	b.API.Auth.Session = APISession{StepUpRaw: "15m", StepUpSensitiveRaw: "1h"}
	refuses(t, b, "longer than `step_up`")

	// A ROTATION WINDOW LONGER THAN THE SESSION means no session ever
	// reaches its second window and the rotation buys nothing.
	b = serving()
	b.API.Auth.Session = APISession{AbsoluteRaw: "2h", RotateAfterRaw: "24h"}
	refuses(t, b, "longer than `absolute`")

	// THE ROTATION FLOOR IS ARITHMETIC. Below it an ordinary NTP spread
	// reaches the reuse arm, which ends every session a person holds.
	b = serving()
	b.API.Auth.Session = APISession{RotateAfterRaw: "1m"}
	refuses(t, b, "rotate_after")

	b = serving()
	b.API.Auth.Session = APISession{AbsoluteRaw: "1000h"}
	refuses(t, b, "absolute")

	b = serving()
	b.API.Auth.Session = APISession{AbsoluteRaw: "sometimes"}
	refuses(t, b, "is not a duration")
}

// THE DEFAULTS ARE WHAT AN UNSET BLOCK MEANS, and they are read through the
// accessors rather than restated at each use.
func TestTheSessionDefaultsApplyToAnUnsetBlock(t *testing.T) {
	t.Parallel()
	var s APISession
	if !s.IsZero() {
		t.Error("an unset block does not read as zero, so it survives an export round trip")
	}
	for name, pair := range map[string][2]any{
		"absolute":          {s.Absolute(), DefaultSessionAbsolute},
		"rotate_after":      {s.RotateAfter(), DefaultSessionRotateAfter},
		"step_up":           {s.StepUp(), DefaultSessionStepUp},
		"step_up_sensitive": {s.StepUpSensitive(), DefaultSessionStepUpSensitive},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %v, want the default %v", name, pair[0], pair[1])
		}
	}
}

// A SIGN-IN THAT OUTLIVES THE RECORD OF WHAT THAT PERSON WAS ALLOWED TO DO is
// half an answer: the investigation finds the login and cannot interpret it.
func TestTheAuthenticationTrailMayNotOutliveTheChangeTrail(t *testing.T) {
	t.Parallel()
	b := serving()
	b.API.Auth.Audit = APIAudit{ChangesRaw: "2160h", SessionsRaw: "9600h"}
	refuses(t, b, "longer than `changes`")

	b = serving()
	b.API.Auth.Audit = APIAudit{ChangesRaw: "100h"}
	refuses(t, b, "changes")

	var a APIAudit
	if !a.IsZero() || a.Changes() != DefaultAuditChanges || a.Sessions() != DefaultAuditSessions {
		t.Errorf("an unset audit block = %v/%v, want the defaults", a.Changes(), a.Sessions())
	}
}

// --- the ceiling, and what a fleet can see of it ------------------------- //

func TestTheCeilingRefusesWhatItCannotMean(t *testing.T) {
	t.Parallel()
	b := serving()
	b.API.Auth.MaxGrants = []iam.Grant{"config:everything"}
	refuses(t, b, "not a grant this build knows")

	b = serving()
	b.API.Auth.MaxGrants = []iam.Grant{iam.GrantStateRead, iam.GrantStateRead, iam.GrantConfigWrite}
	refuses(t, b, "duplicate grant")
}

// TWO NODES WITH DIFFERENT CEILINGS PUBLISH DIFFERENT HASHES, which is the
// whole reason the hash exists: the ceiling is applied per node, per request,
// so a fleet mid-rollout legally disagrees and nothing else would say so.
func TestTwoNodesWithDifferentCeilingsPublishDifferentHashes(t *testing.T) {
	t.Parallel()
	wide := APIAuth{MaxGrants: iam.AllGrants}
	narrow := APIAuth{MaxGrants: []iam.Grant{iam.GrantStateRead}}
	if wide.CeilingHash() == narrow.CeilingHash() {
		t.Fatal("two different ceilings hash the same, so a mixed fleet is invisible")
	}

	// AND TWO NODES WITH THE SAME CEILING WRITTEN IN A DIFFERENT ORDER
	// PUBLISH THE SAME ONE. Every fleet whose config is assembled by a
	// template is this case, and a disagreement reported on one is a
	// signal an operator stops believing.
	reversed := make([]iam.Grant, len(iam.AllGrants))
	for i, g := range iam.AllGrants {
		reversed[len(iam.AllGrants)-1-i] = g
	}
	if got := (APIAuth{MaxGrants: reversed}).CeilingHash(); got != wide.CeilingHash() {
		t.Errorf("the same ceiling in another order hashed %q, want %q",
			got, wide.CeilingHash())
	}
	// Duplicates are the same set, for the same reason.
	dup := append(append([]iam.Grant{}, iam.AllGrants...), iam.GrantStateRead)
	if got := (APIAuth{MaxGrants: dup}).CeilingHash(); got != wide.CeilingHash() {
		t.Errorf("a repeated grant changed the hash: %q", got)
	}

	// AN ABSENT CEILING IS NOT A CEILING OF NOTHING. A node serving no API
	// declares none, and hashing the empty set would give it a digest
	// differing from every serving node's — drawing a disagreement across
	// a fleet whose workers simply have no opinion.
	if got := (APIAuth{}).CeilingHash(); got != "" {
		t.Errorf("a node with no ceiling published %q, want nothing", got)
	}
}

// --- the port ------------------------------------------------------------ //

// ONE NUMBER, because there were two: the examples and the quickstart bound
// 8000 while the image's EXPOSE and four reference pages said 8080, so a
// founder who followed one and ran the other published a port nothing was
// listening on.
func TestTheDefaultPortIsTheOneEverythingQuotes(t *testing.T) {
	t.Parallel()
	if DefaultAPIPort != 8000 {
		t.Errorf("DefaultAPIPort = %d; the examples, the quickstart, the image "+
			"and the reference all name 8000", DefaultAPIPort)
	}
	// IT IS NOT APPLIED AS A DEFAULT, because zero is a real posture — a
	// node that serves no HTTP at all — and a default would take it away.
	if DefaultBootstrap().API.Port != 0 {
		t.Error("the default binds a port, so a worker node cannot say it serves nothing")
	}
}

// --- the warnings -------------------------------------------------------- //

// A WARNING IS WHAT IS VALID AND WORTH READING, and every one of these is a
// configuration that works exactly as written with its consequence somewhere
// else: at an identity provider somebody else administers, or in a horizon
// nothing reaches until it has already been crossed.
//
// NONE OF THEM MAY FAIL `crewlet validate`, which is asserted here as well as
// named: a warning that can fail a build is one somebody suppresses.
func TestTheApiWarningsAreAdvisoryAndSayWhereTheConsequenceIs(t *testing.T) {
	t.Parallel()
	named := func(t *testing.T, b Bootstrap, want string) {
		t.Helper()
		if err := b.Validate(); err != nil {
			t.Fatalf("a warned configuration was REFUSED, which is the one "+
				"thing a warning may not do: %v", err)
		}
		for _, w := range b.Warnings() {
			if strings.Contains(w.Path, want) {
				return
			}
		}
		t.Errorf("nothing warned about %s: %v", want, b.Warnings())
	}

	t.Run("an acknowledged insecure posture", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.Auth.Backend = AuthBackendLocal
		b.API.Auth.Local = &APILocal{
			TOTP: iam.SecondFactorOptional, AcceptInsecure: true,
		}
		named(t, b, "accept_insecure")
	})
	t.Run("plain http off loopback", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.ExternalURL = "http://crewlet.example.com"
		named(t, b, "api.external_url")
	})
	t.Run("an oidc backend with no refresh token", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.Auth.Backend = AuthBackendOIDC
		b.API.Auth.OIDC = &APIOIDC{
			Issuer: "https://acme.example.com", ClientID: "c", ClientSecret: "s",
			Scopes: []string{ScopeOpenID, "email"},
		}
		named(t, b, "oidc.scopes")
	})
	t.Run("a group that confers the secret store", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.Auth.Backend = AuthBackendOIDC
		b.API.Auth.OIDC = &APIOIDC{
			Issuer: "https://acme.example.com", ClientID: "c", ClientSecret: "s",
			Scopes:      []string{ScopeOpenID, ScopeOfflineAccess},
			GroupGrants: map[string][]iam.Grant{"ops": {iam.GrantSecretRead}},
		}
		named(t, b, "group_grants")
	})
}

// AND THE COUNTERFACTUAL: a deployment doing none of those warns about none
// of them. Without this the table above would pass on a build that warned
// unconditionally, which is the same as a build that warns about nothing.
func TestASoundApiPostureWarnsAboutNothing(t *testing.T) {
	t.Parallel()
	b := serving()
	b.API.Auth.Backend = AuthBackendOIDC
	b.API.Auth.OIDC = &APIOIDC{
		Issuer: "https://acme.example.com", ClientID: "c", ClientSecret: "s",
		Scopes:      []string{ScopeOpenID, ScopeOfflineAccess},
		GroupGrants: map[string][]iam.Grant{"ops": {iam.GrantStateRead}},
	}
	for _, w := range b.Warnings() {
		if strings.HasPrefix(w.Path, "api.") {
			t.Errorf("a sound API posture warned: %s — %s", w.Path, w.Message)
		}
	}
}

// A NODE THAT SERVES NO API WARNS ABOUT NONE OF IT EITHER, whatever its auth
// block happens to say: every one of these is about a surface it does not
// bind.
func TestANodeServingNoApiWarnsAboutNoneOfIt(t *testing.T) {
	t.Parallel()
	b := DefaultBootstrap()
	b.API.ExternalURL = "http://crewlet.example.com"
	b.API.Auth.Local = &APILocal{TOTP: iam.SecondFactorOptional, AcceptInsecure: true}
	for _, w := range b.Warnings() {
		if strings.HasPrefix(w.Path, "api.") {
			t.Errorf("a node binding no port warned about its API: %s", w.Path)
		}
	}
}
