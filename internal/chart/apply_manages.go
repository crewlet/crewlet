package chart

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// WHAT A RENAME DOES TO THE `manages:` ENTRIES NAMING ITS OBJECT: it moves each
// one onto the new address, as the rest of its cascade moves every reference
// by key.
//
// # Why the entries move, and did not
//
// An entry is what somebody typed, and it used to be left as typed, on two
// grounds: editing it changed a document nobody edited, and the next config
// apply would write the old spelling straight back. The second stopped being
// true when the chart left the company document — `/config` refuses a chart,
// and the seed writes one only onto an empty chart — and the first was never a
// reason to leave a reference pointing at an address the object no longer
// holds. What kept such an entry working was the retired alias, which is
// CAPPED ([MaxFormerKeys]): sixteen renames later the entry named nobody. And a
// creation MAY TAKE a retired alias, so before then a `create_seat` on the old
// handle silently made the manager manage the newcomer instead. A rename's
// cascade already moves a unit's children, its members and the units a seat
// leads, each by the key the rename retires; an authored entry is one more
// reference by key, and it moves for the same reason.
//
// # Which entries
//
// Every entry that names the object BEFORE the rename — by the address it is
// leaving, by a retired one or by the one it was created under — and that the
// organisation resolves to THIS object: a unit and a seat may share a spelling
// and a seat reading wins (the manages index in internal/org), so an entry
// naming a unit's key that some seat also answers to names the seat and is not
// the unit's to move. The resolution is the chart's own ([txBook.holder]),
// asked of the rows before the rename, which is the order the organisation
// reads them in.
//
// And ONLY WHERE THE NEW ADDRESS NAMES THE SAME OBJECT: a seat's new handle
// always does, since a live handle wins every reading, but a unit renamed onto
// a key some seat answers to would hand every entry to that seat. There the
// entries are left, and they go on reaching the unit through the address it
// retired, exactly as they did before this rule.
//
// FROM VERSION 2. A version-1 rekey left the entries as authored, which is
// what it meant, and it is read for ever as that ([Applier.applyRekey]).
//
// # And the batch's replay moves them the same way
//
// From version 3 a seat's `manages:` list is structure, and every seat edge
// states the whole list ([Edge.Manages]). So a batch that renames an object
// and also names a seat whose list reaches it — a set_manages before the
// rename, a move of the manager after it — has to publish the list the rename
// leaves, or the edge, applied after the cascade, would write the old address
// back. The replay asks the same two questions of its own state
// ([working.moveManages]) — which entries reach the object, and whether its
// new address still names it — through the one reading both answer by
// ([managesResolves]).

// managesNaming is every address an authored `manages:` entry names the object
// at `from` by — read before its rename moves anything.
func managesNaming(ctx context.Context, tx *sql.Tx, kind ObjectKind, from string) (
	[]string, error) {

	var addresses []string
	switch kind {
	case KindUnit:
		unit, found, err := readUnit(ctx, tx, from)
		if err != nil || !found {
			return nil, err
		}
		addresses = append([]string{from, unit.Origin()}, unit.FormerKeys...)
	case KindSeat:
		seat, found, err := readSeat(ctx, tx, from)
		if err != nil || !found {
			return nil, err
		}
		addresses = append([]string{from, seat.Origin()}, seat.FormerHandles...)
	default:
		return nil, nil
	}
	slices.Sort(addresses)
	addresses = slices.Compact(addresses)

	targets, err := manageTargets(ctx, tx, addresses)
	if err != nil {
		return nil, err
	}
	book := txBook{tx: tx}
	var named []string
	for _, target := range targets {
		resolves, err := managesResolves(ctx, book, kind, target, from)
		if err != nil {
			return nil, err
		}
		if resolves {
			named = append(named, target)
		}
	}
	return named, nil
}

// manageTargets is which of addresses some authored entry names, in order.
func manageTargets(ctx context.Context, tx *sql.Tx, addresses []string) ([]string, error) {
	if len(addresses) == 0 {
		return nil, nil
	}
	args := make([]any, len(addresses))
	for i, address := range addresses {
		args[i] = address
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT target FROM chart_manages
		WHERE target IN (`+strings.TrimSuffix(strings.Repeat("?,", len(addresses)), ",")+`)
		ORDER BY target`, args...)
	if err != nil {
		return nil, fmt.Errorf("chart: read the manages entries naming %v: %w",
			addresses, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, fmt.Errorf("chart: read a manages entry: %w", err)
		}
		out = append(out, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chart: read the manages entries naming %v: %w",
			addresses, err)
	}
	return out, nil
}

// managesResolves reports whether an entry naming address reaches the object
// of kind answering to key — a SEAT reading first, as the organisation's own
// manages index reads an entry, and the object's own kind after it.
//
// ASKED OF AN [addressBook], so the apply's rows and the batch's replay answer
// it by one reading: the two disagreeing would be a list the edge states and a
// cascade that moved it somewhere else.
func managesResolves(ctx context.Context, book addressBook, kind ObjectKind,
	address, key string) (bool, error) {

	seat, how, err := book.holder(ctx, KindSeat, address)
	if err != nil {
		return false, err
	}
	if kind == KindSeat {
		return how != heldByNothing && seat == key, nil
	}
	if how != heldByNothing {
		// A SEAT ANSWERS TO IT, so the entry names that seat.
		return false, nil
	}
	unit, how, err := book.holder(ctx, KindUnit, address)
	if err != nil {
		return false, err
	}
	return how != heldByNothing && unit == key, nil
}

// moveManages moves every entry named by [managesNaming] onto key, the address
// the object's rename landed on, and reports how many rows it wrote.
//
// AN ENTRY A MANAGER ALREADY HOLDS UNDER THE NEW ADDRESS is dropped rather than
// moved onto it: the pair is the table's primary key, and an UPDATE that met it
// would raise inside the apply on every node alike.
func (a *Applier) moveManages(ctx context.Context, tx *sql.Tx, at applyContext,
	kind ObjectKind, key string, named []string) (int, error) {

	if len(named) == 0 {
		return 0, nil
	}
	if moves, err := managesFollow(ctx, txBook{tx: tx}, kind, key); err != nil || !moves {
		return 0, err
	}
	rows := 0
	for _, target := range named {
		if target == key {
			// A RENAME BACK onto an address the entry already names.
			continue
		}
		managers, err := managersOf(ctx, tx, target)
		if err != nil {
			return 0, err
		}
		held, err := tx.ExecContext(ctx, `
			DELETE FROM chart_manages
			WHERE target = ? AND manager IN (
				SELECT manager FROM chart_manages WHERE target = ?)`,
			target, key)
		if err != nil {
			return 0, fmt.Errorf("chart: drop the manages entries naming %s "+
				"twice at %s: %w", key, at.position, err)
		}
		moved, err := tx.ExecContext(ctx,
			`UPDATE chart_manages SET target = ? WHERE target = ?`, key, target)
		if err != nil {
			return 0, fmt.Errorf("chart: move the manages entries naming %s to "+
				"%s at %s: %w", target, key, at.position, err)
		}
		h, _ := held.RowsAffected()
		m, _ := moved.RowsAffected()
		rows += int(h + m)
		for _, manager := range managers {
			a.note(ObjectRef{Kind: KindSeat, ID: manager})
		}
	}
	return rows, nil
}

// managesFollow reports whether the entries naming an object of kind may move
// onto key, the address its rename lands on: always for a seat, whose live
// handle wins every reading, and for a unit only where no seat answers to key —
// a seat reading wins there, so the entries would name the seat instead. See
// this file's header.
func managesFollow(ctx context.Context, book addressBook, kind ObjectKind,
	key string) (bool, error) {

	if kind != KindUnit {
		return true, nil
	}
	_, how, err := book.holder(ctx, KindSeat, key)
	if err != nil {
		return false, err
	}
	return how == heldByNothing, nil
}

// managesRefusal is why a list may not be a seat's `manages:`, in a batch
// rule's vocabulary.
type managesRefusal struct {
	rule   string
	detail string
}

// manageList is entries as a seat's `manages:` list is held — folded,
// de-duplicated and sorted, exactly as the apply stores them ([sortedKeys]) —
// or why it may not be one.
//
// CHECKED WHERE A RECORD IS WRITTEN — a set_manages at its batch's replay, an
// import's edge at its writer — and never where one is applied, for the
// asymmetry every value rule here follows ([Unit.Validate]).
//
// TWO RULES. Every entry is an ADDRESS — a seat's handle or a unit's key — so
// one no address could take ([checkKey]) names nothing any chart can hold, and
// the company file refuses the same entry for the same reason. And the list is
// bounded at [MaxManages], counted as it lands: two spellings of one address
// are one row. An empty entry is dropped rather than refused, as the apply
// always dropped it, because an edge to nothing is not an entry.
//
// AN ENTRY THAT RESOLVES TO NOTHING IS KEPT: a chart is assembled in pieces,
// and a seat managing somebody not hired yet is what the organisation's own
// dangling-reference report is for.
func manageList(entries []string) ([]string, *managesRefusal) {
	for _, entry := range entries {
		folded := NormalizeKey(entry)
		if folded == "" {
			continue
		}
		if err := checkKey("manages", folded); err != nil {
			return nil, &managesRefusal{rule: RuleBadKey,
				detail: fmt.Sprintf("the `manages:` entry %q names no seat handle "+
					"and no unit key: %v", entry, err)}
		}
	}
	list := sortedKeys(entries)
	if len(list) > MaxManages {
		return nil, &managesRefusal{rule: RuleTooManyManaged,
			detail: fmt.Sprintf("the list holds %d entries and a seat manages at "+
				"most %d directly — a unit key reaches a whole team in one entry, "+
				"which is what a list this long wants", len(list), MaxManages)}
	}
	return list, nil
}

// managersOf is every seat with an authored entry naming target, in order.
func managersOf(ctx context.Context, tx *sql.Tx, target string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT manager FROM chart_manages WHERE target = ? ORDER BY manager`, target)
	if err != nil {
		return nil, fmt.Errorf("chart: read who manages %s: %w", target, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var manager string
		if err := rows.Scan(&manager); err != nil {
			return nil, fmt.Errorf("chart: read a manager of %s: %w", target, err)
		}
		out = append(out, manager)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chart: read who manages %s: %w", target, err)
	}
	return out, nil
}
