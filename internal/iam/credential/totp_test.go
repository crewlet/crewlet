package credential_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam/credential"
)

// THE RFC 6238 TEST VECTORS.
//
// The reason to write TOTP out rather than depend on it is that it is forty
// lines of arithmetic; the reason that is SAFE is this case. The vectors are
// the specification's own, so an implementation that passes them produces the
// codes every authenticator app produces — which is the one property no
// amount of internal consistency can establish.
//
// The RFC's SHA-1 vectors use the twenty-byte ASCII secret "12345678901234567890".
func TestTheRfcTestVectorsProduceTheRfcCodes(t *testing.T) {
	t.Parallel()
	// base32("12345678901234567890"), unpadded.
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for _, tc := range []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	} {
		step := credential.TOTPStep(time.Unix(tc.unix, 0).UTC())
		got, err := credential.TOTPCode(secret, step)
		if err != nil {
			t.Fatalf("at %d: %v", tc.unix, err)
		}
		if got != tc.want {
			t.Errorf("at unix %d the code is %s, and RFC 6238 says %s — this "+
				"implementation produces codes no authenticator app agrees "+
				"with", tc.unix, got, tc.want)
		}
	}
}

// A CODE CANNOT BE REPLAYED INSIDE ITS OWN WINDOW.
//
// THE CASE THE STORED STEP EXISTS FOR. Without it the drift tolerance is a
// ninety-second window in which one code shoulder-surfed off somebody's screen
// works over and over — and the tolerance is what makes a second factor usable
// at all, so removing it is not the fix.
func TestATotpCodeCannotReplayInsideItsWindow(t *testing.T) {
	t.Parallel()
	secret, err := credential.NewTOTPSecret()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	at := time.Unix(1_700_000_000, 0).UTC()
	step := credential.TOTPStep(at)
	code, err := credential.TOTPCode(secret, step)
	if err != nil {
		t.Fatalf("code: %v", err)
	}

	// First use: accepted, and it names the step to store.
	accepted, ok := credential.VerifyTOTP(secret, code, at, 0)
	if !ok || accepted != step {
		t.Fatalf("the first use answered (%d, %v), want (%d, true)", accepted,
			ok, step)
	}
	// Second use, with that step stored: refused.
	if _, ok := credential.VerifyTOTP(secret, code, at, accepted); ok {
		t.Error("the same code was accepted twice inside one window, so a " +
			"code read off a screen is usable for ninety seconds")
	}
	// And still refused a moment later, inside the drift window.
	if _, ok := credential.VerifyTOTP(secret, code, at.Add(25*time.Second), accepted); ok {
		t.Error("the spent code was accepted again once the clock moved " +
			"inside its own drift window")
	}
}

// ONE STEP OF DRIFT IS ACCEPTED, TWO ARE NOT.
//
// One step is a phone whose clock is a little out and a person who started
// typing just before the code rolled. Two is a ninety-second window, which
// doubles how long an observed code lives for nothing anybody perceives.
func TestOneStepOfDriftIsAcceptedAndTwoAreNot(t *testing.T) {
	t.Parallel()
	secret, err := credential.NewTOTPSecret()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	at := time.Unix(1_700_000_000, 0).UTC()
	now := credential.TOTPStep(at)
	for offset, want := range map[int64]bool{
		-2: false, -1: true, 0: true, 1: true, 2: false,
	} {
		code, err := credential.TOTPCode(secret, now+offset)
		if err != nil {
			t.Fatalf("code at %+d: %v", offset, err)
		}
		if _, ok := credential.VerifyTOTP(secret, code, at, 0); ok != want {
			t.Errorf("a code %+d steps away was accepted %v, want %v",
				offset, ok, want)
		}
	}
}

// A MALFORMED SECRET OR CODE IS A REFUSAL AND NEVER A PANIC.
func TestAMalformedSecretOrCodeRefuses(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_700_000_000, 0).UTC()
	if _, ok := credential.VerifyTOTP("not base32 at all!!", "123456", at, 0); ok {
		t.Error("a secret that is not base32 verified")
	}
	if _, err := credential.TOTPCode("not base32 at all!!", 1); err == nil {
		t.Error("a secret that is not base32 produced a code")
	}
	for _, code := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := credential.VerifyTOTP("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ",
			code, at, 0); ok {
			t.Errorf("%q verified", code)
		}
	}
}

// A SECRET IS ACCEPTED IN THE FORM A PERSON RETYPES IT.
//
// Authenticator apps show the secret in spaced, upper-case groups, and a
// person setting one up by hand types it back that way. Refusing that is a
// lockout produced by formatting.
func TestASecretIsAcceptedAsAPersonWouldTypeItBack(t *testing.T) {
	t.Parallel()
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	step := credential.TOTPStep(time.Unix(59, 0).UTC())
	want, err := credential.TOTPCode(secret, step)
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	for _, typed := range []string{
		strings.ToLower(secret),
		"GEZD GNBV GY3T QOJQ GEZD GNBV GY3T QOJQ",
		"  " + secret + "  ",
	} {
		got, err := credential.TOTPCode(typed, step)
		if err != nil || got != want {
			t.Errorf("%q produced (%q, %v), want %q", typed, got, err, want)
		}
	}
}

// THE URI CARRIES THE ISSUER TWICE, WHICH IS WHAT EVERY APP EXPECTS.
//
// The label is what the app shows in its list; the parameter is what it groups
// on. Writing only one produces an entry that says the account name and
// nothing about which company it is for, on a phone holding twenty of them.
func TestTheOtpauthUriNamesTheIssuerInBothPlaces(t *testing.T) {
	t.Parallel()
	uri := credential.TOTPURI("Crewlet", "sarah.chen", "GEZDGNBVGY3TQOJQ")
	for _, want := range []string{
		"otpauth://totp/Crewlet:sarah.chen?",
		"issuer=Crewlet", "algorithm=SHA1", "digits=6", "period=30",
		"secret=GEZDGNBVGY3TQOJQ",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("the uri %q does not carry %q", uri, want)
		}
	}
}
