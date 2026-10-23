// Package words says a set in a sentence: "a", "b" or "c", and "a", "b" and
// "c".
//
// # Why a set needs a package
//
// A Go format prints a slice as Go spells one, and every verb does: %v and %s
// write [html slides doc design], %q writes ["html" "slides" "doc" "design"],
// and %d writes [1 2 3]. That is the shape of a value in a program, and it
// reached people and models as the end of a sentence: an artifact refusal on a
// screen read `kind "page" is not one of [html slides doc design]`, a tool
// answer handed a model the same brackets to copy into its next call, and a
// CLI line named the nodes holding an admission as `[node-a node-b]`. Every
// one of those is a closed set a reader picks from or a list they read, and
// neither reader should have to know what Go's brackets mean.
//
// So a set goes through [Or] or [And], each member quoted as %q quotes the
// value beside it ("kind %q is not one of %s"), so the value a reader typed
// and the values they may type read alike. A member that is not a value (a
// command, a phrase, a field path written as the file writes it) is a [Said],
// joined the same way and written as it is. Config's warnings and the query
// surface each had a helper of their own for this (the second listed a set
// unquoted with commas, so "one of day, week, and \"hour\" isn't" read as a
// set of three), a dozen packages joined a list by hand, and the rest of the
// tree printed brackets; [Or] and [And] are the one copy now.
//
// # The gate
//
// A rule nobody checks decays, so this package's suite walks every package of
// the module with its types and refuses any format call whose verb prints a
// slice, an array or a map, and any such operand of a print that takes no
// format (fmt.Sprint and its kin, which print every operand as %v does). It
// refuses a list joined by hand into a sentence too: a strings.Join whose
// separator is a conjunction, one written beside one ("a, b" + " or " +
// "c"), one that joins all but the last member, and a closed set joined by
// commas after "one of", "(want" or "offers", which are the shapes every copy
// of [Or] the tree carried took, whether the strings.Join sits beside the
// sentence or inside a function of the module that returns it (config had
// one, names()). A value whose type says how it prints, through a String,
// Error or Format method, is that type's own business, and a byte slice
// under %s, %q or %x is text. It runs go list for the export data the type
// checker reads, because the shape of an operand is a property of its type
// and not of its spelling: `artifacts.Kinds` and `AuthorKinds()` look like
// any other identifier and call.
//
// A LEAF over the standard library, held to it by a test, because its readers
// sit at every height — the identity vocabulary and config, which import only
// leaves, as much as the domains and the API.
package words

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Word is what a set this package says is made of: text, said quoted, or a
// whole number, said as its digits.
//
// NOT ANY VALUE, because a set of structs said through fmt is the Go shape
// this package exists to keep out of a sentence, and a caller with one has to
// decide what each member is called.
type Word interface {
	~string | ~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

// Or is set as the alternatives it offers: "a", "b" or "c". It is the closed
// set a value has to come from ("pass one of %s").
func Or[S ~[]E, E Word](set S) string { return list(set, "or") }

// And is set as every member of it: "a", "b" and "c". It is a list a reader
// reads whole ("%s still hold an admission").
func And[S ~[]E, E Word](set S) string { return list(set, "and") }

// none is what an empty set is said as. A closed set a value comes from is
// never empty, and a list read whole can be — the keys a keyring holds, the
// nodes that did not answer — where "none" finishes the sentence and an empty
// string would end it mid-word.
const none = "none"

func list[S ~[]E, E Word](set S, conjunction string) string {
	said := make([]string, len(set))
	for i, member := range set {
		said[i] = word(member)
	}
	switch len(said) {
	case 0:
		return none
	case 1:
		return said[0]
	case 2:
		return said[0] + " " + conjunction + " " + said[1]
	default:
		return strings.Join(said[:len(said)-1], ", ") + " " + conjunction + " " + said[len(said)-1]
	}
}

// Said is a member its caller has already said as the sentence says it, which
// [Or], [And] and [AndMore] write as it is: a command in backticks, a
// variable reference written `${NAME}`, a phrase ("the Slack integration to
// mint ${TOKEN}"), a name a screen prints capitalised.
//
// A VALUE IS QUOTED and a Said is not, because the two are different things
// in a sentence: a value is what a reader typed or may type, quoted so it
// reads like the value beside it, while a phrase is already words and a
// command already carries the quoting its reader copies. Every list of those
// was joined by hand before this type, each with its own idea of the last
// comma, and the gate in this package's suite refuses one joined by hand now.
type Said string

// word is one member as a sentence says it. A named string type keeps its
// spelling, through reflect rather than a conversion, because a type
// parameter constrained to ~string and to the integers cannot be converted to
// either alone; a [Said] member is written as its caller said it.
func word[E Word](member E) string {
	if said, ok := any(member).(Said); ok {
		return string(said)
	}
	if v := reflect.ValueOf(member); v.Kind() == reflect.String {
		return strconv.Quote(v.String())
	}
	return fmt.Sprint(member)
}

// AndMore is set as [And] says it when it has at most shown members, and
// otherwise as its first shown and how many more: "a", "b" and 3 more. It is
// a list read whole that may be too long to read whole, such as the objects a
// backup could not reach.
func AndMore[S ~[]E, E Word](set S, shown int) string {
	if shown < 1 {
		shown = 1
	}
	if len(set) <= shown {
		return And(set)
	}
	said := make([]string, shown)
	for i, member := range set[:shown] {
		said[i] = word(member)
	}
	return fmt.Sprintf("%s and %d more", strings.Join(said, ", "), len(set)-shown)
}
