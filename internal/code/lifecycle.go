package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE REPOSITORY LIFECYCLE AS TOOLS: what a person, a unit's lead and a seat
// may make, rename, change, archive, delete and restore through a tool — and a
// live turn's hold on a scratch repository — each ONE request decided by
// [Store.Lifecycle] on any data node, in ONE snapshot of its subject.
//
// # A change, never a whole
//
// What a caller sends for an existing repository is a CHANGE — a field, an
// archive flag, one protection rule — which the decide resolves against the
// rows it reads into the whole settings the record carries. A caller cannot
// form a whole it read in another transaction: two people adding two rules at
// once would each send a list missing the other's, and the later record would
// delete the earlier rule.
//
// # Who may
//
// ONE FUNCTION, [lifecycleRefusal], over the chart of the DECIDING node's
// epoch: a bound person maintains every tool-made repository and creates
// projects for any owner the chart holds; a seat maintains what its grant on
// the repository lets it ([GrantOf], ADR-0058) — a unit's projects if it is
// the unit's effective lead at full_access, its own scratch repositories at
// full_access — and never leaves a project owned by a seat: its own
// repositories are its scratch repositories, which a cap and an idle expiry
// bound, and only a person makes a project a seat's. And a protection binds
// every seat, its unit's lead included: a seat adds a rule or tightens one,
// and never removes or loosens one, which is a person's ([loosenings]). A
// node behind the company's configuration decides none of it
// ([StoreOptions.Current]): every kind reads the chart, a person's included.

// LifecycleKind is what a lifecycle request asks.
type LifecycleKind string

const (
	// LifecycleCreate makes a repository: a project, or a seat's scratch
	// repository named <handle>.<slug> after it.
	LifecycleCreate LifecycleKind = "create"
	// LifecycleRename moves a repository to a new name, the old one
	// redirecting to it for [RedirectWindow].
	LifecycleRename LifecycleKind = "rename"
	// LifecycleSettings changes its description, default branch, visibility
	// or (a project's) owner.
	LifecycleSettings LifecycleKind = "settings"
	// LifecycleArchive makes it read-only, and LifecycleUnarchive takes it
	// back.
	LifecycleArchive   LifecycleKind = "archive"
	LifecycleUnarchive LifecycleKind = "unarchive"
	// LifecycleDelete deletes it, restorable until its purge.
	LifecycleDelete LifecycleKind = "delete"
	// LifecycleRestore brings a deleted one back as it was.
	LifecycleRestore LifecycleKind = "restore"
	// LifecycleProtect sets or removes the protection of one pattern: a
	// seat may add a rule or tighten one, and only a person removes or
	// loosens one ([loosenings]).
	LifecycleProtect LifecycleKind = "protect"
	// LifecycleHold is a live turn's hold on a scratch repository idle
	// [HoldRecordAfter] or more ([Hold]) — no tool's: the one seam a turn's
	// code tools reach a repository through asks it before the request.
	LifecycleHold LifecycleKind = "hold"
)

// LifecycleKinds is the closed set, in this order.
func LifecycleKinds() []LifecycleKind {
	return []LifecycleKind{LifecycleCreate, LifecycleRename, LifecycleSettings, LifecycleArchive,
		LifecycleUnarchive, LifecycleDelete, LifecycleRestore, LifecycleProtect, LifecycleHold}
}

// Valid reports a kind this build knows.
func (k LifecycleKind) Valid() bool { return slices.Contains(LifecycleKinds(), k) }

// LifecycleRefusal is why a lifecycle request was refused — answered, and
// nothing published.
type LifecycleRefusal string

const (
	// LifecycleNotPermitted: the actor may not ask this of this repository
	// ([lifecycleRefusal]).
	LifecycleNotPermitted LifecycleRefusal = "not_permitted"
	// LifecycleConfigManaged: code.repos declares the repository, and a
	// configuration-managed repository takes no tool's write.
	LifecycleConfigManaged LifecycleRefusal = "config_managed"
	// LifecycleNameTaken: the name is another repository's, a deleted one's
	// until its purge, or an old name still redirecting to one.
	LifecycleNameTaken LifecycleRefusal = "name_taken"
	// LifecycleScratchLimit: the seat holds code.scratch.max_per_seat scratch
	// repositories, a deleted one counting until its purge.
	LifecycleScratchLimit LifecycleRefusal = "scratch_limit"
	// LifecycleRepoLimit: the code log holds config.CodeMaxRepos project
	// repositories, declared ones and every status short of purged included.
	LifecycleRepoLimit LifecycleRefusal = "repo_limit"
	// LifecycleRepoDeleted: the repository is deleted (and this is not its
	// restore), being purged, or purged.
	LifecycleRepoDeleted LifecycleRefusal = "repo_deleted"
	// LifecycleStale: the repository moved between the decide and its apply —
	// a rename another rename beat to the repository.
	LifecycleStale LifecycleRefusal = "stale"
)

// LifecycleRefusals is the closed set, in this order.
func LifecycleRefusals() []LifecycleRefusal {
	return []LifecycleRefusal{LifecycleNotPermitted, LifecycleConfigManaged, LifecycleNameTaken,
		LifecycleScratchLimit, LifecycleRepoLimit, LifecycleRepoDeleted, LifecycleStale}
}

// Valid reports a refusal this build knows.
func (r LifecycleRefusal) Valid() bool { return slices.Contains(LifecycleRefusals(), r) }

// LifecycleRequest is one repository-lifecycle request, decided by
// [Store.Lifecycle] on any data node in ONE snapshot of the name's or the
// repository's subject.
type LifecycleRequest struct {
	Kind LifecycleKind `json:"kind"`
	// OpID is the caller's operation: the record's op id, so the domain's
	// ledger answers a repeat, and a create's repository id is derived from
	// it ([LifecycleRequest.RepoIDOf]).
	OpID string `json:"op_id"`
	// Actor is who asks: a seat (no run), or a bound person — an operator
	// credential with the human seat it is bound to. A token bound to nobody
	// has no seat to be judged as, and asks nothing here.
	Actor Principal `json:"actor"`
	// RepoID names the repository, for every kind but create.
	RepoID string `json:"repo_id,omitempty"`
	// Name is a create's name — a scratch one <handle>.<slug> — or a
	// rename's new name.
	Name string `json:"name,omitempty"`
	// Create is a create's whole initial settings: ManagedTools, no
	// configuration activation, and the kind its name says ([KindOf]).
	Create *RepoSettings `json:"create,omitempty"`
	// Change is a settings request's fields, each nil left as it stands.
	Change *RepoChange `json:"change,omitempty"`
	// Protect is a protect request: one rule for one pattern, Rule nil
	// removing it.
	Protect *ProtectChange `json:"protect,omitempty"`
	// Reason is a delete's, recorded on its record.
	Reason string `json:"reason,omitempty"`
	// TurnID is a hold's, and only a hold's: the turn taking it, recorded on
	// its [Hold] so a reader of the log can say whose hold ordered an expiry.
	TurnID string `json:"turn_id,omitempty"`
	// Seen is a hold's, and only a hold's: the position the turn's read of
	// what it holds answered at — the repository's resolution
	// ([RepoRef.Position]). The hold is decided at or past it (the publish's
	// session floor, as the committer decides a batch at its pushes' Seen),
	// so its refusals are never answered from rows older than what the turn
	// has just read: the router may ask a data node behind that read, since
	// a request carries this node's own writes' floor and never a read's,
	// and such a node answers that it is behind ([ErrNotServing]) rather than
	// that a repository the turn read active is deleted.
	Seen statelog.Position `json:"seen,omitzero"`
}

// RepoChange is a settings request: each field nil is left as it stands.
type RepoChange struct {
	Description   *string `json:"description,omitempty"`
	DefaultBranch *string `json:"default_branch,omitempty"` // a branch name (gitref.ValidBranch)
	// Visibility is who besides persons reads it.
	Visibility *Visibility `json:"visibility,omitempty"`
	// Owner is a PROJECT repository's new owner: a scratch repository stays
	// its seat's own.
	Owner *Owner `json:"owner,omitempty"`
	// PathOwners is a PROJECT repository's path owners, changed glob by glob:
	// a scratch repository takes none.
	//
	// A request is no record, so this field takes no versionedFields row:
	// the record a settings change leads to carries the repository's settings
	// whole, and [RepoSettings.PathOwners] names the row those take. It
	// crosses the estate's wire to whichever data node decides it, under the
	// code domain's stated condition — no build carrying the domain has run
	// without it, none of the domain being on main — so every node asked it
	// reads it.
	PathOwners *PathOwnersChange `json:"path_owners,omitempty"`
}

// empty reports a change that changes nothing.
func (c RepoChange) empty() bool {
	return c.Description == nil && c.DefaultBranch == nil && c.Visibility == nil && c.Owner == nil &&
		c.PathOwners == nil
}

// PathOwnersChange is a settings request's change to who owns which of a
// repository's paths: globs ADDED to an owner and globs REMOVED from one —
// never the whole list, which a caller could form only from rows it read in
// another transaction, so two requests adding globs to one owner at once would
// each drop the other's. The decide applies it to the owners its own rows hold
// ([PathOwnersChange.applyTo]): a glob an owner has already is added as
// nothing, one it does not have is removed as nothing, and an owner left with
// no glob is no path owner any more. An owner it adds globs to is one the
// deciding node's chart holds; one it only removes globs from need not be, so
// a unit the chart dropped can still be taken off.
type PathOwnersChange struct {
	// Add gives each owner the globs beside it, beside those it has.
	Add []PathOwner `json:"add,omitempty"`
	// Remove takes the globs beside each owner from it.
	Remove []PathOwner `json:"remove,omitempty"`
}

// invalid is why c is no change, or "": a glob at least, each entry naming a
// unit or a seat — that owner once in each list — with 1 to [MaxPathOwnerPaths]
// globs gitref reads, each once, and no glob both added to and removed from
// one owner, which is two requests rather than one.
func (c PathOwnersChange) invalid() string {
	if len(c.Add) == 0 && len(c.Remove) == 0 {
		return "path_owners: a change adds or removes a glob at least"
	}
	added := make(map[Owner][]string, len(c.Add))
	for _, l := range []struct {
		list    string
		entries []PathOwner
	}{{"add", c.Add}, {"remove", c.Remove}} {
		list, entries := l.list, l.entries
		seen := make(map[Owner]bool, len(entries))
		for i, po := range entries {
			at := fmt.Sprintf("path_owners.%s[%d]", list, i)
			switch {
			case !po.Owner.Kind.Valid():
				return fmt.Sprintf("%s.owner.kind: %q is not one of %v", at, po.Owner.Kind, OwnerKinds())
			case strings.TrimSpace(po.Owner.Ref) == "":
				return at + ".owner.ref: a path owner names a unit or a seat"
			case seen[po.Owner]:
				return fmt.Sprintf("%s.owner: %s is named twice; list each owner's globs once", at,
					ownerWords(po.Owner))
			case len(po.Paths) == 0:
				return at + ".paths: an entry names a glob at least"
			case len(po.Paths) > MaxPathOwnerPaths:
				return fmt.Sprintf("%s.paths: %d globs, past the %d one path owner may name", at,
					len(po.Paths), MaxPathOwnerPaths)
			}
			seen[po.Owner] = true
			for k, glob := range po.Paths {
				if _, err := gitref.ParsePathGlob(glob); err != nil {
					return fmt.Sprintf("%s.paths[%d]: %q is not a path glob: %v", at, k, glob, err)
				}
				if slices.Contains(po.Paths[:k], glob) {
					return fmt.Sprintf("%s.paths[%d]: %q is listed twice", at, k, glob)
				}
			}
			if list == "add" {
				added[po.Owner] = po.Paths
			}
		}
	}
	for i, po := range c.Remove {
		for k, glob := range po.Paths {
			if slices.Contains(added[po.Owner], glob) {
				return fmt.Sprintf("path_owners.remove[%d].paths[%d]: %q is added to %s by the same "+
					"change; ask one or the other", i, k, glob, ownerWords(po.Owner))
			}
		}
	}
	return ""
}

// applyTo is owners — a repository's, as its rows hold them — with c applied,
// in the one form a record carries them in ([CanonicalPathOwners]).
func (c PathOwnersChange) applyTo(owners []PathOwner) []PathOwner {
	globs := make(map[Owner][]string, len(owners)+len(c.Add))
	for _, po := range owners {
		globs[po.Owner] = slices.Clone(po.Paths)
	}
	for _, po := range c.Remove {
		globs[po.Owner] = slices.DeleteFunc(globs[po.Owner], func(g string) bool {
			return slices.Contains(po.Paths, g)
		})
	}
	for _, po := range c.Add {
		for _, g := range po.Paths {
			if !slices.Contains(globs[po.Owner], g) {
				globs[po.Owner] = append(globs[po.Owner], g)
			}
		}
	}
	out := make([]PathOwner, 0, len(globs))
	for owner, paths := range globs {
		if len(paths) > 0 {
			out = append(out, PathOwner{Owner: owner, Paths: paths})
		}
	}
	return CanonicalPathOwners(out)
}

// ProtectChange is one rule for one pattern; a nil Rule removes the pattern's.
type ProtectChange struct {
	// Pattern is refs/heads/<glob> or refs/tags/<glob> (gitref's grammar).
	Pattern string `json:"pattern"`
	// Rule is what the pattern's push is held to; its own Pattern is left
	// empty or names the same pattern.
	Rule *Protection `json:"rule,omitempty"`
}

// LifecycleResult is what became of a lifecycle request.
type LifecycleResult struct {
	// Outcome is applied, pending or unknown — or empty for a refusal.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Repo is the repository as read back after the record applied, or as it
	// stood where the request needed no record or was refused.
	Repo RepoRef `json:"repo"`
	// Refusal names why the request was refused; empty where it was not.
	Refusal LifecycleRefusal `json:"refusal,omitempty"`
	// Detail is the refusal's sentence, or for an outcome unknown what is
	// unknown and that the same request again is how to learn it.
	Detail string `json:"detail,omitempty"`
	// Position is where the request's record landed — this request's own write,
	// or for a repeat the first copy's — and ZERO where it wrote none: a
	// refusal, or an answer the rows already gave, whose row's own Position says
	// where it was read. A caller raises its floor by this alone, never by a
	// read's position, which is somebody else's writes.
	Position statelog.Position `json:"position"`
}

// Err is the answer as an error: nil for one that is not a refusal, and
// otherwise one wrapping the refusal's sentinel — [ErrConfigManaged] for
// config_managed, [ErrNotPermitted] for not_permitted, [ErrRepoNotFound] for
// repo_deleted (the repository is gone to anything the request could ask) and
// [ErrInvalid] for the rest, each a request another node judging the same rows
// refuses alike — so a caller that answers in errors keeps each one's identity
// across the estate's wire.
func (r LifecycleResult) Err() error {
	var sentinel error
	switch r.Refusal {
	case "":
		return nil
	case LifecycleConfigManaged:
		sentinel = ErrConfigManaged
	case LifecycleNotPermitted:
		sentinel = ErrNotPermitted
	case LifecycleRepoDeleted:
		sentinel = ErrRepoNotFound
	default:
		sentinel = ErrInvalid
	}
	return fmt.Errorf("%w: %s: %s", sentinel, r.Refusal, r.Detail)
}

// RepoIDOf is the repository a create of operation opID makes: a canonical
// UUIDv7 DERIVED from the operation ([statelog.DeriveOpID] of its own instant,
// unnamed), so a retry whose ledger row the ledger's sweep has taken names the
// same repository and is answered from it, while two requests are two
// operations and never one id. A turn holds a scratch repository it creates
// on this id before its create is sent. False for an id that carries no
// instant.
func RepoIDOf(opID string) (string, bool) {
	at, ok := statelog.OpMintedAt(opID)
	if !ok {
		return "", false
	}
	return statelog.DeriveOpID(at, "", "code.repo", opID), true
}

// Validate refuses a request no decide could judge ([ErrInvalid], naming the
// field): a kind this build knows, with exactly that kind's fields; an op id a
// caller may bring ([statelog.CheckCallerOpID]); an actor that is a seat or a
// bound person, carrying no run; and each field within its rules — a create's
// settings a tool's, of the kind its name says, a protect request's rule one a
// record may carry, a hold's turn bounded and its read named.
func (r LifecycleRequest) Validate() error {
	if why := r.invalid(); why != "" {
		return fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	return nil
}

// invalid is why r is no request, or "".
func (r LifecycleRequest) invalid() string {
	if !r.Kind.Valid() {
		return fmt.Sprintf("kind: %q is not one of %v", r.Kind, LifecycleKinds())
	}
	if err := statelog.CheckCallerOpID(r.OpID); err != nil {
		return "op_id: " + err.Error()
	}
	if why := r.Actor.invalid(); why != "" {
		return "actor: " + why
	}
	switch {
	case !r.Actor.Run.IsZero():
		return "actor.run: a lifecycle request names who asks, and no run"
	case r.Actor.Kind == PrincipalOperator && r.Actor.Seat == "":
		return "actor: a token bound to nobody holds no seat to be judged as, so it asks no lifecycle " +
			"request — a person asks through the token bound to them"
	}
	// EXACTLY THE KIND'S FIELDS: a field another kind reads, sent with this
	// one, is a caller that meant another request.
	has := map[string]bool{
		"repo_id": r.RepoID != "", "name": r.Name != "", "create": r.Create != nil,
		"change": r.Change != nil, "protect": r.Protect != nil, "reason": r.Reason != "",
		"turn_id": r.TurnID != "", "seen": !r.Seen.IsZero(),
	}
	want := map[LifecycleKind][]string{
		LifecycleCreate:    {"name", "create"},
		LifecycleRename:    {"repo_id", "name"},
		LifecycleSettings:  {"repo_id", "change"},
		LifecycleArchive:   {"repo_id"},
		LifecycleUnarchive: {"repo_id"},
		LifecycleDelete:    {"repo_id", "reason"},
		LifecycleRestore:   {"repo_id"},
		LifecycleProtect:   {"repo_id", "protect"},
		LifecycleHold:      {"repo_id", "turn_id", "seen"},
	}[r.Kind]
	for _, field := range []string{"repo_id", "name", "create", "change", "protect", "reason", "turn_id",
		"seen"} {
		// A delete's reason is optional; every other field its kind names is
		// required.
		switch named := slices.Contains(want, field); {
		case named && !has[field] && !(r.Kind == LifecycleDelete && field == "reason"):
			return fmt.Sprintf("%s: a %s request names it", field, r.Kind)
		case !named && has[field]:
			return fmt.Sprintf("%s: a %s request carries none", field, r.Kind)
		}
	}
	switch r.Kind {
	case LifecycleCreate:
		return r.createInvalid()
	case LifecycleRename:
		if !ValidName(r.Name) {
			return fmt.Sprintf("name: %q is not a repository name", r.Name)
		}
	case LifecycleSettings:
		if r.Change.empty() {
			return "change: a settings request changes at least one field"
		}
		if r.Change.PathOwners != nil {
			if why := r.Change.PathOwners.invalid(); why != "" {
				return "change." + why
			}
		}
	case LifecycleProtect:
		switch {
		case !gitref.ValidPattern(r.Protect.Pattern):
			return fmt.Sprintf("protect.pattern: %q is not a protection pattern", r.Protect.Pattern)
		case r.Protect.Rule != nil && r.Protect.Rule.Pattern != "" && r.Protect.Rule.Pattern != r.Protect.Pattern:
			return fmt.Sprintf("protect.rule.pattern: %q is not the change's pattern %q",
				r.Protect.Rule.Pattern, r.Protect.Pattern)
		}
		// THE RULE IS HELD HERE TO WHAT A RECORD MAY CARRY, before any
		// decide: left to the settings a decide forms, a rule no record may
		// carry was refused naming its index among the repository's stored
		// rules rather than the field its caller sent, and only once the
		// request had been judged — a seat that may not protect the
		// repository was told so of a request no node could decide.
		if r.Protect.Rule != nil {
			rule := *r.Protect.Rule
			rule.Pattern = r.Protect.Pattern
			if why := rule.invalid(); why != "" {
				return "protect.rule." + why
			}
		}
	case LifecycleDelete:
		switch {
		case len(r.Reason) > MaxDescriptionBytes:
			return fmt.Sprintf("reason: %d bytes, past the %d a repository's description may hold",
				len(r.Reason), MaxDescriptionBytes)
		case !utf8.ValidString(r.Reason):
			return "reason: not valid UTF-8"
		}
	case LifecycleHold:
		switch {
		case r.Actor.Kind != PrincipalAgent:
			return "actor: a hold is a turn's, and only a seat runs a turn — a person holds nothing"
		case len(r.TurnID) > MaxProvenanceBytes || !utf8.ValidString(r.TurnID) ||
			strings.ContainsFunc(r.TurnID, unicode.IsControl):
			return fmt.Sprintf("turn_id: at most %d bytes of valid UTF-8 with no control character",
				MaxProvenanceBytes)
		}
	}
	return ""
}

// createInvalid is why a create's name and settings are no tool's create, or
// "".
func (r LifecycleRequest) createInvalid() string {
	s := *r.Create
	switch {
	case !ValidName(r.Name):
		return fmt.Sprintf("name: %q is not a repository name", r.Name)
	case s.ManagedBy != ManagedTools:
		return fmt.Sprintf("create.managed_by: a tool's repository is managed by %s, not %q", ManagedTools,
			s.ManagedBy)
	case s.Kind != KindOf(r.Name):
		// THE KIND FOLLOWS THE NAME: a create whose settings say otherwise
		// would make a scratch repository readers take for a project, or the
		// reverse.
		return fmt.Sprintf("create.kind: %q names a %s repository, and the settings say %q", r.Name,
			KindOf(r.Name), s.Kind)
	}
	if err := s.Validate(); err != nil {
		return "create." + strings.TrimPrefix(strings.TrimPrefix(err.Error(), ErrInvalid.Error()), ": ")
	}
	return ""
}

// LifecyclePolicy is what a create is held to, and how long a scratch
// repository may sit idle — the epoch's code.scratch and the repository bound
// ([ScratchOf]).
type LifecyclePolicy struct {
	// ScratchMaxPerSeat is code.scratch.max_per_seat: 0 lets no seat create
	// one.
	ScratchMaxPerSeat int
	// ScratchIdle is code.scratch.idle_days, read by the code-retention pass.
	ScratchIdle time.Duration
	// MaxRepos is config.CodeMaxRepos: the project repositories, any status
	// short of purged, a tool's create may bring the code log to.
	MaxRepos int
}

// ScratchOf is the lifecycle policy c states, its defaults applied — THE ONE
// CONVERSION, read by the lifecycle store's decide and the retention pass
// alike, so a create and an expiry are held to one reading of the document.
func ScratchOf(c config.Code) LifecyclePolicy {
	idleDays, maxPerSeat := c.Scratch.Resolved()
	return LifecyclePolicy{
		ScratchMaxPerSeat: maxPerSeat,
		ScratchIdle:       time.Duration(idleDays) * 24 * time.Hour,
		MaxRepos:          config.CodeMaxRepos,
	}
}

// lifecycleActor is who a lifecycle record names as its decider: a seat by
// its handle, as an agent; a person by the human seat they hold, as an
// operator, with the credential they asked through beside it — the name
// iam.ActorFor gives them, as this log's eviction gate names its operator.
func lifecycleActor(p Principal) Actor {
	if p.Kind == PrincipalAgent {
		return Actor{ID: p.Seat, Kind: ActorAgent}
	}
	return Actor{ID: p.Seat, Kind: ActorOperator, OperatorID: p.OperatorID}
}

// settingsOf is a repository's settings as a record carries them, from its
// row as a read serves it and its path owners, which no [RepoRef] carries —
// read beside it in the same decide ([pathOwnersIn]), since a record carries
// the settings WHOLE, and one formed without them would remove every path
// owner the repository has.
func settingsOf(r RepoRef, owners []PathOwner) RepoSettings {
	return RepoSettings{
		ManagedBy: r.ManagedBy, Kind: r.Kind, Owner: r.Owner, DefaultBranch: r.DefaultBranch,
		Description: r.Description, Archived: r.Status == RepoStatusArchived, Visibility: r.Visibility,
		Protections: slices.Clone(r.Protections), PathOwners: slices.Clone(owners),
	}
}

// pathOwnersAdded is every path owner req gives globs to: a settings change's
// additions, and a create's whole list — what the deciding node's chart must
// hold, whoever asks.
func pathOwnersAdded(req LifecycleRequest) []PathOwner {
	switch {
	case req.Kind == LifecycleSettings && req.Change.PathOwners != nil:
		return req.Change.PathOwners.Add
	case req.Kind == LifecycleCreate:
		return req.Create.PathOwners
	}
	return nil
}

// lifecycleRefusal is whether actor may ask req of a repository — repo as it
// stands for every kind but a create, and next as the request would leave it
// (a create's new repository, or a settings change's result; the zero RepoRef
// where the kind leaves the repository's grant as it is) — reading the chart
// roster, and the sentence why not. THE ONE JUDGEMENT of who may, every kind's:
//
//   - a BOUND PERSON maintains every tool-made repository, and creates a
//     PROJECT for any owner the chart holds — never a scratch repository,
//     which belongs to the seat whose handle it carries;
//   - a SEAT may what its grant on the repository maintains ([GrantOf]: its
//     tier and its relation on the chart) — for a settings change both as the
//     repository stands and as the change leaves it, so a seat cannot hand a
//     repository to a unit whose lead it is not, which it could not take back;
//     for a project's create as the new repository will stand, so only the
//     owning unit's effective lead at full_access creates one; for a scratch
//     create, its own handle, at full_access, owning it; and for a hold, only
//     what its grant reads, since a seat holds what it may read;
//   - and a seat leaves NO PROJECT OWNED BY A SEAT, its own or another's,
//     whether it creates the project or hands one on by a settings change —
//     though GrantOf gives an owning seat maintain on what it owns — because a
//     seat's own repositories are its scratch repositories, capped and
//     expiring, and a seat-owned project is neither: only a person makes one
//     ([seatProjectRefusal]);
//   - and A PROTECTION BINDS EVERY SEAT, its unit's lead included: whatever
//     its grant, a seat adds a protection rule or tightens one and never
//     removes or loosens one ([loosenings]), judged against the rule repo —
//     the row this decide's snapshot read — holds for the pattern, so two
//     requests cannot loosen a rule between them. With maintain alone
//     deciding, a lead steered by what it read could lift main's protection
//     and then force it: the protections would bind every seat but the one
//     with the most reach. Only a person loosens one.
//
// A NAME OF THE OTHER KIND'S FORM is refused for every actor, a person's
// included: renamed to <handle>.<slug>, a project would read as that seat's
// own scratch repository and squat the name the seat's create would take; and
// a scratch repository keeps its seat's handle, whoever renames it.
//
// AND A PATH OWNER THE CHART DOES NOT HOLD, for every actor, wherever a
// request gives one globs ([pathOwnersAdded]), as an owner the chart does not
// hold is refused a repository: a path owner is whom a review is asked of, and
// a unit or a seat the chart lacks is nobody to ask. Naming one grants nothing
// ([PathOwner]), so a seat that maintains the repository may name any the
// chart holds — the same review it could request by name.
func lifecycleRefusal(req LifecycleRequest, repo, next RepoRef, roster Roster) (bool, string) {
	actor := req.Actor
	if req.Kind == LifecycleRename {
		if why := renameForm(repo, req.Name); why != "" {
			return true, why
		}
	}
	for _, po := range pathOwnersAdded(req) {
		if !rosterHolds(roster, po.Owner) {
			return true, fmt.Sprintf("the path owner %s is no %s of the chart", ownerWords(po.Owner),
				po.Owner.Kind)
		}
	}
	if actor.Kind == PrincipalOperator {
		switch {
		case req.Kind == LifecycleCreate && next.Kind == RepoScratch:
			return true, fmt.Sprintf("%s is a seat's scratch repository's name: a scratch repository "+
				"belongs to the seat whose handle it carries, and only that seat creates it; create a "+
				"project repository", req.Name)
		case req.Kind == LifecycleCreate && !rosterHolds(roster, next.Owner):
			return true, fmt.Sprintf("the owner %s %s is no %s of the chart", next.Owner.Kind,
				next.Owner.Ref, next.Owner.Kind)
		case req.Kind == LifecycleSettings && next.Owner != repo.Owner && !rosterHolds(roster, next.Owner):
			return true, fmt.Sprintf("the owner %s %s is no %s of the chart", next.Owner.Kind,
				next.Owner.Ref, next.Owner.Kind)
		}
		return false, ""
	}
	switch req.Kind {
	case LifecycleHold:
		if g := GrantOf(actor, repo, roster); !g.MayRead() {
			return true, g.ReadRefusal()
		}
		return false, ""
	case LifecycleCreate:
		if next.Kind == RepoScratch {
			return scratchCreateRefusal(actor, req.Name, next, roster)
		}
		if !rosterHolds(roster, next.Owner) {
			return true, fmt.Sprintf("the owner %s %s is no %s of the chart", next.Owner.Kind,
				next.Owner.Ref, next.Owner.Kind)
		}
		if next.Owner.Kind != OwnerUnit {
			return true, seatProjectRefusal(actor, "create the project repository "+req.Name+" owned by "+
				ownerWords(next.Owner))
		}
		if g := GrantOf(actor, next, roster); g.Access < AccessMaintain {
			return true, maintainRefusal(g, "create a project repository owned by "+ownerWords(next.Owner))
		}
		return false, ""
	}
	if g := GrantOf(actor, repo, roster); g.Access < AccessMaintain {
		return true, maintainRefusal(g, string(req.Kind)+" "+repo.Name)
	}
	// A PROTECTION BINDS EVERY SEAT: maintain lets a seat add a rule or
	// tighten one, and loosening one is a person's.
	if req.Kind == LifecycleProtect {
		if opens := loosenings(repo.Protections, *req.Protect); len(opens) > 0 {
			return true, fmt.Sprintf("only a person may remove or loosen the protection on %s, and this "+
				"change %s; a seat, its unit's lead included, may add a rule or tighten one",
				req.Protect.Pattern, strings.Join(opens, " and "))
		}
	}
	// AS THE CHANGE LEAVES IT TOO: a seat cannot hand a repository to an
	// owner, or narrow it to a visibility, under which it would not maintain
	// it — it could not take it back — and hands a project to no seat, itself
	// included, where it would maintain it ([seatProjectRefusal]).
	if req.Kind == LifecycleSettings && (next.Owner != repo.Owner || next.Visibility != repo.Visibility) {
		if next.Owner != repo.Owner && !rosterHolds(roster, next.Owner) {
			return true, fmt.Sprintf("the owner %s %s is no %s of the chart", next.Owner.Kind,
				next.Owner.Ref, next.Owner.Kind)
		}
		if next.Owner != repo.Owner && next.Owner.Kind != OwnerUnit {
			return true, seatProjectRefusal(actor, "give "+repo.Name+" to "+ownerWords(next.Owner))
		}
		if g := GrantOf(actor, next, roster); g.Access < AccessMaintain {
			return true, fmt.Sprintf("seat %s may not leave %s owned by %s and visible to %s: it would not "+
				"maintain it there, so it could not take it back — %s", actor.Seat, repo.Name,
				ownerWords(next.Owner), next.Visibility, maintainRefusal(g, "maintain it"))
		}
	}
	return false, ""
}

// renameForm is why renaming repo to name would make it read as the other
// kind, or "": a scratch repository keeps its seat's handle and its form, and
// a project stays one label.
func renameForm(repo RepoRef, name string) string {
	handle, scratch := gitref.ScratchName(name)
	switch {
	case repo.Kind == RepoScratch && !scratch:
		return fmt.Sprintf("%s is a scratch repository, and a scratch repository keeps its seat's "+
			"<handle>.<slug> form: %s is a project's name", repo.Name, name)
	case repo.Kind == RepoScratch && handle != repo.Owner.Ref:
		return fmt.Sprintf("%s is seat %s's scratch repository, and a scratch repository keeps its seat's "+
			"handle: %s names seat %s", repo.Name, repo.Owner.Ref, name, handle)
	case repo.Kind != RepoScratch && scratch:
		return fmt.Sprintf("%s is a project repository, and a project repository's name is one label: a "+
			"name with a dot, %s, is seat %s's own scratch repository", repo.Name, name, handle)
	}
	return ""
}

// scratchCreateRefusal is whether a seat may create the scratch repository
// next, named name: its own handle, at full_access, owning it.
func scratchCreateRefusal(actor Principal, name string, next RepoRef, roster Roster) (bool, string) {
	handle, _ := gitref.ScratchName(name)
	if handle != actor.Seat {
		if !org.ValidHandle(actor.Seat) || !gitref.ValidProjectName(actor.Seat) {
			return true, fmt.Sprintf("%s is not a host-name label (lowercase letters, digits and hyphens, "+
				"starting and ending with a letter or a digit, at most %d), so it cannot name a scratch "+
				"repository", actor.Seat, gitref.MaxRepoNameBytes)
		}
		return true, fmt.Sprintf("a scratch repository is its seat's own and carries its handle: seat %s "+
			"names one %s.<slug>, and %s is seat %s's", actor.Seat, actor.Seat, name, handle)
	}
	if next.Owner != (Owner{Kind: OwnerSeat, Ref: actor.Seat}) {
		return true, fmt.Sprintf("a scratch repository is owned by the seat whose handle it carries, "+
			"%s, and not by %s", actor.Seat, ownerWords(next.Owner))
	}
	seat, ok := roster.Seat(actor.Seat)
	switch {
	case !ok || seat.Human:
		return true, fmt.Sprintf("%s is no agent seat of the chart", actor.Seat)
	case seat.Tier != TierFullAccess:
		return true, fmt.Sprintf("seat %s's role.code.access is %s, and a scratch repository is "+
			"created by a seat at %s, which may push to it", actor.Seat, seat.Tier, TierFullAccess)
	}
	return false, ""
}

// maintainRefusal is the sentence of a seat whose grant g does not maintain
// what it asked to do.
func maintainRefusal(g Grant, what string) string {
	if g.none != "" {
		return fmt.Sprintf("seat %s may not %s: %s", g.Principal.Seat, what, g.none)
	}
	return fmt.Sprintf("seat %s may not %s: it holds %s (role.code.access %s, %s by the chart) and "+
		"this takes maintain — the owning unit's effective lead at %s, or the seat a scratch "+
		"repository belongs to", g.Principal.Seat, what, g.Access, g.Tier.spelled(), g.Relation,
		TierFullAccess)
}

// seatProjectRefusal is the sentence of a seat that asked to do what, which
// would leave a PROJECT repository owned by a seat — its own or another's.
// A seat makes a project only for a unit whose effective lead it is, and
// hands one on only to such a unit, because a seat's own repositories are its
// SCRATCH repositories: capped by code.scratch.max_per_seat and expired by
// code.scratch.idle_days. A project a seat owns is under neither — it never
// expires, and only the company-wide repo_limit bounds it, the cap every
// person's and every lead's project counts against — so one seat steered by
// what it read could fill that cap with projects of its own. Only a person
// makes a project a seat's.
func seatProjectRefusal(actor Principal, what string) string {
	return fmt.Sprintf("seat %s may not %s: a seat's own repositories are its scratch repositories, "+
		"named %s.<slug>, which code.scratch.max_per_seat caps and code.scratch.idle_days expires — a seat "+
		"makes a project, and hands one on, only for a unit whose effective lead it is, and only a person "+
		"makes a project a seat's", actor.Seat, what, actor.Seat)
}

// ownerWords is an owner as a sentence names it.
func ownerWords(o Owner) string { return fmt.Sprintf("%s %s", o.Kind, o.Ref) }

// rosterHolds reports whether the chart holds owner: a unit it resolves, or a
// seat it has.
func rosterHolds(roster Roster, o Owner) bool {
	switch o.Kind {
	case OwnerUnit:
		return o.Ref != "" && roster.Within(o.Ref, o.Ref)
	case OwnerSeat:
		_, ok := roster.Seat(o.Ref)
		return ok
	}
	return false
}

// lifecycleDecision is what one decide of a lifecycle request concluded,
// reset at the top of every round.
type lifecycleDecision struct {
	// wrote reports a record formed.
	wrote bool
	// refusal and detail are a refusal's; empty where the request was not
	// refused.
	refusal LifecycleRefusal
	detail  string
	// row is the repository as the decide read it — or, for a create, as
	// its record will make it.
	row RepoRef
}

// refuse records a refusal.
func (d *lifecycleDecision) refuse(r LifecycleRefusal, format string, args ...any) {
	d.refusal, d.detail = r, fmt.Sprintf(format, args...)
}

// Lifecycle decides req in ONE snapshot of its subject — the name's for a
// create and a rename, the repository's for every other kind — publishes its
// record under req.OpID, and reads the repository back at the record's
// position. An op id the domain's ledger already holds is answered from the
// ledger (the publisher's rule every write takes) and read back, so a repeated
// request is one write; a create whose ledger row was swept finds the
// repository its operation derives ([RepoIDOf]) already holding the name, and
// is answered from it.
//
// A REFUSAL IS AN ANSWER, never an error: [LifecycleResult.Refusal] and its
// sentence, nothing published. So is an OUTCOME this node could not learn — a
// record appended and its acknowledgement lost — answered
// statelog.OutcomeUnknown, which its caller retries as the same operation
// (the ledger answers what landed) rather than reads as a fault of the node
// that decided it. An ERROR is a request this node could not
// decide — [ErrInvalid] for a request no node could, [ErrNotServing] for one
// this node RAN NOTHING of (it decides no lifecycle request, holds no chart, is
// behind the company's configuration, or the code log refused this node), so
// the estate's router asks the next data node.
func (s *Store) Lifecycle(ctx context.Context, req LifecycleRequest) (LifecycleResult, error) {
	if err := req.Validate(); err != nil {
		return LifecycleResult{}, err
	}
	switch {
	case s.seatRules == nil || s.policy == nil:
		return LifecycleResult{}, fmt.Errorf("%w: this node's code store decides no lifecycle request",
			ErrNotServing)
	case s.current != nil && !s.current():
		// EVERY KIND READS THE CHART, a person's included (a create's owner,
		// who may ask what), so a node confirmed behind the company's
		// configuration decides none of them, asked first or asked last:
		// the revision it failed to apply may be the one that took the
		// actor's grant away.
		return LifecycleResult{}, fmt.Errorf("%w: this node is behind the company's configuration and "+
			"decides no lifecycle request from the chart the fleet replaced", ErrNotServing)
	}
	rules, ok := s.seatRules()
	if !ok || rules.Roster == nil {
		return LifecycleResult{}, fmt.Errorf("%w: this node holds no chart to judge a lifecycle request by",
			ErrNotServing)
	}
	policy := s.policy()
	switch req.Kind {
	case LifecycleCreate:
		return s.lifecycleCreate(ctx, req, rules.Roster, policy)
	case LifecycleRename:
		return s.lifecycleRename(ctx, req, rules.Roster)
	}
	return s.lifecycleRepo(ctx, req, rules.Roster)
}

// lifecycleCreate decides a tool's create, on the NAME's subject.
func (s *Store) lifecycleCreate(ctx context.Context, req LifecycleRequest, roster Roster,
	policy LifecyclePolicy) (LifecycleResult, error) {

	id, _ := RepoIDOf(req.OpID)
	token := NameToken(req.Name)
	subject := NameSubject(req.Name)
	scope := []string{NamePath(token), RepoPath(id)}
	settings := *req.Create
	settings.ConfigEpoch = 0
	at := s.now()
	var d lifecycleDecision
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: req.OpID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			d = lifecycleDecision{}
			cp, err := checkpointIn(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			// THE NEW REPOSITORY, as its record will make it: what the
			// grant is judged on, and the answer's row while it is
			// pending. A refusal answers no row: nothing was made.
			made := RepoRef{ID: id, Name: req.Name, Status: settings.status(), ManagedBy: ManagedTools,
				Kind: settings.Kind, Owner: settings.Owner, Visibility: settings.Visibility,
				DefaultBranch: settings.DefaultBranch, Description: settings.Description,
				Protections: settings.Protections, Position: cp.At}
			holder, redirect, found, err := claimRowIn(ctx, tx, token)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case found && holder == id:
				// THIS OPERATION'S OWN REPOSITORY, its ledger row swept: the
				// create landed, and is answered from the row.
				v, present, err := repoViewIn(ctx, tx, id, cp.At)
				if err != nil {
					return statelog.Decision{}, err
				}
				if present {
					d.row = v.ref
				}
				return statelog.Decision{}, nil
			case found:
				d.refuse(LifecycleNameTaken, "%s", s.takenDetail(ctx, tx, req.Name, holder, redirect))
				return statelog.Decision{}, nil
			}
			if purged, err := purgedIn(ctx, tx, id); err != nil || purged {
				if purged {
					d.refuse(LifecycleRepoDeleted, "%s", RepoGone{ID: id, Name: req.Name, Purged: true,
						ManagedBy: ManagedTools}.Detail())
				}
				return statelog.Decision{}, err
			}
			if refused, why := lifecycleRefusal(req, RepoRef{}, made, roster); refused {
				d.refuse(LifecycleNotPermitted, "%s", why)
				return statelog.Decision{}, nil
			}
			if settings.Kind == RepoScratch {
				if policy.ScratchMaxPerSeat == 0 {
					d.refuse(LifecycleScratchLimit, "this company allows no scratch repositories: "+
						"code.scratch.max_per_seat is 0")
					return statelog.Decision{}, nil
				}
				var held int
				if err := tx.QueryRowContext(ctx, scratchCountSQL, settings.Owner.Ref).Scan(&held); err != nil {
					return statelog.Decision{}, fmt.Errorf("code: count seat %s's scratch repositories: %w",
						settings.Owner.Ref, err)
				}
				if held >= policy.ScratchMaxPerSeat {
					d.refuse(LifecycleScratchLimit, "seat %s holds %d scratch repositories, the most "+
						"code.scratch.max_per_seat allows (%d) — a deleted one counts until its purge "+
						"(code.deleted_retention_days) frees its room; delete one, or reuse one",
						settings.Owner.Ref, held, policy.ScratchMaxPerSeat)
					return statelog.Decision{}, nil
				}
			} else {
				var held int
				switch err := tx.QueryRowContext(ctx, kindCountSQL, string(RepoProject)).Scan(&held); {
				case errors.Is(err, sql.ErrNoRows):
					// No project counted in yet.
				case err != nil:
					return statelog.Decision{}, fmt.Errorf("code: count the project repositories: %w", err)
				}
				if held >= policy.MaxRepos {
					d.refuse(LifecycleRepoLimit, "the code host holds %d project repositories, the most "+
						"it may (config.CodeMaxRepos, %d) — declared ones and deleted ones until their "+
						"purge included; delete one first", held, policy.MaxRepos)
					return statelog.Decision{}, nil
				}
			}
			d.wrote, d.row = true, made
			return recordFor(stamp, subject, OpCreate, scope, req.OpID, lifecycleActor(req.Actor),
				CreateRepo{RepoID: id, Name: req.Name, Settings: settings}, at)
		},
	})
	return s.lifecycleAnswer(ctx, req, id, result, err, d, func(back RepoRef) (LifecycleRefusal, string) {
		if back.Name != req.Name {
			return LifecycleStale, fmt.Sprintf("the create's record applied and %s does not hold the name "+
				"%s here; read the repository again", id, req.Name)
		}
		return "", ""
	})
}

// takenDetail is why a name is taken: the repository holding it, or the one
// it still redirects to and until when.
func (s *Store) takenDetail(ctx context.Context, tx *sql.Tx, name, holder string, redirect sql.NullInt64) string {
	if redirect.Valid {
		until := store.DecodeTime(redirect.Int64).UTC().Format(time.RFC3339)
		current, found, err := repoNameIn(ctx, tx, holder)
		if err != nil || !found {
			// THE REPOSITORY IS GONE and its old name is still reserved: a
			// purge releases the name a repository holds, and an old one
			// ends where every redirect does, at its window's end.
			return fmt.Sprintf("%s is the old name of a renamed repository that has since been purged, "+
				"and it stays reserved until %s (code.RedirectWindow after the rename); choose another "+
				"name", name, until)
		}
		return fmt.Sprintf("%s was renamed to %s, and the old name redirects to it until %s "+
			"(code.RedirectWindow after the rename); choose another name", name, current, until)
	}
	return fmt.Sprintf("%s is another repository's name (a deleted one's included, until its purge); "+
		"choose another name", name)
}

// lifecycleRename decides a rename, on the NEW name's subject. The scope names
// the old name too, which is read before the decide — the request's scope is
// stated before its decide runs — and held to the row inside it.
func (s *Store) lifecycleRename(ctx context.Context, req LifecycleRequest, roster Roster) (LifecycleResult,
	error) {

	from, found, err := s.repoName(ctx, req.RepoID)
	switch {
	case err != nil:
		return LifecycleResult{}, err
	case !found:
		return LifecycleResult{}, fmt.Errorf("%w: %s", ErrRepoNotFound, req.RepoID)
	}
	token := NameToken(req.Name)
	subject := NameSubject(req.Name)
	scope := []string{NamePath(token), RepoPath(req.RepoID)}
	if from != req.Name {
		scope = append(scope, NamePath(NameToken(from)))
	}
	at := s.now()
	var d lifecycleDecision
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: req.OpID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			d = lifecycleDecision{}
			cp, err := checkpointIn(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			v, present, err := repoViewIn(ctx, tx, req.RepoID, cp.At)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !present:
				return statelog.Decision{}, s.goneOrMissing(ctx, tx, req.RepoID, &d)
			}
			d.row = v.ref
			if refusal := toolRepoRefusal(v.ref, &d); refusal {
				return statelog.Decision{}, nil
			}
			if refused, why := lifecycleRefusal(req, v.ref, RepoRef{}, roster); refused {
				d.refuse(LifecycleNotPermitted, "%s", why)
				return statelog.Decision{}, nil
			}
			if v.ref.Name == req.Name {
				// ALREADY ITS NAME: nothing to write.
				return statelog.Decision{}, nil
			}
			if v.ref.Name != from {
				// RENAMED SINCE THE SCOPE WAS STATED: this decide's scope
				// names the old name the request did not see.
				d.refuse(LifecycleStale, "%s was renamed to %s while this request was decided; read it "+
					"again", from, v.ref.Name)
				return statelog.Decision{}, nil
			}
			holder, redirect, taken, err := claimRowIn(ctx, tx, token)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case taken && !(holder == req.RepoID && redirect.Valid):
				// ITS OWN REDIRECTING OLD NAME may be taken back; any other
				// holder keeps the name.
				d.refuse(LifecycleNameTaken, "%s", s.takenDetail(ctx, tx, req.Name, holder, redirect))
				return statelog.Decision{}, nil
			}
			d.wrote = true
			return recordFor(stamp, subject, OpRename, scope, req.OpID, lifecycleActor(req.Actor),
				Rename{RepoID: req.RepoID, From: from, To: req.Name}, at)
		},
	})
	return s.lifecycleAnswer(ctx, req, req.RepoID, result, err, d, func(back RepoRef) (LifecycleRefusal, string) {
		if back.Name != req.Name {
			// TWO RENAMES OF ONE REPOSITORY arbitrate on two name subjects,
			// so both land: the second applies as nothing (its apply finds
			// the row no longer under From), and is answered so.
			return LifecycleStale, fmt.Sprintf("another rename of the repository landed first: it is %s, "+
				"and this rename to %s applied as nothing", back.Name, req.Name)
		}
		return "", ""
	})
}

// lifecycleRepo decides every kind on the repository's own subject: a
// settings change, an archive or unarchive, a delete, a restore, a protection
// and a hold.
func (s *Store) lifecycleRepo(ctx context.Context, req LifecycleRequest, roster Roster) (LifecycleResult,
	error) {

	subject := RepoSubject(req.RepoID)
	scope := []string{RepoPath(req.RepoID)}
	at := s.now()
	var d lifecycleDecision
	pub := statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: req.OpID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			d = lifecycleDecision{}
			cp, err := checkpointIn(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			v, present, err := repoViewIn(ctx, tx, req.RepoID, cp.At)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !present:
				return statelog.Decision{}, s.goneOrMissing(ctx, tx, req.RepoID, &d)
			}
			d.row = v.ref
			// ITS PATH OWNERS, which no RepoRef carries, beside its row: every
			// kind but a hold writes the settings whole, or decides from them.
			var owners []PathOwner
			if req.Kind != LifecycleHold {
				if owners, err = pathOwnersIn(ctx, tx, req.RepoID); err != nil {
					return statelog.Decision{}, err
				}
			}
			payload, op, err := s.decideOnRepo(req, v.ref, owners, roster, &d)
			if err != nil || payload == nil {
				return statelog.Decision{}, err
			}
			d.wrote = true
			return recordFor(stamp, subject, op, scope, req.OpID, lifecycleActor(req.Actor), payload, at)
		},
	}
	if req.Kind == LifecycleHold {
		// DECIDED AT THE READ IT ORDERS: a node behind the turn's own read of
		// the repository waits for it, and answers that it is behind rather
		// than refuse from rows older than what the turn has just read.
		pub.Session = req.Seen
	}
	result, err := s.publisher.Publish(ctx, pub)
	return s.lifecycleAnswer(ctx, req, req.RepoID, result, err, d, nil)
}

// decideOnRepo is the decide of a kind on the repository's own subject, from
// its row v and its path owners: the record's payload and op, or nil where the
// request is refused (d says why) or writes nothing. An error is the request's
// own fault.
func (s *Store) decideOnRepo(req LifecycleRequest, v RepoRef, owners []PathOwner, roster Roster,
	d *lifecycleDecision) (any, OpKind, error) {

	if req.Kind == LifecycleHold {
		return decideHold(req, v, roster, d)
	}
	// A DELETED REPOSITORY IS A DELETE'S AND A RESTORE'S TO ANSWER: the one
	// answers it from the row, the other brings it back. Every other kind is
	// refused on it as on one being purged.
	deletedAnswers := v.Status == RepoStatusDeleted && (req.Kind == LifecycleDelete || req.Kind == LifecycleRestore)
	if !deletedAnswers && toolRepoRefusal(v, d) {
		return nil, "", nil
	}
	if deletedAnswers && v.ManagedBy != ManagedTools {
		toolRepoRefusal(v, d)
		return nil, "", nil
	}
	next := settingsOf(v, owners)
	nextRef := v
	switch req.Kind {
	case LifecycleSettings:
		c := req.Change
		if c.Description != nil {
			next.Description = *c.Description
		}
		if c.DefaultBranch != nil {
			next.DefaultBranch = *c.DefaultBranch
		}
		if c.Visibility != nil {
			next.Visibility = *c.Visibility
		}
		if c.Owner != nil {
			if v.Kind == RepoScratch && *c.Owner != v.Owner {
				d.refuse(LifecycleNotPermitted, "%s is a scratch repository, which stays the seat's own "+
					"whose handle it carries", v.Name)
				return nil, "", nil
			}
			next.Owner = *c.Owner
		}
		if c.PathOwners != nil {
			if v.Kind == RepoScratch {
				d.refuse(LifecycleNotPermitted, "%s is a scratch repository, which takes no path owners: it "+
					"is its seat's own", v.Name)
				return nil, "", nil
			}
			// RESOLVED AGAINST THE ROWS THIS DECIDE READ, so a change to one
			// owner's globs keeps every other's — and another request's
			// globs added to the same owner meanwhile.
			next.PathOwners = c.PathOwners.applyTo(next.PathOwners)
		}
		nextRef.Owner, nextRef.Visibility = next.Owner, next.Visibility
	case LifecycleArchive:
		next.Archived = true
	case LifecycleUnarchive:
		next.Archived = false
	case LifecycleProtect:
		if v.Kind == RepoScratch {
			d.refuse(LifecycleNotPermitted, "%s is a scratch repository, which takes no protections: it is "+
				"its seat's own", v.Name)
			return nil, "", nil
		}
		next.Protections = protected(next.Protections, *req.Protect)
	}
	if refused, why := lifecycleRefusal(req, v, nextRef, roster); refused {
		d.refuse(LifecycleNotPermitted, "%s", why)
		return nil, "", nil
	}
	switch req.Kind {
	case LifecycleDelete:
		if v.Status == RepoStatusDeleted {
			// ALREADY DELETED: answered from the row.
			return nil, "", nil
		}
		return LifecycleRecord{RepoID: v.ID, Reason: req.Reason}, OpDelete, nil
	case LifecycleRestore:
		if v.Status != RepoStatusDeleted {
			// NOT DELETED: there is nothing to restore, and the row is the
			// answer.
			return nil, "", nil
		}
		restored := settingsOf(v, owners)
		return LifecycleRecord{RepoID: v.ID, Settings: &restored}, OpRestore, nil
	}
	if err := next.Validate(); err != nil {
		return nil, "", err
	}
	if next.equal(settingsOf(v, owners)) {
		// AN IDEMPOTENT NO-OP: the row already says it.
		return nil, "", nil
	}
	return SettingsRecord{RepoID: v.ID, Settings: next}, OpSettings, nil
}

// decideHold is a hold's decide: a record on an active scratch repository the
// seat may read, nothing on a project or an archived one (neither expires
// while so, and the scratch expiry refuses on any record past its page, an
// unarchive's included, so there is nothing to order), and repo_deleted for one
// deleted, being purged or purged — the turn's request is then refused before
// it reads anything.
func decideHold(req LifecycleRequest, v RepoRef, roster Roster, d *lifecycleDecision) (any, OpKind, error) {
	switch v.Status {
	case RepoStatusDeleted, RepoStatusPurging:
		d.refuse(LifecycleRepoDeleted, "%s", RepoGone{ID: v.ID, Name: v.Name, Status: v.Status,
			DeletedAt: v.DeletedAt, ManagedBy: v.ManagedBy}.Detail())
		return nil, "", nil
	}
	if refused, why := lifecycleRefusal(req, v, RepoRef{}, roster); refused {
		d.refuse(LifecycleNotPermitted, "%s", why)
		return nil, "", nil
	}
	if v.Kind != RepoScratch || v.Status != RepoStatusActive {
		return nil, "", nil
	}
	return Hold{RepoID: v.ID, TurnID: req.TurnID}, OpHold, nil
}

// toolRepoRefusal records, in d, the refusal every kind on an existing
// repository shares — config_managed for one code.repos declares, and
// repo_deleted for one deleted, being purged or purged — and reports whether it
// recorded one.
func toolRepoRefusal(v RepoRef, d *lifecycleDecision) bool {
	switch {
	case v.ManagedBy != ManagedTools:
		d.refuse(LifecycleConfigManaged, "%s is declared in code.repos; change it there (code.repos[%s]) — a "+
			"configuration-managed repository takes no tool's lifecycle write", v.Name, v.Name)
		return true
	case v.Status == RepoStatusDeleted || v.Status == RepoStatusPurging:
		d.refuse(LifecycleRepoDeleted, "%s", RepoGone{ID: v.ID, Name: v.Name, Status: v.Status,
			DeletedAt: v.DeletedAt, ManagedBy: v.ManagedBy}.Detail())
		return true
	}
	return false
}

// goneOrMissing records repo_deleted in d for a repository with no row whose
// deletion marker this node holds, and answers ErrRepoNotFound for one
// nobody has.
func (s *Store) goneOrMissing(ctx context.Context, tx *sql.Tx, id string, d *lifecycleDecision) error {
	name, purged, err := deletionIn(ctx, tx, id)
	switch {
	case err != nil:
		return err
	case !purged:
		return fmt.Errorf("%w: %s", ErrRepoNotFound, id)
	}
	d.refuse(LifecycleRepoDeleted, "%s", RepoGone{ID: id, Name: name, Purged: true}.Detail())
	return nil
}

// protected is rules with change applied: the pattern's rule replaced or
// removed, sorted by pattern and unique — the canonical order every record
// carries.
func protected(rules []Protection, change ProtectChange) []Protection {
	out := slices.DeleteFunc(slices.Clone(rules), func(p Protection) bool { return p.Pattern == change.Pattern })
	if change.Rule != nil {
		rule := *change.Rule
		rule.Pattern = change.Pattern
		out = append(out, rule)
	}
	slices.SortFunc(out, func(a, b Protection) int { return strings.Compare(a.Pattern, b.Pattern) })
	return out
}

// loosenings is every way change LOOSENS the protection rules hold for its
// pattern — lets through a push, or a merge, that rule refuses — each as the
// phrase a refusal names it by; nil where the change loosens nothing.
//
// A PATTERN NO RULE HOLDS loosens nothing, whatever the change: every rule
// that matches a ref holds ([ProtectionRefusal]), so a rule added only
// refuses more, and removing one nobody holds changes nothing. A rule REMOVED
// loosens, since every rule refuses at least a seat's direct push. A rule
// CHANGED loosens by each CONTROL — each field but its pattern — that the
// change opens, judged control by control, so one that tightens a control and
// opens another loosens by the one it opens:
//
//   - direct_push loosens unless it stays as it was or becomes none, which
//     admits nobody. FAIL CLOSED: a value this build does not know, on either
//     side, is judged a loosening, since nothing here says whom it admits. A
//     request never carries one this far ([LifecycleRequest.Validate] refuses
//     it, whoever asks), so what this guards is a rule the rows hold, which a
//     seat may tighten to none and never move elsewhere.
//   - allow_force and allow_delete loosen when they are turned on.
//   - the merge gate ([MergeGate]) is ONE control here, judged whole: added
//     where the rule had none, it only holds a merge to more, and removed, it
//     loosens. ALTERED, it loosens too — FAIL CLOSED, as an unknown
//     direct_push does: whether an altered gate holds a merge to more or to
//     less is a judgement of its fields one by one, and none of them is
//     weighed here, so a seat adds a gate and a person alters or removes one.
//     Compared by value ([MergeGate.Equal], which reads every field), never by
//     pointer: two reads of one gate are two pointers, and a request carrying
//     the rule's gate as it stands alters nothing of it.
//
// A field a later milestone adds to [Protection] is a control too, judged
// here the same way: TestEveryProtectionControlIsJudged holds this function
// to the struct's own fields and to every pair of direct_push values, so a
// control or a value nobody judged cannot arrive unseen.
func loosenings(rules []Protection, change ProtectChange) []string {
	i := slices.IndexFunc(rules, func(p Protection) bool { return p.Pattern == change.Pattern })
	switch {
	case i < 0:
		return nil
	case change.Rule == nil:
		return []string{"removes its rule"}
	}
	from, to := rules[i], *change.Rule
	var opens []string
	if to.DirectPush != from.DirectPush && to.DirectPush != DirectNone {
		opens = append(opens, fmt.Sprintf("makes direct_push %s where it was %s", to.DirectPush,
			from.DirectPush))
	}
	if to.AllowForce && !from.AllowForce {
		opens = append(opens, "turns allow_force on")
	}
	if to.AllowDelete && !from.AllowDelete {
		opens = append(opens, "turns allow_delete on")
	}
	switch {
	case from.Gate == nil:
	case to.Gate == nil:
		opens = append(opens, "removes its merge gate")
	case !from.Gate.Equal(*to.Gate):
		opens = append(opens, "alters its merge gate (a seat may add a gate, never alter one)")
	}
	return opens
}

// lifecycleAnswer is a lifecycle request's answer from its publish: the
// error, a refusal, a no-op answered from the row, or the record's outcome —
// read back where it applied here, and held by check, which answers a refusal
// for a record that applied as nothing.
func (s *Store) lifecycleAnswer(ctx context.Context, req LifecycleRequest, id string, result statelog.Result,
	err error, d lifecycleDecision, check func(RepoRef) (LifecycleRefusal, string)) (LifecycleResult, error) {

	if err != nil {
		return s.lifecycleRefused(err, req, id)
	}
	switch {
	case result.Collapsed:
		// THIS OPERATION ALREADY APPLIED: answered from the row as it
		// stands now, at the first copy's position.
		back, err := s.readRepo(ctx, id)
		if err != nil {
			return LifecycleResult{}, err
		}
		return LifecycleResult{Outcome: statelog.OutcomeApplied, Repo: back, Position: result.Position}, nil
	case d.refusal != "":
		return LifecycleResult{Repo: d.row, Refusal: d.refusal, Detail: d.detail}, nil
	case !d.wrote:
		return LifecycleResult{Outcome: statelog.OutcomeApplied, Repo: d.row}, nil
	case result.Outcome == statelog.OutcomeUnknown:
		// AN OUTCOME, NEVER AN ERROR: the record may have landed, and the
		// same request again — the same operation — is what finds out. As an
		// error it crossed the estate's wire as words with no sentinel, and
		// every caller asking a data node read it as that node's fault,
		// telling whoever asked that trying again could not help.
		return LifecycleResult{Outcome: statelog.OutcomeUnknown, Repo: d.row, Detail: fmt.Sprintf(
			"whether the %s of %s landed is unknown (operation %s); make the same request again",
			req.Kind, cmpName(req, d.row), req.OpID)}, nil
	case result.Outcome == statelog.OutcomePending:
		// DURABLE, NOT YET APPLIED HERE: the row is the one the decide read.
		return LifecycleResult{Outcome: statelog.OutcomePending, Repo: d.row, Position: result.Position}, nil
	}
	back, err := s.readRepo(ctx, id)
	if err != nil {
		return LifecycleResult{}, err
	}
	if check != nil {
		if refusal, detail := check(back); refusal != "" {
			return LifecycleResult{Repo: back, Refusal: refusal, Detail: detail, Position: result.Position}, nil
		}
	}
	return LifecycleResult{Outcome: statelog.OutcomeApplied, Repo: back, Position: result.Position}, nil
}

// cmpName is how an answer names the repository a request is about.
func cmpName(req LifecycleRequest, row RepoRef) string {
	switch {
	case row.Name != "":
		return row.Name
	case req.Name != "":
		return req.Name
	}
	return req.RepoID
}

// lifecycleRefused is a lifecycle request's answer from a publish that
// failed: [ErrNotServing] where the code log refused THIS NODE before anything
// was appended (the router asks the next data node), repo_deleted where the
// repository's deletion marker refused it, and the error otherwise.
func (s *Store) lifecycleRefused(err error, req LifecycleRequest, id string) (LifecycleResult, error) {
	var refusal *statelog.Unavailable
	switch {
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		return LifecycleResult{Refusal: LifecycleRepoDeleted, Detail: RepoGone{ID: id, Purged: true}.Detail(),
			Repo: RepoRef{ID: id}}, nil
	case errors.As(err, &refusal) && nodeRefusals[refusal.Reason] && !refusal.Landed():
		return LifecycleResult{}, fmt.Errorf("%w: %w", ErrNotServing, refusal)
	case errors.Is(err, statelog.ErrConflict):
		return LifecycleResult{}, fmt.Errorf("code: the %s of %s kept changing under every round the "+
			"request may take; make the same request again: %w", req.Kind, cmpOr(req.Name, id), err)
	}
	return LifecycleResult{}, fmt.Errorf("code: the %s of %s: %w", req.Kind, cmpOr(req.Name, id), err)
}

// cmpOr is the first of two names that is not empty.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// readRepo is a repository as this node's rows hold it now, at the checkpoint
// they were read at.
func (s *Store) readRepo(ctx context.Context, id string) (RepoRef, error) {
	var out RepoRef
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		cp, err := checkpointIn(ctx, tx)
		if err != nil {
			return err
		}
		v, present, err := repoViewIn(ctx, tx, id, cp.At)
		switch {
		case err != nil:
			return err
		case !present:
			return fmt.Errorf("%w: %s", ErrRepoNotFound, id)
		}
		out = v.ref
		return nil
	})
	if err != nil {
		return RepoRef{}, fmt.Errorf("code: read back repository %s: %w", id, err)
	}
	return out, nil
}

// claimRowIn is a name's claim in tx: the repository it names, and when its
// redirect ends where it is an old name redirecting.
func claimRowIn(ctx context.Context, tx *sql.Tx, token string) (holder string, redirect sql.NullInt64,
	found bool, err error) {

	err = tx.QueryRowContext(ctx, claimRedirectSQL, token).Scan(&holder, &redirect)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", sql.NullInt64{}, false, nil
	case err != nil:
		return "", sql.NullInt64{}, false, fmt.Errorf("code: read the claim %s: %w", token, err)
	}
	return holder, redirect, true, nil
}

// purgedIn reports a repository id whose deletion marker tx holds.
func purgedIn(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var marked int
	if err := tx.QueryRowContext(ctx, purgedSQL, id).Scan(&marked); err != nil {
		return false, fmt.Errorf("code: read %s's deletion marker: %w", id, err)
	}
	return marked > 0, nil
}

// repoNameIn is a repository's name in tx; false where it has no row.
func repoNameIn(ctx context.Context, tx *sql.Tx, id string) (string, bool, error) {
	var name string
	err := tx.QueryRowContext(ctx, repoNameSQL, id).Scan(&name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return name, true, nil
}

// ScratchExpiry is one retention pass's view of the scratch expiry, FIXED
// BEFORE THE PASS READS ANY HOLD (Retention.Pass): what makes a hold the pass
// could not see one its decide refuses.
type ScratchExpiry struct {
	// Cutoff is the pass's now less code.scratch.idle_days, taken once per
	// pass.
	Cutoff time.Time
	// Through is the position the candidate page was read at — the code
	// log's checkpoint, read inside the listing's transaction — and KeyedTo
	// the stream identity that checkpoint is keyed to, so a position on a
	// log recreated since names nothing and the repository is kept.
	Through statelog.Position
	KeyedTo time.Time
}

// ExpireScratch deletes a scratch repository that is, in ONE snapshot, still
// active, last active at or before x.Cutoff, and has had NO record on its
// subject past x.Through — its row's version (the composed position of its
// subject's last record) at or before x.Through, on the same stream identity
// — and reports whether it did; false where any of it fails, a log recreated
// since the page included (an unknown, so the repository is kept). The
// code-retention pass's; the record is an ordinary delete, restorable for
// code.deleted_retention_days, its actor the system.
//
// THE ORDER IS WHAT MAKES A HOLD SAFE: the pass fixes both fields before it
// reads the live turns' holds, so a hold it could not see either was taken on
// a repository active within [HoldRecordAfter] — later than this cutoff, which
// the activity term refuses — or was recorded on this subject after the page
// was read, which the version term refuses. Any other record past the page
// refuses it too (a push, a rename, a settings change); the next pass lists it
// again if it is still idle.
func (s *Store) ExpireScratch(ctx context.Context, repoID string, x ScratchExpiry) (bool, error) {
	if x.Cutoff.IsZero() || x.Through.IsZero() {
		return false, fmt.Errorf("%w: a scratch expiry names its pass's cutoff and the position its page "+
			"was read at", ErrInvalid)
	}
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	opID := statelog.NewOpID(time.Now(), "code.expire_scratch")
	at := s.now()
	var wrote bool
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			wrote = false
			cp, err := checkpointIn(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !cp.KeyedTo.Equal(x.KeyedTo) || cp.At.Stream != x.Through.Stream {
				return statelog.Decision{}, nil
			}
			row, present, err := repoIn(ctx, tx, repoID)
			if err != nil || !present {
				return statelog.Decision{}, err
			}
			if row.Kind != RepoScratch || row.Status != RepoStatusActive ||
				store.DecodeTime(row.LastActivityAt).After(x.Cutoff) || row.Version > x.Through.Packed() {
				return statelog.Decision{}, nil
			}
			wrote = true
			idle := at.Sub(store.DecodeTime(row.LastActivityAt))
			return recordFor(stamp, subject, OpDelete, scope, opID, Actor{ID: s.nodeID, Kind: ActorSystem},
				LifecycleRecord{RepoID: repoID, Reason: fmt.Sprintf("idle for %d days (code.scratch.idle_days)",
					int(idle/(24*time.Hour)))}, at)
		},
	})
	switch {
	case err != nil:
		return false, fmt.Errorf("code: expire scratch repository %s: %w", repoID, err)
	case !wrote:
		return false, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return false, unknownOutcome(ctx, "code: whether scratch repository %s was expired is unknown "+
			"(operation %s); the next pass decides it again", repoID, opID)
	}
	return true, nil
}

// ReleaseName frees a renamed repository's old name when, in ONE snapshot, its
// row still points at repoID with its redirect ended by the decide's clock — a
// release_name record on the name's subject — and reports whether it did;
// false where the row is gone, re-pointed (renamed back), or not yet due. The
// code-retention pass's. The apply removes the row only where the record's own
// broker instant is past the redirect too, so one decided early releases
// nothing, alike on every node.
func (s *Store) ReleaseName(ctx context.Context, name, repoID string) (bool, error) {
	token := NameToken(name)
	subject := NameSubject(name)
	scope := []string{NamePath(token)}
	opID := statelog.NewOpID(time.Now(), "code.release_name")
	at := s.now()
	var wrote bool
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			wrote = false
			holder, redirect, found, err := claimRowIn(ctx, tx, token)
			if err != nil || !found || holder != repoID || !redirect.Valid ||
				store.DecodeTime(redirect.Int64).After(s.now()) {
				return statelog.Decision{}, err
			}
			wrote = true
			return recordFor(stamp, subject, OpReleaseName, scope, opID, Actor{ID: s.nodeID, Kind: ActorSystem},
				ReleaseName{Name: name, RepoID: repoID}, at)
		},
	})
	switch {
	case err != nil:
		return false, fmt.Errorf("code: release the name %q: %w", name, err)
	case !wrote:
		return false, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return false, unknownOutcome(ctx, "code: whether the name %q was released is unknown (operation "+
			"%s); the next pass decides it again", name, opID)
	}
	return true, nil
}

// The lifecycle decides' statements, each registered in plan_test.go.
const (
	// claimRedirectSQL is a name's claim and its redirect's end, by its key.
	claimRedirectSQL = `SELECT repo_id, redirect_until FROM code_names WHERE token = ?`
	// scratchCountSQL is a seat's scratch repositories, EVERY status — a
	// deleted one counts until its purge frees its room — over
	// code_repos_scratch_idx, its predicate repeated so the planner may use
	// it: a seek on the owner, at most code.scratch.max_per_seat entries.
	scratchCountSQL = `SELECT COUNT(*) FROM code_repos WHERE kind = 'scratch' AND owner = ?`
	// kindCountSQL is how many repositories of a kind the log holds, every
	// status short of purged, declared ones included: code_repo_kinds' one
	// row, by its key — the count the applier keeps, never a count over the
	// repository rows, which would read every project inside the snapshot.
	kindCountSQL = `SELECT repos FROM code_repo_kinds WHERE kind = ?`
)
