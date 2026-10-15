package iamdomain

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/secrets"
)

// THE VALUES THAT ARE SOMEBODY'S, and why they are sealed at the writer under
// the fleet keyring.
//
// # What is sealed
//
// A person's NAME and ADDRESS, an invitation's ADDRESS, and a second factor's
// SEED — the one credential this estate keeps as a secret rather than a
// verifier. Everything else a row holds is either not personal (a stage, a
// grant, a position) or deliberately in the clear (a login, which the
// dashboard prints beside every change an operator reads), or a one-way value
// (an address's keyed blind, a password's argon2id verifier).
//
// # Sealed at the WRITER, before publication
//
// The log is the write-ahead log and every node's rows are derived from it,
// so a value sealed anywhere later would already have crossed the broker, the
// cluster port and every node's deferred table in the clear. Sealing it before
// the record is formed is what keeps it ciphertext on the log, in every node's
// rows, in every snapshot a node donates and in every backup — and it is what
// keeps the identity claim a byte comparison: every node writes the same
// ciphertext, so a determinism check compares content rather than counting
// rows it cannot read. The APPLIER never opens anything.
//
// # Under the fleet keyring, the one every node already holds
//
// Tier A requires the keyring on every node, serving or not: it signs every
// state-log record and seals the company document and the secret store. A
// person's values are sealed under the same keyring, through the same
// [secrets.Cipher], so there is one envelope format and one rotation story
// rather than a second key hierarchy to keep in step with it. The envelope
// NAMES the keyring key that sealed it, so a value written before a rotation
// opens under the key it was written with for as long as that key is on the
// ring — and a rekey moves every person's values onto the active key before
// the old one is dropped ([Writer.Reseal]).
//
// # Associated data is what keeps a ciphertext where it was written
//
// Every node holds the one keyring, so without associated data any sealed
// value would open wherever it was pasted: a person's address as another
// person's name, a seed they replaced over the one they hold now, an
// invitation's address as somebody's own. So each value is bound to WHOSE it
// is and WHICH of their values it is ([AADFor], [AADForCredential],
// [AADForInvitation]), and a value moved anywhere else fails to open — which
// is internal/secrets' own [secrets.AADForVar] idiom, for the same reason.
//
// # What a removal does to them
//
// A removal ERASES: its apply deletes the person's rows and clears every
// sealed value of theirs the estate still holds elsewhere — an invitation
// addressed to them, the trail rows that carried a record's payload — so no
// node's rows hold anything of theirs that opens ([Applier.writeRemoval]).
// What it cannot reach it cannot reach: the log's copy of the records that
// wrote those values, until the retention trim passes them, and a backup taken
// before the removal, until that backup is deleted. Both are sealed under the
// keyring its operator holds, and both are stated where somebody answering an
// erasure request will read them (docs/concepts/identity-and-access.md,
// SECURITY.md).
//
// # There was a key per person, and a development estate sealed under one is reset
//
// Each person's values used to be sealed under a data key of their own, kept in
// the company's secret store, and a removal destroyed it. It cost a key per
// person, a fleet-secret read on every open (so a sign-in could fail on a
// coordination blip), a duty that retried failed destroys and collected keys
// nobody owned, and a census that had to prove an absence before it destroyed
// anything — for an erasure a small self-hosted deployment gets from the
// removal, the trim and its backup rotation. A value sealed that way names a
// key this sealer never holds and opens as nothing. No tag ever shipped it, so
// no deployment an operator runs holds one; an estate made while developing
// this engine is reset — its identity log, its replicated estate and the
// person keys in its secret store — rather than carried by a second opener for
// a format nobody ran.

// Field names which of a person's values a ciphertext is.
//
// A NAMED STRING TYPE because it is ASSOCIATED DATA, which means a typo is not
// a compile error and not a decryption failure at the write — it is a value
// sealed under associated data nothing will ever present again, discovered the
// first time somebody opens it.
type Field string

const (
	// FieldName is a person's own name, as they gave it.
	FieldName Field = "name"

	// FieldEmail is their address, as authored. The MATCHED form of it
	// never appears in cleartext anywhere: what a lookup uses is the
	// keyed blind, which is not reversible at all.
	FieldEmail Field = "email"

	// FieldTOTP is an authenticator app's shared SEED — the one credential
	// this estate stores as a secret rather than a verifier, because TOTP
	// is symmetric and an engine holding only a digest could verify
	// nothing (see [MethodTOTP]). It is sealed through
	// [Sealer.SealCredential], never [Sealer.Seal]: a person may re-enrol,
	// so the value is bound to the CREDENTIAL as well as to them.
	FieldTOTP Field = "totp"
)

// Sealer seals and opens the values this estate keeps about somebody.
type Sealer struct {
	cipher secrets.Cipher

	// active is the keyring key everything this sealer seals is sealed
	// under — what a value under any other key is moved onto
	// ([Writer.Reseal]).
	active string
}

// NewSealer builds one over the fleet keyring.
//
// A NIL CIPHER IS REFUSED rather than read as "seal nothing": a sealer that
// passed values through would write cleartext names, addresses and second
// factors into the log, every node's database, every snapshot and every
// backup — and no node runs without a keyring, so a nil here is a wiring
// mistake.
//
// THE ACTIVE KEY IS READ OFF THE CIPHER ITSELF, by sealing nothing and reading
// the envelope's key id, rather than handed in beside it: a key id stated
// separately is a second answer to which key seals, and a sealer whose idea of
// "active" differed from the key its cipher actually sealed under would count
// every value it had just re-sealed as still to move.
func NewSealer(cipher secrets.Cipher) (*Sealer, error) {
	if cipher == nil {
		return nil, errors.New("iamdomain: a sealer with no keyring would " +
			"write cleartext names, addresses and second factors into the " +
			"log, every node's database, every snapshot and every backup")
	}
	probe, err := cipher.Encrypt("", "iam_sealer/active-key")
	if err != nil {
		return nil, fmt.Errorf("iamdomain: the keyring cannot seal: %w", err)
	}
	active, ok := secrets.EnvelopeKeyID(probe)
	if !ok {
		return nil, errors.New("iamdomain: the keyring sealed a value that is " +
			"not an envelope, so what it seals could never be told apart " +
			"from a value in the clear")
	}
	return &Sealer{cipher: cipher, active: active}, nil
}

// Seal encrypts one of a person's own values under the keyring's active key.
//
// THE ASSOCIATED DATA IS (PERSON, FIELD), and both halves are load-bearing
// because every person's values are sealed under one keyring: the person half
// is what stops one person's sealed address opening on somebody else's row,
// and the field half is what stops their address opening as their name after
// a restore, a bad migration or a bug put it in the wrong column.
func (s *Sealer) Seal(personID string, field Field, plaintext string) (string, error) {
	if err := s.check(personID); err != nil {
		return "", err
	}
	sealed, err := s.cipher.Encrypt(plaintext, AADFor(personID, field))
	if err != nil {
		return "", fmt.Errorf("iamdomain: seal %s's %s: %w", personID, field, err)
	}
	return sealed, nil
}

// Open decrypts one of a person's own values.
//
// AN EMPTY VALUE OPENS TO EMPTY, which is what a value nobody gave and a value
// a removal erased both are; neither is a failure. A value this keyring cannot
// open wraps [secrets.ErrDecrypt] — a key dropped from the ring while values
// were still sealed under it, a restore under a different keyring, or a
// ciphertext moved from where it was written — and says nothing about which,
// for internal/secrets' reason.
func (s *Sealer) Open(personID string, field Field, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	if err := s.check(personID); err != nil {
		return "", err
	}
	plain, err := s.cipher.Decrypt(sealed, AADFor(personID, field))
	if err != nil {
		return "", fmt.Errorf("iamdomain: open %s's %s: %w", personID, field, err)
	}
	return plain, nil
}

// AADFor binds a sealed value to the person it belongs to AND to which of
// their values it is.
func AADFor(personID string, field Field) string {
	return "iam_person/" + personID + "/" + string(field)
}

// SealCredential encrypts one of a person's CREDENTIAL secrets — a second
// factor's seed — bound to the credential it belongs to.
//
// # Sealed, because in the clear it is a second factor everybody holds
//
// The estate a seed is written to is replicated, snapshotted, backed up and
// donated to joining peers: in the clear it was a second factor every node,
// every artefact and every donor held, and read off the cluster port by
// anybody who could read a stream.
//
// # Bound to the credential as well as the person
//
// The associated data is [AADForCredential]: the person, AND the credential's
// own id, AND the field. Without the credential half a seed they replaced —
// still sitting in an old backup, or a row a restore brought back — could be
// pasted over the current credential's and would open. With it, a seed opens
// only as the credential it was enrolled as.
func (s *Sealer) SealCredential(personID, credentialID string, field Field,
	plaintext string) (string, error) {

	if credentialID == "" {
		return "", errors.New("iamdomain: a credential secret needs the " +
			"credential it belongs to: an empty id would bind every one of " +
			"this person's seeds to the same associated data")
	}
	if err := s.check(personID); err != nil {
		return "", err
	}
	sealed, err := s.cipher.Encrypt(plaintext, AADForCredential(personID, credentialID, field))
	if err != nil {
		return "", fmt.Errorf("iamdomain: seal %s's %s %s: %w", personID, field,
			credentialID, err)
	}
	return sealed, nil
}

// OpenCredential decrypts a credential secret [Sealer.SealCredential] sealed.
//
// TWO ANSWERS AND NO THIRD: the seed, or [secrets.ErrDecrypt] — a value that
// is not this credential's seed under this keyring, which a verification
// refuses, and which is worth an error line because it is a row that was
// moved, forged, erased, or sealed under a key this node's ring no longer
// holds. There is no store to be unreachable: the keyring is this process's
// own.
func (s *Sealer) OpenCredential(personID, credentialID string, field Field,
	sealed string) (string, error) {

	if credentialID == "" || sealed == "" {
		return "", fmt.Errorf("iamdomain: %s's %s has no credential id or no "+
			"sealed value to open: %w", personID, field, secrets.ErrDecrypt)
	}
	if err := s.check(personID); err != nil {
		return "", err
	}
	plain, err := s.cipher.Decrypt(sealed, AADForCredential(personID, credentialID, field))
	if err != nil {
		return "", fmt.Errorf("iamdomain: open %s's %s %s: %w", personID, field,
			credentialID, err)
	}
	return plain, nil
}

// AADForCredential binds a sealed credential secret to the person it belongs
// to, the credential it was enrolled as, and which of that credential's values
// it is — the person's half in [AADFor]'s grammar and the credential's in
// [secrets.AADForCredential]'s, so neither rule is written twice.
func AADForCredential(personID, credentialID string, field Field) string {
	return "iam_person/" + personID + "/" +
		secrets.AADForCredential(credentialID, string(field))
}

// SealInvitation encrypts the address an invitation was issued to, bound to
// that invitation.
//
// ITS OWN ASSOCIATED DATA and never a person's: there is no person yet, and an
// invitation's id is not one — bound in [AADFor]'s grammar, an invitation's
// sealed address would open as the name or the address of anybody whose id
// happened to be spelled like it. So an invitation's address opens only as
// that invitation's.
func (s *Sealer) SealInvitation(invitationID, address string) (string, error) {
	if err := s.check(invitationID); err != nil {
		return "", err
	}
	sealed, err := s.cipher.Encrypt(address, AADForInvitation(invitationID))
	if err != nil {
		return "", fmt.Errorf("iamdomain: seal invitation %s's address: %w",
			invitationID, err)
	}
	return sealed, nil
}

// OpenInvitation decrypts an invitation's address, on [Sealer.Open]'s two
// answers.
func (s *Sealer) OpenInvitation(invitationID, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	if err := s.check(invitationID); err != nil {
		return "", err
	}
	plain, err := s.cipher.Decrypt(sealed, AADForInvitation(invitationID))
	if err != nil {
		return "", fmt.Errorf("iamdomain: open invitation %s's address: %w",
			invitationID, err)
	}
	return plain, nil
}

// AADForInvitation binds a sealed address to the invitation it was issued as.
func AADForInvitation(invitationID string) string {
	return "iam_invitation/" + invitationID + "/" + string(FieldEmail)
}

// check refuses a call that names nobody.
//
// AN OWNER'S ID IS HALF THE ASSOCIATED DATA, so an empty one would bind every
// unidentified caller's value to the same data, where any of them opens as any
// other.
func (s *Sealer) check(ownerID string) error {
	if s == nil || s.cipher == nil {
		return errors.New("iamdomain: this sealer has no keyring")
	}
	if ownerID == "" {
		return errors.New("iamdomain: a sealed value needs the person or the " +
			"invitation it belongs to: an empty id binds every unidentified " +
			"caller's value to the same associated data")
	}
	return nil
}
