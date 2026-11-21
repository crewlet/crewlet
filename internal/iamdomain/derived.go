package iamdomain

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// THE PERSON AN ADMINISTRATOR'S CREATE CREATES is derived from its key: a
// retry under that key is answered from the operation ledger without running
// the decide again, so its answer names the same person only if the person is
// a function of the key.
//
// A redemption needs no derivation: as one record under an operation derived
// from its invitation, its retry is either refused as a link already used or
// follows an attempt that published nothing, so a fresh person is correct.
//
// A created person's first password link is derived from the person
// ([Blinder.PasswordLinkID], [Blinder.PasswordLinkSecret]), so the ledger's
// answer hands back the link whose verifier was recorded.

// CreatedPersonID is the person an administrator's create names, derived from
// the OPERATION KEY the create is published under.
//
// The key must be a uuid7, and its instant is the person's: the directory
// pages in id order, and that order is creation order. Any other key is
// [ErrInvalid]; the surface mints one where the caller sent none.
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
		return uuid.UUID{}, fmt.Errorf("%w: the Idempotency-Key %q isn't a "+
			"UUIDv7. To retry, send the op_id the first answer gave you",
			ErrInvalid, key)
	}
	return id, nil
}

// derivedID is the one derivation every derived identity shares. Its random
// bits are a digest of the origin under a label of its own, so it meets a
// minted uuid7 only by the collision 74 random bits rule out.
func derivedID(label, origin string, at time.Time) uuid.UUID {
	digest := sha256.Sum256([]byte("crewlet/iam/derived-person/" + label +
		"\x00" + origin))
	return uuid7At(at, digest[:16])
}

// uuid7At lays sixteen derived bytes out as a uuid7 at an instant: 48 bits of
// milliseconds first, then the version and the variant, so every reader that
// orders or ages an id reads a derived one as it reads a minted one.
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
