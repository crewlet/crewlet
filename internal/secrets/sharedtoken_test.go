package secrets_test

import (
	"crypto/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/secrets"
)

// THE FLOOR IS WHAT THIS ENGINE ITSELF HANDS OUT. Both mints on these two
// surfaces are crypto/rand.Text(), so an operator who pressed Generate is
// above the floor by construction — and if that ever stopped being true, the
// engine would be refusing its own credential.
func TestTheMintClearsItsOwnFloor(t *testing.T) {
	t.Parallel()
	minted := rand.Text()
	if len([]rune(minted)) != secrets.MinSharedTokenChars {
		t.Fatalf("rand.Text() is %d characters and the floor is %d; one of the "+
			"two moved without the other", len([]rune(minted)),
			secrets.MinSharedTokenChars)
	}
	if err := secrets.CheckSharedToken(minted); err != nil {
		t.Fatalf("a freshly minted token was refused: %v", err)
	}
}

// AND THE FLOOR CAN REFUSE, which is the half a length constant alone does
// not give you.
func TestAWeakSharedTokenIsRefused(t *testing.T) {
	t.Parallel()
	for _, token := range []string{
		"",
		"t",
		"letmein",
		"crewlet-datadog-token", // 21: plausible, and still short
		strings.Repeat("a", secrets.MinSharedTokenChars-1),
	} {
		if err := secrets.CheckSharedToken(token); err == nil {
			t.Errorf("%q was accepted as the whole authentication for a route", token)
		}
	}
}

// A token with a space in it never survives the trip. It is sent verbatim in a
// header or a query string, where whitespace is re-encoded or trimmed by
// something in the path, so the value that arrives is not the value that was
// configured and every delivery is refused with nothing naming the reason.
func TestASharedTokenCannotHoldWhitespace(t *testing.T) {
	t.Parallel()
	for _, token := range []string{
		"EXAMPLE TOKEN THAT IS LONG ENOUGH",
		"EXAMPLETOKENLONGENOUGHXXXX\n",
	} {
		err := secrets.CheckSharedToken(token)
		if err == nil {
			t.Errorf("%q was accepted", token)
			continue
		}
		if !strings.Contains(err.Error(), "whitespace") {
			t.Errorf("%q was refused for the wrong reason: %v", token, err)
		}
	}
}

// The refusal names THE FLOOR, because "too short" without the number sends an
// operator to the source to find it, and it never names the length of the
// token it refused. The message is answered to API callers and written to
// logs, and a credential's length is a fact about the credential.
func TestTheRefusalNamesTheFloorAndNotTheTokensLength(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"letmein", "crewlet-datadog-token", strings.Repeat("a", 25)} {
		err := secrets.CheckSharedToken(token)
		if err == nil {
			t.Fatalf("%q was accepted", token)
		}
		if !strings.Contains(err.Error(), "at least 26 characters") {
			t.Errorf("the refusal %q does not name the floor", err)
		}
		length := strconv.Itoa(len([]rune(token)))
		for _, word := range strings.FieldsFunc(err.Error(), func(r rune) bool {
			return r < '0' || r > '9'
		}) {
			if word == length {
				t.Errorf("the refusal %q names %s, the length of the token it refused", err, length)
			}
		}
	}
}
