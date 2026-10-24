package artifactsdomain

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/foldername"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/store"
)

// THE FOLDER TREE IN MEMORY: one shape the writer's snapshot, the applier and
// the reader all read the tree into, so a depth, a namespace, a path and a
// subtree are computed one way wherever they are asked.
//
// # Why the whole tree is read
//
// A company holds at most [artifacts.MaxFolders] live folders, and every
// question a folder write asks (sibling names, the ancestor chain a move must
// not close into a cycle, the depth beneath a folder, the live count) is about
// the tree as a whole. Reading every live row once is one indexed statement,
// cheaper and simpler than a walk of statements per question, and it is the
// snapshot the tree subject's arbitration covers.
//
// # The live tree is closed
//
// A live folder's parent is always a live folder or the top level: a removal
// moves every child of the folder it removes up to that folder's parent in the
// same apply, and a create or a move names only a live parent. So a parent
// chain read from live rows never reaches a tombstone, and a tombstone is a
// leaf nothing live sits under.

// folderRow is one row of artifact_folders.
type folderRow struct {
	id, parent, root, name, nameKey, owner string
	ownerKind                              iam.ActorKind
	maxDepth                               int
	status                                 FolderStatus
	createdBy, updatedBy                   iam.Actor
	createdAt, updatedAt                   int64
	version                                int64
}

// folderColumns is every column a folderRow is read from, in [scanFolderRow]'s
// order, qualified by the alias `f`.
const folderColumns = `f.id, f.parent, f.root, f.name, f.name_key, f.owner, f.owner_kind,
	f.max_depth, f.status, f.created_by, f.created_by_kind, f.created_at, f.updated_by,
	f.updated_by_kind, f.updated_at, f.version`

// scanFolderRow reads one folderRow from a row of [folderColumns].
func scanFolderRow(row rowScanner) (*folderRow, error) {
	var (
		f                                                       folderRow
		ownerKind, status, createdBy, createdKind, updatedBy, k string
	)
	if err := row.Scan(&f.id, &f.parent, &f.root, &f.name, &f.nameKey, &f.owner, &ownerKind,
		&f.maxDepth, &status, &createdBy, &createdKind, &f.createdAt, &updatedBy, &k,
		&f.updatedAt, &f.version); err != nil {
		return nil, err
	}
	f.ownerKind, f.status = iam.ActorKind(ownerKind), FolderStatus(status)
	f.createdBy = iam.Actor{Name: createdBy, Kind: iam.ActorKind(createdKind)}
	f.updatedBy = iam.Actor{Name: updatedBy, Kind: iam.ActorKind(k)}
	return &f, nil
}

// folderTree is a set of folder rows and what they are related by.
type folderTree struct {
	// rows is every row read, by id.
	rows map[string]*folderRow

	// kids is every LIVE row by its parent ("" for the top level), siblings
	// A to Z by name key and then by id, so every reader walks one order.
	kids map[string][]*folderRow

	// shares is every top-level folder's shares, by its id, ordered by
	// audience so a folder reads the same on every node.
	shares map[string][]artifacts.Share
}

// readFolderTree reads the folders a filter keeps (`where` over the aliases
// `f`, the folder, and `r`, its top-level folder) and the shares of every
// top-level folder among them.
func readFolderTree(ctx context.Context, tx *sql.Tx, where string, args ...any) (*folderTree, error) {
	query := `SELECT ` + folderColumns + ` FROM artifact_folders f
		JOIN artifact_folders r ON r.id = f.root`
	if where != "" {
		query += ` WHERE ` + where
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY f.name_key, f.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read the folder tree: %w", err)
	}
	t := &folderTree{rows: map[string]*folderRow{}, kids: map[string][]*folderRow{},
		shares: map[string][]artifacts.Share{}}
	for rows.Next() {
		f, scanErr := scanFolderRow(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("artifactsdomain: read the folder tree: %w", scanErr)
		}
		t.rows[f.id] = f
		if f.status == FolderLive {
			t.kids[f.parent] = append(t.kids[f.parent], f)
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("artifactsdomain: read the folder tree: %w", err)
	}
	for _, kids := range t.kids {
		slices.SortFunc(kids, func(a, b *folderRow) int {
			return cmp.Or(cmp.Compare(a.nameKey, b.nameKey), cmp.Compare(a.id, b.id))
		})
	}
	if t.shares, err = readFolderShares(ctx, tx); err != nil {
		return nil, err
	}
	return t, nil
}

// readLiveTree is every live folder: the writer's and the applier's view.
func readLiveTree(ctx context.Context, tx *sql.Tx) (*folderTree, error) {
	return readFolderTree(ctx, tx, `f.status = ?`, string(FolderLive))
}

// readFolderShares reads every top-level folder's shares. The table holds
// one preset row at most per top-level folder, so reading it whole is
// cheaper than a statement per folder.
func readFolderShares(ctx context.Context, tx *sql.Tx) (map[string][]artifacts.Share, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT folder_id, audience_kind, audience_id, role, granted_by, granted_by_kind, granted_at
		FROM artifact_folder_shares ORDER BY folder_id, audience_kind, audience_id`)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read the folder shares: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]artifacts.Share{}
	for rows.Next() {
		var (
			id, kind, audience, role, by, byKind string
			at                                   int64
		)
		if err := rows.Scan(&id, &kind, &audience, &role, &by, &byKind, &at); err != nil {
			return nil, fmt.Errorf("artifactsdomain: read the folder shares: %w", err)
		}
		out[id] = append(out[id], artifacts.Share{
			Audience:  artifacts.Audience{Kind: artifacts.AudienceKind(kind), ID: audience},
			Role:      artifacts.Role(role),
			GrantedBy: iam.Actor{Name: by, Kind: iam.ActorKind(byKind)},
			GrantedAt: store.DecodeTime(at),
		})
	}
	return out, rows.Err()
}

// live is the live folder id names, or nil.
func (t *folderTree) live(id string) *folderRow {
	if f := t.rows[id]; f != nil && f.status == FolderLive {
		return f
	}
	return nil
}

// liveCount is how many live folders the tree holds.
func (t *folderTree) liveCount() int {
	n := 0
	for _, kids := range t.kids {
		n += len(kids)
	}
	return n
}

// depth is a live folder's depth, 1 at the top level, and 0 for the top level
// itself (id ""). The walk is bounded by the rows it holds, so rows that
// formed a cycle (which no apply writes) end it rather than spin.
func (t *folderTree) depth(id string) int {
	d := 0
	for at := id; at != "" && d <= len(t.rows); d++ {
		f := t.rows[at]
		if f == nil {
			break
		}
		at = f.parent
	}
	return d
}

// height is how many levels a live folder's subtree spans, 1 for a folder
// holding no folder.
func (t *folderTree) height(id string) int {
	best := 0
	for _, kid := range t.kids[id] {
		best = max(best, t.height(kid.id))
	}
	return best + 1
}

// within reports whether id is under, or is, ancestor: the question a move
// asks so a folder never moves into itself or beneath itself.
func (t *folderTree) within(id, ancestor string) bool {
	for at, steps := id, 0; at != "" && steps <= len(t.rows); steps++ {
		if at == ancestor {
			return true
		}
		f := t.rows[at]
		if f == nil {
			return false
		}
		at = f.parent
	}
	return false
}

// subtree is a live folder and every live folder beneath it, parents first.
func (t *folderTree) subtree(id string) []string {
	out := []string{id}
	for i := 0; i < len(out); i++ {
		for _, kid := range t.kids[out[i]] {
			out = append(out, kid.id)
		}
	}
	return out
}

// path is a live folder's path as S3 writes a prefix, its names from the top
// level each followed by [ListDelimiter] ("Marketing/Q4/"), and "" for the top
// level itself.
func (t *folderTree) path(id string) string {
	var names []string
	for at, steps := id, 0; at != "" && steps <= len(t.rows); steps++ {
		f := t.rows[at]
		if f == nil {
			return ""
		}
		names = append(names, f.name)
		at = f.parent
	}
	slices.Reverse(names)
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, ListDelimiter) + ListDelimiter
}

// companyWide reports whether a top-level folder is shared with the whole
// company: what puts it in the company's namespace of top-level names.
func companyWide(shares []artifacts.Share) bool {
	return slices.ContainsFunc(shares, func(s artifacts.Share) bool {
		return s.Audience.Kind == artifacts.AudienceCompany
	})
}

// topLevel is one namespace of top-level folder names.
//
// TWO KINDS OF NAMESPACE, so a name is never an oracle: the company's
// top-level folders are unique among themselves (everyone sees all of them),
// and each owner's other top-level folders are unique among that owner's own
// (nobody else's private name can refuse theirs). A private folder may share a
// name with a company one, and a path that then names both is refused naming
// both ([FolderAmbiguous]) rather than resolved by a guess.
type topLevel struct {
	company bool
	owner   string
}

// namespaceOf is the namespace a top-level folder owned by owner and holding
// shares sits in.
func namespaceOf(owner string, shares []artifacts.Share) topLevel {
	if companyWide(shares) {
		return topLevel{company: true}
	}
	return topLevel{owner: owner}
}

// siblings is the live folders a folder placed under parent is named among:
// parent's live children, or at the top level the live top-level folders of
// ns.
func (t *folderTree) siblings(parent string, ns topLevel) []*folderRow {
	if parent != "" {
		return t.kids[parent]
	}
	var out []*folderRow
	for _, f := range t.kids[""] {
		if namespaceOf(f.owner, t.shares[f.id]) == ns {
			out = append(out, f)
		}
	}
	return out
}

// named is the live sibling of a folder placed under parent (in ns at the top
// level) whose name key is key, other than except, or nil.
func (t *folderTree) named(parent string, ns topLevel, key, except string) *folderRow {
	for _, f := range t.siblings(parent, ns) {
		if f.nameKey == key && f.id != except {
			return f
		}
	}
	return nil
}

// resolvePath is every live folder a path of names names: the names split on
// [ListDelimiter], one trailing delimiter allowed, each compared by its fold
// ([foldername.Key]). Several answers are possible only where two top-level
// namespaces hold one name; a segment that is not a folder name names
// nothing.
func (t *folderTree) resolvePath(path string) ([]*folderRow, error) {
	trimmed := strings.TrimSuffix(path, ListDelimiter)
	if trimmed == "" || strings.HasPrefix(trimmed, ListDelimiter) {
		return nil, fmt.Errorf("%w: %q is not a folder path: name the folders from the top "+
			"level, separated by %q", ErrInvalid, path, ListDelimiter)
	}
	frontier := []string{""}
	for _, segment := range strings.Split(trimmed, ListDelimiter) {
		if segment == "" {
			return nil, fmt.Errorf("%w: %q is not a folder path: it holds an empty name",
				ErrInvalid, path)
		}
		key, named := folderKey(segment)
		if !named {
			// A NAME NO FOLDER COULD HOLD names no folder: answered as
			// absent, the same answer a name nobody used gets.
			return nil, nil
		}
		var next []string
		for _, at := range frontier {
			for _, kid := range t.kids[at] {
				if kid.nameKey == key {
					next = append(next, kid.id)
				}
			}
		}
		if frontier = next; len(frontier) == 0 {
			return nil, nil
		}
	}
	out := make([]*folderRow, len(frontier))
	for i, id := range frontier {
		out[i] = t.rows[id]
	}
	return out, nil
}

// folderKey is the key a path segment is compared by, and false for a segment
// no folder could be named ([foldername.Check]).
func folderKey(segment string) (string, bool) {
	_, key, err := foldername.Check(segment)
	return key, err == nil
}

// visibilityOf is a top-level folder's preset, read off its shares: no share is
// "only its owner", exactly the company as contributor is "everyone", and
// anything else is a set no preset names.
func visibilityOf(shares []artifacts.Share) FolderVisibility {
	switch {
	case len(shares) == 0:
		return FolderPrivate
	case len(shares) == 1 && shares[0].Audience.Kind == artifacts.AudienceCompany &&
		shares[0].Role == artifacts.RoleContributor:
		return FolderCompany
	}
	return FolderCustom
}

// folder is a row as the tree shows it: its depth, its top-level folder's
// owner, shares and preset.
func (t *folderTree) folder(f *folderRow) Folder {
	out := Folder{
		ID: f.id, Parent: f.parent, Root: f.root, Name: f.name, Owner: f.owner,
		OwnerKind: f.ownerKind, Depth: t.depth(f.id), MaxDepth: f.maxDepth, Status: f.status,
		CreatedBy: f.createdBy, CreatedAt: store.DecodeTime(f.createdAt),
		UpdatedBy: f.updatedBy, UpdatedAt: store.DecodeTime(f.updatedAt), Version: f.version,
	}
	if root := t.rows[f.root]; root != nil {
		out.RootOwner, out.RootOwnerKind = root.owner, root.ownerKind
	}
	out.Shares = t.shares[f.root]
	out.Visibility = visibilityOf(out.Shares)
	return out
}

// preorder is every live folder, parents before children and siblings A to Z:
// the order the tree is answered in.
func (t *folderTree) preorder() []*folderRow {
	var out []*folderRow
	var walk func(parent string)
	walk = func(parent string) {
		for _, f := range t.kids[parent] {
			out = append(out, f)
			walk(f.id)
		}
	}
	walk("")
	return out
}

// ids is the tree's live folders' ids, for a `json_each` term.
func (t *folderTree) ids() []string {
	out := make([]string, 0, len(t.rows))
	for id, f := range t.rows {
		if f.status == FolderLive {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
