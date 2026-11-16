package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// RefOrder is the order a refs listing walks. A closed set ([RefOrders]).
type RefOrder string

const (
	// RefOrderName is by name, today's order; the zero order reads as it.
	RefOrderName RefOrder = "name"
	// RefOrderMoved is the branches by when each last moved, newest first, two
	// moved by one record by name, highest first (code_refs_moved_idx).
	RefOrderMoved RefOrder = "moved"
)

// RefOrders is the closed set.
func RefOrders() []RefOrder { return []RefOrder{RefOrderName, RefOrderMoved} }

// Valid reports an order this build knows.
func (o RefOrder) Valid() bool { return slices.Contains(RefOrders(), o) }

// THE LISTING'S BOUNDS.
const (
	// MaxRefsScanned is the most refs a page narrowed by Contains reads: no
	// index answers a substring, so a page reads a tenth of MaxRefsPerRepo at
	// most and answers what it found with a Next.
	MaxRefsScanned = 10_000
	// MaxActiveBranchesCounted is the most branches an active count counts
	// before it answers More: a figure a person reads at a glance, and each
	// counted branch is one index entry stepped on the first page.
	MaxActiveBranchesCounted = 1_000
)

// RefListingQuery is how a refs listing is ordered, narrowed and detailed.
type RefListingQuery struct {
	// Order is RefOrderName (or zero), or RefOrderMoved, which lists
	// refs/heads/ alone.
	Order RefOrder `json:"order,omitempty"`
	// MovedSince and MovedBefore keep the branches whose last move is at or
	// after MovedSince and before MovedBefore: the Active and Stale tabs. Only
	// in moved order; either may be zero.
	MovedSince  time.Time `json:"moved_since,omitzero"`
	MovedBefore time.Time `json:"moved_before,omitzero"`
	// Contains keeps the refs whose name below the listing's prefix holds it
	// in any case (unicode.SimpleFold's orbits, as strings.EqualFold): at most
	// gitref.MaxBranchBytes of UTF-8. A page reads at most MaxRefsScanned refs.
	Contains string `json:"contains,omitempty"`
	// Detail answers each ref as a RefListing (ReadAnswer.RefsDetail) in place
	// of a RefRow: who moved it and when, its protection, its change and
	// whether the asker may delete it.
	Detail bool `json:"detail,omitempty"`
	// ActiveSince asks a first page of branches how many other than the
	// default branch moved at or after it (ReadAnswer.ActiveBranches).
	ActiveSince time.Time `json:"active_since,omitzero"`
}

// RefListing is one ref of a listing read in detail, kept apart from RefRow,
// which a snapshot's delta ships, so the cache's pages do not grow.
type RefListing struct {
	Ref      string            `json:"ref"`
	OID      string            `json:"oid"`
	Position statelog.Position `json:"position"`
	// MovedAt is when the record that last moved it applied (code_refs.at).
	MovedAt time.Time `json:"moved_at"`
	// MovedBy is the push that moved it there; absent where the push history
	// no longer holds it (PushHistory) or one record moved it there twice.
	MovedBy *RefMover `json:"moved_by,omitempty"`
	// Protection is the rules whose pattern matches it; absent where none does.
	Protection *RefProtection `json:"protection,omitempty"`
	// Change is a branch's live change, else the newest change from it;
	// absent for a branch no change came from and for every other ref.
	Change *RefChange `json:"change,omitempty"`
	// MayDelete says the asker may delete it now; where it may not,
	// DeleteRefusal and DeleteDetail are the refusal and the sentence a
	// deletion would be answered with. Exactly one side is set.
	MayDelete     bool    `json:"may_delete,omitempty"`
	DeleteRefusal Refusal `json:"delete_refusal,omitempty"`
	DeleteDetail  string  `json:"delete_detail,omitempty"`
}

// RefMover is the push that last moved a ref, as its history row names it
// ([PushRow]'s fields, so a mover is drawn as a pusher is).
type RefMover struct {
	PushID    string  `json:"push_id"`
	Actor     string  `json:"actor"`
	ActorKind string  `json:"actor_kind"`
	ActorSeat string  `json:"actor_seat,omitempty"`
	Via       PushVia `json:"via"`
}

// RefProtection is the rules that match a ref, in the rules' order, and what
// they allow together: a force-push or a deletion only where every one allows
// it, a direct push by persons unless any says none ([ProtectionRefusal]).
type RefProtection struct {
	Patterns    []string   `json:"patterns"`
	AllowForce  bool       `json:"allow_force,omitempty"`
	AllowDelete bool       `json:"allow_delete,omitempty"`
	DirectPush  DirectPush `json:"direct_push"`
}

// RefChange is the change a branch heads, or last headed.
type RefChange struct {
	Number int64       `json:"number"`
	Title  string      `json:"title" untrusted:"name"`
	State  ChangeState `json:"state"`
}

// BranchCount is a bounded count: Count up to MaxActiveBranchesCounted, More
// where there were more.
type BranchCount struct {
	Count int  `json:"count"`
	More  bool `json:"more,omitempty"`
}

// validateRefListing is [ReadQuery.validate] for a refs listing's
// RefListing, each refusal naming its field.
func (q ReadQuery) validateRefListing() error {
	lq := q.RefListing
	refused := func(field, why string) error {
		return fmt.Errorf("%w: ref_listing.%s: %s", ErrInvalid, field, why)
	}
	moved := lq.Order == RefOrderMoved
	branches := cmp.Or(q.Prefix, branchPrefix) == branchPrefix
	switch {
	case lq.Order != "" && !lq.Order.Valid():
		return refused("order", fmt.Sprintf("%q is not one of %s", lq.Order, words.Or(RefOrders())))
	case moved && !branches:
		return refused("order", "only the branches ("+branchPrefix+") are listed by when they moved; "+
			"narrow them with contains")
	case !lq.MovedSince.IsZero() && !moved:
		return refused("moved_since", "only a listing in the order branches moved is narrowed by when they moved")
	case !lq.MovedBefore.IsZero() && !moved:
		return refused("moved_before", "only a listing in the order branches moved is narrowed by when they moved")
	}
	for _, instant := range []struct {
		field string
		at    time.Time
	}{{"moved_since", lq.MovedSince}, {"moved_before", lq.MovedBefore}, {"active_since", lq.ActiveSince}} {
		if !instant.at.IsZero() && !instant.at.After(time.Unix(0, 0)) {
			return refused(instant.field, instant.at.UTC().Format(time.RFC3339Nano)+" is not after 1970")
		}
	}
	switch {
	case !lq.MovedSince.IsZero() && !lq.MovedBefore.IsZero() && !lq.MovedBefore.After(lq.MovedSince):
		return refused("moved_before", fmt.Sprintf("%s is not after moved_since %s: the window holds no instant",
			lq.MovedBefore.UTC().Format(time.RFC3339Nano), lq.MovedSince.UTC().Format(time.RFC3339Nano)))
	case len(lq.Contains) > gitref.MaxBranchBytes:
		return refused("contains", fmt.Sprintf("%d bytes is past the %d a branch's name holds", len(lq.Contains),
			gitref.MaxBranchBytes))
	case !utf8.ValidString(lq.Contains):
		return refused("contains", "is not UTF-8, which every ref's name is")
	case !lq.ActiveSince.IsZero() && q.Cursor != "":
		return refused("active_since", "only a first page counts the active branches")
	case !lq.ActiveSince.IsZero() && !branches:
		return refused("active_since", "only the branches ("+branchPrefix+") are counted")
	}
	return nil
}

// refsCursor is where a refs page resumes: past ref in name order, past
// (at, ref) in moved order.
type refsCursor struct {
	at  int64
	ref string
}

// refsAfter is where q's page resumes. A name-order cursor is the last ref
// served, under the prefix; a moved one is `<at> <ref>`: the instant in
// microseconds, one space, and a branch (a ref holds no space).
func (q ReadQuery) refsAfter(prefix string) (refsCursor, error) {
	switch {
	case q.Cursor == "":
		return refsCursor{ref: prefix}, nil
	case q.RefListing == nil || q.RefListing.Order != RefOrderMoved:
		if !strings.HasPrefix(q.Cursor, prefix) {
			return refsCursor{}, fmt.Errorf("%w: %q is not a ref under %s", ErrBadCursor, q.Cursor, prefix)
		}
		return refsCursor{ref: q.Cursor}, nil
	}
	at, ref, ok := strings.Cut(q.Cursor, " ")
	micros, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || !strings.HasPrefix(ref, branchPrefix) {
		return refsCursor{}, fmt.Errorf("%w: %q is not a moved listing's place, <microseconds> <branch>",
			ErrBadCursor, q.Cursor)
	}
	return refsCursor{at: micros, ref: ref}, nil
}

// in is the cursor as a page of the given order answers it.
func (c refsCursor) in(order RefOrder) string {
	if order == RefOrderMoved {
		return strconv.FormatInt(c.at, 10) + " " + c.ref
	}
	return c.ref
}

// scannedRef is one row a listing's scan read.
type scannedRef struct {
	ref, target  string
	position, at int64
}

// refsListed serves a refs page of repository id past after as q.RefListing
// asks, in two passes so no read runs while the scan's rows are open; a scan
// narrowed by Contains answers a Next past the last row it read.
func (r *Reader) refsListed(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position,
	id string, after refsCursor, out *ReadAnswer) error {

	lq := *q.RefListing
	prefix := cmp.Or(q.Prefix, branchPrefix)
	limit := min(limitOr(q.Limit, defaultRefLimit), r.pageRows)
	if !lq.ActiveSince.IsZero() {
		// A FIRST PAGE ([ReadQuery.validateRefListing]), so out.Repo is the
		// repository's detail.
		var n int
		if err := tx.QueryRowContext(ctx, activeBranchesSQL, id, microsFrom(lq.ActiveSince),
			branchPrefix+out.Repo.DefaultBranch, r.activeCounted+1).Scan(&n); err != nil {
			return fmt.Errorf("gitlet: count %s's active branches: %w", q.Repo, err)
		}
		out.ActiveBranches = &BranchCount{Count: min(n, r.activeCounted), More: n > r.activeCounted}
	}
	var detail *refDetailer
	if lq.Detail {
		var err error
		if detail, err = detailerOf(ctx, tx, q, chart, cp, id, out); err != nil {
			return err
		}
	}

	scanLimit := limit
	if lq.Contains != "" {
		scanLimit = r.refsScanned
	}
	scanned, err := scanRefs(ctx, tx, q, id, prefix, after, scanLimit, limit)
	if err != nil {
		return fmt.Errorf("gitlet: read %s's refs under %s: %w", q.Repo, prefix, err)
	}
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for _, row := range scanned.kept {
		var bytes int
		if detail == nil {
			ref := RefRow{Ref: row.ref, OID: row.target, Position: unpacked(row.position)}
			out.Refs = append(out.Refs, ref)
			bytes = ref.encodedBytes()
		} else {
			listing, err := detail.of(ctx, tx, &pg, row)
			if err != nil {
				return err
			}
			out.RefsDetail = append(out.RefsDetail, listing)
			bytes = listing.encodedBytes()
		}
		if pg.add(bytes) {
			out.Next = refsCursor{at: row.at, ref: row.ref}.in(lq.Order)
			return nil
		}
	}
	if lq.Contains != "" && scanned.read == scanLimit {
		out.Next = scanned.last.in(lq.Order)
	}
	return nil
}

// refScan is what a listing's scan read: the rows it kept, how many it read,
// and the place of the last.
type refScan struct {
	kept []scannedRef
	read int
	last refsCursor
}

// scanRefs reads up to scanLimit refs of the listing past after, in its
// order, and keeps the ones Contains matches, at most keep of them.
func scanRefs(ctx context.Context, tx *sql.Tx, q ReadQuery, id, prefix string, after refsCursor,
	scanLimit, keep int) (refScan, error) {

	lq := q.RefListing
	var rows *sql.Rows
	var err error
	switch {
	case lq.Order != RefOrderMoved:
		rows, err = tx.QueryContext(ctx, refsUnderSQL, id, after.ref, prefixEnd(prefix), scanLimit)
	default:
		since, upper := int64(0), int64(math.MaxInt64)
		if !lq.MovedSince.IsZero() {
			since = microsFrom(lq.MovedSince)
		}
		if !lq.MovedBefore.IsZero() {
			upper = microsFrom(lq.MovedBefore) - 1
		}
		if q.Cursor == "" {
			rows, err = tx.QueryContext(ctx, refsMovedSQL, id, since, upper, scanLimit)
		} else {
			rows, err = tx.QueryContext(ctx, refsMovedAfterSQL, id, since, min(upper, after.at), after.at,
				after.ref, scanLimit)
		}
	}
	if err != nil {
		return refScan{}, err
	}
	defer func() { _ = rows.Close() }()
	want := foldCase(lq.Contains)
	var out refScan
	for rows.Next() {
		var row scannedRef
		if err := rows.Scan(&row.ref, &row.target, &row.position, &row.at); err != nil {
			return refScan{}, err
		}
		out.read++
		out.last = refsCursor{at: row.at, ref: row.ref}
		if want != "" && !strings.Contains(foldCase(strings.TrimPrefix(row.ref, prefix)), want) {
			continue
		}
		if out.kept = append(out.kept, row); len(out.kept) == keep {
			break
		}
	}
	return out, rows.Err()
}

// refDetailer details a page's refs for one asker: the repository and its
// rules as a push is judged by them, the asker's grant, and each position's
// movers once read.
type refDetailer struct {
	repo   RepoRef
	rules  Protections
	grant  Grant
	by     PrincipalKind
	movers map[int64]map[refTarget]*RefMover
}

// refTarget is a ref and the target a command set it to.
type refTarget struct{ ref, target string }

// detailerOf is the detailer of q's page: on a first page from the detail it
// already read, on a later one from the repository's row at cp.
func detailerOf(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position, id string,
	out *ReadAnswer) (*refDetailer, error) {

	var repo RepoRef
	if out.Repo != nil {
		repo = out.Repo.RepoRef
	} else {
		v, found, err := repoViewIn(ctx, tx, id, cp)
		switch {
		case err != nil:
			return nil, err
		case !found:
			return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
		}
		repo = v.ref
	}
	// A RULE THIS BUILD CANNOT READ fails the read: read as absent, its
	// branch would be listed unprotected and deletable.
	rules, err := ParseProtections(repo.Protections)
	if err != nil {
		return nil, err
	}
	grant := GrantOf(q.Asker, repo, chart)
	if q.CeilingSeat != "" {
		grant = grant.Under(GrantOf(Principal{Kind: PrincipalAgent, Seat: q.CeilingSeat}, repo, chart))
	}
	return &refDetailer{repo: repo, rules: rules, grant: grant, by: q.Asker.Kind,
		movers: map[int64]map[refTarget]*RefMover{}}, nil
}

// of is one scanned ref in detail, the mover rows it read charged to pg.
func (d *refDetailer) of(ctx context.Context, tx *sql.Tx, pg *page, row scannedRef) (RefListing, error) {
	out := RefListing{Ref: row.ref, OID: row.target, Position: unpacked(row.position),
		MovedAt: store.DecodeTime(row.at)}
	setters, err := d.moversAt(ctx, tx, pg, row.position)
	if err != nil {
		return RefListing{}, err
	}
	if m := setters[refTarget{row.ref, row.target}]; m != nil {
		mover := *m
		out.MovedBy = &mover
	}
	out.Protection = d.protection(row.ref)
	if strings.HasPrefix(row.ref, branchPrefix) {
		if out.Change, err = changeOfSource(ctx, tx, d.repo.ID, row.ref); err != nil {
			return RefListing{}, err
		}
	}
	if out.DeleteRefusal, out.DeleteDetail = d.deletion(row.ref, row.target); out.DeleteRefusal == "" {
		out.MayDelete = true
	}
	return out, nil
}

// moversAt is, for the record at position, the push whose ok command set each
// (ref, target), nil where several did (a record moved the ref away and back):
// rows are stored by push id, not in record order. Read once, charged to pg.
func (d *refDetailer) moversAt(ctx context.Context, tx *sql.Tx, pg *page,
	position int64) (map[refTarget]*RefMover, error) {

	if setters, read := d.movers[position]; read {
		return setters, nil
	}
	rows, err := tx.QueryContext(ctx, pushesAtSQL, d.repo.ID, position)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the pushes at %d of %s: %w", position, d.repo.Name, err)
	}
	defer func() { _ = rows.Close() }()
	setters := map[refTarget]*RefMover{}
	for rows.Next() {
		var m RefMover
		var via, commands string
		var bytes int
		if err := rows.Scan(&m.PushID, &m.Actor, &m.ActorKind, &m.ActorSeat, &via, &commands,
			&bytes); err != nil {
			return nil, fmt.Errorf("gitlet: read the pushes at %d of %s: %w", position, d.repo.Name, err)
		}
		pg.charge(bytes)
		m.Via = PushVia(via)
		var cmds []Command
		if err := json.Unmarshal([]byte(commands), &cmds); err != nil {
			return nil, fmt.Errorf("gitlet: push %s's commands: %w", m.PushID, err)
		}
		for _, c := range cmds {
			if c.Result != ResultOK {
				continue
			}
			key := refTarget{c.Ref, c.New}
			if _, set := setters[key]; set {
				setters[key] = nil
				continue
			}
			setters[key] = &m
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read the pushes at %d of %s: %w", position, d.repo.Name, err)
	}
	d.movers[position] = setters
	return setters, nil
}

// protection is the rules matching ref and what they allow together, nil
// where none does.
func (d *refDetailer) protection(ref string) *RefProtection {
	matching := d.rules.matching(ref)
	if len(matching) == 0 {
		return nil
	}
	p := &RefProtection{AllowForce: true, AllowDelete: true, DirectPush: DirectPersons}
	for _, rule := range matching {
		p.Patterns = append(p.Patterns, rule.Pattern)
		p.AllowForce = p.AllowForce && rule.AllowForce
		p.AllowDelete = p.AllowDelete && rule.AllowDelete
		if rule.DirectPush == DirectNone {
			p.DirectPush = DirectNone
		}
	}
	return p
}

// deletion is how a deletion of ref at target by the asker would be answered:
// "" where it would land, else the refusal and its sentence, judged by an
// engine write's rules in its order (host's writing.rules).
func (d *refDetailer) deletion(ref, target string) (Refusal, string) {
	repo := d.repo
	if repo.Status == RepoStatusDeleted || repo.Status == RepoStatusPurging {
		return RefusalRepoDeleted, RepoGone{ID: repo.ID, Name: repo.Name, Status: repo.Status,
			DeletedAt: repo.DeletedAt, ManagedBy: repo.ManagedBy}.Detail()
	}
	if refusal, why := RepoStateRefusal(repo); refusal != "" {
		return refusal, why
	}
	c := Command{Ref: ref, Old: target, New: ZeroOID}
	if refusal := StatelessRefusal(c); refusal != "" {
		return refusal, refusal.Sentence()
	}
	if refusal, why := d.grant.RefRule(c); refusal != "" {
		return refusal, why
	}
	if refusal, rule := ProtectionRefusal(c, d.rules, d.by); refusal != "" {
		return refusal, ProtectionDetail(rule, refusal)
	}
	return DefaultBranchRefusal(c, repo.Name, repo.DefaultBranch)
}

// changeOfSource is the live change branch heads, else the newest change
// from it, nil where none came from it.
func changeOfSource(ctx context.Context, tx *sql.Tx, repoID, branch string) (*RefChange, error) {
	for _, query := range []string{liveChangeOfSourceSQL, newestChangeOfSourceSQL} {
		var c RefChange
		var state string
		err := tx.QueryRowContext(ctx, query, repoID, branch).Scan(&c.Number, &c.Title, &state)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, fmt.Errorf("gitlet: read the change from %s: %w", branch, err)
		}
		c.State = ChangeState(state)
		return &c, nil
	}
	return nil, nil
}

// matching is the rules whose pattern matches ref, in the rules' order.
func (ps Protections) matching(ref string) []PushRule {
	var out []PushRule
	for i, p := range ps.patterns {
		if p.Match(ref) {
			out = append(out, ps.rules[i])
		}
	}
	return out
}

// foldCase is s with each rune its case orbit's smallest, so two strings
// strings.EqualFold calls equal fold alike (the Kelvin sign is K).
func foldCase(s string) string { return strings.Map(foldRune, s) }

// foldRune is the smallest rune of r's unicode.SimpleFold orbit.
func foldRune(r rune) rune {
	if r < utf8.RuneSelf {
		if 'a' <= r && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		least = min(least, f)
	}
	return least
}

// microsFrom is the first whole microsecond at or after t, as code_refs.at
// holds instants, so a bound with a fraction of one keeps exactly its side.
func microsFrom(t time.Time) int64 {
	m := store.EncodeTime(t)
	if store.DecodeTime(m).Before(t) {
		m++
	}
	return m
}

// The listing's statements, each registered in plan_test.go.
const (
	// refsMovedSQL is a page of a repository's branches newest move first,
	// within [?2, ?3] of code_refs.at; refsMovedAfterSQL the page past a
	// cursor (?4 its at, ?5 its ref), whose upper bound is clamped to ?4.
	refsMovedSQL = `SELECT ref, target, position, at FROM code_refs INDEXED BY ` + movedRefsKey + `
		WHERE repo_id = ? AND ` + movedRefs + ` AND at >= ? AND at <= ?
		ORDER BY at DESC, ref DESC LIMIT ?`
	refsMovedAfterSQL = `SELECT ref, target, position, at FROM code_refs INDEXED BY ` + movedRefsKey + `
		WHERE repo_id = ? AND ` + movedRefs + ` AND at >= ? AND at <= ? AND (at < ? OR ref < ?)
		ORDER BY at DESC, ref DESC LIMIT ?`
	// activeBranchesSQL counts, up to its limit, the branches other than ?3
	// that moved at or after ?2.
	activeBranchesSQL = `SELECT count(*) FROM (SELECT 1 FROM code_refs INDEXED BY ` + movedRefsKey + `
		WHERE repo_id = ? AND ` + movedRefs + ` AND at >= ? AND ref != ? LIMIT ?)`
	// pushesAtSQL is the pushes one record of a repository wrote: what a
	// ref's mover is read from.
	pushesAtSQL = `SELECT push_id, actor, actor_kind, actor_seat, via, commands, row_bytes FROM code_pushes
		WHERE repo_id = ? AND position = ? ORDER BY push_id`
	// liveChangeOfSourceSQL is the live change a branch heads, and
	// newestChangeOfSourceSQL the newest change from it.
	liveChangeOfSourceSQL = `SELECT number, title, state FROM code_changes INDEXED BY code_changes_open_idx
		WHERE repo_id = ? AND source_ref = ? AND ` + liveChanges + ` LIMIT 1`
	newestChangeOfSourceSQL = `SELECT number, title, state FROM code_changes INDEXED BY ` + changesSourceKey + `
		WHERE repo_id = ? AND source_ref = ? ORDER BY number DESC LIMIT 1`
)
