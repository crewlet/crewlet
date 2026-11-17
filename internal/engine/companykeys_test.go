package engine_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// peopleDoc is [companyDoc] with a free human seat for each person these cases
// create (ADR-0032), so a case expecting the blind-key refusal is not refused
// over the seat instead.
const peopleDoc = companyDoc + `
  - name: Ada Lovelace
    handle: ada-lovelace
    kind: human
  - name: Grace Hopper
    handle: grace-hopper
    kind: human
`

// identityEngine is a node running the identity domain on a company declaring
// a human seat per person, with the company's secret store it mints into.
func identityEngine(t *testing.T) (*engine.Engine, *fleetsecrets.Estate) {
	t.Helper()
	boot := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	})
	e := newEngine(t, engine.Options{Bootstrap: boot,
		Company: parsedCompany(t, peopleDoc)})
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	if e.IAMWriter() == nil {
		t.Fatal("the node runs no identity domain")
	}
	return e, fleetsecrets.New(e.Backends().Fleet, cipher).Estate()
}

// enrolAddress creates one person with an address on a human seat through the
// node's own admin writer, the record that blinds the address. The person is
// derived from the operation's key ([iamdomain.CreatedPersonID]), and the key
// is MINTED with its instant, for the reason [statelog.NewOpID] gives.
func enrolAddress(t *testing.T, e *engine.Engine, login, email, seat string) (string, error) {
	t.Helper()
	key := statelog.NewOpID(time.Now(), "")
	id, err := iamdomain.CreatedPersonID(key)
	if err != nil {
		t.Fatalf("derive the created person: %v", err)
	}
	_, err = e.IAMWriter().Create(t.Context(), iamdomain.Creation{
		PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: login, Email: email, Login: login, Seat: seat,
		LinkExpiresAt: time.Now().Add(credential.EnrolmentLinkLifetime),
		OpID:          key, Reason: "the first person",
	})
	return id, err
}

// THE FIRST ADDRESS A COMPANY BLINDS MINTS ITS KEY, and every later blind is
// derived under the stored one.
//
// A real node and its own writer, not a writer handed a key by the test: an
// enrolment with an address on a fresh company must mint the key itself.
func TestTheFirstAddressAnEngineBlindsMintsTheCompanysKey(t *testing.T) {
	t.Parallel()
	e, store := identityEngine(t)

	// THE CONTROL: a fresh company has no key, so what follows is a mint.
	if _, err := store.Get(t.Context(), iamdomain.BlindKeyName); !errors.Is(err,
		secrets.ErrNotFound) {
		t.Fatalf("a fresh company already holds %s (%v)", iamdomain.BlindKeyName, err)
	}

	id, err := enrolAddress(t, e, "ada.lovelace", "ada@example.com", "ada-lovelace")
	if err != nil {
		t.Fatalf("enrol somebody with an address on a fresh company: %v", err)
	}
	stored, err := store.Get(t.Context(), iamdomain.BlindKeyName)
	if err != nil {
		t.Fatalf("the enrolment minted no key: %v", err)
	}

	// THE NODE DERIVES UNDER THE STORED KEY, the only one a peer can read.
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
// An estate holding a blind had a key, so a fresh one would orphan every
// address and let a second person claim each (the claim arbitrates on the
// blind). The node refuses by name, stores nothing, and refuses every address
// write until an operator restores the key.
func TestAMissingBlindKeyIsNeverMintedOverAnEstateThatUsedOne(t *testing.T) {
	t.Parallel()
	e, store := identityEngine(t)
	if _, err := enrolAddress(t, e, "ada.lovelace", "ada@example.com",
		"ada-lovelace"); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	// DELETED UNDER THE ENGINE, in the coordination store: no engine surface
	// deletes the key, so a deletion is somebody else's mistake.
	if removed, err := e.Backends().Fleet.DeleteSecret(t.Context(),
		iamdomain.BlindKeyName); err != nil || !removed {
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
	if _, err := enrolAddress(t, e, "grace.hopper", "grace@example.com",
		"grace-hopper"); !errors.Is(err,
		iamdomain.ErrNoBlindKey) {
		t.Errorf("an enrolment with an address answered %v, want the refusal "+
			"naming the key", err)
	}
}
