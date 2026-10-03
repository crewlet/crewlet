package config

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

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
	b.Stream.StoreDir = TestStoreDir
	b.API.Port = DefaultAPIPort
	b.API.ExternalURL = "https://crewlet.example.com"
	// The front end that terminates the https above, named — which is what
	// a sound deployment behind a proxy looks like.
	b.API.TrustedProxies = []string{"10.0.0.0/8"}
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
		remove func(*Bootstrap)
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := serving()
			tc.remove(&b)
			refuses(t, b, tc.want)
		})
	}
}

// THE COUNTERFACTUAL, and it is what keeps the three rules above from being
// ceremony every node pays. A worker or a seats-only node binds nothing, so it
// needs no address, no ceiling and no credential — and the keyed default Tier
// A is exactly that node.
func TestANodeThatServesNoApiNeedsNoneOfIt(t *testing.T) {
	t.Parallel()
	b := RunnableBootstrap()
	if b.API.Serving() {
		t.Fatal("the default binds a port, so this case is not the one it names")
	}
	if err := b.Validate(); err != nil {
		t.Errorf("a node serving no API was refused: %v", err)
	}
}

// BUT EVERY NODE NEEDS THE KEYRING, whatever its roles and whether or not it
// binds a port.
//
// It used to be one of the rules above, required only once `api.port` was
// set. Every node runs every state log, though — a seats-only satellite
// included, the identity directory's too — and every record on every log is
// signed and verified under the keyring, so the engine refused
// the keyless satellite the moment it started its logs, after this command had
// called its file sound. The satellite is the case, because it is the one the
// old rule let through.
func TestASatelliteServingNoApiIsRefusedWithoutAKeyring(t *testing.T) {
	t.Parallel()
	const satellite = "node:\n  roles: [seats]\napi:\n  port: 0\n"
	_, err := ParseBootstrap([]byte(satellite), EnvOnly())
	if err == nil {
		t.Fatal("a seats-only node with no keyring validated, and the engine " +
			"refuses it the moment it starts its state logs")
	}
	if !errors.Is(err, ErrMissing) {
		t.Errorf("err = %v, want ErrMissing", err)
	}
	for _, says := range []string{"secrets.keys", "every state log", "crewlet secrets keygen"} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}

	// THE CONTROL: the same node holding a keyring is sound, so the refusal
	// above is about the keyring and nothing else in the file.
	if _, err := ParseRunnableBootstrap([]byte(satellite), EnvOnly()); err != nil {
		t.Fatalf("the same satellite with a keyring was refused: %v", err)
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
		{
			// The block was in the reference, so there is no spelling
			// of it that works any more, and "unknown field" would send
			// the operator looking for one.
			"api.auth.oidc",
			"api:\n  auth:\n    backend: oidc\n    oidc:\n" +
				"      issuer: https://idp.example.com\n",
			"`backend: local`",
		},
		{
			// `closed` described the posture every deployment now
			// runs, and `open` has no route left to serve — both are
			// told where the first person comes from instead.
			"api.auth.bootstrap",
			"api:\n  auth:\n    bootstrap: closed\n",
			"crewlet iam invite",
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

// AN UNSET BACKEND DERIVES FROM WHETHER THE BLOCK IS PRESENT, and without it
// it is `none` — never a password backend nobody asked for.
func TestTheBackendDerivesFromWhetherTheBlockIsPresent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		auth APIAuth
		want AuthBackend
	}{
		{"no block", APIAuth{}, AuthBackendNone},
		{"a local block", APIAuth{Local: &APILocal{}}, AuthBackendLocal},
		{"a declaration wins", APIAuth{Backend: AuthBackendNone, Local: &APILocal{}}, AuthBackendNone},
	} {
		if got := tc.auth.Resolved(); got != tc.want {
			t.Errorf("%s resolved to %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A BLOCK NO BACKEND READS IS REFUSED rather than ignored, because ignoring it
// is how a deployment runs with sign-in switched off while its file carries a
// fully configured password policy and everybody believes it is set up.
func TestABlockNoBackendReadsIsRefused(t *testing.T) {
	t.Parallel()
	b := serving()
	b.API.Auth.Backend = AuthBackendNone
	b.API.Auth.Local = &APILocal{TOTP: iam.SecondFactorRequired}
	refuses(t, b, "reads nothing under `local`")
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

// THE STEP-UP WINDOW IS BOUNDED BOTH WAYS, AND AN UNSET ONE IS AN HOUR.
//
// One window sizes every step-up gesture, so both bounds are load-bearing:
// below five minutes an administrator re-proves between one screen and the
// next, and past a day a proof that old is not a step-up at all. Each refusal
// names the setting an operator has to change. The controls sit on and inside
// both bounds, and the unset value reads as the hour the setting's doc
// defends.
//
// Mutation: drop the range check on `step_up` and both refusals validate;
// move either bound and the case beside it flips.
func TestTheStepUpWindowIsBoundedAndDefaultsToAnHour(t *testing.T) {
	t.Parallel()
	control := serving()
	if err := control.Validate(); err != nil {
		t.Fatalf("the control does not validate: %v", err)
	}
	for _, raw := range []string{"4m59s", "24h1m"} {
		b := serving()
		b.API.Auth.Session.StepUpRaw = raw
		refuses(t, b, "api.auth.session.step_up")
	}
	for _, raw := range []string{"5m", "1h", "24h"} {
		b := serving()
		b.API.Auth.Session.StepUpRaw = raw
		if err := b.Validate(); err != nil {
			t.Errorf("step_up %s was refused: %v", raw, err)
			continue
		}
		want, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := b.API.Auth.Session.StepUp(); got != want {
			t.Errorf("step_up %s reads as %s", raw, got)
		}
	}
	if got := (APISession{}).StepUp(); got != time.Hour {
		t.Errorf("an unset step_up reads as %s, want the hour the setting "+
			"defends", got)
	}
}

// --- the warnings -------------------------------------------------------- //

// A WARNING IS WHAT IS VALID AND WORTH READING, and every one of these is a
// configuration that works exactly as written with its consequence somewhere
// else: on the network between a browser and this node, or in a front end the
// engine cannot tell apart from its callers.
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
	t.Run("https behind nothing trusted", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.TrustedProxies = nil
		named(t, b, "api.trusted_proxies")
	})
}

// AND THE COUNTERFACTUAL: a deployment doing none of those warns about none
// of them. Without this the table above would pass on a build that warned
// unconditionally, which is the same as a build that warns about nothing.
func TestASoundApiPostureWarnsAboutNothing(t *testing.T) {
	t.Parallel()
	b := serving()
	b.API.Auth.Backend = AuthBackendLocal
	b.API.Auth.Local = &APILocal{TOTP: iam.SecondFactorRequired}
	if err := b.Validate(); err != nil {
		t.Fatalf("the fixture does not validate: %v", err)
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
	// Plain http off loopback, and https with nothing trusted: each is a
	// warning on a node that serves.
	for _, external := range []string{
		"http://crewlet.example.com", "https://crewlet.example.com",
	} {
		b := DefaultBootstrap()
		b.API.ExternalURL = external
		b.API.Auth.Local = &APILocal{TOTP: iam.SecondFactorOptional, AcceptInsecure: true}
		for _, w := range b.Warnings() {
			if strings.HasPrefix(w.Path, "api.") {
				t.Errorf("a node binding no port, reached at %s, warned about "+
					"its API: %s", external, w.Path)
			}
		}
	}
}
