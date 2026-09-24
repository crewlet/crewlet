package iamdomain

import (
	"bytes"
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
	return derivedID("invitation", invitationID, instantOf(id)).String(), nil
}

// BootstrappedPersonID is the person a founding with this bootstrap code
// creates.
//
// mintedAt is the code's own row's instant — the broker's, identical on every
// node — rather than a clock read at redemption, which would differ on every
// attempt and derive a different person each time.
//
// # It carries the founder SHAPE
//
// Its last four bytes are a mark over the rest ([FounderAttempt]), so a person
// id says by itself that a founding attempt made it. That is what lets the
// next founding find an earlier attempt's reservation after the code it was
// taken with has been swept off the log — the reservation outlives the code
// row by as long as nobody releases it, and a founding that could not
// recognise it would be refused the founder's own address by it for ever. The
// three namespaces a login can take are kept apart the same way, by the shape
// of the value rather than by a lookup somebody has to remember to make.
func BootstrappedPersonID(codeID string, mintedAt time.Time) string {
	id := derivedID("bootstrap", codeID, mintedAt)
	mark := founderMark(id)
	copy(id[founderMarkAt:], mark[:])
	return id.String()
}

// FounderAttempt reports whether a person id has the shape only
// [BootstrappedPersonID] gives one: a uuid7 whose last four bytes are the
// mark over the rest.
//
// A SHAPE AND NOT A PROOF. Nothing but a founding derives an id carrying it —
// every other person id is minted fresh or derived from an invitation, and a
// fresh uuid7 carries it by chance once in four billion — and what reads it
// only ever releases a RESERVATION on an estate nobody is enrolled in, so a
// chance match costs one unfinished enrolment its claims and nothing else.
func FounderAttempt(personID string) bool {
	id, err := uuid.Parse(personID)
	if err != nil || id.Version() != 7 || id.String() != personID {
		return false
	}
	mark := founderMark(id)
	return bytes.Equal(id[founderMarkAt:], mark[:])
}

// founderMarkAt is where the founder mark starts: the LAST four bytes, which
// leaves the instant, the version, the variant and forty-two bits of the
// code's digest ahead of it — enough that two codes minted in the same
// millisecond derive two people but for a one-in-four-trillion collision.
const founderMarkAt = 12

// founderMark is the four-byte mark a founder id carries over its first
// twelve bytes, under a label nothing else derives with.
func founderMark(id uuid.UUID) [16 - founderMarkAt]byte {
	sum := sha256.Sum256(append([]byte("crewlet/iam/founder-mark\x00"),
		id[:founderMarkAt]...))
	var mark [16 - founderMarkAt]byte
	copy(mark[:], sum[:])
	return mark
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
