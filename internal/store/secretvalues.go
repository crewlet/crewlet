package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/crewlet/crewlet/internal/secrets"
)

// SecretValues is the encrypted secret store, keyed by the ${VAR} name a
// config value references.
//
// # It is this NODE's, and that is the anomaly rather than the design
//
// Six other kinds of shared state moved onto the coordination KV (d-201)
// precisely because a node's own database is invisible to its peers, and the
// company config — which may itself carry credentials — travels that path
// too, sealed with the very cipher this table uses. A rotation here reaches
// one node; every other node keeps what it booted with.
//
// decisions/203 records that secrets should follow config onto the KV and
// what that takes. Until they do, a rotation is per node and the environment
// fallback is the fleet-wide path — see docs/concepts/secret-store.md.
//
// # It is the companion to company_config, not a duplicate of it
//
// Rotation is an UPDATE of one row here. Writing the literal into the config
// instead would archive the OLD secret forever, because every revision is an
// immutable copy and revisions are never scrubbed. And one name can have
// many pointers: a bot token referenced from an integration block and from a
// per-role MCP env is ONE credential with two readers, which keying by var
// name keeps as one row.
//
// # No plaintext mode, unlike company_config
//
// A keyring is REQUIRED to read or write a row. There is no legacy corpus to
// stay compatible with, and a dedicated secret store that can hold
// unencrypted secrets is a footgun with no upside.
//
// # Reads fail CLOSED
//
// Everything else in this package that cannot reach its data returns the
// safe answer and carries on. This one raises. An unreadable secret
// resolving to "" does not fail here — it becomes an empty Bearer token
// hours later and somewhere else entirely, on a request whose 401 names the
// vendor rather than this store.
type SecretValues struct {
	db     *DB
	cipher secrets.Cipher
}

// SecretValues returns the secret store sealed with this cipher.
//
// A NIL CIPHER IS A STORE THAT REFUSES EVERYTHING rather than one that
// stores plaintext — see the type comment. It is not an error to construct,
// because a node with no keyring is a supported deployment; it is an error
// to use.
func (d *DB) SecretValues(cipher secrets.Cipher) *SecretValues {
	return &SecretValues{db: d, cipher: cipher}
}

// ErrNoSecretKeyring reports a store asked to work without a keyring.
var ErrNoSecretKeyring = errors.New(
	"store: the secret store needs a keyring; set bootstrap secrets.keys")

// ErrSecretNotFound reports a name with no row.
//
// Its own error because the caller's answer differs: a missing secret is an
// operator's unset variable, while a decrypt failure is a keyring that no
// longer opens what it wrote. Collapsing them would have a rotation that
// dropped a key look exactly like a name nobody set.
var ErrSecretNotFound = errors.New("store: no such secret")

// SecretRecord is one stored secret's metadata, without its value.
//
// The value is DELIBERATELY absent: this is what a listing renders, and a
// listing that carried plaintext would put every credential a company has on
// one screen, in one response body, in one log line if anything printed it.
type SecretRecord struct {
	Name string

	// KeyID is the keyring key this row is sealed under, denormalised out
	// of the envelope so a rotation sweep can find stale rows without
	// decrypting any of them.
	KeyID string

	UpdatedAt time.Time

	// UpdatedBy and Source are provenance: who wrote it and through what —
	// an operator at the CLI, a provisioning run that minted a token.
	// Answering "where did this credential come from" months later is the
	// whole reason they are columns rather than a log line.
	UpdatedBy string
	Source    string
}

const secretUpsertSQL = `
INSERT INTO secret_values (name, value, key_id, updated_at, updated_by, source)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
    value = excluded.value, key_id = excluded.key_id,
    updated_at = excluded.updated_at, updated_by = excluded.updated_by,
    source = excluded.source`

// Set seals a secret and stores it under name, replacing any prior value.
//
// AN UPSERT rather than a read-then-write, because rotation is the common
// path and two operators rotating at once must not produce a row that holds
// neither of their values.
func (s *SecretValues) Set(ctx context.Context, name, value, by, source string, now time.Time) error {
	if s.cipher == nil {
		return ErrNoSecretKeyring
	}
	if name == "" {
		return errors.New("store: a secret needs a name")
	}
	// THE NAME IS BOUND IN as associated data, so a ciphertext moved to
	// another row fails to decrypt instead of silently impersonating a
	// different secret — which is what an attacker with UPDATE but not the
	// key would otherwise be able to do.
	sealed, err := s.cipher.Encrypt(value, secrets.AADForVar(name))
	if err != nil {
		return fmt.Errorf("store: seal %s: %w", name, err)
	}
	keyID, ok := secrets.EnvelopeKeyID(sealed)
	if !ok {
		return fmt.Errorf("store: seal %s: the cipher produced no key id", name)
	}
	_, err = s.db.sql.ExecContext(ctx, secretUpsertSQL, name, sealed, keyID,
		EncodeTime(now), by, source)
	if err != nil {
		return fmt.Errorf("store: write secret %s: %w", name, err)
	}
	return nil
}

// Get unseals one secret.
//
// The error says WHICH failure it was — see [ErrSecretNotFound] — and never
// carries the value or the ciphertext: a decrypt error that echoed its input
// would put an envelope into a log that a keyring might later open.
func (s *SecretValues) Get(ctx context.Context, name string) (string, error) {
	if s.cipher == nil {
		return "", ErrNoSecretKeyring
	}
	var sealed string
	err := s.db.sql.QueryRowContext(ctx,
		`SELECT value FROM secret_values WHERE name = ?`, name).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	}
	if err != nil {
		return "", fmt.Errorf("store: read secret %s: %w", name, err)
	}
	plain, err := s.cipher.Decrypt(sealed, secrets.AADForVar(name))
	if err != nil {
		return "", fmt.Errorf("store: open secret %s: %w", name, err)
	}
	return plain, nil
}

// List returns every stored secret's metadata, name-ordered.
//
// NAME-ORDERED rather than by recency, because this is what an operator
// reads to answer "is X set", and a list that moves every time something is
// rotated is one they have to search rather than scan.
func (s *SecretValues) List(ctx context.Context) ([]SecretRecord, error) {
	rows, err := s.db.sql.QueryContext(ctx,
		`SELECT name, key_id, updated_at, updated_by, source
		 FROM secret_values ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list secrets: %w", err)
	}
	defer rows.Close()
	var out []SecretRecord
	for rows.Next() {
		var r SecretRecord
		var micros int64
		if err := rows.Scan(&r.Name, &r.KeyID, &micros, &r.UpdatedBy, &r.Source); err != nil {
			return nil, fmt.Errorf("store: scan secret: %w", err)
		}
		r.UpdatedAt = DecodeTime(micros)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Unset removes a secret, reporting whether a row went.
func (s *SecretValues) Unset(ctx context.Context, name string) (bool, error) {
	res, err := s.db.sql.ExecContext(ctx,
		`DELETE FROM secret_values WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("store: unset secret %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: unset secret %s: %w", name, err)
	}
	return n > 0, nil
}

// All unseals every secret, for the boot snapshot the resolver reads.
//
// A SNAPSHOT rather than a per-lookup query, because ${VAR} resolution
// happens deep inside config expansion — per role, per provider, per MCP
// server — and a database round trip there would put the store on the path
// of every config read, including the ones a validate command makes with no
// database at all.
//
// It FAILS CLOSED on the first row it cannot open: a partial snapshot is
// the worst outcome, because the names that are missing resolve to whatever
// the environment happens to hold, which is exactly the stale-.env shadowing
// the store exists to prevent.
func (s *SecretValues) All(ctx context.Context) (map[string]string, error) {
	if s.cipher == nil {
		return nil, ErrNoSecretKeyring
	}
	rows, err := s.db.sql.QueryContext(ctx,
		`SELECT name, value FROM secret_values ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: read secrets: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, sealed string
		if err := rows.Scan(&name, &sealed); err != nil {
			return nil, fmt.Errorf("store: scan secret: %w", err)
		}
		plain, err := s.cipher.Decrypt(sealed, secrets.AADForVar(name))
		if err != nil {
			return nil, fmt.Errorf("store: open secret %s: %w", name, err)
		}
		out[name] = plain
	}
	return out, rows.Err()
}

// Rekey re-seals every row that is not already under the keyring's active
// key, reporting the names it moved.
//
// NAMES RATHER THAN A COUNT, because that is what an operator needs to
// confirm afterwards, and because a rekey that moved 12 of 13 rows is a
// question ("which one?") that a count cannot answer.
//
// One transaction: a rekey interrupted halfway leaves rows under a key the
// operator is about to retire, and the whole point of the pass is being able
// to retire it.
func (s *SecretValues) Rekey(ctx context.Context, activeKeyID, by string, now time.Time) ([]string, error) {
	if s.cipher == nil {
		return nil, ErrNoSecretKeyring
	}
	var moved []string
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT name, value FROM secret_values WHERE key_id != ? ORDER BY name`,
			activeKeyID)
		if err != nil {
			return fmt.Errorf("store: find stale secrets: %w", err)
		}
		// Collected before writing: the driver holds one connection per
		// transaction, so writing while the rows are open would deadlock
		// against the read this is iterating.
		stale := map[string]string{}
		for rows.Next() {
			var name, sealed string
			if err := rows.Scan(&name, &sealed); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan secret: %w", err)
			}
			stale[name] = sealed
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: find stale secrets: %w", err)
		}

		for _, name := range sortedNames(stale) {
			plain, err := s.cipher.Decrypt(stale[name], secrets.AADForVar(name))
			if err != nil {
				// ABORTS THE WHOLE PASS. A row this keyring cannot open
				// is a key that was dropped from the config, and moving
				// the others while leaving it would report a successful
				// rekey over a secret that is now unreadable for ever.
				return fmt.Errorf("store: open secret %s for rekey: %w", name, err)
			}
			sealed, err := s.cipher.Encrypt(plain, secrets.AADForVar(name))
			if err != nil {
				return fmt.Errorf("store: re-seal %s: %w", name, err)
			}
			if _, err := tx.ExecContext(ctx, secretUpsertSQL, name, sealed,
				activeKeyID, EncodeTime(now), by, "rekey"); err != nil {
				return fmt.Errorf("store: write re-sealed %s: %w", name, err)
			}
			moved = append(moved, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return moved, nil
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
