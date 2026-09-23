package engine_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/google/uuid"
)

// identityEngine is a node running the identity domain, with the company's
// secret store it mints into.
func identityEngine(t *testing.T) (*engine.Engine, *fleetsecrets.Estate) {
	t.Helper()
	boot := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	e := newEngine(t, engine.Options{Bootstrap: boot})
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	if e.IAMWriter() == nil {
		t.Fatal("the node runs no identity domain")
	}
	return e, fleetsecrets.New(e.Backends().Fleet, cipher).Estate()
}

// enrolAddress enrols one person with an address through the node's own
// writer, which is what a bootstrap code and an invitation redemption do.
func enrolAddress(t *testing.T, e *engine.Engine, login, email string) (string, error) {
	t.Helper()
	id := uuid.New().String()
	_, err := e.IAMWriter().Enrol(t.Context(), iamdomain.Enrolment{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: login, Email: email, Login: login,
		OpID: "enrol-" + login, Reason: "the first person",
	})
	return id, err
}

// THE FIRST ADDRESS A COMPANY BLINDS MINTS ITS KEY, and every later blind is
// derived under the stored one.
//
// Nothing minted the identity estate's blind-index key, and the node read it
// once at boot and answered nil when it was absent — so on every deployment
// every enrolment with an address, every invitation and every sign-in by
// address was refused for a key that did not exist, while the estate's own
// suite passed on a writer handed a key by the test. This boots a real node
// and enrols through its own writer.
func TestTheFirstAddressAnEngineBlindsMintsTheCompanysKey(t *testing.T) {
	t.Parallel()
	e, store := identityEngine(t)

	// THE CONTROL: a fresh company has no key, so what follows is a mint
	// and not a read of something the boot already wrote.
	if _, err := store.Get(t.Context(), iamdomain.BlindKeyName); !errors.Is(err,
		secrets.ErrNotFound) {
		t.Fatalf("a fresh company already holds %s (%v)", iamdomain.BlindKeyName, err)
	}

	id, err := enrolAddress(t, e, "ada.lovelace", "ada@example.com")
	if err != nil {
		t.Fatalf("enrol somebody with an address on a fresh company: %v", err)
	}
	stored, err := store.Get(t.Context(), iamdomain.BlindKeyName)
	if err != nil {
		t.Fatalf("the enrolment minted no key: %v", err)
	}

	// THE NODE DERIVES UNDER THE STORED KEY, which is the only key a peer
	// can read: a sign-in by address on this node resolves the person the
	// enrolment wrote, and a blinder built from the store agrees with it.
	blinder, err := e.PersonBlinder().Blinder(t.Context())
	if err != nil {
		t.Fatalf("resolve the blinder: %v", err)
	}
	blind, err := blinder.Email("ada@example.com")
	if err != nil {
		t.Fatalf("blind: %v", err)
	}
	fromStore, err := iamdomain.NewBlinder([]byte(stored))
	if err != nil {
		t.Fatalf("the stored key is not one a blinder accepts: %v", err)
	}
	if peer, _ := fromStore.Email("ada@example.com"); peer != blind {
		t.Error("this node blinds under a key other than the stored one, so " +
			"no peer can match an address it wrote")
	}
	held, err := e.IAM().PersonByEmailBlind(t.Context(), blind)
	if err != nil {
		t.Fatalf("resolve by address: %v", err)
	}
	if held.ID != id {
		t.Errorf("a sign-in by address resolved %q, want the person enrolled (%s)",
			held.ID, id)
	}
}

// A MISSING KEY IS NEVER MINTED OVER AN ESTATE THAT USED ONE.
//
// No blind can be derived without the key, so an estate holding one means the
// key existed and was deleted. A fresh key then would orphan every address in
// the directory and let a second person claim each of them — the claim
// arbitrates on the blind, and a new key is a new subject. So the node refuses
// by name, stores nothing, and every address write is refused until an
// operator restores the key.
func TestAMissingBlindKeyIsNeverMintedOverAnEstateThatUsedOne(t *testing.T) {
	t.Parallel()
	e, store := identityEngine(t)
	if _, err := enrolAddress(t, e, "ada.lovelace", "ada@example.com"); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if removed, err := store.Unset(t.Context(), iamdomain.BlindKeyName); err != nil ||
		!removed {
		t.Fatalf("delete the key: removed=%v, %v", removed, err)
	}
	// A NODE THAT RESTARTED after the deletion, which holds no blinder.
	engine.ForgetPersonBlinderForTest(e)

	_, err := e.PersonBlinder().Blinder(t.Context())
	if !errors.Is(err, iamdomain.ErrNoBlindKey) {
		t.Fatalf("resolving a deleted key answered %v, want ErrNoBlindKey", err)
	}
	if _, err := store.Get(t.Context(), iamdomain.BlindKeyName); !errors.Is(err,
		secrets.ErrNotFound) {
		t.Fatalf("a key was minted over the deleted one (%v), so every stored "+
			"address is now unmatchable and claimable again", err)
	}
	if _, err := enrolAddress(t, e, "grace.hopper", "grace@example.com"); !errors.Is(err,
		iamdomain.ErrNoBlindKey) {
		t.Errorf("an enrolment with an address answered %v, want the refusal "+
			"naming the key", err)
	}
}
