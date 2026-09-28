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
// a person holding a one-time credential (an invitation link), a FRESH id per
// attempt makes the sequence impossible to finish once
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
// spent invitation is refused before any record is formed.
//
// A UUID7, because every person id is one: the directory pages in id order and
// that order is creation order. Its instant is the credential's own — the
// invitation's id is a uuid7 minted when it was issued — and its random bits
// are a
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
	return derivedID("invitation", invitationID, instantOf(id)).String(), nil
}

// CreatedPersonID is the person an administrator's create names, derived from
// the OPERATION KEY the create is published under.
//
// # Why a create is derived too
//
// `POST /iam/people` is the same sequence a redemption is — the address, the
// login, the person — and a create whose outcome nobody could establish is
// retried under the same key, which the answer hands back for exactly that. A
// person minted per request made that retry name a SECOND person: its address
// claim found the address held by the first attempt's person and refused the
// retry as a conflict with somebody else, so the documented retry of an unknown
// answered 409 against its own first attempt, and the person it may have
// created could not be recovered. Derived from the key, every attempt of one
// operation names one person, and a claim its first attempt took is one the
// retry already holds.
//
// THE KEY MUST BE A UUID7, and its instant is the person's, for the reason
// every person id is one: the directory pages in id order and that order is
// creation order. A key that is not one is [ErrInvalid] — the surface mints one
// where the caller sent none, and hands it back for the retry.
func CreatedPersonID(key string) (string, error) {
	id, err := operationKey(key)
	if err != nil {
		return "", err
	}
	return derivedID("create", id.String(), instantOf(id)).String(), nil
}

// operationKey parses an operation key a created identity is derived from,
// refusing one that is not a uuid7.
func operationKey(key string) (uuid.UUID, error) {
	id, err := uuid.Parse(key)
	if err != nil || id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		return uuid.UUID{}, fmt.Errorf("%w: the operation key %q is not a "+
			"uuid7. A create's key is the seed of the id it creates, and every "+
			"id this estate creates is a uuid7 whose instant is its creation — "+
			"send the key the first attempt was published under, which the "+
			"answer carried as its op_id", ErrInvalid, key)
	}
	return id, nil
}

// derivedID is the one derivation every derived identity shares.
func derivedID(label, origin string, at time.Time) uuid.UUID {
	digest := sha256.Sum256([]byte("crewlet/iam/derived-person/" + label +
		"\x00" + origin))
	return uuid7At(at, digest[:16])
}

// uuid7At lays sixteen derived bytes out as a uuid7 at an instant.
//
// THE INSTANT FIRST, 48 bits of milliseconds, then the version and the variant
// over the derived bits — the uuid7 layout, so every reader that orders or ages
// an id reads a derived one as it reads a minted one.
func uuid7At(at time.Time, bits []byte) uuid.UUID {
	var id uuid.UUID
	copy(id[:], bits)
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(at.UnixMilli()))
	copy(id[:6], ms[2:])
	id[6] = (id[6] & 0x0f) | 0x70
	id[8] = (id[8] & 0x3f) | 0x80
	return id
}

// instantOf is the millisecond a uuid7 was minted at.
func instantOf(id uuid.UUID) time.Time {
	var ms [8]byte
	copy(ms[2:], id[:6])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(ms[:]))).UTC()
}
