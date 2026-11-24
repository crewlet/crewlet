package coord

import (
	"context"
	"errors"
	"time"
)

// A SEAT'S GOOGLE GROWTH IDENTITY: the service account the engine signs in to
// Google's growth products as for one agent seat (internal/googleauth's
// package doc), never the seat's Google Cloud account. Its owner is the Google
// identity pass, not any lane, so its record is a class of its own rather than
// a seat access ledger row, which is keyed by one integration: no lane's sync
// can disable it while another lane is on. The whole company agrees on it,
// since every node mints the seat's tokens from the account it names. It
// lives in the positions register until the pass deletes it.

// GoogleIdentity is one seat's growth identity at Google, addressed by the
// unique ID Google gave it when the engine made it, never by its name.
type GoogleIdentity struct {
	// Handle is the seat's handle, and the record's key.
	Handle string `json:"handle"`

	// Email is the account's email in the identity project, and UniqueID
	// the unique ID Google gave it. A record with no unique ID is the
	// pass's intent to make the account, written BEFORE Google is asked, so
	// an account made and then not recorded is still known to be Crewlet's.
	UniqueID string `json:"unique_id,omitempty"`
	Email    string `json:"email"`

	// Disabled is whether the engine turned the account off: no growth lane
	// of the seat is on.
	Disabled bool `json:"disabled"`

	// Delegated is whether the provisioner's Token Creator grant on the
	// account was written and read back, which every token waits on.
	Delegated bool `json:"delegated"`

	// CreatedAt is when the engine set out to make the account.
	CreatedAt time.Time `json:"created_at"`

	// Version is the store's version of the record as it was read. Not on
	// the wire.
	Version uint64 `json:"-"`
}

// Validate reports why an identity record cannot be written.
func (g GoogleIdentity) Validate() error {
	switch {
	case g.Handle == "":
		return errors.New("coord: a Google identity needs its seat's handle")
	case g.Email == "":
		return errors.New("coord: a Google identity needs its account's email")
	case g.CreatedAt.IsZero():
		return errors.New("coord: a Google identity needs when it was made")
	case !plainWithin(g.Handle, MaxActorBytes) || len(g.UniqueID) > maxGoogleIDBytes ||
		len(g.Email) > MaxEmailBytes:
		return errors.New("coord: a Google identity names a handle, id or email past its bound")
	}
	return nil
}

// Made reports whether Google made the account and its unique ID is recorded.
func (g GoogleIdentity) Made() bool { return g.UniqueID != "" }

// Usable reports whether a token may be minted for the identity: made, on,
// and the provisioner's grant on it in place.
func (g GoogleIdentity) Usable() bool { return g.Made() && !g.Disabled && g.Delegated }

// GoogleIdentityClass is the key class an identity is filed under in the
// positions register.
const GoogleIdentityClass = "google_identity"

// MaxEmailBytes bounds an email address a record names: RFC 5321's 254.
const MaxEmailBytes = 254

// maxGoogleIDBytes bounds a Google account's unique id, a string of digits a
// few dozen long.
const maxGoogleIDBytes = 128

// GoogleIdentityKey is a seat's identity's key in the register.
func GoogleIdentityKey(handle string) string { return DocumentKey(GoogleIdentityClass, handle) }

// GoogleIdentityHandleOf recovers the handle from a key of the identity class.
func GoogleIdentityHandleOf(key string) (string, bool) { return serverOf(GoogleIdentityClass, key) }

// GoogleIdentities is the fleet's record of each seat's growth identity.
//
// Every read RAISES rather than answering empty when the store cannot be
// read: "no identity" sends the pass to make one, which is a second account.
type GoogleIdentities interface {
	// GoogleIdentity reads one seat's identity.
	GoogleIdentity(ctx context.Context, handle string) (GoogleIdentity, bool, error)

	// ListGoogleIdentities returns every identity, ordered by handle.
	ListGoogleIdentities(ctx context.Context) ([]GoogleIdentity, error)

	// CreateGoogleIdentity writes an identity for a seat that has none,
	// returning it as stored; one that has one is left alone and reports
	// false.
	CreateGoogleIdentity(ctx context.Context, g GoogleIdentity) (GoogleIdentity, bool, error)

	// UpdateGoogleIdentity writes g at g.Version, reporting false when that
	// version no longer holds, including when the record is gone.
	UpdateGoogleIdentity(ctx context.Context, g GoogleIdentity) (GoogleIdentity, bool, error)

	// DeleteGoogleIdentity removes a seat's identity at a version, reporting
	// whether that version still held. A version of zero never holds.
	DeleteGoogleIdentity(ctx context.Context, handle string, version uint64) (bool, error)
}
