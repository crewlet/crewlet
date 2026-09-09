package pages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier, and the four rules that make it a PURE FUNCTION of the log.
//
// N nodes derive one SQL state from one ordered stream, and the claim that
// their tables are byte-identical is checkable — a checksum over the
// replicated file. That claim survives exactly as long as these four hold, and
// they are the tracker's four because they are properties of the framework
// rather than of either domain:
//
//  1. IT READS NO CONFIG AND NO CLOCK. Everything it would otherwise read
//     arrives in [statelog.ApplyOptions]: the batch's instant, the broker's own
//     timestamp, and the epoch values the domain declared it reads.
//  2. IT WRITES ONLY WHAT THE RECORD SAYS. No lookup against a live counter,
//     no "current" anything: the record carries its complete new values,
//     because that is what makes a replay from zero produce the same rows as
//     the original apply did. The revision prune is the sharpest case — the
//     list of retired versions RIDES THE COMMIT rather than being recomputed.
//  3. EVERY DERIVED INSTANT IS A MAX OVER A SET, never a fold over an arrival
//     order. Two nodes at one checkpoint have seen the same set and a different
//     order, so a fold gives them different answers.
//  4. EVERY UPSERT CARRIES ITS VERSION GUARD, and the guard is a SKIP rather
//     than an error: a redelivered record is ordinary traffic, and a constraint
//     violation inside this transaction would abort it identically on every
//     node and stall the whole fleet's log.
//
// # The one thing it does that the tracker's does not
//
// It writes a DIVERGENT row. `pages_skills` is this build's parser answering
// about this build's rules, so it is recomputed on every apply and excluded
// from the identity claim — which is exactly what the Divergent class names,
// and what lets a parser fix reach every existing page on the next rebuild
// rather than only the pages edited since.

// Applier writes this node's copy of the knowledge base.
type Applier struct {
	// NodeID is this node's own id, which the eviction gate compares a
	// record's writer against.
	NodeID string

	// skills is the tool-skill parser, and may be nil.
	skills SkillDetector

	// touched is called after a committed batch that changed a TOOL-SKILL
	// page, so a registry can re-read the container.
	//
	// # Why the applier is what notices
	//
	// A tool skill reaches the registry by being READ OUT of its
	// container, and something has to say when to read again. On the
	// vendor backend that is a page webhook; natively the change feed
	// deliberately drops these changes — a skill is machinery, and waking
	// a team about an edit to a procedure written for one phase of one
	// turn is exactly what the quiet rule exists to prevent — so there is
	// no delivery to hang the resync off.
	//
	// The apply is the other thing that sees every page change, and it
	// already computes the skill flag. Nil is legal and means nobody is
	// listening.
	touched func()

	// skillMoved is set inside Apply and drained by Committed.
	//
	// NOT GUARDED BY A MUTEX, unlike the projection applier this replaces,
	// and the reason is the framework's own contract: an applier is ONE
	// writer, and Apply and Committed are called from the same goroutine
	// with the commit in between. The projection had two writers racing on
	// one field; this has one.
	skillMoved bool
}

// NewApplier builds the knowledge base's applier for one node.
//
// onSkillChange is called after a committed batch that touched a tool-skill
// page. AFTER THE COMMIT, never inside the transaction: the store's
// transactions are optimistic and a conflicted one re-runs its body, so a
// callback inside Apply would fire twice for one record.
func NewApplier(nodeID string, skills SkillDetector, onSkillChange func()) *Applier {
	return &Applier{NodeID: nodeID, skills: skills, touched: onSkillChange}
}

// Committed is the post-commit half.
//
// The one consequence of a pages record that is not a row: a tool-skill page
// moved, and the registry has to read its container again. Every OTHER
// consequence is a row, and a wake is derived by the change feed — something
// that outlives this process — rather than published here as a courtesy.
func (a *Applier) Committed(context.Context) {
	if !a.skillMoved {
		return
	}
	a.skillMoved = false
	if a.touched != nil {
		a.touched()
	}
}

// Gated reports a record that must produce no rows at all.
//
// TWO GATES, READ FROM THIS SAME TRANSACTION, because the answer has to come
// from the state the record would have applied against rather than from a
// cache that may be a heartbeat old:
//
//  1. THE EVICTION GATE. A record written by a node the fleet evicted before
//     the record's own position is dropped everywhere. It depends on nothing
//     but the log's own order, which is what makes it the fence that holds
//     when coordination cannot be reached at all.
//  2. THE DELETION GATE. A record about a page a purge destroyed applies
//     nowhere, for ever — otherwise a redelivery months later would resurrect
//     a page an operator deliberately removed.
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (
	statelog.Reason, bool, error) {

	if rec.Writer != "" {
		var from, readmitted sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position
			FROM pages_evictions WHERE node_id = ?`, rec.Writer).
			Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return "", false, fmt.Errorf("pages: read the eviction gate for "+
				"node %s: %w", rec.Writer, err)
		default:
			at := rec.Position.Packed()
			// THE WINDOW IS HALF-OPEN AT BOTH ENDS, and both ends
			// matter: a record at or below the eviction's own
			// position was written while the node was still counted,
			// and one at or above a readmission is written by a node
			// the fleet has taken back.
			evicted := from.Valid && at > from.Int64
			back := readmitted.Valid && at >= readmitted.Int64
			if evicted && !back {
				return statelog.ReasonEvicted, true, nil
			}
		}
	}

	// The deletion gate reads the page's own marker. A purge is the one
	// operation that removes rows, and its marker is what makes the
	// removal permanent rather than a race a redelivery can undo.
	pageID, ok := gatedPage(rec)
	if !ok {
		return "", false, nil
	}
	var author sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT purge_record_id FROM pages_deletions WHERE page_id = ?`,
		pageID).Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("pages: read the deletion gate for page "+
			"%s: %w", pageID, err)
	}
	// THE ONE EXCEPTION IS THE RECORD THAT WROTE THE MARKER, by its own id
	// — not by its op kind. "Any purge" would let a SECOND purge of the
	// same page through, and a purge is the one operation that destroys
	// rows; by the committed sequence would fail for a republished copy,
	// leaving a node holding only that copy unable to write its own marker
	// at all.
	if author.Valid && author.String == rec.OpID {
		return "", false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE pages_deletions
		SET rejects = rejects + 1, last_reject_at = ?
		WHERE page_id = ?`,
		store.EncodeTime(rec.StoredAt), pageID); err != nil {
		return "", false, fmt.Errorf("pages: count a gate hit on the purged "+
			"page %s: %w", pageID, err)
	}
	return statelog.ReasonDeleted, true, nil
}

// gatedPage is the page a record is ABOUT, for the deletion gate.
//
// A PAGE SUBJECT NAMES ITS OWN, and a TITLE subject names one in its payload —
// which the gate cannot read, because a record at an unknown version reaches
// here with an opaque payload. So a title record is not gated on the page: it
// is gated on nothing, and its apply refuses to write a head for a page the
// deletions table holds. That keeps the gate answerable from the envelope
// while still making a purge permanent.
func gatedPage(rec statelog.Record) (string, bool) {
	if ObjectKind(rec.Subject.Kind) == KindPage {
		return rec.Subject.ID, true
	}
	return "", false
}

// Apply writes one record's rows.
//
// The dispatch is on the SUBJECT KIND, and every kind has a case — including
// the one that writes nothing, which is a case rather than a default so that a
// kind added later without one is a compile-time hole rather than a silently
// ignored record.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("pages: decode the record at %s: %w", rec.Position, err)
	}
	at := applyContext{
		record:   record,
		position: rec.Position,
		packed:   rec.Position.Packed(),
		brokerAt: opts.StoredAt,
		epoch:    opts.Epoch,
	}

	switch ObjectKind(rec.Subject.Kind) {
	case KindBarrier:
		// THE READ INDEX'S OWN APPEND. It writes no row on any node, and
		// that is its entire content: it exists so a linearizable read
		// has a quorum-committed position to wait through.
		return 0, nil
	case KindEviction:
		return a.applyEviction(ctx, tx, at)
	case KindGeneration:
		return a.applyGeneration(ctx, tx, at)
	case KindContainer:
		return a.applyContainer(ctx, tx, at)
	case KindTitle:
		return a.applyTitle(ctx, tx, at)
	case KindPage:
		return a.applyPage(ctx, tx, at)
	}
	// A KIND THIS BUILD DOES NOT KNOW REACHES HERE ONLY BY WAY OF A RECORD
	// AT A VERSION IT CAN READ, which is a writer publishing a kind it
	// never declared. Retaining it would file it under a kind nothing will
	// ever apply; failing is what makes the writer's mistake visible.
	return 0, fmt.Errorf("pages: %s is not a kind this build applies, and the "+
		"record at %s claims version %d — a kind is declared before it is "+
		"published", rec.Subject.Kind, rec.Position, record.V)
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position

	// packed is the composed position, which is the version every object
	// row's guard compares against.
	packed int64

	// brokerAt is the broker's own instant for this record. THE APPLIER
	// NEVER READS A CLOCK: this is what makes an effective instant
	// byte-identical on every node.
	brokerAt time.Time

	epoch map[string]any
}

// subject is the record's own subject.
func (c applyContext) subject() Subject { return c.record.Subject }

// authored is the writer's own instant, which is REPORTED and never ordered
// on.
func (c applyContext) authored() time.Time { return c.record.CreatedAt }

// applyEviction records a node's removal from this log, or its readmission.
//
// A READMISSION IS AN INVERSE COMMIT rather than a delete, so an eviction's
// whole history survives a replay — and a node that was evicted, readmitted
// and evicted again reads correctly rather than as one long absence.
func (a *Applier) applyEviction(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	e, ok := payload.(Eviction)
	if !ok {
		return 0, fmt.Errorf("pages: the eviction at %s carries a %T",
			at.position, payload)
	}
	if e.NodeID == "" {
		return 0, fmt.Errorf("pages: the eviction at %s names no node", at.position)
	}
	if e.Readmitted {
		res, err := tx.ExecContext(ctx, `
			UPDATE pages_evictions
			SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`,
			at.packed, at.packed, e.NodeID, at.packed)
		if err != nil {
			return 0, fmt.Errorf("pages: readmit node %s at %s: %w",
				e.NodeID, at.position, err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	// A SECOND EVICTION CLEARS THE READMISSION, which is what makes the
	// evicted-readmitted-evicted sequence read as three facts rather than
	// as one window with a hole in it.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO pages_evictions
			(node_id, at, by, from_position, readmitted_position, version)
		VALUES (?, ?, ?, ?, NULL, ?)
		ON CONFLICT (node_id) DO UPDATE SET
			at = excluded.at, by = excluded.by,
			from_position = excluded.from_position,
			readmitted_position = NULL,
			version = excluded.version
		WHERE excluded.version > pages_evictions.version`,
		e.NodeID, store.EncodeTime(at.brokerAt), e.EvictedBy, at.packed, at.packed)
	if err != nil {
		return 0, fmt.Errorf("pages: evict node %s at %s: %w",
			e.NodeID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyGeneration writes a reanchor's audit row.
func (a *Applier) applyGeneration(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	g, ok := payload.(Generation)
	if !ok {
		return 0, fmt.Errorf("pages: the generation record at %s carries a %T",
			at.position, payload)
	}
	// CREATE-ONLY. Two operators deriving the same number race at the
	// broker and exactly one wins; the loser's record never applies, so a
	// conflict here is a redelivery of the winner's.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO pages_log_generations
			(generation, at, by, new_stream_created_at, prev_last_seq_seen,
			 record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (generation) DO NOTHING`,
		g.Generation, store.EncodeTime(at.brokerAt), g.By,
		store.EncodeTime(g.StreamCreatedAt), int64(g.PrevHighest),
		at.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("pages: record generation %d at %s: %w",
			g.Generation, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyContainer upserts a space's settings.
func (a *Applier) applyContainer(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	if at.record.Op == OpPurge {
		return a.purgeContainer(ctx, tx, at)
	}
	c, ok := payload.(ContainerPayload)
	if !ok {
		return 0, fmt.Errorf("pages: the container record at %s carries a %T",
			at.position, payload)
	}
	document, err := EncodeContainer(Container{
		V: DocumentVersion, Key: c.Key, Name: c.Name, Purpose: c.Purpose,
		CreatedAt: at.brokerAt,
	})
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO pages_containers
			(key, name, purpose, created_at, version, scoped_through, document)
		VALUES (?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT (key) DO UPDATE SET
			name = excluded.name, purpose = excluded.purpose,
			version = excluded.version, document = excluded.document
		WHERE excluded.version > pages_containers.version`,
		c.Key, c.Name, c.Purpose, store.EncodeTime(at.brokerAt), at.packed,
		document)
	if err != nil {
		return 0, fmt.Errorf("pages: apply the container %s at %s: %w",
			c.Key, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// purgeContainer removes an empty space.
//
// IT REFUSES A SPACE THAT STILL HOLDS PAGES rather than cascading: a cascade
// is a delete nobody committed, and a container purge that removed a thousand
// pages would do it without a deletion marker for any of them.
func (a *Applier) purgeContainer(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	key := at.subject().ID
	var held int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pages_heads WHERE container = ?`, key).
		Scan(&held); err != nil {
		return 0, fmt.Errorf("pages: count %s's pages at %s: %w",
			key, at.position, err)
	}
	if held > 0 {
		// APPLIED COMPLETELY AS A NO-OP rather than refused, on the
		// tracker's own rule for a shape two concurrent writers can
		// legally create: a page created into the space between the
		// purge's decision and its commit is ordinary traffic, and
		// stalling every node's log over it would be the outage.
		return 0, nil
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM pages_containers WHERE key = ? AND version < ?`,
		key, at.packed)
	if err != nil {
		return 0, fmt.Errorf("pages: purge the container %s at %s: %w",
			key, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyTitle is a CREATE or a RENAME: the two operations the address
// arbitrates.
//
// # Why the whole create is here
//
// The record's subject is the title, so the broker has already decided which
// of two concurrent creates wins. The apply then writes the claim, the head,
// the first revision and the history entry in ONE transaction — which is what
// removes the three-key sequence the bucket forced and every crash state that
// came with it.
func (a *Applier) applyTitle(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	switch p := payload.(type) {
	case CreatePayload:
		return a.applyCreate(ctx, tx, at, p)
	case RenamePayload:
		return a.applyRename(ctx, tx, at, p)
	}
	return 0, fmt.Errorf("pages: the title record at %s carries a %T",
		at.position, payload)
}
