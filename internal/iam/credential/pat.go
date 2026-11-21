package credential

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MACHINE TOKENS: what a person's assistant, a pipeline or a service account
// presents, and what is stored for it.
//
//	cwl_pat_<id>_<position>_<secret>
//
//   - The ID is the credential's own uuid, IN THE CLEAR: one keyed read, no scan.
//   - The POSITION is where the mint landed on the identity log, IN THE CLEAR;
//     it makes an absent row three-valued: a node past it with no row says the
//     token is gone (401), one below it says it has not seen the mint yet (503).
//   - The SECRET is 32 bytes of crypto/rand, hex, the only part that
//     authenticates.

const (
	// TokenBytes is the entropy in a minted token's secret: 32.
	TokenBytes = 32

	// TokenPrefix marks a value as this engine's machine token, so a secret
	// scanner recognises one in a commit, log or paste and a person who finds
	// one knows what it is. Underscore-terminated, as scanner rules match.
	TokenPrefix = "cwl_pat_"

	// DefaultTokenLifetime is how long a machine token lives when its mint
	// names no lifetime: NINETY DAYS, the shortest rotation an ordinary CI
	// schedule absorbs without anybody noticing.
	DefaultTokenLifetime = 90 * 24 * time.Hour

	// MaxTokenLifetime is the longest a machine token may live: A YEAR, and
	// the ceiling is the point, because "forever" is unexpressible. A token
	// is a bearer secret that lives in a pipeline's environment, gets
	// copied into a second pipeline, and outlives whoever minted it — so
	// the only bound anybody can rely on is one the mint refuses to exceed,
	// and a year is the longest a credential that nothing re-proves should
	// be trusted.
	MaxTokenLifetime = 365 * 24 * time.Hour
)

// Token is one presented machine token, taken apart.
type Token struct {
	// ID is the credential's own id.
	ID string

	// Position is where the mint landed on the identity log, packed.
	Position uint64

	// Secret is the random half, which is what is verified.
	Secret string
}

// Value renders the token as it is shown once and presented thereafter.
//
// A METHOD NAMED FOR WHAT IT RETURNS rather than a String, because a String
// would print the secret into every log line a Token value was ever passed to.
func (t Token) Value() string {
	return TokenPrefix + t.ID + "_" + strconv.FormatUint(t.Position, 10) +
		"_" + t.Secret
}

// NewTokenSecret mints a token's random half.
//
// THE SECRET ALONE, not a whole token: the position is where the mint lands,
// which is only known once it has, and the verifier stored by that very mint
// has to be formed before it is published — so the verifier covers everything
// but the position, and the value is assembled from the answer.
func NewTokenSecret() (string, error) {
	raw := make([]byte, TokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("credential: mint a token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// TokenVerifier is what the estate stores for a token: SHA-256 over the prefix,
// the id and the secret.
//
// SHA-256 AND NOT ARGON2ID: a password gets argon2id because a person chose it
// and a stolen digest is worth grinding against a dictionary. A minted token has
// none; its digest faces 2^256 candidates, and argon2id would add a hundred
// milliseconds to every request. Spend cost where an attacker has a shortcut.
//
// THE ID IS INSIDE IT, so a verifier copied onto another row verifies nothing
// there. THE PREFIX IS INSIDE IT so another format never collides. THE POSITION
// IS NOT: unknown until the mint lands, and it authenticates nothing; it only
// decides whether an ABSENT row is an answer or a wait.
func TokenVerifier(id, secret string) string {
	sum := sha256.Sum256([]byte(TokenPrefix + id + "_" + secret))
	return hex.EncodeToString(sum[:])
}

// ParseToken takes a presented value apart, answering false for anything that
// is not one of this engine's machine tokens.
//
// WHAT IT IS FOR is choosing which credential arm checks a bearer, never
// refusing one: a value that parses is still verified, and the guard counts a
// value that does not parse as the refused credential it is.
//
// THE SECRET MAY NOT CARRY THE SEPARATOR, and neither may the id or the
// position, so the three parts are unambiguous: the id is a uuid, the position
// is digits and the secret is hex.
func ParseToken(value string) (Token, bool) {
	rest, ok := strings.CutPrefix(value, TokenPrefix)
	if !ok {
		return Token{}, false
	}
	parts := strings.Split(rest, "_")
	if len(parts) != 3 {
		return Token{}, false
	}
	id, err := uuid.Parse(parts[0])
	if err != nil || id.String() != parts[0] {
		return Token{}, false
	}
	position, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return Token{}, false
	}
	secret := parts[2]
	if len(secret) != 2*TokenBytes {
		return Token{}, false
	}
	if _, err := hex.DecodeString(secret); err != nil {
		return Token{}, false
	}
	return Token{ID: parts[0], Position: position, Secret: secret}, true
}

// VerifyToken reports whether a presented token matches a stored verifier.
//
// CONSTANT-TIME, because the verifier is a hex string an attacker can probe a
// character at a time through the endpoint's own latency if it is compared
// with `==`.
func VerifyToken(verifier string, t Token) bool {
	return subtle.ConstantTimeCompare(
		[]byte(TokenVerifier(t.ID, t.Secret)), []byte(verifier)) == 1
}
