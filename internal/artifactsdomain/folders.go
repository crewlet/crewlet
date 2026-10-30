package artifactsdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/foldername"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// THE FOLDER TREE (ADR-0047: an artifact lives in one folder, and a folder
// grants nothing).
//
// One tree per company, on ONE subject ([FolderSubject]). Each artifact is
// filed in at most one folder ([Head.Folder], '' for the top level), and
// filing it never widens or narrows who may open it: an artifact's access is
// still its owner and its own shares (ADR-0034). A folder's OWN visibility is
// its top-level folder's owner and shares, decided as a relation exactly as an
// artifact's is (authz.KindArtifactFolder), so every ancestor of a folder a
// reader sees is one they see too, and sibling names never disclose a folder
// the reader cannot see.
//
// Filing asks two decisions, both the API's: editor on the artifact (with the
// artifacts app) and contributor on the destination folder, each through the
// requester ceiling. The folder's owner, the owner's lead and an administrator
// may rename, move, remove and unfile anything they can see. Removing a folder
// moves everything in it up one level; nothing is trashed and nobody loses
// access.

// FolderNamespace scopes the derivation of a folder's id from its creating
// operation and its place in the chain. FIXED for the life of the format: a new
// namespace would give a retry that straddles an upgrade a second folder.
var FolderNamespace = uuid.MustParse("3b8e2f61-5c4a-5d09-8e7f-1a2b3c4d5e6f")

// FolderIDOf is the folder a create under opID makes at index in its chain.
func FolderIDOf(opID string, index int) uuid.UUID {
	return uuid.NewSHA1(FolderNamespace, []byte(opID+"/"+strconv.Itoa(index)))
}

// FolderVisibility is a top-level folder's preset, the two the product offers:
// everyone at the company (a company share as contributor) or only its owner
// (no share).
type FolderVisibility string

const (
	// FolderCompany is "Everyone at <company>": the company holds a
	// contributor share on the top-level folder.
	FolderCompany FolderVisibility = "company"

	// FolderPrivate is "Only you": no share, so its owner (and the owner's
	// lead, for an agent's) alone sees it.
	FolderPrivate FolderVisibility = "private"

	// FolderCustom is a top-level folder whose shares are neither preset:
	// granted through the share machinery a later surface offers. Answered,
	// never asked for.
	FolderCustom FolderVisibility = "custom"
)

// FolderVisibilities are the presets a create may ask for, in the order the
// product offers them.
var FolderVisibilities = []FolderVisibility{FolderCompany, FolderPrivate}

// Valid reports whether v is a preset a create may ask for.
func (v FolderVisibility) Valid() bool { return v == FolderCompany || v == FolderPrivate }

// Folder is one folder as the tree and a point read show it.
type Folder struct {
	ID string `json:"id"`

	// Parent is the folder it sits in, empty for a top-level folder, and
	// Root its top-level folder (its own id for a top-level folder).
	Parent string `json:"parent,omitempty"`
	Root   string `json:"root"`

	Name string `json:"name"`

	Owner     string        `json:"owner"`
	OwnerKind iam.ActorKind `json:"owner_kind"`

	// RootOwner and RootOwnerKind are its top-level folder's owner (its
	// own, for a top-level folder): with Shares, what decides who sees the
	// folder and who may file into it ([FolderObject]).
	RootOwner     string        `json:"root_owner"`
	RootOwnerKind iam.ActorKind `json:"root_owner_kind"`

	// Depth is 1 for a top-level folder; MaxDepth the bound stamped when it
	// was created.
	Depth    int `json:"depth"`
	MaxDepth int `json:"max_depth"`

	// Visibility is the top-level folder's preset, read off its shares.
	Visibility FolderVisibility `json:"visibility"`

	// Shares are the top-level folder's shares: what a decision about the
	// folder is made on. A surface shows them to the folder's owner and
	// managers only, as an artifact's are.
	Shares []artifacts.Share `json:"shares,omitempty"`

	Status    FolderStatus `json:"status"`
	CreatedBy iam.Actor    `json:"created_by"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedBy iam.Actor    `json:"updated_by"`
	UpdatedAt time.Time    `json:"updated_at"`

	// Version is the packed position of the last record that wrote the
	// folder's row.
	Version int64 `json:"version"`
}

// FolderStatus is whether a folder is live or a removed tombstone.
type FolderStatus string

const (
	// FolderLive is a folder in the tree.
	FolderLive FolderStatus = "live"

	// FolderRemoved is a removed folder's tombstone: kept so a late retry
	// of its create never brings the id back, and in no reader's tree.
	FolderRemoved FolderStatus = "removed"
)

// FolderItem is one folder of a viewer's tree, with what is directly in it
// that the viewer can see.
type FolderItem struct {
	Folder

	// Artifacts and Folders count what sits DIRECTLY in the folder that the
	// viewer can see, from the listing's own visibility predicate in the
	// same transaction: a total over the subtree is the client's sum.
	Artifacts int `json:"artifacts"`
	Folders   int `json:"folders"`
}

// FolderTree is every live folder a viewer can see, parents before children
// and siblings A to Z, and what it is true as of.
type FolderTree struct {
	Folders []FolderItem `json:"folders"`

	// TopLevel is how many artifacts the viewer can see that are filed in
	// no folder they can see ("Not in a folder").
	TopLevel int `json:"top_level"`

	Level    statelog.ReadLevel `json:"read_level"`
	Position statelog.Position  `json:"position"`
	Complete bool               `json:"complete"`
}

// FolderCreateInput is one folder, or a chain of them, to create.
type FolderCreateInput struct {
	// Parent is the folder the chain is created in, empty for the top
	// level.
	Parent string

	// Names is the chain beneath Parent, parents first: one name, or a
	// path's names (at most artifacts.MaxFolderDepth), each checked by
	// foldername.Check.
	Names []string

	// Parents makes the create a `mkdir -p`: a name that already exists in
	// its place is reused rather than refused, and only the missing tail is
	// created, all in one record. Without it Names is exactly one name, and
	// an existing sibling of that name is ErrFolderNameTaken.
	Parents bool

	// Visibility is a top-level folder's preset, required when Parent is
	// empty and refused otherwise: a subfolder is decided by its top-level
	// folder.
	Visibility FolderVisibility

	// Owner is the seat handle (or machine login) that owns the folders,
	// and OwnerKind its actor kind.
	Owner     string
	OwnerKind iam.ActorKind
}

// FolderCreated is the chain a create answers, parents first: the folders it
// made and the ones a `mkdir -p` found already there.
type FolderCreated struct {
	Folders []Folder `json:"folders"`

	// Created is how many of Folders this write made.
	Created int `json:"created"`

	Result statelog.Result `json:"result"`
}

// ---- the authority table's objects --------------------------------------- //

// FolderObject is the authority table's object for SEEING a folder and
// FILING INTO it ([authz.ActionArtifactRead] through authz.Visible, and
// [authz.ActionArtifactFolderFile]): its top-level folder's owner and shares,
// so a whole tree is seen, and filed into, alike. The one builder every
// surface decides a folder's visibility on, the API's routes and the agents'
// tools alike, so the two cannot disagree about who sees a folder.
//
// agentsManageOthers is the company's `artifacts.agents_manage_others`, read
// by the surface, exactly as an artifact's object carries it.
func FolderObject(f Folder, agentsManageOthers bool) authz.Object {
	return authz.Object{
		Kind: authz.KindArtifactFolder, ID: f.ID, Owner: f.RootOwner, Shares: f.Shares,
		OwnerIsAgent: f.RootOwnerKind == iam.ActorAgent, AgentsManageOthers: agentsManageOthers,
	}
}

// FolderManagerObject is the authority table's object for MANAGING a folder
// (renaming, moving and removing it, and taking an artifact out of it): the
// folder's OWN owner beside its top-level folder's shares. The managing rows
// ask for owner, which no share grants, so they admit the folder's owner, the
// owner's lead where the owner is an agent, and an administrator, elevated.
// A surface asks it only of a folder [FolderObject] already shows the caller.
func FolderManagerObject(f Folder, agentsManageOthers bool) authz.Object {
	return authz.Object{
		Kind: authz.KindArtifactFolder, ID: f.ID, Owner: f.Owner, Shares: f.Shares,
		OwnerIsAgent: f.OwnerKind == iam.ActorAgent, AgentsManageOthers: agentsManageOthers,
	}
}

// ---- the writer's folder operations --------------------------------------- //

// The verbs a folder write's operation id is derived under: distinct from an
// artifact's own, so one operation key sent to a folder create and to an
// artifact create is two operations rather than the second answered as the
// first.
const (
	verbFolderCreate = "folder_create"
	verbFolderRename = "folder_rename"
	verbFolderMove   = "folder_move"
	verbFolderRemove = "folder_remove"
	verbMove         = "move"
)

// errFolderMoved is an artifact filed elsewhere between the read that named
// its folder in a move's scope and the move's decide.
var errFolderMoved = errors.New("artifactsdomain: the artifact was filed elsewhere under this move")

// treeScope is the scope of a record on the tree subject that writes only the
// tree: the subject sentinel, which is the tree's own path (`a/f`).
var treeScope = ScopeSet{Subject: true}

// CreateFolders creates one folder, or a chain of them (a path with its
// missing parents, `mkdir -p`), on the tree subject, in ONE record.
//
// THE CHAIN'S IDS ARE THE OPERATION'S ([FolderIDOf]), so a retry names the
// folders its first attempt made. A create that finds every name already in
// place (Parents) publishes nothing and answers the chain with Created 0.
//
// TWO DOMAIN RULES the writer holds, beside the tree's own: an agent creates
// subfolders only (a person decides what the company's top level holds, and a
// company-wide top-level folder is a company-wide audience an agent never
// grants), and a top-level folder states its visibility while a subfolder
// states none (its top-level folder decides it).
func (w *Writer) CreateFolders(ctx context.Context, c Call, in FolderCreateInput) (FolderCreated, error) {
	if err := validCall(c); err != nil {
		return FolderCreated{}, err
	}
	names, keys, err := checkFolderCreate(c, in)
	if err != nil {
		return FolderCreated{}, err
	}
	at := w.now()
	opID, err := w.operation(c, verbFolderCreate, in.Parent, in)
	if err != nil {
		return FolderCreated{}, err
	}
	var shares []artifacts.Share
	if in.Parent == "" && in.Visibility == FolderCompany {
		shares = []artifacts.Share{{
			Audience: artifacts.Audience{Kind: artifacts.AudienceCompany},
			Role:     artifacts.RoleContributor, GrantedBy: c.Authority.Actor, GrantedAt: at,
		}}
	}
	ns := namespaceOf(in.Owner, shares)
	var answer FolderCreated
	res, err := w.publish(ctx, request(FolderSubject(), treeScope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (_ statelog.Decision, err error) {
			tree, err := readLiveTree(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			specs, reused, err := planFolderCreate(tree, in, ns, names, keys, opID)
			if err != nil {
				return statelog.Decision{}, err
			}
			answer = FolderCreated{Folders: reused}
			if len(specs) == 0 {
				return statelog.Decision{}, nil
			}
			answer.Folders = append(answer.Folders, plannedFolders(tree, specs, in, c, at)...)
			answer.Created = len(specs)
			payload := FolderCreatePayload{
				V: baseRecordVersion, Folders: specs, Owner: in.Owner, OwnerKind: in.OwnerKind,
				MaxDepth: artifacts.MaxFolderDepth, MaxFolders: artifacts.MaxFolders,
				MaxChildren: artifacts.MaxFolderChildren, Authority: c.Authority,
			}
			if specs[0].Parent == "" {
				payload.Shares = shares
			}
			return w.decision(stamp, c, FolderSubject(), OpCreate, treeScope, opID, payload, at)
		}))
	answer.Result = res
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() {
		return answer, err
	}
	// THE ANSWER IS READ BACK FROM THE ROWS the record wrote, because a
	// retry answered from the ledger never ran the decide above.
	e, found, err := w.folderVerdict(ctx, opID)
	if err != nil || !found {
		return answer, err
	}
	if e.outcome == OutcomeRefused {
		return answer, w.refusal(ctx, "", e.reason, HistorySummary{}, 0)
	}
	chain, err := w.createdChain(ctx, in.Parent, e.summary.Created)
	if err != nil {
		return answer, err
	}
	answer.Folders, answer.Created = chain, len(e.summary.Created)
	return answer, nil
}

// checkFolderCreate refuses a create the domain would never write, and answers
// each name canonical with its key.
func checkFolderCreate(c Call, in FolderCreateInput) (names, keys []string, err error) {
	switch {
	case len(in.Names) == 0:
		return nil, nil, fmt.Errorf("%w: name the folder to create", ErrInvalid)
	case !in.Parents && len(in.Names) != 1:
		return nil, nil, fmt.Errorf("%w: a create names one folder; to create a path with its "+
			"missing parents, ask for parents", ErrInvalid)
	case len(in.Names) > artifacts.MaxFolderDepth:
		return nil, nil, fmt.Errorf("%w: the path names %d folders and folders go at most %d deep",
			ErrFolderDepth, len(in.Names), artifacts.MaxFolderDepth)
	case in.Parent == "" && c.Authority.Actor.Kind != iam.ActorHuman:
		// A TOP-LEVEL FOLDER IS A PERSON'S: it decides an audience (the
		// company, or its owner alone), which neither an agent nor an
		// unbound machine login decides for anybody.
		return nil, nil, fmt.Errorf("%w: a person creates top-level folders; create this one "+
			"inside a folder that already exists", ErrInvalid)
	case in.Parent == "" && !in.Visibility.Valid():
		return nil, nil, fmt.Errorf("%w: a top-level folder states who sees it, one of %s",
			ErrInvalid, words.Or(FolderVisibilities))
	case in.Parent != "" && in.Visibility != "":
		return nil, nil, fmt.Errorf("%w: a folder inside another is seen by whoever sees its "+
			"top-level folder, so it states no visibility of its own", ErrInvalid)
	case in.OwnerKind != iam.ActorAgent && in.OwnerKind != iam.ActorHuman &&
		in.OwnerKind != iam.ActorOperator:
		return nil, nil, fmt.Errorf("%w: owner kind %q: a folder is owned by a seat (an agent's "+
			"or a person's) or an unbound machine", ErrInvalid, in.OwnerKind)
	}
	if err = checkName("the owner", in.Owner); err != nil {
		return nil, nil, err
	}
	if in.Parent != "" && !validFolderID(in.Parent) {
		return nil, nil, fmt.Errorf("%w: %q", ErrFolderNotFound, in.Parent)
	}
	names, keys = make([]string, len(in.Names)), make([]string, len(in.Names))
	for i, raw := range in.Names {
		if names[i], keys[i], err = foldername.Check(raw); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	return names, keys, nil
}

// validFolderID reports whether id could name a folder: every folder's id is a
// uuid ([FolderIDOf]), so anything else names none, and is answered as an
// absent folder before any row is read.
func validFolderID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// planFolderCreate is the chain a create makes, decided in the snapshot: the
// folders a `mkdir -p` finds already in place, then the specs of those it
// creates, and every refusal the apply would make, made here first.
func planFolderCreate(tree *folderTree, in FolderCreateInput, ns topLevel, names, keys []string,
	opID string) (specs []FolderSpec, reused []Folder, err error) {

	current, depth := in.Parent, 0
	if in.Parent != "" {
		if tree.live(in.Parent) == nil {
			return nil, nil, fmt.Errorf("%w: %s", ErrFolderNotFound, in.Parent)
		}
		depth = tree.depth(in.Parent)
	}
	for i := range names {
		if len(specs) == 0 {
			if f := tree.named(current, ns, keys[i], ""); f != nil {
				if !in.Parents {
					return nil, nil, fmt.Errorf("%w: %q is already there", ErrFolderNameTaken, f.name)
				}
				reused = append(reused, tree.folder(f))
				current, depth = f.id, depth+1
				continue
			}
		}
		if depth++; depth > artifacts.MaxFolderDepth {
			return nil, nil, fmt.Errorf("%w: %q would sit %d deep and folders go at most %d deep",
				ErrFolderDepth, names[i], depth, artifacts.MaxFolderDepth)
		}
		id := FolderIDOf(opID, i).String()
		specs = append(specs, FolderSpec{ID: id, Parent: current, Name: names[i], NameKey: keys[i]})
		current = id
	}
	if len(specs) == 0 {
		return nil, reused, nil
	}
	if live := tree.liveCount(); live+len(specs) > artifacts.MaxFolders {
		return nil, nil, fmt.Errorf("%w: the company holds %d folders and holds at most %d; "+
			"remove folders it no longer uses", ErrFolderLimit, live, artifacts.MaxFolders)
	}
	if held := len(tree.kids[specs[0].Parent]); held+1 > artifacts.MaxFolderChildren {
		return nil, nil, fmt.Errorf("%w: that place holds %d folders and holds at most %d; "+
			"file the new folder inside one of them", ErrFolderLimit, held, artifacts.MaxFolderChildren)
	}
	return specs, reused, nil
}

// plannedFolders is the folders specs make as the decide sees them, for the
// answer to a create whose record is durable and not yet applied here: the
// instants are the broker's and unknown until it applies.
func plannedFolders(tree *folderTree, specs []FolderSpec, in FolderCreateInput, c Call,
	at time.Time) []Folder {

	root := specs[0].ID
	rootOwner, rootKind := in.Owner, in.OwnerKind
	shares := []artifacts.Share(nil)
	if specs[0].Parent != "" {
		parent := tree.rows[specs[0].Parent]
		root = parent.root
		if r := tree.rows[root]; r != nil {
			rootOwner, rootKind = r.owner, r.ownerKind
		}
		shares = tree.shares[root]
	} else if in.Visibility == FolderCompany {
		shares = []artifacts.Share{{Audience: artifacts.Audience{Kind: artifacts.AudienceCompany},
			Role: artifacts.RoleContributor, GrantedBy: c.Authority.Actor, GrantedAt: at}}
	}
	depth := tree.depth(specs[0].Parent)
	out := make([]Folder, len(specs))
	for i, s := range specs {
		out[i] = Folder{
			ID: s.ID, Parent: s.Parent, Root: root, Name: s.Name, Owner: in.Owner,
			OwnerKind: in.OwnerKind, RootOwner: rootOwner, RootOwnerKind: rootKind,
			Depth: depth + i + 1, MaxDepth: artifacts.MaxFolderDepth,
			Visibility: visibilityOf(shares), Shares: shares, Status: FolderLive,
			CreatedBy: c.Authority.Actor, UpdatedBy: c.Authority.Actor,
		}
	}
	return out
}

// createdChain is the chain a create applied, read from this node's rows: the
// folders above the first one it created, up to the parent it was asked for,
// then each folder it created that is still live.
func (w *Writer) createdChain(ctx context.Context, parent string, created []string) ([]Folder, error) {
	var out []Folder
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		tree, err := readLiveTree(ctx, tx)
		if err != nil {
			return err
		}
		out = nil
		if len(created) > 0 {
			if first := tree.live(created[0]); first != nil {
				var above []Folder
				for at := first.parent; at != "" && at != parent; {
					f := tree.live(at)
					if f == nil {
						break
					}
					above = append(above, tree.folder(f))
					at = f.parent
				}
				slices.Reverse(above)
				out = append(out, above...)
			}
		}
		for _, id := range created {
			if f := tree.live(id); f != nil {
				out = append(out, tree.folder(f))
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read the folders a create made: %w", err)
	}
	return out, nil
}

// RenameFolder renames one folder. A rename to the name it already has
// publishes nothing and answers success; a rename that changes only the case
// is a rename.
func (w *Writer) RenameFolder(ctx context.Context, c Call, id, name string) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	name, key, err := foldername.Check(name)
	if err != nil {
		return Written{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !validFolderID(id) {
		return Written{}, fmt.Errorf("%w: %q", ErrFolderNotFound, id)
	}
	at := w.now()
	opID, err := w.operation(c, verbFolderRename, id, name)
	if err != nil {
		return Written{}, err
	}
	res, err := w.publish(ctx, request(FolderSubject(), treeScope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (_ statelog.Decision, err error) {
			tree, err := readLiveTree(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			f := tree.live(id)
			switch {
			case f == nil:
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrFolderNotFound, id)
			case f.name == name:
				return statelog.Decision{}, nil
			}
			ns := namespaceOf(f.owner, tree.shares[f.id])
			if other := tree.named(f.parent, ns, key, f.id); other != nil {
				return statelog.Decision{}, fmt.Errorf("%w: %q is already there", ErrFolderNameTaken,
					other.name)
			}
			return w.decision(stamp, c, FolderSubject(), OpRename, treeScope, opID,
				FolderRenamePayload{V: baseRecordVersion, Folder: id, Name: name, NameKey: key,
					Authority: c.Authority}, at)
		}))
	return w.folderWritten(ctx, opID, res, err)
}

// MoveFolder moves one folder, and everything beneath it, under parent (empty
// for the top level). A move to where it already is publishes nothing.
//
// WHO SEES THE TREE IS ITS TOP-LEVEL FOLDER'S, so a move under another
// top-level folder changes who sees the folder and everything beneath it to
// that tree's audience, and a move to the top level keeps the shares it had
// (the record carries its old top-level folder's shares) under its own owner,
// who becomes the new tree's (ADR-0047 names the one case that narrows). Whether
// a move widens is the surface's question to put to the person before it calls
// this.
func (w *Writer) MoveFolder(ctx context.Context, c Call, id, parent string) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	if !validFolderID(id) {
		return Written{}, fmt.Errorf("%w: %q", ErrFolderNotFound, id)
	}
	if parent != "" && !validFolderID(parent) {
		return Written{}, fmt.Errorf("%w: %q", ErrFolderNotFound, parent)
	}
	at := w.now()
	opID, err := w.operation(c, verbFolderMove, id, parent)
	if err != nil {
		return Written{}, err
	}
	res, err := w.publish(ctx, request(FolderSubject(), treeScope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (_ statelog.Decision, err error) {
			tree, err := readLiveTree(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			payload, moves, err := planFolderMove(tree, c, id, parent, at)
			if err != nil || !moves {
				return statelog.Decision{}, err
			}
			return w.decision(stamp, c, FolderSubject(), OpMove, treeScope, opID, payload, at)
		}))
	return w.folderWritten(ctx, opID, res, err)
}

// planFolderMove is the record a folder move publishes, decided in the
// snapshot, and false for a move to where the folder already is.
func planFolderMove(tree *folderTree, c Call, id, parent string, at time.Time) (
	FolderMovePayload, bool, error) {

	f := tree.live(id)
	if f == nil {
		return FolderMovePayload{}, false, fmt.Errorf("%w: %s", ErrFolderNotFound, id)
	}
	if f.parent == parent {
		return FolderMovePayload{}, false, nil
	}
	root := id
	var shares []artifacts.Share
	if parent == "" {
		// TO THE TOP LEVEL, KEEPING ITS AUDIENCE: the shares of the tree it
		// leaves become its own, granted by the person who moved it.
		for _, s := range tree.shares[f.root] {
			shares = append(shares, artifacts.Share{Audience: s.Audience, Role: s.Role,
				GrantedBy: c.Authority.Actor, GrantedAt: at})
		}
	} else {
		p := tree.live(parent)
		switch {
		case p == nil:
			return FolderMovePayload{}, false, fmt.Errorf("%w: %s", ErrFolderNotFound, parent)
		case tree.within(parent, id):
			return FolderMovePayload{}, false, fmt.Errorf("%w: %q holds the folder it would move into",
				ErrFolderCycle, f.name)
		}
		root = p.root
	}
	if depth := tree.depth(parent) + tree.height(id); depth > artifacts.MaxFolderDepth {
		return FolderMovePayload{}, false, fmt.Errorf("%w: %q and what it holds would reach %d deep "+
			"and folders go at most %d deep", ErrFolderDepth, f.name, depth, artifacts.MaxFolderDepth)
	}
	if held := len(tree.kids[parent]); held+1 > artifacts.MaxFolderChildren {
		return FolderMovePayload{}, false, fmt.Errorf("%w: that place holds %d folders and holds "+
			"at most %d", ErrFolderLimit, held, artifacts.MaxFolderChildren)
	}
	if other := tree.named(parent, namespaceOf(f.owner, shares), f.nameKey, id); other != nil {
		return FolderMovePayload{}, false, fmt.Errorf("%w: %q is already there", ErrFolderNameTaken,
			other.name)
	}
	return FolderMovePayload{
		V: baseRecordVersion, Folder: id, Parent: parent, Root: root,
		MaxDepth: artifacts.MaxFolderDepth, MaxChildren: artifacts.MaxFolderChildren,
		Shares: shares, Authority: c.Authority,
	}, true, nil
}

// RemoveFolder removes one folder, moving its child folders and every artifact
// in it (live and trashed) up to its parent in the same apply. NEVER REFUSED
// FOR WHAT IT HOLDS: a "not empty" refusal on a folder that looks empty to its
// remover would say a folder they cannot see is inside it, and moving work up
// takes nothing from anybody. The one refusal the contents can cause is a
// child whose name its new place already holds, which the remover sees both
// of and renames.
func (w *Writer) RemoveFolder(ctx context.Context, c Call, id string) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	if !validFolderID(id) {
		return Written{}, fmt.Errorf("%w: %q", ErrFolderNotFound, id)
	}
	at := w.now()
	opID, err := w.operation(c, verbFolderRemove, id, nil)
	if err != nil {
		return Written{}, err
	}
	// THE TREE AND EVERY ARTIFACT: the apply re-files the artifacts the
	// folder held, which nests every artifact's path.
	scope := ScopeSet{Terms: []ScopeTerm{TreeTerm(), ArtifactsTerm()}}
	res, err := w.publish(ctx, request(FolderSubject(), scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (_ statelog.Decision, err error) {
			tree, err := readLiveTree(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			f := tree.live(id)
			if f == nil {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrFolderNotFound, id)
			}
			if taken := movedUpCollisions(tree, f); len(taken) > 0 {
				return statelog.Decision{}, fmt.Errorf("%w: %s already holds %s; rename one of "+
					"each pair first", ErrFolderNameTaken, placeName(tree, f.parent),
					strings.Join(taken, ", "))
			}
			return w.decision(stamp, c, FolderSubject(), OpRemove, scope, opID,
				FolderRemovePayload{V: baseRecordVersion, Folder: id, Parent: f.parent,
					Authority: c.Authority}, at)
		}))
	return w.folderWritten(ctx, opID, res, err)
}

// movedUpCollisions is the names, quoted, of f's live children that the place
// they move up to already holds: f's parent's other children, or at the top
// level the namespace each child joins there (f's own audience, which a child
// moved up to the top level keeps).
func movedUpCollisions(tree *folderTree, f *folderRow) []string {
	var taken []string
	for _, kid := range tree.kids[f.id] {
		ns := namespaceOf(kid.owner, tree.shares[f.id])
		if other := tree.named(f.parent, ns, kid.nameKey, f.id); other != nil {
			taken = append(taken, strconv.Quote(kid.name))
		}
	}
	return taken
}

// placeName is a place in the tree as a refusal names it.
func placeName(tree *folderTree, id string) string {
	if f := tree.live(id); f != nil {
		return strconv.Quote(f.name)
	}
	return "the top level"
}

// Move files one artifact in folder (empty for the top level), on the
// artifact's own subject, ordered against its publishes and naming the folders
// it reads in its scope. A move to where it already is publishes nothing. A
// MOVE IS NOT AN EDIT: it leaves the artifact's `updated_at` alone, and it
// grants and withdraws nothing (ADR-0047).
func (w *Writer) Move(ctx context.Context, c Call, id artifacts.ID, folder string) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	if id.IsZero() {
		return Written{}, fmt.Errorf("%w: a move names its artifact", ErrInvalid)
	}
	if folder != "" && !validFolderID(folder) {
		return Written{}, fmt.Errorf("%w: %q", ErrFolderNotFound, folder)
	}
	at := w.now()
	opID, err := w.operation(c, verbMove, id.String(), folder)
	if err != nil {
		return Written{}, err
	}
	subject := ArtifactSubject(id)
	var res statelog.Result
	for range sourcedMoveAttempts {
		var from string
		if err = w.db.Read(ctx, func(tx *sql.Tx) error {
			head, found, err := readHeadTx(ctx, tx, id.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if found {
				from = head.Folder
			}
			return err
		}); err != nil {
			return Written{}, fmt.Errorf("artifactsdomain: read where %s is filed: %w", id, err)
		}
		scope := moveScope(id, from, folder)
		res, err = w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
			func(tx *sql.Tx, stamp statelog.Stamp) (_ statelog.Decision, err error) {
				head, err := mustHead(ctx, tx, id.String())
				switch {
				case err != nil:
					return statelog.Decision{}, err
				case head.Folder != from:
					return statelog.Decision{}, errFolderMoved
				case head.Status == StatusTrashed:
					return statelog.Decision{}, fmt.Errorf("%w: %s", ErrTrashed, id)
				case head.Folder == folder:
					return statelog.Decision{}, nil
				}
				if folder != "" {
					live, err := folderLive(ctx, tx, folder)
					switch {
					case err != nil:
						return statelog.Decision{}, err
					case !live:
						return statelog.Decision{}, fmt.Errorf("%w: %s", ErrFolderNotFound, folder)
					}
				}
				return w.decision(stamp, c, subject, OpMove, scope, opID,
					MovePayload{V: baseRecordVersion, From: from, To: folder, Authority: c.Authority}, at)
			}))
		if !errors.Is(err, errFolderMoved) {
			break
		}
	}
	return w.written(ctx, id, opID, res, err)
}

// moveScope is a move's scope: the artifact, and each folder it reads (the one
// it leaves and the one it enters; the top level is no folder).
func moveScope(id artifacts.ID, from, to string) ScopeSet {
	terms := []ScopeTerm{artifactTerm(id.String())}
	if from != "" {
		terms = append(terms, FolderTerm(from))
	}
	if to != "" && to != from {
		terms = append(terms, FolderTerm(to))
	}
	return ScopeSet{Terms: terms}
}

// folderLive reports whether folder id is a live row, read in tx.
func folderLive(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM artifact_folders WHERE id = ?`, id).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("artifactsdomain: read folder %s: %w", id, err)
	}
	return FolderStatus(status) == FolderLive, nil
}

// folderEntry is one folder history row, as a writer reads its verdict.
type folderEntry struct {
	folder  string
	outcome Outcome
	reason  Refusal
	summary FolderHistorySummary
}

// folderVerdict is what the apply of operation opID did to the tree, read from
// this node's own rows, and false where there is no row to read.
func (w *Writer) folderVerdict(ctx context.Context, opID string) (folderEntry, bool, error) {
	var (
		e                        folderEntry
		outcome, reason, summary string
		found                    bool
	)
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT folder_id, outcome, reason, summary FROM artifact_folder_history
			WHERE op_id = ? ORDER BY position LIMIT 1`, opID).Scan(&e.folder, &outcome, &reason, &summary)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return json.Unmarshal([]byte(summary), &e.summary)
	})
	if err != nil {
		return folderEntry{}, false, fmt.Errorf("artifactsdomain: read what operation %s did to "+
			"the folder tree: %w", opID, err)
	}
	e.outcome, e.reason = Outcome(outcome), Refusal(reason)
	return e, found, nil
}

// folderWritten is a folder write's answer: its result, and the refusal its
// own history row records where the apply refused it.
func (w *Writer) folderWritten(ctx context.Context, opID string, res statelog.Result, err error) (
	Written, error) {

	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() {
		return Written{Result: res}, err
	}
	e, found, err := w.folderVerdict(ctx, opID)
	if err != nil || !found || e.outcome != OutcomeRefused {
		return Written{Result: res}, err
	}
	return Written{Result: res}, w.refusal(ctx, "", e.reason, HistorySummary{}, 0)
}

// ---- the reader's folder questions ---------------------------------------- //

// Folders answers every live folder v can see, parents before children and
// siblings A to Z, each with what sits directly in it that v can see, and how
// many artifacts v can see that sit in no folder v sees. The counts come from
// the listing's own predicate in the same transaction ([visibleClause]), so a
// folder's count never holds an artifact v could not open.
func (r *Reader) Folders(ctx context.Context, v Viewer, fresh statelog.Freshness) (FolderTree, error) {
	if v.Record == "" && !v.Admin {
		return FolderTree{}, fmt.Errorf("%w: the folder tree names who is viewing it", ErrInvalid)
	}
	var out FolderTree
	served, err := r.log.Read(ctx, staleFresh(fresh).Query(DomainScope(), true),
		func(tx *sql.Tx) error {
			tree, err := visibleTree(ctx, tx, []Viewer{v})
			if err != nil {
				return err
			}
			counts, err := folderCounts(ctx, tx, []Viewer{v}, StatusLive, nil)
			if err != nil {
				return err
			}
			out = FolderTree{Folders: []FolderItem{}}
			for _, f := range tree.preorder() {
				out.Folders = append(out.Folders, FolderItem{Folder: tree.folder(f),
					Artifacts: counts[f.id], Folders: len(tree.kids[f.id])})
			}
			for folder, n := range counts {
				if tree.live(folder) == nil {
					out.TopLevel += n
				}
			}
			return nil
		})
	if err != nil {
		return FolderTree{}, err
	}
	out.Level, out.Position, out.Complete = served.Level, served.Position, served.Complete
	return out, nil
}

// Folder answers one folder by id, live or removed, with its top-level
// folder's owner and shares, whatever the caller may see: what a decision
// about the folder is made on. The surface decides before it answers anything,
// and a caller the decision refuses is told ErrFolderNotFound.
func (r *Reader) Folder(ctx context.Context, id string, fresh statelog.Freshness) (Folder, statelog.Position, error) {
	if !validFolderID(id) {
		return Folder{}, statelog.Position{}, fmt.Errorf("%w: %q", ErrFolderNotFound, id)
	}
	var out Folder
	served, err := r.log.Read(ctx, staleFresh(fresh).Query(TreeScope(), false),
		func(tx *sql.Tx) error {
			tree, err := readLiveTree(ctx, tx)
			if err != nil {
				return err
			}
			f := tree.rows[id]
			if f == nil {
				// A TOMBSTONE IS NOT IN THE LIVE TREE: read it alone. It
				// was a leaf when it was removed, so its depth is its
				// parent's and one.
				if f, err = readFolderRowTx(ctx, tx, id); err != nil {
					return err
				}
				tree.rows[id] = f
			}
			out = tree.folder(f)
			return nil
		})
	if err != nil {
		return Folder{}, statelog.Position{}, err
	}
	return out, served.Position, nil
}

// readFolderRowTx reads one folder's row, live or removed.
func readFolderRowTx(ctx context.Context, tx *sql.Tx, id string) (*folderRow, error) {
	f, err := scanFolderRow(tx.QueryRowContext(ctx,
		`SELECT `+folderColumns+` FROM artifact_folders f WHERE f.id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %s", ErrFolderNotFound, id)
	case err != nil:
		return nil, fmt.Errorf("artifactsdomain: read folder %s: %w", id, err)
	}
	return f, nil
}

// ResolveFolder resolves a folder reference (an id, or a path of names
// separated by "/", a trailing one allowed) among the live folders EVERY
// viewer in within can see: an agent and the requester it acts for. A
// reference naming none, or naming only folders somebody in within cannot see,
// is ErrFolderNotFound, one answer for both; one naming several is a
// *FolderAmbiguous carrying each. An id is tried first, so a folder named like
// another's id is still found by its path when the id names nothing visible.
func (r *Reader) ResolveFolder(ctx context.Context, ref string, within []Viewer, fresh statelog.Freshness) (Folder, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return Folder{}, fmt.Errorf("%w: name the folder, by its id or its path", ErrInvalid)
	case len(within) == 0:
		return Folder{}, fmt.Errorf("%w: a folder is resolved among what somebody can see", ErrInvalid)
	}
	for _, v := range within {
		if v.Record == "" && !v.Admin {
			return Folder{}, fmt.Errorf("%w: every viewer a folder is resolved for names who "+
				"they are", ErrInvalid)
		}
	}
	var out Folder
	_, err := r.log.Read(ctx, staleFresh(fresh).Query(TreeScope(), true), func(tx *sql.Tx) error {
		tree, err := visibleTree(ctx, tx, within)
		if err != nil {
			return err
		}
		if f := tree.live(ref); f != nil {
			out = tree.folder(f)
			return nil
		}
		found, err := tree.resolvePath(ref)
		switch {
		case err != nil:
			return err
		case len(found) == 0:
			return fmt.Errorf("%w: %q", ErrFolderNotFound, ref)
		case len(found) > 1:
			return ambiguous(tree, ref, found)
		}
		out = tree.folder(found[0])
		return nil
	})
	return out, err
}
