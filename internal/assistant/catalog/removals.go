package catalog

import (
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/words"
)

// A REPLACE NEVER REMOVES BY OMISSION (ADR-0055).
//
// A route that replaces the whole object at its address ([Route.Replaces])
// removes every field its body leaves out. A model rebuilding a unit to add
// one seat could leave out the unit's project and space, and the card drew
// that as two struck-through rows under twenty rows of the new seat: a person
// approving "add a seat" removed them without meaning to. So a removal is a
// decision the call states, never a side effect of what it forgot:
//
//   - The preview compares the body with the representation it read and
//     lists every field the body would remove or empty ([removalsOf]).
//   - Each one must be named in the call's `removes`; one that is not refuses
//     the step before any card, in plain words, so the model either keeps the
//     field or the person's request is what removes it ([undeclaredRemoval]).
//     A named path the change does not remove is refused too, so the card
//     never warns of a removal that will not happen.
//   - A declared removal leads the card as its own warning ([Preview.Removes]),
//     ahead of every other row.
//
// The digest binds it without a field of its own: the removals are a function
// of the body, which the request digest binds, and of the representation the
// source revision names, which the approval re-reads. The `removes` argument
// only admits the call; what the card warns of is computed, never echoed.

// RemovesArgument is the one argument a write takes that it never sends: what
// the call says it removes from what it replaces (the generic change's paths,
// a chat room's members). It admits a removal the preview found, and the
// request makes the removal by leaving it out.
const RemovesArgument = "removes"

// removesArgument is the generic change's `removes`: the paths the call
// intends to remove from what the route replaces.
var removesArgument = names("Only for a route that replaces the whole object (a PUT "+
	"describe_api marks as replacing): each field the change removes from the object as "+
	"it is now, as a path of its keys, a list member by its handle, id, key or name: "+
	"[\"project\", \"roles.qa\"]. A field the body leaves out is removed, so list one here "+
	"only when the person asked for it to go; otherwise send it in the body as it is. "+
	"The person sees every removal at the top of the card.", 64)

// removalsOf are the fields of before that after removes or empties, as paths
// ([join]): a key after leaves out, a value it empties, a list member it drops
// by identity. A top-level field naming the address itself (a unit's `id`
// equal to its path's `{id}`) is the route's to fill, so leaving it out
// removes nothing. Sorted, each once.
func removalsOf(before, after any, address map[string]string) []string {
	var out []string
	walkRemovals(&out, "", before, after, address)
	slices.Sort(out)
	return slices.Compact(out)
}

func walkRemovals(out *[]string, field string, before, after any, address map[string]string) {
	if empty(before) {
		return
	}
	if empty(after) {
		*out = append(*out, field)
		return
	}
	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok {
			return
		}
		for _, k := range slices.Sorted(maps.Keys(b)) {
			is, has := a[k]
			if !has && field == "" && addressed(b[k], address) {
				continue
			}
			if !has {
				if !empty(b[k]) {
					*out = append(*out, join(field, k))
				}
				continue
			}
			walkRemovals(out, join(field, k), b[k], is, nil)
		}
	case []any:
		a, ok := after.([]any)
		if !ok {
			return
		}
		was, wasKeyed := keyed(b)
		is, isKeyed := keyed(a)
		if !wasKeyed || !isKeyed {
			// A list of plain values, or of records without one identity,
			// is one value: shorter is a change the card draws, empty is the
			// removal caught above.
			return
		}
		for _, e := range was {
			if match := find(is, e.id); match == nil {
				*out = append(*out, join(field, e.id))
			} else {
				walkRemovals(out, join(field, e.id), e.value, match.value, nil)
			}
		}
	}
}

// addressed reports a value the request's own address states: one of its
// path values, which the route fills from the path.
func addressed(v any, address map[string]string) bool {
	s, ok := v.(string)
	if !ok || s == "" {
		return false
	}
	for _, value := range address {
		if value == s {
			return true
		}
	}
	return false
}

// empty reports a value that holds nothing: absent, null, "", [] or {}.
func empty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// covered reports a removal path one of declared names: the path itself, or
// a path it lies under ("roles.qa" covers "roles.qa.goal").
func covered(path string, declared []string) bool {
	for _, d := range declared {
		if path == d || strings.HasPrefix(path, d+".") {
			return true
		}
	}
	return false
}

// removalCheck holds a replace's removals to the call's `removes`: every
// removal declared, every declaration a removal. It answers the removals the
// card warns of, or the step's refusal, which names EVERY removal the call
// does not declare and every declaration the change does not make, in one
// refusal (ADR-0059).
func removalCheck(route Route, method string, before, after any, address map[string]string,
	declared []string, title string) ([]string, error) {

	if len(declared) > 0 && (!route.Replaces || method != http.MethodPut) {
		return nil, &PreviewRefusal{Status: http.StatusUnprocessableEntity, Refusal: wire.Refusal{
			Code: removalCode,
			Message: route.Pattern + " does not replace a whole object, so `removes` " +
				"has nothing to declare.",
			Hint: "Leave `removes` out; this change's card shows what it changes.",
		}}
	}
	if !route.Replaces || method != http.MethodPut || before == nil {
		return nil, nil
	}
	removals := removalsOf(before, after, address)
	var undeclared, unmade []string
	for _, r := range removals {
		if !covered(r, declared) {
			undeclared = append(undeclared, r)
		}
	}
	for _, d := range declared {
		if !slices.ContainsFunc(removals, func(r string) bool { return covered(r, []string{d}) }) {
			unmade = append(unmade, d)
		}
	}
	if len(undeclared) > 0 || len(unmade) > 0 {
		return nil, undeclaredRemoval(title, undeclared, unmade)
	}
	return removals, nil
}

// removalCode is the refusal code of a removal the call did not declare, or
// declared but did not make.
const removalCode = "undeclared_removal"

// undeclaredRemoval is the refusal of a replace that would remove paths the
// call did not declare, or that declares paths it would not remove, in the
// words the model acts on: both, every path of each, in one refusal.
func undeclaredRemoval(title string, undeclared, unmade []string) *PreviewRefusal {
	var said, hints []string
	if len(undeclared) > 0 {
		said = append(said, "This would remove "+inWords(undeclared)+" from "+title+".")
		hints = append(hints, "Include them in the body to keep them, or list them in "+
			"`removes` if the person asked for that.")
	}
	if len(unmade) > 0 {
		which := "which this change does not remove"
		switch {
		case len(unmade) == 2:
			which = "neither of which this change removes"
		case len(unmade) > 2:
			which = "none of which this change removes"
		}
		said = append(said, "`removes` names "+inWords(unmade)+", "+which+" from "+title+".")
		hints = append(hints, "List in `removes` only what the body leaves out or empties.")
	}
	return &PreviewRefusal{Status: http.StatusUnprocessableEntity, Refusal: wire.Refusal{
		Code: removalCode, Message: strings.Join(said, " "), Hint: strings.Join(hints, " "),
	}}
}

// inWords is a list of paths as a sentence says it: "project", "project and
// space", "project, space and lead". Each path is a [words.Said], written as
// the body writes it rather than quoted, because that is how this refusal's
// other sentences name a path and how the model writes it back in `removes`.
func inWords(paths []string) string {
	said := make([]words.Said, len(paths))
	for i, p := range paths {
		said[i] = words.Said(p)
	}
	return words.And(said)
}
