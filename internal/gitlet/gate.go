package gitlet

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/words"
)

// A protection rule's merge gate, recorded on its rule: why it is one value
// and how two compare is the package doc's "A protection rule's merge gate
// rides it whole".

// MergeBy is who may merge into a gated ref: the repository standing of a
// merge's principal ([GrantOf]), or a person.
type MergeBy string

const (
	// MergeByWriters lets anybody who may write to the repository merge.
	MergeByWriters MergeBy = "writers"
	// MergeByMaintainers lets only those who maintain the repository merge.
	MergeByMaintainers MergeBy = "maintainers"
	// MergeByPersons lets only a person merge, never a seat, a unit's lead
	// included.
	MergeByPersons MergeBy = "persons"
)

// MergeBys is the closed set, loosest first: the order [MergeBy.Rank] reads.
func MergeBys() []MergeBy { return []MergeBy{MergeByWriters, MergeByMaintainers, MergeByPersons} }

// Valid reports a value this build knows. Empty is not one: a record's
// writer resolves every default ([MergeGate]).
func (m MergeBy) Valid() bool { return slices.Contains(MergeBys(), m) }

// Rank is how strict m is: its place in [MergeBys], or past every known
// value for an unknown one, so an authority this build cannot read never
// ranks looser than one it can.
func (m MergeBy) Rank() int {
	if i := slices.Index(MergeBys(), m); i >= 0 {
		return i
	}
	return len(MergeBys())
}

// Stricter is the stricter of m and o: what a merge is held to where both
// hold (two rules matching one ref, or a rule and code.merge_by_floor).
func (m MergeBy) Stricter(o MergeBy) MergeBy {
	if o.Rank() > m.Rank() {
		return o
	}
	return m
}

// Approvals is how many counted approvals a merge waits for.
type Approvals string

const (
	// ApprovalsNone waits for none.
	ApprovalsNone Approvals = "none"
	// ApprovalsOne waits for one.
	ApprovalsOne Approvals = "one"
)

// ApprovalCounts is the closed set, fewest first.
func ApprovalCounts() []Approvals { return []Approvals{ApprovalsNone, ApprovalsOne} }

// Valid reports a value this build knows; empty is none of them, as for
// [MergeBy].
func (a Approvals) Valid() bool { return slices.Contains(ApprovalCounts(), a) }

// DeleteSource is whether a merge deletes its change's source branch in the
// record that merges it.
type DeleteSource string

const (
	// DeleteSeats deletes a source branch in a seat's own namespace
	// (refs/heads/seats/<handle>/…), whose life the change was.
	DeleteSeats DeleteSource = "seats"
	// DeleteNone keeps every source branch.
	DeleteNone DeleteSource = "none"
)

// DeleteSources is the closed set, the default first.
func DeleteSources() []DeleteSource { return []DeleteSource{DeleteSeats, DeleteNone} }

// Valid reports a value this build knows; empty is none of them, as for
// [MergeBy].
func (d DeleteSource) Valid() bool { return slices.Contains(DeleteSources(), d) }

// The merge gate's bounds are config's, so a configuration is validated
// against the same values.
const (
	// MaxHumanPaths is the human paths one gate may name.
	MaxHumanPaths = config.CodeMaxHumanPaths
	// MaxRequiredChecks is the checks one gate may require.
	MaxRequiredChecks = config.CodeMaxRequiredChecks
	// MaxGateBytes is what one gate may weigh as its canonical JSON.
	MaxGateBytes = config.CodeMaxGateBytes
	// MaxQueueDepth is the candidates a target's queue tests at once.
	MaxQueueDepth = config.CodeMaxQueueDepth
	// MaxQueueBatch is the changes one candidate takes.
	MaxQueueBatch = config.CodeMaxQueueBatch
)

// MergeGate is what a merge into the refs its rule matches needs, carried
// whole on the rule ([Protection.Gate]). Every field is explicit, its writer
// having resolved each default: an omitted one would be the reader's guess,
// and the defaults are the configuration's to change. Its lists are in the
// order [CanonicalGate] forms, which [RepoSettings.Validate] enforces.
//
// Base format ([RecordVersion]): the domain landed on main with this type, so
// every build reads a record carrying one.
type MergeGate struct {
	// MergeBy is who may merge.
	MergeBy MergeBy `json:"merge_by"`
	// Approvals is how many counted approvals a merge waits for.
	Approvals Approvals `json:"approvals"`
	// ApprovalsUnless are the path classes ([PathClasses], never
	// no_code_change: [classInvalid]) that exempt a change whose every touched
	// path is of one from those approvals; sorted, each once.
	ApprovalsUnless []ChangeClass `json:"approvals_unless,omitempty"`
	// HumanPaths are the paths a change touching which waits for a person's
	// approval, whatever else approved it: at most [MaxHumanPaths], sorted by
	// glob, each once.
	HumanPaths []HumanPath `json:"human_paths,omitempty"`
	// Checks are the checks a merge waits for, named as a checks file names
	// them ([config.ValidCodeCheckName]): at most [MaxRequiredChecks], sorted,
	// each once.
	Checks []string `json:"checks,omitempty"`
	// Landing is how the target's queue lands a change.
	Landing Landing `json:"landing"`
	// OwnersApprove makes the path owners gating: a change waits for one
	// counted approval from someone eligible for every owned glob it touches.
	OwnersApprove bool `json:"owners_approve,omitempty"`
	// DeleteSource is whether a merge deletes its source branch in the same
	// record.
	DeleteSource DeleteSource `json:"delete_source"`
	// AgentsEditChecks lets a change to the checks' own `.crewlet/**`
	// ([ChecksDirGlob]) merge on a seat's approval where the rule requires
	// checks; without it such a rule holds that glob as an unlisted human
	// path, added where the gate is combined ([GateRulesOf], U44). Turning it
	// on is a loosening ([MergeGate.LoosenedBy]).
	AgentsEditChecks bool `json:"agents_edit_checks,omitempty"`
}

// HumanPath is one glob ([gitref.ParsePathGlob]) a change touching which
// waits for a person's approval, unless every path of it the glob matches is
// of a class Unless names: path classes only, sorted, each once.
type HumanPath struct {
	Glob   string        `json:"glob"`
	Unless []ChangeClass `json:"unless,omitempty"`
}

// Landing is how a gated target's queue lands a change, each tested merged on
// the target first: candidates tested at once (1..[MaxQueueDepth]) and
// changes one candidate takes (1..[MaxQueueBatch]).
type Landing struct {
	Depth int `json:"depth"`
	Batch int `json:"batch"`
}

// RuleGate is one branch rule's merge gate, named by its pattern: what a
// repository's detail answers a read that asks ([ReadQuery.Gates],
// [RepoDetail.Gates]). Never on the repository's row ([RepoRef.Protections]):
// the package doc's "A protection rule's merge gate rides it whole" says why.
type RuleGate struct {
	Pattern string    `json:"pattern"`
	Gate    MergeGate `json:"gate"`
}

// Equal reports two rules the same: their push rules by == (a [PushRule]
// holds no pointer), their gates by value. Never slices.Equal over rules: two
// reads of one rule hold two gate pointers, so every gated rule would compare
// changed, and every reconcile or no-op lifecycle request would write a
// settings record.
func (p Protection) Equal(o Protection) bool {
	if p.PushRule != o.PushRule {
		return false
	}
	if p.Gate == nil || o.Gate == nil {
		return p.Gate == nil && o.Gate == nil
	}
	return p.Gate.Equal(*o.Gate)
}

// Equal reports two gates the same field by field, lists in order: the same
// values in another order are another gate, which [RepoSettings.Validate]
// refuses to record.
func (g MergeGate) Equal(o MergeGate) bool {
	return g.MergeBy == o.MergeBy && g.Approvals == o.Approvals &&
		slices.Equal(g.ApprovalsUnless, o.ApprovalsUnless) &&
		slices.EqualFunc(g.HumanPaths, o.HumanPaths, HumanPath.Equal) &&
		slices.Equal(g.Checks, o.Checks) && g.Landing == o.Landing && g.OwnersApprove == o.OwnersApprove &&
		g.DeleteSource == o.DeleteSource && g.AgentsEditChecks == o.AgentsEditChecks
}

// Equal reports two human paths the same: their glob and their classes.
func (h HumanPath) Equal(o HumanPath) bool {
	return h.Glob == o.Glob && slices.Equal(h.Unless, o.Unless)
}

// CanonicalGate is a copy of g in the one form a record carries: every list
// sorted, human paths by glob, each one's classes too. It is the one place
// that order is formed (the engine's reconcile reads it), so every node
// deciding one declaration forms byte-identical records. It removes nothing:
// a duplicate is the caller's to refuse, as [RepoSettings.Validate] does.
func CanonicalGate(g MergeGate) MergeGate {
	out := g
	out.ApprovalsUnless = sortedClasses(g.ApprovalsUnless)
	if g.HumanPaths != nil {
		out.HumanPaths = make([]HumanPath, len(g.HumanPaths))
		for i, h := range g.HumanPaths {
			out.HumanPaths[i] = HumanPath{Glob: h.Glob, Unless: sortedClasses(h.Unless)}
		}
		slices.SortStableFunc(out.HumanPaths, func(a, b HumanPath) int { return strings.Compare(a.Glob, b.Glob) })
	}
	if g.Checks != nil {
		out.Checks = slices.Sorted(slices.Values(g.Checks))
	}
	return out
}

// sortedClasses is a sorted copy of classes; nil stays nil, so a gate that
// names none encodes unchanged.
func sortedClasses(classes []ChangeClass) []ChangeClass {
	if classes == nil {
		return nil
	}
	return slices.Sorted(slices.Values(classes))
}

// invalid is where g is no gate a record may carry and why (field a path
// below the gate, "" for the whole), or two empty strings: its fields
// ([MergeGate.fieldsInvalid]), then its weight against [MaxGateBytes], last
// so a gate bad in a field is refused for what that field says.
func (g MergeGate) invalid() (field, why string) {
	if field, why := g.fieldsInvalid(); why != "" {
		return field, why
	}
	if n := g.encodedBytes(); n > MaxGateBytes {
		return "", fmt.Sprintf("%d bytes as a record carries it, past the %d one gate may weigh: name fewer "+
			"human paths or checks", n, MaxGateBytes)
	}
	return "", ""
}

// fieldsInvalid is where a field of g is none a record may carry and why, or
// two empty strings: every enum known, every list in [CanonicalGate] order
// and bound, every glob one gitref reads, every check a name. It leaves out
// the whole weight, which a protect request cannot judge: what its change
// keeps of the rule's gate is the decide's to read ([GateChange.invalid]).
func (g MergeGate) fieldsInvalid() (field, why string) {
	switch {
	case !g.MergeBy.Valid():
		return "merge_by", fmt.Sprintf("%q is not one of %s", g.MergeBy, words.Or(MergeBys()))
	case !g.Approvals.Valid():
		return "approvals", fmt.Sprintf("%q is not one of %s", g.Approvals, words.Or(ApprovalCounts()))
	case g.Landing.Depth < 1 || g.Landing.Depth > MaxQueueDepth:
		return "landing.depth", fmt.Sprintf("%d is outside 1..%d", g.Landing.Depth, MaxQueueDepth)
	case g.Landing.Batch < 1 || g.Landing.Batch > MaxQueueBatch:
		return "landing.batch", fmt.Sprintf("%d is outside 1..%d", g.Landing.Batch, MaxQueueBatch)
	case !g.DeleteSource.Valid():
		return "delete_source", fmt.Sprintf("%q is not one of %s", g.DeleteSource, words.Or(DeleteSources()))
	case len(g.HumanPaths) > MaxHumanPaths:
		return "human_paths", fmt.Sprintf("%d paths, past the cap of %d", len(g.HumanPaths), MaxHumanPaths)
	case len(g.Checks) > MaxRequiredChecks:
		return "checks", fmt.Sprintf("%d checks, past the cap of %d", len(g.Checks), MaxRequiredChecks)
	}
	if field, why := classesInvalid("approvals_unless", g.ApprovalsUnless); field != "" {
		return field, why
	}
	for k, h := range g.HumanPaths {
		at := fmt.Sprintf("human_paths[%d]", k)
		if _, err := gitref.ParsePathGlob(h.Glob); err != nil {
			return at + ".glob", fmt.Sprintf("%q is not a path glob: %v", h.Glob, err)
		}
		if k > 0 && g.HumanPaths[k-1].Glob >= h.Glob {
			return at + ".glob", fmt.Sprintf("%q is not after %q — a gate's human paths are sorted by glob "+
				"and each glob named once, so one declaration forms one record on every node", h.Glob,
				g.HumanPaths[k-1].Glob)
		}
		if field, why := classesInvalid(at+".unless", h.Unless); field != "" {
			return field, why
		}
	}
	for k, name := range g.Checks {
		switch {
		case checkInvalid(name) != "":
			return fmt.Sprintf("checks[%d]", k), checkInvalid(name)
		case k > 0 && g.Checks[k-1] >= name:
			return fmt.Sprintf("checks[%d]", k), fmt.Sprintf("%q is not after %q — a gate's checks are "+
				"sorted and each named once", name, g.Checks[k-1])
		}
	}
	return "", ""
}

// classesInvalid is where list, the field at, is no list of exempting classes
// and why, or two empty strings: each a path class ([classInvalid]), sorted,
// each once.
func classesInvalid(at string, list []ChangeClass) (field, why string) {
	for k, c := range list {
		field := fmt.Sprintf("%s[%d]", at, k)
		if why := classInvalid(c); why != "" {
			return field, why
		}
		if k > 0 && list[k-1] >= c {
			return field, fmt.Sprintf("%q is not after %q — the classes are sorted and each named once", c,
				list[k-1])
		}
	}
	return "", ""
}

// classInvalid is why c is no class that exempts a change, or "": only a
// path class does, and no_code_change's refusal says why it is none.
func classInvalid(c ChangeClass) string {
	switch {
	case c == ChangeClassNoCodeChange:
		return "no_code_change is a revision's class, never a path's: an approval already carries " +
			"across a revision that changed nothing, so it has nothing to exempt"
	case !slices.Contains(PathClasses(), c):
		return fmt.Sprintf("%q is not one of %s", c, words.Or(PathClasses()))
	}
	return ""
}

// checkInvalid is why name is no check's name, or "".
func checkInvalid(name string) string {
	if config.ValidCodeCheckName(name) {
		return ""
	}
	return fmt.Sprintf("%q is not a check's name: a lowercase letter or digit, then up to %d more of them, "+
		"'.', '_' and '-'", name, MaxCheckNameBytes-1)
}

// LoosenedBy is every way g, the gate a protect request leaves a rule with,
// loosens old, each the phrase a refusal names it by; nil where g holds every
// merge to all old did. The one judgement of a loosening, read by the
// lifecycle decide ([loosenings]), which leaves one to a person (ADR-0088),
// field by field, so tightening one field never hides opening another:
// merge_by and approvals by lowering, failing closed ([lowered]);
// approvals_unless by each class added; human_paths by each glob dropped and
// each class added to a kept glob's unless (a broader glob replacing a
// narrower one drops it: containment is never judged, lest a misjudgement be
// a loosening a seat may make); checks by each dropped; owners_approve off;
// agents_edit_checks on. Landing and delete_source are neutral. Every field
// and ranked pair is held here by TestEveryGateFieldIsJudged.
func (g MergeGate) LoosenedBy(old MergeGate) []string {
	var opens []string
	if lowered(MergeBys(), old.MergeBy, g.MergeBy) {
		opens = append(opens, fmt.Sprintf("lowers merge_by from %s to %s", old.MergeBy, g.MergeBy))
	}
	if lowered(ApprovalCounts(), old.Approvals, g.Approvals) {
		opens = append(opens, fmt.Sprintf("lowers approvals from %s to %s", old.Approvals, g.Approvals))
	}
	if added := notIn(g.ApprovalsUnless, old.ApprovalsUnless); len(added) > 0 {
		opens = append(opens, fmt.Sprintf("adds %s to approvals_unless", words.And(added)))
	}
	var dropped []string
	for _, h := range old.HumanPaths {
		i := slices.IndexFunc(g.HumanPaths, func(kept HumanPath) bool { return kept.Glob == h.Glob })
		if i < 0 {
			dropped = append(dropped, h.Glob)
			continue
		}
		if added := notIn(g.HumanPaths[i].Unless, h.Unless); len(added) > 0 {
			opens = append(opens, fmt.Sprintf("adds %s to the unless of %s in human_paths", words.And(added),
				h.Glob))
		}
	}
	if len(dropped) > 0 {
		opens = append(opens, fmt.Sprintf("drops %s from human_paths", words.And(dropped)))
	}
	if dropped := notIn(old.Checks, g.Checks); len(dropped) > 0 {
		opens = append(opens, fmt.Sprintf("drops %s from checks", words.And(dropped)))
	}
	if old.OwnersApprove && !g.OwnersApprove {
		opens = append(opens, "turns owners_approve off")
	}
	if g.AgentsEditChecks && !old.AgentsEditChecks {
		opens = append(opens, "turns agents_edit_checks on")
	}
	return opens
}

// lowered reports whether a ranked field moved from from to to is lowered,
// order being its closed set, loosest first. Fail closed both ways: an
// unknown to is a lowering whatever it replaces, and an unknown from ranks
// above every known value, since a newer build's value may hold more.
func lowered[T comparable](order []T, from, to T) bool {
	if from == to {
		return false
	}
	rank := func(v T) int {
		if i := slices.Index(order, v); i >= 0 {
			return i
		}
		return len(order)
	}
	return !slices.Contains(order, to) || rank(to) < rank(from)
}

// notIn is every value of list that of does not hold, in list's order.
func notIn[T comparable](list, of []T) []T {
	var out []T
	for _, v := range list {
		if !slices.Contains(of, v) {
			out = append(out, v)
		}
	}
	return out
}

// defaultGate is the gate a rule with none takes under a [GateChange]: each
// field config's default, read from where config applies it
// ([config.CodeProtection.Gate]), so a tool's gate and a declaration's
// default alike.
func defaultGate() MergeGate {
	landing := config.CodeLanding{}.Resolved()
	return MergeGate{
		MergeBy:      MergeBy(config.CodeMergeBy("").Resolved()),
		Approvals:    Approvals(config.CodeApprovals("").Resolved()),
		Landing:      Landing{Depth: landing.Depth, Batch: landing.Batch},
		DeleteSource: DeleteSource(config.CodeDeleteSource("").Resolved()),
	}
}

// GateChange is a protect request's change to its rule's merge gate, which
// the deciding node resolves against the gate its own snapshot holds
// ([ProtectChange]). Never a gate or a list whole: either is formed from a
// read made elsewhere, so two concurrent requests (two each adding a check)
// would each undo the other's change unseen. A scalar it names replaces the
// field, a list it names changes item by item ([ListChange],
// [HumanPathsChange]), and a nil field is kept, or defaulted where the rule
// has no gate ([defaultGate]). Remove takes the gate away and names no field.
//
// A request is no record, so this type takes no versionedFields row: the
// record it leads to carries the settings whole in the base format
// ([MergeGate]), and every data node reads it ([RecordVersion]).
type GateChange struct {
	// Remove takes the rule's gate away.
	Remove    bool       `json:"remove,omitempty"`
	MergeBy   *MergeBy   `json:"merge_by,omitempty"`
	Approvals *Approvals `json:"approvals,omitempty"`
	// ApprovalsUnless, HumanPaths and Checks each change their list item by
	// item; the resolution puts what is left in [CanonicalGate] order.
	ApprovalsUnless *ListChange[ChangeClass] `json:"approvals_unless,omitempty"`
	HumanPaths      *HumanPathsChange        `json:"human_paths,omitempty"`
	Checks          *ListChange[string]      `json:"checks,omitempty"`
	// Landing changes the landing a field at a time.
	Landing          *LandingChange `json:"landing,omitempty"`
	OwnersApprove    *bool          `json:"owners_approve,omitempty"`
	DeleteSource     *DeleteSource  `json:"delete_source,omitempty"`
	AgentsEditChecks *bool          `json:"agents_edit_checks,omitempty"`
}

// ListChange is a gate change's change to one of the gate's value lists (its
// exempting classes or its checks): values added and removed, applied to the
// list the decide's snapshot holds ([ListChange.applyTo]), so adding a held
// value or removing an absent one does nothing.
type ListChange[T ~string] struct {
	Add    []T `json:"add,omitempty"`
	Remove []T `json:"remove,omitempty"`
}

// invalid is where c, the gate's field at, is no change and why, or two empty
// strings: a value at least, at most limit per list (more forms nothing a
// record carries), each passing valid (its why, or ""), each once, and none
// both added and removed, which is two requests.
func (c ListChange[T]) invalid(at string, limit int, valid func(T) string) (field, why string) {
	if len(c.Add) == 0 && len(c.Remove) == 0 {
		return at, "a change adds or removes one at least"
	}
	for _, l := range []struct {
		name   string
		values []T
	}{{"add", c.Add}, {"remove", c.Remove}} {
		if len(l.values) > limit {
			return at + "." + l.name, fmt.Sprintf("%d, past the %d the field may hold", len(l.values), limit)
		}
		for k, v := range l.values {
			item := fmt.Sprintf("%s.%s[%d]", at, l.name, k)
			if why := valid(v); why != "" {
				return item, why
			}
			if slices.Contains(l.values[:k], v) {
				return item, fmt.Sprintf("%q is listed twice", v)
			}
			if l.name == "remove" && slices.Contains(c.Add, v) {
				return item, fmt.Sprintf("%q is added by the same change; ask one or the other", v)
			}
		}
	}
	return "", ""
}

// applyTo is list with c applied: a copy, never list's own array, which is
// the snapshot's row; nil where nothing is left, as a gate naming none
// encodes it.
func (c ListChange[T]) applyTo(list []T) []T {
	out := slices.DeleteFunc(slices.Clone(list), func(v T) bool { return slices.Contains(c.Remove, v) })
	for _, v := range c.Add {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// HumanPathsChange is a gate change's change to its human paths, keyed by
// glob: each Put entry adds its glob or replaces its unless, and each Remove
// glob is taken away ([HumanPathsChange.applyTo]).
type HumanPathsChange struct {
	Put    []HumanPath `json:"put,omitempty"`
	Remove []string    `json:"remove,omitempty"`
}

// invalid is where c is no change and why, or two empty strings: an entry at
// least, at most [MaxHumanPaths] per list, every glob one gitref reads and
// each once, every unless of path classes each once, no glob put and removed.
func (c HumanPathsChange) invalid() (field, why string) {
	if len(c.Put) == 0 && len(c.Remove) == 0 {
		return "human_paths", "a change puts or removes a glob at least"
	}
	globs := make([]string, len(c.Put))
	for i, h := range c.Put {
		globs[i] = h.Glob
	}
	for _, l := range []struct {
		name  string
		globs []string
	}{{"put", globs}, {"remove", c.Remove}} {
		if len(l.globs) > MaxHumanPaths {
			return "human_paths." + l.name, fmt.Sprintf("%d, past the %d a gate may name", len(l.globs),
				MaxHumanPaths)
		}
		for k, glob := range l.globs {
			at := fmt.Sprintf("human_paths.%s[%d]", l.name, k)
			if l.name == "put" {
				at += ".glob"
			}
			switch _, err := gitref.ParsePathGlob(glob); {
			case err != nil:
				return at, fmt.Sprintf("%q is not a path glob: %v", glob, err)
			case slices.Contains(l.globs[:k], glob):
				return at, fmt.Sprintf("%q is listed twice", glob)
			case l.name == "remove" && slices.Contains(globs, glob):
				return at, fmt.Sprintf("%q is put by the same change; ask one or the other", glob)
			}
		}
	}
	for k, h := range c.Put {
		for i, class := range h.Unless {
			at := fmt.Sprintf("human_paths.put[%d].unless[%d]", k, i)
			if why := classInvalid(class); why != "" {
				return at, why
			}
			if slices.Contains(h.Unless[:i], class) {
				return at, fmt.Sprintf("%q is listed twice", class)
			}
		}
	}
	return "", ""
}

// applyTo is paths, the snapshot's, with c applied: a copy, nil where
// nothing is left.
func (c HumanPathsChange) applyTo(paths []HumanPath) []HumanPath {
	out := slices.DeleteFunc(slices.Clone(paths), func(h HumanPath) bool {
		return slices.Contains(c.Remove, h.Glob) ||
			slices.ContainsFunc(c.Put, func(put HumanPath) bool { return put.Glob == h.Glob })
	})
	for _, h := range c.Put {
		out = append(out, HumanPath{Glob: h.Glob, Unless: slices.Clone(h.Unless)})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// LandingChange is a gate change's change to the landing: each field it
// leaves nil is kept.
type LandingChange struct {
	Depth *int `json:"depth,omitempty"`
	Batch *int `json:"batch,omitempty"`
}

// gateChangeField is one field a [GateChange] names, by the name a request
// spells it, and whether a change names it.
type gateChangeField struct {
	name string
	set  bool
}

// fields is every field c may name but Remove, in [MergeGate]'s order: the
// one list [GateChangeFields] and [GateChange.named] read.
func (c GateChange) fields() []gateChangeField {
	return []gateChangeField{
		{"merge_by", c.MergeBy != nil}, {"approvals", c.Approvals != nil},
		{"approvals_unless", c.ApprovalsUnless != nil}, {"human_paths", c.HumanPaths != nil},
		{"checks", c.Checks != nil}, {"landing", c.Landing != nil},
		{"owners_approve", c.OwnersApprove != nil}, {"delete_source", c.DeleteSource != nil},
		{"agents_edit_checks", c.AgentsEditChecks != nil},
	}
}

// GateChangeFields is every field a [GateChange] may name but Remove, spelled
// as the gate's own, in [MergeGate]'s order: what a tool takes for a rule's
// gate. TestAGateChangeNamesEveryGateField holds it to both types.
func GateChangeFields() []string {
	fields := GateChange{}.fields()
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = f.name
	}
	return out
}

// named is every field c names, in [GateChangeFields]' order.
func (c GateChange) named() []string {
	var out []string
	for _, f := range c.fields() {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// invalid is where c is no change a request may carry and why (field a path
// below the gate, "" for the whole), or two empty strings: a removal names no
// field, any other change one at least, each list's change valid on its own
// ([ListChange.invalid], [HumanPathsChange.invalid]), and each field named
// valid on the gate it forms on a rule with none ([defaultGate]), since one
// refused there is refused on any gate. A list's length and the whole weight
// are the decide's, which alone reads what the change keeps.
func (c GateChange) invalid() (field, why string) {
	named := c.named()
	switch {
	case c.Remove && len(named) > 0:
		return "remove", fmt.Sprintf("a change removing the gate names none of its fields, and this one "+
			"names %s", words.And(named))
	case c.Remove:
		return "", ""
	case len(named) == 0:
		return "", "a change of the gate names one of its fields at least, or removes it"
	case c.Landing != nil && *c.Landing == (LandingChange{}):
		return "landing", "a change of the landing names its depth or batch at least"
	}
	// Each list's change on its own, so a refusal names the item sent: the
	// default gate below holds what it adds and none of what it removes.
	if l := c.ApprovalsUnless; l != nil {
		if field, why := l.invalid("approvals_unless", len(PathClasses()), classInvalid); why != "" {
			return field, why
		}
	}
	if l := c.HumanPaths; l != nil {
		if field, why := l.invalid(); why != "" {
			return field, why
		}
	}
	if l := c.Checks; l != nil {
		if field, why := l.invalid("checks", MaxRequiredChecks, checkInvalid); why != "" {
			return field, why
		}
	}
	return c.applyTo(defaultGate()).fieldsInvalid()
}

// applyTo is base with every field c names put in it, in [CanonicalGate]
// form, sharing no array with base. Remove is its caller's to read.
func (c GateChange) applyTo(base MergeGate) MergeGate {
	g := base
	if c.MergeBy != nil {
		g.MergeBy = *c.MergeBy
	}
	if c.Approvals != nil {
		g.Approvals = *c.Approvals
	}
	if c.ApprovalsUnless != nil {
		g.ApprovalsUnless = c.ApprovalsUnless.applyTo(base.ApprovalsUnless)
	}
	if c.HumanPaths != nil {
		g.HumanPaths = c.HumanPaths.applyTo(base.HumanPaths)
	}
	if c.Checks != nil {
		g.Checks = c.Checks.applyTo(base.Checks)
	}
	if l := c.Landing; l != nil {
		if l.Depth != nil {
			g.Landing.Depth = *l.Depth
		}
		if l.Batch != nil {
			g.Landing.Batch = *l.Batch
		}
	}
	if c.OwnersApprove != nil {
		g.OwnersApprove = *c.OwnersApprove
	}
	if c.DeleteSource != nil {
		g.DeleteSource = *c.DeleteSource
	}
	if c.AgentsEditChecks != nil {
		g.AgentsEditChecks = *c.AgentsEditChecks
	}
	return CanonicalGate(g)
}

// resolve is the gate c leaves a rule that held held (nil for none), as the
// decide resolves it: held where c is nil, none where c removes it, else c
// applied to held, or to the defaults where the rule held none.
func (c *GateChange) resolve(held *MergeGate) *MergeGate {
	switch {
	case c == nil:
		return held
	case c.Remove:
		return nil
	}
	base := defaultGate()
	if held != nil {
		base = *held
	}
	g := c.applyTo(base)
	return &g
}

// gateColumn is a rule's gate as code_protections.gate holds it: its JSON —
// the encoding the record carried it in — or "" for a rule with none.
func gateColumn(g *MergeGate) (string, error) {
	if g == nil {
		return "", nil
	}
	b, err := json.Marshal(g)
	if err != nil {
		return "", fmt.Errorf("gitlet: encode a merge gate: %w", err)
	}
	return string(b), nil
}

// gateFrom is the gate a code_protections.gate column holds: nil for "", a
// rule with none.
func gateFrom(column string) (*MergeGate, error) {
	if column == "" {
		return nil, nil
	}
	g, err := decodeGate(column)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// decodeGate is the gate a code_protections.gate column holds where it holds
// one: an empty column is no gate's JSON, and refused as such.
func decodeGate(column string) (MergeGate, error) {
	var g MergeGate
	if err := json.Unmarshal([]byte(column), &g); err != nil {
		return MergeGate{}, fmt.Errorf("gitlet: decode a merge gate: %w", err)
	}
	return g, nil
}
