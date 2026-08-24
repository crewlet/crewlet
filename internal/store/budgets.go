package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The shared token counter.
//
// USAGE IS SHARED, CAPS ARE NOT. A cap belongs to a config epoch — a revision
// that raises a ceiling takes effect on the next turn — while the counter has
// to be one number across the fleet, because per-process counters mean N nodes
// each spend the whole allowance. So the limit travels IN on every call and
// the row holds only what has been spent.

// OrgScope is the company-wide counter's key.
const OrgScope = "org"

// AgentScope is one seat's counter key.
//
// Keyed on the DERIVED agent id rather than the handle, matching the diary and
// the episodes: renaming a handle then starts a fresh budget rather than
// inheriting the spend of whoever held the name before.
func AgentScope(agentID string) string { return "agent:" + agentID }

// Spend is what one charge did.
type Spend struct {
	// OK is false when a scope refused. RefusedScope, RefusedUsed and
	// RefusedLimit then say WHICH and by how much — "the company is out"
	// and "this seat is out" send an operator to different places, and a
	// bare refusal sends them to neither.
	OK           bool
	RefusedScope string
	RefusedUsed  int
	RefusedLimit int

	// OrgUsed and AgentUsed are the counters after a successful charge.
	OrgUsed   int
	AgentUsed int
}

// Budgets is the shared token counter.
type Budgets struct{ db *DB }

// Budgets returns the counter backed by this database.
func (d *DB) Budgets() *Budgets { return &Budgets{db: d} }

// bumpSQL is the atomic check-and-increment.
//
// The WHERE on DO UPDATE is what makes it atomic: a peer racing the last of a
// cap either lands inside it or updates nothing. Note it applies ONLY to the
// update branch — a scope with no row yet takes the INSERT, which has no
// existing value to test — which is why [Budgets.Spend] screens an
// over-cap charge before it gets here.
//
// No RETURNING: it is outside the intersection of the two certified drivers
// (rewrite/decisions/002), so the outcome is read from RowsAffected and the
// value from a following SELECT inside the same transaction.
const bumpSQL = `
INSERT INTO token_budget_usage (scope, used_tokens, updated_at)
VALUES (?, ?, ?)
ON CONFLICT (scope) DO UPDATE
SET used_tokens = token_budget_usage.used_tokens + excluded.used_tokens,
    updated_at  = excluded.updated_at
WHERE ? = 0 OR token_budget_usage.used_tokens + excluded.used_tokens <= ?`

// Charge checks and increments both scopes, atomically.
//
// ORG FIRST, THEN THE SEAT, in one transaction — so a seat that is refused
// unwinds the org bump it already made. Charging the org for a turn that never
// ran would let a company exhaust its budget on work it did not do.
//
// A limit of 0 is UNLIMITED, matching the config: `token_budget: 0` is how an
// operator says "no ceiling", and reading it as "no allowance" would stop every
// company that never set one.
//
// An error means the counter could not be reached, which is NOT a refusal and
// must not be reported as one: the caller fails the turn rather than telling an
// agent it is out of budget.
func (b *Budgets) Charge(ctx context.Context, agentID string, tokens, orgLimit, agentLimit int) (Spend, error) {
	if tokens <= 0 {
		// Not an error and not a charge. A phase whose provider reported
		// nothing still ran, and refusing it would stop a company over a
		// backend that omits usage.
		return Spend{OK: true}, nil
	}
	seat := AgentScope(agentID)

	// A charge larger than a whole cap can never fit, and the INSERT branch
	// of the upsert has no existing value to test against — so it is
	// screened here, before anything is written. Without this a first-ever
	// charge of a million tokens against a cap of ten would be accepted.
	for _, s := range []struct {
		name, scope string
		limit       int
	}{{"org", OrgScope, orgLimit}, {"agent", seat, agentLimit}} {
		if s.limit > 0 && tokens > s.limit {
			used, err := b.Used(ctx, s.scope)
			if err != nil {
				return Spend{}, err
			}
			return Spend{RefusedScope: s.name, RefusedUsed: used, RefusedLimit: s.limit}, nil
		}
	}

	var out Spend
	err := b.db.Tx(ctx, func(tx *sql.Tx) error {
		orgUsed, ok, err := bump(ctx, tx, OrgScope, tokens, orgLimit)
		if err != nil {
			return err
		}
		if !ok {
			out = Spend{RefusedScope: "org", RefusedUsed: orgUsed, RefusedLimit: orgLimit}
			return errRefused
		}
		agentUsed, ok, err := bump(ctx, tx, seat, tokens, agentLimit)
		if err != nil {
			return err
		}
		if !ok {
			out = Spend{RefusedScope: "agent", RefusedUsed: agentUsed, RefusedLimit: agentLimit}
			// Rolls the transaction back, which unwinds the org bump above.
			return errRefused
		}
		out = Spend{OK: true, OrgUsed: orgUsed, AgentUsed: agentUsed}
		return nil
	})
	if errors.Is(err, errRefused) {
		return out, nil
	}
	if err != nil {
		return Spend{}, fmt.Errorf("store: charge budget: %w", err)
	}
	return out, nil
}

// errRefused rolls a charge back without being an error to the caller. A
// sentinel rather than a bool out-parameter, because Tx's contract is that a
// non-nil error rolls back — which is exactly what a refusal needs.
var errRefused = errors.New("store: budget refused")

// bump applies one scope's charge, reporting the resulting usage and whether
// it fit.
func bump(ctx context.Context, tx *sql.Tx, scope string, tokens, limit int) (int, bool, error) {
	res, err := tx.ExecContext(ctx, bumpSQL, scope, tokens, EncodeTime(now()), limit, limit)
	if err != nil {
		return 0, false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	used, err := usedIn(ctx, tx, scope)
	if err != nil {
		return 0, false, err
	}
	return used, rows > 0, nil
}

func usedIn(ctx context.Context, tx *sql.Tx, scope string) (int, error) {
	var used int
	err := tx.QueryRowContext(ctx,
		`SELECT used_tokens FROM token_budget_usage WHERE scope = ?`, scope).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return used, err
}

// Used reports one scope's spend. A scope with no row has spent nothing.
func (b *Budgets) Used(ctx context.Context, scope string) (int, error) {
	var used int
	err := b.db.sql.QueryRowContext(ctx,
		`SELECT used_tokens FROM token_budget_usage WHERE scope = ?`, scope).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: budget usage %q: %w", scope, err)
	}
	return used, nil
}

// Usage is one scope's counter, for the operator surface.
type Usage struct {
	Scope     string
	Used      int
	UpdatedAt time.Time
}

// List returns every counter, org first then seats by scope.
//
// Ordered so the operator surface does not have to sort, and so two reads of
// an unchanged table are byte-identical — a listing that reshuffled would make
// a diff of two captures unreadable.
func (b *Budgets) List(ctx context.Context) ([]Usage, error) {
	rows, err := b.db.sql.QueryContext(ctx,
		`SELECT scope, used_tokens, updated_at FROM token_budget_usage ORDER BY scope`)
	if err != nil {
		return nil, fmt.Errorf("store: list budgets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Usage
	for rows.Next() {
		var (
			u  Usage
			at int64
		)
		if err := rows.Scan(&u.Scope, &u.Used, &at); err != nil {
			return nil, fmt.Errorf("store: list budgets: scan: %w", err)
		}
		u.UpdatedAt = DecodeTime(at)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list budgets: %w", err)
	}
	// "org" sorts before "agent:…" alphabetically? It does not — so the
	// order is fixed here rather than left to the collation, which differs
	// between the two drivers for exactly this kind of key.
	for i, u := range out {
		if u.Scope == OrgScope && i != 0 {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	return out, nil
}

// Reset zeroes one scope, or every scope when given "".
//
// An operator action, never a schedule. A budget is a ceiling for the life of
// a deployment; a table that rolled itself over would silently re-arm a
// company somebody had stopped on purpose.
func (b *Budgets) Reset(ctx context.Context, scope string) (int64, error) {
	query := `DELETE FROM token_budget_usage`
	var args []any
	if strings.TrimSpace(scope) != "" {
		query += ` WHERE scope = ?`
		args = append(args, scope)
	}
	res, err := b.db.sql.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store: reset budget: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: reset budget: %w", err)
	}
	return n, nil
}
