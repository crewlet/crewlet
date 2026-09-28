package credential_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam/credential"
)

// mintToken is a token as the mint surface assembles one: a fresh id, a
// secret, and the position the mint landed at.
func mintToken(t *testing.T, position uint64) (credential.Token, string) {
	t.Helper()
	secret, err := credential.NewTokenSecret()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := credential.Token{
		ID: uuid.Must(uuid.NewV7()).String(), Position: position, Secret: secret,
	}
	return token, credential.TokenVerifier(token.ID, secret)
}

// A MINTED TOKEN ROUND-TRIPS THROUGH ITS VALUE AND VERIFIES, AND WHAT IS
// STORED IS NOT THE TOKEN.
func TestAMintedTokenParsesVerifiesAndIsNotItsVerifier(t *testing.T) {
	t.Parallel()
	token, verifier := mintToken(t, 1<<40|17)
	value := token.Value()
	if !strings.HasPrefix(value, credential.TokenPrefix) {
		t.Fatalf("a minted token %q carries no prefix for a scanner to "+
			"recognise it by", value)
	}
	parsed, ok := credential.ParseToken(value)
	if !ok || parsed != token {
		t.Fatalf("the value %q parsed as %+v (%v), want %+v", value, parsed,
			ok, token)
	}
	if !credential.VerifyToken(verifier, parsed) {
		t.Error("a minted token did not verify against its own verifier")
	}
	if strings.Contains(verifier, token.Secret) {
		t.Fatal("the verifier carries the secret, so a replicated table holds " +
			"a live credential every operator with a backup also holds")
	}
	// TWO MINTS ARE TWO SECRETS. A mint that repeated would be one
	// credential handed to two machines.
	second, _ := mintToken(t, 1)
	if second.Secret == token.Secret {
		t.Fatal("two mints produced one secret")
	}
}

// THE VERIFIER COVERS THE PREFIX, THE ID AND THE SECRET — AND NOT THE
// POSITION.
//
// The id is inside it so a verifier copied onto another credential's row
// verifies nothing there, and the prefix so another format's value cannot
// collide with one of these. The position is outside it because it is not
// known until the mint that stores the verifier has landed — so a token whose
// position somebody edits still verifies, which costs nothing: the position
// authenticates nothing, it only decides whether an absent row is an answer or
// a wait. PINNING THE CONSTRUCTION IS CORRECT HERE: the verifier is stored
// durably and never re-derivable, so its shape is a contract with every row
// already written.
func TestTheVerifierCoversEverythingButThePosition(t *testing.T) {
	t.Parallel()
	token, verifier := mintToken(t, 42)
	sum := sha256.Sum256([]byte(credential.TokenPrefix + token.ID + "_" + token.Secret))
	if verifier != hex.EncodeToString(sum[:]) {
		t.Errorf("the verifier is %q, want sha256 over the prefix, the id and "+
			"the secret", verifier)
	}
	moved := token
	moved.Position = 43
	if !credential.VerifyToken(verifier, moved) {
		t.Error("a token verified differently at another position, so the " +
			"verifier could never have been formed before the mint landed")
	}
	other := token
	other.ID = uuid.Must(uuid.NewV7()).String()
	if credential.VerifyToken(verifier, other) {
		t.Error("the secret verified under another credential's id")
	}
	tampered := token
	tampered.Secret = strings.Repeat("0", len(token.Secret))
	if credential.VerifyToken(verifier, tampered) {
		t.Error("a token with another secret verified")
	}
}

// ONLY A VALUE OF THIS SHAPE IS A TOKEN, and the parse routes rather than
// refuses: whatever it declines, the guard still counts as a refused bearer.
func TestOnlyAValueOfThisShapeParsesAsAToken(t *testing.T) {
	t.Parallel()
	token, _ := mintToken(t, 7)
	good := token.Value()
	for _, value := range []string{
		"", "hello", "sk-ant-abc", credential.TokenPrefix,
		strings.TrimPrefix(good, credential.TokenPrefix), // no prefix
		good + "_extra", // a fourth part
		strings.Replace(good, token.ID, "not-a-uuid", 1),
		strings.Replace(good, "_7_", "_seven_", 1),
		good[:len(good)-2],                                            // a short secret
		good[:len(good)-2] + "zz",                                     // not hex
		strings.Replace(good, token.ID, strings.ToUpper(token.ID), 1), // not canonical
	} {
		if _, ok := credential.ParseToken(value); ok {
			t.Errorf("%q parsed as a token", value)
		}
	}
	if _, ok := credential.ParseToken(good); !ok {
		t.Errorf("a minted value %q did not parse", good)
	}
}
