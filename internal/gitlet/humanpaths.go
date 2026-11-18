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

// A GATE'S HUMAN PATHS ARE MATCHED ON THE HOME: which entries of a target's
// [GateRules.HumanPaths] hold a revision to a person is a match of up to ~2000
// globs against MaxTouchedPaths paths (up to seconds), too costly for every
// read, listing row and decide in the one committer, as M5-D5 found for path
// owners. So the home matches once a revision, beside [MatchOwners], and its
// revise records each entry by PLACE ([GateRulesOf]'s order) beside the
// [HumanPathsDigest] it matched under, which alone gives a place its meaning.
// Readers hold it to code_repos.human_paths_digest: a stale revise is refused
// superseded, a stale change is due its facts again ([ReadChangesDue]) and
// human_approval reads COMPUTING meanwhile. The digest is the REPOSITORY'S (no
// statement can tell which rules match a branch): a change to any rule's human
// paths, and to no other field, re-dues every live change (cf. OwnersDigest).

// HumanPathMatch is which of a target's human paths ([GateRules.HumanPaths])
// hold a revision, each by its place counted from 0: Held those holding it to
// a person, Waived those whose every matched path is of a class their unless
// names. Each list ascending, no place in both; an entry matching no touched
// path is in neither. A PLACE AND NOT A GLOB, which would carry up to a gate's
// bytes per matching rule; a place names its entry only under
// [Revise.HumanPathsDigest].
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

// MatchHumanPaths is which of humanPaths (a target's, in [GateRules.HumanPaths]
// order) hold a revision touching touched, or every path where touchesAll,
// its paths classed by classes ([HumanPathMatch]): an entry matching a
// touched path holds it unless its unless waives every path it matched
// ([waives]). Touching all is held by every entry, waivable by
// none, and a glob this build cannot read holds it too, FAIL CLOSED: every
// stored glob passed validation. Run ON THE HOME only (this file's head).
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
// lowercase-hex SHA-256 of the JSON, in pattern order, of each gated rule
// holding a target to any: its pattern and its sorted human paths
// ([humanPathsOf]; ChecksDirGlob among them where it requires checks without
// agents_edit_checks). No such rule is the digest of an empty list. The
// applier writes it on the repository's row (code_repos.human_paths_digest),
// and the home sends it beside its match.
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
		// Strings always encode; reaching here is a broken encoder.
		panic(fmt.Sprintf("gitlet: encoding human paths: %v", err))
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
