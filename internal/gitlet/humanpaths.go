package gitlet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/gitref"
)

// A GATE'S HUMAN PATHS, MATCHED ON THE HOME: which of the human paths a
// change's target's merge gate names ([GateRules.HumanPaths]) hold its head
// revision to a person — an entry matching a path it touches, unless every
// path the entry matched is of a class its unless names — is a GLOB MATCH,
// and no judgment makes it. A target's human paths are the union of every
// rule matching it, up to MaxProtectionsPerRepo × MaxHumanPaths entries and
// ChecksDirGlob, against up to MaxTouchedPaths paths: some two thousand globs
// at twenty nanoseconds a typical match against a thousand paths is some
// forty milliseconds, and the costliest glob gitref measures, 3.2 ms against
// a whole touched list, seconds — paid on every read of a change, every row
// of a listing and every round of every enqueue's and mergeability's decide,
// inside the repository's one committer. It is the cost M5-D5 refused for
// path owners, at the same bound, and it is answered the same way.
//
// So the repository's HOME matches, once a revision, beside the path owners
// ([MatchOwners]), and its revise records the answer ([Revise.HumanPaths]):
// each entry that holds the revision and each its unless waived, by its PLACE
// in the target's human paths in their one order — the order [GateRulesOf]
// sorts them in, a function of the rules alone — beside the DIGEST of the
// repository's rules' human paths it matched against ([HumanPathsDigest]).
// The applier writes the same digest on the repository's row beside the rules
// (code_repos.human_paths_digest), and every reader holds the recorded match
// to it, as it holds the owners' match to theirs: a revise matched against
// other rules is refused superseded, a change whose facts were matched
// against rules changed since is due its facts again ([ReadChangesDue]), and
// the gate judges human_approval COMPUTING meanwhile, never on a match of
// rules the repository no longer has. A place names one entry only under the
// digest it was matched under, which is what lets a place stand for a glob.
//
// THE DIGEST IS THE REPOSITORY'S, NOT THE TARGET'S: a statement compares it
// with the row's, and no statement can tell which rules match a branch. So a
// change of any rule's human paths — its pattern, its globs, its unless, or
// whether it requires checks and so holds ChecksDirGlob — makes every live
// change of the repository due its facts again, and no other change of a
// rule does: its merge_by, its approvals, its queue and its owners_approve
// are not in it (the path owners have a digest of their own, OwnersDigest).

// HumanPathMatch is which of the human paths a revision's target is held to
// hold it: each by its place in the target's human paths ([GateRules.HumanPaths],
// counted from 0), Held those holding the revision to a person and Waived
// those whose every matched path is of a class their unless names. Each list
// ascending, no place in both; an entry matching no touched path is in
// neither.
//
// A PLACE AND NOT A GLOB: a target's human paths are up to
// [MaxTargetHumanPaths] entries of up to a gate's bytes, and a record carrying
// them again by name would carry up to a gate's bytes for every rule matching
// the target; a place is a few bytes, and names its entry under the digest
// the match was made under ([Revise.HumanPathsDigest]).
type HumanPathMatch struct {
	Held   []int `json:"held,omitempty"`
	Waived []int `json:"waived,omitempty"`
}

// IsZero reports a match naming no entry — none of the human paths matched a
// touched path, or the target names none — which a record and a row omit.
func (m HumanPathMatch) IsZero() bool { return len(m.Held) == 0 && len(m.Waived) == 0 }

// MaxTargetHumanPaths is the most human paths a target may be held to: every
// rule of its repository matching it, each naming [MaxHumanPaths], and
// ChecksDirGlob, which every rule requiring checks holds it to unless it sets
// agents_edit_checks and which the combination names once. A place of a
// [HumanPathMatch] is below it.
const MaxTargetHumanPaths = MaxProtectionsPerRepo*MaxHumanPaths + 1

// humanPathMatchProblem refuses m as a revise carries it: each list ascending,
// each place once and below [MaxTargetHumanPaths], and no place in both.
// Whether a place names an entry of its target's human paths is the decide's
// to judge, against the rules its snapshot holds.
func humanPathMatchProblem(m HumanPathMatch) error {
	seen := make(map[int]bool, len(m.Held)+len(m.Waived))
	for _, list := range []struct {
		field  string
		places []int
	}{{"revise.human_paths.held", m.Held}, {"revise.human_paths.waived", m.Waived}} {
		for i, at := range list.places {
			field := fmt.Sprintf("%s[%d]", list.field, i)
			switch {
			case at < 0 || at >= MaxTargetHumanPaths:
				return badField(field, "%d is no place among the %d human paths a target may be held to "+
					"(MaxTargetHumanPaths)", at, MaxTargetHumanPaths)
			case i > 0 && at <= list.places[i-1]:
				return badField(field, "%d is not after %d: a match names its entries in their order, each once",
					at, list.places[i-1])
			case seen[at]:
				return badField(field, "human path %d is named held and waived: an entry holds the revision or "+
					"its unless waives it", at)
			}
			seen[at] = true
		}
	}
	return nil
}

// MatchHumanPaths is which of humanPaths — a target's, in their one order
// ([GateRules.HumanPaths]) — hold a revision touching touched, or every path
// where touchesAll, its paths classed by classes ([HumanPathMatch]): an entry
// matching a touched path holds it, unless every path it matched is of a
// class its unless names, which waives it — a whitespace_only change being a
// comment_only one too. A revision that touches all is held by every entry,
// which no class can waive, and a glob this build cannot read holds it too,
// FAIL CLOSED: every stored glob passed validation before a record carried it.
//
// ON THE HOME, NEVER IN A JUDGMENT (this file's head): up to
// [MaxTargetHumanPaths] globs against MaxTouchedPaths paths, each path's class
// looked up by its place.
func MatchHumanPaths(humanPaths []HumanPath, touched []string, touchesAll bool,
	classes RevisionClasses) HumanPathMatch {

	var m HumanPathMatch
	if touchesAll {
		for i := range humanPaths {
			m.Held = append(m.Held, i)
		}
		return m
	}
	class := make([]ChangeClass, len(touched))
	for _, at := range classes.CommentOnly {
		if at >= 0 && at < len(class) {
			class[at] = ChangeClassCommentOnly
		}
	}
	for _, at := range classes.WhitespaceOnly {
		if at >= 0 && at < len(class) {
			class[at] = ChangeClassWhitespaceOnly
		}
	}
	for i, entry := range humanPaths {
		glob, err := gitref.ParsePathGlob(entry.Glob)
		if err != nil {
			m.Held = append(m.Held, i)
			continue
		}
		matched, waived := false, true
		for at, path := range touched {
			if !glob.Match(path) {
				continue
			}
			matched = true
			if !waives(entry.Unless, class[at]) {
				waived = false
				break
			}
		}
		switch {
		case !matched:
		case waived:
			m.Waived = append(m.Waived, i)
		default:
			m.Held = append(m.Held, i)
		}
	}
	return m
}

// waives reports a path whose change is of class — its narrowest, "" for none
// — waived by unless: a comment_only change by comment_only, and a
// whitespace_only one by either, being a comment_only change too.
func waives(unless []ChangeClass, class ChangeClass) bool {
	switch class {
	case ChangeClassWhitespaceOnly:
		return slices.Contains(unless, ChangeClassWhitespaceOnly) || slices.Contains(unless, ChangeClassCommentOnly)
	case ChangeClassCommentOnly:
		return slices.Contains(unless, ChangeClassCommentOnly)
	}
	return false
}

// MatchTargetHumanPaths is [MatchHumanPaths] over the human paths gates — a
// repository's rules that carry a gate, as its home reads them — hold target
// to ([GateRulesOf]), and the digest of gates' human paths
// ([HumanPathsDigest]) a revise sends beside the match: what the home records
// of a revision touching touched, or all, its paths classed by classes.
func MatchTargetHumanPaths(gates []RuleGate, target string, touched []string, touchesAll bool,
	classes RevisionClasses) (HumanPathMatch, string, error) {

	rules := make([]Protection, len(gates))
	for i := range gates {
		rules[i] = Protection{PushRule: PushRule{Pattern: gates[i].Pattern}, Gate: &gates[i].Gate}
	}
	gate, err := GateRulesOf(rules, target, "")
	if err != nil {
		return HumanPathMatch{}, "", err
	}
	return MatchHumanPaths(gate.HumanPaths, touched, touchesAll, classes), HumanPathsDigest(gates), nil
}

// HumanPathsDigest is the digest of a repository's rules' human paths: the
// SHA-256, in lowercase hex, of the JSON of every rule whose gate holds a
// target to one — its pattern and the human paths it holds a target it
// matches to, ChecksDirGlob among them where it requires checks and does not
// set agents_edit_checks ([humanPathsOf]), each sorted — in pattern order.
// gates are the rules that carry a gate; a rule with none holds no human
// path, and none at all is the digest of an empty list. The applier writes it
// on the repository's row beside the rules (code_repos.human_paths_digest),
// and its home sends it beside the match it made against them.
func HumanPathsDigest(gates []RuleGate) string {
	type ruleHumanPaths struct {
		Pattern    string      `json:"pattern"`
		HumanPaths []HumanPath `json:"human_paths"`
	}
	canonical := []ruleHumanPaths{}
	for i := range gates {
		held := slices.Clone(humanPathsOf(&gates[i].Gate))
		if len(held) == 0 {
			continue
		}
		for j := range held {
			held[j].Unless = slices.Sorted(slices.Values(held[j].Unless))
		}
		slices.SortFunc(held, compareHumanPaths)
		canonical = append(canonical, ruleHumanPaths{Pattern: gates[i].Pattern, HumanPaths: held})
	}
	slices.SortFunc(canonical, func(a, b ruleHumanPaths) int { return strings.Compare(a.Pattern, b.Pattern) })
	encoded, err := json.Marshal(canonical)
	if err != nil {
		// A list of strings and named strings always encodes; reaching here
		// is a broken encoder, which no digest could be taken past.
		panic(fmt.Sprintf("code: encoding human paths: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// ruleGatesOf is rules' gates, each named by its rule's pattern — the rules
// that carry one, in rules' order.
func ruleGatesOf(rules []Protection) []RuleGate {
	var out []RuleGate
	for _, r := range rules {
		if r.Gate != nil {
			out = append(out, RuleGate{Pattern: r.Pattern, Gate: *r.Gate})
		}
	}
	return out
}
