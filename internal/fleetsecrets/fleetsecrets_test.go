package fleetsecrets_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/secrets"
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
	if err := s.Set(t.Context(), name, value, sam, secrets.SourceAPI, clock); err != nil {
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
	if err := stranger.Set(t.Context(), "FOREIGN", "other", sam, secrets.SourceAPI, clock); err != nil {
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

// A READ OF SEVERAL NAMES OPENS THOSE AND NO OTHERS.
//
// It is how a CLI run resolving one company document through a running node
// reads the variables its command reads that the node lists, so the answer
// carries exactly the names asked for that are held — an absent name absent,
// never empty — and the read fails closed on a named row it cannot open, while
// a row nobody named that it cannot open does not fail a read that never
// needed it.
func TestAReadOfSeveralNamesOpensThoseAndNoOthers(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1"))
	mustSet(t, s, "GITLAB_TOKEN", "glpat")
	mustSet(t, s, "SLACK_TOKEN", "xoxb")
	mustSet(t, s, "UNASKED", "not-for-this-reader")
	stranger := fleetsecrets.New(fleet, ring(t, "k9"))
	if err := stranger.Set(t.Context(), "FOREIGN", "sealed-elsewhere", sam,
		secrets.SourceAPI, clock); err != nil {
		t.Fatal(err)
	}

	values, err := s.Values(t.Context(), []string{"GITLAB_TOKEN", "SLACK_TOKEN",
		"NEVER_SET", "GITLAB_TOKEN"})
	if err != nil {
		t.Fatalf("a row nobody asked for failed the read: %v", err)
	}
	if len(values) != 2 || values["GITLAB_TOKEN"] != "glpat" || values["SLACK_TOKEN"] != "xoxb" {
		t.Fatalf("values = %v, want exactly the two held names asked for", values)
	}
	if _, present := values["NEVER_SET"]; present {
		t.Error("a name nothing holds came back present, so it would resolve " +
			"to the empty string instead of falling through")
	}

	if _, err := s.Values(t.Context(), []string{"GITLAB_TOKEN", "FOREIGN"}); err == nil ||
		!strings.Contains(err.Error(), "FOREIGN") {
		t.Errorf("a named row this keyring cannot open answered %v, want a "+
			"refusal naming it", err)
	}
	if _, err := s.Values(t.Context(), []string{"iam/blind-index-key"}); !errors.Is(err,
		secrets.ErrReservedName) {
		t.Errorf("a read naming the engine's own key answered %v, want "+
			"ErrReservedName", err)
	}
	if _, err := fleetsecrets.New(fleet, nil).Values(t.Context(),
		[]string{"GITLAB_TOKEN"}); !errors.Is(err, secrets.ErrNoKeyring) {
		t.Errorf("a store with no keyring answered %v, want ErrNoKeyring", err)
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
	if err := s.Set(t.Context(), key, "overwritten", sam, secrets.SourceAPI, clock); !errors.Is(err,
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
	if created, err := estate.Create(t.Context(), name, value, nodeA, secrets.SourceIAM,
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
		if _, err := s.Estate().Create(t.Context(), name, "v", nodeA, secrets.SourceIAM,
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
	if err := s.Set(t.Context(), "A", "v", sam, secrets.SourceAPI, clock); !errors.Is(err, secrets.ErrNoKeyring) {
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
			secrets.SourceAPI, clock); err != nil {
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
	if err := s.Set(t.Context(), "GL", "v", dana, secrets.SourceProvision, clock); err != nil {
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
		OperatorID: row.OperatorID}); got != dana || row.Source != secrets.SourceProvision ||
		!row.UpdatedAt.Equal(clock) {
		t.Errorf("after the rekey the row records %+v, %q at %s, want %+v, "+
			"%q at %s", got, row.Source, row.UpdatedAt, dana, secrets.SourceProvision, clock)
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
	if err := stranger.Set(t.Context(), "THEIRS", "other", sam, secrets.SourceAPI, clock); err != nil {
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
	if created, err := estate.Create(ctx, name, "first-key", nodeA, secrets.SourceIAM,
		clock); err != nil || !created {
		t.Fatalf("the first create = (%v, %v)", created, err)
	}
	if created, err := estate.Create(ctx, name, "second-key", nodeB, secrets.SourceIAM,
		clock); err != nil || created {
		t.Fatalf("the second create = (%v, %v), want it refused", created, err)
	}
	if got, _ := estate.Get(ctx, name); got != "first-key" {
		t.Fatalf("a refused create replaced the key: %q", got)
	}
}

// THE STORE KEYS ON AN ENVIRONMENT-VARIABLE NAME. A row stored under a name
// the ${VAR} grammar cannot name is sealed, listed and resolved by nothing, so
// the write reports a success that no config can ever act on.
func TestASecretNameOutsideTheReferenceGrammarIsRefused(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1"))
	for _, name := range []string{"", "gitlab token", "gitlab-token", "9LIVES"} {
		err := s.Set(t.Context(), name, "value", sam, secrets.SourceAPI, clock)
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

// A ROW RECORDS THE WORD OF THE PATH THAT WROTE IT, AND NO OTHER. The source
// was a word the caller chose, so a row could claim any path it liked; now
// every path stamps its own constant and the store refuses anything else —
// the words a caller used to send included — before a byte is sealed, on the
// operator's view and the engine's alike. Mutation: drop either CheckSource
// call and its half of this fails.
func TestAWriteWithAWordNoPathStampsIsRefused(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1"))
	for _, word := range []secrets.Source{"", "operator", "gitlab-provision", "rekey"} {
		if err := s.Set(t.Context(), "GITLAB_TOKEN", "glpat", sam, word, clock); err == nil {
			t.Errorf("Set stamped %q, which no write path stamps", word)
		}
		if created, err := s.Estate().Create(t.Context(), "iam/blind-index-key",
			"the-key", nodeA, word, clock); err == nil || created {
			t.Errorf("Create stamped %q (created %v), which no write path stamps",
				word, created)
		}
	}
	rows, err := fleet.SecretValues(t.Context())
	if err != nil {
		t.Fatalf("SecretValues: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused write left %d row(s) on the fleet", len(rows))
	}
	// AND THE PATHS' OWN WORDS PASS, or the guard is just an outage.
	mustSet(t, s, "GITLAB_TOKEN", "glpat")
	mustCreate(t, s.Estate(), "iam/blind-index-key", "the-key")
}

// A VALUE PAST THE SIZE EVERY STORE KEEPS IS REFUSED ON THE FLEET, BY EITHER
// VIEW, AND NOTHING IS WRITTEN.
//
// The fleet's store is what every node resolves through and what both reveals
// read back, to a length sized from [secrets.MaxValueBytes]; a row past it can
// be longer than a reveal gives back, a credential possibly unreadable through
// the node. The API route bounds its body to that size, but a command that
// writes this store directly, with its node's engine stopped, has no route in
// front of it: the store is the one place that holds every writer to the
// limit. A value exactly at the limit is stored.
func TestAValuePastTheSizeEveryStoreKeepsIsRefusedOnTheFleet(t *testing.T) {
	t.Parallel()
	s, fleet := fleetStore(t, ring(t, "k1"))
	past := strings.Repeat("&", secrets.MaxValueBytes+1)
	if err := s.Set(t.Context(), "PAST_IT", past, sam, secrets.SourceAPI, clock); !errors.Is(err, secrets.ErrValueTooLarge) {
		t.Errorf("Set of a value a byte past the limit answered %v, want ErrValueTooLarge", err)
	}
	if created, err := s.Estate().Create(t.Context(), "iam/blind-index-key", past,
		nodeA, secrets.SourceIAM, clock); !errors.Is(err, secrets.ErrValueTooLarge) || created {
		t.Errorf("Create of a value a byte past the limit answered %v (created %v)", err, created)
	}
	rows, err := fleet.SecretValues(t.Context())
	if err != nil {
		t.Fatalf("SecretValues: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused write left %d row(s) on the fleet", len(rows))
	}
	atLimit := past[1:]
	mustSet(t, s, "AT_THE_LIMIT", atLimit)
	if got, err := s.Get(t.Context(), "AT_THE_LIMIT"); err != nil || got != atLimit {
		t.Errorf("the value at the limit read back as %d bytes, %v", len(got), err)
	}
}

// A CREATE-ONLY WRITE OF AN OPERATOR'S SECRET NEVER SEALS OVER ONE STORED.
//
// A credential sent with `If-None-Match: *` is tested for up to two minutes
// before it is sealed; a value another writer stored meanwhile is kept, and
// the create says it lost.
func TestACreateNeverSealsOverAStoredSecret(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, _ := fleetStore(t, ring(t, "k1"))
	mustSet(t, store, "ANTHROPIC_API_KEY", "theirs")
	if created, err := store.Create(ctx, "ANTHROPIC_API_KEY", "mine", nodeA, secrets.SourceModels,
		clock, ""); err != nil || created {
		t.Fatalf("a create over a stored value = (%v, %v), want it refused", created, err)
	}
	if got, _ := store.Get(ctx, "ANTHROPIC_API_KEY"); got != "theirs" {
		t.Errorf("a refused create replaced the value: %q", got)
	}
	if created, err := store.Create(ctx, "OPENAI_API_KEY", "mine", nodeA, secrets.SourceModels,
		clock, ""); err != nil || !created {
		t.Fatalf("a create of a new name = (%v, %v)", created, err)
	}
	if _, err := store.Create(ctx, "iam/blind-index-key", "x", nodeA, secrets.SourceModels,
		clock, ""); err == nil {
		t.Error("a create of the engine's own name was not refused")
	}
}

// A CREATE'S RETRY RE-SEALS ONLY THE ROWS ITS OWN KEY SEALED, AND READS NONE.
//
// Reclaim writes the value it is handed over a row the keyed create sealed,
// whatever that row holds, and refuses a row any other write left: another
// key's, a plain write's (which clears the key), or none. OpOf answers the key
// and never the value.
func TestACreatesRetryReclaimsOnlyItsOwnRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, _ := fleetStore(t, ring(t, "k1"))
	if created, err := store.Create(ctx, "GATEWAY_KEY", "first", nodeA, secrets.SourceModels,
		clock, "op-1"); err != nil || !created {
		t.Fatalf("the keyed create = (%v, %v)", created, err)
	}
	if op, found, err := store.OpOf(ctx, "GATEWAY_KEY"); err != nil || !found || op != "op-1" {
		t.Fatalf("OpOf = (%q, %v, %v), want op-1", op, found, err)
	}
	if wrote, err := store.Reclaim(ctx, "GATEWAY_KEY", "corrected", nodeA, secrets.SourceModels,
		clock, "op-2"); err != nil || wrote {
		t.Errorf("another key's reclaim = (%v, %v), want it refused", wrote, err)
	}
	if wrote, err := store.Reclaim(ctx, "GATEWAY_KEY", "corrected", nodeA, secrets.SourceModels,
		clock, "op-1"); err != nil || !wrote {
		t.Fatalf("its own key's reclaim = (%v, %v)", wrote, err)
	}
	if got, _ := store.Get(ctx, "GATEWAY_KEY"); got != "corrected" {
		t.Errorf("after the reclaim the value is %q, want the retry's", got)
	}
	// A PLAIN WRITE CLEARS THE KEY: the row is that write's now.
	mustSet(t, store, "GATEWAY_KEY", "rotated")
	if op, _, _ := store.OpOf(ctx, "GATEWAY_KEY"); op != "" {
		t.Errorf("a plain write left the key %q on the row", op)
	}
	if wrote, _ := store.Reclaim(ctx, "GATEWAY_KEY", "late", nodeA, secrets.SourceModels,
		clock, "op-1"); wrote {
		t.Error("a retry re-sealed over a plain write that followed its create")
	}
	if wrote, _ := store.Reclaim(ctx, "ABSENT", "x", nodeA, secrets.SourceModels, clock, "op-1"); wrote {
		t.Error("a reclaim wrote a row nothing created")
	}
}
