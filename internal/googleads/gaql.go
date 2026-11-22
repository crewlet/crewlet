package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// comparedFields is what each kind's rules read of a resource beyond an update's
// own mask: its status, how it bids, what it points at and what it is; and of
// a batch job a run approves, how many operations it holds.
var comparedFields = map[string][]string{
	"batch_job":                        {"status", "metadata.operation_count"},
	"campaign":                         {"status", "bidding_strategy_type", "campaign_budget"},
	"campaign_budget":                  {"amount_micros", "total_amount_micros"},
	"bidding_strategy":                 {"type"},
	"ad_group_criterion":               {"negative", "type"},
	"campaign_criterion":               {"negative", "type"},
	"ad_group_bid_modifier":            {"bid_modifier"},
	"campaign_bid_modifier":            {"bid_modifier"},
	"bidding_seasonality_adjustment":   {"conversion_rate_modifier"},
	"asset_group_listing_group_filter": {"type"},
	"asset_set":                        {"type"},
	"account_budget":                   {"approved_spending_limit_micros", "approved_end_date_time"},
	"recommendation":                   {"type"},
}

// searchOp is GoogleAdsService.Search, which every live read goes through.
const searchOp = "googleads.customers.googleAds.search"

// readLive reads every resource in names that the rules compare against, as
// handle's identity, with the fields of masks for an update's own before
// values. A kind whose read Google refuses is left unread: its rules then
// class the change as the highest they allow.
func (p *Product) readLive(ctx context.Context, handle string, names []string, masks map[string][]string) live {
	type query struct {
		customer, kind string
		names          []string
		fields         map[string]bool
	}
	queries := map[string]*query{}
	for _, name := range names {
		customer, kind, ok := kindOf(name)
		if !ok {
			continue
		}
		key := customer + "/" + kind
		q := queries[key]
		if q == nil {
			q = &query{customer: customer, kind: kind, fields: map[string]bool{"resource_name": true}}
			queries[key] = q
		}
		q.names = append(q.names, name)
		for _, f := range append(comparedFields[kind], masks[name]...) {
			q.fields[f] = true
		}
	}
	out := live{}
	for _, q := range queries {
		fields := make([]string, 0, len(q.fields))
		for f := range q.fields {
			fields = append(fields, q.kind+"."+f)
		}
		sort.Strings(fields)
		// QUOTED AS THEY STAND: kindOf admits a name of IDs alone, which holds
		// no quote to close the literal with.
		quoted := make([]string, len(q.names))
		for i, n := range q.names {
			quoted[i] = "'" + n + "'"
		}
		gaql := fmt.Sprintf("SELECT %s FROM %s WHERE %s.resource_name IN (%s)", strings.Join(fields, ", "),
			q.kind, q.kind, strings.Join(quoted, ", "))
		rows, err := p.search(ctx, handle, q.customer, gaql)
		if err != nil {
			continue
		}
		for _, row := range rows {
			s, _ := snaked(row[camel(q.kind)]).(map[string]any)
			if name := str(s["resource_name"]); name != "" {
				out[name] = s
			}
		}
	}
	return out
}

// search runs one GAQL query on customer as handle's identity, every page,
// and answers its rows.
func (p *Product) search(ctx context.Context, handle, customer, gaql string) ([]map[string]any, error) {
	op, _ := p.Catalog().Operation(searchOp)
	var out []map[string]any
	token := ""
	for page := 0; page < maxPages; page++ {
		body := map[string]any{"query": gaql}
		if token != "" {
			body["pageToken"] = token
		}
		req, err := p.newRequest(op, map[string]string{"customersId": customer}, nil, body)
		if err != nil {
			return nil, err
		}
		raw, err := p.client.Send(ctx, handle, req)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Results []map[string]any `json:"results"`
			Next    string           `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			// A SEARCH CHANGES NOTHING: an answer it cannot read is Google's
			// fault, never an outcome of the act it serves that is not known.
			return nil, &googlegrowth.CallError{Failure: growth.FailureVendorUnavailable,
				Err: fmt.Errorf("googleads: decode a search on %s: %w", customer, err)}
		}
		out = append(out, resp.Results...)
		if resp.Next == "" {
			return out, nil
		}
		token = resp.Next
	}
	return nil, fmt.Errorf("googleads: a search on %s did not end after %d pages", customer, maxPages)
}

// maxPages bounds a search the engine reads whole: 10,000 rows a page.
const maxPages = 20
