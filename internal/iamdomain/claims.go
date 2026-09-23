package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE DUPLICATE- AND ORPHAN-CLAIM REPORT: the two claim states the broker
// cannot refuse, found and named rather than prevented.
//
// # A duplicate is reported, never refused
//
// Uniqueness in this estate is the SUBJECT a claim arbitrates on, not an
// index: the replicated estate forbids a UNIQUE index outside a primary key,
// because a violation inside an apply transaction aborts it identically on
// every node and stalls the log — which here is every sign-in in the company.
// So ordinary traffic cannot put one address on two people (the broker refuses
// the second create at zero, and the applier takes a moved token off its old
// row), and a RESTORE or a REANCHOR can: rows copied back from an artefact do
// not pass through the broker at all.
//
// The migration ships three NON-UNIQUE partial indexes — the address blind, the
// login and the seat — for exactly this reader. It walks them, says who shares
// what, and changes nothing: which of two people keeps an address is a
// decision somebody makes, and an estate that picked for them would be a
// second, silent answer to who somebody is.
//
// # An orphan is a sequence that stopped
//
// An enrolment takes its claims FIRST, because they are what can be refused,
// and a claim's apply leaves a RESERVATION row — no kind, no stage, not
// `active`, so it can do nothing — that the content record then fills in. A
// request that stopped between the two leaves the reservation holding an
// address or a login nobody can use. That is a LEGAL NAMED STATE rather than
// corruption, and removing the reservation's id is the repair: a removal reads
// the claims inside its own snapshot and releases every one.

// OrphanGrace is how old a reservation must be before it is reported as a
// sequence that stopped rather than one still running.
//
// AN HOUR. An enrolment is a handful of appends inside ONE request, each
// bounded by the publisher's resolve budget of seconds, so a reservation still
// unfilled after an hour belongs to a request that ended long ago — and an hour
// is short enough that the address it is holding is reported the same day
// somebody tried and failed to invite that person.
const OrphanGrace = time.Hour

// DuplicateClaim is one token held by more than one person.
type DuplicateClaim struct {
	// Kind is which claim: [KindEmail], [KindLogin] or [KindSeat].
	Kind ObjectKind

	// Token is the claimed value: the login or the seat handle in the
	// clear, and for an address the keyed BLIND — the address itself is
	// sealed and this report opens nothing.
	Token string

	// People are the holders, in id order.
	People []string
}

// OrphanedClaim is a reservation an enrolment left behind.
type OrphanedClaim struct {
	// Person is the reservation's id, which is what a removal names to
	// release everything it holds.
	Person string

	// Holds names the claims it is keeping out of circulation.
	Holds []ObjectKind

	// Login and Seat are in the clear when held; an address is not.
	Login string
	Seat  string

	// Since is when the first claim reserved it.
	Since time.Time
}

// ClaimReport is one reading of both states.
type ClaimReport struct {
	Duplicates []DuplicateClaim
	Orphans    []OrphanedClaim

	// At is what this node had applied when it read them, because both
	// are findings about THIS node's rows — a node behind the log can hold
	// a duplicate a later record has already resolved.
	At statelog.Position
}

// claimColumns are the three claims and the column each lives in, in the order
// the report renders them.
var claimColumns = []struct {
	kind   ObjectKind
	column string
}{
	{KindEmail, "email_blind"},
	{KindLogin, "login"},
	{KindSeat, "seat_id"},
}

// Claims reads both states off this node's rows, in ONE snapshot.
//
// A READ, NEVER A WRITE AND NEVER A REFUSAL, which is the whole of the
// contract: nothing here can abort an apply, fail a sign-in or change a row.
func (r *Reader) Claims(ctx context.Context, now time.Time) (ClaimReport, error) {
	out := ClaimReport{At: r.committed()}
	err := r.scan(ctx, func(tx *sql.Tx) error {
		for _, claim := range claimColumns {
			dups, err := duplicatesOf(ctx, tx, claim.kind, claim.column)
			if err != nil {
				return err
			}
			out.Duplicates = append(out.Duplicates, dups...)
		}
		orphans, err := orphansBefore(ctx, tx, now.Add(-OrphanGrace))
		if err != nil {
			return err
		}
		out.Orphans = orphans
		return nil
	})
	if err != nil {
		return ClaimReport{}, err
	}
	return out, nil
}

// duplicatesOf is every token of one claim kind that more than one row holds.
//
// OVER THE PARTIAL INDEX the migration ships for it, so the grouping reads
// only rows that hold a token — an unclaimed column is the empty string on
// most rows, and an index over thousands of identical empty keys would be a
// scan wearing an index's name.
func duplicatesOf(ctx context.Context, tx *sql.Tx, kind ObjectKind,
	column string) ([]DuplicateClaim, error) {

	// THE COLUMN IS ONE OF THREE CONSTANTS above, never a caller's string,
	// which is what makes interpolating it into the statement safe.
	rows, err := tx.QueryContext(ctx, `
		SELECT `+column+` FROM iam_people
		WHERE `+column+` != ''
		GROUP BY `+column+` HAVING COUNT(*) > 1
		ORDER BY `+column)
	if err != nil {
		return nil, fmt.Errorf("iamdomain: find duplicate %s claims: %w", kind, err)
	}
	var tokens []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iamdomain: scan a duplicate %s claim: %w", kind, err)
		}
		tokens = append(tokens, token)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("iamdomain: find duplicate %s claims: %w", kind, err)
	}

	out := make([]DuplicateClaim, 0, len(tokens))
	for _, token := range tokens {
		holders, err := tx.QueryContext(ctx, `
			SELECT id FROM iam_people WHERE `+column+` = ? ORDER BY id`, token)
		if err != nil {
			return nil, fmt.Errorf("iamdomain: read who holds a duplicate %s "+
				"claim: %w", kind, err)
		}
		dup := DuplicateClaim{Kind: kind, Token: token}
		for holders.Next() {
			var id string
			if err := holders.Scan(&id); err != nil {
				_ = holders.Close()
				return nil, fmt.Errorf("iamdomain: scan a holder: %w", err)
			}
			dup.People = append(dup.People, id)
		}
		if err := errors.Join(holders.Err(), holders.Close()); err != nil {
			return nil, fmt.Errorf("iamdomain: read who holds a duplicate %s "+
				"claim: %w", kind, err)
		}
		out = append(out, dup)
	}
	return out, nil
}

// orphansBefore is every reservation created before cutoff.
//
// A RESERVATION IS A ROW NO CONTENT RECORD HAS FILLED — no kind and no stage —
// and it is read through the stage index, whose empty-stage prefix holds
// nothing else.
func orphansBefore(ctx context.Context, tx *sql.Tx, cutoff time.Time) (
	[]OrphanedClaim, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT id, login, email_blind, seat_id, created_at FROM iam_people
		WHERE stage = '' AND kind = '' AND created_at < ?
		ORDER BY id`, cutoff.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("iamdomain: find orphaned reservations: %w", err)
	}
	defer rows.Close()
	var out []OrphanedClaim
	for rows.Next() {
		var (
			o       OrphanedClaim
			blind   string
			created int64
		)
		if err := rows.Scan(&o.Person, &o.Login, &blind, &o.Seat,
			&created); err != nil {
			return nil, fmt.Errorf("iamdomain: scan a reservation: %w", err)
		}
		if blind != "" {
			o.Holds = append(o.Holds, KindEmail)
		}
		if o.Login != "" {
			o.Holds = append(o.Holds, KindLogin)
		}
		if o.Seat != "" {
			o.Holds = append(o.Holds, KindSeat)
		}
		// A RESERVATION HOLDING NOTHING is a row every claim was released
		// from — by the removal that repaired it, or a move — and there
		// is nothing left for it to keep out of circulation.
		if len(o.Holds) == 0 {
			continue
		}
		o.Since = fromMillis(created)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iamdomain: find orphaned reservations: %w", err)
	}
	return out, nil
}

// scan runs one of the DUTIES' reads: a walk over a table rather than the
// keyed lookup a request decides by.
//
// FROM THE ORDINARY POOL, deliberately not [Reader.withTx]'s: that marks the
// read as identity work and spends the one connection the store holds back so
// a sign-in can be decided during a socket storm, and a background report that
// took it would be queueing the very lookups the reserve exists to keep moving.
func (r *Reader) scan(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := r.db.Replicated().Read(ctx, fn); err != nil {
		if errors.Is(err, store.ErrNoEstate) {
			return fmt.Errorf("%w: the replicated estate is not open on this "+
				"node, so nothing can be established about any identity here",
				err)
		}
		return err
	}
	return nil
}
