package admob

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// publisherPattern is a publisher ID as Google writes it.
const publisherPattern = "^pub-[0-9]{16}$"

var publisherRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(publisherPattern) })

// publisherID reports whether s is a publisher ID.
func publisherID(s string) bool { return publisherRE().MatchString(s) }

// maxPageSize bounds a listing's page: Google's own pages hold up to 10,000
// entries, and an agent reads a hundred at a time.
const maxPageSize = 1000

// Run makes a read call; every other call is prepared and applied
// ([Product.Prepare], [Product.Apply]).
func (p *Product) Run(ctx context.Context, call growth.Call) (any, error) {
	switch call.Tool {
	case toolAccounts:
		return p.listAccounts(ctx, call)
	case toolNetworkReport, toolMediationReport, toolCampaignReport:
		return p.report(ctx, call)
	}
	req, class, err := p.requestOf(call)
	if err != nil {
		return nil, err
	}
	if class != growth.ClassRead || !req.Reads() {
		return nil, refusal(growth.RefusedInvalid, call.Tool+" changes AdMob, so it is prepared and applied, "+
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

// requestOf is a tool's request and the class of its tool: a curated tool's
// built from its typed arguments, a generic tool's from the operation it names.
func (p *Product) requestOf(call growth.Call) (googlegrowth.Request, growth.Class, error) {
	build, curated := map[string]func(growth.Call) (googlegrowth.Request, error){
		toolApps:            p.listing("admob.accounts.apps.list"),
		toolAdUnits:         p.listing("admob.accounts.adUnits.list"),
		toolMediationGroups: p.listing("admob.accounts.mediationGroups.list"),
		toolAppCreate:       p.appCreate,
		toolAdUnitCreate:    p.adUnitCreate,
		toolMappingCreate:   p.mappingCreate,
		toolGroupUpsert:     p.groupUpsert,
		toolABExperiment:    p.abExperiment,
	}[call.Tool]
	if !curated {
		return p.generic(call)
	}
	tool, _ := p.Catalog().Tool(call.Tool)
	req, err := build(call)
	return req, tool.Class, err
}

// generic builds a generic tool's request and the class its tool makes.
func (p *Product) generic(call growth.Call) (googlegrowth.Request, growth.Class, error) {
	tool, ok := p.Catalog().Tool(call.Tool)
	if !ok {
		return googlegrowth.Request{}, "", refusal(growth.RefusedNotFound, call.Tool+" is not an AdMob tool.")
	}
	id, _ := call.Args["operation"].(string)
	if !slices.Contains(tool.Operations, id) {
		return googlegrowth.Request{}, "", p.Catalog().Outside(call.Tool, id, p.Level(call.Handle))
	}
	op, _ := p.Catalog().Operation(id)
	req, err := newRequest(op, stringMap(call.Args["path"]), stringMap(call.Args["query"]),
		objectArg(call.Args["body"]))
	return req, tool.Class, err
}

// cut is how an answer to req is cut to handle's accounts: only the account
// listing names accounts beyond the seat's.
func (p *Product) cut(handle string, req googlegrowth.Request) func(any) any {
	if req.Op.ID != "admob.accounts.list" {
		return nil
	}
	return func(v any) any {
		obj, _ := v.(map[string]any)
		if obj == nil {
			return map[string]any{}
		}
		mine := p.accountsOf(handle)
		list, _ := obj["account"].([]any)
		obj["account"] = slices.DeleteFunc(list, func(e any) bool {
			m, _ := e.(map[string]any)
			id, _ := m["publisherId"].(string)
			return !slices.Contains(mine, id)
		})
		return obj
	}
}

// listing is a curated listing tool's request: op on the account, paged.
func (p *Product) listing(id string) func(growth.Call) (googlegrowth.Request, error) {
	return func(call growth.Call) (googlegrowth.Request, error) {
		op, _ := p.Catalog().Operation(id)
		query := map[string]string{"pageSize": strconv.Itoa(pageSize(call.Args))}
		if t, _ := call.Args["page_token"].(string); t != "" {
			query["pageToken"] = t
		}
		if f, _ := call.Args["filter"].(string); f != "" {
			query["filter"] = f
		}
		return newRequest(op, map[string]string{"accountsId": argString(call.Args, "account")}, query, nil)
	}
}

// pageSize is the page a listing asks for: 100 by default, at most maxPageSize.
func pageSize(args map[string]any) int {
	n := argInt(args, "page_size")
	switch {
	case n <= 0:
		return 100
	case n > maxPageSize:
		return maxPageSize
	}
	return n
}

// appCreate is admob_app_create's request: a manual app, never linked.
func (p *Product) appCreate(call growth.Call) (googlegrowth.Request, error) {
	op, _ := p.Catalog().Operation("admob.accounts.apps.create")
	name := strings.TrimSpace(argString(call.Args, "display_name"))
	if name == "" {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "name the app. No vendor was reached.")
	}
	platform := argString(call.Args, "platform")
	if err := within("platform", []string{platform}, []string{"IOS", "ANDROID"}); err != nil {
		return googlegrowth.Request{}, err
	}
	body := map[string]any{"platform": platform, "manualAppInfo": map[string]any{"displayName": name}}
	if err := within("android_app_stores", stringsOf(names(call.Args["android_app_stores"])), appStores); err != nil {
		return googlegrowth.Request{}, err
	}
	if stores := names(call.Args["android_app_stores"]); len(stores) > 0 {
		if platform != "ANDROID" {
			return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "android_app_stores is for an ANDROID "+
				"app. No vendor was reached.")
		}
		body["linkedAppInfo"] = map[string]any{"androidAppStores": stores}
	}
	return newRequest(op, map[string]string{"accountsId": argString(call.Args, "account")}, nil, body)
}

// adUnitCreate is admob_ad_unit_create's request.
func (p *Product) adUnitCreate(call growth.Call) (googlegrowth.Request, error) {
	op, _ := p.Catalog().Operation("admob.accounts.adUnits.create")
	body := map[string]any{"appId": argString(call.Args, "app_id"),
		"displayName": argString(call.Args, "display_name"), "adFormat": argString(call.Args, "ad_format")}
	if err := within("ad_format", []string{argString(call.Args, "ad_format")}, adFormats); err != nil {
		return googlegrowth.Request{}, err
	}
	if err := within("ad_types", stringsOf(names(call.Args["ad_types"])), []string{"RICH_MEDIA", "VIDEO"}); err != nil {
		return googlegrowth.Request{}, err
	}
	if types := names(call.Args["ad_types"]); len(types) > 0 {
		body["adTypes"] = types
	}
	amount, item := argString(call.Args, "reward_amount"), argString(call.Args, "reward_item")
	switch {
	case (amount != "" || item != "") && body["adFormat"] != "REWARDED":
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "a reward is set only on a REWARDED ad "+
			"unit. No vendor was reached.")
	case amount != "" || item != "":
		body["rewardSettings"] = map[string]any{"unitAmount": amount, "unitType": item}
	}
	return newRequest(op, map[string]string{"accountsId": argString(call.Args, "account")}, nil, body)
}

// mappingCreate is admob_ad_unit_mapping_create's request: one mapping, or a
// batch of up to [maxBatchMappings], which Google refuses whole when larger.
func (p *Product) mappingCreate(call growth.Call) (googlegrowth.Request, error) {
	account := argString(call.Args, "account")
	items, _ := call.Args["mappings"].([]any)
	if len(items) == 0 || len(items) > maxBatchMappings {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("give 1 to %d mappings. No "+
			"vendor was reached.", maxBatchMappings))
	}
	mapping := func(m map[string]any) map[string]any {
		out := map[string]any{"adapterId": argString(m, "adapter_id"), "adUnitConfigurations": m["configurations"]}
		if name := argString(m, "display_name"); name != "" {
			out["displayName"] = name
		}
		return out
	}
	if len(items) == 1 {
		m, _ := items[0].(map[string]any)
		op, _ := p.Catalog().Operation("admob.accounts.adUnits.adUnitMappings.create")
		return newRequest(op, map[string]string{"accountsId": account, "adUnitsId": argString(m, "ad_unit")}, nil,
			mapping(m))
	}
	var requests []any
	for _, item := range items {
		m, _ := item.(map[string]any)
		requests = append(requests, map[string]any{"parent": "accounts/" + account + "/adUnits/" +
			argString(m, "ad_unit"), "adUnitMapping": mapping(m)})
	}
	op, _ := p.Catalog().Operation("admob.accounts.adUnitMappings.batchCreate")
	return newRequest(op, map[string]string{"accountsId": account}, nil, map[string]any{"requests": requests})
}

// groupUpsert is admob_mediation_group_upsert's request: a create without a
// group ID, a patch of its update mask with one.
func (p *Product) groupUpsert(call growth.Call) (googlegrowth.Request, error) {
	account, group := argString(call.Args, "account"), argString(call.Args, "mediation_group_id")
	body := objectArg(call.Args["mediation_group"])
	if group == "" {
		op, _ := p.Catalog().Operation("admob.accounts.mediationGroups.create")
		return newRequest(op, map[string]string{"accountsId": account}, nil, body)
	}
	mask := argString(call.Args, "update_mask")
	op, _ := p.Catalog().Operation("admob.accounts.mediationGroups.patch")
	return newRequest(op, map[string]string{"accountsId": account, "mediationGroupsId": group},
		map[string]string{"updateMask": mask}, body)
}

// abExperiment is admob_ab_experiment's request: start or stop.
func (p *Product) abExperiment(call growth.Call) (googlegrowth.Request, error) {
	path := map[string]string{"accountsId": argString(call.Args, "account"),
		"mediationGroupsId": argString(call.Args, "mediation_group_id")}
	switch argString(call.Args, "action") {
	case "start":
		op, _ := p.Catalog().Operation("admob.accounts.mediationGroups.mediationAbExperiments.create")
		body := map[string]any{"displayName": argString(call.Args, "display_name"),
			"treatmentTrafficPercentage": strconv.Itoa(argInt(call.Args, "treatment_traffic_percentage")),
			"treatmentMediationLines":    call.Args["treatment_mediation_lines"]}
		return newRequest(op, path, nil, body)
	case "stop":
		choice := argString(call.Args, "variant_choice")
		if err := within("variant_choice", []string{choice}, []string{variantA, variantB}); err != nil {
			return googlegrowth.Request{}, err
		}
		op, _ := p.Catalog().Operation("admob.accounts.mediationGroups.mediationAbExperiments.stop")
		return newRequest(op, path, nil, map[string]any{"variantChoice": choice})
	}
	return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "action is start or stop. No vendor was reached.")
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

// argString is a string argument, trimmed, "" where absent.
func argString(args map[string]any, name string) string {
	s, _ := args[name].(string)
	return strings.TrimSpace(s)
}

// argInt is an integer argument, 0 where absent.
func argInt(args map[string]any, name string) int {
	switch v := args[name].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

// names is a list argument's non-empty strings.
func names(v any) []any {
	list, _ := v.([]any)
	out := make([]any, 0, len(list))
	for _, n := range list {
		if s, ok := n.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
