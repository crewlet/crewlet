package googleads

import (
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/crewlet/crewlet/internal/growth"
)

// snake is a camelCase name in snake_case: campaignBudget, campaign_budget.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// camel is a snake_case name in camelCase, as REST's JSON writes it.
func camel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// canonical is a request body with every object key in snake_case, as the
// rules and GAQL name fields. Google takes a field by either spelling, so one
// named in both with two values is refused: which value Google keeps is not
// the engine's to guess, and the rules would read the other.
func canonical(v any, at string) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for _, k := range slices.Sorted(maps.Keys(t)) {
			key := snake(k)
			inner, err := canonical(t[k], at+key+".")
			if err != nil {
				return nil, err
			}
			if seen, ok := out[key]; ok && !reflect.DeepEqual(seen, inner) {
				return nil, refusal(growth.RefusedInvalid, fmt.Sprintf("the body names %s%s twice, in two "+
					"spellings with two values: name each field once. No vendor was reached.", at, key))
			}
			out[key] = inner
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, inner := range t {
			c, err := canonical(inner, fmt.Sprintf("%s%d.", at, i))
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	return v, nil
}

// snaked is a JSON value with every object key in snake_case: Google's own
// answers, and a request body [canonical] admitted.
func snaked(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, inner := range t {
			out[snake(k)] = snaked(inner)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, inner := range t {
			out[i] = snaked(inner)
		}
		return out
	}
	return v
}

// singular is a resource collection's name as one of its kind, snake_case:
// campaignBudgets is campaign_budget, adGroupCriteria ad_group_criterion.
func singular(collection string) string {
	s := snake(strings.TrimSpace(collection))
	switch {
	case strings.HasSuffix(s, "criteria"):
		return strings.TrimSuffix(s, "criteria") + "criterion"
	case strings.HasSuffix(s, "strategies"):
		return strings.TrimSuffix(s, "ies") + "y"
	case strings.HasSuffix(s, "s"):
		return strings.TrimSuffix(s, "s")
	}
	return s
}

// resourceName is a Google Ads resource name: customers/{id}/{collection}/{id}.
var resourceName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^customers/([0-9]{1,20})/([A-Za-z]+)/([A-Za-z0-9_~-]{1,200})$`)
})

// kindOf is a resource name's customer and its kind as GAQL names it
// (campaign, ad_group_criterion); ok is false for anything else.
func kindOf(name string) (customer, kind string, ok bool) {
	m := resourceName().FindStringSubmatch(name)
	if m == nil {
		return "", "", false
	}
	return m[1], singular(m[2]), true
}

// serviceOfMutate is the operation type a service's mutate method acts on:
// googleads.customers.campaignBudgets.mutate is campaign_budget,
// mutateRecommendationSubscription recommendation_subscription, and
// CustomerService's googleads.customers.mutate customer.
func serviceOfMutate(id string) (string, bool) {
	if id == "googleads.customers.mutate" {
		return "customer", true
	}
	rest, ok := strings.CutPrefix(id, "googleads.customers.")
	if !ok {
		return "", false
	}
	collection, verb, ok := strings.Cut(rest, ".")
	if !ok || !strings.HasPrefix(verb, "mutate") || strings.Contains(verb, ".") {
		return "", false
	}
	return singular(collection), true
}

// operationOf parses one operation list item: a create, an update with its
// mask or a remove, of service.
func operationOf(service string, raw any) (operation, bool) {
	m, _ := snaked(raw).(map[string]any)
	op := operation{service: service}
	if m["exempt_policy_violation_keys"] != nil || m["policy_validation_parameter"] != nil {
		op.exempt = true
	}
	switch {
	case m["create"] != nil:
		op.kind, op.object = opCreate, object(m["create"])
	case m["update"] != nil:
		op.kind, op.object = opUpdate, object(m["update"])
		op.name = str(op.object["resource_name"])
		for _, path := range strings.Split(str(m["update_mask"]), ",") {
			if path = strings.TrimSpace(path); path != "" {
				op.mask = append(op.mask, snake(path))
			}
		}
		if len(op.mask) == 0 {
			// NO MASK sets every field the object holds, each one changed.
			op.mask = leaves(op.object, "")
		}
	case m["remove"] != nil:
		op.kind, op.name = opRemove, str(m["remove"])
	default:
		return operation{}, false
	}
	return op, true
}

// object is a JSON object, an empty one where it is none.
func object(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// leaves is every leaf path of m below prefix, resource_name aside.
func leaves(m map[string]any, prefix string) []string {
	var out []string
	for k, v := range m {
		if prefix == "" && k == "resource_name" {
			continue
		}
		if inner, ok := v.(map[string]any); ok && len(inner) > 0 {
			out = append(out, leaves(inner, prefix+k+".")...)
			continue
		}
		out = append(out, prefix+k)
	}
	return out
}

// parse is every operation a call of op makes with body (in REST's JSON),
// each of its service; false where op holds no operations the engine reads.
// The field read is op's own: GoogleAdsService.Mutate and a batch job's
// staging take mutate_operations, a service's mutate operations.
func parse(op growth.Operation, body map[string]any) ([]operation, bool) {
	m, _ := snaked(body).(map[string]any)
	if ruleOf()[op.ID] == "FR-MUTATE" {
		// Each item names its own operation type.
		var out []operation
		for _, item := range list(m["mutate_operations"]) {
			for key, inner := range object(item) {
				service := strings.TrimSuffix(key, "_operation")
				o, ok := operationOf(service, inner)
				if !ok {
					o = operation{service: service, kind: opCreate, object: object(inner)}
				}
				out = append(out, o)
			}
		}
		return out, len(out) > 0
	}
	service, ok := serviceOfMutate(op.ID)
	if !ok {
		return nil, false
	}
	raws := list(m["operations"])
	if one, ok := m["operation"]; ok && raws == nil {
		raws = []any{one}
	}
	out := make([]operation, 0, len(raws))
	for _, raw := range raws {
		if o, ok := operationOf(service, raw); ok {
			out = append(out, o)
		}
	}
	return out, len(out) > 0
}

// toRead is every resource a call's rules compare against: the targets of its
// updates and removes, and what they point at.
func toRead(ops []operation) []string {
	var out []string
	add := func(name string) {
		if _, _, ok := kindOf(name); ok && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for _, o := range ops {
		add(o.name)
		for _, key := range []string{"campaign_budget", "account_budget", "asset_set"} {
			add(str(o.object[key]))
		}
	}
	return out
}
