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
// # Three parts, and why each is in the clear or not
//
//   - The ID is the credential's own uuid, IN THE CLEAR, so verifying one is a
//     single keyed read of one row rather than a scan of every verifier the
//     company holds — which is what a token presented on every request a
//     pipeline makes has to cost.
//   - The POSITION is where the mint landed on the identity log, packed, IN THE
//     CLEAR, and it is what makes an absent row three-valued: a node that has
//     applied past it and holds no row knows the token is gone (401), and one
//     below it has simply not seen the mint yet and says so (503) — exactly the
//     start position a session's bearer carries, for exactly its reason.
//   - The SECRET is 32 bytes of crypto/rand, hex, and it is the only part that
//     authenticates anything.
//
// # SHA-256 at rest, and why that is not the password rule being broken
//
// A password is hashed with argon2id because a person chose it: the space it
// came from is small enough that a stolen digest is worth grinding against a
// dictionary, and the memory cost is what makes that grinding expensive. A
// token minted here is 32 bytes of crypto/rand. There is no dictionary, no
// pattern and nothing to grind — an attacker holding the digest has 2^256
// candidates and the only thing argon2id would add is a hundred milliseconds
// per request, on a credential presented on every request a pipeline makes.
//
// So the rule is not "hash secrets with argon2id", it is "spend cost where an
// attacker has a shortcut" — and against a value this engine minted there is
// none.
//
// # The prefix is deliberate and it is not decoration
//
// A token carries a readable prefix so a secret scanner can recognise one in a
// commit, a log or a paste, and so a person who finds one in a file knows what
// they are holding. It costs nothing: the entropy is in the secret.

const (
	// TokenBytes is the entropy in a minted token's secret: 32.
	TokenBytes = 32

	// TokenPrefix marks a value as this engine's machine token, for a
	// scanner and for a person who finds one. Underscore-terminated, which
	// is the convention every scanner's rules already match on.
	TokenPrefix = "cwl_pat_"

	// DefaultTokenLifetime and MaxTokenLifetime bound a machine token's life.
	//
	// NINETY DAYS AND A YEAR, and the ceiling is the point: "forever" is
	// unexpressible. A token is a bearer secret that lives in a pipeline's
	// environment, gets copied into a second pipeline, and outlives
	// whoever minted it — so the only bound anybody can rely on is one the
	// mint refuses to exceed. Ninety days is the default because it is the
	// shortest rotation an ordinary CI schedule absorbs without anybody
	// noticing, and a year is the longest a credential that nothing
	// re-proves should be trusted.
	DefaultTokenLifetime = 90 * 24 * time.Hour
	MaxTokenLifetime     = 365 * 24 * time.Hour
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
// THE ID IS INSIDE IT, so a verifier copied onto another credential's row —
// a restore that mixed two artefacts, a hand-edited backup — verifies nothing
// there. THE PREFIX IS INSIDE IT so a value of another format can never collide
// with one of these. THE POSITION IS NOT, because it is not known until the
// mint that stores this has landed — and it authenticates nothing: all it
// decides is whether an ABSENT row is an answer or a wait.
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
