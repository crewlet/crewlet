package catalog

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// SelectArgument is the read argument naming the fields of an answer the model
// reads, applied by [Select] before the answer is shaped.
const SelectArgument = "select"

// The bounds of a selection: how many paths, how long each, and how long a
// list route's cursor may be.
const (
	SelectMax       = 32
	selectPathBytes = 200
	cursorBytes     = 512
)

// selectArgument is the select argument as the model is told it.
var selectArgument = &schema{kind: "array", maxItems: SelectMax,
	description: "Optional: the fields of the answer you need, as dotted paths " +
		"(\"items.*.key\", \"items.*.flow.state\", \"next_cursor\"). `*` is every element " +
		"of a list or every key of an object; a list is also walked when a path names " +
		"a field of its elements. Only those fields reach you, so a wide answer fits.",
	items: &schema{kind: "string", maxLen: selectPathBytes}}

// selectSegment is one segment of a select path: a field name, `*` or an
// index. Compiled at first use, not at init: Gitlet's push hook is this binary,
// and pays every package's init (cmd/crewlet's hookinit_test.go).
var selectSegment = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(\*|[A-Za-z0-9_\-]+)$`) })

// Selection is a read call's select paths, each split into its segments, or
// nil when it names none. A path that is empty, too long or holds a segment
// that is not a field name, `*` or an index is refused.
func Selection(args Args) ([][]string, error) {
	raw, given := args[SelectArgument]
	if !given || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, errors.New("select is a list of dotted field paths")
	}
	if len(list) > SelectMax {
		return nil, fmt.Errorf("select names %d paths and takes at most %d", len(list),
			SelectMax)
	}
	out := make([][]string, 0, len(list))
	for _, v := range list {
		path, ok := v.(string)
		if !ok || strings.TrimSpace(path) == "" || len(path) > selectPathBytes {
			return nil, fmt.Errorf("select holds %v, which is not a dotted field path of at "+
				"most %d characters", v, selectPathBytes)
		}
		segments := strings.Split(strings.TrimSpace(path), ".")
		for _, s := range segments {
			if !selectSegment().MatchString(s) {
				return nil, fmt.Errorf("select path %q holds the segment %q; a segment is a "+
					"field name, * or an index", path, s)
			}
		}
		out = append(out, segments)
	}
	return out, nil
}

// Select is the part of doc the paths name, in doc's own shape: an object
// keeps only the keys a path names, a list keeps every element, each narrowed
// the same way. A path naming nothing in doc adds nothing; ok is false when no
// path named anything.
func Select(doc any, paths [][]string) (any, bool) {
	if len(paths) == 0 {
		return doc, true
	}
	return project(doc, paths)
}

// project narrows v to paths, each relative to v.
func project(v any, paths [][]string) (any, bool) {
	for _, p := range paths {
		if len(p) == 0 {
			// A PATH THAT ENDS HERE takes v whole.
			return v, true
		}
	}
	switch t := v.(type) {
	case map[string]any:
		next := map[string][][]string{}
		for _, p := range paths {
			keys := []string{p[0]}
			if p[0] == "*" {
				keys = slices.Collect(maps.Keys(t))
			}
			for _, k := range keys {
				if _, held := t[k]; held {
					next[k] = append(next[k], p[1:])
				}
			}
		}
		out := map[string]any{}
		for k, rest := range next {
			if got, ok := project(t[k], rest); ok {
				out[k] = got
			}
		}
		return out, len(out) > 0
	case []any:
		// AN INDEX OR `*` names elements; any other segment is a field of
		// every element, so "items.key" reads as "items.*.key".
		var each [][]string
		picked := map[int][][]string{}
		for _, p := range paths {
			if i, err := strconv.Atoi(p[0]); err == nil {
				if i >= 0 && i < len(t) {
					picked[i] = append(picked[i], p[1:])
				}
				continue
			}
			if p[0] == "*" {
				each = append(each, p[1:])
				continue
			}
			each = append(each, p)
		}
		out := make([]any, 0, len(t))
		found := false
		for i, e := range t {
			got, ok := project(e, slices.Concat(each, picked[i]))
			if ok {
				found = true
				out = append(out, got)
			} else if len(each) > 0 {
				// A ROW THAT HOLDS NONE OF THE FIELDS stays a row, so the
				// list keeps its length and its order.
				out = append(out, map[string]any{})
			}
		}
		return out, found
	default:
		return nil, false
	}
}

// topFields are the top-level keys of an answer, sorted, for telling the model
// what a selection could have named.
func topFields(doc any) []string {
	if m, ok := doc.(map[string]any); ok {
		return slices.Sorted(maps.Keys(m))
	}
	if l, ok := doc.([]any); ok && len(l) > 0 {
		if m, ok := l[0].(map[string]any); ok {
			return slices.Sorted(maps.Keys(m))
		}
	}
	return nil
}
