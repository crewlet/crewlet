package credential

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/iam"
)

// WHAT THIS ENGINE ASKS OF A PASSWORD, AND WHAT IT DELIBERATELY DOES NOT.
//
// ONE RULE: twelve characters. No required digit or symbol, no forbidden
// repeat, no expiry. Composition rules shrink the set people choose from
// (everybody appends `1!`), are enumerable by an attacker, and push people to
// write the result down; length is the only thing that buys entropy from a
// human at no cost to them. The second screen is a blocklist of the families
// that clear the floor ([buildCommon]), matched on a folded form ([fold]).

//go:embed commonpasswords.txt
var commonList string

// common is the blocklist, already folded, as a MAP so a check does not walk the
// file on every password set.
//
// BUILT AT FIRST USE rather than at init: this package is linked into every
// process of the engine, Gitlet's proc-receive hook (exec'd once a push, running
// every linked init) among them. Folding the file is about 140 KB and 2 200
// allocations, which that hook paid per push for a table only a password change
// reads (cmd/crewlet's TestTheHookPaysLittleForTheEnginesInit holds the
// module's init to its bound).
var common = sync.OnceValue(func() map[string]bool { return buildCommon(commonList) })

// buildCommon folds the embedded list into a set.
//
// The list is not a published top-ten-thousand corpus: behind a TWELVE
// CHARACTER floor almost all of it is unreachable (people choose at six to ten
// characters), so embedding it would cost about 80 KB per binary to refuse
// strings nothing can present. What survives the floor is a small set of
// FAMILIES, which `commonpasswords.txt` holds:
//
//   - keyboard walks (`qwertyuiopas`, `1qaz2wsx3edc`);
//   - runs and repeats (`123456789012`, `aaaaaaaaaaaa`);
//   - a common word with padding (`password1234`);
//   - phrases famous enough to be common (`correcthorsebatterystaple`).
func buildCommon(raw string) map[string]bool {
	out := make(map[string]bool, 512)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[fold(line)] = true
	}
	return out
}

// CheckStrength reports why a password cannot be used, or nil.
//
// IT SAYS WHAT IS WRONG, unlike every other refusal here: a failed sign-in goes
// to a stranger and discloses nothing, while this goes to somebody who has
// proved who they are, where a generic refusal is them typing variations.
//
// The floor is an ARGUMENT, because a deployment may raise it: min is the
// company's `sign_in.min_password_length` held within the node's limit
// (internal/config's APIAuth.SignInPolicy). The length refused is the larger of
// it and [iam.MinPasswordChars], so zero or less still gets twelve.
func CheckStrength(password string, min int) error {
	floor := max(min, iam.MinPasswordChars)
	if n := utf8.RuneCountInString(password); n < floor {
		return fmt.Errorf("%w: it has %d characters, and it needs at least "+
			"%d. Length is the only rule", ErrWeak, n, floor)
	}
	if common()[fold(password)] {
		return fmt.Errorf("%w: it's too easy to guess. Avoid a word with "+
			"digits after it, keyboard runs and repeated blocks",
			ErrWeak)
	}
	return nil
}

// fold is the form a candidate is MATCHED on: lower-cased, trimmed, with the
// substitutions everybody makes undone, so `P@ssw0rd1234` and `p4ssw0rd1234`
// are both `password1234` and refused by ONE entry. The list carries one
// spelling per family, not every one.
//
// ONLY EVER USED TO REFUSE: never stored, compared against a verifier or shown,
// so a password surviving it is no weaker for the folding.
func fold(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return substitutions.Replace(s)
}

// substitutions are the letter-for-digit swaps a blocklist would otherwise
// have to carry a spelling of each.
//
// ONE DIRECTION ONLY — digit to letter, never letter to digit. Folding the
// other way as well would collapse `passw0rd` and `passward` onto one string
// and start refusing passwords that are not in any family at all.
var substitutions = strings.NewReplacer(
	"4", "a", "@", "a",
	"3", "e",
	"1", "i", "!", "i",
	"0", "o",
	"5", "s", "$", "s",
	"7", "t",
)

// BlocklistEntries is the file's own lines, VERBATIM, for the suite that holds
// the file to its own rules.
//
// THE RAW LINES AND NOT THE FOLDED KEYS: the map is keyed on the fold, so
// reading it back would make every "is this entry already folded" assertion true
// by construction.
//
// EXPORTED FOR A TEST AND SAYING SO. What it checks (no entry below the length
// floor, none in leetspeak, no two folding onto one string) are facts about the
// DATA, and a data file nothing reads back accumulates lines that only look
// like they work.
func BlocklistEntries() []string {
	var out []string
	for _, line := range strings.Split(commonList, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// Fold is [fold], exported for the same suite: a test that reimplemented the
// folding would be asserting its own copy of the rule.
func Fold(s string) string { return fold(s) }

// Blocked reports whether a candidate is on the blocklist, WITHOUT the length
// rule in front of it.
//
// Exported for the suite that holds the data file to its own rules: the length
// check refuses most of the file's own entries before the list is ever
// consulted, so a test that went through [CheckStrength] could not tell the
// embed working from the floor doing all the work.
func Blocked(candidate string) bool { return common()[fold(candidate)] }
