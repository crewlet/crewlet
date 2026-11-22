package admob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// A mediation line's state, and the canonical field its changes are classed
// by (operations.go).
const (
	linesField     = "mediationGroupLines"
	lineStateField = linesField + ".state"
	lineEnabled    = "ENABLED"
	lineDisabled   = "DISABLED"
	lineRemoved    = "REMOVED"
)

// mapFields are the body's fields that are maps, whose keys are IDs or a third
// party's configuration keys, never field names.
var mapFields = []string{linesField, "adUnitMappings", secretField}

// snakeKey is the first field name in body spelled with an underscore, ""
// for none. Google takes a field by its proto name as well as its JSON name,
// and a change is classed by the JSON name, so a proto name would pass a
// field's rule by.
func snakeKey(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(t) {
			if strings.Contains(k, "_") {
				return k
			}
			inner := t[k]
			if slices.Contains(mapFields, k) {
				// A MAP'S KEYS ARE NOT FIELDS; its values may hold fields.
				m, _ := inner.(map[string]any)
				for _, id := range sortedKeys(m) {
					if found := snakeKey(m[id]); found != "" {
						return found
					}
				}
				continue
			}
			if found := snakeKey(inner); found != "" {
				return found
			}
		}
	case []any:
		for _, inner := range t {
			if found := snakeKey(inner); found != "" {
				return found
			}
		}
	}
	return ""
}

// lineOutputs are a mediation line's fields Google sets alone, never a change.
var lineOutputs = []string{"id", "experimentVariant"}

// prepared builds call's request, holds it to the seat's scope, reads its
// target and classes it against that. Its request is kept on a later
// failure, for the record of it.
func (p *Product) prepared(ctx context.Context, call growth.Call) (googlegrowth.Held, growth.Class, error) {
	req, _, err := p.requestOf(call)
	if err != nil {
		return googlegrowth.Held{}, "", err
	}
	if req.Reads() {
		return googlegrowth.Held{}, "", refusal(growth.RefusedInvalid, call.Tool+" only reads AdMob, so it is "+
			"run, never prepared as a change. No vendor was reached.")
	}
	if err = p.inScope(call.Handle, req); err != nil {
		return googlegrowth.Held{Req: req}, "", err
	}
	before, err := p.before(ctx, call.Handle, req)
	if err != nil {
		return googlegrowth.Held{Req: req}, "", err
	}
	return googlegrowth.NewHeld(req, before), classOf(req, before), nil
}

// Prepare classes a write against its target as read now and builds what a
// person is asked where it is held.
func (p *Product) Prepare(ctx context.Context, call growth.Call) (growth.Hold, error) {
	h, class, err := p.prepared(ctx, call)
	if err != nil {
		return growth.Hold{}, err
	}
	return p.client.Hold(call, h, class), nil
}

// Card is a held call's decision card as its target reads now.
func (p *Product) Card(ctx context.Context, call growth.Call, binding string) (growth.Card, error) {
	h, class, err := p.prepared(ctx, call)
	if err != nil {
		return growth.Card{}, err
	}
	return p.client.Card(call, h, binding, class), nil
}

// Apply makes a prepared write once, reading its target again first and
// sending nothing where it moved since binding was taken.
func (p *Product) Apply(ctx context.Context, call growth.Call, binding string) (any, error) {
	h, class, err := p.prepared(ctx, call)
	if class == "" {
		tool, _ := p.Catalog().Tool(call.Tool)
		class = tool.Class
	}
	return p.client.Apply(ctx, call, h, err, binding, class, nil)
}

// classOf is the class req comes to on its target as before reads: its
// operation's, or for a field-classed write the highest class of what it
// changes, each change named by its field with no map key (a line's state is
// one rule whichever line it is).
func classOf(req googlegrowth.Request, before []byte) growth.Class {
	if len(req.Op.Fields) == 0 {
		return req.Op.Class
	}
	var current map[string]any
	_ = json.Unmarshal(before, &current)
	changes := req.Changes(current)
	for i := range changes {
		changes[i].Field = canonical(changes[i].Field)
	}
	return growth.Classify(req.Op, changes)
}

// mapKey is a map entry's key in a field's name, as a mask writes it.
var mapKey = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\["(?:[^"\\]|\\.)*"\]`) })

// canonical is a field's name without its map keys.
func canonical(field string) string { return mapKey().ReplaceAllString(field, "") }

// before is the target's live state a write changes: a mediation group, read
// for a change to it or its A/B experiment; none for a create, which acts on
// nothing that exists yet.
func (p *Product) before(ctx context.Context, handle string, req googlegrowth.Request) ([]byte, error) {
	group := req.Params["mediationGroupsId"]
	if group == "" {
		return nil, nil
	}
	return p.mediationGroup(ctx, handle, req.Account, group)
}

// mediationGroup is one group as Google lists it: the API has no get, so it
// is the listing filtered to its ID, encoded with sorted keys so two reads of
// an unchanged group bind alike.
func (p *Product) mediationGroup(ctx context.Context, handle, account, id string) ([]byte, error) {
	op, _ := p.Catalog().Operation("admob.accounts.mediationGroups.list")
	req, err := newRequest(op, map[string]string{"accountsId": account},
		map[string]string{"filter": fmt.Sprintf("IN(MEDIATION_GROUP_ID, %q)", id), "pageSize": "10"}, nil)
	if err != nil {
		return nil, err
	}
	raw, err := p.client.Send(ctx, handle, req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Groups []map[string]any `json:"mediationGroups"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome,
			Err: fmt.Errorf("admob: decode mediation group %s: %w", id, err)}
	}
	for _, g := range resp.Groups {
		if g["mediationGroupId"] == id {
			return json.Marshal(g)
		}
	}
	return nil, refusal(growth.RefusedNotFound, "account "+account+" has no mediation group "+id+
		" this sign-in can see. No vendor was changed.")
}

// diffOf is how a request of op reads what it changes: nothing for a read, the
// update mask's paths for a patch, and every field the body sets for a create.
func diffOf(op growth.Operation, body map[string]any, mask string) func(map[string]any) []growth.Change {
	switch {
	case op.Class == growth.ClassRead:
		return func(map[string]any) []growth.Change { return nil }
	case op.Method == "PATCH":
		return func(before map[string]any) []growth.Change { return maskChanges(mask, before, body) }
	}
	return func(map[string]any) []growth.Change {
		var out []growth.Change
		flatten("", withheld(body), &out)
		return out
	}
}

// flatten appends every leaf v sets, by its dotted field: objects are walked,
// and a list is one value.
func flatten(prefix string, v any, out *[]growth.Change) {
	obj, ok := v.(map[string]any)
	if !ok {
		if v != nil {
			*out = append(*out, growth.Change{Field: prefix, After: v})
		}
		return
	}
	for _, k := range sortedKeys(obj) {
		field := k
		if prefix != "" {
			field = prefix + "." + k
		}
		flatten(field, obj[k], out)
	}
}

// withheld is a copy of v with every third party's configuration, at any
// depth (a batch's items too), replaced by its digest: what a change, a
// question or the record may say of it.
func withheld(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, inner := range t {
			if k == secretField {
				out[k] = digestOf(inner)
				continue
			}
			out[k] = withheld(inner)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = withheld(t[i])
		}
		return out
	}
	return v
}

// digestOf is a value as the record may keep it: its SHA-256, never itself.
func digestOf(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// maskChanges are the changes a patch makes: each path of its update mask,
// with its value before and after (newRequest refused a path it cannot read).
// A whole mediation line is each of its fields that differs, and the whole
// line map each line's, a line it leaves out being removed, so each change is
// classed on its own.
func maskChanges(mask string, before, after map[string]any) []growth.Change {
	var out []growth.Change
	for _, path := range maskPaths(mask) {
		steps, _ := parseMask(path)
		label := labelOf(steps)
		b, a := walk(before, steps), walk(after, steps)
		switch {
		case len(steps) == 1 && steps[0].name == linesField && steps[0].key != "":
			out = append(out, lineChanges(label, b, a)...)
		case len(steps) == 1 && steps[0].name == linesField:
			bl, _ := b.(map[string]any)
			al, _ := a.(map[string]any)
			for _, id := range sortedKeys(union(bl, al)) {
				line := labelOf([]step{{name: linesField, key: id}})
				if _, kept := al[id]; !kept {
					// A LINE THE NEW MAP LEAVES OUT IS REMOVED by the replacement.
					was, _ := bl[id].(map[string]any)
					out = append(out, growth.Change{Field: line + ".state", Before: was["state"], After: lineRemoved})
					continue
				}
				out = append(out, lineChanges(line, bl[id], al[id])...)
			}
		case !reflect.DeepEqual(b, a):
			out = append(out, growth.Change{Field: label, Before: b, After: a})
		}
	}
	return out
}

// maskPaths are an update mask's non-empty paths.
func maskPaths(mask string) []string {
	var out []string
	for _, p := range strings.Split(mask, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// union is every key of a and b.
func union(a, b map[string]any) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// lineChanges are the fields of one mediation line that differ, before to after.
func lineChanges(label string, before, after any) []growth.Change {
	b, _ := before.(map[string]any)
	a, _ := after.(map[string]any)
	var out []growth.Change
	for _, k := range sortedKeys(union(b, a)) {
		if slices.Contains(lineOutputs, k) || reflect.DeepEqual(b[k], a[k]) {
			continue
		}
		// A FIELD THE NEW LINE LEAVES OUT IS NOT CHANGED: Google keeps it.
		if _, set := a[k]; !set {
			continue
		}
		out = append(out, growth.Change{Field: label + "." + k, Before: b[k], After: a[k]})
	}
	return out
}

// step is one segment of a mask path: a field, and a map key where indexed.
type step struct{ name, key string }

// maskStep is one segment: a snake_case name, optionally ["key"].
var maskStep = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^([a-z][a-z0-9_]*)(?:\["((?:[^"\\]|\\.)*)"\])?`)
})

// parseMask reads a mask path into its steps, names in the body's camelCase.
func parseMask(path string) ([]step, bool) {
	var out []step
	for path != "" {
		m := maskStep().FindStringSubmatch(path)
		if m == nil {
			return nil, false
		}
		out = append(out, step{name: camel(m[1]), key: m[2]})
		path = path[len(m[0]):]
		if path == "" {
			break
		}
		if path[0] != '.' {
			return nil, false
		}
		path = path[1:]
	}
	return out, len(out) > 0
}

// labelOf is a path as a change names it: camelCase, its keys kept.
func labelOf(steps []step) string {
	parts := make([]string, len(steps))
	for i, s := range steps {
		parts[i] = s.name
		if s.key != "" {
			parts[i] += fmt.Sprintf("[%q]", s.key)
		}
	}
	return strings.Join(parts, ".")
}

// walk is the value steps name in v, nil where absent.
func walk(v any, steps []step) any {
	cur := v
	for _, s := range steps {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[s.name]
		if s.key != "" {
			inner, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = inner[s.key]
		}
	}
	return cur
}

// camel is a snake_case name in camelCase.
func camel(snake string) string {
	parts := strings.Split(snake, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// sortedKeys are a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
