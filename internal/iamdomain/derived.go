package iamdomain

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// THE PERSON AN ENROLMENT THAT MAY BE RETRIED CREATES.
//
// An enrolment is a sequence — the address claim, the login claim, the person —
// and every append names the person id the caller minted. Where the caller is
// a person holding a one-time credential (an invitation link, the bootstrap
// code), a FRESH id per attempt makes the sequence impossible to finish once
// it has stopped: the first attempt's address claim holds the address for the
// id it named, and every later attempt names another id, so the retry that
// would have finished the enrolment is refused as "that address belongs to
// somebody" — by its own first attempt. A redeemer told their chosen login was
// taken could never try another.
//
// So those enrolments name a person DERIVED from the credential: every attempt
// of one redemption names one person, a claim its first attempt took is one
// the retry already holds, and the sequence finishes wherever it stopped. What
// keeps a derived person single-use is the credential rather than the id — a
// spent invitation and a spent code are refused before any record is formed.
//
// A UUID7, because every person id is one: the directory pages in id order and
// that order is creation order. Its instant is the credential's own — the
// invitation's id is a uuid7 minted when it was issued, and a bootstrap code's
// row carries the broker instant it was minted at — and its random bits are a
// digest of the credential's identity under a label of their own, so two
// derivations can never meet and no derivation can meet a minted id except by
// the collision a uuid7's 74 random bits already rule out.

// InvitedPersonID is the person redeeming this invitation creates.
//
// THE INVITATION'S ID MUST BE A UUID7, which is what this build mints for
// every invitation; its instant is the derived person's, so somebody invited
// in March sorts before somebody invited in May whenever each redeemed.
func InvitedPersonID(invitationID string) (string, error) {
	id, err := uuid.Parse(invitationID)
	if err != nil || id.Version() != 7 {
		return "", fmt.Errorf("iamdomain: invitation %q is not a uuid7, so the "+
			"person it creates has no instant to be derived at — every "+
			"invitation this build issues is one", invitationID)
	}
	return derivedPersonID("invitation", invitationID, instantOf(id)), nil
}

// BootstrappedPersonID is the person redeeming this bootstrap code creates.
//
// mintedAt is the code's own row's instant — the broker's, identical on every
// node — rather than a clock read at redemption, which would differ on every
// attempt and derive a different person each time.
func BootstrappedPersonID(codeID string, mintedAt time.Time) string {
	return derivedPersonID("bootstrap", codeID, mintedAt)
}

// derivedPersonID is the one derivation both credentials share.
func derivedPersonID(label, origin string, at time.Time) string {
	digest := sha256.Sum256([]byte("crewlet/iam/derived-person/" + label +
		"\x00" + origin))
	var id uuid.UUID
	copy(id[:], digest[:16])
	// THE INSTANT FIRST, 48 bits of milliseconds, then the version and the
	// variant over the digest's bits — the uuid7 layout, so every reader
	// that orders or ages a person id reads this one as it reads a minted
	// one.
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(at.UnixMilli()))
	copy(id[:6], ms[2:])
	id[6] = (id[6] & 0x0f) | 0x70
	id[8] = (id[8] & 0x3f) | 0x80
	return id.String()
}

// instantOf is the millisecond a uuid7 was minted at.
func instantOf(id uuid.UUID) time.Time {
	var ms [8]byte
	copy(ms[2:], id[:6])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(ms[:]))).UTC()
}
