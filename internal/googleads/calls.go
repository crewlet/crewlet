package googleads

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// accountRow is one account the seat may reach.
type accountRow struct {
	Customer string `json:"customer"`
	Name     string `json:"name" untrusted:"name"`
	Currency string `json:"currency,omitempty"`
	TimeZone string `json:"time_zone,omitempty"`
	Status   string `json:"status,omitempty"`
	Manager  string `json:"manager,omitempty"`
}

// accountsAnswer is google_ads_list_accounts' answer.
type accountsAnswer struct {
	Accounts []accountRow `json:"accounts"`
	// Unreached are the seat's accounts its identity could not read.
	Unreached []string `json:"unreached,omitempty"`
}

// Run makes a read call; every other call is prepared and applied
// ([Product.Prepare], [Product.Apply]).
func (p *Product) Run(ctx context.Context, call growth.Call) (any, error) {
	if call.Tool == toolListAccounts {
		return p.listAccounts(ctx, call)
	}
	req, err := p.requestOf(call)
	if err != nil {
		return nil, err
	}
	if req.Op.Class != growth.ClassRead || len(req.Op.Classes) > 0 {
		return nil, refusal(growth.RefusedInvalid, call.Tool+" changes Google Ads, so it is prepared and applied, "+
			"never run as a read. No vendor was reached.")
	}
	if err = p.inScope(call.Handle, req); err != nil {
		return nil, err
	}
	out, err := p.client.SendAndRead(ctx, call.Handle, req, p.cut(call.Handle, req))
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// requestOf is a tool's request: a curated tool's built from its typed
// arguments, a generic tool's from the operation it names.
func (p *Product) requestOf(call growth.Call) (googlegrowth.Request, error) {
	switch call.Tool {
	case toolQuery:
		customer, _ := call.Args["customer"].(string)
		body := map[string]any{"query": call.Args["query"]}
		if token, _ := call.Args["page_token"].(string); token != "" {
			body["pageToken"] = token
		}
		op, _ := p.Catalog().Operation(searchOp)
		return p.newRequest(op, map[string]string{"customersId": customer}, nil, body)
	case toolPause:
		return p.pauseRequest(call)
	case toolSetBudget:
		return p.budgetRequest(call)
	}
	tool, ok := p.Catalog().Tool(call.Tool)
	if !ok {
		return googlegrowth.Request{}, refusal(growth.RefusedNotFound, call.Tool+" is not a Google Ads tool.")
	}
	id, _ := call.Args["operation"].(string)
	if !slices.Contains(tool.Operations, id) {
		return googlegrowth.Request{}, p.Catalog().Outside(call.Tool, id, p.Level(call.Handle))
	}
	op, _ := p.Catalog().Operation(id)
	return p.newRequest(op, stringMap(call.Args["path"]), stringMap(call.Args["query"]), objectArg(call.Args["body"]))
}

// pauseRequest is google_ads_pause's mutate: every resource named, of one
// kind and one account, set to PAUSED.
func (p *Product) pauseRequest(call growth.Call) (googlegrowth.Request, error) {
	var customer, collection string
	var operations []any
	for _, v := range list(call.Args["resources"]) {
		name := str(v)
		m := resourceName().FindStringSubmatch(name)
		if m == nil || !slices.Contains(pausable, m[2]) {
			return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("%q is not a resource "+
				"google_ads_pause pauses. No vendor was reached.", name))
		}
		if customer != "" && (m[1] != customer || m[2] != collection) {
			return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "google_ads_pause pauses resources of one "+
				"kind in one account at a time. No vendor was reached.")
		}
		customer, collection = m[1], m[2]
		operations = append(operations, map[string]any{"update": map[string]any{"resourceName": name,
			"status": "PAUSED"}, "updateMask": "status"})
	}
	if customer == "" {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "name what to pause. No vendor was reached.")
	}
	op, _ := p.Catalog().Operation(pauseOf(collection))
	return p.newRequest(op, map[string]string{"customersId": customer}, nil, map[string]any{"operations": operations})
}

// budgetRequest is google_ads_set_budget's mutate: one budget's daily amount.
func (p *Product) budgetRequest(call growth.Call) (googlegrowth.Request, error) {
	name, _ := call.Args["budget"].(string)
	customer, kind, ok := kindOf(name)
	amount, err := growth.ParseAmount(str(call.Args["amount"]))
	if !ok || kind != "campaign_budget" || err != nil {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "name a campaign budget's resource name and "+
			"an amount such as 25.50. No vendor was reached.")
	}
	op, _ := p.Catalog().Operation("googleads.customers.campaignBudgets.mutate")
	return p.newRequest(op, map[string]string{"customersId": customer}, nil, map[string]any{"operations": []any{
		map[string]any{"update": map[string]any{"resourceName": name, "amountMicros": fmt.Sprint(amount)},
			"updateMask": "amountMicros"},
	}})
}

// listAccounts is every account the seat may reach, read on each account.
func (p *Product) listAccounts(ctx context.Context, call growth.Call) (accountsAnswer, error) {
	b := p.seat(call.Handle)
	if b == nil {
		return accountsAnswer{}, refusal(growth.RefusedForbidden, "this agent is off at Google Ads.")
	}
	out := accountsAnswer{Accounts: []accountRow{}}
	op, _ := p.Catalog().Operation(searchOp)
	const gaql = "SELECT customer.descriptive_name, customer.currency_code, customer.time_zone, customer.status " +
		"FROM customer"
	for _, customer := range b.Customers {
		rows, err := p.search(ctx, call.Handle, customer, gaql)
		if req, rerr := p.newRequest(op, map[string]string{"customersId": customer}, nil,
			map[string]any{"query": gaql}); rerr == nil {
			p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
		}
		if err != nil || len(rows) == 0 {
			out.Unreached = append(out.Unreached, customer)
			continue
		}
		m, _ := snaked(rows[0]["customer"]).(map[string]any)
		out.Accounts = append(out.Accounts, accountRow{Customer: customer, Name: str(m["descriptive_name"]),
			Currency: str(m["currency_code"]), TimeZone: str(m["time_zone"]), Status: str(m["status"]),
			Manager: p.block().ManagerOf(customer)})
	}
	return out, nil
}

// cut is how an answer to req is cut to handle's accounts: the accessible
// customers listing names every account the identity reaches.
func (p *Product) cut(handle string, req googlegrowth.Request) func(any) any {
	if req.Op.ID != "googleads.customers.listAccessibleCustomers" {
		return nil
	}
	b := p.seat(handle)
	return func(v any) any {
		m, _ := v.(map[string]any)
		names := list(m["resourceNames"])
		kept := []any{}
		for _, n := range names {
			id, _ := strings.CutPrefix(str(n), "customers/")
			if b != nil && slices.Contains(b.Customers, id) {
				kept = append(kept, n)
			}
		}
		return map[string]any{"resourceNames": kept}
	}
}

// stringMap is an argument object of strings.
func stringMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

// objectArg is an argument object, nil where none.
func objectArg(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
