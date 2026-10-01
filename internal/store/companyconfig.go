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

// AuthorKind is WHAT wrote a revision: a person through a credential, or the
// engine itself.
//
// # Why a kind and not just a name
//
// `created_by` is a LABEL — a token's name, a login, a node id, "reconcile
// loop" — and the two name spaces overlap: nothing stops an operator token
// being called `node`. A reader that inferred the writer from the label was
// guessing, and the audit screen stopped guessing by assuming instead: it drew
// every revision as an operator's, including the ones a node seeded from its
// file and the reloads the reconcile loop makes after sealing a credential.
// So the writer states its kind at the write, where it is a fact, beside the
// label it chose.
//
// There is no `seat` kind. No seat writes configuration in this build — the
// agent tools reach the tracker and the knowledge base, never /config — and a
// constant with no producer would be a value every reader had to handle and no
// test could reach. A newer build that adds one needs no change here to be
// READ: [Configs.Adopt] keeps whatever kind the fleet's pointer carries.
type AuthorKind string

// The two writers of a revision in this build.
const (
	// AuthorOperator is a person, through the credential that
	// authenticated them: an API token on /config or /setup, or the login
	// running `crewlet config import` and `crewlet config rekey`.
	AuthorOperator AuthorKind = "operator"
	// AuthorNode is the engine acting on its own: a node seeding the store
	// from its -company file at boot, and the reconcile loop's own writes
	// (removing a disconnected integration's block, recording a site it
	// discovered, reloading after it sealed a credential).
	AuthorNode AuthorKind = "node"
)

// Valid reports a kind this build writes.
//
// Only a WRITE is held to it. A revision adopted from the fleet carries the
// kind its origin recorded, which may be one a newer build added, or none at
// all from a pointer an older build published — an unknown kind off the wire
// is a value, and refusing it would stop this node recording a revision it is
// running.
func (k AuthorKind) Valid() bool {
	switch k {
	case AuthorOperator, AuthorNode:
		return true
	}
	return false
}

// Author is who wrote a revision: the label they are recorded under and
// their [AuthorKind]. One value, so a writer cannot pass a name and forget
// what it names.
type Author struct {
	Name string
	Kind AuthorKind
}

// Revision is one immutable snapshot of the whole Tier B document.
type Revision struct {
	ID       string
	ParentID string

	CreatedAt time.Time
	CreatedBy string
	// CreatedByKind is what CreatedBy names. EMPTY only on a revision this
	// node adopted from a pointer that did not say — published by a build
	// older than the one that records it, or stored before migration
	// 0035 could classify it — and a reader shows that as "not recorded"
	// rather than picking a kind.
	CreatedByKind AuthorKind
	Source        string
	Summary       string

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
	created_by_kind, source, summary, payload, is_active, activated_at`

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
//
// # Active on the way in is a claim, and only the offline paths may make it
//
// This node's active revision is what it serves from GET /config, what it
// boots on, and what it offers the fleet at its next start when the pointer
// it finds is older (see the boot publish in cmd/crewlet). So marking a
// revision active here says "this is the company", and that is true for a
// command run while the engine is stopped, which cannot move the pointer and
// leaves the publish to the next boot. A write through a running node's API
// has not been accepted by anybody yet when it stores its revision, and it
// uses [Configs.Insert] instead.
func (c *Configs) InsertActive(ctx context.Context, r Revision) (string, error) {
	return c.insert(ctx, r, true)
}

// Insert writes a new revision into the history WITHOUT making it the active
// one, returning its id.
//
// The config API's write path: its revision becomes this node's active one
// only once the FLEET has taken it, with [Configs.Activate] after the pointer
// moved. A revision that lost the activation's compare-and-set stays here,
// readable and revertable, and nothing on this node treats it as the
// company. Inserted active, the loser was what GET /config served until the
// next activation, and what this node published to the whole fleet at its
// next start, since a locally active revision newer than the pointer is
// exactly what the boot publish offers: a write answered with a 409 or a 412
// landed after all, one restart later.
func (c *Configs) Insert(ctx context.Context, r Revision) (string, error) {
	return c.insert(ctx, r, false)
}

// insert is the one INSERT both writes share, so a revision stored active and
// one stored inactive cannot differ in anything but the flag.
//
// THE KIND IS REQUIRED. Every writer on this node knows whether it is a person
// or the engine, and a revision stored without saying is exactly the row the
// audit screen used to fill in with a guess.
func (c *Configs) insert(ctx context.Context, r Revision, active bool) (string, error) {
	if !r.CreatedByKind.Valid() {
		return "", fmt.Errorf("store: a config revision needs its author's kind "+
			"(%q or %q), got %q", AuthorOperator, AuthorNode, r.CreatedByKind)
	}
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
	// NULL until the revision is active, which is what activated_at says
	// about every row a later activation deactivated too.
	isActive, activatedAt := 0, sql.NullInt64{}
	if active {
		isActive, activatedAt = 1, sql.NullInt64{Int64: EncodeTime(at), Valid: true}
	}
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		if active {
			if _, err := tx.ExecContext(ctx,
				`UPDATE company_config SET is_active = 0 WHERE is_active <> 0`); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO company_config
			     (revision_id, parent_revision_id, created_at, created_by,
			      created_by_kind, source, summary, payload, is_active, activated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, NullText(r.ParentID), EncodeTime(at), r.CreatedBy,
			string(r.CreatedByKind), r.Source, r.Summary, string(payload),
			isActive, activatedAt)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("store: insert config revision: %w", err)
	}
	log.InfoContext(ctx, "config_revision_stored",
		"revision", id, "source", r.Source, "by", r.CreatedBy,
		"by_kind", string(r.CreatedByKind), "active", active)
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
		// Checked through RowsAffected rather than RETURNING. That was
		// once about the dialect intersection two drivers shared;
		// with one driver it is simply the narrower thing that works,
		// and it is what the statement needs — without the check
		// the transaction commits having deactivated everything, which
		// is a company with no config.
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
//
// # The author is the ORIGIN's, never this node's
//
// The caller passes the author, kind, source and creation instant the fleet's
// pointer carries, so a revision reads the same on every node rather than
// "peer" everywhere but the one it was written on. The kind is NOT held to
// [AuthorKind.Valid] here, for the reason that method gives.
//
// A row that is already here keeps its body, but an author it did not know
// is FILLED IN when the fleet now says: a node that adopted a revision from
// an older build's pointer, or before this was recorded at all, learns who
// wrote it the next time the fleet points at it. A known author is never
// overwritten — the row this node wrote itself is the authority on its own
// write.
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
			      created_by_kind, source, summary, payload, is_active, activated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
			 ON CONFLICT (revision_id) DO UPDATE
			    SET created_by = excluded.created_by,
			        created_by_kind = excluded.created_by_kind
			  WHERE company_config.created_by_kind = ''
			    AND excluded.created_by_kind <> ''`,
			r.ID, NullText(r.ParentID), EncodeTime(at), r.CreatedBy,
			string(r.CreatedByKind), r.Source, r.Summary, string(payload),
			EncodeTime(at)); err != nil {
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
	log.InfoContext(ctx, "config_revision_adopted", "revision", r.ID, "source", r.Source,
		"by", r.CreatedBy, "by_kind", string(r.CreatedByKind))
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
	var kind string
	if err := rows.Scan(&r.ID, &parent, &createdAt, &r.CreatedBy, &kind, &r.Source,
		&r.Summary, &payload, &active, &activatedAt); err != nil {
		return Revision{}, fmt.Errorf("store: read config revision: %w", err)
	}
	r.ParentID = Text(parent)
	r.CreatedByKind = AuthorKind(kind)
	r.CreatedAt = DecodeTime(createdAt)
	r.Payload = json.RawMessage(payload)
	r.Active = active != 0
	r.ActivatedAt = TimeAt(activatedAt)
	return r, nil
}
