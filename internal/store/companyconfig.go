package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// DefaultRevisionPage is how many revisions a listing returns when the caller
// names no limit. One screen of history, which is what the operator view asks
// for; the whole chain is available by paging.
const DefaultRevisionPage = 50

// ErrNoRevision reports a revision id that does not exist.
var ErrNoRevision = errors.New("store: no such config revision")

// ErrNotOffered reports a boot-publish step aimed at a revision that is not
// this node's unpublished offline choice — a revision in history, or a boot row
// that carries no offer. The store's own guard, kept whatever its caller holds:
// `crewlet run` holds this node's store from the read its boot decides on until
// its engine has opened the file, so nothing can replace the offer in between,
// and a boot that meets this refuses to start over it as a defect.
var ErrNotOffered = errors.New("store: the revision is not this node's unpublished offline choice")

// Revision is one immutable snapshot of the whole Tier B document, and what
// this node knows about it.
type Revision struct {
	ID       string
	ParentID string

	CreatedAt time.Time

	// CreatedBy is the revision's AUTHOR, CreatedByKind what sort of
	// author, and OperatorID the credential it was written through — empty
	// for a write no credential made. Three rather than the one this used
	// to be, which held the credential and so named, for a revision written
	// through a person's machine token, a token whose row the identity
	// sweep collects a week after it lapses. See
	// node migration `0047_identity_and_access.sql`.
	//
	// THE KIND IS [iam.ActorKind], the vocabulary every other trail in the
	// engine records an author in, rather than a second one of this
	// table's: a label alone cannot say whether a person or the engine
	// wrote the revision — the name spaces overlap, and nothing stops a
	// token being called after a node — so the writer states it at the
	// write, where it is a fact. NEVER EMPTY: one of this build's kinds on
	// every revision it writes, and on a copy of a fleet revision whatever
	// kind the fleet's pointer carries — which may be one a newer build
	// added, and arrives as itself ([Configs.SawFleet]).
	CreatedBy     string
	CreatedByKind iam.ActorKind
	OperatorID    string

	Source  string
	Summary string

	// Payload is the document as stored: the sealed envelope, under the
	// keyring every node holds, rather than the plaintext structure. Opaque
	// to SQL, which is why it is one column and not a schema — and to this
	// package, which stores what it is handed and never opens it.
	Payload json.RawMessage

	// Boots is whether this node STARTS on the revision: the last one its
	// engine applied, or one an offline command chose ([Configs.Boot]).
	// Fleets is whether it is the newest fleet revision this node's
	// reconciler opened, runnable here or not ([Configs.Fleets]). Offer is
	// set on a boot row an offline command chose and the fleet has not taken
	// yet ([Configs.Offer]).
	//
	// READ-ONLY on a write: each is moved by the verb that says what it
	// means, and [Configs.Insert] refuses a revision that sets one.
	Boots  bool
	Fleets bool
	Offer  *Offer

	// ActivatedAt is the latest instant this node knows the fleet activated
	// the revision at, on every row: what the retention sweep dates a
	// re-activated old revision by, what the boot restores a lost pointer at,
	// and what the reconciler holds the pointer's instant against. It rises
	// and never falls ([Configs.SawFleet], [Configs.NoteActivation],
	// [Configs.Serve]). Zero for a revision nothing ever activated.
	ActivatedAt time.Time

	// ServedAt is the boot row's alone: the instant of the activation this
	// node APPLIED, which the engine stamps the org chart and the knowledge
	// spaces with when it boots on the revision. Only [Configs.Serve] writes
	// it, and a row that stops being the boot row loses it, as does an
	// offline choice, which nothing has applied.
	//
	// NOT ActivatedAt, which it used to be: a re-activation of the revision
	// this node runs that the node then refused raises the fleet's instant
	// without being applied. One column could keep only one of the two, so
	// either the boot stamped the chart with an activation it never applied,
	// or the fleet's instant was lost — and the reconciler, holding the
	// pointer against the stale one, wrote the fleet's row again every tick.
	ServedAt time.Time
}

// Offer is an offline choice of what this node boots on, not yet published to
// the fleet.
type Offer struct {
	// Base is the revision the change was built on: what this node knew the
	// company to be when the command ran. The boot publishes the offer only
	// while the fleet still runs it — a compare-and-set, the rule every API
	// write follows — so a change made on a node that was behind is never
	// published over what the fleet moved on to. Empty for a change built on
	// no company, which is published only where the fleet has none.
	Base string

	// Unconfirmed is the offer this one replaced while a start's publish of
	// that one was unresolved: it had been sent ([Offer.PlannedAt]) and
	// never confirmed, so whether the fleet runs it is not known here. The
	// change was made on it, so it is safe over either outcome — over Base if
	// that publish never landed, over Unconfirmed if it did — and the boot
	// publishes it over whichever the pointer names. Empty when there is
	// none.
	//
	// AT MOST ONE, by construction: the boot that plans a publish records
	// which of the two it found ([Configs.PlanOffer]), and an offline command
	// building on an offer that was never sent inherits that offer's
	// candidates rather than adding one.
	Unconfirmed string

	// PlannedAt is the instant the boot publish asked the pointer for, written
	// BEFORE it publishes ([Configs.PlanOffer]); zero until then. It is how a
	// node that crashed between the publish and its record of it tells the
	// pointer naming its own landed offer from the pointer still naming the
	// base — which, for a re-activation of the fleet's own revision, is the
	// same id. The publish pins the epoch it read, so a landed offer carries
	// exactly this instant.
	PlannedAt time.Time
}

// Over reports whether a pointer naming revisionID is one this offer may be
// published over: its base, or the unconfirmed offer it replaced.
func (o Offer) Over(revisionID string) bool {
	return revisionID == o.Base || (o.Unconfirmed != "" && revisionID == o.Unconfirmed)
}

// Configs is this node's config history and its three facts about it.
//
// # The company is the pointer; these are this node's own facts
//
// WHICH revision is the company is the fleet's activation pointer, in
// coordination, and nothing here answers it while the pointer can be read. What
// this table holds is what only this node can know:
//
//   - the BOOT row ([Configs.Boot]): what this node starts on. Online it is
//     moved by the reconciler alone, and only to a revision the engine has
//     just applied ([Configs.Serve]), so a node never boots on a revision it
//     refused. Offline, a command marks it ([Configs.Offer],
//     [Configs.OfferStored]), because that is what an offline command is for.
//   - the FLEET's row ([Configs.Fleets]): the newest pointer revision this
//     node's reconciler opened ([Configs.SawFleet]), refused or not. It is
//     what the node knows the company to be when the pointer is gone
//     ([Configs.Restorable]) — a node that refused R must restore R, not the P
//     it runs — and what an offline command builds on.
//   - an OFFER: a boot row an offline command chose and the fleet has not
//     taken, carrying the revision it was built on.
//
// These used to be one flag, `is_active`, and each reader took it for the fact
// it needed: a peer marked the fleet's revision active as it fetched it,
// before validating it, and booted on that refused revision at its next start;
// a write marked itself active as the pointer moved, and a reconciler that had
// read the pointer a moment earlier walked it back; an offline command marked
// a revision active "now", and the next boot published it over a fleet that
// had moved on. See node migration 0052.
//
// Single-tenant, like everything here: at most one boot row and one fleet's
// row, each enforced by a partial unique index, and ZERO rows is a real state
// rather than a failure — the engine boots, the API serves /config, and the
// first import populates the company.
type Configs struct{ db *DB }

// Configs returns the company-config store backed by this database.
func (d *DB) Configs() *Configs { return &Configs{db: d} }

const revisionColumns = `revision_id, parent_revision_id, created_at, created_by,
	created_by_kind, operator_id, source, summary, payload, is_active,
	activated_at, is_fleets, offered, offered_over, offered_unconfirmed,
	offered_at, served_at`

// bootRow and fleetsRow are the predicates that name the boot row and the
// fleet's row, each in the form its partial unique index
// (company_config_one_active_idx, `(is_active) WHERE is_active <> 0`, and
// company_config_one_fleets_idx) serves.
//
// BOTH TERMS, because neither alone seeks the index. `<> 0` is the index's own
// predicate, which a statement has to state for the planner to consider a
// partial index at all; `= 1` is the term it SEEKS on. With the predicate
// alone the planner, which keeps no statistics, scanned the table and tested
// every row — and every row is a whole sealed company document, kept for 400
// days. Every writer stores exactly 1 for a marked row, so the two terms
// select the one row the predicate alone did.
const (
	bootRow   = `is_active = 1 AND is_active <> 0`
	fleetsRow = `is_fleets = 1 AND is_fleets <> 0`
)

// The statements that read and clear the two marked rows, named so a test can
// read back the plan of the exact statement each runs.
//
// unbootSQL clears an offer's columns and the served instant WITH the flag, in
// the same statement: both are properties of the boot row, and a row that
// stops being it must not carry a base, a planned instant or an applied one
// into history, where a later [Configs.OfferStored] or [Configs.Serve] of the
// same row would find them.
const (
	bootSQL   = `SELECT ` + revisionColumns + ` FROM company_config WHERE ` + bootRow + ` LIMIT 1`
	fleetsSQL = `SELECT ` + revisionColumns + ` FROM company_config WHERE ` + fleetsRow + ` LIMIT 1`
	unbootSQL = `UPDATE company_config SET is_active = 0, offered = 0, offered_over = NULL,
		offered_unconfirmed = NULL, offered_at = NULL, served_at = NULL WHERE ` + bootRow
	unfleetsSQL = `UPDATE company_config SET is_fleets = 0 WHERE ` + fleetsRow
)

// insertSQL writes one revision with every column a writer states; keepSQL is
// the same row kept only when this node does not hold the id yet.
const (
	insertSQL = `INSERT INTO company_config
	     (revision_id, parent_revision_id, created_at, created_by,
	      created_by_kind, operator_id, source, summary, payload,
	      is_active, activated_at, offered, offered_over, offered_unconfirmed)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	keepSQL = insertSQL + ` ON CONFLICT (revision_id) DO NOTHING`
)

// raiseSQL moves a row's activation instant forward to the one given, never
// back — the boot row's too, whose applied instant is [Revision.ServedAt].
const raiseSQL = `UPDATE company_config SET activated_at = ?
	 WHERE revision_id = ? AND (activated_at IS NULL OR activated_at < ?)`

// Insert writes a new revision into the history, marking nothing, and returns
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
// A revision the fleet takes becomes what this node BOOTS on only when its
// reconciler has applied it ([Configs.Serve]): until then nothing has shown
// that this node can run it. One that lost the activation's compare-and-set
// stays here, readable and revertable, and nothing on this node treats it as
// the company.
func (c *Configs) Insert(ctx context.Context, r Revision) (string, error) {
	if err := written(r); err != nil {
		return "", err
	}
	id, at, payload := minted(r)
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		return insertRow(ctx, tx, insertSQL, id, r, at, payload, false, nil)
	})
	if err != nil {
		return "", fmt.Errorf("store: insert config revision: %w", err)
	}
	logStored(ctx, id, r, "history")
	return id, nil
}

// InsertOn writes a new revision built on base, which may be a fleet revision
// this node has never held, and returns its id.
//
// The config API's write path. An API write is built on the FLEET's revision —
// the pointer's, read through coordination — and on a peer that has not
// ticked since the pointer moved, that revision is in no row here. Its parent
// is a foreign key into this table, so the base is kept first, as history,
// from the copy the caller holds: it is the pointer's own sealed body and
// origin. A base this node already holds keeps its row as it is.
//
// r.ParentID must be base.ID, and a base with no id is a write built on no
// company.
func (c *Configs) InsertOn(ctx context.Context, base, r Revision) (string, error) {
	if r.ParentID != base.ID {
		return "", fmt.Errorf("store: revision built on %q names %q as its parent",
			base.ID, r.ParentID)
	}
	if err := written(r); err != nil {
		return "", err
	}
	if base.ID != "" {
		if err := origin(base, "keeping the base"); err != nil {
			return "", err
		}
	}
	id, at, payload := minted(r)
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		if base.ID != "" {
			if err := insertRow(ctx, tx, keepSQL, base.ID, base, base.CreatedAt,
				orEmpty(base.Payload), false, nil); err != nil {
				return err
			}
		}
		return insertRow(ctx, tx, insertSQL, id, r, at, payload, false, nil)
	})
	if err != nil {
		return "", fmt.Errorf("store: insert config revision: %w", err)
	}
	logStored(ctx, id, r, "history")
	return id, nil
}

// SawFleet records r as the fleet's revision as this node last saw it, keeping
// a copy of it as history where this node holds none.
//
// The reconciler's verb, and only the reconciler's: it is called for every
// pointer revision the tick has opened under this node's keyring, BEFORE it is
// validated or built, because what it records is what the fleet is on — which
// is true whether or not this build can run it. A node that refused R still
// knows the company is R, and that is the revision it restores when the
// pointer is gone ([Configs.Restorable]); the P it runs is what it boots on,
// which this does not touch.
//
// AUTHENTICATED BY THE CALLER: r is a peer's body, and only one that opened
// under the fleet's keyring may become this node's history.
//
// The row's instant rises to r.ActivatedAt and never falls, on the boot row as
// on any other: a re-activation of the revision this node runs is the fleet's
// whether or not this node applies it. What the boot row applied is
// [Revision.ServedAt], which this does not touch.
//
// # The author is the ORIGIN's, never this node's
//
// The caller passes the author, kind, credential, source and creation instant
// the fleet's pointer carries, so a revision reads the same on every node
// rather than "peer" everywhere but the one it was written on. The kind is NOT
// held to [iam.ActorKind.Valid] here, as a local write's is: an unknown kind
// off the wire is a value, and refusing it would stop this node recording the
// revision its fleet is on. But it must be SOME kind, as must the source and
// both instants: the pointer always carries them, and a row missing one is a
// guess on the audit screen. The credential alone may be empty — a revision no
// credential wrote, the engine's own, names none. A row that is already here
// keeps its body and its author: the row this node wrote, or kept first, is the
// authority on that write.
func (c *Configs) SawFleet(ctx context.Context, r Revision) error {
	if err := fleetCopy(r, "recording the fleet's revision"); err != nil {
		return err
	}
	at := EncodeTime(r.ActivatedAt)
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := insertRow(ctx, tx, keepSQL, r.ID, r, r.CreatedAt,
			orEmpty(r.Payload), false, nil); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, unfleetsSQL); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE company_config SET is_fleets = 1 WHERE revision_id = ?`, r.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, raiseSQL, at, r.ID, at)
		return err
	})
	if err != nil {
		return fmt.Errorf("store: record the fleet's config revision %s: %w", r.ID, err)
	}
	log.InfoContext(ctx, "config_revision_seen", "revision", r.ID, "source", r.Source,
		"by", r.CreatedBy, "by_kind", string(r.CreatedByKind), "operator", r.OperatorID,
		"activated_at", r.ActivatedAt)
	return nil
}

// Serve makes r what this node boots on, at the activation instant it was
// applied from ([Revision.ServedAt]), keeping a copy of it as history where this
// node holds none, and raising the row's [Revision.ActivatedAt] to that instant
// as [Configs.SawFleet] would.
//
// The reconciler's verb, called only once the engine has APPLIED r: that is the
// whole of the guarantee that a node boots on something it can run. A revision
// this node refused never gets here, so its next start boots on the one it
// served instead, rather than being refused before the engine runs.
//
// It replaces an offline offer the fleet never took: the node runs the fleet's
// revision now, and what an offline command chose stays as history. The caller
// says so.
//
// The copy's author is the origin's, as [Configs.SawFleet] keeps it.
func (c *Configs) Serve(ctx context.Context, r Revision) error {
	if err := fleetCopy(r, "serving a revision"); err != nil {
		return err
	}
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := insertRow(ctx, tx, keepSQL, r.ID, r, r.CreatedAt,
			orEmpty(r.Payload), false, nil); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, unbootSQL); err != nil {
			return err
		}
		at := EncodeTime(r.ActivatedAt)
		if _, err := tx.ExecContext(ctx,
			`UPDATE company_config SET is_active = 1, served_at = ? WHERE revision_id = ?`,
			at, r.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, raiseSQL, at, r.ID, at)
		return err
	})
	if err != nil {
		return fmt.Errorf("store: serve config revision %s: %w", r.ID, err)
	}
	log.InfoContext(ctx, "config_revision_served", "revision", r.ID,
		"activated_at", r.ActivatedAt)
	return nil
}

// NoteActivation records that the fleet activated a revision this node holds
// at the instant given, raising its history row's instant and never lowering
// it.
//
// Called by a writer once its activation LANDED — an API write, a seed, a boot
// publish — with the pointer's instant, which may be later than the one it
// asked for ([coord.ActivationAt]): the history shows the activation, and the
// retention sweep dates the row by it. It does not make the revision what this
// node boots on, which only an apply does, and it leaves the instant the boot
// row was applied at ([Revision.ServedAt]) alone.
func (c *Configs) NoteActivation(ctx context.Context, revisionID string, at time.Time) error {
	encoded := EncodeTime(at)
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		var held int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM company_config WHERE revision_id = ?`,
			revisionID).Scan(&held); err != nil {
			return err
		}
		if held == 0 {
			return fmt.Errorf("%w: %s", ErrNoRevision, revisionID)
		}
		_, err := tx.ExecContext(ctx, raiseSQL, encoded, revisionID, encoded)
		return err
	})
	if errors.Is(err, ErrNoRevision) {
		return err
	}
	if err != nil {
		return fmt.Errorf("store: record the activation of revision %s: %w", revisionID, err)
	}
	return nil
}

// Offer writes a new revision an offline command made and makes it what this
// node boots on, as a choice the fleet has not taken yet, built on over. It
// returns the revision's id.
//
// # The offline paths only
//
// A command run while the engine is stopped cannot move the pointer, so what
// it writes is a choice this node starts on and publishes at its next start.
// It carries what it was built on — what this node knew the company to be when
// the command ran ([Configs.Restorable]) — because the publish is a
// compare-and-set against it: a change made on a node that was behind its
// fleet is built on a revision the fleet has moved past, and publishing it
// anyway rolled the fleet back to the stale document underneath it. An empty
// over.Base is a change built on no company, and over.Unconfirmed the offer it
// replaced, where a start had sent that one and never confirmed it
// ([Offer.Unconfirmed]). over.PlannedAt must be zero: only a boot plans.
//
// NO ACTIVATION INSTANT: nothing has activated it. The boot passes the engine
// none for an offer, and the instant comes from the first apply of the
// activation that publishes it.
//
// ONE transaction covers clearing the old boot row and the insert. The partial
// unique index refuses two boot rows, so a clear that landed without its
// insert would leave this node with nothing to start on.
func (c *Configs) Offer(ctx context.Context, over Offer, r Revision) (string, error) {
	if err := written(r); err != nil {
		return "", err
	}
	if err := unplanned(over); err != nil {
		return "", err
	}
	id, at, payload := minted(r)
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, unbootSQL); err != nil {
			return err
		}
		return insertRow(ctx, tx, insertSQL, id, r, at, payload, true, &over)
	})
	if err != nil {
		return "", fmt.Errorf("store: insert config revision: %w", err)
	}
	logStored(ctx, id, r, "offered over "+over.String())
	return id, nil
}

// OfferStored makes a revision this node already holds what it boots on, as a
// choice the fleet has not taken yet, built on over.
//
// The offline `crewlet config activate`: [Configs.Offer] for a revision that
// exists. Its activation instant is kept, as the last one the fleet gave it;
// it has no served one, since nothing applied the choice.
//
// Re-activating the revision that is already the company is a supported
// gesture, not a no-op: it is how an operator asks a fleet to re-resolve its
// ${VAR} references and pick up a rotated credential, so it is an offer like
// any other, built on itself, and the boot publishes it as a new activation.
func (c *Configs) OfferStored(ctx context.Context, revisionID string, over Offer) error {
	if err := unplanned(over); err != nil {
		return err
	}
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, unbootSQL); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE company_config SET is_active = 1, offered = 1, offered_over = ?,
			     offered_unconfirmed = ?, offered_at = NULL, served_at = NULL
			 WHERE revision_id = ?`, NullText(over.Base), NullText(over.Unconfirmed), revisionID)
		if err != nil {
			return err
		}
		// Checked through RowsAffected, and inside the transaction: without
		// the check it commits having cleared the boot row, which is a node
		// with nothing to start on.
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s", ErrNoRevision, revisionID)
		}
		return nil
	})
	if errors.Is(err, ErrNoRevision) {
		return err
	}
	if err != nil {
		return fmt.Errorf("store: offer revision %s: %w", revisionID, err)
	}
	log.InfoContext(ctx, "config_revision_offered", "revision", revisionID,
		"base", over.String())
	return nil
}

// PlanOffer records, on the offer the boot publish is about to publish, the
// instant it will ask the pointer for and the revision it publishes over — the
// one the pointer named, of the two [Offer.Over] accepts. See
// [Offer.PlannedAt].
//
// THE BASE IS SETTLED HERE: the pointer named over, so the fleet runs it, and
// an unconfirmed offer the change was also safe over is no longer a candidate
// — it landed and is over, or it never did and the fleet is on the base. A
// change an offline command builds on this offer next has one base, not two.
//
// [ErrNotOffered] when the revision is not this node's unpublished offer, with
// nothing written: a plan recorded on any other row would say a publish of a
// revision nobody offered was under way. A boot that holds the store from its
// read, as `crewlet run` does, meets this only as a defect.
func (c *Configs) PlanOffer(ctx context.Context, revisionID string, at time.Time, over string) error {
	return c.onOffer(ctx, revisionID, "plan",
		`UPDATE company_config SET offered_at = ?, offered_over = ?, offered_unconfirmed = NULL
		 WHERE revision_id = ? AND `+bootRow+` AND offered <> 0`,
		EncodeTime(at), NullText(over), revisionID)
}

// ConfirmOffer records that the fleet took this node's offer: it stays what
// the node boots on, as a revision rather than a choice.
//
// Its served instant is NOT written here ([Revision.ServedAt]): that is the
// instant of the activation this node APPLIED, and the publish is not an apply
// — the reconciler's first apply of the published epoch serves it at the
// pointer's instant. Until then the node boots on it at none, as it boots on an
// offer.
func (c *Configs) ConfirmOffer(ctx context.Context, revisionID string) error {
	return c.onOffer(ctx, revisionID, "confirm",
		`UPDATE company_config SET offered = 0, offered_over = NULL,
		     offered_unconfirmed = NULL, offered_at = NULL
		 WHERE revision_id = ? AND `+bootRow+` AND offered <> 0`, revisionID)
}

// onOffer runs one statement aimed at the boot row's offer, reporting
// [ErrNotOffered] when it named no such row.
func (c *Configs) onOffer(ctx context.Context, revisionID, verb, statement string, args ...any) error {
	err := c.db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s", ErrNotOffered, revisionID)
		}
		return nil
	})
	if errors.Is(err, ErrNotOffered) {
		return err
	}
	if err != nil {
		return fmt.Errorf("store: %s the offer of revision %s: %w", verb, revisionID, err)
	}
	return nil
}

// Boot returns the revision this node starts on, and whether there is one.
func (c *Configs) Boot(ctx context.Context) (Revision, bool, error) {
	return c.one(ctx, bootSQL)
}

// Fleets returns the fleet's revision as this node last saw it, and whether
// it has seen one.
func (c *Configs) Fleets(ctx context.Context) (Revision, bool, error) {
	return c.one(ctx, fleetsSQL)
}

// Restorable is the company as this node knows it, for when the pointer cannot
// say: what the API answers with no pointer, what the boot republishes where
// the fleet's pointer is gone, and what an offline command builds on.
//
//   - An OFFER built on the fleet's revision this node last saw — its base,
//     or the unconfirmed offer it replaced ([Offer.Over]) — or built while it
//     had seen none: that is a change to the company somebody made here, on
//     what they knew it to be.
//   - Otherwise the FLEET's revision as this node last saw it — even one this
//     build refused. A node that runs P because it could not run R must not
//     offer P as the company: every node that could run R does, and P would
//     roll them back.
//   - Otherwise the boot row: a node that never saw a pointer runs what it was
//     given.
//
// An offer whose base is not the fleet's revision is a change made on
// something the fleet has moved past, and it is not the company.
//
// ONE SNAPSHOT for both rows, so a reconciler recording a new fleet revision
// between the two reads cannot pair an offer with a fleet row it was never
// compared with.
func (c *Configs) Restorable(ctx context.Context) (Revision, bool, error) {
	var boot, fleets Revision
	var booted, seen bool
	err := c.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		if boot, booted, err = oneIn(ctx, tx, bootSQL); err != nil {
			return err
		}
		fleets, seen, err = oneIn(ctx, tx, fleetsSQL)
		return err
	})
	if err != nil {
		return Revision{}, false, err
	}
	switch {
	case booted && boot.Offer != nil && (!seen || boot.Offer.Over(fleets.ID)):
		return boot, true, nil
	case seen:
		return fleets, true, nil
	case booted:
		return boot, true, nil
	}
	return Revision{}, false, nil
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
	return firstRevision(rows)
}

// oneIn is [Configs.one] inside a transaction.
func oneIn(ctx context.Context, tx *sql.Tx, query string, args ...any) (Revision, bool, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return Revision{}, false, fmt.Errorf("store: read config revision: %w", err)
	}
	return firstRevision(rows)
}

func firstRevision(rows *sql.Rows) (Revision, bool, error) {
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
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
	var parent, offeredOver, unconfirmed sql.NullString
	var payload string
	var createdAt int64
	var activatedAt, offeredAt, servedAt sql.NullInt64
	var active, fleets, offered int64
	var kind string
	if err := rows.Scan(&r.ID, &parent, &createdAt, &r.CreatedBy,
		&kind, &r.OperatorID, &r.Source, &r.Summary, &payload,
		&active, &activatedAt, &fleets, &offered, &offeredOver, &unconfirmed,
		&offeredAt, &servedAt); err != nil {
		return Revision{}, fmt.Errorf("store: read config revision: %w", err)
	}
	r.ParentID = Text(parent)
	r.CreatedByKind = iam.ActorKind(kind)
	r.CreatedAt = DecodeTime(createdAt)
	r.Payload = json.RawMessage(payload)
	r.Boots = active != 0
	r.Fleets = fleets != 0
	r.ActivatedAt = TimeAt(activatedAt)
	r.ServedAt = TimeAt(servedAt)
	if offered != 0 {
		r.Offer = &Offer{Base: Text(offeredOver), Unconfirmed: Text(unconfirmed),
			PlannedAt: TimeAt(offeredAt)}
	}
	return r, nil
}

// Purge deletes revisions older than cutoff, keeping what this node boots on,
// the fleet's revision as it last saw it, the chain of ancestors behind each,
// and every revision activated since the cutoff, and reports how many went.
//
// # Why this table needs a sweep at all
//
// Every node keeps its OWN copy of every revision it ever met, in an
// append-only table, and nothing deleted from it — so a company that edits
// its configuration daily accumulates a row per edit per node for the life of
// the deployment, and each row is a copy of the whole document — every seat's
// address among it, which this is what eventually removes.
//
// # What is kept whatever its age
//
// The boot row, because a node whose boot row went has nothing to start on;
// the fleet's row, because it is what the node restores when the pointer is
// gone, and it is often NOT the boot row — a node that refused the fleet's
// revision runs an older one. And the chain behind each: deleting an ancestor
// breaks a revert and a diff, and a company that has not changed its
// configuration for a year has a revision OLDER than any horizon worth
// setting. So both are excluded by id rather than by date.
//
// A row is dated by the later of its creation and the latest activation this
// node knows of ([Revision.ActivatedAt]), so an old revision the fleet
// re-activated yesterday is kept even before this node's reconciler has
// recorded it as the fleet's — and even when it is the revision this node runs
// and refused that re-activation.
//
// THE CHAINS ARE READ INSIDE THE TRANSACTION THAT DELETES, so a reconciler
// moving either row between the read and the delete cannot have its new row's
// ancestors swept on the strength of the old one's chain.
func (c *Configs) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := c.db.Tx(ctx, func(tx *sql.Tx) (err error) {
		keep, err := keptIn(ctx, tx)
		if err != nil {
			return err
		}
		// BOUND PARAMETERS, one per kept revision, rather than an
		// interpolated list: the chains are short and the ids are uuids this
		// process wrote, but a query built by concatenation is a query
		// somebody later feeds something else.
		placeholders := make([]byte, 0, 2*len(keep))
		args := []any{EncodeTime(cutoff), EncodeTime(cutoff)}
		for i, id := range keep {
			if i > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args = append(args, id)
		}
		doomed := `created_at < ? AND (activated_at IS NULL OR activated_at < ?)`
		if len(keep) > 0 {
			doomed += ` AND revision_id NOT IN (` + string(placeholders) + `)`
		}
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
		// ever written has no parent. The chain walk stops at a break for
		// this reason among others.
		if _, err = tx.ExecContext(ctx,
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

// keptIn is every revision the sweep may not delete by id: the boot row, the
// fleet's row and every ancestor each reaches through parent_revision_id.
//
// STOPS AT THE FIRST BREAK rather than reporting one. A chain is broken from
// the start on every revision this node holds as a copy of the fleet's — the
// pointer carries no parent, because it would name a revision the copying node
// may never have held — and a sweep may already have cut an older one. That is
// an ordinary state rather than damage, so the sweep protects what it can
// reach and lets the horizon cover the rest.
//
// A VISITED SET, because a cycle in parent pointers would otherwise be an
// infinite loop inside a maintenance tick. Nothing writes one, which is
// exactly why nothing would notice it.
func keptIn(ctx context.Context, tx *sql.Tx) ([]string, error) {
	var keep []string
	seen := map[string]bool{}
	for _, marked := range []string{bootSQL, fleetsSQL} {
		at, found, err := oneIn(ctx, tx, marked)
		if err != nil {
			return nil, err
		}
		for found && !seen[at.ID] {
			seen[at.ID] = true
			keep = append(keep, at.ID)
			if at.ParentID == "" {
				break
			}
			if at, found, err = oneIn(ctx, tx,
				`SELECT `+revisionColumns+` FROM company_config WHERE revision_id = ?`,
				at.ParentID); err != nil {
				return nil, err
			}
		}
	}
	return keep, nil
}

// written refuses a revision a local write may not store: one whose author's
// kind is not this build's, or one that claims a mark only a verb may set.
//
// THE KIND IS REQUIRED, and held to [iam.ActorKind.Valid]. Every writer on
// this node knows whether it is a person, a credential or the engine, and a
// revision stored without saying is exactly the row the audit screen used to
// fill in with a guess. Only a WRITE is held to it: a copy of a fleet revision
// stores the kind the fleet's pointer carries, which may be one a newer build
// added — an unknown kind off the wire is a value, and refusing it would stop
// this node recording a revision its fleet is on.
func written(r Revision) error {
	if !r.CreatedByKind.Valid() {
		return fmt.Errorf("store: a config revision needs its author's kind "+
			"(one of %s), got %q", words.Or(iam.ActorKinds), r.CreatedByKind)
	}
	if r.Boots || r.Fleets || r.Offer != nil || !r.ActivatedAt.IsZero() || !r.ServedAt.IsZero() {
		return fmt.Errorf("store: a new config revision cannot arrive marked: "+
			"what this node boots on, the fleet's revision, an offline offer and "+
			"an activation or applied instant are each set by the verb that says so "+
			"(boots=%v fleets=%v offer=%v activated=%v served=%v)",
			r.Boots, r.Fleets, r.Offer != nil, !r.ActivatedAt.IsZero(), !r.ServedAt.IsZero())
	}
	return nil
}

// unplanned refuses an offline command's offer that claims a planned instant:
// only the boot publish plans one ([Configs.PlanOffer]), and an offer that
// arrived planned would read as a publish this node may have landed.
func unplanned(over Offer) error {
	if !over.PlannedAt.IsZero() {
		return fmt.Errorf("store: an offline offer cannot arrive planned: only the "+
			"boot publish plans one (planned at %s)", over.PlannedAt)
	}
	if over.Unconfirmed != "" && over.Unconfirmed == over.Base {
		return fmt.Errorf("store: an offline offer names %s as both its base and the "+
			"unconfirmed offer it replaced", over.Base)
	}
	return nil
}

// String is an offer's base as a log line says it: the base, "(no company)"
// for none, and the unconfirmed offer beside it where there is one.
func (o Offer) String() string {
	if o.Unconfirmed == "" {
		return orNothing(o.Base)
	}
	return orNothing(o.Base) + " or the unconfirmed " + o.Unconfirmed
}

// fleetCopy refuses a copy of a fleet revision that names nobody: the pointer
// always carries the origin and the instant, so a row missing one would be a
// guess on the audit screen.
func fleetCopy(r Revision, doing string) error {
	if err := origin(r, doing); err != nil {
		return err
	}
	if r.ActivatedAt.IsZero() {
		return fmt.Errorf("store: %s %s needs the instant the fleet activated it", doing, r.ID)
	}
	return nil
}

// origin refuses a fleet revision without the identity and origin its pointer
// carries. The credential alone may be empty — the engine's own write names
// none.
func origin(r Revision, doing string) error {
	switch {
	case r.ID == "":
		return fmt.Errorf("store: %s needs the revision's fleet id", doing)
	case r.CreatedByKind == "":
		return fmt.Errorf("store: %s %s needs its author's kind", doing, r.ID)
	case r.Source == "":
		return fmt.Errorf("store: %s %s needs its source", doing, r.ID)
	case r.CreatedAt.IsZero():
		return fmt.Errorf("store: %s %s needs its creation instant", doing, r.ID)
	}
	return nil
}

// minted is a new revision's id, creation instant and body, each defaulted.
func minted(r Revision) (string, time.Time, json.RawMessage) {
	id := r.ID
	if id == "" {
		id = uuid.NewString()
	}
	at := r.CreatedAt
	if at.IsZero() {
		at = now()
	}
	return id, at, orEmpty(r.Payload)
}

func orEmpty(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 {
		return json.RawMessage("{}")
	}
	return payload
}

func orNothing(base string) string {
	if base == "" {
		return "(no company)"
	}
	return base
}

// insertRow is the one INSERT every write shares, so two rows cannot differ in
// anything but what their writer states. A row's activation instant is the
// one r carries, NULL for none; boots and offer mark the boot row an offline
// command writes.
func insertRow(ctx context.Context, tx *sql.Tx, statement, id string, r Revision,
	createdAt time.Time, payload json.RawMessage, boots bool, offer *Offer) error {

	isActive, offered, over, unconfirmed := 0, 0, any(nil), any(nil)
	if boots {
		isActive = 1
	}
	if offer != nil {
		offered, over, unconfirmed = 1, NullText(offer.Base), NullText(offer.Unconfirmed)
	}
	_, err := tx.ExecContext(ctx, statement,
		id, NullText(r.ParentID), EncodeTime(createdAt), r.CreatedBy,
		string(r.CreatedByKind), r.OperatorID, r.Source, r.Summary,
		string(payload), isActive, NullTime(r.ActivatedAt), offered, over, unconfirmed)
	return err
}

func logStored(ctx context.Context, id string, r Revision, as string) {
	log.InfoContext(ctx, "config_revision_stored",
		"revision", id, "source", r.Source, "by", r.CreatedBy,
		"by_kind", string(r.CreatedByKind), "operator", r.OperatorID, "as", as)
}
