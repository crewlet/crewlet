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
package fleetsecrets

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
)

var log = logging.Get("secrets.fleet")

// Store is the fleet's secret values, sealed under one node's keyring.
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

// Set seals a value and writes it for the whole fleet.
func (s *Store) Set(ctx context.Context, name, value, by, source string, now time.Time) error {
	if s == nil || s.cipher == nil {
		return secrets.ErrNoKeyring
	}
	if name == "" {
		return errors.New("fleetsecrets: a secret needs a name")
	}
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

// All unseals every value, for the resolver's boot snapshot.
//
// ONE ROUND TRIP, because ${VAR} expansion happens per role, per provider,
// per MCP server — the engine takes a snapshot and resolves from it rather
// than putting the fleet's store on the path of every config read.
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

// List reports what is stored, without opening anything.
//
// NO VALUES, ever — this is what an operator reads to answer "is X set", and
// it deliberately does not need the keyring, so a node that cannot decrypt can
// still say what exists.
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
		// The envelope is dropped on the way out rather than left for a
		// caller to be careful with. A listing is printed, and the one
		// thing that must never reach a terminal is the ciphertext.
		out = append(out, secrets.Record{
			Name: row.Name, KeyID: row.KeyID, UpdatedAt: row.UpdatedAt,
			UpdatedBy: row.UpdatedBy, Source: row.Source,
		})
	}
	slices.SortFunc(out, func(a, b secrets.Record) int { return cmp.Compare(a.Name, b.Name) })
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
	rec, found, err := s.fleet.Secret(ctx, name)
	if err != nil {
		return secrets.Record{}, false, fmt.Errorf("fleetsecrets: read %s: %w", name, err)
	}
	if !found {
		return secrets.Record{}, false, nil
	}
	return secrets.Record{
		Name: rec.Name, KeyID: rec.KeyID, UpdatedAt: rec.UpdatedAt,
		UpdatedBy: rec.UpdatedBy, Source: rec.Source,
	}, true, nil
}

// Unset removes a value, reporting whether it was there.
func (s *Store) Unset(ctx context.Context, name string) (bool, error) {
	if s == nil {
		return false, secrets.ErrNoKeyring
	}
	removed, err := s.fleet.DeleteSecret(ctx, name)
	if err != nil {
		return false, fmt.Errorf("fleetsecrets: unset %s: %w", name, err)
	}
	return removed, nil
}

// Rekey re-seals every row this node can open under the active key.
//
// Returns the names it moved. A row already under the active key is left
// alone, so a second run reports nothing and costs one read — which is what
// makes this safe to put in a deploy script.
func (s *Store) Rekey(ctx context.Context, activeKeyID, by string, now time.Time) ([]string, error) {
	if s == nil || s.cipher == nil {
		return nil, secrets.ErrNoKeyring
	}
	rows, err := s.fleet.SecretValues(ctx)
	if err != nil {
		return nil, fmt.Errorf("fleetsecrets: read the secrets: %w", err)
	}
	var moved []string
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
			// of.
			return nil, fmt.Errorf("fleetsecrets: open %s for rekey: %w",
				row.Name, err)
		}
		if err := s.Set(ctx, row.Name, value, by, "rekey", now); err != nil {
			// The names moved so far come back WITH the error: a
			// partial rekey is a fact an operator has to act on, and
			// discarding the list would leave them re-running a pass
			// with no idea which rows already moved.
			slices.Sort(moved)
			return moved, err
		}
		moved = append(moved, row.Name)
	}
	slices.Sort(moved)
	return moved, nil
}
