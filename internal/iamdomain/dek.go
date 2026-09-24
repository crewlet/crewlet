package iamdomain

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/secrets"
)

// THE PER-PERSON KEY, and why removing somebody is a key deletion rather than
// a row deletion.
//
// # A removal has to reach an artefact taken before it
//
// Deleting a person's row removes them from every node's current database and
// from nothing else. The backups still hold them, the donated snapshots still
// hold them, and the log — which is the only copy of what no node has applied
// yet — holds the record that wrote them, byte for byte, for as long as
// retention says. A removal that only deleted rows would be a promise the
// estate cannot keep.
//
// So a person's NAME and ADDRESS are sealed under a key that belongs to them
// alone, and removing them DESTROYS THAT KEY. Every copy of the ciphertext,
// wherever it already is, becomes unreadable at once, and nothing has to be
// found or rewritten. The row and the id survive, because the audit trail
// names them: a history whose authors evaporate is not an audit trail.
//
// # Why the key lives in the fleet secret store and not in a column
//
// A key beside its own ciphertext protects nothing. The fleet secret store is
// the one place in this estate whose bucket deliberately has NO retention age
// — which is exactly what makes a delete there a delete, rather than a value
// that ages out on some nodes and not others.
//
// It is also why the DEK is not in the replicated estate: the replicated
// tables are byte-identical across the fleet BY CLAIM, and a key that rode
// them would be copied into every snapshot a node donates, which is the one
// artefact a removal cannot reach.
//
// # Sealing happens at the WRITER, before publication
//
// Every node writes the same ciphertext, which is what keeps the identity
// claim a byte comparison rather than a claim about which nodes hold which
// keys. Per-node encryption would have made a determinism check impossible to
// write — and a domain whose rows legitimately differ everywhere has no way to
// notice that one node's applier has diverged.

// Keys is the fleet secret store, as this domain uses it.
//
// CONSUMER-DEFINED and three methods wide, because that is what this package
// calls for a value it may simply replace — a session's refresh grant, which a
// provider's rotation overwrites: [fleetsecrets.Estate] is the concrete type
// and it does more. A seam this narrow is also what lets the suite exercise a
// shredded key, a store that cannot be reached and a key that is not there,
// none of which a real coordination backend makes easy to arrange.
type Keys interface {
	Get(ctx context.Context, name string) (string, error)
	Set(ctx context.Context, name, value string, by secrets.Author,
		source string, now time.Time) error
	Unset(ctx context.Context, name string) (bool, error)
}

// PersonKeys is the store as a person's key lives and dies in it: [Keys], and
// the three writes that never act on a key they have not seen.
//
// A person's key is the one value here a plain put is WRONG for, three ways,
// and each of the three writes is the answer to one:
//
//   - Create writes only where no key is stored, because two minters of one
//     id — a redemption and its own retry, both naming the derived person —
//     each seal under the key they wrote while the store keeps one of them.
//   - Touch re-dates a key only while it is there, because a gesture that
//     re-uses a key has to leave a mark the key duty ages it by, and a mark
//     written after a removal's shred would bring the removed key back.
//   - UnsetAt destroys a key only at the version it was judged at, because
//     the key duty judges from a census and a key a retry touched since is
//     no longer the key it judged.
type PersonKeys interface {
	Keys
	Create(ctx context.Context, name, value string, by secrets.Author, source string,
		now time.Time) (bool, error)
	Touch(ctx context.Context, name string, by secrets.Author, source string,
		now time.Time) (bool, error)
	UnsetAt(ctx context.Context, name string, version uint64) (bool, error)
}

// PersonDEKName is where one person's data encryption key lives.
//
// KEYED ON THE ID NOTHING RENAMES, so a person who changes their address,
// their login and their seat keeps the key their existing ciphertext was
// sealed under. Keyed on anything mutable, a rename would orphan every value
// already written and read as a shredded person.
//
// IN THE ENGINE'S OWN NAMESPACE — `iam/person/<id>/dek` — which no operator
// surface lists, reveals, writes, deletes or resolves ([secrets.Reserved]). It
// was briefly `IAM_PERSON_<ID>_DEK`, an environment-variable name, so the
// store would accept it; that made every person's key an ordinary operator
// secret — revealable, so a copy taken before a removal defeated the shred the
// removal is, and deletable, so a DELETE shredded somebody with no removal on
// record. The path shape is also what no `${VAR}` can ever name.
//
// THE ID IS KEPT VERBATIM, one segment, which makes the name REVERSIBLE: the
// key duty reads the id back out of a name to ask whether anybody still owns
// the key ([idOfPersonKey]). An id that does not fit one segment of the
// engine's grammar is refused at the mint rather than folded, because a fold
// is how two ids come to share a key.
func PersonDEKName(personID string) string {
	return personKeyPrefix + personID + personKeySuffix
}

// SessionRefreshName is where one session lineage's refresh material lives.
//
// PER LINEAGE rather than per person, because ending one session must not end
// the others: a delete here is what makes a single sign-out irreversible for
// that session and leaves every other one alone. The person's own revocation
// epoch is the other lever, and it is the one that ends all of them at once.
//
// `iam/session/<lineage>/refresh`, in the engine's own namespace for
// [PersonDEKName]'s reason — and for a sharper one: this value is a live
// credential at somebody else's identity provider.
func SessionRefreshName(lineage string) string {
	return sessionRefreshPrefix + lineage + sessionRefreshSuffix
}

// The fixed halves of the two per-object names, which is also what a duty
// lists the store by: the key duty finds every person key by its prefix, and
// the deactivation probe every session's refresh material by its own.
const (
	personKeyPrefix      = "iam/person/"
	personKeySuffix      = "/dek"
	sessionRefreshPrefix = "iam/session/"
	sessionRefreshSuffix = "/refresh"
)

// idOfPersonKey reads the id back out of a person key's name, or reports that
// the name is not one [PersonDEKName] writes.
func idOfPersonKey(name string) (string, bool) {
	id, ok := strings.CutPrefix(name, personKeyPrefix)
	if !ok {
		return "", false
	}
	id, ok = strings.CutSuffix(id, personKeySuffix)
	if !ok || checkKeyID(id) != nil {
		return "", false
	}
	return id, true
}

// checkKeyID refuses an id that cannot be ONE segment of an engine-owned name.
//
// ONE SEGMENT, not merely a valid path: an id carrying a `/` would still make
// a well-formed name, and one that no longer reads back as the id it was
// written for.
func checkKeyID(id string) error {
	if strings.Contains(id, "/") {
		return fmt.Errorf("iamdomain: %q cannot address a key: an id is one "+
			"segment of the key's name, and a '/' would make it several", id)
	}
	if err := secrets.CheckEstateName(personKeyPrefix + id + personKeySuffix); err != nil {
		return fmt.Errorf("iamdomain: %q cannot address a key — an id is one "+
			"segment of the key's name, lower-case letters, digits, '-', '_' "+
			"and '.': %w", id, err)
	}
	return nil
}

// ErrShredded reports a person whose key has been destroyed.
//
// ITS OWN ERROR, and not a decryption failure, because the two send a caller
// to opposite places. A removed person's row is expected to be unreadable and
// a surface renders it as "removed"; a decryption failure on a person who is
// NOT removed is a key-store outage or a corrupted row, and rendering that as
// "removed" would tell an operator somebody had been off-boarded who had not.
var ErrShredded = errors.New("iamdomain: this person's key has been destroyed")

// Sealer seals and opens the values that belong to one person at a time.
type Sealer struct{ keys PersonKeys }

// NewSealer builds one over the company's secret store.
func NewSealer(keys PersonKeys) (*Sealer, error) {
	if keys == nil {
		return nil, errors.New("iamdomain: a sealer with no key store would " +
			"write cleartext names and addresses into every node's database, " +
			"every snapshot and every backup")
	}
	return &Sealer{keys: keys}, nil
}

// mintAttempts bounds how many times a mint goes round create-or-touch.
//
// THREE, for the secret store's own reason for bounding a conditional write: each
// round loses only to a write that landed between its two steps — a concurrent
// minter's create, or a destroy of the key it was about to touch — and each of
// those lands once. A key still changing after that is being written in a
// loop, and the gesture fails naming it rather than spinning.
const mintAttempts = 3

// Mint makes sure a person's key exists, and that its WRITE TIME says a gesture
// is using it now: it creates the key where there is none, and otherwise
// touches the one it finds. It never replaces a key.
//
// NEVER A REPLACEMENT, because overwriting a live key fails nothing
// immediately: new values seal fine, and every value already written becomes
// unreadable — a silent, irreversible shred of everything that person's row
// held. So the key is CREATED, which the store refuses where a key exists,
// rather than read and then put: two minters of one id — a redemption and its
// own retry, which name the same derived person — each read "none" and each
// put, and the loser's values were sealed under a key the store did not keep.
//
// A KEY IT FINDS IS TOUCHED, because a retry re-uses the key its first attempt
// minted, and the key duty ages a key nobody owns by its write time: left at
// the first attempt's, an hour or a week old, the key could be judged nobody's
// and destroyed under the retry that was about to seal a person's name and
// address under it. The touch moves the key's version too, so a destroy
// judged before it is refused ([Sealer.Collect]). And a touch finds no key
// that went in between — a removal's shred, the duty's collection — rather
// than writing it back, so the round creates a fresh one instead: values
// sealed under the destroyed key stay unreadable, which is what destroying it
// promised.
//
// BY IS THE PARTY WHOSE GESTURE MINTED IT — the enrolment or the invitation
// that needed a key — recorded as the secret store records every author, on
// the create and on every touch.
//
// Every failure is the unknown answer: a store that cannot be reached is never
// read as "no key here".
func (s *Sealer) Mint(ctx context.Context, personID string, by secrets.Author,
	now time.Time) error {
	if err := s.check(personID); err != nil {
		return err
	}
	name := PersonDEKName(personID)
	for attempt := 1; ; attempt++ {
		key := make([]byte, keyBytes)
		if _, err := rand.Read(key); err != nil {
			return fmt.Errorf("iamdomain: generate a key for %s: %w", personID, err)
		}
		created, err := s.keys.Create(ctx, name,
			base64.StdEncoding.EncodeToString(key), by, "iam", now)
		if err != nil {
			return fmt.Errorf("iamdomain: mint %s: %w — this node cannot tell "+
				"whether a key is already there", name, err)
		}
		if created {
			return nil
		}
		touched, err := s.keys.Touch(ctx, name, by, "iam", now)
		if err != nil {
			return fmt.Errorf("iamdomain: re-date %s for the gesture using it: "+
				"%w", name, err)
		}
		if touched {
			return nil
		}
		if attempt == mintAttempts {
			return fmt.Errorf("iamdomain: %s was created and destroyed %d times "+
				"while this gesture minted it; retry the gesture", name, mintAttempts)
		}
	}
}

// Collect destroys a key nobody owns, but only the version of it a census
// judged, reporting whether it destroyed it.
//
// FALSE IS A KEY WRITTEN SINCE — a retried gesture that touched it to seal
// under it, or a rekey that moved it — or one already gone. The judgement was
// about a key that is no longer what is stored, so it is void, and the next
// census judges what is.
func (s *Sealer) Collect(ctx context.Context, personID string, version uint64) (bool, error) {
	if err := s.check(personID); err != nil {
		return false, err
	}
	destroyed, err := s.keys.UnsetAt(ctx, PersonDEKName(personID), version)
	if err != nil {
		return false, fmt.Errorf("iamdomain: destroy %s's unowned key: %w",
			personID, err)
	}
	return destroyed, nil
}

// Shred destroys a person's key, which is what removing them does.
//
// IT REPORTS WHETHER A KEY WAS THERE, because the two answers are both
// ordinary and the caller renders them differently: a key that was destroyed
// is a removal that took effect now, and a key that was already gone is a
// removal being retried — which is the common case, since the record that
// asks for it is replayed on every node and re-delivered on any of them.
func (s *Sealer) Shred(ctx context.Context, personID string) (bool, error) {
	if err := s.check(personID); err != nil {
		return false, err
	}
	removed, err := s.keys.Unset(ctx, PersonDEKName(personID))
	if err != nil {
		return false, fmt.Errorf("iamdomain: destroy %s's key: %w — the person "+
			"is removed from every node's rows and their name and address are "+
			"still readable from an artefact taken before this, so the delete "+
			"has to be retried until it succeeds", personID, err)
	}
	return removed, nil
}

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
)

// Seal encrypts one of a person's own values.
//
// THE ASSOCIATED DATA IS (PERSON, FIELD), and the FIELD half is the part that
// buys something. The person half looks like the defence and is not: every
// person has a key of their own, so a ciphertext moved between two people
// already fails on the key, and binding the id is a second, redundant
// statement of what the key already says. It is kept because it costs nothing
// and it is what makes an envelope self-describing — but the doc says which
// half is load-bearing, because a defence with a false rationale is one the
// next reader trusts for the wrong reason.
//
// THE FIELD HALF IS NOT REDUNDANT: all of one person's values are sealed under
// one key, so without it a restore, a bad migration or a bug that put their
// sealed ADDRESS in the name column would decrypt cleanly and render an
// address as somebody's name. With it, that row refuses. It is
// internal/secrets' own [secrets.AADForVar] idiom, which binds a value to the
// variable it belongs to for exactly this reason.
func (s *Sealer) Seal(ctx context.Context, personID string, field Field,
	plaintext string) (string, error) {

	cipher, err := s.cipherFor(ctx, personID)
	if err != nil {
		return "", err
	}
	sealed, err := cipher.Encrypt(plaintext, AADFor(personID, field))
	if err != nil {
		return "", fmt.Errorf("iamdomain: seal %s's %s: %w", personID, field, err)
	}
	return sealed, nil
}

// Open decrypts one of a person's own values.
func (s *Sealer) Open(ctx context.Context, personID string, field Field,
	sealed string) (string, error) {

	if sealed == "" {
		return "", nil
	}
	cipher, err := s.cipherFor(ctx, personID)
	if err != nil {
		return "", err
	}
	plain, err := cipher.Decrypt(sealed, AADFor(personID, field))
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

// cipherFor builds the one-key cipher a person's values are sealed under.
//
// IT REUSES [secrets.NewCipher] rather than reaching for AES-GCM here, on
// ADR-0008's terms: a second implementation of an envelope format is two
// answers to what a sealed value looks like, and the one that drifts is
// whichever nobody re-reads. A person's key is simply a keyring with one key
// in it.
//
// NOT CACHED, deliberately. A cache keyed on the person id would go on opening
// values for somebody whose key a removal has just destroyed, on whichever
// nodes happened to hold the entry — which turns a fleet-wide irreversible
// shred into one that took effect on some nodes and not others, discovered
// whenever a cache happened to be evicted. The cost is a secret-store read per
// open, and the reads that matter are on the request path's own budget.
func (s *Sealer) cipherFor(ctx context.Context, personID string) (secrets.Cipher, error) {
	if err := s.check(personID); err != nil {
		return nil, err
	}
	name := PersonDEKName(personID)
	encoded, err := s.keys.Get(ctx, name)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return nil, fmt.Errorf("%w: %s is gone", ErrShredded, name)
	case err != nil:
		return nil, fmt.Errorf("iamdomain: read %s: %w", name, err)
	case encoded == "":
		// AN EMPTY VALUE IS A SHRED, not an empty key. A store that
		// answers "" for a name it holds is the shape a partially
		// completed delete leaves behind, and deriving a cipher from
		// nothing would seal every person's values under one key.
		return nil, fmt.Errorf("%w: %s is empty", ErrShredded, name)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("iamdomain: %s is not base64: %w", name, err)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: personKeyID, Keys: map[string][]byte{personKeyID: key},
	})
	if err != nil {
		return nil, fmt.Errorf("iamdomain: %s is not a usable key: %w", name, err)
	}
	return cipher, nil
}

// check refuses a call that names nobody, or names somebody by an id no key
// can be addressed under.
//
// A PERSON ID IS THE WHOLE ADDRESS of a key and the whole of its associated
// data, so an empty one would name `iam/person//dek` — one key every
// unidentified caller would share, with an AAD they would all match.
func (s *Sealer) check(personID string) error {
	if s == nil || s.keys == nil {
		return errors.New("iamdomain: this sealer has no key store")
	}
	if personID == "" {
		return errors.New("iamdomain: a sealed value needs the person it " +
			"belongs to: an empty id names one key every unidentified caller " +
			"would share, under associated data they would all match")
	}
	return checkKeyID(personID)
}

const (
	// personKeyID is the one key id inside a person's own keyring. The
	// envelope format carries it, and every person's is the same literal
	// because the keyring has exactly one key in it — which key it is, is
	// said by the secret NAME rather than by this.
	personKeyID = "dek"

	// keyBytes is AES-256's key length, which internal/secrets' keyring
	// requires and validates.
	keyBytes = 32
)
