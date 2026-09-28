package chart

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHICH SEALED VALUES THE ROWS STILL NAME.
//
// A value this domain seals lives in the company's secret store under a name
// derived from its object and field ([SecretName]), and a row carries the
// reference. Two readers need the other direction — from the rows to the names
// they reference:
//
//   - A NODE'S RESOLVER, which takes a snapshot of the store and must hold
//     every value its rows name the moment it builds a seat from them. A
//     value sealed by a chart write after the snapshot was taken is one no
//     apply picked up, so the node re-reads what its rows name when they move
//     (internal/engine).
//   - THE ORPHAN SWEEP, which deletes a sealed value nothing names any more —
//     a field cleared, a credential replaced by the operator's own reference,
//     an object removed. The store has no retention of its own, so without it
//     every such value outlived the company. Its absence answer has to be
//     PROVED, which is what [Reader.SealedNames] is for.

// Sealed is every name this domain derived that the seat's row references — in
// its address and anywhere in its runtime half — each once, in order.
//
// THE WHOLE HALF, every string in it, and not only the fields a walk of the
// half's type calls credentials: what resolves a reference is the node reading
// the half, and it reads a reference wherever it sits. A half that does not
// decode is an ERROR rather than no names, because the one reader that acts on
// an empty answer deletes values.
func (s Seat) Sealed() ([]string, error) { return sealedIn(s.Email, s.Runtime) }

// Sealed is [Seat.Sealed] for a unit, whose sealed values are all in its
// runtime half.
func (u Unit) Sealed() ([]string, error) { return sealedIn("", u.Runtime) }

// sealedIn is every derived name one row's address and runtime half reference.
func sealedIn(email string, runtime json.RawMessage) ([]string, error) {
	var out []string
	add := func(value string) {
		for _, name := range envref.Names(value) {
			if OwnsSecret(name) && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	add(email)
	if len(runtime) == 0 {
		return out, nil
	}
	var doc any
	if err := json.Unmarshal(runtime, &doc); err != nil {
		return nil, fmt.Errorf("chart: read the runtime half: %w", err)
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			add(x)
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(doc)
	return out, nil
}

// ErrNotCurrent reports chart rows that cannot vouch for an absence: they have
// not applied every record the log held when the question was asked, whether
// because this node is behind or because it RETAINED one it could not apply.
//
// A SENTINEL, because the question that turns on it — does any row still name
// this sealed value — is answered by a reference being MISSING, and on such a
// snapshot a missing reference is the one observation that proves nothing: the
// record that names the value may be exactly the one not applied here.
var ErrNotCurrent = errors.New("chart: this node's chart rows do not hold " +
	"every record the log held when it was asked, so a reference missing from " +
	"them proves nothing")

// SealedNames is every derived name any row of the chart references, read from
// rows proved, in the same snapshot, to hold every record the chart log held at
// end — which the caller reads BEFORE asking, so a record landing between the
// two can only make the answer "not current", never let a snapshot that missed
// it vouch for it.
//
// THE PROOF IS AGAINST WHAT THE SNAPSHOT APPLIED, never its checkpoint: the
// checkpoint moves past a record a node retains, and a census that read it
// would call a node current while that record's references were missing from
// its rows. See [statelog.Prefix.Applied].
func (r *Reader) SealedNames(ctx context.Context, end uint64) (map[string]bool, error) {
	names := map[string]bool{}
	err := r.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		prefix, err := statelog.PrefixIn(ctx, tx, Domain{})
		if err != nil {
			return fmt.Errorf("chart: read how much of the log these rows hold: %w", err)
		}
		if err = coversLog(prefix, end); err != nil {
			return err
		}
		seats, err := readSeats(ctx, tx)
		if err != nil {
			return err
		}
		for _, seat := range seats {
			var held []string
			if held, err = seat.Sealed(); err != nil {
				return fmt.Errorf("chart: seat %s: %w", seat.Handle, err)
			}
			for _, name := range held {
				names[name] = true
			}
		}
		units, err := readUnits(ctx, tx)
		if err != nil {
			return err
		}
		for _, unit := range units {
			var held []string
			if held, err = unit.Sealed(); err != nil {
				return fmt.Errorf("chart: unit %s: %w", unit.Key, err)
			}
			for _, name := range held {
				names[name] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// SealSource is the provenance a value this domain sealed carries in the secret
// store — what an operator listing their secrets reads to tell a credential
// somebody typed into a seat from one a provisioner minted, and what the orphan
// sweep asks before it deletes anything: a row the operator wrote under a
// chart-shaped name is theirs, whatever its name looks like.
const SealSource = "chart"

// SealGrace is how long a sealed value nothing names is kept before the sweep
// may delete it.
//
// A VALUE IS SEALED BEFORE ITS RECORD IS PUBLISHED — inside the decide — so for
// the seconds between the two, and across a retry the broker sent back to
// decide again, a value exists that no row names yet. An hour is two orders of
// magnitude past the longest of those (the publisher's own resolve budget is
// seconds), and it is the grace the identity estate's key duty gives a key
// nobody owns for the same reason. A re-seal re-dates the row, so a value
// being written is never an hour old.
const SealGrace = time.Hour

// OrphanedSeals is every value this domain sealed that nothing names and that
// has not been written for [SealGrace] — what the sweep may delete, each at
// the version it was listed at.
//
// PURE OVER VALUES, for the reason every judgement a duty acts on is: named is
// read by [Reader.SealedNames] from rows proved current, and held is one
// listing of the store, and a rule exercised only through both is a rule
// nobody re-reads. A row is judged only if it has this domain's SHAPE and its
// SOURCE: a name somebody set by hand is not this domain's to delete, however
// it is spelled.
func OrphanedSeals(held []secrets.Record, named map[string]bool, now time.Time) []secrets.Record {
	var out []secrets.Record
	for _, row := range held {
		if !OwnsSecret(row.Name) || row.Source != SealSource || named[row.Name] {
			continue
		}
		if now.Sub(row.UpdatedAt) < SealGrace {
			continue
		}
		out = append(out, row)
	}
	return out
}

// coversLog proves from one snapshot's prefix and the log's last sequence read
// before it that the snapshot holds every record the log held, or says —
// wrapping [ErrNotCurrent] — why it does not.
func coversLog(prefix statelog.Prefix, end uint64) error {
	if prefix.Applied().Seq >= end {
		return nil
	}
	if prefix.Retains && prefix.Retained.Position.Seq <= end {
		return fmt.Errorf("%w: it holds a record at %s it could not apply "+
			"(record version %d) — a newer build's, or one signed under a "+
			"keyring key this node does not hold", ErrNotCurrent,
			prefix.Retained.Position, prefix.Retained.Version)
	}
	return fmt.Errorf("%w: it has applied %d of the %d records the log held",
		ErrNotCurrent, prefix.Applied().Seq, end)
}
