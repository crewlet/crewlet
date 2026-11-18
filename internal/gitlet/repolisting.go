package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// A LISTING OF REPOSITORIES is read by name or by last push, over a whole index or one kind's
// range; where [ReadQuery.RepoFacts] asks, each row carries its card ([RepoCard]) and a first page
// the listing's total ([ReposTotal]).

// RepoSort is the order a listing of repositories walks. A closed set ([RepoSorts]).
type RepoSort string

const (
	// RepoSortName is by name; the zero sort reads as it.
	RepoSortName RepoSort = "name"
	// RepoSortPushed is by last push, newest first, then the never-pushed by name: the
	// repositories in use alone (code_repos_pushed_idx).
	RepoSortPushed RepoSort = "pushed"
)

// RepoSorts is the closed set.
func RepoSorts() []RepoSort { return []RepoSort{RepoSortName, RepoSortPushed} }

// Valid reports an order this build knows.
func (s RepoSort) Valid() bool { return slices.Contains(RepoSorts(), s) }

const (
	// MaxReposSized is the most repositories of a listing's kinds and statuses, readable or not, whose
	// size its total sums: config.CodeMaxRepos, so projects are summed whole; past it, no size.
	MaxReposSized = config.CodeMaxRepos
	// MaxLastPushWalk is the most push rows a card walks back past expiries: enough expiry pushes
	// to delete every seat branch a repository holds, and the push before them.
	MaxLastPushWalk = MaxRefsPerRepo/MaxCommandsPerPush + 1
)

// RepoCard is a repository's push mirror, its last push and its default branch's checks.
type RepoCard struct {
	// PushMirrored is a push mirror: its home pushes it to another host (mirror_to).
	PushMirrored bool `json:"push_mirrored,omitempty"`
	// LastPush is its newest push that was no expiry; nil where the history no longer holds it.
	LastPush *LastPush `json:"last_push,omitempty"`
	// TipChecks is where its default branch's head stands, each check's latest attempt for that
	// branch; nil for an empty repository or a head no attempt is kept of.
	TipChecks *CheckRollup `json:"tip_checks,omitempty"`
}

// LastPush is a push as its history row names it ([PushRow]), with what became of its commands.
type LastPush struct {
	PushID    string    `json:"push_id"`
	At        time.Time `json:"at"`
	Actor     string    `json:"actor"`
	ActorKind string    `json:"actor_kind"`
	ActorSeat string    `json:"actor_seat,omitempty"`
	Via       PushVia   `json:"via"`
	// Outcome is its commands counted by what became of each: a refused one recorded beside
	// those that landed counts as refused.
	Outcome CommandCounts `json:"outcome"`
}

// ReposTotal is a listing's whole, all its pages: how many repositories and their live bytes.
type ReposTotal struct {
	// Repos is counted from the tallies, exact at any size.
	Repos int64 `json:"repos"`
	// LiveBytes is summed at read time; nil where the company holds more than MaxReposSized
	// repositories of the listing's kinds and statuses, whatever the asker reads.
	LiveBytes *int64 `json:"live_bytes,omitempty"`
}

// validateRepos is [ReadQuery.validate] for a `repos` listing: a kind and an order this build
// knows, each over an index of its own, so neither combines with a prefix or the protected ones.
func (q ReadQuery) validateRepos() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.RepoKind != "" && !q.RepoKind.Valid():
		return refused("repo_kind", fmt.Sprintf("%q is not one of %s", q.RepoKind, words.Or(RepoKinds())))
	case q.RepoSort != "" && !q.RepoSort.Valid():
		return refused("repo_sort", fmt.Sprintf("%q is not one of %s", q.RepoSort, words.Or(RepoSorts())))
	case q.RepoKind != "" && (q.Prefix != "" || q.Protected):
		return refused("repo_kind", "a listing narrowed by a prefix or to the protected repositories lists "+
			"every kind")
	case q.RepoSort == RepoSortPushed && (q.Prefix != "" || q.Protected):
		return refused("repo_sort", "a listing narrowed by a prefix or to the protected repositories is by name")
	case q.RepoSort == RepoSortPushed && slices.ContainsFunc(q.Status, func(s RepoStatus) bool { return !s.live() }):
		return refused("repo_sort", "only the repositories in use (importing, active or archived) are listed "+
			"by last push")
	}
	return nil
}

// listingRow is one row a listing's statement read, and the cursor that resumes after it.
type listingRow struct {
	v      repoView
	head   string
	cursor string
}

// reposListed reads up to limit rows of q's listing in its order: by name over the name, the
// protected or the kind index, or by last push over each kind's pushed range, merged.
func reposListed(ctx context.Context, tx *sql.Tx, q ReadQuery, statuses string, limit int) ([]listingRow, error) {
	if q.RepoSort == RepoSortPushed {
		return pushedListed(ctx, tx, q, statuses, limit)
	}
	if q.Cursor != "" && !ValidName(q.Cursor) {
		return nil, fmt.Errorf("%w: %q is not a repository name, where a listing by name resumes", ErrBadCursor,
			q.Cursor)
	}
	// The protected ones ([ReadQuery.Protected]) and one kind ([ReadQuery.RepoKind]) each walk, in
	// the same order, an index holding no other row; a prefix ([ReadQuery.Prefix]) reads one range
	// of either name index, past a cursor that must begin with it.
	var (
		listing string
		args    []any
	)
	switch {
	case q.RepoKind != "":
		listing, args = kindReposSQL, []any{statuses, string(q.RepoKind), q.Cursor, limit}
	case q.Prefix == "":
		listing, args = reposSQL, []any{statuses, q.Cursor, limit}
		if q.Protected {
			listing = protectedReposSQL
		}
	case q.Cursor == "":
		listing, args = reposUnderFirstSQL, []any{statuses, q.Prefix, prefixEnd(q.Prefix), limit}
		if q.Protected {
			listing = protectedReposUnderFirstSQL
		}
	case !strings.HasPrefix(q.Cursor, q.Prefix):
		return nil, fmt.Errorf("%w: %q is not a repository name beginning %q", ErrBadCursor, q.Cursor, q.Prefix)
	default:
		listing, args = reposUnderAfterSQL, []any{statuses, q.Cursor, prefixEnd(q.Prefix), limit}
		if q.Protected {
			listing = protectedReposUnderAfterSQL
		}
	}
	return listedBy(ctx, tx, func(name string, _ sql.NullInt64) string { return name }, listing, args...)
}

// pushedListed is [reposListed] by last push: each kind's dated range from the cursor, its
// never-pushed range after it where the dated one runs short, merged in the listing's order.
func pushedListed(ctx context.Context, tx *sql.Tx, q ReadQuery, statuses string, limit int) ([]listingRow,
	error) {

	at, name, never, err := pushedCursor(q.Cursor)
	if err != nil {
		return nil, err
	}
	kinds := RepoKinds()
	if q.RepoKind != "" {
		kinds = []RepoKind{q.RepoKind}
	}
	var all []listingRow
	for _, k := range kinds {
		var read []listingRow
		if !never {
			listing, args := pushedReposSQL, []any{statuses, string(k), int64(math.MinInt64), limit}
			if q.Cursor != "" {
				listing, args = pushedReposAfterSQL, []any{statuses, string(k), at, at, name, limit}
			}
			if read, err = listedBy(ctx, tx, pushedCursorOf, listing, args...); err != nil {
				return nil, err
			}
		}
		if len(read) < limit {
			after := ""
			if never {
				after = name
			}
			var undated []listingRow
			if undated, err = listedBy(ctx, tx, pushedCursorOf, neverPushedReposSQL, statuses, string(k), after,
				limit-len(read)); err != nil {
				return nil, err
			}
			read = append(read, undated...)
		}
		all = append(all, read...)
	}
	slices.SortStableFunc(all, func(a, b listingRow) int {
		pa, pb := a.v.lastPushAt, b.v.lastPushAt
		switch {
		case pa.Valid != pb.Valid && pa.Valid:
			return -1
		case pa.Valid != pb.Valid:
			return 1
		case pa.Valid:
			return cmp.Or(cmp.Compare(pb.Int64, pa.Int64), cmp.Compare(b.v.ref.Name, a.v.ref.Name))
		}
		return cmp.Compare(a.v.ref.Name, b.v.ref.Name)
	})
	return all[:min(len(all), limit)], nil
}

// pushedCursor is where a listing by last push resumes: after a push's instant and a name, or
// after a name among the never-pushed; nothing for a first page.
func pushedCursor(cursor string) (at int64, name string, never bool, err error) {
	if cursor == "" {
		return 0, "", false, nil
	}
	head, name, found := strings.Cut(cursor, " ")
	if found && ValidName(name) {
		if head == "never" {
			return 0, name, true, nil
		}
		if at, err = strconv.ParseInt(head, 10, 64); err == nil {
			return at, name, false, nil
		}
	}
	return 0, "", false, fmt.Errorf("%w: %q is not a place in a listing by last push: <microseconds> <name> "+
		"or never <name>", ErrBadCursor, cursor)
}

// pushedCursorOf is the cursor a listing by last push resumes after a row at: its instant as
// stored and its name, or never and its name.
func pushedCursorOf(name string, pushed sql.NullInt64) string {
	if !pushed.Valid {
		return "never " + name
	}
	return strconv.FormatInt(pushed.Int64, 10) + " " + name
}

// listedBy is the rows listing reads, each with the cursor cursorOf makes of it.
func listedBy(ctx context.Context, tx *sql.Tx, cursorOf func(name string, pushed sql.NullInt64) string,
	listing string, args ...any) ([]listingRow, error) {

	rows, err := tx.QueryContext(ctx, listing, args...)
	if err != nil {
		return nil, fmt.Errorf("gitlet: list repositories: %w", err)
	}
	var out []listingRow
	if err = eachRow(rows, func(s scanner) (err error) {
		var l listingRow
		if l, err = scanListingRow(s, cursorOf); err == nil {
			out = append(out, l)
		}
		return err
	}); err != nil {
		return nil, fmt.Errorf("gitlet: list repositories: %w", err)
	}
	return out, nil
}

// scanListingRow reads one row of [reposColumns].
func scanListingRow(s scanner, cursorOf func(name string, pushed sql.NullInt64) string) (listingRow, error) {
	var (
		v                                                 repoView
		status, managed, kind, owners, visibility, search string
		deleted                                           sql.NullInt64
		active                                            int64
		head                                              sql.NullString
	)
	if err := s.Scan(&v.ref.ID, &v.ref.Name, &status, &managed, &kind, &owners, &v.ref.Owner.Ref,
		&visibility, &search, &v.ref.Mirrored, &v.ref.DefaultBranch, &v.ref.Description, &deleted, &active,
		&v.ref.Refs, &v.ref.LivePacks, &v.ref.LiveBytes, &v.ref.LiveObjects, &v.ref.DropEpoch, &v.pushes,
		&v.lastPushAt, &head); err != nil {
		return listingRow{}, err
	}
	v.ref.Status, v.ref.ManagedBy, v.ref.Owner.Kind = RepoStatus(status), ManagedBy(managed), OwnerKind(owners)
	v.ref.Kind, v.ref.Visibility, v.ref.Search = RepoKind(kind), Visibility(visibility), SearchMode(search)
	v.ref.LastActivityAt = activityAt(active)
	if deleted.Valid {
		v.ref.DeletedAt = store.DecodeTime(deleted.Int64)
	}
	return listingRow{v: v, head: head.String, cursor: cursorOf(v.ref.Name, v.lastPushAt)}, nil
}

// carded fills s.Card's last push and default branch's checks, charging the push rows it
// walks to pg where one is given; withFacts made the card.
func (r *Reader) carded(ctx context.Context, tx *sql.Tx, s *RepoSummary, pg *page) (err error) {
	if s.Card.LastPush, err = r.lastPushOf(ctx, tx, s.ID, pg); err != nil {
		return err
	}
	if s.Head != "" {
		s.Card.TipChecks, err = rollupOfTarget(ctx, tx, s.ID, s.Head, branchPrefix+s.DefaultBranch)
	}
	return err
}

// lastPushOf is repoID's newest push that was no expiry, in its history's order, read back at
// most r.lastPushWalk rows; nil where none is within them.
func (r *Reader) lastPushOf(ctx context.Context, tx *sql.Tx, repoID string, pg *page) (*LastPush, error) {
	rows, err := tx.QueryContext(ctx, latestPushesSQL, repoID, r.lastPushWalk)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's last push: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			p             LastPush
			at, rowBytes  int64
			via, commands string
		)
		if err = rows.Scan(&p.PushID, &at, &p.Actor, &p.ActorKind, &p.ActorSeat, &via, &commands,
			&rowBytes); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's last push: %w", repoID, err)
		}
		if pg != nil {
			pg.charge(int(rowBytes))
		}
		if p.Via = PushVia(via); !p.Via.Valid() {
			return nil, fmt.Errorf("%w: push %s's row names its way as %q, which is not one of %s",
				ErrInvalid, p.PushID, via, words.Or(PushVias()))
		}
		if p.Via == ViaExpiry {
			continue
		}
		var cmds []Command
		if err = json.Unmarshal([]byte(commands), &cmds); err != nil {
			return nil, fmt.Errorf("gitlet: push %s's commands: %w", p.PushID, err)
		}
		for _, c := range cmds {
			p.Outcome.add(c)
		}
		p.At = store.DecodeTime(at)
		return &p, nil
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read %s's last push: %w", repoID, err)
	}
	return nil, nil
}

// reposTotal is q's listing whole: its count from the repository tallies the asker reads, its
// size from a walk of at most r.reposSized repositories, judged a tally key at a time.
func (r *Reader) reposTotal(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, statuses []RepoStatus,
	statusList string) (*ReposTotal, error) {

	reads := func(key tallyKey) bool {
		_, refused := JudgeRead(q.Asker, q.CeilingSeat, RepoRef{Owner: key.owner, Visibility: key.visibility}, chart)
		return refused == nil
	}
	kinds := RepoKinds()
	if q.RepoKind != "" {
		kinds = []RepoKind{q.RepoKind}
	}
	var total ReposTotal
	if err := eachRepoTally(ctx, tx, func(key repoTallyKey, c RepoCounts) error {
		if slices.Contains(kinds, key.kind) && reads(key.tallyKey) {
			total.Repos += c.sum(statuses)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sized := map[tallyKey]int64{}
	left := r.reposSized
	for _, k := range kinds {
		rows, err := tx.QueryContext(ctx, reposSizedSQL, string(k), statusList, left+1)
		if err != nil {
			return nil, fmt.Errorf("gitlet: size the listing's %s repositories: %w", k, err)
		}
		if err = eachRow(rows, func(s scanner) (err error) {
			var (
				visibility, ownerKind, owner string
				bytes                        int64
			)
			if err = s.Scan(&visibility, &ownerKind, &owner, &bytes); err == nil {
				left--
				key := tallyKey{visibility: Visibility(visibility), owner: Owner{Kind: OwnerKind(ownerKind), Ref: owner}}
				sized[key] += bytes
			}
			return err
		}); err != nil {
			return nil, fmt.Errorf("gitlet: size the listing's %s repositories: %w", k, err)
		}
		if left < 0 {
			return &total, nil
		}
	}
	var bytes int64
	for key, n := range sized {
		if reads(key) {
			bytes += n
		}
	}
	total.LiveBytes = &bytes
	return &total, nil
}

// The listing's statements, each registered in plan_test.go. Each one-kind statement reads its
// kind's own range of an index that leads with the kind, so another kind's rows cost it nothing.
const (
	// kindReposSQL is a page of one kind's listing by name, past its cursor.
	kindReposSQL = reposColumns + ` FROM code_repos r INDEXED BY ` + kindReposKey + reposHead + `
		WHERE r.status IN (SELECT value FROM json_each(?)) AND r.kind = ? AND r.name > ? ORDER BY r.name LIMIT ?`
	// pushedReposFrom is one kind's repositories in use by last push, with the predicate the
	// pushed index is partial on repeated verbatim; the statements below end it.
	pushedReposFrom = reposColumns + ` FROM code_repos r INDEXED BY ` + pushedReposKey + reposHead + `
		WHERE r.` + reposInUse + ` AND r.status IN (SELECT value FROM json_each(?)) AND r.kind = ? AND `
	// pushedReposSQL is a first page of the dated range, newest first, its index walked backwards
	// from its end down to ?3 ([math.MinInt64], every instant a row holds, as a seek's bound where
	// IS NOT NULL is none); pushedReposAfterSQL the same past a cursor's instant and name.
	pushedReposSQL = pushedReposFrom + `r.last_push_at > ?
		ORDER BY r.last_push_at DESC, r.name DESC LIMIT ?`
	pushedReposAfterSQL = pushedReposFrom + `r.last_push_at <= ? AND (r.last_push_at < ? OR r.name < ?)
		ORDER BY r.last_push_at DESC, r.name DESC LIMIT ?`
	// neverPushedReposSQL is the never-pushed range by name, past a name.
	neverPushedReposSQL = pushedReposFrom + `r.last_push_at IS NULL AND r.name > ? ORDER BY r.name LIMIT ?`
	// latestPushesSQL is a repository's pushes newest first, as its history reads them.
	latestPushesSQL = `SELECT push_id, at, actor, actor_kind, actor_seat, via, commands, row_bytes
		FROM code_pushes INDEXED BY code_pushes_repo_idx WHERE repo_id = ?
		ORDER BY position DESC, push_id DESC LIMIT ?`
	// reposSizedSQL is one kind's repositories of the listing's statuses, who reads them and their bytes.
	reposSizedSQL = `SELECT visibility, owner_kind, owner, live_bytes FROM code_repos INDEXED BY ` + kindReposKey + `
		WHERE kind = ? AND status IN (SELECT value FROM json_each(?)) LIMIT ?`
)
