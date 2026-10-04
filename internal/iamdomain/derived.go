package iamdomain

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// THE PERSON AN ADMINISTRATOR'S CREATE CREATES, derived from its key.
//
// A create whose outcome nobody could establish is retried under the key its
// answer handed back, and the framework answers that retry from its ledger
// without running the decide again — so the answer, which names the person
// created, can only name the same one if the person is a function of the key.
// Minted per request, the retry's answer named a second person nobody created.
//
// A REDEMPTION NEEDS NO DERIVATION. It is one record under an operation derived
// from its invitation, so a retry of one that landed is answered from the
// ledger and refused as a link already used, and one that did not land
// published nothing — a fresh person per attempt is correct, and a redeemer
// told their login was taken simply chooses another.
//
// A UUID7, because every person id is one: the directory pages in id order and
// that order is creation order. Its instant is the key's own, and its random
// bits are a digest of the key under a label of its own, so a derivation can
// never meet a minted id except by the collision a uuid7's 74 random bits
// already rule out.

// CreatedPersonID is the person an administrator's create names, derived from
// the OPERATION KEY the create is published under.
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
