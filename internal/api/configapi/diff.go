package configapi

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// The structural diff behind GET /config/revisions/{id}/diff.
//
// A LINE diff was the alternative and it is the wrong tool: the stored form is
// JSON produced by marshalling a struct, so re-ordering a map or adding a
// field with a default rewrites lines that mean nothing to a reader. What an
// operator asks is "what changed about the company", and that question is
// answered by paths and values.

// Change is one difference between two documents.
type Change struct {
	// Path is dotted, with list positions as [n]: roles[2].llm,
	// integrations.github.webhook_secret.
	Path string `json:"path"`

	// Kind is added, removed or changed.
	Kind string `json:"kind"`

	// From and To are the values on each side. Absent on the side where
	// the path does not exist, which is what makes added and removed
	// readable without consulting Kind.
	From any `json:"from,omitempty"`
	To   any `json:"to,omitempty"`
}

// The three kinds.
const (
	KindAdded   = "added"
	KindRemoved = "removed"
	KindChanged = "changed"
)

// MaxChanges bounds the diff ONE ANSWER carries — never the comparison.
//
// The budget being spent is the transport's rather than the differ's, so the
// cut belongs to whoever is rendering: [Service.Diff] answers over an HTTP
// body and a socket frame, so it cuts to this and reports `changes_total`
// beside the listing; `crewlet config diff` writes to a terminal, which has
// no such budget and a pager, so it prints every change [Changes] found.
//
// 500 sits above a whole-document rewrite of a real company. The example
// company in examples/ has 401 leaves and serializes to 40 KB of JSON, so a
// diff that changed every one of them is ~110 KB and still arrives complete.
// What the cap is for is the document several times that size, where one
// change per leaf makes an answer a download — and it is REPORTED rather
// than silent, because a diff that quietly stopped would be read as "that is
// all that changed".
const MaxChanges = 500

// Changes compares two documents as they are STORED, oldest first, and reports
// every value REDACTED.
//
// COMPLETE: every difference it found, however many that is. What to do about
// a diff longer than anybody reads is a property of where the answer is going
// rather than of the comparison — see [MaxChanges] — and the walk builds the
// whole list before it can sort it anyway, so cutting in here bought no work
// and cost the one caller with no size limit its answer.
//
// # Compared unredacted, reported redacted
//
// Reporting raw values would put the old and the new value of a rotated
// credential in one response, which is strictly worse than the read this
// surface already refuses to serve. Comparing redacted documents instead, as
// this once did, is blind: every literal credential masks to the same marker
// on both sides, so rotating one, or replacing one with a `${VAR}` that embeds
// a literal, diffed to nothing, and a diff is exactly where an operator looks
// to confirm a rotation landed. So each side is held in both forms, the
// comparison reads the stored values, and every value a change carries comes
// from the redacted copy at the same place. A rotated credential reads as a
// change from the marker to the marker: that it changed, and never what to.
func Changes(from, to *config.Company) ([]Change, error) {
	before, err := sideOf(from)
	if err != nil {
		return nil, err
	}
	after, err := sideOf(to)
	if err != nil {
		return nil, err
	}
	var out []Change
	compare("", before, after, &out)
	slices.SortFunc(out, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// document turns a config into the generic shape a diff walks.
//
// Through JSON rather than reflect, so the paths a reader sees are the field
// names they wrote — and so a field that does not survive serialization does
// not appear in a diff of documents that are compared as stored.
func document(cfg *config.Company) (map[string]any, error) {
	if cfg == nil {
		return map[string]any{}, nil
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("configapi: encode for diff: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("configapi: decode for diff: %w", err)
	}
	return out, nil
}

// side is one document of a diff, or one value inside it, in the two forms a
// diff needs: the stored value it compares, and the redacted value at the same
// place, which is the only one it reports.
type side struct{ compared, shown any }

// sideOf renders a company in both forms.
//
// The redacted copy has the stored one's exact shape, because redaction
// replaces a non-empty credential string with the non-empty marker and touches
// nothing else, so the two can be walked in lockstep.
func sideOf(cfg *config.Company) (side, error) {
	compared, err := document(cfg)
	if err != nil {
		return side{}, err
	}
	shown, err := document(cfg.Redact())
	if err != nil {
		return side{}, err
	}
	return side{compared: compared, shown: shown}, nil
}

// field steps into a mapping on both forms at once.
//
// A shown form that does not have the same shape yields nil, never the stored
// value: a diff that fell back to the unredacted side would publish exactly
// what the redacted side exists to hide.
func (s side) field(key string) side {
	compared, _ := s.compared.(map[string]any)
	shown, _ := s.shown.(map[string]any)
	return side{compared: compared[key], shown: shown[key]}
}

// item steps into a list on both forms at once, with the same rule as field.
func (s side) item(i int) side {
	out := side{}
	if compared, ok := s.compared.([]any); ok && i < len(compared) {
		out.compared = compared[i]
	}
	if shown, ok := s.shown.([]any); ok && i < len(shown) {
		out.shown = shown[i]
	}
	return out
}

// compare walks two values in parallel, appending what differs.
func compare(path string, before, after side, out *[]Change) {
	switch left := before.compared.(type) {
	case map[string]any:
		right, ok := after.compared.(map[string]any)
		if !ok {
			*out = append(*out, Change{Path: path, Kind: KindChanged, From: before.shown, To: after.shown})
			return
		}
		for _, key := range union(left, right) {
			_, hasA := left[key]
			_, hasB := right[key]
			switch {
			case !hasA:
				*out = append(*out, Change{Path: join(path, key), Kind: KindAdded, To: after.field(key).shown})
			case !hasB:
				*out = append(*out, Change{Path: join(path, key), Kind: KindRemoved, From: before.field(key).shown})
			default:
				compare(join(path, key), before.field(key), after.field(key), out)
			}
		}
	case []any:
		right, ok := after.compared.([]any)
		if !ok {
			*out = append(*out, Change{Path: path, Kind: KindChanged, From: before.shown, To: after.shown})
			return
		}
		// BY POSITION, which is the only correspondence a JSON list has.
		// Matching by an identity field would be right for roles and
		// wrong for api_keys, and guessing which is which per path is how
		// a diff comes to describe a change nobody made.
		for i := range max(len(left), len(right)) {
			at := index(path, i)
			switch {
			case i >= len(left):
				*out = append(*out, Change{Path: at, Kind: KindAdded, To: after.item(i).shown})
			case i >= len(right):
				*out = append(*out, Change{Path: at, Kind: KindRemoved, From: before.item(i).shown})
			default:
				compare(at, before.item(i), after.item(i), out)
			}
		}
	default:
		if !equal(before.compared, after.compared) {
			*out = append(*out, Change{Path: path, Kind: KindChanged, From: before.shown, To: after.shown})
		}
	}
}

// union is every key of either side, sorted, so two readers of one diff see
// the same order.
func union(a, b map[string]any) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for key := range a {
		seen[key] = struct{}{}
	}
	for key := range b {
		seen[key] = struct{}{}
	}
	keys := slices.Sorted(maps.Keys(seen))
	return keys
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func index(path string, i int) string {
	return path + "[" + strconv.Itoa(i) + "]"
}

// equal compares two leaves. JSON numbers are all float64 here, so this is a
// value comparison rather than a type-aware one.
func equal(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a == b
}
