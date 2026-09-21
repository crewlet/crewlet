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

	// ScrubbedAt is when this revision's personal fields were erased, and
	// the zero time means they were not.
	//
	// It narrows the "immutable snapshot" this table's own comment states,
	// which is why it is a stamp rather than a silent rewrite: a diff
	// across a scrub boundary shows a tombstone, and the next reader has
	// to be able to tell that from corruption. See
	// `0030_a_superseded_revision_can_be_scrubbed.sql`.
	ScrubbedAt time.Time

	// ChartPosition is the org chart position THIS NODE composed its epoch
	// at when it activated this revision, packed, and nil where it has
	// none.
	//
	// A POINTER because zero is a real answer and absence is a different
	// one: a revision activated on an empty chart ran at position 0, and a
	// revision activated by a build before the column existed has no
	// recorded position at all. A plain int64 would report every historical
	// activation as having run on an empty company.
	//
	// See `0031_an_activation_records_the_chart_it_ran.sql` for why the
	// company needs both halves to be answerable.
	ChartPosition *int64
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
	source, summary, payload, is_active, activated_at, scrubbed_at,
	chart_position`

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
func (c *Configs) insert(ctx context.Context, r Revision, active bool) (string, error) {
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
			      source, summary, payload, is_active, activated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, NullText(r.ParentID), EncodeTime(at), r.CreatedBy,
			r.Source, r.Summary, string(payload), isActive, activatedAt)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("store: insert config revision: %w", err)
	}
	log.InfoContext(ctx, "config_revision_stored",
		"revision", id, "source", r.Source, "by", r.CreatedBy, "active", active)
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
	var activatedAt, scrubbedAt, chartPosition sql.NullInt64
	var active int64
	if err := rows.Scan(&r.ID, &parent, &createdAt, &r.CreatedBy, &r.Source,
		&r.Summary, &payload, &active, &activatedAt, &scrubbedAt,
		&chartPosition); err != nil {
		return Revision{}, fmt.Errorf("store: read config revision: %w", err)
	}
	if chartPosition.Valid {
		r.ChartPosition = &chartPosition.Int64
	}
	r.ParentID = Text(parent)
	r.CreatedAt = DecodeTime(createdAt)
	r.Payload = json.RawMessage(payload)
	r.Active = active != 0
	r.ActivatedAt = TimeAt(activatedAt)
	r.ScrubbedAt = TimeAt(scrubbedAt)
	return r, nil
}

// ErrRevisionIsActive reports a scrub aimed at the revision the fleet serves.
var ErrRevisionIsActive = errors.New(
	"store: the active revision cannot be scrubbed")

// Scrub replaces one SUPERSEDED revision's payload and stamps scrubbed_at.
//
// # The one write that edits a revision, and what bounds it
//
// Every other write here appends: importing writes a new row, activating
// appends to the pointer, and that is what makes the history a record rather
// than a claim. This one rewrites a row in place, because appending cannot
// erase anything — a new revision with the address removed leaves the old row
// holding it, which is the whole problem.
//
// So it is bounded twice. It reaches only what `crewlet config scrub` names —
// personal fields, never a setting — and it REFUSES THE ACTIVE REVISION,
// which is enforced by the statement rather than by the caller remembering
// to: the fleet is serving that document, every node is holding it, and a
// rewrite underneath them would be a config change nothing activated. An
// operator who wants the address out of the live company edits the company.
//
// `0030_a_superseded_revision_can_be_scrubbed.sql` is where the narrowed
// immutability is written down.
func (c *Configs) Scrub(ctx context.Context, revisionID string, payload json.RawMessage, at time.Time) error {
	result, err := c.db.sql.ExecContext(ctx,
		`UPDATE company_config SET payload = ?, scrubbed_at = ?
		 WHERE revision_id = ? AND is_active = 0`,
		string(payload), EncodeTime(at), revisionID)
	if err != nil {
		return fmt.Errorf("store: scrub config revision %s: %w", revisionID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: scrub config revision %s: %w", revisionID, err)
	}
	if n == 0 {
		// TWO CAUSES, ONE STATEMENT, and they are told apart by a read
		// rather than by a second guarded write: asking first and updating
		// after would be a race with an activation, and the clause above
		// is what actually holds.
		_, found, err := c.Get(ctx, revisionID)
		switch {
		case err != nil:
			return err
		case !found:
			return fmt.Errorf("%w: %s", ErrNoRevision, revisionID)
		default:
			return fmt.Errorf("%w: %s", ErrRevisionIsActive, revisionID)
		}
	}
	log.InfoContext(ctx, "config_revision_scrubbed", "revision", revisionID)
	return nil
}

// Chain is the active revision and every ancestor it reaches through
// parent_revision_id, newest first.
//
// WHAT THE RETENTION SWEEP MAY NOT DELETE, whatever its horizon. A revert
// re-activates an older revision by id, and `crewlet config diff` walks the
// chain — so a deleted ancestor turns both into an error naming a row that
// used to exist. The chain is normally short: it is one revision per config
// write, not per node and not per apply.
//
// STOPS AT THE FIRST BREAK rather than reporting one. A chain can already be
// broken by a revision this node never adopted — a node joins a running fleet
// and fetches the active revision alone — and that is an ordinary state
// rather than damage, so the sweep protects what it can reach and lets the
// horizon cover the rest.
//
// A VISITED SET, because a cycle in parent pointers would otherwise be an
// infinite loop inside a maintenance tick. Nothing writes one, which is
// exactly why nothing would notice it.
func (c *Configs) Chain(ctx context.Context) ([]Revision, error) {
	active, found, err := c.Active(ctx)
	if err != nil || !found {
		return nil, err
	}
	out := []Revision{active}
	seen := map[string]bool{active.ID: true}
	for at := active; at.ParentID != ""; {
		if seen[at.ParentID] {
			return out, nil
		}
		parent, found, err := c.Get(ctx, at.ParentID)
		if err != nil {
			return nil, err
		}
		if !found {
			return out, nil
		}
		seen[parent.ID] = true
		out = append(out, parent)
		at = parent
	}
	return out, nil
}

// Purge deletes revisions older than cutoff, keeping the active one and its
// unbroken parent chain, and reports how many went.
//
// # Why this table needs a sweep at all
//
// Every node keeps its OWN copy of every revision it ever met, in an
// append-only table, and nothing deleted from it — so a company that edits
// its configuration daily accumulates a row per edit per node for the life of
// the deployment, and each row is a copy of the whole document. It is also
// where a pre-split revision's `roles[].email` lives, which is why this ships
// beside `crewlet config scrub`: the scrub erases what is inside a row it
// keeps, and this is what eventually removes the row.
//
// # The chain is kept whatever its age
//
// Deleting an ancestor of the active revision breaks a revert and a diff, and
// a company that has not changed its configuration for a year has an active
// revision OLDER than any horizon worth setting. So the chain is excluded by
// id rather than by date.
func (c *Configs) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	chain, err := c.Chain(ctx)
	if err != nil {
		return 0, err
	}
	// BOUND PARAMETERS, one per kept revision, rather than an interpolated
	// list: the chain is short and the ids are uuids this process wrote,
	// but a query built by concatenation is a query somebody later feeds
	// something else.
	keep := make([]any, 0, len(chain))
	placeholders := make([]byte, 0, 2*len(chain))
	for i, rev := range chain {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		keep = append(keep, rev.ID)
	}
	doomed := `created_at < ? AND is_active = 0`
	if len(chain) > 0 {
		doomed += ` AND revision_id NOT IN (` + string(placeholders) + `)`
	}
	args := append([]any{EncodeTime(cutoff)}, keep...)

	var n int64
	err = c.db.Tx(ctx, func(tx *sql.Tx) error {
		// THE SURVIVORS' PARENT POINTERS FIRST, and this is not tidiness:
		// parent_revision_id is a real foreign key and this database runs
		// with `PRAGMA foreign_keys = ON`, so deleting a row something
		// still points at FAILS. Off-chain revisions point at each other
		// — a reverted branch is exactly that shape — so a bare delete
		// aborts the whole tick on the first row whose child has not gone
		// yet, and the table never shrinks while the log says the sweep
		// ran.
		//
		// NULLING IT IS THE TRUTHFUL REPAIR rather than a way round the
		// constraint: the ancestor is gone, so a pointer at it is a lie,
		// and the column is already nullable because the first revision
		// ever written has no parent. [Configs.Chain] stops at a break for
		// this reason among others.
		if _, err := tx.ExecContext(ctx,
			`UPDATE company_config SET parent_revision_id = NULL
			 WHERE parent_revision_id IN (
			     SELECT revision_id FROM company_config WHERE `+doomed+`)`,
			args...); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`DELETE FROM company_config WHERE `+doomed, args...)
		if err != nil {
			return err
		}
		n, err = result.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: purge config revisions: %w", err)
	}
	return n, nil
}

// RecordChartPosition stamps the chart position this node composed its epoch
// at when it activated a revision.
//
// # Why it is a write of its own rather than a column of the activation
//
// The two happen at different moments and the order matters. A node activates
// a revision by flipping `is_active`, and it composes the epoch AFTER that —
// against whatever chart position its applier has reached, which may itself
// move while the settings are being installed. Writing the position inside
// [Configs.Activate] would record the position at the flip, which is the one
// instant the epoch has not been composed at yet.
//
// # It is BEST EFFORT at the caller, and that is a property of the value
//
// The pair (revision, position) is how a reader answers "what was this
// company"; it is not what the node RUNS on. An epoch composes from the
// settings and the live view whether or not this row was written, so a failure
// here is a gap in the history rather than a company that stops. The caller
// logs it and carries on — refusing the activation over an audit column would
// take a healthy company down to protect a record of it.
//
// IDEMPOTENT, because re-activating an unchanged revision is the ordinary
// credential-rotation gesture: it re-stamps with the position that
// re-activation ran at, which is the true answer for the activation that just
// happened.
func (c *Configs) RecordChartPosition(ctx context.Context, revisionID string, packed int64) error {
	result, err := c.db.sql.ExecContext(ctx,
		`UPDATE company_config SET chart_position = ? WHERE revision_id = ?`,
		packed, revisionID)
	if err != nil {
		return fmt.Errorf("store: record the chart position of revision %s: %w",
			revisionID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: record the chart position of revision %s: %w",
			revisionID, err)
	}
	if n == 0 {
		// A REVISION THIS NODE DOES NOT HOLD, which is an ordinary state
		// rather than a fault: the local copy is best effort, so a node
		// whose disk was full when the pointer moved applies the epoch
		// from the fleet's bytes and has no row to stamp.
		return fmt.Errorf("%w: %s", ErrNoRevision, revisionID)
	}
	return nil
}
