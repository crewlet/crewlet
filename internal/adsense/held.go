package adsense

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// masks are the update mask paths Google documents for each patch, in the
// body's camelCase: every field the method changes. Any other path Google
// might read in a way the engine does not class.
var masks = map[string][]string{
	adUnitPatchOp: {"displayName", "state", adSettingsField, adSettingsField + ".size", adSettingsField + ".type"},
	"adsense.accounts.adclients.customchannels.patch": {"displayName", "active"},
	"adsenseplatform.accounts.platforms.groups.patch": {"description"},
	childSitePatchOp: {"platformGroup"},
}

// personalField is a platform event's person: an email and a billing
// address, a third party's personal data (package doc).
const personalField = "eventInfo"

// maskHolds refuses a patch whose update mask the engine cannot read: none
// (Google then replaces the whole resource), or a path Google does not
// document for the method.
func maskHolds(op growth.Operation, mask string) error {
	paths := maskPaths(mask)
	if len(paths) == 0 {
		return refusal(growth.RefusedInvalid, "a change names the fields it sets in updateMask; without one "+
			"Google replaces the whole resource. No vendor was reached.")
	}
	for _, p := range paths {
		if field, ok := maskField(p); !ok || !slices.Contains(masks[op.ID], field) {
			return refusal(growth.RefusedInvalid, fmt.Sprintf("updateMask path %q is not one Google documents "+
				"for %s; give %s. No vendor was reached.", p, op.ID, strings.Join(masks[op.ID], ", ")))
		}
	}
	return nil
}

// maskSegment is one segment of a mask path: snake_case or camelCase, which
// Google reads alike.
var maskSegment = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^([a-z][a-z0-9]*(_[a-z0-9]+)*|[a-z][a-zA-Z0-9]*)$`)
})

// maskField is a mask path as the body's camelCase field, false for a path
// of another shape.
func maskField(path string) (string, bool) {
	segments := strings.Split(path, ".")
	for i, s := range segments {
		if !maskSegment().MatchString(s) {
			return "", false
		}
		segments[i] = camel(s)
	}
	return strings.Join(segments, "."), true
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
			if found := snakeKey(t[k]); found != "" {
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

// prepared builds call's request, holds it to the seat's scope, reads its
// target and classes it against that. Its request is kept on a later
// failure, for the record of it.
func (p *Product) prepared(ctx context.Context, call growth.Call) (googlegrowth.Held, growth.Class, error) {
	req, _, err := p.requestOf(call)
	if err != nil {
		return googlegrowth.Held{}, "", err
	}
	if req.Reads() {
		return googlegrowth.Held{}, "", refusal(growth.RefusedInvalid, call.Tool+" only reads AdSense, so it is "+
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
// changes, a value under a rule's field (an ad unit's size) being the rule's.
func classOf(req googlegrowth.Request, before []byte) growth.Class {
	if len(req.Op.Fields) == 0 {
		return req.Op.Class
	}
	var current map[string]any
	_ = json.Unmarshal(before, &current)
	changes := req.Changes(current)
	for i := range changes {
		for _, r := range req.Op.Fields {
			if strings.HasPrefix(changes[i].Field, r.Field+".") {
				changes[i].Field = r.Field
			}
		}
	}
	return growth.Classify(req.Op, changes)
}

// before is the target's live state a write changes, read by its getter and
// encoded with sorted keys, so two reads of an unchanged target bind alike:
// none for a create but a platform sub-account's (operations.go, getters).
func (p *Product) before(ctx context.Context, handle string, req googlegrowth.Request) ([]byte, error) {
	id, ok := getters[req.Op.ID]
	if !ok {
		return nil, nil
	}
	target, err := p.read(ctx, handle, id, req.Params)
	if err != nil {
		return nil, err
	}
	if req.Op.ID == childSitePatchOp {
		// A SITE'S MOVE IS A CHANGE OF REVENUE SHARE: both groups' shares, read
		// now, are what a person approves and what the approval binds to.
		from, _ := target["platformGroup"].(string)
		to, _ := req.Body["platformGroup"].(string)
		share := map[string]any{}
		for side, name := range map[string]string{"before": from, "after": to} {
			group, err := p.group(ctx, handle, req, name)
			if err != nil {
				return nil, err
			}
			share[side] = group["revshareMillipercent"]
		}
		target[revenueShareField] = share
	}
	return json.Marshal(target)
}

// revenueShareField is where a site's before-state keeps the revenue shares
// of the group it leaves and the group it joins.
const revenueShareField = "revenueShare"

// read is the resource the read id names on params, as Google answers it.
func (p *Product) read(ctx context.Context, handle, id string, params map[string]string) (map[string]any, error) {
	op, _ := p.Catalog().Operation(id)
	path := map[string]string{}
	for name, v := range params {
		if strings.Contains(op.Path, "{"+name+"}") {
			path[name] = v
		}
	}
	get, err := newRequest(op, path, nil, nil)
	if err != nil {
		return nil, err
	}
	raw, err := p.client.Send(ctx, handle, get)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome,
			Err: fmt.Errorf("adsense: decode %s: %w", get.Target(), err)}
	}
	return out, nil
}

// group is the platform group name names, of the site's own platform; none
// for a site in no group.
func (p *Product) group(ctx context.Context, handle string, req googlegrowth.Request,
	name string) (map[string]any, error) {
	if name == "" {
		return nil, nil
	}
	prefix := "accounts/" + req.Params["accountsId"] + "/platforms/" + req.Params["platformsId"] + "/groups/"
	id, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return nil, refusal(growth.RefusedInvalid, fmt.Sprintf("the site's group %q is not one of its "+
			"platform's. No vendor was changed.", name))
	}
	params := maps.Clone(req.Params)
	params["groupsId"] = id
	return p.read(ctx, handle, "adsenseplatform.accounts.platforms.groups.get", params)
}

// diffOf is how a request of op reads what it changes: nothing for a read,
// the update mask's fields that differ for a patch, and every field the body
// sets for a create or an act, a person's data as its digest.
func diffOf(op growth.Operation, body map[string]any, mask string) func(map[string]any) []growth.Change {
	switch {
	case op.Class == growth.ClassRead:
		return func(map[string]any) []growth.Change { return nil }
	case op.Method == "PATCH":
		return func(before map[string]any) []growth.Change {
			var out []growth.Change
			for _, path := range maskPaths(mask) {
				field, _ := maskField(path)
				b, a := googlegrowth.Lookup(before, field), googlegrowth.Lookup(body, field)
				if !reflect.DeepEqual(b, a) {
					out = append(out, growth.Change{Field: field, Before: b, After: a})
				}
			}
			share := googlegrowth.Lookup(before, revenueShareField)
			if b, a := googlegrowth.Lookup(before, revenueShareField+".before"),
				googlegrowth.Lookup(before, revenueShareField+".after"); share != nil && !reflect.DeepEqual(b, a) {
				out = append(out, growth.Change{Field: "revshareMillipercent", Before: b, After: a})
			}
			return out
		}
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

// withheld is a copy of body with a person's data replaced by its digest:
// what a change, a question or the record may say of it.
func withheld(body map[string]any) map[string]any {
	if _, set := body[personalField]; !set {
		return body
	}
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	raw, _ := json.Marshal(body[personalField])
	sum := sha256.Sum256(raw)
	out[personalField] = "sha256:" + hex.EncodeToString(sum[:])
	return out
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
