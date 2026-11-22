package googleads

import (
	"fmt"
	"slices"
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
		// Reservations: a booking commits what its quote says, which no cap
		// holds (ADR-0171), and a quote reads.
		"book_campaigns": {class: admin}, "quote_campaigns": {class: read},
	}
	for _, op := range operations() {
		service, ok := serviceOfMutate(op.ID)
		rule := ruleOf()[op.ID]
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

// ruled is what a call came to: its class, whether it undoes nothing, its
// operations as the caps measure them, and whether any of it spends. A call
// that spends is held to the caps whatever its class, so an admin operation
// beside a budget raise hides nothing from them.
type ruled struct {
	class  growth.Class
	danger bool
	ops    []operation
	spends bool
}

// classify is what a call of op with body comes to, read against l. A method
// of one class is that class, and one of spend names what the caps measure of
// it (measures); a mutate, the highest any of its operations comes to; a
// recommendation applied, its type's class and the budget it sets. An
// operation of a type the engine does not know refuses the call.
func classify(op growth.Operation, body map[string]any, l live) (ruled, error) {
	rule := ruleOf()[op.ID]
	switch {
	case rule == "" && op.Class == spend:
		measure, ok := measures(op.ID)
		if !ok {
			return ruled{}, fmt.Errorf("googleads: %s spends, and the build names nothing the caps measure of it",
				op.ID)
		}
		ops, ok := measure(body)
		if !ok {
			return ruled{}, refusal(growth.RefusedInvalid, op.ID+" names nothing Crewlet can hold to the spending "+
				"caps: give its body as the method takes it. No vendor was reached.")
		}
		return ruled{class: op.Class, danger: op.Danger, ops: ops, spends: true}, nil
	case rule == "":
		return ruled{class: op.Class, danger: op.Danger}, nil
	case rule == "FR-RECOMMENDATION":
		return applied(op, body, l)
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
		c, spends := svc.class, svc.class == spend
		if svc.family != "" {
			f, ok := fams[svc.family]
			if !ok {
				return ruled{}, fmt.Errorf("googleads: %s names rule %s, which the build does not hold", o.service,
					svc.family)
			}
			c, spends = f.classOf(o, l)
		}
		if o.exempt {
			c = higher(c, public)
		}
		out.class = higher(out.class, c)
		out.danger = out.danger || o.kind == opRemove
		// AN OPERATION THE CAPS MEASURE, at or above spend and short of a
		// removal, may add to the day's capacity whatever else it changes.
		out.spends = out.spends || spends || (measured(o) && o.kind != opRemove && c.Rank() >= spend.Rank())
	}
	return out, nil
}

// measured reports whether the caps' capacity reads o: a campaign or a
// campaign budget made, changed or removed (capacity.go).
func measured(o operation) bool { return o.service == "campaign" || o.service == "campaign_budget" }

// graduateOp is ExperimentService.GraduateExperiment.
const graduateOp = "googleads.customers.experiments.graduateExperiment"

// measures is what a method of one class, spend, that carries no operations
// adds to the day's capacity, as operations capacity applies (false where the
// body names none), and false for a method it does not know. A method whose
// request names no amount a cap can hold is admin (ADR-0171), never here.
func measures(id string) (func(body map[string]any) ([]operation, bool), bool) {
	return graduated, id == graduateOp
}

// graduated is what GraduateExperiment adds: each experiment campaign runs on
// the budget its mapping names, counted as serving, as it does once it is a
// campaign of its own.
func graduated(body map[string]any) ([]operation, bool) {
	var out []operation
	for _, item := range list(object(snaked(body))["campaign_budget_mappings"]) {
		m := object(item)
		name, budget := str(m["experiment_campaign"]), str(m["campaign_budget"])
		_, ck, okC := kindOf(name)
		_, bk, okB := kindOf(budget)
		if !okC || !okB || ck != "campaign" || bk != "campaign_budget" {
			return nil, false
		}
		out = append(out, operation{service: "campaign", kind: opUpdate, name: name, mask: []string{"campaign_budget",
			"status"}, object: map[string]any{"campaign_budget": budget, "status": "ENABLED"}})
	}
	return out, len(out) > 0
}

// hasRemove reports whether a batch job's mutate removes one.
func hasRemove(body map[string]any) bool {
	return object(object(snaked(body))["operation"])["remove"] != nil
}

// applied is ApplyRecommendation's ruling (FR-RECOMMENDATION): each
// recommendation's type's class, read live. A spend type is held to the caps
// by the budget it sets: none for a type that sets none, the new amount its
// parameters name on the budget it targets, and otherwise admin, since what
// it adds is not in the request (ADR-0171). One the engine did not read is
// admin, the highest.
func applied(op growth.Operation, body map[string]any, l live) (ruled, error) {
	items := list(object(snaked(body))["operations"])
	if len(items) == 0 {
		return ruled{}, refusal(growth.RefusedInvalid, op.ID+" names no recommendation to apply. No vendor was "+
			"reached.")
	}
	out := ruled{class: read}
	for _, item := range items {
		m := object(item)
		s := l[str(m["resource_name"])]
		kind, budgets := str(s["type"]), budgetParameters(m)
		c := recommendationClass(kind)
		switch {
		case s == nil:
			c = admin
		case c != spend && len(budgets) == 0:
		case len(budgets) == 0 && slices.Contains(unbudgetedRecommendations, kind):
			out.spends = true
		case len(budgets) == 1 && slices.Contains(newBudgetAmounts, budgets[0]) && str(s["campaign_budget"]) != "":
			o := operation{service: "campaign_budget", kind: opUpdate, name: str(s["campaign_budget"]),
				object: map[string]any{"amount_micros": lookup(m, budgets[0])}, mask: []string{"amount_micros"}}
			// The amount is what a budget recommendation does; it never lowers
			// what another type's recommendation is.
			bc, spends := budgetFamily().classOf(o, l)
			if strings.Contains(kind, "BUDGET") {
				c = bc
			} else {
				c = higher(c, bc)
			}
			out.ops, out.spends = append(out.ops, o), out.spends || spends
		default:
			c = admin
		}
		out.class = higher(out.class, c)
	}
	return out, nil
}

// newBudgetAmounts are the apply parameters that set the budget a
// recommendation targets (recommendation.campaign_budget) to a new amount.
var newBudgetAmounts = []string{"campaign_budget.new_budget_amount_micros",
	"raise_budget_performance_too_low.new_budget_amount_micros"}

// budgetParameters are the apply parameters of one recommendation that name a
// budget, by their dotted paths.
func budgetParameters(m map[string]any) []string {
	var out []string
	for _, path := range leaves(m, "") {
		if strings.Contains(path, "budget") {
			out = append(out, path)
		}
	}
	return out
}

// recommendationsToRead are the recommendations an apply names.
func recommendationsToRead(body map[string]any) []string {
	var out []string
	for _, item := range list(object(snaked(body))["operations"]) {
		if name := str(object(item)["resource_name"]); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// refusal is a call refused, in words the agent acts on.
func refusal(kind growth.RefusalKind, said string) error { return growth.Refuse(kind, said) }
