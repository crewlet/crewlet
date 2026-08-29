package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DefaultRevisionPage is how many revisions a listing returns when the caller
// names no limit. One screen of history, which is what the operator view asks
// for; the whole chain is available by paging.
const DefaultRevisionPage = 50

// ErrNoRevision reports a revision id that does not exist.
var ErrNoRevision = errors.New("store: no such config revision")

// Revision is one immutable snapshot of the whole Tier B document.
type Revision struct {
	ID       string
	ParentID string

	CreatedAt time.Time
	CreatedBy string
	Source    string
	Summary   string

	// Payload is the document as stored. When a keyring is configured this
	// is the sealed envelope rather than the plaintext structure — opaque
	// to SQL either way, which is why it is one column and not a schema.
	Payload json.RawMessage

	Active      bool
	ActivatedAt time.Time
}

// Configs is the versioned Tier B store.
//
// Single-tenant, like everything here: at most one row is active, and ZERO
// rows is a real state rather than a failure — the engine boots, the API
// serves /config, and the first import populates the company.
type Configs struct{ db *DB }

// Configs returns the company-config store backed by this database.
func (d *DB) Configs() *Configs { return &Configs{db: d} }

const revisionColumns = `revision_id, parent_revision_id, created_at, created_by,
	source, summary, payload, is_active, activated_at`

// InsertActive writes a new revision and makes it the active one, returning
// its id.
//
// # It stores the revision; it does NOT move the fleet's pointer
//
// The pointer is coordination state — its epoch is a fencing token every node
// has to agree on — and this database is the node's own. So publishing is two
// steps in two stores, and the ORDER is the safe one: the revision is stored
// FIRST, then the caller points the fleet at it with coord.Plane.Activate. A
// crash between them leaves a revision nothing points at, which is inert and
// re-activatable; the other order would point a fleet at a revision nobody
// can read.
//
// ONE transaction still covers the deactivate and the insert. The partial
// unique index refuses two active rows, so a deactivate that landed without
// its insert would leave a company with no configuration.
func (c *Configs) InsertActive(ctx context.Context, r Revision) (string, error) {
	id := r.ID
	if id == "" {
		id = uuid.NewString()
	}
	at := r.CreatedAt
	if at.IsZero() {
		at = now()
	}
	payload := r.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE company_config SET is_active = 0 WHERE is_active <> 0`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO company_config
			     (revision_id, parent_revision_id, created_at, created_by,
			      source, summary, payload, is_active, activated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)`,
			id, NullText(r.ParentID), EncodeTime(at), r.CreatedBy,
			r.Source, r.Summary, string(payload), EncodeTime(at))
		return err
	})
	if err != nil {
		return "", fmt.Errorf("store: insert config revision: %w", err)
	}
	log.InfoContext(ctx, "config_revision_stored",
		"revision", id, "source", r.Source, "by", r.CreatedBy)
	return id, nil
}

// Activate makes an existing revision the active one LOCALLY and returns its
// summary, for the caller to publish with the pointer. See [Configs.InsertActive]
// for why the two are separate steps.
//
// Re-activating the revision that is ALREADY active is a supported gesture,
// not a no-op: it is how an operator asks a running fleet to re-resolve its
// ${VAR} references and pick up a rotated credential. The pointer append is
// unconditional for exactly that reason — keyed on the revision id it could
// never express "the same configuration, resolved again".
func (c *Configs) Activate(ctx context.Context, revisionID string, at time.Time) (string, error) {
	if at.IsZero() {
		at = now()
	}
	var summary string
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE company_config SET is_active = 0 WHERE is_active <> 0`); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE company_config SET is_active = 1, activated_at = ?
			 WHERE revision_id = ?`, EncodeTime(at), revisionID)
		if err != nil {
			return err
		}
		// Checked through RowsAffected rather than RETURNING, which is
		// outside the dialect intersection the two certified drivers
		// share. Without the check the transaction commits having
		// deactivated everything, which is a company with no config.
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s", ErrNoRevision, revisionID)
		}
		return tx.QueryRowContext(ctx,
			`SELECT summary FROM company_config WHERE revision_id = ?`,
			revisionID).Scan(&summary)
	})
	if err != nil {
		if errors.Is(err, ErrNoRevision) {
			return "", err
		}
		return "", fmt.Errorf("store: activate revision %s: %w", revisionID, err)
	}
	log.InfoContext(ctx, "config_revision_marked_active", "revision", revisionID)
	return summary, nil
}

// Adopt records a revision this node fetched from the FLEET and makes it the
// active one locally.
//
// The peer's path. A revision is stored in the database of whichever node
// served the write, so every other node meets it for the first time when the
// activation pointer names it — and this is where that node keeps its own
// copy. Without it a peer would apply revisions it can never show: the config
// history, the diffs and the revert targets are all read out of this table.
//
// IDEMPOTENT on the revision id, unlike [Configs.InsertActive], because a
// re-fetch is ordinary: the local write is best effort, so a node whose disk
// was full when it first adopted comes back through here on its next miss.
// The body is left as it was found — the fleet's copy and this one are the
// same sealed bytes, and rewriting it would be a no-op that could only differ
// if something had already gone wrong.
//
// The id is REQUIRED, and that is the difference from InsertActive minting
// one: this row's identity belongs to the fleet, and a generated id would
// make the node's own history disagree with the pointer it converged on.
func (c *Configs) Adopt(ctx context.Context, r Revision) error {
	if r.ID == "" {
		return fmt.Errorf("store: adopting a revision needs its fleet id")
	}
	at := r.CreatedAt
	if at.IsZero() {
		at = now()
	}
	payload := r.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE company_config SET is_active = 0 WHERE is_active <> 0`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO company_config
			     (revision_id, parent_revision_id, created_at, created_by,
			      source, summary, payload, is_active, activated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)
			 ON CONFLICT (revision_id) DO NOTHING`,
			r.ID, NullText(r.ParentID), EncodeTime(at), r.CreatedBy,
			r.Source, r.Summary, string(payload), EncodeTime(at)); err != nil {
			return err
		}
		// The row may already have been here — the conflict above did
		// nothing — and the deactivate above cleared its flag, so the
		// activate is unconditional rather than part of the insert.
		_, err := tx.ExecContext(ctx,
			`UPDATE company_config SET is_active = 1, activated_at = ?
			 WHERE revision_id = ?`, EncodeTime(at), r.ID)
		return err
	})
	if err != nil {
		return fmt.Errorf("store: adopt config revision %s: %w", r.ID, err)
	}
	log.InfoContext(ctx, "config_revision_adopted", "revision", r.ID, "source", r.Source)
	return nil
}

// Active returns the currently-active revision, and whether there is one.
func (c *Configs) Active(ctx context.Context) (Revision, bool, error) {
	return c.one(ctx,
		`SELECT `+revisionColumns+` FROM company_config WHERE is_active <> 0 LIMIT 1`)
}

// Get returns a revision by id, and whether it exists.
func (c *Configs) Get(ctx context.Context, revisionID string) (Revision, bool, error) {
	return c.one(ctx,
		`SELECT `+revisionColumns+` FROM company_config WHERE revision_id = ?`, revisionID)
}

func (c *Configs) one(ctx context.Context, query string, args ...any) (Revision, bool, error) {
	rows, err := c.db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return Revision{}, false, fmt.Errorf("store: read config revision: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return Revision{}, false, fmt.Errorf("store: read config revision: %w", err)
		}
		return Revision{}, false, nil
	}
	r, err := scanRevision(rows)
	if err != nil {
		return Revision{}, false, err
	}
	return r, true, nil
}

// List returns revisions newest first.
//
// The tiebreak is the INSERTION order, not the revision id. Time alone is not
// unique — an import that writes several revisions in one burst shares a
// microsecond — and a random uuid as the tiebreak is stable without being
// truthful: it can put the older of two revisions first, and a history read in
// the wrong order is worse than one read slowly. The implicit rowid is the
// only monotonic thing this table has, and it is exactly the fact needed.
func (c *Configs) List(ctx context.Context, limit, offset int) ([]Revision, error) {
	if limit <= 0 {
		limit = DefaultRevisionPage
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := c.db.sql.QueryContext(ctx,
		`SELECT `+revisionColumns+` FROM company_config
		 ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list config revisions: %w", err)
	}
	defer rows.Close()

	var out []Revision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list config revisions: %w", err)
	}
	return out, nil
}

func scanRevision(rows *sql.Rows) (Revision, error) {
	var r Revision
	var parent sql.NullString
	var payload string
	var createdAt int64
	var activatedAt sql.NullInt64
	var active int64
	if err := rows.Scan(&r.ID, &parent, &createdAt, &r.CreatedBy, &r.Source,
		&r.Summary, &payload, &active, &activatedAt); err != nil {
		return Revision{}, fmt.Errorf("store: read config revision: %w", err)
	}
	r.ParentID = Text(parent)
	r.CreatedAt = DecodeTime(createdAt)
	r.Payload = json.RawMessage(payload)
	r.Active = active != 0
	r.ActivatedAt = TimeAt(activatedAt)
	return r, nil
}
