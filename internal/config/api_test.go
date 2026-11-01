package config

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
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
	b.API.Auth.Admin = iam.AdminServe
	b.API.Auth.Tokens = []APIToken{{
		ID: "founder", Token: strings.Repeat("k", 32),
		Grants: []iam.Grant{iam.GrantAdmin},
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
		{"no word on whether this node serves admin", func(b *Bootstrap) {
			b.API.Auth.Admin = ""
		}, "api.auth.admin"},
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
// ceremony every node pays. A node that binds nothing needs no address, no
// ceiling and no credential — and the keyed default Tier A is exactly that
// node. NOR DOES ONE THAT BINDS A PORT WITHOUT THE INGRESS ROLE: that port
// carries its probes (and a seats node's tool bridge), and serving the API is a
// port AND the ingress role together, so a worker probed on api.port is asked
// for none of it.
//
// Mutation: read [Bootstrap.ServesAPI] as the port alone and the second case
// is refused for an external URL it never needed.
func TestANodeThatServesNoApiNeedsNoneOfIt(t *testing.T) {
	t.Parallel()
	b := RunnableBootstrap()
	if b.ServesAPI() {
		t.Fatal("the default binds a port, so this case is not the one it names")
	}
	if err := b.Validate(); err != nil {
		t.Errorf("a node serving no API was refused: %v", err)
	}
	probed := RunnableBootstrap()
	probed.Node.Roles = []string{"data", "seats", "workers"}
	probed.API.Port = DefaultAPIPort
	if probed.ServesAPI() {
		t.Fatal("a node without ingress reads as serving the API")
	}
	if err := probed.Validate(); err != nil {
		t.Errorf("a node binding api.port for its probes alone was refused: %v", err)
	}
}

// BUT EVERY NODE NEEDS THE KEYRING, whatever its roles, whatever its broker and
// whether or not it binds a port.
//
// It used to be one of the rules above, required only once `api.port` was
// set. Every data node runs every state log, though — the identity
// directory's too — and every record on every log is signed and verified under
// the keyring; and a seats-only satellite, a LEAF holding no estate, still
// verifies the company document's seal, mints its runs' tokens and signs what
// it asks of a data node's estate under it — so the engine refused the keyless
// satellite at its door, after this command had called its file sound. The
// satellite is the case, because it is the one the old rule let through and
// the one that holds no log of its own.
func TestASatelliteServingNoApiIsRefusedWithoutAKeyring(t *testing.T) {
	t.Parallel()
	const satellite = "node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
		"stream:\n  leaf:\n    urls: [nats-leaf://data-a.internal:7422]\n" +
		"coordination:\n  type: embedded-kv\napi:\n  port: 0\n"
	_, err := ParseBootstrap([]byte(satellite), EnvOnly())
	if err == nil {
		t.Fatal("a seats-only node with no keyring validated, and the engine " +
			"refuses it the moment it starts its state logs")
	}
	if !errors.Is(err, ErrMissing) {
		t.Errorf("err = %v, want ErrMissing", err)
	}
	for _, says := range []string{"secrets.keys", "every node in a fleet", "crewlet secrets keygen"} {
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

// A DEPLOYMENT WITH NO TIER A TOKEN IS A FAULT: a fresh deployment's identity
// estate is empty, so the token is the only credential that exists to invite
// the first person with.
func TestADeploymentWithNoTierATokenIsAFault(t *testing.T) {
	t.Parallel()
	b := serving()
	// The control: with a token this posture is legal, so the refusal
	// below is about the token and nothing else.
	if err := b.Validate(); err != nil {
		t.Fatalf("the fixture with a token was refused: %v", err)
	}
	b.API.Auth.Tokens = nil
	refuses(t, b, "at least one token is required")
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

// A TOKEN SHAPED AS AN ASSISTANT GRANT IS REFUSED, because the guard reads
// that shape as a grant before any Tier A comparison, so the token could never
// authenticate as itself. Mutation: drop the prefix check and it validates.
func TestATokenShapedAsAnAssistantGrantIsRefused(t *testing.T) {
	t.Parallel()
	b := serving()
	value := iam.AssistBearerPrefix + "long-enough-to-pass-the-entropy-floor"
	b.API.Auth.Tokens[0].Token = value
	err := b.Validate()
	if err == nil || !strings.Contains(err.Error(), "api.auth.tokens[0].token") ||
		!strings.Contains(err.Error(), iam.AssistBearerPrefix) {
		t.Fatalf("a token shaped as a grant answered %v, want a refusal naming the field "+
			"and the reserved prefix", err)
	}
	if strings.Contains(err.Error(), value) {
		t.Errorf("the refusal carries the token value: %v", err)
	}
	b.API.Auth.Tokens[0].Token = "long-enough-to-pass-the-entropy-floor-" + iam.AssistBearerPrefix
	if err := b.Validate(); err != nil {
		t.Errorf("a token carrying the prefix past its start was refused: %v", err)
	}
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
    admin: serve
    tokens:
      - id: founder
        token: "${CREWLET_TEST_WEAK_TOKEN}"
        grants: [admin]
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
		refuses(t, b, "List the permissions this token gets, any of "+words.Or(iam.AllGrants))
	})
	// A NAME NOBODY KNOWS IS TOLD EVERY ONE IT COULD BE, as a sentence says
	// them: the refusal printed Go's slice, `[admin worklet doclet …]`,
	// to whoever runs `crewlet validate` and to every /config write.
	// Mutation: print iam.AllGrants with %v and this fails.
	t.Run("a name this build does not know", func(t *testing.T) {
		t.Parallel()
		b := serving()
		b.API.Auth.Tokens[0].Grants = []iam.Grant{"swarm"}
		refuses(t, b, iam.NotAGrant("swarm"))
	})
}

// A SERVING NODE NAMES ITS ADMIN POSTURE, AND ONLY ONE THIS BUILD KNOWS.
//
// Required once the node serves the API, with no default, because either
// default is a decision about whether this node publishes host access; and an
// unknown spelling is refused naming the set rather than read as either. The
// control is each valid posture, which validates; a node serving nothing
// needs none. Mutation: default an empty posture to serve and the first case
// validates.
func TestAServingNodeNamesItsAdminPosture(t *testing.T) {
	t.Parallel()
	for _, posture := range iam.AdminPostures {
		b := serving()
		b.API.Auth.Admin = posture
		if posture == iam.AdminRefuse {
			b.API.Auth.Tokens[0].Grants = iam.Apps
		}
		if err := b.Validate(); err != nil {
			t.Errorf("%q is refused: %v", posture, err)
		}
	}
	unset := serving()
	unset.API.Auth.Admin = ""
	err := unset.Validate()
	if !errors.Is(err, ErrMissing) || !strings.Contains(err.Error(), "api.auth.admin") {
		t.Errorf("an unset posture on a serving node: %v, want ErrMissing naming "+
			"api.auth.admin", err)
	}
	for _, word := range []string{"serve", "refuse"} {
		if err == nil || !strings.Contains(err.Error(), word) {
			t.Errorf("the refusal does not say what %q does: %v", word, err)
		}
	}
	unknown := serving()
	unknown.API.Auth.Admin = "sometimes"
	err = unknown.Validate()
	if !errors.Is(err, ErrUnknownValue) || !strings.Contains(err.Error(), "refuse") {
		t.Errorf("an unknown posture: %v, want ErrUnknownValue naming the set", err)
	}
}

// A TOKEN LISTING ADMIN ON A REFUSING NODE IS REFUSED.
//
// The node would clamp it to its apps on every request, so the file says one
// thing and the node does another — and the break-glass token an operator
// reaches for on the day nobody can sign in would turn out to run nothing.
// The controls: the same token on a node serving admin, and an apps-only
// token on the refusing node, both validate. Mutation: drop the conflict and
// the first case validates.
func TestATokenListingAdminOnARefusingNodeIsRefused(t *testing.T) {
	t.Parallel()
	b := serving()
	b.API.Auth.Admin = iam.AdminRefuse
	b.API.Auth.Tokens[0].Grants = []iam.Grant{iam.GrantWorklet, iam.GrantAdmin}
	err := b.Validate()
	if !errors.Is(err, ErrConflict) ||
		!strings.Contains(err.Error(), "api.auth.tokens[0].grants[1]") ||
		!strings.Contains(err.Error(), "refuse") {
		t.Errorf("admin on a refusing node: %v, want a conflict at the entry "+
			"naming the posture", err)
	}
	b.API.Auth.Tokens[0].Grants = iam.Apps
	if err := b.Validate(); err != nil {
		t.Errorf("an apps-only token on a refusing node is refused: %v", err)
	}
	served := serving()
	if err := served.Validate(); err != nil {
		t.Errorf("an admin token on a serving node is refused: %v", err)
	}
}

// AN UNKNOWN PERMISSION IN TIER A IS REFUSED, NAMING THE SET.
//
// Unlike a replicated row, where a newer peer's name is kept and ignored, a
// Tier A file is something an operator typed — so a name this build does not
// know is a typo that would leave the token holding less than its author
// meant, and every `domain:verb` spelling of the old vocabulary is one. Refused at its own entry,
// naming every permission there is. Mutation: skip the unknown-name check and
// every case validates.
func TestAnUnknownGrantIsRefusedNamingTheSet(t *testing.T) {
	t.Parallel()
	for _, name := range []iam.Grant{"work:read", "Admin", "swarm"} {
		b := serving()
		b.API.Auth.Tokens[0].Grants = []iam.Grant{iam.GrantAdmin, name}
		err := b.Validate()
		if !errors.Is(err, ErrUnknownValue) ||
			!strings.Contains(err.Error(), "api.auth.tokens[0].grants[1]") {
			t.Errorf("%q: %v, want an unknown value at the entry", name, err)
			continue
		}
		for _, g := range iam.AllGrants {
			if !strings.Contains(err.Error(), string(g)) {
				t.Errorf("%q: the refusal does not name %q: %v", name, g, err)
			}
		}
	}
}

// THE SCHEMA SAYS WHAT A TOKEN MAY CARRY, every permission there is.
//
// The field's `desc` is the sentence an operator's editor shows beside
// `grants`, and a struct tag cannot be built from [iam.AllGrants], so it is a
// copy — the one that went on naming three permissions after chat made four.
// Held here in order, as the vocabulary declares them. Mutation: drop `chatlet`
// from the tag and this goes red.
func TestATokensGrantsDescriptionNamesEveryPermission(t *testing.T) {
	t.Parallel()
	field, ok := reflect.TypeFor[APIToken]().FieldByName("Grants")
	if !ok {
		t.Fatal("APIToken has no Grants field")
	}
	names := make([]string, 0, len(iam.AllGrants))
	for _, g := range iam.AllGrants {
		names = append(names, string(g))
	}
	if want := strings.Join(names, ", "); !strings.Contains(field.Tag.Get("desc"), want) {
		t.Errorf("api.auth.tokens[].grants is described %q, which does not name %q",
			field.Tag.Get("desc"), want)
	}
}

// --- the retired keys ---------------------------------------------------- //

// A RETIRED KEY IS REFUSED, never ignored, and the control is what makes that
// matter: ignoring `allow_anonymous_read: false` runs the OPPOSITE posture
// from the one the file asks for, and ignoring `disabled: true` runs the
// opposite of that. Each is refused at its own line, because both tiers
// decode with unknown keys refused, and BY NAME: the refusal says the key was
// removed and what replaces it, never "check the spelling" (see removedKeys).
//
// Mutation: decode Tier A without KnownFields and every row is accepted;
// drop an entry from removedKeys and its row reads as a misspelling.
func TestEveryRetiredKeyIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, doc, says string }{
		{"api.auth.allow_anonymous_read", "api:\n  auth:\n    allow_anonymous_read: false\n",
			"every request now needs a credential"},
		{"api.auth.disabled", "api:\n  auth:\n    disabled: true\n",
			"every request now needs a credential"},
		{"api.auth.oidc", "api:\n  auth:\n    oidc:\n      issuer: https://idp.example.com\n",
			"password and two-step verification"},
		{"api.auth.bootstrap", "api:\n  auth:\n    bootstrap: closed\n",
			"crewlet iam invite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseRunnableBootstrap([]byte(tc.doc), EnvOnly())
			if err == nil {
				t.Fatalf("`%s` was accepted, so the file asks for one posture "+
					"and the engine runs the other", tc.name)
			}
			assertRemoved(t, err, tc.name, tc.says)
		})
	}
	t.Run("contact.crewlet_operator_id", func(t *testing.T) {
		t.Parallel()
		_, err := ParseCompany([]byte("name: Acme\nroles:\n  - handle: ana\n" +
			"    kind: human\n    contact:\n      crewlet_operator_id: founder\n"))
		if err == nil {
			t.Fatal("`contact.crewlet_operator_id` was accepted")
		}
		assertRemoved(t, err, "contact.crewlet_operator_id", "token:<id> -seat")
	})
	// THE SAME WORD IN ANOTHER BLOCK IS AN ORDINARY TYPO, and reads like one.
	t.Run("a disabled key outside api.auth", func(t *testing.T) {
		t.Parallel()
		_, err := ParseRunnableBootstrap([]byte("api:\n  disabled: true\n"), EnvOnly())
		if err == nil || !strings.Contains(err.Error(), "Check the spelling") {
			t.Fatalf("err = %v, want the ordinary unknown-setting refusal", err)
		}
	})
	for _, key := range []string{"public_base_url", "dashboard_base_url"} {
		t.Run("integrations."+key, func(t *testing.T) {
			t.Parallel()
			_, err := ParseCompany([]byte(
				"name: Acme\nintegrations:\n  " + key + ": https://x.example.com\n"))
			if err == nil {
				t.Fatalf("the retired Tier B address key %s was accepted", key)
			}
			if !errors.Is(err, ErrUnknownField) {
				t.Errorf("err = %v, want ErrUnknownField", err)
			}
		})
	}
}

// --- the external URL ---------------------------------------------------- //

func TestTheExternalURLIsRefusedWhenItCannotBeAnAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, url, want string }{
		{"no scheme", "crewlet.example.com", "must start with http://"},
		{"a scheme a browser never sends", "ftp://crewlet.example.com", "must start with http://"},
		{"no host", "https://", "has no host"},
		{"a query", "https://crewlet.example.com?a=b", "query or fragment"},
		{"a fragment", "https://crewlet.example.com#x", "query or fragment"},
		// Hosts a browser rewrites before it sends them, which the origin
		// check could not match against this spelling.
		{"a host outside ASCII", "https://bücher.example", "outside ASCII"},
		{"an IPv4 address in short form", "http://127.1:8080", "four decimal octets"},
		{"an IPv4 address in hex", "http://0x7f.0.0.1:8080", "four decimal octets"},
		{"an octet with a leading zero", "https://192.168.010.1", "four decimal octets"},
		{"an IPv4 address with a trailing dot", "http://127.0.0.1.:8080", "four decimal octets"},
		{"an IPv6 zone", "http://[fe80::1%25eth0]:8080", "IPv6 zone"},
		{"a port out of range", "https://crewlet.example.com:99999", "a port is 1 to 65535"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := serving()
			b.API.ExternalURL = tc.url
			refuses(t, b, tc.want)
		})
	}
	// The controls: the same host in its ASCII form, and spellings the
	// origin check puts in the browser's form rather than refusing — a host
	// in upper case, a default port written out or with a leading zero, an
	// IPv4-mapped address, a dotted quad, a domain's trailing dot and a label
	// that only begins with a digit.
	for _, external := range []string{
		"https://xn--bcher-kva.example", "https://Crewlet.Example.com:443",
		"https://crewlet.example.com:0443", "http://[::ffff:127.0.0.1]:8080", "https://10.0.0.7",
		"https://crewlet.example.com.", "https://crewlet-2.example.com",
	} {
		b := serving()
		b.API.ExternalURL = external
		if err := b.Validate(); err != nil {
			t.Errorf("%s was refused: %v", external, err)
		}
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
		{"everything", "0.0.0.0/0", "anyone could fake their address"},
		{"everything, v6", "::/0", "anyone could fake their address"},
		{"a bare address", "10.0.0.7", "is an address, not a block"},
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

// --- the second factor --------------------------------------------------- //

// AN UNSET SECOND FACTOR IS REQUIRED, the secure value, so a deployment that
// says nothing gets one and `optional` is always somebody's choice — judged on
// where a browser reaches the deployment, never on the bind.
//
// Mutation: read the zero as optional in [APIAuth.SecondFactor] and the first
// case fails; drop the loopback rule and the second does.
func TestTheSecondFactorIsRequiredUnlessOptionalIsStatedAndJudgedOnTheExternalURL(t *testing.T) {
	t.Parallel()
	auth := func(f iam.SecondFactor, insecure bool, url string) Bootstrap {
		b := serving()
		b.API.ExternalURL = url
		b.API.Auth.TOTP = f
		b.API.Auth.AcceptInsecure = insecure
		return b
	}
	t.Run("unset is required", func(t *testing.T) {
		t.Parallel()
		b := auth("", false, "https://crewlet.example.com")
		if err := b.Validate(); err != nil {
			t.Fatalf("an unset second factor was refused: %v", err)
		}
		if got := b.API.Auth.SecondFactor(); got != iam.SecondFactorRequired {
			t.Errorf("an unset second factor reads as %q, want %q", got,
				iam.SecondFactorRequired)
		}
		// AN UNKNOWN VALUE REQUIRES ONE TOO: a denylist would read a newer
		// peer's value as optional and drop the requirement for the length
		// of a rolling upgrade.
		newer := APIAuth{TOTP: "somethingnewer"}
		if got := newer.SecondFactor(); got != iam.SecondFactorRequired {
			t.Errorf("a value this build does not know reads as %q, want %q",
				got, iam.SecondFactorRequired)
		}
	})
	t.Run("a value this build does not know is refused", func(t *testing.T) {
		t.Parallel()
		refuses(t, auth("sometimes", false, "https://crewlet.example.com"), "sometimes")
	})
	t.Run("optional off loopback is refused", func(t *testing.T) {
		t.Parallel()
		refuses(t, auth(iam.SecondFactorOptional, false, "https://crewlet.example.com"),
			"allows password-only sign-in")
	})
	t.Run("optional on loopback is fine", func(t *testing.T) {
		t.Parallel()
		for _, url := range []string{"http://localhost:8000", "http://LocalHost:8000", "http://127.0.0.1:8000"} {
			b := auth(iam.SecondFactorOptional, false, url)
			if err := b.Validate(); err != nil {
				t.Errorf("%s was refused: %v", url, err)
			}
			if got := b.API.Auth.SecondFactor(); got != iam.SecondFactorOptional {
				t.Errorf("a stated optional reads as %q", got)
			}
		}
	})
	t.Run("and off loopback with the acknowledgement", func(t *testing.T) {
		t.Parallel()
		b := auth(iam.SecondFactorOptional, true, "https://crewlet.example.com")
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
		b := auth(iam.SecondFactorOptional, false, "https://crewlet.example.com")
		b.API.Host = "127.0.0.1"
		refuses(t, b, "allows password-only sign-in")
	})
}

func TestAPasswordFloorBelowTheEngineOwnIsRefusedRatherThanRaised(t *testing.T) {
	t.Parallel()
	b := serving()
	b.API.Auth.MinPasswordLength = 8
	refuses(t, b, "below the minimum of")

	b.API.Auth.MinPasswordLength = MaxPasswordLength + 1
	refuses(t, b, "over the maximum of")

	b.API.Auth.MinPasswordLength = 0
	if got := b.API.Auth.Passwords(); got != iam.MinPasswordChars {
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
		b.API.Auth.TOTP = iam.SecondFactorOptional
		b.API.Auth.AcceptInsecure = true
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
	b.API.Auth.TOTP = iam.SecondFactorRequired
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
		b.API.Auth.TOTP = iam.SecondFactorOptional
		b.API.Auth.AcceptInsecure = true
		for _, w := range b.Warnings() {
			if strings.HasPrefix(w.Path, "api.") {
				t.Errorf("a node binding no port, reached at %s, warned about "+
					"its API: %s", external, w.Path)
			}
		}
	}
}

// assertRemoved holds a refusal to the named form a removed key is answered
// with: an unknown field, naming the key, saying it was removed and what
// replaces it, and never sending its reader to check a spelling.
func assertRemoved(t *testing.T, err error, key, says string) {
	t.Helper()
	if !errors.Is(err, ErrUnknownField) {
		t.Errorf("err = %v, want ErrUnknownField", err)
	}
	for _, want := range []string{"`" + key + "` was removed", says} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "Check the spelling") {
		t.Errorf("a removed key was reported as a misspelling: %v", err)
	}
}
