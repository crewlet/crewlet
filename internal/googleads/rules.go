package googleads

import (
	"encoding/json"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
)

// opKind is what one operation of a mutate does.
type opKind string

const (
	opCreate opKind = "create"
	opUpdate opKind = "update"
	opRemove opKind = "remove"
)

// operation is one operation of a mutate request, its field names in
// snake_case as the matrix and GAQL write them.
type operation struct {
	// service is its operation's type as GoogleAdsService.Mutate names it,
	// without `_operation`: "campaign", "ad_group_ad".
	service string
	kind    opKind
	// object is what a create or an update sets.
	object map[string]any
	// mask is an update's field paths, each to a leaf the object sets.
	mask []string
	// name is the resource an update or a remove acts on.
	name string
	// exempt marks an operation asking to serve what the policy system
	// flagged (exempt_policy_violation_keys, policy_validation_parameter).
	exempt bool
}

// state is a resource's live fields, snake_case and nested as GAQL answers
// them; nil where the engine read none.
type state map[string]any

// live is every resource the request's rules compare against, by resource
// name: the targets of its updates and removes, and what they point at.
type live map[string]state

// how is how a field rule compares a field's value before and after.
type how int

const (
	// always is hi for any change.
	always how = iota
	// raise is hi for a larger number or a cleared cap, lo for a smaller one.
	raise
	// lower is hi for a smaller number or a cleared value, lo for a larger one:
	// a target ROAS, whose lowering bids more.
	lower
	// on is hi for false to true, lo for true to false.
	on
	// off is hi for true to false, lo for false to true.
	off
	// toward is hi where the new value is one of values, lo otherwise.
	toward
	// earlier is hi for an earlier time, lo for a later one.
	earlier
	// later is hi for a later time or a cleared one, lo for an earlier one.
	later
	// grows is hi where a list loses an entry or changes one, lo where it only
	// gains: removing a cap or a restriction widens.
	grows
	// optsIn is hi where an asset automation setting turns OPTED_IN.
	optsIn
)

// fieldRule is the class of changing one field of an update.
type fieldRule struct {
	// path is the field, dotted snake_case; it covers the fields below it.
	path   string
	how    how
	hi, lo growth.Class
	values []string
}

// family is one service's rule (the matrix's FR-* rules): how each create,
// update and remove of its resource is classed.
type family struct {
	create func(o operation, l live) growth.Class
	remove func(o operation, l live) growth.Class
	fields []fieldRule
	// other is the class of an update to a field no rule names: the highest
	// the family allows, since a rule the engine cannot resolve asks most.
	other growth.Class
	// update decides a whole update first, where a field alone cannot (a
	// bidding strategy switched); "" leaves it to the fields.
	update func(o operation, l live) growth.Class
}

// classOf is what op comes to under f, read against l.
func (f family) classOf(op operation, l live) growth.Class {
	switch op.kind {
	case opCreate:
		return f.create(op, l)
	case opRemove:
		return f.remove(op, l)
	}
	out := growth.ClassRead
	if f.update != nil {
		out = f.update(op, l)
	}
	before := l[op.name]
	for _, path := range op.mask {
		c := f.other
		if r, ok := f.ruleFor(path); ok {
			c = r.classOf(lookup(before, path), lookup(op.object, path), before == nil)
		}
		out = higher(out, c)
	}
	return out
}

// ruleFor is the rule covering path: its own, or the nearest above it.
func (f family) ruleFor(path string) (fieldRule, bool) {
	best, found := fieldRule{}, false
	for _, r := range f.fields {
		if (path == r.path || strings.HasPrefix(path, r.path+".")) && len(r.path) >= len(best.path) {
			best, found = r, true
		}
	}
	return best, found
}

// classOf is the rule's class for a change from before to after; unread, a
// comparing rule is hi, since a change it cannot tell asks most.
func (r fieldRule) classOf(before, after any, unread bool) growth.Class {
	if r.how == always {
		return r.hi
	}
	if r.how == toward {
		if s, ok := after.(string); ok && slices.Contains(r.values, s) {
			return r.hi
		}
		return r.lo
	}
	if r.how == optsIn {
		return pick(optedIn(before, after), r.hi, r.lo)
	}
	if unread {
		return r.hi
	}
	switch r.how {
	case raise, lower:
		if after == nil {
			return r.hi // a cap or a target cleared
		}
		b, okB := number(before)
		a, okA := number(after)
		if !okB || !okA {
			return pick(before == nil, r.lo, r.hi) // a cap added where there was none narrows
		}
		cmp := a.Cmp(b)
		return pick((r.how == raise && cmp > 0) || (r.how == lower && cmp < 0), r.hi, r.lo)
	case on:
		return pick(truthy(after) && !truthy(before), r.hi, r.lo)
	case off:
		return pick(truthy(before) && !truthy(after), r.hi, r.lo)
	case earlier, later:
		b, okB := when(before)
		a, okA := when(after)
		if r.how == later && after == nil {
			return r.hi // an end cleared runs for ever
		}
		if !okA || !okB {
			return r.hi
		}
		return pick((r.how == earlier && a.Before(b)) || (r.how == later && a.After(b)), r.hi, r.lo)
	case grows:
		return pick(onlyGains(before, after), r.lo, r.hi)
	}
	return r.hi
}

// optedIn reports whether an asset automation list turns a type OPTED_IN that
// was not before.
func optedIn(before, after any) bool {
	was := map[string]string{}
	for _, e := range list(before) {
		m, _ := e.(map[string]any)
		was[str(m["asset_automation_type"])] = str(m["asset_automation_status"])
	}
	for _, e := range list(after) {
		m, _ := e.(map[string]any)
		if str(m["asset_automation_status"]) == "OPTED_IN" && was[str(m["asset_automation_type"])] != "OPTED_IN" {
			return true
		}
	}
	return false
}

// onlyGains reports whether after holds every entry of before, and more.
func onlyGains(before, after any) bool {
	b, a := list(before), list(after)
	if len(a) <= len(b) {
		return false
	}
	for _, e := range b {
		if !slices.ContainsFunc(a, func(x any) bool { return reflect.DeepEqual(x, e) }) {
			return false
		}
	}
	return true
}

// createStatus is a create's class by its status: PAUSED is operate, ENABLED
// or unset (which Google serves) is enabled.
func createStatus(enabled growth.Class) func(operation, live) growth.Class {
	return func(o operation, _ live) growth.Class {
		return pick(str(o.object["status"]) == "PAUSED", growth.ClassOperate, enabled)
	}
}

// fixed is a create or a remove of one class.
func fixed(c growth.Class) func(operation, live) growth.Class {
	return func(operation, live) growth.Class { return c }
}

// statusRule is a status set to ENABLED (enabled) or PAUSED (operate).
func statusRule(enabled growth.Class) fieldRule {
	return fieldRule{path: "status", how: toward, hi: enabled, lo: growth.ClassOperate, values: []string{"ENABLED"}}
}

// operates is a rule of operate for each path.
func operates(paths ...string) []fieldRule {
	return classed(growth.ClassOperate, paths...)
}

// classed is a rule of c for each path.
func classed(c growth.Class, paths ...string) []fieldRule {
	out := make([]fieldRule, 0, len(paths))
	for _, p := range paths {
		out = append(out, fieldRule{path: p, how: always, hi: c})
	}
	return out
}

// raises is a spend-on-raise, operate-on-lower rule for each path.
func raises(paths ...string) []fieldRule {
	return compared(raise, paths...)
}

// turnsOn is a spend-on-on, operate-on-off rule for each path.
func turnsOn(paths ...string) []fieldRule {
	return compared(on, paths...)
}

// compared is a spend/operate rule comparing by h for each path.
func compared(h how, paths ...string) []fieldRule {
	out := make([]fieldRule, 0, len(paths))
	for _, p := range paths {
		out = append(out, fieldRule{path: p, how: h, hi: growth.ClassSpend, lo: growth.ClassOperate})
	}
	return out
}

// higher is the higher of two classes.
func higher(a, b growth.Class) growth.Class {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

// pick is a when cond holds, else b.
func pick[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}

// lookup is a dotted field's value in an object, nil where absent.
func lookup(m map[string]any, path string) any {
	var cur any = m
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[part]
	}
	return cur
}

// number reads a JSON number or a decimal string (int64 travels as one).
func number(v any) (*big.Rat, bool) {
	switch t := v.(type) {
	case string:
		return new(big.Rat).SetString(strings.TrimSpace(t))
	case float64:
		return new(big.Rat).SetFloat64(t), true
	case json.Number:
		return new(big.Rat).SetString(t.String())
	}
	return nil, false
}

// truthy is a JSON boolean's truth, false where absent.
func truthy(v any) bool { b, _ := v.(bool); return b }

// str is a JSON string, "" where it is none.
func str(v any) string { s, _ := v.(string); return s }

// list is a JSON array, nil where it is none.
func list(v any) []any { l, _ := v.([]any); return l }

// when reads a Google Ads date time ("2026-11-21 00:00:00" or a date).
func when(v any) (time.Time, bool) {
	s := str(v)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
