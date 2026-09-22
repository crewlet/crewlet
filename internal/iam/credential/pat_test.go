package credential_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam/credential"
)

// A MINTED TOKEN VERIFIES, AND WHAT IS STORED IS NOT THE TOKEN.
func TestAMintedTokenVerifiesAndTheVerifierIsNotIt(t *testing.T) {
	t.Parallel()
	token, verifier, err := credential.NewToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !credential.VerifyToken(verifier, token) {
		t.Error("a minted token did not verify against its own verifier")
	}
	if verifier == token || strings.Contains(verifier, token) {
		t.Fatal("the verifier is the token, so a replicated table holds a " +
			"live credential every operator with a backup also holds")
	}
	if credential.VerifyToken(verifier, token+"0") {
		t.Error("a token with a character appended verified")
	}
	// TWO MINTS ARE TWO TOKENS. A mint that repeated would be one
	// credential handed to two machines.
	second, _, err := credential.NewToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if second == token {
		t.Fatal("two mints produced one token")
	}
}

// THE VERIFIER IS SHA-256 OVER THE WHOLE TOKEN, PREFIX INCLUDED.
//
// Hashing only the random half would make two tokens with different prefixes —
// a future format's, another deployment's — collide on one verifier, which is
// a credential from somewhere else authenticating here. The construction is
// asserted DIRECTLY rather than through a near-miss, because a near-miss
// passes under exactly the mutation this is guarding against: strip a prefix
// that is not there and the whole string is hashed anyway.
//
// PINNING THE CONSTRUCTION IS CORRECT HERE, unlike a password's: the verifier
// is stored durably and is never re-derivable from anything else, so its shape
// is a contract with every row already written rather than an implementation
// detail.
func TestTheVerifierIsSha256OverTheWholeToken(t *testing.T) {
	t.Parallel()
	token, verifier, err := credential.NewToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(token, credential.TokenPrefix) {
		t.Fatalf("a minted token %q carries no prefix for a scanner to "+
			"recognise it by", token)
	}
	whole := sha256.Sum256([]byte(token))
	if verifier != hex.EncodeToString(whole[:]) {
		t.Errorf("the verifier is %q and sha256 of the whole token is %q — a "+
			"verifier over the random half alone collides across prefixes",
			verifier, hex.EncodeToString(whole[:]))
	}
	half := sha256.Sum256([]byte(strings.TrimPrefix(token, credential.TokenPrefix)))
	if verifier == hex.EncodeToString(half[:]) {
		t.Error("the verifier is sha256 of the random half alone")
	}
}

// THE SHAPE CHECK ROUTES AND NEVER REFUSES.
//
// It is for choosing which verifier to check a bearer against without running
// every one of them. Using it as a gate would turn the prefix into a filter an
// attacker strips.
func TestTheShapeCheckOnlyRecognisesAndNeverGates(t *testing.T) {
	t.Parallel()
	token, verifier, err := credential.NewToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !credential.LooksLikeToken(token) {
		t.Error("a minted token is not recognised as one")
	}
	for _, value := range []string{"", "hello", "crw_nothex", "sk-ant-abc"} {
		if credential.LooksLikeToken(value) {
			t.Errorf("%q was recognised as a token", value)
		}
	}
	// AND THE VERIFICATION IS INDEPENDENT of it: a value that fails the
	// shape check is still compared, and one that passes is still
	// verified.
	if credential.VerifyToken(verifier, "crw_deadbeef") {
		t.Error("a token-shaped value verified without matching")
	}
}
