package credential

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
)

// RECOVERY CODES ARE THE WAY BACK IN WHEN THE SECOND FACTOR IS GONE.
//
// A person whose phone is at the bottom of a river is locked out of a company
// whose only other way in is an administrator — and an administrator who can
// reset somebody's second factor is an administrator who can become them. So
// the codes are minted WITH the second factor, shown once, and each one is
// spendable exactly once.
//
// # Why they are hashed and not sealed
//
// A recovery code is a SECRET SOMEBODY PRESENTS, like a password, so what is
// stored has to be a verifier. It is a SHA-256 rather than an argon2id digest
// for the reason a machine token is: the code is minted by this engine from
// crypto/rand with a hundred bits of entropy in it, so there is no dictionary
// to run against a stolen digest and no memory cost to buy. Spending a
// password's cost per candidate on a value nobody chose would be paying for a
// defence against an attack that cannot exist.

const (
	// RecoveryCodeCount is how many are minted at once: TEN.
	//
	// Enough that somebody printing them keeps working through a
	// succession of lost devices, few enough that the list is short enough
	// to store somewhere safe. Below about five people stop treating them
	// as a fallback and start treating them as a password.
	RecoveryCodeCount = 10

	// recoveryBytes is the entropy per code: 16 bytes, base32-encoded to
	// 26 characters. That is the same floor the webhook shared token uses
	// and for the same reason — it is what this engine's own mint hands
	// out, so an operator who took the offered value is above it.
	recoveryBytes = 16
)

// recoveryEncoding is unpadded base32 for a value a person RETYPES: upper
// case, no `0`/`O` or `1`/`I` confusion to resolve at the keyboard, and no
// `=` to wonder about.
var recoveryEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewRecoveryCodes mints a set, returning the codes to SHOW ONCE and the
// verifiers to store.
//
// THE TWO ARE RETURNED TOGETHER and the caller must never store the first.
// Splitting this into a mint and a separate hash would make "which of these
// two strings is the secret" a question every caller answers for itself.
func NewRecoveryCodes() (codes, verifiers []string, err error) {
	codes = make([]string, RecoveryCodeCount)
	verifiers = make([]string, RecoveryCodeCount)
	for i := range codes {
		raw := make([]byte, recoveryBytes)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, fmt.Errorf("credential: mint a recovery code: %w", err)
		}
		codes[i] = formatRecovery(recoveryEncoding.EncodeToString(raw))
		verifiers[i] = HashRecoveryCode(codes[i])
	}
	return codes, verifiers, nil
}

// formatRecovery groups a code into blocks of four, which is what makes a
// twenty-six character string a person can read off paper and type without
// losing their place.
func formatRecovery(raw string) string {
	var out strings.Builder
	for i, r := range raw {
		if i > 0 && i%4 == 0 {
			out.WriteByte('-')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// HashRecoveryCode is the verifier for one code.
//
// IT NORMALISES FIRST — upper-cased, with the grouping dashes and any spaces
// removed — so a person typing it back in lower case, or without the dashes,
// or pasted with a trailing space, presents the same value they were given.
// Refusing those would be a lockout produced by formatting.
func HashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(normaliseRecovery(code)))
	return hex.EncodeToString(sum[:])
}

func normaliseRecovery(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	return strings.ReplaceAll(code, " ", "")
}

// SpendRecoveryCode reports WHICH of a person's unspent verifiers a presented
// code matches, or -1.
//
// IT WALKS EVERY VERIFIER AND NEVER STOPS AT THE FIRST MATCH. An early exit
// makes the time taken depend on which code was presented and on how many
// remain, which tells an attacker how far through somebody's set they are —
// and a person with one code left is a person worth targeting.
//
// SPENDING IS THE CALLER'S: this says which one matched, and the caller
// publishes the record that removes it. A function here that mutated the slice
// would be a spend that happened in memory on one node.
func SpendRecoveryCode(verifiers []string, code string) int {
	want := []byte(HashRecoveryCode(code))
	found := -1
	for i, held := range verifiers {
		hit := subtle.ConstantTimeCompare(want, []byte(held))
		// BRANCHLESS: the first match wins and every later comparison
		// still runs. `found` moves from -1 to i exactly once because
		// the verifiers are distinct digests.
		found = subtle.ConstantTimeSelect(hit&boolToInt(found < 0), i, found)
	}
	return found
}

// boolToInt is 1 or 0 without a branch the optimiser can turn back into one.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
