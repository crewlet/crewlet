package code

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
)

// A PROTECTION RULE'S MERGE GATE: what a merge into the refs the rule matches
// needs — who may merge it, the approvals it waits for and the paths only a
// person approves, the checks that must pass, how a queue lands it — recorded
// on the rule it belongs to. Why it is one value, and how two of them are
// compared, is the package doc's "A protection rule's merge gate rides it
// whole".

// MergeBy is who may merge into a gated ref: the standing on the repository
// a merge's principal holds ([GrantOf]), or a person.
type MergeBy string

const (
	// MergeByWriters lets anybody who may write to the repository merge.
	MergeByWriters MergeBy = "writers"
	// MergeByMaintainers lets only those who maintain the repository merge.
	MergeByMaintainers MergeBy = "maintainers"
	// MergeByPersons lets only a person merge — never a seat, a unit's lead
	// included.
	MergeByPersons MergeBy = "persons"
)

// MergeBys is the closed set, loosest first: the order [MergeBy.Rank] reads.
func MergeBys() []MergeBy { return []MergeBy{MergeByWriters, MergeByMaintainers, MergeByPersons} }

// Valid reports a value this build knows. EMPTY IS NOT ONE: a record carries
// a gate whole, its writer having resolved every default ([MergeGate]).
func (m MergeBy) Valid() bool { return slices.Contains(MergeBys(), m) }

// Rank is how strict m is: its place in [MergeBys] — and past every value this
// build knows for one it does not, so an authority it cannot read is never
// taken as looser than one it can.
func (m MergeBy) Rank() int {
	if i := slices.Index(MergeBys(), m); i >= 0 {
		return i
	}
	return len(MergeBys())
}

// Stricter is the stricter of m and o: what a merge is held to where both
// hold — two rules matching one ref, or a rule and the company's floor
// (code.merge_by_floor).
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

// The merge gate's bounds, each config's ([config.CodeMaxHumanPaths] and the
// rest), which a configuration is validated against with the same value.
const (
	// MaxHumanPaths is config.CodeMaxHumanPaths: the human paths one gate
	// names.
	MaxHumanPaths = config.CodeMaxHumanPaths
	// MaxRequiredChecks is config.CodeMaxRequiredChecks: the checks one gate
	// requires.
	MaxRequiredChecks = config.CodeMaxRequiredChecks
	// MaxGateBytes is config.CodeMaxGateBytes: what one gate weighs as a
	// record carries it, its canonical JSON.
	MaxGateBytes = config.CodeMaxGateBytes
	// MaxQueueDepth is config.CodeMaxQueueDepth: the candidates a target's
	// queue tests at once.
	MaxQueueDepth = config.CodeMaxQueueDepth
	// MaxQueueBatch is config.CodeMaxQueueBatch: the changes one candidate
	// takes.
	MaxQueueBatch = config.CodeMaxQueueBatch
)

// MergeGate is what a merge into the refs its rule matches needs, carried
// whole on the rule ([Protection.Gate]): EVERY FIELD EXPLICIT, its writer
// having resolved each default — as a repository's settings carry its
// visibility — because a record that left one out would be a reader's guess
// at which it meant, and the configuration's defaults are the configuration's
// to change. Its lists are in the one order [CanonicalGate] forms, which
// [RepoSettings.Validate] holds a record to.
//
// BASE FORMAT, AS A STATED CONDITION: no build that carries the code domain
// has run without this type — none of the domain is on main, so no image and
// no release holds it — and while that is so, every build that meets a record
// carrying a gate reads it. Once a build without it has run, each op whose
// payload carries settings takes {Name: "Protection.Gate", Since: 2, Op: <the
// op: create, settings, restore>, Path: []string{"mutation", "settings",
// "protections", "gate"}} in versionedFields, with RecordVersion at 2, its
// line under "What each version added" and a Carrying record in the
// statelogtest candidate — such a build would apply settings without their
// gates, and a node running it would hold rules no gate guards that every
// other node gates.
type MergeGate struct {
	// MergeBy is who may merge.
	MergeBy MergeBy `json:"merge_by"`
	// Approvals is how many counted approvals a merge waits for.
	Approvals Approvals `json:"approvals"`
	// ApprovalsUnless are the PATH classes that exempt a change from those
	// approvals: one whose every touched path is of a class listed here
	// waits for none. Path classes only ([PathClasses]) — an approval already
	// carries across a revision that changed nothing, so no_code_change has
	// nothing to exempt — sorted and each once.
	ApprovalsUnless []ChangeClass `json:"approvals_unless,omitempty"`
	// HumanPaths are the paths a change touching which waits for a PERSON's
	// approval, whatever else approved it: at most [MaxHumanPaths], sorted by
	// glob and each glob once.
	HumanPaths []HumanPath `json:"human_paths,omitempty"`
	// Checks are the checks a merge waits for, by the name a checks file
	// gives each ([config.ValidCodeCheckName], the one rule both read): at
	// most [MaxRequiredChecks], sorted and each once.
	Checks []string `json:"checks,omitempty"`
	// Landing is how the target's queue lands a change.
	Landing Landing `json:"landing"`
	// OwnersApprove makes the repository's path owners gating: a change waits
	// for ONE counted approval from someone eligible for every owned glob its
	// paths touch.
	OwnersApprove bool `json:"owners_approve,omitempty"`
	// DeleteSource is whether a merge deletes its source branch in the same
	// record.
	DeleteSource DeleteSource `json:"delete_source"`
	// AgentsEditChecks lets a change to `.crewlet/**` — where a repository's
	// checks are defined ([ChecksDirGlob]) — merge on a seat's approval where
	// the rule requires checks, which otherwise waits for a person's: a rule
	// with Checks and without it holds that glob as a human path its record
	// never lists, added where the gate is combined ([GateRulesOf], U44).
	// Turning it on is a loosening ([MergeGate.LoosenedBy]).
	AgentsEditChecks bool `json:"agents_edit_checks,omitempty"`
}

// HumanPath is one glob of a repository's tree a change touching which waits
// for a person's approval ([gitref.ParsePathGlob]), unless every path of the
// change the glob matched is of a class Unless names: path classes only,
// sorted and each once.
type HumanPath struct {
	Glob   string        `json:"glob"`
	Unless []ChangeClass `json:"unless,omitempty"`
}

// Landing is how a gated target's queue lands a change — every change tested
// merged on the target before it lands: how many candidates it tests at once
// (1..[MaxQueueDepth]) and how many changes one candidate takes
// (1..[MaxQueueBatch]).
type Landing struct {
	Depth int `json:"depth"`
	Batch int `json:"batch"`
}

// RuleGate is one branch rule's merge gate, named by the rule's pattern: how
// one repository's detail answers its rules' gates to a read that asks for
// them ([ReadQuery.Gates], [RepoDetail.Gates]), beside the rules as a push is
// judged by them, which the repository's row answers ([RepoRef.Protections]).
// Never on that row: every git request's admission, every page of a git node's
// sync and the cache's state file read it, and no gate changes a thing about
// any of them.
type RuleGate struct {
	Pattern string    `json:"pattern"`
	Gate    MergeGate `json:"gate"`
}

// Equal reports two rules the same: their push rules, and their gates BY
// VALUE.
//
// NEVER slices.Equal OVER RULES: a gate is a pointer, and two reads of one
// rule are two pointers, so a comparison of rules by == finds every gated
// rule changed — every reconcile of an unchanged declaration would write a
// settings record for it, and a lifecycle request that changes nothing would
// write one too. The push half compares by ==: a [PushRule] holds no pointer.
func (p Protection) Equal(o Protection) bool {
	if p.PushRule != o.PushRule {
		return false
	}
	if p.Gate == nil || o.Gate == nil {
		return p.Gate == nil && o.Gate == nil
	}
	return p.Gate.Equal(*o.Gate)
}

// Equal reports two gates the same, field by field — their lists as lists,
// so a gate whose lists hold the same values in another order is another
// gate, which [RepoSettings.Validate] refuses to record.
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

// CanonicalGate is g in the ONE FORM a record carries it in: its exempting
// classes sorted, its human paths sorted by glob — each one's classes sorted
// — and its checks sorted; a copy, leaving g as it was. THE ONE PLACE the
// order is formed, read by the engine's reconcile for a declaration, so every
// node deciding one declaration forms byte-identical records. It removes
// nothing: a value named twice is the caller's to refuse, and
// [RepoSettings.Validate] refuses it.
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

// sortedClasses is classes sorted, a copy; nil stays nil, so a gate that
// names none encodes as it did.
func sortedClasses(classes []ChangeClass) []ChangeClass {
	if classes == nil {
		return nil
	}
	return slices.Sorted(slices.Values(classes))
}

// invalid is where g is no gate a record may carry and why — field a path
// below the gate, "" for the gate whole — or two empty strings: every field
// one a record may carry ([MergeGate.fieldsInvalid]), and the whole at most
// [MaxGateBytes] as a record encodes it — judged last, since a gate refused
// for a field is refused for what that field says.
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
// two empty strings: every enum one this build knows, every list in its one
// order ([CanonicalGate]) and inside its bound, every glob gitref reads and
// every check a name. Everything [MergeGate.invalid] judges but the gate's
// whole weight, which a protect request's own validation cannot judge: what
// its change keeps of the rule's gate is the decide's to read
// ([GateChange.invalid]).
func (g MergeGate) fieldsInvalid() (field, why string) {
	switch {
	case !g.MergeBy.Valid():
		return "merge_by", fmt.Sprintf("%q is not one of %v", g.MergeBy, MergeBys())
	case !g.Approvals.Valid():
		return "approvals", fmt.Sprintf("%q is not one of %v", g.Approvals, ApprovalCounts())
	case g.Landing.Depth < 1 || g.Landing.Depth > MaxQueueDepth:
		return "landing.depth", fmt.Sprintf("%d is outside 1..%d", g.Landing.Depth, MaxQueueDepth)
	case g.Landing.Batch < 1 || g.Landing.Batch > MaxQueueBatch:
		return "landing.batch", fmt.Sprintf("%d is outside 1..%d", g.Landing.Batch, MaxQueueBatch)
	case !g.DeleteSource.Valid():
		return "delete_source", fmt.Sprintf("%q is not one of %v", g.DeleteSource, DeleteSources())
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

// classesInvalid is where list, the field at, is no list of the classes that
// exempt a change in the form a gate carries one and why, or two empty
// strings: each a PATH class — no_code_change named for why it is none —
// sorted and each once.
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

// classInvalid is why c is no class that exempts a change, or "": a PATH
// class, no_code_change named for why it is none.
func classInvalid(c ChangeClass) string {
	switch {
	case c == ChangeClassNoCodeChange:
		return "no_code_change is a revision's class, never a path's: an approval already carries " +
			"across a revision that changed nothing, so it has nothing to exempt"
	case !slices.Contains(PathClasses(), c):
		return fmt.Sprintf("%q is not one of %v", c, PathClasses())
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

// LoosenedBy is every way g — the gate a protect request leaves a rule with —
// LOOSENS old, the gate the rule held: lets through a merge old held back, or
// lets it through on less. Each is the phrase a refusal names it by, which
// names its field; nil where g holds every merge to all old did. THE ONE
// JUDGEMENT of a gate's loosening, read by the lifecycle decide
// ([loosenings]), which leaves a loosening to a person (ADR-0058) — field by
// field, so a change that tightens one field and opens another loosens by the
// one it opens:
//
//   - merge_by and approvals loosen LOWERED. FAIL CLOSED, as a rule's
//     direct_push is judged: a value this build does not know ranks above
//     every value it does, so a move away from one is a lowering, and a move
//     to one is judged a loosening, since nothing here says what it lets
//     through ([lowered]);
//   - approvals_unless loosens by each class it adds: one more kind of
//     change that merges with no approval;
//   - human_paths loosens by each glob it drops and by each class it adds to
//     the unless of a glob it keeps. An entry is KEPT only by the same glob
//     with an unless naming nothing the old one did not: a broader glob put
//     in a narrower one's place is the narrower one dropped, since whether one
//     glob matches everything another does is no judgement made here — and one
//     that erred would make a loosening a seat may make;
//   - checks loosens by each required check it drops;
//   - owners_approve loosens turned off, as a merge then waits for no path
//     owner; and agents_edit_checks turned on, as a seat's approval then
//     stands for a change to the checks themselves;
//   - landing.depth, landing.batch and delete_source are NEUTRAL: how fast a
//     queue lands and what a merge tidies after it, never what a merge is held
//     to.
//
// A field a later milestone adds to [MergeGate] is judged here or named
// neutral: TestEveryGateFieldIsJudged holds this method to the struct's own
// fields by reflection, and to every pair of each ranked field's closed set.
func (g MergeGate) LoosenedBy(old MergeGate) []string {
	var opens []string
	if lowered(MergeBys(), old.MergeBy, g.MergeBy) {
		opens = append(opens, fmt.Sprintf("lowers merge_by from %s to %s", old.MergeBy, g.MergeBy))
	}
	if lowered(ApprovalCounts(), old.Approvals, g.Approvals) {
		opens = append(opens, fmt.Sprintf("lowers approvals from %s to %s", old.Approvals, g.Approvals))
	}
	if added := notIn(g.ApprovalsUnless, old.ApprovalsUnless); len(added) > 0 {
		opens = append(opens, fmt.Sprintf("adds %s to approvals_unless", listed(added)))
	}
	var dropped []string
	for _, h := range old.HumanPaths {
		i := slices.IndexFunc(g.HumanPaths, func(kept HumanPath) bool { return kept.Glob == h.Glob })
		if i < 0 {
			dropped = append(dropped, h.Glob)
			continue
		}
		if added := notIn(g.HumanPaths[i].Unless, h.Unless); len(added) > 0 {
			opens = append(opens, fmt.Sprintf("adds %s to the unless of %s in human_paths", listed(added),
				h.Glob))
		}
	}
	if len(dropped) > 0 {
		opens = append(opens, fmt.Sprintf("drops %s from human_paths", listed(dropped)))
	}
	if dropped := notIn(old.Checks, g.Checks); len(dropped) > 0 {
		opens = append(opens, fmt.Sprintf("drops %s from checks", listed(dropped)))
	}
	if old.OwnersApprove && !g.OwnersApprove {
		opens = append(opens, "turns owners_approve off")
	}
	if g.AgentsEditChecks && !old.AgentsEditChecks {
		opens = append(opens, "turns agents_edit_checks on")
	}
	return opens
}

// lowered reports whether a ranked field moved from from to to is LOWERED —
// order is its closed set, the loosest first: to ranks below from, or to is a
// value this build does not know, which is judged a lowering whatever it
// replaces. A value from that this build does not know ranks above every value
// it does, so any move away from one is a lowering: a newer build's value may
// hold more than any this one knows. FAIL CLOSED both ways.
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

// listed is values as a sentence names them: "a", "a and b", "a, b and c".
func listed[T ~string](values []T) string {
	words := make([]string, len(values))
	for i, v := range values {
		words[i] = string(v)
	}
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// defaultGate is the gate a rule with none takes where a protect request's
// change names some of a gate's fields ([GateChange]): every other field its
// default — writers, none, a queue at depth 4 and batch 2, seats — each read
// from the ONE PLACE config applies it ([config.CodeProtection.Gate]), so a
// gate a tool forms and a gate a declaration forms default alike.
func defaultGate() MergeGate {
	landing := config.CodeLanding{}.Resolved()
	return MergeGate{
		MergeBy:      MergeBy(config.CodeMergeBy("").Resolved()),
		Approvals:    Approvals(config.CodeApprovals("").Resolved()),
		Landing:      Landing{Depth: landing.Depth, Batch: landing.Batch},
		DeleteSource: DeleteSource(config.CodeDeleteSource("").Resolved()),
	}
}

// GateChange is a protect request's change to the merge gate of the rule it
// sets, which the deciding node RESOLVES against the gate that rule carries
// in the rows its own snapshot read ([ProtectChange]) — never a gate whole,
// which a caller could form only from a read made in another transaction: two
// requests changing two fields of one gate at once would each send the
// other's field as it had read it, and the later record would undo the
// earlier change — an approval it required, a check it added — unseen. Each
// scalar it names REPLACES that field; each LIST it names is changed item by
// item ([ListChange], [HumanPathsChange]), for the same reason one level down:
// requiring one more check is the commonest edit a gate takes, and a list
// sent whole is one formed from a read made elsewhere, so two requests each
// adding a check would each drop the other's — silently for a person, and as
// a refused "drops" for a seat that only added. Every field it leaves nil is
// kept as the rule's gate has it, or takes its default where the rule has
// none ([defaultGate]), so naming one field forms a gate, as a declaration
// naming one does. Remove takes the gate away and names no field.
//
// A REQUEST IS NO RECORD, so this type takes no versionedFields row: the
// record a protect request leads to carries the repository's settings whole,
// and [MergeGate] names the row those take. It crosses the estate's wire to
// whichever data node decides it, under the code domain's stated condition —
// no build carrying the domain has run without it, none of the domain being
// on main — so every node asked it reads it.
type GateChange struct {
	// Remove takes the rule's gate away.
	Remove    bool       `json:"remove,omitempty"`
	MergeBy   *MergeBy   `json:"merge_by,omitempty"`
	Approvals *Approvals `json:"approvals,omitempty"`
	// ApprovalsUnless, HumanPaths and Checks each change the field's list
	// item by item; the resolution puts what it leaves in the one order a
	// record carries ([CanonicalGate]).
	ApprovalsUnless *ListChange[ChangeClass] `json:"approvals_unless,omitempty"`
	HumanPaths      *HumanPathsChange        `json:"human_paths,omitempty"`
	Checks          *ListChange[string]      `json:"checks,omitempty"`
	// Landing changes the landing a field at a time.
	Landing          *LandingChange `json:"landing,omitempty"`
	OwnersApprove    *bool          `json:"owners_approve,omitempty"`
	DeleteSource     *DeleteSource  `json:"delete_source,omitempty"`
	AgentsEditChecks *bool          `json:"agents_edit_checks,omitempty"`
}

// ListChange is a gate change's change to one of the gate's lists of values —
// its exempting classes or its required checks: values ADDED beside those it
// holds and values REMOVED from them, applied by the decide to the list its
// snapshot holds ([ListChange.applyTo]), so a value added that the list holds
// already is added as nothing and one removed that it does not hold is
// removed as nothing.
type ListChange[T ~string] struct {
	Add    []T `json:"add,omitempty"`
	Remove []T `json:"remove,omitempty"`
}

// invalid is where c, the gate's field at, is no change and why, or two empty
// strings: a value at least, at most limit in each list — a list longer than
// the field may hold adds or removes nothing a record could carry — each a
// value valid holds of the field (its why, or ""), each once in its list, and
// none both added and removed, which is two requests rather than one.
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

// applyTo is list — the field as the gate holds it — with c applied: a copy,
// never list's own array, which is the snapshot's row; nil where nothing is
// left, as a gate that names none encodes it.
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
// glob: each entry Put sets that glob's entry — added, or its unless replaced
// where the gate has the glob — and each glob Remove names is taken away,
// applied by the decide to the human paths its snapshot holds
// ([HumanPathsChange.applyTo]).
type HumanPathsChange struct {
	Put    []HumanPath `json:"put,omitempty"`
	Remove []string    `json:"remove,omitempty"`
}

// invalid is where c is no change and why, or two empty strings: an entry at
// least, at most [MaxHumanPaths] in each list, every glob one gitref reads and
// each once, every unless of path classes each once, and no glob both put and
// removed.
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

// applyTo is paths — the gate's, as its snapshot holds them — with c applied:
// a copy, nil where nothing is left.
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

// fields is every field c may name besides its removal, in [MergeGate]'s
// order — THE ONE LIST, read by [GateChangeFields] and by what c names.
func (c GateChange) fields() []gateChangeField {
	return []gateChangeField{
		{"merge_by", c.MergeBy != nil}, {"approvals", c.Approvals != nil},
		{"approvals_unless", c.ApprovalsUnless != nil}, {"human_paths", c.HumanPaths != nil},
		{"checks", c.Checks != nil}, {"landing", c.Landing != nil},
		{"owners_approve", c.OwnersApprove != nil}, {"delete_source", c.DeleteSource != nil},
		{"agents_edit_checks", c.AgentsEditChecks != nil},
	}
}

// GateChangeFields is every field a [GateChange] may name besides its
// removal, by the name a request spells it — the gate's own — in
// [MergeGate]'s order: what a tool takes for a rule's gate.
// TestAGateChangeNamesEveryGateField holds it to both types.
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

// invalid is where c is no change a request may carry and why — field a path
// below the request's gate, "" for the change whole — or two empty strings: a
// removal naming no field, or a change naming one at least, a landing's
// change one of its own at least, each list's change an item at least, each
// item a value the list may hold and named once ([ListChange.invalid],
// [HumanPathsChange.invalid]); and each field it names one a record may
// carry, judged on the gate it forms on a rule with none ([defaultGate]),
// since a field refused there is refused whatever gate it changes. A list's
// length and the gate's whole weight are left to the decide, which alone
// reads what the change keeps of the rule's gate.
func (c GateChange) invalid() (field, why string) {
	named := c.named()
	switch {
	case c.Remove && len(named) > 0:
		return "remove", fmt.Sprintf("a change removing the gate names none of its fields, and this one "+
			"names %s", listed(named))
	case c.Remove:
		return "", ""
	case len(named) == 0:
		return "", "a change of the gate names one of its fields at least, or removes it"
	case c.Landing != nil && *c.Landing == (LandingChange{}):
		return "landing", "a change of the landing names its depth or batch at least"
	}
	// EACH LIST'S CHANGE ON ITS OWN, naming the item its caller sent: the
	// gate a change forms on a rule with none holds what it adds, and nothing
	// of what it removes.
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

// applyTo is base with every field c names put in it — a scalar replaced, a
// list changed item by item — in the one form a record carries a gate in
// ([CanonicalGate]), sharing no array with base. Remove is its caller's to
// read.
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

// resolve is the gate c leaves a rule with that held held — nil for none —
// RESOLVED as the decide resolves it: held as it stands where c is nil, none
// where c removes it, and otherwise held with c's fields put in it, or the
// defaults with them where the rule held none.
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
		return "", fmt.Errorf("code: encode a merge gate: %w", err)
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
		return MergeGate{}, fmt.Errorf("code: decode a merge gate: %w", err)
	}
	return g, nil
}
