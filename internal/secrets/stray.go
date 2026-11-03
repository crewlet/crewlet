package secrets

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/words"
)

// A PASTED CREDENTIAL WITH STRAY CHARACTERS INSIDE IS NEVER ONE A VENDOR
// ISSUED. A terminal that wraps a long token turns the break into spaces when
// it is copied, and a zero-width character comes along from chat and
// documents. Every surface that takes a pasted credential refuses such a value
// with [StrayProblem] before it is tested or stored.

// strayRune is a character no credential holds, at its 1-based position.
type strayRune struct {
	at int
	r  rune
}

// strayRunes is every whitespace, control or invisible formatting character
// in value.
func strayRunes(value string) []strayRune {
	var out []strayRune
	at := 0
	for _, r := range value {
		at++
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			out = append(out, strayRune{at: at, r: r})
		}
	}
	return out
}

// StrayProblem is the sentence for a credential value holding stray
// characters, "" for one that holds none: how many of which kind, where, and
// what to do. noun names the credential ("token", "provisioner token"). The
// sentence never quotes the value.
func StrayProblem(noun, value string) string {
	stray := strayRunes(value)
	if len(stray) == 0 {
		return ""
	}
	counts := map[string]int{}
	var kinds []string
	for _, s := range stray {
		kind := strayKind(s.r)
		if counts[kind] == 0 {
			kinds = append(kinds, kind)
		}
		counts[kind]++
	}
	parts := make([]words.Said, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, words.Said(plural(counts[kind], "1 "+kind, strconv.Itoa(counts[kind])+" "+kind+"s")))
	}
	positions := make([]int, 0, len(stray))
	for _, s := range stray {
		positions = append(positions, s.at)
	}
	// THE FIRST THREE POSITIONS and how many more, since a value pasted with a
	// space between every word would otherwise list every one of them.
	where := plural(len(stray), "character ", "characters ") + words.AndMore(positions, 3)
	return fmt.Sprintf("This %s contains %s (%s). %s %s is one unbroken line, so copy it again "+
		"or remove %s.", noun, words.And(parts), where, capitalised(article(noun)), noun,
		plural(len(stray), "it", "them"))
}

// strayKind names a stray character as a person sees it, or does not.
func strayKind(r rune) string {
	switch {
	case r == '\n' || r == '\r' || r == ' ' || r == ' ':
		return "line break"
	case r == '\t':
		return "tab"
	case unicode.IsSpace(r):
		return "space"
	}
	return "invisible character"
}

// capitalised is word with its first letter upper-cased.
func capitalised(word string) string {
	if word == "" {
		return word
	}
	return strings.ToUpper(word[:1]) + word[1:]
}

// article is "a" or "an" before noun, by its first letter.
func article(noun string) string {
	if noun != "" && strings.ContainsRune("AEIOUaeiou", rune(noun[0])) {
		return "an"
	}
	return "a"
}

// plural is one where n is 1, and many otherwise.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
