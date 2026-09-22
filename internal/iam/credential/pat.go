package credential

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// MACHINE TOKENS: what a pipeline, a script or a personal access token
// presents, and what is stored for it.
//
// # SHA-256 at rest, and why that is not the password rule being broken
//
// A password is hashed with argon2id because a person chose it: the space it
// came from is small enough that a stolen digest is worth grinding against a
// dictionary, and the memory cost is what makes that grinding expensive. A
// token minted here is 32 bytes of crypto/rand. There is no dictionary, no
// pattern and nothing to grind — an attacker holding the digest has 2^256
// candidates and the only thing argon2id would add is a hundred milliseconds
// per API request, on a credential presented on every request a CI job makes.
//
// So the rule is not "hash secrets with argon2id", it is "spend cost where an
// attacker has a shortcut" — and against a value this engine minted there is
// none.
//
// # The prefix is deliberate and it is not decoration
//
// A token carries a readable prefix so a secret scanner can recognise one in a
// commit, a log or a paste, and so a person who finds one in a file knows what
// they are holding. It costs nothing: the entropy is in the part after it.

const (
	// TokenBytes is the entropy in a minted token: 32.
	TokenBytes = 32

	// TokenPrefix marks a value as this engine's, for a scanner and for a
	// person who finds one. Underscore-terminated, which is the
	// convention every scanner's rules already match on.
	TokenPrefix = "crw_"
)

// NewToken mints a machine token, returning the value to SHOW ONCE and the
// verifier to store.
//
// RETURNED TOGETHER for [NewRecoveryCodes]'s reason: "which of these two
// strings is the secret" must not be a question a caller answers for itself.
func NewToken() (token, verifier string, err error) {
	raw := make([]byte, TokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("credential: mint a token: %w", err)
	}
	token = TokenPrefix + hex.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken is the verifier for a presented token.
//
// IT HASHES THE WHOLE STRING, prefix included. Hashing only the random half
// would make two tokens with different prefixes — a future format, another
// deployment's — collide on one verifier, which is a credential from
// somewhere else authenticating here.
//
// NO NORMALISATION, unlike a recovery code: a token is pasted by a machine,
// never retyped by a person, so trimming and folding would only widen what is
// accepted for no one's benefit.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyToken reports whether a presented token matches a stored verifier.
//
// CONSTANT-TIME, because the verifier is a hex string an attacker can probe a
// character at a time through the endpoint's own latency if it is compared
// with `==`.
func VerifyToken(verifier, token string) bool {
	return subtle.ConstantTimeCompare(
		[]byte(HashToken(token)), []byte(verifier)) == 1
}

// LooksLikeToken reports whether a presented credential is shaped like one of
// this engine's tokens.
//
// WHAT IT IS FOR is choosing which verifier to check a bearer against without
// running every one of them — NOT for refusing anything. A value that fails
// this is still checked, and a value that passes it is still verified; using
// it as a gate would turn the prefix into a filter an attacker strips.
func LooksLikeToken(value string) bool {
	if !strings.HasPrefix(value, TokenPrefix) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, TokenPrefix))
	return err == nil
}
