package credential

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// RFC 6238, WRITTEN OUT, AND WHY IT IS NOT A DEPENDENCY.
//
// The whole of TOTP is an HMAC, a counter and a modulus — about forty lines
// below. A module for it would bring its own release cadence, its own idea of
// how much clock drift to tolerate and its own default for the digit count,
// and every one of those is a security decision this file states in one place
// where a reader can see it. The engine's dependency rule asks a module to
// earn its place against what the standard library already does, and here the
// standard library does all of it.
//
// # SHA-1, six digits, thirty seconds, and none of them is a choice
//
// They are what every authenticator app implements. An implementation that
// reached for SHA-256 because SHA-1 is broken elsewhere would produce codes
// no phone generates — and SHA-1's collision weakness has no bearing on an
// HMAC over a 160-bit secret, where what matters is the secret's entropy. The
// same goes for eight digits and for a sixty-second period: technically
// stronger, and unusable.

const (
	// TOTPDigits is the code length: six.
	TOTPDigits = 6

	// TOTPPeriod is one step: thirty seconds.
	TOTPPeriod = 30 * time.Second

	// TOTPDrift is how many steps either side of the current one are
	// accepted: ONE.
	//
	// The number is a window of ±30 seconds around a step boundary, which
	// covers a phone whose clock is a little out and a person who started
	// typing just before the code rolled. Two steps is a ninety-second
	// window, which doubles how long a code shoulder-surfed off a screen
	// stays usable for nothing anybody notices.
	TOTPDrift = 1

	// TOTPSecretBytes is the shared secret's length: twenty, which is
	// SHA-1's block output and what RFC 4226 specifies as the minimum.
	TOTPSecretBytes = 20
)

// totpEncoding is RFC 4648 base32 WITHOUT padding.
//
// Authenticator apps accept both, but a padded secret shows the `=` in the
// string a person may have to type by hand, and some readers of an otpauth
// URI treat it as the end of the parameter.
var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret mints a shared secret, base32-encoded as an authenticator app
// expects it.
func NewTOTPSecret() (string, error) {
	raw := make([]byte, TOTPSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("credential: read a totp secret: %w", err)
	}
	return totpEncoding.EncodeToString(raw), nil
}

// TOTPStep is the counter value at an instant: the number of whole periods
// since the unix epoch.
//
// EXPORTED because it is the value a caller STORES. The last accepted step is
// what makes a code single-use — see [VerifyTOTP] — and a caller that could
// not name it would have to store the code itself, which is a live credential
// written to a replicated table.
func TOTPStep(at time.Time) int64 {
	return at.Unix() / int64(TOTPPeriod/time.Second)
}

// TOTPCode is the code for a secret at one step.
func TOTPCode(secret string, step int64) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	return codeAt(key, step), nil
}

// decodeSecret reads a shared secret in whatever form a person retyped it.
//
// AUTHENTICATOR APPS SHOW IT IN SPACED, UPPER-CASE GROUPS, and somebody
// setting one up by hand types it back that way. Refusing the spacing or the
// case would be a lockout produced by formatting.
func decodeSecret(secret string) ([]byte, error) {
	key, err := totpEncoding.DecodeString(strings.ToUpper(
		strings.ReplaceAll(strings.TrimSpace(secret), " ", "")))
	if err != nil {
		return nil, fmt.Errorf("credential: the totp secret is not base32: %w", err)
	}
	return key, nil
}

// codeAt is the arithmetic, over a secret that has already been decoded.
//
// SPLIT OUT SO THE DECODE HAPPENS ONCE, which is not only cheaper: it is what
// lets [VerifyTOTP]'s candidate loop have no early exit in it at all. With the
// decode inside, a malformed stored secret returns from the middle of the
// loop, and how far it got is a measurable fact about a value this engine
// stored.
func codeAt(key []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	// RFC 4226's DYNAMIC TRUNCATION: the low nibble of the last byte
	// picks where to read four bytes from, and the top bit is masked off
	// so the result is positive on a signed reader.
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", TOTPDigits, value%pow10(TOTPDigits))
}

// pow10 is 10^n for the digit count, as an integer rather than a float —
// math.Pow returns a float64 and the modulus of one is a rounding bug waiting
// for a digit count nobody expected.
func pow10(n int) uint32 {
	out := uint32(1)
	for range n {
		out *= 10
	}
	return out
}

// VerifyTOTP checks a code and reports which step it was for.
//
// THE RETURNED STEP IS WHAT MAKES A CODE SINGLE-USE. A caller stores the
// highest step it has accepted for this credential and passes it back as
// `last`: a code for that step or any earlier one is REFUSED, so a code
// shoulder-surfed off somebody's screen cannot be replayed inside its own
// thirty-second window. Without it the drift tolerance is a ninety-second
// window in which one observed code works repeatedly.
//
// THE COMPARISON IS CONSTANT-TIME over every candidate step, and it does NOT
// stop at the first match: an early exit makes the time taken depend on which
// step matched, which is the same leak one level up.
func VerifyTOTP(secret, code string, at time.Time, last int64) (int64, bool) {
	code = strings.TrimSpace(code)
	key, err := decodeSecret(secret)
	if err != nil || len(code) != TOTPDigits {
		// BOTH REFUSALS ARE OUTSIDE THE LOOP, which is what keeps the
		// loop itself uniform: neither says anything about a candidate,
		// and one of them is about a value this engine stored rather
		// than one the caller presented.
		return 0, false
	}
	now := TOTPStep(at)
	matched := int64(0)
	found := 0
	for step := now - TOTPDrift; step <= now+TOTPDrift; step++ {
		want := codeAt(key, step)
		hit := subtle.ConstantTimeCompare([]byte(code), []byte(want))
		// REPLAY IS FOLDED INTO THE SAME BRANCHLESS ARITHMETIC as the
		// comparison, so a code that is correct-but-spent takes the
		// same time as one that is simply wrong.
		fresh := 0
		if step > last {
			fresh = 1
		}
		found |= hit & fresh
		matched |= int64(hit&fresh) * step
	}
	return matched, found == 1
}

// TOTPURI is the `otpauth://` string an authenticator app scans.
//
// THE ISSUER APPEARS TWICE, in the label and as a parameter, which is what
// every app expects: the label is what it shows in its list and the parameter
// is what it groups on. Writing only one of them produces an entry that says
// `account` and nothing about which company it is for, on a phone that holds
// twenty of them.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	query := url.Values{
		"secret":    {secret},
		"issuer":    {issuer},
		"algorithm": {"SHA1"},
		"digits":    {fmt.Sprint(TOTPDigits)},
		"period":    {fmt.Sprint(int(TOTPPeriod / time.Second))},
	}
	return "otpauth://totp/" + label + "?" + query.Encode()
}
