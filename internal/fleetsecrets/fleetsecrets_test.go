package fleetsecrets_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

var clock = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

// sam and the nodes are the authors the cases write as: an operator at a
// shell, and the engine on each of two nodes.
var (
	sam   = secrets.Author{Name: "sam", Kind: "operator"}
	nodeA = secrets.Author{Name: "node-a", Kind: "system"}
	nodeB = secrets.Author{Name: "node-b", Kind: "system"}
)

// ring builds a keyring with the named keys, the first active.
func ring(t *testing.T, ids ...string) secrets.Cipher {
	t.Helper()
	k := secrets.Keyring{ActiveID: ids[0], Keys: map[string][]byte{}}
	for _, id := range ids {
		// FULLY DERIVED FROM THE ID, not random with one byte pinned: a
		// rekey test builds a SECOND keyring holding the same ids in a
		// different order, and it has to open what the first one wrote.
		sum := sha256.Sum256([]byte(id))
		k.Keys[id] = sum[:]
	}
	cipher, err := secrets.NewCipher(k)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return cipher
}

func fleetStore(t *testing.T, cipher secrets.Cipher) (*fleetsecrets.Store, coord.Fleet) {
	t.Helper()
	f := coordmem.NewFleet()
	return fleetsecrets.New(f, cipher), f
}

func mustSet(t *testing.T, s *fleetsecrets.Store, name, value string) {
	t.Helper()
	if err := s.Set(t.Context(), name, value, sam, "cli", clock); err != nil {
		t.Fatalf("Set(%s): %v", name, err)
	}
}

// COORDINATION NEVER SEES PLAINTEXT. That is the whole reason a shared bucket
// is safe to put credentials in: a peer that can read it learns which names
// exist and when they changed, not what they are.
func TestTheBucketHoldsCiphertextAndNothingElse(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	s, fleet := fleetStore(t, cipher)
	mustSet(t, s, "GITLAB_TOKEN", "glpat-not-a-real-token")

	rec, found, err := fleet.Secret(t.Context(), "GITLAB_TOKEN")
	if err != nil || !found {
		t.Fatalf("Secret: %v found=%t", err, found)
	}
	if strings.Contains(rec.Value, "glpat") {
		t.Fatal("the stored value carries the plaintext, so every node that " +
			"can read the bucket can read the credential")
	}
	if rec.KeyID != "k1" {
		t.Errorf("key_id = %q, want it denormalised out of the envelope so a "+
			"rekey sweep can find stale rows without decrypting them", rec.KeyID)
	}
}

// THE NAME IS BOUND IN, so an envelope moved to another row fails to open
// rather than silently impersonating a different secret — which is what an
// attacker with write access to the bucket but no key would otherwise do.
func TestAnEnvelopeMovedToAnotherNameDoesNotOpen(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	s, fleet := fleetStore(t, cipher)
	mustSet(t, s, "REAL", "value")

	rec, _, err := fleet.Secret(t.Context(), "REAL")
	if err != nil {
		t.Fatal(err)
	}
	rec.Name = "IMPOSTOR"
	if err := fleet.PutSecret(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(t.Context(), "IMPOSTOR"); err == nil {
		t.Fatal("a relocated envelope opened, so a row can be made to stand " +
			"in for a different credential")
	}
}

// A SNAPSHOT FAILS CLOSED. A partial one is the worst outcome available: the
// names that are missing resolve to whatever the environment happens to hold,
// which is exactly the stale-.env shadowing the store exists to prevent, and
// it happens silently.
func TestASnapshotRefusesRatherThanSkippingAnUnopenableRow(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1"))
	mustSet(t, s, "GOOD", "value")
	// A row sealed by a keyring this store does not hold — a key dropped
	// from the config, which is the only way this happens.
	stranger := fleetsecrets.New(fleet, ring(t, "k9"))
	if err := stranger.Set(t.Context(), "FOREIGN", "other", sam, "cli", clock); err != nil {
		t.Fatal(err)
	}

	values, err := s.All(t.Context())
	if err == nil {
		t.Fatalf("All returned %v with a row it could not open; every name it "+
			"omitted now resolves from the environment instead", values)
	}
	if !strings.Contains(err.Error(), "FOREIGN") {
		t.Errorf("the error does not name the row that failed: %v", err)
	}
}

// A LISTING NEEDS NO KEYRING AND CARRIES NO ENVELOPE. "Is X set, and when did
// it change" is asked far more often than "what is X", and answering it must
// not require the ability to decrypt — nor put ciphertext into a scrollback.
func TestAListingCarriesNoValueAndNeedsNoKey(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1"))
	mustSet(t, s, "B", "second")
	mustSet(t, s, "A", "first")

	rows, err := fleetsecrets.New(fleet, nil).List(t.Context())
	if err != nil {
		t.Fatalf("a store with no keyring could not list what exists: %v", err)
	}
	if len(rows) != 2 || rows[0].Name != "A" || rows[1].Name != "B" {
		t.Fatalf("rows = %+v, want them name-ordered", rows)
	}
	for _, row := range rows {
		if row.Value != "" {
			t.Errorf("%s carried its envelope into a listing", row.Name)
		}
	}
}

// THE OPERATOR'S VIEW DOES NOT REACH THE ENGINE'S KEYS, by any route.
//
// The blind-index key used to be an ordinary operator secret: listed,
// revealable, deletable — so one DELETE orphaned every address in the
// directory — and decrypted into every node's ${VAR} snapshot on every apply.
// Each of those routes is closed here, and the one gesture that crosses — a
// rekey, because the keyring is one keyring — moves it and counts it.
func TestTheOperatorsViewDoesNotReachTheEnginesKeys(t *testing.T) {
	t.Parallel()
	const key = "iam/blind-index-key"
	old := ring(t, "k1", "k2")
	s, fleet := fleetStore(t, old)
	mustCreate(t, s.Estate(), key, "the-engine-key")
	mustSet(t, s, "GITLAB_TOKEN", "glpat")

	if _, err := s.Get(t.Context(), key); !errors.Is(err, secrets.ErrReservedName) {
		t.Errorf("a reveal of an engine key answered %v, want ErrReservedName", err)
	}
	if _, _, err := s.Describe(t.Context(), key); !errors.Is(err,
		secrets.ErrReservedName) {
		t.Errorf("describing an engine key answered %v, want ErrReservedName", err)
	}
	if err := s.Set(t.Context(), key, "overwritten", sam, "cli", clock); !errors.Is(err,
		secrets.ErrReservedName) {
		t.Errorf("overwriting an engine key answered %v, want ErrReservedName", err)
	}
	if _, err := s.Unset(t.Context(), key); !errors.Is(err, secrets.ErrReservedName) {
		t.Errorf("deleting an engine key answered %v, want ErrReservedName", err)
	}
	if got, err := s.Estate().Get(t.Context(), key); err != nil || got != "the-engine-key" {
		t.Fatalf("after every refused gesture the key reads %q (%v)", got, err)
	}

	rows, err := s.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "GITLAB_TOKEN" {
		t.Errorf("the operator's listing is %+v, want the one operator row", rows)
	}
	values, err := s.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if _, snapped := values[key]; snapped || len(values) != 1 {
		t.Errorf("the ${VAR} snapshot holds %d values, the engine key among "+
			"them: %t", len(values), snapped)
	}
	engine, err := s.EngineKeys(t.Context())
	if err != nil || engine.Total != 1 || engine.ByKey["k1"] != 1 {
		t.Errorf("the engine keys count %+v (%v), want one under k1", engine, err)
	}

	// A REKEY MOVES IT, and counts rather than names it.
	rotated := fleetsecrets.New(fleet, ring(t, "k2", "k1"))
	rekeyed, err := rotated.Rekey(t.Context(), "k2")
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if strings.Join(rekeyed.Moved, ",") != "GITLAB_TOKEN" || rekeyed.EngineKeys != 1 {
		t.Errorf("the rekey reported %+v, want the operator row named and the "+
			"engine key counted", rekeyed)
	}
	if engine, err := rotated.EngineKeys(t.Context()); err != nil ||
		engine.StaleUnder("k2") != 0 {
		t.Errorf("after the rekey %d engine keys are still under a retired key "+
			"(%v)", engine.StaleUnder("k2"), err)
	}
	if got, err := rotated.Estate().Get(t.Context(), key); err != nil ||
		got != "the-engine-key" {
		t.Errorf("after the rekey the engine key reads %q (%v)", got, err)
	}
}

// mustCreate mints one of the engine's own rows, as the engine does.
func mustCreate(t *testing.T, estate *fleetsecrets.Estate, name, value string) {
	t.Helper()
	if created, err := estate.Create(t.Context(), name, value, nodeA, "iam",
		clock); err != nil || !created {
		t.Fatalf("the engine's view could not mint %s: (%v, %v)", name, created, err)
	}
}

// THE ENGINE'S VIEW WRITES ONLY ITS OWN GRAMMAR, so no estate caller can reach
// an operator's credential either — and an id that would leave a segment
// empty is refused rather than shared.
func TestTheEnginesViewWritesOnlyItsOwnNames(t *testing.T) {
	t.Parallel()
	s, _ := fleetStore(t, ring(t, "k1"))
	for _, name := range []string{
		"GITLAB_TOKEN", "iam/nested//key", "iam/Nested/x/key", "iam", "other/x",
	} {
		if _, err := s.Estate().Create(t.Context(), name, "v", nodeA, "iam",
			clock); !errors.Is(err, secrets.ErrInvalidName) {
			t.Errorf("the engine's view wrote %q (%v)", name, err)
		}
	}
}

// A MISSING NAME IS ITS OWN SENTINEL, distinct from a keyring that no longer
// opens what it wrote. Collapsing them would make a dropped key look exactly
// like a variable nobody set.
func TestAMissingNameIsDistinctFromAnUnopenableOne(t *testing.T) {
	t.Parallel()
	s, _ := fleetStore(t, ring(t, "k1"))
	if _, err := s.Get(t.Context(), "NOTHING"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("err = %v, want secrets.ErrNotFound", err)
	}
}

// NO KEYRING IS A REFUSAL, not a store that quietly holds plaintext.
func TestWithoutAKeyringEveryWriteAndReadIsRefused(t *testing.T) {
	t.Parallel()
	s, _ := fleetStore(t, nil)
	if err := s.Set(t.Context(), "A", "v", sam, "cli", clock); !errors.Is(err, secrets.ErrNoKeyring) {
		t.Errorf("Set err = %v, want secrets.ErrNoKeyring", err)
	}
	if _, err := s.Get(t.Context(), "A"); !errors.Is(err, secrets.ErrNoKeyring) {
		t.Errorf("Get err = %v, want secrets.ErrNoKeyring", err)
	}
	if _, err := s.All(t.Context()); !errors.Is(err, secrets.ErrNoKeyring) {
		t.Errorf("All err = %v, want secrets.ErrNoKeyring", err)
	}
}

// A REKEY MOVES ONLY STALE ROWS AND REPORTS THE NAMES, which is what an
// operator confirms before retiring the old key. A count cannot answer "which
// one did not move".
func TestARekeyMovesTheStaleRowsAndNamesThem(t *testing.T) {
	t.Parallel()
	old := ring(t, "k1", "k2")
	s, fleet := fleetStore(t, old)
	mustSet(t, s, "A", "one")
	mustSet(t, s, "B", "two")

	// The same key material, with k2 active.
	rotated := fleetsecrets.New(fleet, ring(t, "k2", "k1"))
	moved, err := rotated.Rekey(t.Context(), "k2")
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if strings.Join(moved.Moved, ",") != "A,B" || moved.EngineKeys != 0 {
		t.Fatalf("moved = %v, want both names", moved)
	}
	// A SECOND RUN IS A NO-OP, which is what makes this safe in a deploy
	// script.
	again, err := rotated.Rekey(t.Context(), "k2")
	if err != nil || len(again.Moved) != 0 || again.EngineKeys != 0 {
		t.Fatalf("a second rekey moved %v (err %v), want nothing", again, err)
	}
	values, err := rotated.All(t.Context())
	if err != nil || values["A"] != "one" || values["B"] != "two" {
		t.Fatalf("after the rekey the values are %v (err %v)", values, err)
	}
}

// interleavedFleet runs one gesture between a rekey's read of every row and its
// first write — the window in which the pass holds values it read and has not
// yet re-sealed.
type interleavedFleet struct {
	coord.Fleet
	between func()
}

func (f *interleavedFleet) SecretValues(ctx context.Context) ([]coord.SecretRecord, error) {
	rows, err := f.Fleet.SecretValues(ctx)
	if f.between != nil {
		between := f.between
		f.between = nil
		between()
	}
	return rows, err
}

// A REKEY NEVER UNDOES A WRITE THAT LANDED WHILE IT RAN.
//
// It reads every row and re-seals each afterwards. Written back with a plain
// put, what it READ replaced whatever landed in between, and both of those are
// irreversible in the wrong direction:
//
//   - an operator's rotation was reverted to the credential they had just
//     replaced — the vendor had already been told to refuse it;
//   - a credential an operator deleted came back, live, on every node.
//
// The control is the row nobody touched, which still moves.
func TestARekeyNeverUndoesAWriteThatLandedWhileItRan(t *testing.T) {
	t.Parallel()
	old := ring(t, "k1", "k2")
	f := &interleavedFleet{Fleet: coordmem.NewFleet()}
	s := fleetsecrets.New(f, old)
	mustSet(t, s, "ROTATED", "the-old-credential")
	mustSet(t, s, "DELETED", "the-leaked-credential")
	mustSet(t, s, "UNTOUCHED", "still-here")

	rotated := fleetsecrets.New(f, ring(t, "k2", "k1"))
	f.between = func() {
		// UNDER THE OLD KEY, so the pass still judges the row stale when
		// it reads it again: what the rekey must not do is put back the
		// value it read before this.
		if err := s.Set(t.Context(), "ROTATED", "the-new-credential", sam,
			"cli", clock); err != nil {
			t.Errorf("rotate mid-pass: %v", err)
		}
		if _, err := s.Unset(t.Context(), "DELETED"); err != nil {
			t.Errorf("delete mid-pass: %v", err)
		}
	}
	moved, err := rotated.Rekey(t.Context(), "k2")
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if got, err := rotated.Get(t.Context(), "ROTATED"); err != nil ||
		got != "the-new-credential" {
		t.Fatalf("after the rekey the rotated credential reads %q (%v) — the "+
			"pass put back the value it read before the rotation", got, err)
	}
	if _, err := rotated.Get(t.Context(), "DELETED"); !errors.Is(err,
		secrets.ErrNotFound) {
		t.Fatalf("after the rekey the deleted credential answers %v — the "+
			"pass resurrected a credential an operator deleted", err)
	}
	if strings.Join(moved.Moved, ",") != "ROTATED,UNTOUCHED" || moved.EngineKeys != 0 {
		t.Errorf("the rekey reported %+v, want both surviving rows moved (the "+
			"rotated one judged again on what it holds now) and nothing else", moved)
	}
	if row, _, err := f.Secret(t.Context(), "ROTATED"); err != nil || row.KeyID != "k2" {
		t.Errorf("the rotated row is under %q (%v), want it re-sealed onto k2",
			row.KeyID, err)
	}
}

// A REKEY KEEPS WHO STORED EACH ROW. It re-seals a value it did not choose, and
// it used to stamp its own caller and `rekey` over every row it moved — so
// after a rotation every credential in the company read as set by whoever
// rotated the keyring, and the one record of who set each was gone. Mutation:
// write the moved row with fresh provenance and every field below fails.
func TestARekeyKeepsWhoStoredEachRow(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1", "k2"))
	dana := secrets.Author{Name: "dana", Kind: "operator",
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}
	if err := s.Set(t.Context(), "GL", "v", dana, "gitlab-provision", clock); err != nil {
		t.Fatal(err)
	}
	rotated := fleetsecrets.New(fleet, ring(t, "k2", "k1"))
	if moved, err := rotated.Rekey(t.Context(), "k2"); err != nil ||
		len(moved.Moved) != 1 {
		t.Fatalf("Rekey moved %+v (%v), want the one row", moved, err)
	}
	rows, err := rotated.List(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v (err %v)", rows, err)
	}
	row := rows[0]
	if row.KeyID != "k2" {
		t.Errorf("the row is under %q, want the rekey to have moved it", row.KeyID)
	}
	if got := (secrets.Author{Name: row.UpdatedBy, Kind: row.UpdatedByKind,
		OperatorID: row.OperatorID}); got != dana || row.Source != "gitlab-provision" ||
		!row.UpdatedAt.Equal(clock) {
		t.Errorf("after the rekey the row records %+v, %q at %s, want %+v, "+
			"gitlab-provision at %s", got, row.Source, row.UpdatedAt, dana, clock)
	}
}

// A REKEY ABORTS ON A ROW IT CANNOT OPEN, rather than reporting success over
// a secret that is now unreadable for ever — which is the state the operator
// is about to retire the old key on the strength of.
func TestARekeyRefusesToLeaveARowBehind(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1"))
	mustSet(t, s, "MINE", "value")
	stranger := fleetsecrets.New(fleet, ring(t, "k9"))
	if err := stranger.Set(t.Context(), "THEIRS", "other", sam, "cli", clock); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Rekey(t.Context(), "k1"); err == nil {
		t.Fatal("a rekey reported success while leaving a row under a key " +
			"this node cannot open")
	}
}

// AN ENGINE KEY IS MINTED ONLY WHERE NONE IS STORED.
//
// The blind-index key is minted by whichever node first needs it, and a fresh
// fleet boots every node at once: with a plain put, two minters each derived
// blinds under the key they wrote and the store kept one of them, so the loser's
// blinds matched nothing. The second create finds the first's key and leaves
// it, and says so.
func TestAnEngineKeyIsMintedOnlyWhereNoneIsStored(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	estate := fleetsecrets.New(coordmem.NewFleet(), ring(t, "k1")).Estate()
	const name = "iam/blind-index-key"
	if created, err := estate.Create(ctx, name, "first-key", nodeA, "iam",
		clock); err != nil || !created {
		t.Fatalf("the first create = (%v, %v)", created, err)
	}
	if created, err := estate.Create(ctx, name, "second-key", nodeB, "iam",
		clock); err != nil || created {
		t.Fatalf("the second create = (%v, %v), want it refused", created, err)
	}
	if got, _ := estate.Get(ctx, name); got != "first-key" {
		t.Fatalf("a refused create replaced the key: %q", got)
	}
}

// A SWEEP'S DELETE OF AN OPERATOR ROW IS CONDITIONED ON THE VERSION IT JUDGED.
//
// The org chart's orphan sweep decides a sealed value is nobody's and deletes
// it; a chart write re-sealing that same field in between writes the row
// again, and a plain delete would then destroy a value a row names. The
// conditional delete spares it — and still refuses the engine's own names,
// because this is the operator's view.
func TestAnOperatorRowIsDeletedOnlyAtTheVersionJudged(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := fleetStore(t, ring(t, "k1"))
	mustSet(t, s, "CHART_SEAT_ANA_EMAIL_0123456789", "ana@example.com")
	judged, found, err := s.Describe(ctx, "CHART_SEAT_ANA_EMAIL_0123456789")
	if err != nil || !found {
		t.Fatalf("Describe = (%v, %v)", found, err)
	}
	mustSet(t, s, "CHART_SEAT_ANA_EMAIL_0123456789", "ana.o@example.com")
	if removed, err := s.UnsetAt(ctx, "CHART_SEAT_ANA_EMAIL_0123456789",
		judged.Version); err != nil || removed {
		t.Fatalf("a delete at the version before a rewrite = (%v, %v), want "+
			"the row spared", removed, err)
	}
	now, _, _ := s.Describe(ctx, "CHART_SEAT_ANA_EMAIL_0123456789")
	if removed, err := s.UnsetAt(ctx, "CHART_SEAT_ANA_EMAIL_0123456789",
		now.Version); err != nil || !removed {
		t.Fatalf("a delete at the current version = (%v, %v), want it removed",
			removed, err)
	}
	if _, err := s.UnsetAt(ctx, "iam/blind-index-key", 1); !errors.Is(err,
		secrets.ErrReservedName) {
		t.Errorf("the operator's view deleted an engine name: %v", err)
	}
}

// A CREATE NEVER REPLACES, AND A HOLD MOVES NOTHING BUT THE VERSION.
//
// The org chart seals every value under a name no other write derives, so a
// value a live row names is never written over: the plain put it used let a
// write refused after its seal replace the credential the unchanged row still
// named. And a writer about to name a value again holds it, which moves the
// row's version and nothing else — the value, its author and when it last
// changed stay as they were — so a sweep that judged it at the old version
// can no longer delete it.
func TestACreateNeverReplacesAndAHoldMovesOnlyTheVersion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := fleetStore(t, ring(t, "k1"))
	const name = "CHART_SEAT_ANA_EMAIL_0123456789"
	if created, err := s.Create(ctx, name, "ana@example.com", nodeA, "chart",
		clock); err != nil || !created {
		t.Fatalf("the first create = (%v, %v)", created, err)
	}
	if created, err := s.Create(ctx, name, "mallory@example.com", nodeB, "chart",
		clock.Add(time.Hour)); err != nil || created {
		t.Fatalf("a second create = (%v, %v), want it refused", created, err)
	}
	before, _, err := s.Describe(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	value, held, err := s.Hold(ctx, name)
	if err != nil || !held || value != "ana@example.com" {
		t.Fatalf("Hold = (%q, %v, %v), want the value the first create wrote",
			value, held, err)
	}
	after, _, err := s.Describe(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version == before.Version {
		t.Error("a hold left the version where it was, so a sweep that judged " +
			"the value before it still deletes it under the row about to name it")
	}
	after.Version = before.Version
	if after != before {
		t.Errorf("a hold rewrote the row's metadata: %+v, want %+v — nothing "+
			"about the value changed, so nothing that says who set it and when "+
			"may either", after, before)
	}
	if removed, err := s.UnsetAt(ctx, name, before.Version); err != nil || removed {
		t.Errorf("a delete at the version judged before the hold = (%v, %v), "+
			"want the value spared", removed, err)
	}
	if _, held, err := s.Hold(ctx, "CHART_SEAT_NOBODY_EMAIL_0123456789"); err != nil || held {
		t.Errorf("a hold of a name nothing stores = (%v, %v), want not held", held, err)
	}
	for _, reserved := range []func() error{
		func() error {
			_, err := s.Create(ctx, "iam/blind-index-key", "x", nodeA, "chart", clock)
			return err
		},
		func() error { _, _, err := s.Hold(ctx, "iam/blind-index-key"); return err },
	} {
		if err := reserved(); !errors.Is(err, secrets.ErrReservedName) {
			t.Errorf("the operator's view reached an engine name: %v", err)
		}
	}
}

// ---- the migration --------------------------------------------------- //

func localStore(t *testing.T, cipher secrets.Cipher) *store.SecretValues {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db.SecretValues(cipher)
}

// THE MIGRATION COPIES AND THEN REMOVES, and the removal is the half that is
// easy to skip and cannot be: a local row left behind is read on every
// subsequent boot, so a later unset on the fleet would be silently undone by
// the stale copy resurfacing, forever.
func TestMigrationMovesTheRowsAndEmptiesTheLocalTable(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	local := localStore(t, cipher)
	fleet, _ := fleetStore(t, cipher)
	if err := local.Set(t.Context(), "GL", "glpat-x", sam, "cli", clock); err != nil {
		t.Fatal(err)
	}

	moved, err := fleetsecrets.Migrate(t.Context(), local, fleet, clock)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if strings.Join(moved, ",") != "GL" {
		t.Fatalf("moved = %v, want the one local row", moved)
	}
	values, err := fleet.All(t.Context())
	if err != nil || values["GL"] != "glpat-x" {
		t.Fatalf("the fleet holds %v (err %v)", values, err)
	}
	rows, err := local.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("the local table still holds %+v, so a later unset on the "+
			"fleet would be undone at the next boot", rows)
	}
}

// A NAME ALREADY ON THE FLEET IS NOT OVERWRITTEN. The local row is by
// definition the older write — the fleet is where every rotation since has
// landed — so copying it would resurrect a value an operator rotated away
// from on another node.
func TestMigrationNeverOverwritesTheFleetsValue(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	local := localStore(t, cipher)
	fleet, _ := fleetStore(t, cipher)
	if err := local.Set(t.Context(), "GL", "the-old-token", sam, "cli", clock); err != nil {
		t.Fatal(err)
	}
	mustSet(t, fleet, "GL", "the-rotated-token")

	moved, err := fleetsecrets.Migrate(t.Context(), local, fleet, clock)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(moved) != 0 {
		t.Errorf("moved = %v, want nothing: the fleet already had it", moved)
	}
	values, _ := fleet.All(t.Context())
	if values["GL"] != "the-rotated-token" {
		t.Fatalf("GL = %q, want the fleet's newer value untouched", values["GL"])
	}
	// AND THE STALE LOCAL COPY IS STILL REMOVED, or it would shadow the
	// fleet's row at every boot from now on.
	rows, _ := local.List(t.Context())
	if len(rows) != 0 {
		t.Fatalf("the shadowing local row survived: %+v", rows)
	}
}

// A ROW THAT COULD NOT BE COPIED IS NOT REMOVED. Deleting it would destroy a
// credential this node is the only holder of, and the first symptom would be
// a vendor 401 hours later on a node that never had the value.
func TestMigrationKeepsWhatItCouldNotCopy(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	local := localStore(t, cipher)
	if err := local.Set(t.Context(), "GL", "glpat-x", sam, "cli", clock); err != nil {
		t.Fatal(err)
	}
	// A fleet store with no keyring refuses every write.
	fleet := fleetsecrets.New(coordmem.NewFleet(), nil)

	if _, err := fleetsecrets.Migrate(t.Context(), local, fleet, clock); err == nil {
		t.Fatal("a migration that could write nothing reported success")
	}
	rows, err := local.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the local rows are %+v, want the uncopied one kept", rows)
	}
}

// THE ORIGINAL AUTHOR SURVIVES. "Who set this" is the question the provenance
// columns exist to answer, and answering it with the migration would erase
// the only record of it.
func TestMigrationPreservesWhoWroteTheRow(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	local := localStore(t, cipher)
	fleet, _ := fleetStore(t, cipher)
	dana := secrets.Author{Name: "dana", Kind: "operator",
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}
	if err := local.Set(t.Context(), "GL", "v", dana, "gitlab-provision", clock); err != nil {
		t.Fatal(err)
	}

	if _, err := fleetsecrets.Migrate(t.Context(), local, fleet, clock); err != nil {
		t.Fatal(err)
	}
	rows, err := fleet.List(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v (err %v)", rows, err)
	}
	// ALL THREE TRAVEL: the author, their kind and the credential beside
	// them, which a migration that carried only the name would lose.
	if got := (secrets.Author{Name: rows[0].UpdatedBy, Kind: rows[0].UpdatedByKind,
		OperatorID: rows[0].OperatorID}); got != dana {
		t.Errorf("the migrated row records %+v, want the original %+v", got, dana)
	}
	if rows[0].Source != fleetsecrets.MigrateSource {
		t.Errorf("source = %q, want %q so a reader can tell where the row "+
			"came from", rows[0].Source, fleetsecrets.MigrateSource)
	}
}

// A NODE WITH NOTHING LOCAL COSTS ONE READ AND WRITES NOTHING, which is the
// steady state on every boot after the first.
func TestMigrationOnAnEmptyTableDoesNothing(t *testing.T) {
	t.Parallel()
	cipher := ring(t, "k1")
	fleet, _ := fleetStore(t, cipher)
	moved, err := fleetsecrets.Migrate(t.Context(), localStore(t, cipher), fleet, clock)
	if err != nil || moved != nil {
		t.Fatalf("moved = %v, err = %v; want a silent no-op", moved, err)
	}
}

// A LOCAL ROW THIS NODE CANNOT OPEN IS KEPT, AND THE MIGRATION SAYS SO.
//
// It used to be a silent no-op for a node with no keyring, which kept its
// secrets in the environment. Every node holds a keyring now; what is left is
// a row sealed under a key this node's keyring no longer carries — dropped
// before the migration ran — and skipping it quietly would leave a credential
// where no peer can see it with nothing said, while removing it would destroy
// the only copy.
func TestAMigrationThatCannotOpenALocalRowKeepsItAndSaysSo(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.SecretValues(ring(t, "k1")).Set(t.Context(), "GL", "glpat-x", sam,
		"cli", clock); err != nil {
		t.Fatal(err)
	}
	// THE KEY THAT SEALED IT IS GONE from the ring this node now holds.
	local := db.SecretValues(ring(t, "k2"))
	fleet, _ := fleetStore(t, ring(t, "k2"))

	moved, err := fleetsecrets.Migrate(context.Background(), local, fleet, clock)
	if err == nil {
		t.Fatalf("a migration that could open nothing reported success, moving %v", moved)
	}
	rows, err := local.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "GL" {
		t.Fatalf("the local rows are %+v, want the unopened one kept", rows)
	}
	if onFleet, err := fleet.List(t.Context()); err != nil || len(onFleet) != 0 {
		t.Errorf("the fleet holds %+v (err %v), want nothing copied", onFleet, err)
	}
}

// BOTH STORES KEY ON AN ENVIRONMENT-VARIABLE NAME, and this is the fleet's
// half of that one rule. A row stored under a name the ${VAR} grammar cannot
// name is sealed, listed and resolved by nothing, so the write reports a
// success that no config can ever act on. Its twin is
// TestASecretNameOutsideTheReferenceGrammarIsRefused in internal/store.
func TestASecretNameOutsideTheReferenceGrammarIsRefused(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1"))
	for _, name := range []string{"", "gitlab token", "gitlab-token", "9LIVES"} {
		err := s.Set(t.Context(), name, "value", sam, "cli", clock)
		if !errors.Is(err, secrets.ErrInvalidName) {
			t.Errorf("Set(%q) = %v, want secrets.ErrInvalidName", name, err)
		}
	}
	rows, err := fleet.SecretValues(t.Context())
	if err != nil {
		t.Fatalf("SecretValues: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused write left %d row(s) on the fleet", len(rows))
	}
	// AND THE ORDINARY NAMES STILL PASS, or the guard is just an outage.
	mustSet(t, s, "GITLAB_TOKEN", "glpat-not-a-real-token")
	mustSet(t, s, "_leading_underscore", "value")
}
