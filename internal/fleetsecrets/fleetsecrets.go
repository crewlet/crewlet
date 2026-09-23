// Package fleetsecrets is the company's credential store on the coordination
// KV — the same rows every node reads, sealed with the Tier A keyring.
//
// # Why it is not internal/store's
//
// It was, and that was the last piece of company-wide state living somewhere
// only one node could see. `crewlet secrets set` reached exactly the
// node whose Tier A file it was pointed at; every peer kept what it booted
// with, and nothing failed until a seat landed on one of them and a vendor
// rejected a credential the operator believed they had rotated.
//
// The company CONFIG already travels this way — the activation plane writes a
// payload sealed with this very keyring into this very bucket family, and a
// company document may itself carry credentials inline — so the secret store
// being per node was an asymmetry rather than a safeguard.
//
// # This package owns the KEY; coordination owns the BYTES
//
// Every value is sealed here, before it is handed over, and opened here after
// it comes back. Coordination stores an envelope whose key it does not have,
// which is what makes a shared store safe to put credentials in: a peer that
// can read the bucket learns which names exist and when they changed, not
// what they are.
//
// The name is bound in as associated data, so an envelope moved to another
// row fails to open rather than silently impersonating a different secret —
// the same binding [store.SecretValues] uses, and the reason both can read
// rows the other wrote during a migration.
//
// # Two views over one bucket: the operator's, and the engine's own
//
// The bucket also holds the ENGINE's key material — a person's data key, an
// OIDC session's refresh token, the identity estate's blind-index key — under
// path-shaped names no `${VAR}` can reach ([secrets.Reserved]). [Store] is the
// OPERATOR's view: it neither lists, snapshots, reads, writes nor deletes a
// reserved row, and says so by name ([secrets.ErrReservedName]). [Estate] is
// the engine's: it addresses reserved rows and nothing else. The one gesture
// that crosses is a REKEY, which re-seals every row under the active key
// because the keyring is one keyring — and reports the engine's rows as a count
// rather than by name ([Rekeyed]).
package fleetsecrets

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
)

var log = logging.Get("secrets.fleet")

// Store is the fleet's secret values, sealed under one node's keyring, as the
// OPERATOR's surfaces address them.
type Store struct {
	fleet  coord.Secrets
	cipher secrets.Cipher
}

// New builds a store over a coordination backend and a keyring.
//
// A nil cipher is a node with no keyring, which every method answers with
// [secrets.ErrNoKeyring] rather than by resolving to plaintext: an
// operator who configured no encryption gets the environment, not a store
// that quietly holds credentials in the clear.
func New(fleet coord.Secrets, cipher secrets.Cipher) *Store {
	if fleet == nil {
		return nil
	}
	return &Store{fleet: fleet, cipher: cipher}
}

// operatorName refuses a name that is the engine's own before anything reads,
// writes or deletes it.
func operatorName(name string) error {
	if secrets.Reserved(name) {
		return fmt.Errorf("%w: %s", secrets.ErrReservedName, name)
	}
	return nil
}

// Set seals a value and writes it for the whole fleet.
func (s *Store) Set(ctx context.Context, name, value, by, source string, now time.Time) error {
	if s == nil || s.cipher == nil {
		return secrets.ErrNoKeyring
	}
	if err := operatorName(name); err != nil {
		return err
	}
	// THE NAME IS THE KEY SPACE, checked before anything is sealed. See
	// [secrets.CheckName]: a row nothing can reference is worse than a
	// refusal, because it reports success.
	if err := secrets.CheckName(name); err != nil {
		return err
	}
	return s.put(ctx, name, value, by, source, now)
}

// put seals a value under its own name and writes it, once the caller's view
// has established the name is one it may write.
func (s *Store) put(ctx context.Context, name, value, by, source string, now time.Time) error {
	sealed, err := s.cipher.Encrypt(value, secrets.AADForVar(name))
	if err != nil {
		return fmt.Errorf("fleetsecrets: seal %s: %w", name, err)
	}
	keyID, ok := secrets.EnvelopeKeyID(sealed)
	if !ok {
		return fmt.Errorf("fleetsecrets: seal %s: the cipher produced no key id", name)
	}
	return s.fleet.PutSecret(ctx, coord.SecretRecord{
		Name: name, Value: sealed, KeyID: keyID,
		UpdatedAt: now.UTC(), UpdatedBy: by, Source: source,
	})
}

// Get unseals one value.
//
// The error says WHICH failure it was and never carries the value or the
// envelope: a decrypt error that echoed its input would put ciphertext into a
// log that a keyring might later open.
func (s *Store) Get(ctx context.Context, name string) (string, error) {
	if s == nil || s.cipher == nil {
		return "", secrets.ErrNoKeyring
	}
	if err := operatorName(name); err != nil {
		return "", err
	}
	return s.get(ctx, name)
}

// get opens one row, once the caller's view has established it may.
func (s *Store) get(ctx context.Context, name string) (string, error) {
	rec, found, err := s.fleet.Secret(ctx, name)
	if err != nil {
		return "", fmt.Errorf("fleetsecrets: read %s: %w", name, err)
	}
	if !found {
		return "", fmt.Errorf("%w: %s", secrets.ErrNotFound, name)
	}
	value, err := s.cipher.Decrypt(rec.Value, secrets.AADForVar(name))
	if err != nil {
		return "", fmt.Errorf("fleetsecrets: open %s: %w", name, err)
	}
	return value, nil
}

// All unseals every OPERATOR value, for the resolver's boot snapshot.
//
// ONE ROUND TRIP, because ${VAR} expansion happens per role, per provider,
// per MCP server — the engine takes a snapshot and resolves from it rather
// than putting the fleet's store on the path of every config read.
//
// THE ENGINE'S ROWS ARE NOT OPENED, and that is the other half of the reserved
// namespace: no reference can name one, so decrypting every person's key and
// every session's refresh token into each node's resolver on every apply was
// all exposure and no use.
//
// It FAILS CLOSED on the first row it cannot open, exactly as the local store
// does. A partial snapshot is the worst outcome available: the names that are
// missing resolve to whatever the environment happens to hold, which is the
// stale-.env shadowing this whole mechanism exists to prevent, and it would
// happen silently. A row this node cannot open is not a fleet mid-rotation —
// every node runs the same Tier A keyring, because the config plane seals its
// payload with that same keyring and a node that cannot open one cannot apply
// a config either — it is a key that was dropped from the keyring, and that
// is a boot failure to read rather than a credential to lose quietly.
func (s *Store) All(ctx context.Context) (map[string]string, error) {
	if s == nil || s.cipher == nil {
		return nil, secrets.ErrNoKeyring
	}
	rows, err := s.fleet.SecretValues(ctx)
	if err != nil {
		return nil, fmt.Errorf("fleetsecrets: read the secrets: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		if secrets.Reserved(row.Name) {
			continue
		}
		value, err := s.cipher.Decrypt(row.Value, secrets.AADForVar(row.Name))
		if err != nil {
			// The NAME, never the envelope: an error that echoed its
			// input would put ciphertext into a log that a keyring
			// might later open.
			return nil, fmt.Errorf("fleetsecrets: open %s: %w", row.Name, err)
		}
		out[row.Name] = value
	}
	return out, nil
}

// List reports what the operator has stored, without opening anything.
//
// NO VALUES, ever — this is what an operator reads to answer "is X set", and
// it deliberately does not need the keyring, so a node that cannot decrypt can
// still say what exists. And no engine row: [Store.EngineKeys] counts those.
func (s *Store) List(ctx context.Context) ([]secrets.Record, error) {
	if s == nil {
		return nil, secrets.ErrNoKeyring
	}
	rows, err := s.fleet.SecretValues(ctx)
	if err != nil {
		return nil, fmt.Errorf("fleetsecrets: list the secrets: %w", err)
	}
	out := make([]secrets.Record, 0, len(rows))
	for _, row := range rows {
		if secrets.Reserved(row.Name) {
			continue
		}
		// The envelope is dropped on the way out rather than left for a
		// caller to be careful with. A listing is printed, and the one
		// thing that must never reach a terminal is the ciphertext.
		out = append(out, record(row))
	}
	slices.SortFunc(out, func(a, b secrets.Record) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// EngineKeys counts the engine's own rows, per keyring key, naming none.
//
// NO KEYRING NEEDED, like [Store.List]: what it answers is whether a rotation
// has moved them, and the key id rides beside each envelope for exactly that.
func (s *Store) EngineKeys(ctx context.Context) (secrets.EngineKeys, error) {
	if s == nil {
		return secrets.EngineKeys{}, secrets.ErrNoKeyring
	}
	rows, err := s.fleet.SecretValues(ctx)
	if err != nil {
		return secrets.EngineKeys{}, fmt.Errorf("fleetsecrets: count the "+
			"engine's keys: %w", err)
	}
	var out secrets.EngineKeys
	for _, row := range rows {
		if !secrets.Reserved(row.Name) {
			continue
		}
		if out.ByKey == nil {
			out.ByKey = map[string]int{}
		}
		out.Total++
		out.ByKey[row.KeyID]++
	}
	return out, nil
}

// Describe reports one row's metadata without opening it.
//
// NO KEYRING NEEDED, which is the point: "is X set, and when did it last
// change" is the question asked overwhelmingly more often than "what is X",
// and answering it must not require the ability to decrypt — nor put a
// plaintext credential one typo away from the caller that asked.
func (s *Store) Describe(ctx context.Context, name string) (secrets.Record, bool, error) {
	if s == nil {
		return secrets.Record{}, false, secrets.ErrNoKeyring
	}
	if err := operatorName(name); err != nil {
		return secrets.Record{}, false, err
	}
	rec, found, err := s.fleet.Secret(ctx, name)
	if err != nil {
		return secrets.Record{}, false, fmt.Errorf("fleetsecrets: read %s: %w", name, err)
	}
	if !found {
		return secrets.Record{}, false, nil
	}
	return record(rec), true, nil
}

// Unset removes a value, reporting whether it was there.
func (s *Store) Unset(ctx context.Context, name string) (bool, error) {
	if s == nil {
		return false, secrets.ErrNoKeyring
	}
	if err := operatorName(name); err != nil {
		return false, err
	}
	return s.unset(ctx, name)
}

// unset deletes one row, once the caller's view has established it may.
func (s *Store) unset(ctx context.Context, name string) (bool, error) {
	removed, err := s.fleet.DeleteSecret(ctx, name)
	if err != nil {
		return false, fmt.Errorf("fleetsecrets: unset %s: %w", name, err)
	}
	return removed, nil
}

// Rekeyed is what one rekey moved.
type Rekeyed struct {
	// Moved names the OPERATOR's rows re-sealed onto the active key.
	Moved []string

	// EngineKeys is how many of the engine's own rows were, named by
	// nobody for [secrets.EngineKeys]' reason.
	EngineKeys int
}

// Rekey re-seals every row this node can open under the active key — the
// operator's AND the engine's, because the keyring is one keyring: an engine
// row left under a retired key is every person's name, every refresh token and
// the blind-index key unreadable the moment that key is dropped.
//
// A row already under the active key is left alone, so a second run reports
// nothing and costs one read — which is what makes this safe to put in a
// deploy script.
func (s *Store) Rekey(ctx context.Context, activeKeyID, by string, now time.Time) (Rekeyed, error) {
	var out Rekeyed
	if s == nil || s.cipher == nil {
		return out, secrets.ErrNoKeyring
	}
	rows, err := s.fleet.SecretValues(ctx)
	if err != nil {
		return out, fmt.Errorf("fleetsecrets: read the secrets: %w", err)
	}
	for _, row := range rows {
		if row.KeyID == activeKeyID {
			continue
		}
		value, err := s.cipher.Decrypt(row.Value, secrets.AADForVar(row.Name))
		if err != nil {
			// ABORTS THE WHOLE PASS, as the local store's does. A row
			// this keyring cannot open is a key that was dropped from
			// the config, and moving the others while leaving it would
			// report a successful rekey over a secret that is now
			// unreadable for ever — which is precisely the state the
			// operator is about to retire the old key on the strength
			// of. An engine row is NAMED by its owner rather than in
			// full, for the listing's reason.
			slices.Sort(out.Moved)
			return out, fmt.Errorf("fleetsecrets: open %s for rekey: %w",
				displayName(row.Name), err)
		}
		if err := s.put(ctx, row.Name, value, by, "rekey", now); err != nil {
			// The names moved so far come back WITH the error: a
			// partial rekey is a fact an operator has to act on, and
			// discarding the list would leave them re-running a pass
			// with no idea which rows already moved.
			slices.Sort(out.Moved)
			return out, err
		}
		if secrets.Reserved(row.Name) {
			out.EngineKeys++
			continue
		}
		out.Moved = append(out.Moved, row.Name)
	}
	slices.Sort(out.Moved)
	return out, nil
}

// displayName is a row's name as an operator surface may print it: the whole
// name for the operator's own, and the owner alone for the engine's.
func displayName(name string) string {
	if !secrets.Reserved(name) {
		return name
	}
	owner, _, _ := strings.Cut(name, "/")
	return "one of the engine's own " + owner + " keys"
}

// record is a row's metadata, with the envelope dropped.
func record(row coord.SecretRecord) secrets.Record {
	return secrets.Record{
		Name: row.Name, KeyID: row.KeyID, UpdatedAt: row.UpdatedAt,
		UpdatedBy: row.UpdatedBy, Source: row.Source,
	}
}

// Estate is the ENGINE's view of the same bucket: its own key material, under
// names in [secrets.CheckEstateName]'s grammar, and nothing else.
//
// It exists so that the one party that may address a reserved row does so
// through a type no operator surface is handed. Every method refuses a name
// outside the engine's namespace, so an estate caller cannot reach an
// operator's credential either.
type Estate struct{ store *Store }

// Estate is this store's engine view.
func (s *Store) Estate() *Estate {
	if s == nil {
		return nil
	}
	return &Estate{store: s}
}

// Get opens one of the engine's own rows.
func (e *Estate) Get(ctx context.Context, name string) (string, error) {
	if e == nil || e.store.cipher == nil {
		return "", secrets.ErrNoKeyring
	}
	if err := secrets.CheckEstateName(name); err != nil {
		return "", err
	}
	return e.store.get(ctx, name)
}

// Set seals and writes one of the engine's own rows.
func (e *Estate) Set(ctx context.Context, name, value, by, source string, now time.Time) error {
	if e == nil || e.store.cipher == nil {
		return secrets.ErrNoKeyring
	}
	if err := secrets.CheckEstateName(name); err != nil {
		return err
	}
	return e.store.put(ctx, name, value, by, source, now)
}

// Unset destroys one of the engine's own rows, reporting whether it was there.
//
// NO KEYRING NEEDED: destroying a removed person's key is what finishes their
// off-boarding, and a node that cannot decrypt anything must still be able to.
func (e *Estate) Unset(ctx context.Context, name string) (bool, error) {
	if e == nil {
		return false, secrets.ErrNoKeyring
	}
	if err := secrets.CheckEstateName(name); err != nil {
		return false, err
	}
	return e.store.unset(ctx, name)
}

// Keys reports every engine row under prefix — its name, its sealing key and
// when and by whom it was last written — sorted by name, without opening
// anything.
//
// NO KEYRING NEEDED, for [Estate.Unset]'s reason: what reads this is the duty
// that finishes a removal and collects a key nobody owns, and the finding half
// must not be the one thing a node with no keyring cannot do. The WRITE TIME
// is what that duty ages a key by — a key nobody owns yet may be one an
// enrolment minted a second ago — and it rides beside every envelope, so it
// costs nothing to hand over. NEVER THE ENVELOPE: a caller that needs a value
// asks for it by name.
func (e *Estate) Keys(ctx context.Context, prefix string) ([]secrets.Record, error) {
	if e == nil {
		return nil, secrets.ErrNoKeyring
	}
	if !secrets.Reserved(prefix) {
		return nil, fmt.Errorf("%w: %q is not a prefix in the engine's own "+
			"namespace", secrets.ErrInvalidName, prefix)
	}
	rows, err := e.store.fleet.SecretValues(ctx)
	if err != nil {
		return nil, fmt.Errorf("fleetsecrets: list the engine's keys under %q: %w",
			prefix, err)
	}
	var out []secrets.Record
	for _, row := range rows {
		if strings.HasPrefix(row.Name, prefix) {
			out = append(out, record(row))
		}
	}
	slices.SortFunc(out, func(a, b secrets.Record) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}
