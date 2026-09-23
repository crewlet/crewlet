package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// THE DUTY THAT FINISHES A REMOVAL: a removed person's key destroyed, retried
// until it lands.
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
// whose delete failed would live for ever. This is what makes "a removal makes
// a name unrecoverable" true rather than usually true: the tombstone is the
// durable record of what still needs destroying, and this pass reads it.
//
// # Why it lists the store rather than walking the tombstones
//
// Every tombstone the company ever wrote stays (it is the removal gate), so a
// pass that tried to shred each would be one secret-store call per person who
// ever left, every interval, for the life of the deployment. Listing the keys
// that EXIST is one read, and intersecting it with the tombstones leaves
// exactly the keys a removal should have destroyed and did not — which on a
// healthy fleet is none.

// KeyIndex is the fleet secret store as the key duty reads it: which names
// exist under a prefix, and nothing else.
//
// CONSUMER-DEFINED and one method wide. [fleetsecrets.Store.Names] satisfies
// it, and it needs no keyring — nor does destroying a key — so a node that
// cannot decrypt anything can still finish somebody's off-boarding.
type KeyIndex interface {
	Names(ctx context.Context, prefix string) ([]string, error)
}

// ShredReport is what one pass found and did.
type ShredReport struct {
	// Keys is how many person keys the store holds, removed or not — the
	// denominator an operator reads the rest against.
	Keys int

	// Pending are the removed people whose key was still live when the
	// pass began: every failed shred since the last pass that landed.
	Pending []string

	// Destroyed are the ones this pass destroyed. A person in Pending and
	// not here is still recoverable, and the pass's error says why.
	Destroyed []string
}

// ShredRemoved destroys every key a removal left behind.
//
// EVERY PENDING KEY IS ATTEMPTED even when one fails: they are independent
// people, and one key the store would not delete must not keep the next
// person's name readable for another interval.
func ShredRemoved(ctx context.Context, reader *Reader, keys KeyIndex,
	shredder Shredder) (ShredReport, error) {

	if reader == nil || keys == nil || shredder == nil {
		return ShredReport{}, errors.New("iamdomain: the key duty needs the " +
			"directory, the store's names and a shredder")
	}
	held, pending, err := reader.KeysOutlivingRemovals(ctx, keys)
	report := ShredReport{Keys: held, Pending: pending}
	if err != nil {
		return report, err
	}
	var errs []error
	for _, id := range report.Pending {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
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
	return report, errors.Join(errs...)
}

// KeysOutlivingRemovals is every removed person whose key still exists — the
// key duty's work, and what the directory report names while it is pending —
// and how many person keys the store holds in all.
func (r *Reader) KeysOutlivingRemovals(ctx context.Context, keys KeyIndex) (
	int, []string, error) {

	names, err := keys.Names(ctx, personKeyPrefix)
	if err != nil {
		return 0, nil, fmt.Errorf("iamdomain: list the person keys: %w", err)
	}
	if len(names) == 0 {
		return 0, nil, nil
	}
	// SORTED HERE and not trusted to arrive so: the seam promises names,
	// and the membership test below is a binary search.
	names = slices.Clone(names)
	slices.Sort(names)
	removed, err := r.RemovedPeople(ctx)
	if err != nil {
		return len(names), nil, err
	}
	var pending []string
	for _, id := range removed {
		if _, live := slices.BinarySearch(names, PersonDEKName(id)); live {
			pending = append(pending, id)
		}
	}
	return len(names), pending, nil
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
