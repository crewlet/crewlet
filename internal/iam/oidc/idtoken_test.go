package oidc_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam/oidc"

	"github.com/golang-jwt/jwt/v5"
)

// THE SUITE MINTS REAL RSA KEYS AND SIGNS REAL TOKENS WITH THEM.
//
// Verifying against a fake that answers "valid" would exercise the plumbing
// and none of the checks — and the checks are the whole of this file. There is
// no shared secret in an OpenID round trip: what authenticates a person is a
// signature over a document, so a suite that did not produce signatures would
// be asserting about nothing.

const (
	testIssuer   = "https://idp.example.com"
	testClientID = "crewlet-test-client"
	testKID      = "k1"
	testNonce    = "the-nonce-from-the-flight-cookie"
	testSubject  = "provider-subject-8f21"
)

var signingKey = func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("oidc: generate a test key: " + err.Error())
	}
	return key
}()

// otherKey is a key the issuer never published, for the arm where a token is
// signed by somebody else entirely.
var otherKey = func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("oidc: generate a second test key: " + err.Error())
	}
	return key
}()

// keySource answers with the published key, and nothing else.
type keySource struct{ pub map[string]any }

func (k keySource) Key(_ context.Context, keyID string) (any, error) {
	key, held := k.pub[keyID]
	if !held {
		return nil, errors.New("unknown key id " + keyID)
	}
	return key, nil
}

func publishedKeys() keySource {
	return keySource{pub: map[string]any{testKID: &signingKey.PublicKey}}
}

func testConfig() oidc.Config {
	return oidc.Config{
		Issuer: testIssuer, ClientID: testClientID,
		ClientSecret: "not-a-real-secret",
		RedirectURI:  "https://crewlet.example.com/auth/oidc/callback",
		GroupsClaim:  "groups",
	}
}

var at = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

// claims is a valid set, for a case to spoil one field of.
func claims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   testIssuer,
		"aud":   testClientID,
		"sub":   testSubject,
		"nonce": testNonce,
		"exp":   at.Add(time.Hour).Unix(),
		"iat":   at.Add(-time.Minute).Unix(),
		"email": "sarah.chen@example.com",
		"name":  "Sarah Chen",
	}
}

// sign produces a token, with the key and the header this engine expects
// unless a case says otherwise.
func sign(t *testing.T, c jwt.MapClaims, method jwt.SigningMethod, kid string, key any) string {
	t.Helper()
	token := jwt.NewWithClaims(method, c)
	if kid != "" {
		token.Header["kid"] = kid
	}
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return raw
}

// A VALID TOKEN IS ACCEPTED, AND WHAT IT ASSERTS IS READ BACK.
func TestAValidIDTokenIsAcceptedAndRead(t *testing.T) {
	t.Parallel()
	c := claims()
	c["email_verified"] = true
	c["groups"] = []string{"engineering", "oncall"}
	c["acr"] = "urn:mace:incommon:iap:silver"
	c["auth_time"] = at.Add(-2 * time.Minute).Unix()

	got, err := testConfig().Verify(t.Context(), publishedKeys(),
		sign(t, c, jwt.SigningMethodRS256, testKID, signingKey), testNonce, at)
	if err != nil {
		t.Fatalf("a valid token was refused: %v", err)
	}
	switch {
	case got.Subject != testSubject:
		t.Errorf("the subject reads %q", got.Subject)
	case got.Email != "sarah.chen@example.com":
		t.Errorf("the address reads %q", got.Email)
	case !got.EmailVerified:
		t.Error("email_verified was not read")
	case len(got.Groups) != 2:
		t.Errorf("the groups read %v", got.Groups)
	case got.ACR != "urn:mace:incommon:iap:silver":
		t.Errorf("the acr reads %q", got.ACR)
	case !got.AuthTime.Equal(at.Add(-2 * time.Minute)):
		t.Errorf("auth_time reads %v", got.AuthTime)
	}
}

// THE GROUPS ARE READ FROM THE CLAIM THE DEPLOYMENT NAMES, AND NO OTHER.
//
// Providers disagree about which claim carries groups, so the operator names
// it in api.auth.oidc.groups_claim. The verifier used to read a fixed
// `groups` claim whatever the setting said: a deployment that named `roles`
// had its mapping confer nothing, and one that named no claim — "this
// deployment maps no groups" — still had its logins' authority read out of a
// claim nobody chose to trust.
func TestTheGroupsAreReadFromTheClaimTheDeploymentNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		claim   string
		payload map[string]any
		want    []string
		refused bool
	}{
		{name: "a renamed claim", claim: "roles",
			payload: map[string]any{"roles": []string{"ops"}, "groups": []string{"decoy"}},
			want:    []string{"ops"}},
		{name: "a namespaced claim", claim: "https://example.com/groups",
			payload: map[string]any{"https://example.com/groups": []string{"ops", "oncall"}},
			want:    []string{"ops", "oncall"}},
		{name: "no claim named reads nothing", claim: "",
			payload: map[string]any{"groups": []string{"ops"}}},
		{name: "an absent claim is no groups", claim: "groups",
			payload: map[string]any{}},
		{name: "a single name is one group", claim: "groups",
			payload: map[string]any{"groups": "ops"}, want: []string{"ops"}},
		{name: "any other shape refuses the sign-in", claim: "groups",
			payload: map[string]any{"groups": map[string]any{"ops": true}},
			refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := claims()
			for k, v := range tc.payload {
				c[k] = v
			}
			config := testConfig()
			config.GroupsClaim = tc.claim
			got, err := config.Verify(t.Context(), publishedKeys(),
				sign(t, c, jwt.SigningMethodRS256, testKID, signingKey), testNonce, at)
			if tc.refused {
				if !errors.Is(err, oidc.ErrRefused) ||
					!strings.Contains(err.Error(), tc.claim) {
					t.Errorf("verified with %v (groups %v), want a refusal "+
						"naming the claim", err, got.Groups)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !slices.Equal(got.Groups, tc.want) {
				t.Errorf("groups read %v, want %v", got.Groups, tc.want)
			}
		})
	}
}

// EACH OF THE FIVE VALIDATIONS, SPOILED IN TURN.
//
// Every one of these tokens is otherwise perfect and signed correctly, so a
// build that dropped the check would accept it and every ordinary sign-in
// would go on working. That is what makes each of them worth a case: the
// failure is silent in exactly the way an absent check always is.
func TestEveryIDTokenValidationRefusesWhatItIsFor(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		token func(*testing.T) string
		nonce string
		why   string
	}{
		"a token signed by a key the issuer never published": {
			token: func(t *testing.T) string {
				return sign(t, claims(), jwt.SigningMethodRS256, "k9", otherKey)
			},
			why: "anybody who can reach the callback signs in as anybody",
		},
		"a token signed with the issuer's own public key as an HMAC secret": {
			token: func(t *testing.T) string {
				// THE CLASSIC CONFUSION ATTACK, and what actually
				// refuses it here is the KEY TYPE rather than the
				// algorithm pin: a key source returning
				// *rsa.PublicKey makes an HS256 token fail on the
				// key alone, because Go's HMAC verifier takes
				// []byte and refuses anything else. The pin is
				// tested for its own job separately, below.
				modulus := signingKey.PublicKey.N.Bytes()
				return sign(t, claims(), jwt.SigningMethodHS256, testKID, modulus)
			},
			why: "a published key becomes a signing secret",
		},
		"a token from another provider": {
			token: func(t *testing.T) string {
				c := claims()
				c["iss"] = "https://idp.attacker.example"
				return sign(t, c, jwt.SigningMethodRS256, testKID, signingKey)
			},
			why: "any provider's token is accepted, including a free tenant " +
				"the attacker registered",
		},
		"a token for another application at the same provider": {
			token: func(t *testing.T) string {
				c := claims()
				c["aud"] = "some-other-application"
				return sign(t, c, jwt.SigningMethodRS256, testKID, signingKey)
			},
			why: "every application at that provider becomes a way in here",
		},
		"a token from another login attempt": {
			token: func(t *testing.T) string {
				c := claims()
				c["nonce"] = "a-nonce-from-somebody-elses-sign-in"
				return sign(t, c, jwt.SigningMethodRS256, testKID, signingKey)
			},
			why: "an id token captured from any other login replays into this one",
		},
		"a token with no nonce at all": {
			token: func(t *testing.T) string {
				c := claims()
				delete(c, "nonce")
				return sign(t, c, jwt.SigningMethodRS256, testKID, signingKey)
			},
			why: "a provider that omits the nonce would make every token replayable",
		},
		"a token with no expiry": {
			token: func(t *testing.T) string {
				c := claims()
				delete(c, "exp")
				return sign(t, c, jwt.SigningMethodRS256, testKID, signingKey)
			},
			why: "a token with no exp is valid for ever, so one captured off " +
				"the wire replays until the provider rotates its key",
		},
		"an expired token": {
			token: func(t *testing.T) string {
				c := claims()
				c["exp"] = at.Add(-time.Minute).Unix()
				return sign(t, c, jwt.SigningMethodRS256, testKID, signingKey)
			},
			why: "expiry is checked against the caller's clock",
		},
		"a token with no kid, which must not reach the key source": {
			token: func(t *testing.T) string {
				return sign(t, claims(), jwt.SigningMethodRS256, "", signingKey)
			},
			why: "an unknown kid makes a cold cache fetch, so a token with " +
				"none would be a way to make this process reach the network",
		},
		"a token naming no subject": {
			token: func(t *testing.T) string {
				c := claims()
				delete(c, "sub")
				return sign(t, c, jwt.SigningMethodRS256, testKID, signingKey)
			},
			why: "there is nothing to bind a person to",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			nonce := tc.nonce
			if nonce == "" {
				nonce = testNonce
			}
			_, err := testConfig().Verify(t.Context(), publishedKeys(),
				tc.token(t), nonce, at)
			if err == nil {
				t.Fatalf("accepted — %s", tc.why)
			}
			if !errors.Is(err, oidc.ErrRefused) {
				t.Errorf("the refusal is %v, which does not answer "+
					"errors.Is(ErrRefused)", err)
			}
		})
	}
}

// A MULTI-AUDIENCE TOKEN MUST NAME ITS AUTHORIZED PARTY.
//
// OpenID Connect requires `azp` when there is more than one audience, and
// requires it to be the client. Without the check, a token minted for two
// applications is accepted by both whichever it was actually meant for.
func TestAMultiAudienceTokenMustNameThisClientAsTheAuthorizedParty(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		azp  string
		want bool
	}{
		"azp naming this client":   {testClientID, true},
		"azp naming the other one": {"some-other-application", false},
		"no azp at all":            {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := claims()
			c["aud"] = []string{testClientID, "some-other-application"}
			if tc.azp != "" {
				c["azp"] = tc.azp
			}
			_, err := testConfig().Verify(t.Context(), publishedKeys(),
				sign(t, c, jwt.SigningMethodRS256, testKID, signingKey), testNonce, at)
			if tc.want && err != nil {
				t.Errorf("a token properly addressed to this client was refused: %v", err)
			}
			if !tc.want && err == nil {
				t.Error("a token minted for two applications was accepted " +
					"without naming this one as its authorized party")
			}
		})
	}
}

// THE AUTHENTICATION CONTEXT IS CHECKED ONLY WHEN THE COMPANY SETS ONE.
//
// Requesting `acr_values` is a request the provider is free to ignore, so the
// check on the way back is what enforces it. A default would be one vendor's
// vocabulary imposed on every other, and a login refused for an `acr` nobody
// configured is a lockout with no field to change.
func TestTheAuthenticationContextIsEnforcedOnlyWhenRequired(t *testing.T) {
	t.Parallel()
	c := claims()
	c["acr"] = "urn:mace:incommon:iap:bronze"
	token := sign(t, c, jwt.SigningMethodRS256, testKID, signingKey)

	if _, err := testConfig().Verify(t.Context(), publishedKeys(), token, testNonce, at); err != nil {
		t.Errorf("a token was refused for its acr with no requirement set: %v", err)
	}
	strict := testConfig()
	strict.RequireACR = "urn:mace:incommon:iap:silver"
	if _, err := strict.Verify(t.Context(), publishedKeys(), token, testNonce, at); err == nil {
		t.Error("a weaker authentication context than the company requires " +
			"was accepted — the provider is free to ignore acr_values, so " +
			"this check is what enforces it")
	}
}

// email_verified ARRIVES AS A BOOL AND AS A STRING.
//
// Not leniency for its own sake: a token that fails to decode is a login
// nobody can complete, and the providers that send it as a string are large
// enough that refusing would be refusing them.
func TestEmailVerifiedDecodesFromBothShapesProvidersSend(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{
		`true`: true, `"true"`: true, `false`: false, `"false"`: false,
	} {
		c := claims()
		switch raw {
		case "true":
			c["email_verified"] = true
		case "false":
			c["email_verified"] = false
		default:
			c["email_verified"] = strings.Trim(raw, `"`)
		}
		got, err := testConfig().Verify(t.Context(), publishedKeys(),
			sign(t, c, jwt.SigningMethodRS256, testKID, signingKey), testNonce, at)
		if err != nil {
			t.Fatalf("email_verified as %s refused the token: %v", raw, err)
		}
		if got.EmailVerified != want {
			t.Errorf("email_verified as %s read %v, want %v", raw,
				got.EmailVerified, want)
		}
	}
}

// THE ALGORITHM PIN'S OWN JOB, TESTED WITHOUT THE KEY TYPE DOING IT.
//
// Against a key source that returns *rsa.PublicKey, an HS256 token fails on
// the key before the algorithm is ever consulted — so the obvious confusion
// case says nothing about the pin. What the pin is actually for is a key
// source that returns BYTES, which is what a reader handling an `oct` key
// would, and what a later reader returning `any` could become. There the
// algorithm is the only thing standing between a published symmetric key and
// a token anybody can mint.
func TestTheAlgorithmPinRefusesASymmetricTokenTheKeyTypeWouldAccept(t *testing.T) {
	t.Parallel()
	secret := []byte("a-key-a-jwks-reader-handed-back-as-bytes")
	bytes := keySource{pub: map[string]any{testKID: secret}}

	// Signed HS256 with exactly the key the source hands back, so the
	// SIGNATURE is valid and only the algorithm is wrong.
	token := sign(t, claims(), jwt.SigningMethodHS256, testKID, secret)
	if _, err := testConfig().Verify(t.Context(), bytes, token, testNonce, at); err == nil {
		t.Fatal("a symmetric token verified against a key the source handed " +
			"back as bytes — the algorithm pin is the only thing between a " +
			"published symmetric key and a token anybody can mint")
	}

	// And the control: the same source, the same key, an algorithm this
	// engine accepts — which must still fail, on the key type, so the
	// case above cannot be passing for the wrong reason.
	rsaToken := sign(t, claims(), jwt.SigningMethodRS256, testKID, signingKey)
	if _, err := testConfig().Verify(t.Context(), bytes, rsaToken, testNonce, at); err == nil {
		t.Error("an RS256 token verified against a []byte key")
	}
}
