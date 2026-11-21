package iamdomain

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/secrets"
)

// A person's name and address, an invitation's address and a second factor's
// seed are sealed under the fleet keyring; the rest of a row is not personal,
// in the clear on purpose (a login), or one-way (a keyed blind, a verifier).
// They are sealed at the WRITER, before publication: the log is the write-ahead
// log, so a value sealed later would already have crossed the broker and every
// data node in the clear. Every node then writes the same ciphertext (identity
// stays a byte comparison), and the applier never opens anything. One keyring
// and [secrets.Cipher] for everything: the envelope names its key, and a rekey
// moves values onto the active one ([Writer.Reseal]). Associated data binds each
// value to whose and which it is ([AADFor]), so a pasted one fails to open. A
// removal erases what the estate holds ([Applier.writeRemoval]); the log until
// trimmed and older backups remain (docs/concepts/identity-and-access.md).

// Field names which of a person's values a ciphertext is. It is associated
// data, so a typo fails neither the compile nor the seal: it seals a value
// nothing will ever open. Hence a named type.
type Field string

const (
	// FieldName is a person's own name, as they gave it.
	FieldName Field = "name"

	// FieldEmail is their address, as authored. Its matched form is never
	// in cleartext: a lookup uses the keyed blind.
	FieldEmail Field = "email"

	// FieldTOTP is an authenticator app's shared seed, stored as a secret
	// because TOTP is symmetric ([MethodTOTP]). It is sealed through
	// [Sealer.SealCredential], never [Sealer.Seal]: a person may re-enrol,
	// so the value is bound to the credential as well as to them.
	FieldTOTP Field = "totp"
)

// Sealer seals and opens the values this estate keeps about somebody.
type Sealer struct {
	cipher secrets.Cipher

	// active is the keyring key this sealer seals under, onto which
	// [Writer.Reseal] moves a value under any other.
	active string
}

// NewSealer builds one over the fleet keyring. A nil cipher is refused, never
// read as "seal nothing": every node has a keyring, so nil is a wiring mistake
// that would write cleartext everywhere the estate goes. The active key is
// read off a probe envelope rather than passed beside the cipher: a second
// answer could differ, and every value just resealed would count as still to
// move.
func NewSealer(cipher secrets.Cipher) (*Sealer, error) {
	if cipher == nil {
		return nil, errors.New("iamdomain: a sealer with no keyring would " +
			"write cleartext names, addresses and second factors into the " +
			"log, every data node's database, every snapshot and every backup")
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

// Seal encrypts one of a person's own values under the keyring's active key,
// with (person, field) as associated data: the person half stops a value
// opening on somebody else's row, the field half stops an address opening as
// a name after a restore, migration or bug put it in the wrong column.
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

// Open decrypts one of a person's own values. Empty opens to empty (never
// given, or erased). A value this keyring cannot open (a dropped key, a restore
// under another keyring, a moved ciphertext) wraps [secrets.ErrDecrypt] without
// saying which, for internal/secrets' reason.
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

// SealCredential encrypts one of a person's credential secrets (a second
// factor's seed), bound by [AADForCredential] to the credential as well as the
// person: otherwise a replaced seed, from an old backup or a restored row,
// could be pasted over the current one and would open.
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
// Two answers, no third (the keyring is this process's own): the seed, or
// [secrets.ErrDecrypt] for a row moved, forged, erased or sealed under a key
// the ring no longer holds: a verification refuses it, worth an error line.
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
// it is, composing [AADFor]'s grammar and [secrets.AADForCredential]'s.
func AADForCredential(personID, credentialID string, field Field) string {
	return "iam_person/" + personID + "/" +
		secrets.AADForCredential(credentialID, string(field))
}

// SealInvitation encrypts the address an invitation was issued to, bound to
// that invitation by its own associated data: in [AADFor]'s grammar it would
// open as the value of any person whose id is spelled like the invitation's.
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

// check refuses a call that names nobody: the owner's id is half the
// associated data, so with an empty one any unidentified value opens as any
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
