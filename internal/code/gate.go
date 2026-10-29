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

// LandingPolicy is how a gated target's queue lands a change.
type LandingPolicy string

const (
	// LandingPreLand tests each change merged on the target before it lands:
	// the speculative queue, candidates of batches, a red batch bisected.
	LandingPreLand LandingPolicy = "pre_land"
	// LandingPostLand lands changes ahead of their checks, which then run on
	// the target's newest tip, a red tip bisected over the landings.
	LandingPostLand LandingPolicy = "post_land"
)

// LandingPolicies is the closed set, the default first.
func LandingPolicies() []LandingPolicy { return []LandingPolicy{LandingPreLand, LandingPostLand} }

// Valid reports a value this build knows; empty is none of them, as for
// [MergeBy].
func (p LandingPolicy) Valid() bool { return slices.Contains(LandingPolicies(), p) }

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
	// AutoRevert lets an enqueuer consent, at the enqueue, to the revert of
	// a landing that breaks the target.
	AutoRevert bool `json:"auto_revert,omitempty"`
	// AgentsEditChecks lets a change to `.crewlet/**` — where a repository's
	// checks are defined — merge on a seat's approval where the rule requires
	// checks, which otherwise waits for a person's.
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

// Landing is how a gated target's queue lands a change: its policy, how many
// candidates it tests at once (1..[MaxQueueDepth]) and how many changes one
// candidate takes (1..[MaxQueueBatch]).
type Landing struct {
	Policy LandingPolicy `json:"policy"`
	Depth  int           `json:"depth"`
	Batch  int           `json:"batch"`
}

// Equal reports two rules the same, field by field and their gates BY VALUE.
//
// NEVER slices.Equal OVER RULES: a gate is a pointer, and two reads of one
// rule are two pointers, so a comparison of rules by == finds every gated
// rule changed — every reconcile of an unchanged declaration would write a
// settings record for it, and every sync round of a git node would write its
// repository's state again.
func (p Protection) Equal(o Protection) bool {
	if p.Pattern != o.Pattern || p.AllowForce != o.AllowForce || p.AllowDelete != o.AllowDelete ||
		p.DirectPush != o.DirectPush {
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
		g.DeleteSource == o.DeleteSource && g.AutoRevert == o.AutoRevert && g.AgentsEditChecks == o.AgentsEditChecks
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
// below the gate, "" for the gate whole — or two empty strings: every enum one
// this build knows, every list in its one order ([CanonicalGate]) and inside
// its bound, every glob gitref reads and every check a name, and the whole at
// most [MaxGateBytes] as a record encodes it — judged last, since a gate
// refused for a field is refused for what that field says.
func (g MergeGate) invalid() (field, why string) {
	switch {
	case !g.MergeBy.Valid():
		return "merge_by", fmt.Sprintf("%q is not one of %v", g.MergeBy, MergeBys())
	case !g.Approvals.Valid():
		return "approvals", fmt.Sprintf("%q is not one of %v", g.Approvals, ApprovalCounts())
	case !g.Landing.Policy.Valid():
		return "landing.policy", fmt.Sprintf("%q is not one of %v", g.Landing.Policy, LandingPolicies())
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
		case !config.ValidCodeCheckName(name):
			return fmt.Sprintf("checks[%d]", k), fmt.Sprintf("%q is not a check's name: a lowercase letter "+
				"or digit, then up to %d more of them, '.', '_' and '-'", name, MaxCheckNameBytes-1)
		case k > 0 && g.Checks[k-1] >= name:
			return fmt.Sprintf("checks[%d]", k), fmt.Sprintf("%q is not after %q — a gate's checks are "+
				"sorted and each named once", name, g.Checks[k-1])
		}
	}
	if n := g.encodedBytes(); n > MaxGateBytes {
		return "", fmt.Sprintf("%d bytes as a record carries it, past the %d one gate may weigh: name fewer "+
			"human paths or checks", n, MaxGateBytes)
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
		switch {
		case c == ChangeClassNoCodeChange:
			return field, "no_code_change is a revision's class, never a path's: an approval already carries " +
				"across a revision that changed nothing, so it has nothing to exempt"
		case !slices.Contains(PathClasses(), c):
			return field, fmt.Sprintf("%q is not one of %v", c, PathClasses())
		case k > 0 && list[k-1] >= c:
			return field, fmt.Sprintf("%q is not after %q — the classes are sorted and each named once", c,
				list[k-1])
		}
	}
	return "", ""
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
	var g MergeGate
	if err := json.Unmarshal([]byte(column), &g); err != nil {
		return nil, fmt.Errorf("code: decode a merge gate: %w", err)
	}
	return &g, nil
}
