package iamdomain_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
)

// keyStore is the fleet secret store as this domain uses it, in memory.
//
// A FAKE RATHER THAN THE REAL STORE, for the reason the seam is four methods
// wide: the cases below need a store that cannot be reached, a name that is
// there but empty, and a key destroyed between two reads — none of which a
// real coordination backend makes easy to arrange, and all three of which are
// the states the sealer's own rules are about.
type keyStore struct {
	// mu guards everything below, because the real store is safe for
	// concurrent use and the cases that matter most here run two
	// enrolments at once: a fake that was not would report a data race
	// for a hazard the production seam does not have.
	mu     sync.Mutex
	values map[string]string
	// fail, when set, is what every call answers. It is what stands in for
	// a coordination store this node cannot reach.
	fail error
	// failGet fails only the READ, which is the hazard the mint's guard is
	// about: a store whose write path works while its read times out is
	// exactly the state in which "I could not read a key" taken as "there
	// is no key" replaces a live one.
	failGet error
	// reads counts the opens, so the no-cache rule is checkable.
	reads int
}

func newKeyStore() *keyStore { return &keyStore{values: map[string]string{}} }

// value and put are how a case reaches inside the fake, under its own lock.
func (k *keyStore) value(name string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.values[name]
}

func (k *keyStore) put(name, value string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.values[name] = value
}

// count is how many reads the fake has served, under its own lock.
func (k *keyStore) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reads
}

func (k *keyStore) Get(_ context.Context, name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail != nil {
		return "", k.fail
	}
	if k.failGet != nil {
		return "", k.failGet
	}
	k.reads++
	value, ok := k.values[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", secrets.ErrNotFound, name)
	}
	return value, nil
}

func (k *keyStore) Set(_ context.Context, name, value, _, _ string, _ time.Time) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail != nil {
		return k.fail
	}
	k.values[name] = value
	return nil
}

func (k *keyStore) Unset(_ context.Context, name string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail != nil {
		return false, k.fail
	}
	_, had := k.values[name]
	delete(k.values, name)
	return had, nil
}

const who = "018f3a9c-0000-7000-8000-000000000001"

func newSealer(t *testing.T) (*iamdomain.Sealer, *keyStore) {
	t.Helper()
	store := newKeyStore()
	sealer, err := iamdomain.NewSealer(store)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return sealer, store
}

// A REMOVAL MAKES A NAME UNRECOVERABLE FROM AN ARTEFACT TAKEN BEFORE IT.
//
// THE case this whole layer exists for. Deleting a person's row removes them
// from every node's current database and from nothing else: the backups still
// hold them, the donated snapshots still hold them, and the log holds the
// record that wrote them byte for byte. Destroying the key reaches all of it
// at once, without anything having to be found or rewritten.
func TestARemovalMakesANameUnrecoverableFromAnEarlierArtefact(t *testing.T) {
	t.Parallel()
	sealer, _ := newSealer(t)
	ctx := t.Context()
	if err := sealer.Mint(ctx, who, "operator", time.Now()); err != nil {
		t.Fatalf("mint: %v", err)
	}

	// The artefact: ciphertext written before the removal and still held
	// by a backup, a snapshot and the log.
	artefact, err := sealer.Seal(ctx, who, iamdomain.FieldName, "Sarah Chen")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(artefact, "Sarah") || strings.Contains(artefact, "Chen") {
		t.Fatalf("the sealed value %q carries the name it seals", artefact)
	}
	if plain, err := sealer.Open(ctx, who, iamdomain.FieldName, artefact); err != nil || plain != "Sarah Chen" {
		t.Fatalf("Open before the removal gave (%q, %v)", plain, err)
	}

	shredded, err := sealer.Shred(ctx, who)
	if err != nil {
		t.Fatalf("shred: %v", err)
	}
	if !shredded {
		t.Fatal("the removal reported that there was no key to destroy")
	}

	// AND THE ARTEFACT IS NOW UNREADABLE — the same bytes, on the same
	// node, by the same sealer.
	plain, err := sealer.Open(ctx, who, iamdomain.FieldName, artefact)
	if err == nil {
		t.Fatalf("a sealed value survived the removal as %q — every backup, "+
			"every snapshot and every retained record still holds it", plain)
	}
	if !errors.Is(err, iamdomain.ErrShredded) {
		t.Errorf("the refusal is %v, which a surface cannot tell from a "+
			"key-store outage — and rendering an outage as 'removed' tells an "+
			"operator somebody was off-boarded who was not", err)
	}
}

// A SHRED IS IDEMPOTENT, AND SAYS WHICH IT WAS.
//
// Both answers are ordinary and a caller renders them differently: a key that
// was destroyed is a removal taking effect now, and a key already gone is a
// removal being retried — which is the COMMON case, because the record asking
// for it is replayed on every node and re-delivered on any of them.
func TestASecondRemovalIsAnOrdinaryNoOpThatSaysSo(t *testing.T) {
	t.Parallel()
	sealer, _ := newSealer(t)
	ctx := t.Context()
	if err := sealer.Mint(ctx, who, "operator", time.Now()); err != nil {
		t.Fatalf("mint: %v", err)
	}
	first, err := sealer.Shred(ctx, who)
	if err != nil || !first {
		t.Fatalf("the first removal gave (%v, %v)", first, err)
	}
	second, err := sealer.Shred(ctx, who)
	if err != nil {
		t.Fatalf("the second removal failed: %v — a replayed record must not "+
			"stall an applier", err)
	}
	if second {
		t.Error("the second removal reported that it destroyed a key, so a " +
			"replay is indistinguishable from a removal nobody had performed")
	}
}

// A MINT NEVER REPLACES A LIVE KEY.
//
// Overwriting one fails nothing immediately: new values seal fine, and every
// value already written becomes unreadable — a silent, irreversible shred of
// everything that person's row held, discovered whenever somebody next opens
// it.
func TestAMintOverALiveKeyKeepsTheKeyItFound(t *testing.T) {
	t.Parallel()
	sealer, _ := newSealer(t)
	ctx := t.Context()
	if err := sealer.Mint(ctx, who, "operator", time.Now()); err != nil {
		t.Fatalf("mint: %v", err)
	}
	sealed, err := sealer.Seal(ctx, who, iamdomain.FieldName, "Sarah Chen")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// The retry: the same record, applied again, on this node or another.
	if err := sealer.Mint(ctx, who, "operator", time.Now()); err != nil {
		t.Fatalf("a repeated mint failed: %v — the record that asks for one is "+
			"replayed on every node", err)
	}
	plain, err := sealer.Open(ctx, who, iamdomain.FieldName, sealed)
	if err != nil || plain != "Sarah Chen" {
		t.Fatalf("a value sealed before the repeated mint opens as (%q, %v) — "+
			"the mint replaced a live key, which shreds everything already "+
			"sealed under it", plain, err)
	}
}

// A STORE THIS NODE CANNOT READ IS NOT AN ABSENT KEY.
//
// The sharpest of the three-valued rules here: a read that failed, taken as
// "no key", makes the write that follows replace a key that exists — which
// shreds the person while looking exactly like a first enrolment.
func TestAnUnreachableStoreNeverReadsAsNoKey(t *testing.T) {
	t.Parallel()
	sealer, store := newSealer(t)
	ctx := t.Context()
	if err := sealer.Mint(ctx, who, "operator", time.Now()); err != nil {
		t.Fatalf("mint: %v", err)
	}
	sealed, err := sealer.Seal(ctx, who, iamdomain.FieldName, "Sarah Chen")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	live := store.value(iamdomain.PersonDEKName(who))

	// ONLY THE READ FAILS. A store that failed the write too would refuse
	// the mint whatever its guard did, and the case would pass without
	// exercising the rule it is about.
	store.failGet = errors.New("the coordination store could not be reached")
	if err := sealer.Mint(ctx, who, "operator", time.Now()); err == nil {
		t.Fatal("a mint whose key READ failed reported success — it cannot " +
			"tell whether a key is there, and writing one destroys every " +
			"value already sealed under it")
	}
	store.failGet = nil
	if store.value(iamdomain.PersonDEKName(who)) != live {
		t.Fatal("the key changed while the store was unreachable")
	}
	if plain, err := sealer.Open(ctx, who, iamdomain.FieldName, sealed); err != nil || plain != "Sarah Chen" {
		t.Errorf("the person's values are unreadable after the failed mint: (%q, %v)",
			plain, err)
	}
}

// AN EMPTY VALUE UNDER A LIVE NAME IS A SHRED, not an empty key.
//
// It is the shape a partially completed delete leaves behind, and deriving a
// cipher from nothing would seal every person's values under one key.
func TestAnEmptyKeyValueReadsAsShreddedRatherThanAsAKey(t *testing.T) {
	t.Parallel()
	sealer, store := newSealer(t)
	ctx := t.Context()
	store.put(iamdomain.PersonDEKName(who), "")
	if _, err := sealer.Seal(ctx, who, iamdomain.FieldName, "Sarah Chen"); !errors.Is(err, iamdomain.ErrShredded) {
		t.Errorf("an empty key value gave %v, want a shredded refusal", err)
	}
}

// A CIPHERTEXT MOVED BETWEEN PEOPLE FAILS, ON THE KEY.
//
// Stated as what it proves rather than as what it looks like it proves: every
// person has a key of their own, so a row that acquired somebody else's sealed
// name refuses because the key is wrong, and the person id in the associated
// data is a second, redundant statement of the same thing. The case that
// exercises the associated data is the next one.
func TestOnePersonsSealedValueDoesNotOpenUnderAnother(t *testing.T) {
	t.Parallel()
	sealer, _ := newSealer(t)
	ctx := t.Context()
	const other = "018f3a9c-0000-7000-8000-000000000002"
	for _, id := range []string{who, other} {
		if err := sealer.Mint(ctx, id, "operator", time.Now()); err != nil {
			t.Fatalf("mint %s: %v", id, err)
		}
	}
	sealed, err := sealer.Seal(ctx, who, iamdomain.FieldName, "Sarah Chen")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if plain, err := sealer.Open(ctx, other, iamdomain.FieldName, sealed); err == nil {
		t.Fatalf("one person's sealed name opened under another's id as %q", plain)
	}
}

// ONE PERSON'S OWN VALUES DO NOT OPEN AS EACH OTHER.
//
// THIS is what the associated data buys, and the key cannot: all of a person's
// values are sealed under one key, so a restore, a bad migration or a bug that
// put their sealed ADDRESS in the name column would otherwise decrypt cleanly
// and render an address as somebody's name.
func TestOneFieldsSealedValueDoesNotOpenAsAnother(t *testing.T) {
	t.Parallel()
	sealer, _ := newSealer(t)
	ctx := t.Context()
	if err := sealer.Mint(ctx, who, "operator", time.Now()); err != nil {
		t.Fatalf("mint: %v", err)
	}
	address, err := sealer.Seal(ctx, who, iamdomain.FieldEmail, "sarah.chen@example.com")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// The same person, the same key, the wrong column.
	if plain, err := sealer.Open(ctx, who, iamdomain.FieldName, address); err == nil {
		t.Fatalf("a sealed address opened as a name, giving %q — every surface "+
			"that renders a person would print their address where their name "+
			"goes, and nothing would refuse", plain)
	}
	if plain, err := sealer.Open(ctx, who, iamdomain.FieldEmail, address); err != nil ||
		plain != "sarah.chen@example.com" {
		t.Errorf("the value does not open as what it is: (%q, %v)", plain, err)
	}
}

// NOTHING IS CACHED, which is what makes a shred take effect everywhere at
// once rather than on whichever nodes happened to evict an entry.
func TestNoKeyIsCachedAcrossOpens(t *testing.T) {
	t.Parallel()
	sealer, store := newSealer(t)
	ctx := t.Context()
	if err := sealer.Mint(ctx, who, "operator", time.Now()); err != nil {
		t.Fatalf("mint: %v", err)
	}
	sealed, err := sealer.Seal(ctx, who, iamdomain.FieldName, "Sarah Chen")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	before := store.count()
	for range 3 {
		if _, err := sealer.Open(ctx, who, iamdomain.FieldName, sealed); err != nil {
			t.Fatalf("open: %v", err)
		}
	}
	if store.count() != before+3 {
		t.Errorf("three opens cost %d key reads — a cached key goes on opening "+
			"values for somebody whose key a removal has just destroyed",
			store.count()-before)
	}
}

// A SEALER WITH NO KEY STORE, AND A CALL THAT NAMES NOBODY, ARE BOTH REFUSED.
//
// An empty id would name `IAM_PERSON__DEK`: one key every unidentified caller
// shares, under associated data they all match.
func TestASealerRefusesWhatWouldSharOneKeyBetweenEverybody(t *testing.T) {
	t.Parallel()
	if _, err := iamdomain.NewSealer(nil); err == nil {
		t.Error("a sealer was built with no key store, so it would write " +
			"cleartext names into every node's database")
	}
	sealer, _ := newSealer(t)
	ctx := t.Context()
	if _, err := sealer.Seal(ctx, "", iamdomain.FieldName, "Sarah Chen"); err == nil {
		t.Error("a value was sealed for nobody")
	}
	if err := sealer.Mint(ctx, "", "operator", time.Now()); err == nil {
		t.Error("a key was minted for nobody")
	}
	if _, err := sealer.Shred(ctx, ""); err == nil {
		t.Error("a key was destroyed for nobody")
	}
}

// AN EMPTY SEALED VALUE OPENS AS EMPTY WITHOUT TOUCHING THE KEY STORE.
//
// Most rows have an unset optional field, and a person whose key is gone must
// still render the columns that were never sealed — so opening "" must not be
// the path that discovers the shred.
func TestAnEmptySealedValueNeedsNoKeyAtAll(t *testing.T) {
	t.Parallel()
	sealer, store := newSealer(t)
	plain, err := sealer.Open(t.Context(), who, iamdomain.FieldName, "")
	if err != nil || plain != "" {
		t.Fatalf("opening an unset value gave (%q, %v)", plain, err)
	}
	if store.count() != 0 {
		t.Errorf("opening an unset value cost %d key reads", store.count())
	}
}

// THE TWO SECRET NAMES ARE ADDRESSED BY WHAT NOTHING RENAMES.
func TestTheSecretNamesAreKeyedOnWhatNothingRenames(t *testing.T) {
	t.Parallel()
	if got, want := iamdomain.PersonDEKName(who),
		"IAM_PERSON_018F3A9C_0000_7000_8000_000000000001_DEK"; got != want {
		t.Errorf("a person's key lives at %q, want %q", got, want)
	}
	if got := iamdomain.SessionRefreshName("lin1"); got != "IAM_SESSION_LIN1_REFRESH" {
		t.Errorf("a session's refresh material lives at %q", got)
	}
	// TWO PEOPLE NEVER SHARE A KEY: the fold keeps every hex digit and
	// puts the hyphens at fixed offsets, so two distinct ids stay two
	// distinct names.
	other := "018f3a9c-0000-7000-8000-000000000002"
	if iamdomain.PersonDEKName(who) == iamdomain.PersonDEKName(other) {
		t.Error("two people fold to one key name — removing either destroys both")
	}
	// PER LINEAGE, not per person: ending one session must not end the
	// others, so two lineages must never share a name.
	if iamdomain.SessionRefreshName("a") == iamdomain.SessionRefreshName("b") {
		t.Error("two session lineages share one secret name — signing out of " +
			"one would end the other")
	}
}

// EVERY SECRET THIS ESTATE STORES IS NAMED IN THE STORE'S OWN GRAMMAR.
//
// The company's secret store is keyed by environment-variable name and refuses
// any other at the write, so a name outside that grammar is not a style
// question: it is a key nothing can mint. The three used to be path-shaped
// (`iam/person/<id>/dek`), every one was refused on a real deployment, and the
// fake above — which accepts any name — is why no case noticed.
func TestEverySecretNameIsOneTheCompanyStoreAccepts(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV7()).String()
	for _, name := range []string{
		iamdomain.PersonDEKName(id),
		iamdomain.SessionRefreshName(id),
		iamdomain.BlindKeyName,
	} {
		if err := secrets.CheckName(name); err != nil {
			t.Errorf("%q is refused by the company's secret store: %v", name, err)
		}
	}
}

// AND A PERSON'S KEY LIVES AND DIES IN THE REAL STORE, not only in the fake.
//
// Mint, seal, open, shred, and the open after the shred answering
// [iamdomain.ErrShredded] — over internal/fleetsecrets on an in-memory
// coordination backend, which is the store every deployment runs. It is the
// case the grammar above protects end to end.
func TestAPersonsKeyLivesAndDiesInTheRealSecretStore(t *testing.T) {
	t.Parallel()
	store := fleetsecrets.New(coordmem.NewFleet(), realCipher(t))
	sealer, err := iamdomain.NewSealer(store)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	ctx := t.Context()
	id := uuid.Must(uuid.NewV7()).String()
	if err := sealer.Mint(ctx, id, "node-a", time.Now()); err != nil {
		t.Fatalf("the real store refused a person's key: %v", err)
	}
	sealed, err := sealer.Seal(ctx, id, iamdomain.FieldName, "Sarah Chen")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if plain, err := sealer.Open(ctx, id, iamdomain.FieldName, sealed); err != nil ||
		plain != "Sarah Chen" {
		t.Fatalf("Open = (%q, %v)", plain, err)
	}
	if destroyed, err := sealer.Shred(ctx, id); err != nil || !destroyed {
		t.Fatalf("Shred = (%v, %v), want the key destroyed", destroyed, err)
	}
	if _, err := sealer.Open(ctx, id, iamdomain.FieldName, sealed); !errors.Is(err,
		iamdomain.ErrShredded) {
		t.Fatalf("a shredded person's name opened (err %v)", err)
	}
}

// realCipher is a one-key keyring, which is what the company store seals under.
func realCipher(t *testing.T) secrets.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": key},
	})
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return cipher
}
