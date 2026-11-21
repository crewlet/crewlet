package googleads

import (
	"fmt"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/growth"
)

// byService is how each operation type is classed: by its family, or at its
// mutate method's one class.
type byService struct {
	family string
	class  growth.Class
}

// services is every operation type GoogleAdsService.Mutate takes, from the
// catalog's service mutate methods, and the two it adds of its own.
var services = sync.OnceValue(func() map[string]byService {
	out := map[string]byService{
		// Reservations: booking commits spend, a quote reads.
		"book_campaigns": {class: spend}, "quote_campaigns": {class: read},
	}
	for _, op := range operations() {
		service, ok := serviceOfMutate(op.ID)
		rule := ruleOf[op.ID]
		if !ok || rule == "FR-MUTATE" || rule == "FR-BATCH" {
			// GoogleAdsService.Mutate and a batch job carry operations; they
			// are none themselves.
			continue
		}
		if rule != "" {
			out[service] = byService{family: rule}
		} else {
			out[service] = byService{class: op.Class}
		}
	}
	return out
})

// ruled is what a call came to: its class, whether it undoes nothing, and its
// operations as read.
type ruled struct {
	class  growth.Class
	danger bool
	ops    []operation
}

// classify is what a call of op with body comes to, read against l. A method
// of one class is that class; a mutate, the highest any of its operations
// comes to; running a batch job, the highest it staged, which staged answers
// from the access log. An operation of a type the engine does not know, or a
// batch job it holds no staging of, refuses the call.
func classify(op growth.Operation, body map[string]any, l live, staged func() (growth.Class, bool)) (ruled, error) {
	rule := ruleOf[op.ID]
	switch {
	case rule == "":
		return ruled{class: op.Class, danger: op.Danger}, nil
	case rule == "FR-RECOMMENDATION":
		return applied(body, l), nil
	case rule == "FR-BATCH" && strings.HasSuffix(op.ID, ".run"):
		c, ok := staged()
		if !ok {
			return ruled{}, refusal(growth.RefusedInvalid, "Crewlet holds no record of what this batch job staged, "+
				"so it cannot class running it: stage its operations through google_ads, then run it. No vendor "+
				"was reached.")
		}
		return ruled{class: c}, nil
	case rule == "FR-BATCH":
		removes := hasRemove(body)
		return ruled{class: pick(removes, admin, operate), danger: removes}, nil
	}
	ops, ok := parse(op, body)
	if !ok {
		return ruled{}, refusal(growth.RefusedInvalid, op.ID+" holds no operation Crewlet can read: give its "+
			"operations as the method takes them. No vendor was reached.")
	}
	fams := families()
	out := ruled{class: read, ops: ops}
	for _, o := range ops {
		svc, known := services()[o.service]
		if !known {
			return ruled{}, refusal(growth.RefusedInvalid, fmt.Sprintf("%s holds a %s operation, which Crewlet "+
				"does not class, so it does not send it. No vendor was reached.", op.ID, o.service))
		}
		c := svc.class
		if svc.family != "" {
			f, ok := fams[svc.family]
			if !ok {
				return ruled{}, fmt.Errorf("googleads: %s names rule %s, which the build does not hold", o.service,
					svc.family)
			}
			c = f.classOf(o, l)
		}
		if o.exempt {
			c = higher(c, public)
		}
		out.class = higher(out.class, c)
		out.danger = out.danger || o.kind == opRemove
	}
	return out, nil
}

// hasRemove reports whether a batch job's mutate removes one.
func hasRemove(body map[string]any) bool {
	op, _ := snaked(body["operation"]).(map[string]any)
	return op["remove"] != nil
}

// applied is ApplyRecommendation's class: each recommendation's type's, read
// live (FR-RECOMMENDATION); one the engine did not read is spend, the highest.
func applied(body map[string]any, l live) ruled {
	out := ruled{class: read}
	for _, item := range list(body["operations"]) {
		name := str(object(item)["resourceName"])
		s := l[name]
		c := spend
		if s != nil {
			c = recommendationClass(str(s["type"]))
		}
		out.class = higher(out.class, c)
	}
	if out.class == read {
		out.class = spend
	}
	return out
}

// recommendationsToRead are the recommendations an apply names.
func recommendationsToRead(body map[string]any) []string {
	var out []string
	for _, item := range list(body["operations"]) {
		if name := str(object(item)["resourceName"]); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// refusal is a call refused, in words the agent acts on.
func refusal(kind growth.RefusalKind, said string) error { return growth.Refuse(kind, said) }
