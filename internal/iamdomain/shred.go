package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/secrets"
)

// THE DUTY THAT FINISHES WHAT A KEY WAS MINTED FOR: a removed person's key
// destroyed, retried until it lands, and a key NOBODY OWNS destroyed once it is
// certainly nobody's.
//
// # A removal's key
//
// A removal is a record and a key deletion, in that order, and only the first
// half is on the log. The applier destroys the key AFTER the rows commit
// ([Applier.Committed]), because destroying it inside the transaction would
// destroy it for a removal that then rolled back — and it does so best effort,
// because failing the apply over a coordination blip would stall the whole
// fleet's log for a deletion that is already durable in every row. So the
// residue of a failed shred is a LEGAL NAMED STATE: a row in `iam_removed`
// that says this person is gone, and a key in the fleet secret store that says
// their name and address can still be read out of every backup, every donated
// snapshot and every record still on the log.
//
// That residue is the ONLY deletion mechanism this estate has for those
// copies, and the secret store's bucket deliberately has no age — so a key
// whose delete failed would live for ever. The tombstone is the durable record
// of what still needs destroying, and a pass reads it. A tombstone is also
// DEFINITIVE on whichever node holds it — nobody comes back from a removal —
// so this arm needs no grace and no proof the node is current.
//
// # A key nobody owns
//
// A key is minted BEFORE the record that makes it somebody's, because the
// record carries values sealed under it: an enrolment mints the person's key
// and then claims their address, an invitation mints its own and then
// publishes itself. So a gesture that stops in between leaves a key with no
// row, no reservation and no tombstone — two administrators adding one joiner
// is exactly that, the second refused on the address after its key was
// minted — and an invitation the sweep collected leaves its key behind the
// same way. Nothing else would ever name such a key or destroy it.
//
// It is destroyed only when BOTH hold: it is older than [OrphanKeyGrace], so it
// cannot be a gesture still running; and the node deciding has applied
// everything the log held when it asked, because a row this node has not
// applied reads exactly like a row that does not exist — and destroying the key
// of somebody whose enrolment simply has not arrived here is an irreversible
// shred of a person nobody removed.
//
// # Why it lists the store rather than walking the rows
//
// Every tombstone the company ever wrote stays (it is the removal gate), so a
// pass that tried to shred each would be one secret-store call per person who
// ever left, every interval, for the life of the deployment. Listing the keys
// that EXIST is one read, and comparing it with who owns them leaves exactly
// the keys that should be gone — which on a healthy fleet is none.

// OrphanKeyGrace is how old a key nobody owns must be before the key duty
// destroys it.
//
// [OrphanGrace]'s HOUR, and for its reason: the gestures that mint a key and
// then publish the record that owns it — an enrolment's first claim, an
// invitation — each finish inside one request, bounded by the publisher's
// resolve budget of seconds. A key unowned after an hour belongs to a request
// that ended long ago; the same hour the claim report waits before naming the
// reservation such a request can leave.
const OrphanKeyGrace = OrphanGrace

// KeyIndex is the fleet secret store as the key duty reads it: which keys
// exist under a prefix and when each was written, and nothing else.
//
// CONSUMER-DEFINED and one method wide. [fleetsecrets.Estate.Keys] satisfies
// it, and it needs no keyring — nor does destroying a key — so a node that
// cannot decrypt anything can still finish somebody's off-boarding.
type KeyIndex interface {
	Keys(ctx context.Context, prefix string) ([]secrets.Record, error)
}

// UnownedKey is one person key no row on this node owns.
type UnownedKey struct {
	// ID is the id the key was minted for: a person nobody enrolled, or
	// an invitation the sweep collected.
	ID string

	// WrittenAt is when the key was last written — its mint, or a rekey
	// since — which is what it is aged by.
	WrittenAt time.Time
}

// KeyCensus is which person keys the store holds, and what this node's rows say
// about each one that should not be there.
type KeyCensus struct {
	// Keys is how many person keys the store holds — the denominator an
	// operator reads the rest against.
	Keys int

	// OutlivedRemoval are removed people whose key still exists.
	OutlivedRemoval []string

	// Unowned are keys no person, reservation, invitation or removal on
	// this node owns, oldest first.
	Unowned []UnownedKey
}

// KeyCensus reads which person keys exist and who, on this node, owns each —
// what the key duty acts on and what the directory report names while it waits.
//
// ONE LISTING AND ONE READ TRANSACTION: the owners are read in the same
// snapshot, so a key is judged against one state of the estate. It JUDGES
// NOTHING: an unowned key here may be one a running gesture is about to claim
// with, or one whose owner this node has not applied, and telling those apart
// is [ShredKeys]'s grace and its caught-up question.
func (r *Reader) KeyCensus(ctx context.Context, keys KeyIndex) (KeyCensus, error) {
	listed, err := keys.Keys(ctx, personKeyPrefix)
	if err != nil {
		return KeyCensus{}, fmt.Errorf("iamdomain: list the person keys: %w", err)
	}
	census := KeyCensus{Keys: len(listed)}
	if len(listed) == 0 {
		return census, nil
	}
	var removed, owned map[string]bool
	err = r.scan(ctx, func(tx *sql.Tx) error {
		var err error
		if removed, err = idsIn(ctx, tx, `SELECT person_id FROM iam_removed`); err != nil {
			return fmt.Errorf("iamdomain: read the removals: %w", err)
		}
		// A PERSON ROW OWNS ITS KEY, and so does a RESERVATION — the half
		// of an enrolment a claim wrote before the person record — which
		// is a row in the same table with no kind. An INVITATION owns the
		// key minted under its own id.
		if owned, err = idsIn(ctx, tx, `SELECT id FROM iam_people`); err != nil {
			return fmt.Errorf("iamdomain: read the people: %w", err)
		}
		invites, err := idsIn(ctx, tx, `SELECT id FROM iam_invites`)
		if err != nil {
			return fmt.Errorf("iamdomain: read the invitations: %w", err)
		}
		for id := range invites {
			owned[id] = true
		}
		return nil
	})
	if err != nil {
		return census, err
	}
	for _, key := range listed {
		id, ok := idOfPersonKey(key.Name)
		if !ok {
			// A NAME THIS BUILD DID NOT WRITE under the person prefix is
			// nobody's to judge here, least of all to destroy.
			continue
		}
		switch {
		case removed[id]:
			census.OutlivedRemoval = append(census.OutlivedRemoval, id)
		case !owned[id]:
			census.Unowned = append(census.Unowned,
				UnownedKey{ID: id, WrittenAt: key.UpdatedAt})
		}
	}
	slices.Sort(census.OutlivedRemoval)
	slices.SortFunc(census.Unowned, func(a, b UnownedKey) int {
		return a.WrittenAt.Compare(b.WrittenAt)
	})
	return census, nil
}

// idsIn reads one column of ids into a set.
func idsIn(ctx context.Context, tx *sql.Tx, query string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ShredReport is what one pass found and did.
type ShredReport struct {
	// Keys is how many person keys the store holds, removed or not.
	Keys int

	// Pending are the removed people whose key was still live when the
	// pass began: every failed shred since the last pass that landed.
	Pending []string

	// Destroyed are the ones this pass destroyed. A person in Pending and
	// not here is still recoverable, and the pass's error says why.
	Destroyed []string

	// Collected are keys nobody owned, past the grace, that this pass
	// destroyed.
	Collected []string

	// Waiting is how many keys nobody owns this pass left alone: inside the
	// grace — a gesture that may be running — or with no write time the
	// store recorded, or, while Unjudged is set, all of them.
	Waiting int

	// Unjudged is why the pass could not judge ownership at all — this
	// node is behind the identity log — or nil. Nothing unowned is
	// destroyed while it is set; the removals are, because a tombstone
	// is definitive wherever it is.
	Unjudged error
}

// ShredKeys destroys every key a removal left behind, and every key nobody
// owns that is certainly nobody's.
//
// caughtUp answers whether this node has applied everything the identity log
// held when it was asked, and is asked BEFORE the census is read: a key older
// than the grace was minted before the question, so the record that would own
// it — published seconds after the mint — is inside what the answer covers.
//
// EVERY KEY IS ATTEMPTED even when one fails: they are independent people, and
// one key the store would not delete must not keep the next person's name
// readable for another interval.
func ShredKeys(ctx context.Context, reader *Reader, keys KeyIndex,
	shredder Shredder, now time.Time, caughtUp func(context.Context) error) (
	ShredReport, error) {

	if reader == nil || keys == nil || shredder == nil || caughtUp == nil {
		return ShredReport{}, errors.New("iamdomain: the key duty needs the " +
			"directory, the store's keys, a shredder and a way to tell whether " +
			"this node is current")
	}
	judged := caughtUp(ctx)
	census, err := reader.KeyCensus(ctx, keys)
	report := ShredReport{Keys: census.Keys, Pending: census.OutlivedRemoval,
		Unjudged: judged}
	if err != nil {
		return report, err
	}
	var errs []error
	for _, id := range report.Pending {
		if ctx.Err() != nil {
			return report, errors.Join(append(errs, ctx.Err())...)
		}
		// DESTROYED WHETHER OR NOT IT WAS STILL THERE: a key a peer's
		// applier destroyed between the listing and this call is a
		// removal that landed, which is the outcome this pass exists for.
		if _, err := shredder.Shred(ctx, id); err != nil {
			errs = append(errs, err)
			continue
		}
		report.Destroyed = append(report.Destroyed, id)
	}
	if judged != nil {
		report.Waiting = len(census.Unowned)
		return report, errors.Join(errs...)
	}
	cutoff := now.Add(-OrphanKeyGrace)
	for _, key := range census.Unowned {
		if ctx.Err() != nil {
			return report, errors.Join(append(errs, ctx.Err())...)
		}
		// A KEY WITH NO RECORDED WRITE TIME HAS NO PROVABLE AGE, and an age
		// is what separates a key nobody owns from one a running gesture is
		// about to claim with — so it waits rather than reading as the
		// oldest key there is.
		if key.WrittenAt.IsZero() || !key.WrittenAt.Before(cutoff) {
			report.Waiting++
			continue
		}
		if _, err := shredder.Shred(ctx, key.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		report.Collected = append(report.Collected, key.ID)
	}
	return report, errors.Join(errs...)
}

// RemovedPeople is every person a removal tombstoned on this node, in id
// order.
//
// FROM THE TOMBSTONE rather than from a record: a removal below the trim floor
// has no record left on the log, and its row is what still says it happened.
func (r *Reader) RemovedPeople(ctx context.Context) ([]string, error) {
	var out []string
	err := r.scan(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT person_id FROM iam_removed ORDER BY person_id`)
		if err != nil {
			return fmt.Errorf("iamdomain: read the removals: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("iamdomain: scan a removal: %w", err)
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
