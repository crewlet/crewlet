package credential

import (
	_ "embed"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/iam"
)

// WHAT THIS ENGINE ASKS OF A PASSWORD, AND WHAT IT DELIBERATELY DOES NOT.
//
// ONE RULE: twelve characters. No required digit, no required symbol, no
// forbidden repeat, no expiry. Composition rules are measurably
// counter-productive — they shrink the set people actually choose from
// (everybody appends `1!`), an attacker who knows the rule enumerates it, and
// they push people to write the result down. Length is the only thing that
// buys entropy from a human at no cost to them.
//
// # The blocklist, and why it is not ten thousand entries
//
// The obvious second screen is a corpus of the most common passwords, and the
// obvious corpus is a published top-ten-thousand list. Behind a TWELVE
// CHARACTER floor almost none of it is reachable: those lists are ranked by
// observed frequency, and what people actually choose clusters at six to ten
// characters, so the overwhelming majority of a top-ten-thousand list is
// already refused by the rule above. Embedding it would be roughly eighty
// kilobytes in every binary to refuse strings nothing can present.
//
// What DOES survive a twelve-character floor is a small, enumerable set of
// FAMILIES, and that is what `commonpasswords.txt` holds:
//
//   - keyboard walks long enough to clear the floor (`qwertyuiopas`,
//     `1qaz2wsx3edc`);
//   - runs and repeats (`123456789012`, `aaaaaaaaaaaa`, `123123123123`);
//   - a common word with padding, which is what a length rule produces when
//     somebody is trying to satisfy it (`password1234`, `letmein123456`);
//   - the handful of phrases that became common by being famous
//     (`correcthorsebatterystaple`).
//
// # Normalising is what makes a short list worth a long one
//
// A candidate is folded before it is compared: lower-cased, stripped of
// surrounding space, and with the substitutions everybody makes undone —
// `4` to `a`, `3` to `e`, `1` to `i`, `0` to `o`, `5` to `s`, `@` to `a`,
// `$` to `s`. So `P@ssw0rd1234` and `p4ssw0rd1234` are both `password1234`
// and both refused by ONE entry. A raw list has to carry every spelling; this
// carries one per family and catches the rest by construction.
//
// THE FOLD IS ONLY EVER USED TO REFUSE. It is never stored, never compared
// against a verifier and never shown to anybody: it is strictly a wider net,
// so a password that survives it is no weaker for the folding having happened.

//go:embed commonpasswords.txt
var commonList string

// common is the blocklist, already folded, built once.
//
// A MAP BUILT AT INIT rather than a scan of the file per call: the check runs
// on every password anybody sets, and a linear walk of the file would be the
// one part of this package whose cost grows with the list.
var common = buildCommon(commonList)

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
// IT SAYS WHAT IS WRONG, unlike every other refusal in this package, and the
// asymmetry is the whole of the difference between the two audiences: a failed
// SIGN-IN is answered to a stranger and must disclose nothing, while this is
// answered to somebody who has already proved who they are and is choosing a
// new secret. A generic refusal there is somebody typing variations until one
// sticks.
func CheckStrength(password string) error {
	if n := utf8.RuneCountInString(password); n < iam.MinPasswordChars {
		return fmt.Errorf("%w: it is %d characters and the minimum is %d. "+
			"There are no other rules — no required digit, no required symbol "+
			"— because length is the only one that makes a password harder to "+
			"guess without making it harder to remember", ErrWeak, n,
			iam.MinPasswordChars)
	}
	if common[fold(password)] {
		return fmt.Errorf("%w: it is one of the patterns people reach for when "+
			"a length rule is the only thing in the way — a word with digits "+
			"after it, a run along the keyboard, a repeated block. Substituting "+
			"digits for letters does not change that, and it is checked for",
			ErrWeak)
	}
	return nil
}

// fold is the form a candidate is MATCHED on. See the package's own rules
// above: it is only ever used to refuse, so a wider net costs nothing.
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
// THE RAW LINES AND NOT THE FOLDED KEYS, which is the whole point: the map is
// keyed on the fold, so reading it back would make every "is this entry
// already folded" assertion true by construction and the suite would be
// asserting its own preprocessing.
//
// EXPORTED FOR A TEST AND SAYING SO. The three properties it checks — that no
// entry sits below the length floor, that none is written in leetspeak, and
// that no two fold onto one string — are facts about the DATA rather than
// about the code, and a data file nothing reads back is one that accumulates
// lines that look like they work.
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
func Blocked(candidate string) bool { return common[fold(candidate)] }
