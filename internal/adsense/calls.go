package adsense

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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

// Run makes a read call; every other call is prepared and applied
// ([Product.Prepare], [Product.Apply]).
func (p *Product) Run(ctx context.Context, call growth.Call) (any, error) {
	switch call.Tool {
	case toolAccounts:
		return p.listAccounts(ctx, call)
	case toolReport:
		return p.report(ctx, call)
	case toolSavedReport:
		if argString(call.Args, "report") != "" {
			return p.savedReport(ctx, call)
		}
	}
	req, class, err := p.requestOf(call)
	if err != nil {
		return nil, err
	}
	if class != growth.ClassRead || !req.Reads() {
		return nil, refusal(growth.RefusedInvalid, call.Tool+" changes AdSense, so it is prepared and applied, "+
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
		toolAdClients:      p.listing("adsense.accounts.adclients.list"),
		toolAdUnits:        p.listing("adsense.accounts.adclients.adunits.list"),
		toolCustomChannels: p.listing("adsense.accounts.adclients.customchannels.list"),
		toolSites:          p.listing("adsense.accounts.sites.list"),
		toolAlerts:         p.listing("adsense.accounts.alerts.list"),
		toolPolicyIssues:   p.listing("adsense.accounts.policyIssues.list"),
		toolPayments:       p.listing("adsense.accounts.payments.list"),
		toolSavedReport:    p.listing(savedListOp),
		toolAdCode:         p.adCode,
		toolAdUnitUpsert:   p.adUnitUpsert,
		toolChannelUpsert:  p.channelUpsert,
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
		return googlegrowth.Request{}, "", refusal(growth.RefusedNotFound, call.Tool+" is not an AdSense tool.")
	}
	id, _ := call.Args["operation"].(string)
	if !slices.Contains(tool.Operations, id) {
		return googlegrowth.Request{}, "", p.Catalog().Outside(call.Tool, id, p.Level(call.Handle))
	}
	op, _ := p.Catalog().Operation(id)
	query, err := queryOf(call.Args["query"])
	if err != nil {
		return googlegrowth.Request{}, "", err
	}
	req, err := newRequest(op, stringMap(call.Args["path"]), query, objectArg(call.Args["body"]))
	return req, tool.Class, err
}

// queryOf is a generic call's query: each parameter a string, or a list of
// strings for a parameter Google repeats (a report's dimensions).
func queryOf(v any) (url.Values, error) {
	m, _ := v.(map[string]any)
	out := url.Values{}
	for k, val := range m {
		switch t := val.(type) {
		case string:
			out.Add(k, t)
		case []any:
			for _, e := range t {
				s, ok := e.(string)
				if !ok {
					return nil, refusal(growth.RefusedInvalid, fmt.Sprintf("query parameter %q lists a value "+
						"that is not a string. No vendor was reached.", k))
				}
				out.Add(k, s)
			}
		default:
			return nil, refusal(growth.RefusedInvalid, fmt.Sprintf("query parameter %q is a string or a list "+
				"of strings. No vendor was reached.", k))
		}
	}
	return out, nil
}

// newRequest is op's request, acting in the publisher account its path
// names, held to the path parameters, query keys, body spelling and update
// mask the engine classes it by.
func newRequest(op growth.Operation, path map[string]string, query url.Values,
	body map[string]any) (googlegrowth.Request, error) {
	for name, v := range path {
		switch {
		case !strings.Contains(op.Path, "{"+name+"}"):
			// A PARAMETER THE PATH DOES NOT NAME would address nothing, yet be
			// read as the call's account or target.
			return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("%s takes no path "+
				"parameter %q. No vendor was reached.", op.ID, name))
		case publisherParam(op, name) && !publisherID(v):
			return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("%s %q is not a "+
				"publisher ID: pub- and 16 digits. No vendor was reached.", name, v))
		}
	}
	for name, values := range query {
		switch {
		case !slices.Contains(queryKeys(op), name):
			// A QUERY KEY THE METHOD DOES NOT TAKE could carry a field mask
			// under its proto name (update_mask) that the engine never read.
			return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("%s takes no query "+
				"parameter %q; it takes %s. No vendor was reached.", op.ID, name, orNone(queryKeys(op))))
		case len(values) > 1 && !slices.Contains(repeated, name):
			// A SECOND VALUE IS ONE THE ENGINE NEVER READS: a mask given twice
			// would send a path nobody classed.
			return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("query parameter %q "+
				"takes one value. No vendor was reached.", name))
		}
	}
	if key := snakeKey(body); key != "" {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("the body's field %q is "+
			"spelled as the API's proto name; give it as its JSON name, %q, which the engine classes the "+
			"change by. No vendor was reached.", key, camel(key)))
	}
	req, err := googlegrowth.NewRequest(op, path, nil, body)
	if err != nil {
		return req, err
	}
	if len(query) > 0 {
		req.Query = query
	}
	if op.Method == "PATCH" {
		if err := maskHolds(op, req.Query.Get("updateMask")); err != nil {
			return googlegrowth.Request{}, err
		}
		if err := patchBodyMatches(op, req, body); err != nil {
			return googlegrowth.Request{}, err
		}
	}
	req.Account = path[owner(op)]
	req.Raw = strings.HasSuffix(op.ID, "Csv")
	req.Diff = diffOf(op, body, req.Query.Get("updateMask"))
	return req, nil
}

// publisherParam reports whether op's path parameter name holds a publisher
// ID: the account a call acts in, and a platform's sub-account.
func publisherParam(op growth.Operation, name string) bool {
	return name == owner(op) || subAccount(op, name) && name == "accountsId"
}

// patchBodyMatches refuses a patch body whose own names differ from what the
// engine classes it by: a resource name other than the call's, and a site
// moved to a group of another platform.
func patchBodyMatches(op growth.Operation, req googlegrowth.Request, body map[string]any) error {
	if name, set := body["name"]; set && name != req.Target() {
		return refusal(growth.RefusedInvalid, fmt.Sprintf("the body names %v; the call changes %s. No vendor "+
			"was reached.", name, req.Target()))
	}
	if op.ID != childSitePatchOp {
		return nil
	}
	group, _ := body["platformGroup"].(string)
	prefix := "accounts/" + req.Params["accountsId"] + "/platforms/" + req.Params["platformsId"] + "/groups/"
	if !strings.HasPrefix(group, prefix) || !googlegrowth.IDParam("", strings.TrimPrefix(group, prefix)) {
		return refusal(growth.RefusedInvalid, fmt.Sprintf("platformGroup is one of the site's platform's "+
			"groups, %s<group>; not %q. No vendor was reached.", prefix, group))
	}
	return nil
}

// orNone is names as a list, "none" for none.
func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// cut is how an answer to req is cut to handle's accounts: only the account
// listing names accounts beyond the seat's.
func (p *Product) cut(handle string, req googlegrowth.Request) func(any) any {
	if req.Op.ID != "adsense.accounts.list" {
		return nil
	}
	return func(v any) any {
		obj, _ := v.(map[string]any)
		if obj == nil {
			return map[string]any{}
		}
		mine := p.accountsOf(handle)
		list, _ := obj["accounts"].([]any)
		obj["accounts"] = slices.DeleteFunc(list, func(e any) bool {
			m, _ := e.(map[string]any)
			name, _ := m["name"].(string)
			return !slices.Contains(mine, strings.TrimPrefix(name, "accounts/"))
		})
		return obj
	}
}

// listing is a curated listing tool's request: op on the account and the ad
// client where its path names one, paged where it takes pages.
func (p *Product) listing(id string) func(growth.Call) (googlegrowth.Request, error) {
	return func(call growth.Call) (googlegrowth.Request, error) {
		op, _ := p.Catalog().Operation(id)
		path := map[string]string{"accountsId": argString(call.Args, "account")}
		if strings.Contains(op.Path, "{adclientsId}") {
			path["adclientsId"] = argString(call.Args, "ad_client")
		}
		query := url.Values{}
		if slices.Contains(queryKeys(op), "pageSize") {
			query.Set("pageSize", strconv.Itoa(pageSize(call.Args)))
			if t := argString(call.Args, "page_token"); t != "" {
				query.Set("pageToken", t)
			}
		}
		if lang := argString(call.Args, "language"); lang != "" && slices.Contains(queryKeys(op), "languageCode") {
			query.Set("languageCode", lang)
		}
		return newRequest(op, path, query, nil)
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

// adCode is adsense_ad_code's request: one ad unit's code, or the ad client's.
func (p *Product) adCode(call growth.Call) (googlegrowth.Request, error) {
	path := map[string]string{"accountsId": argString(call.Args, "account"),
		"adclientsId": argString(call.Args, "ad_client")}
	id := "adsense.accounts.adclients.getAdcode"
	if unit := argString(call.Args, "ad_unit"); unit != "" {
		id, path["adunitsId"] = "adsense.accounts.adclients.adunits.getAdcode", unit
	}
	op, _ := p.Catalog().Operation(id)
	return newRequest(op, path, nil, nil)
}

// sizePattern is an ad unit's size: WIDTHxHEIGHT, or 1x3 for a responsive one.
var sizePattern = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{1,4}x[0-9]{1,4}$`) })

// adUnitUpsert is adsense_ad_unit_upsert's request: a display unit created,
// or a patch of what the call names.
func (p *Product) adUnitUpsert(call growth.Call) (googlegrowth.Request, error) {
	path := map[string]string{"accountsId": argString(call.Args, "account"),
		"adclientsId": argString(call.Args, "ad_client")}
	name, size, state := argString(call.Args, "display_name"), argString(call.Args, "size"),
		argString(call.Args, "state")
	if size != "" && !sizePattern().MatchString(size) {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("size is WIDTHxHEIGHT (728x90) "+
			"or 1x3, not %q. No vendor was reached.", size))
	}
	if state != "" {
		if err := within("state", []string{state}, adUnitStates); err != nil {
			return googlegrowth.Request{}, err
		}
	}
	unit := argString(call.Args, "ad_unit")
	if unit == "" {
		if name == "" || size == "" {
			return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "a new ad unit needs display_name "+
				"and size. No vendor was reached.")
		}
		// GOOGLE CREATES DISPLAY UNITS ALONE, and a new one is active.
		op, _ := p.Catalog().Operation("adsense.accounts.adclients.adunits.create")
		return newRequest(op, path, nil, map[string]any{"displayName": name, "state": cmp.Or(state, "ACTIVE"),
			"contentAdsSettings": map[string]any{"size": size, "type": "DISPLAY"}})
	}
	body, fields := map[string]any{}, []string{}
	if name != "" {
		body["displayName"], fields = name, append(fields, "displayName")
	}
	if state != "" {
		body["state"], fields = state, append(fields, "state")
	}
	if size != "" {
		body[adSettingsField], fields = map[string]any{"size": size}, append(fields, adSettingsField+".size")
	}
	if len(fields) == 0 {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "name what changes: display_name, state "+
			"or size. No vendor was reached.")
	}
	path["adunitsId"] = unit
	op, _ := p.Catalog().Operation(adUnitPatchOp)
	return newRequest(op, path, url.Values{"updateMask": {strings.Join(fields, ",")}}, body)
}

// channelUpsert is adsense_custom_channel_upsert's request: a channel
// created, or a patch of what the call names.
func (p *Product) channelUpsert(call growth.Call) (googlegrowth.Request, error) {
	path := map[string]string{"accountsId": argString(call.Args, "account"),
		"adclientsId": argString(call.Args, "ad_client")}
	name := argString(call.Args, "display_name")
	active, activeSet := call.Args["active"].(bool)
	channel := argString(call.Args, "custom_channel")
	if channel == "" {
		if name == "" {
			return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "a new custom channel needs "+
				"display_name. No vendor was reached.")
		}
		body := map[string]any{"displayName": name, "active": !activeSet || active}
		op, _ := p.Catalog().Operation("adsense.accounts.adclients.customchannels.create")
		return newRequest(op, path, nil, body)
	}
	body, fields := map[string]any{}, []string{}
	if name != "" {
		body["displayName"], fields = name, append(fields, "displayName")
	}
	if activeSet {
		body["active"], fields = active, append(fields, "active")
	}
	if len(fields) == 0 {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "name what changes: display_name or "+
			"active. No vendor was reached.")
	}
	path["customchannelsId"] = channel
	op, _ := p.Catalog().Operation("adsense.accounts.adclients.customchannels.patch")
	return newRequest(op, path, url.Values{"updateMask": {strings.Join(fields, ",")}}, body)
}

// within refuses values not in allowed, naming them all.
func within(arg string, values, allowed []string) error {
	for _, v := range values {
		if !slices.Contains(allowed, v) {
			return refusal(growth.RefusedInvalid, fmt.Sprintf("%s takes %s, not %q. No vendor was reached.", arg,
				strings.Join(allowed, ", "), v))
		}
	}
	return nil
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
func names(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, n := range list {
		if s, ok := n.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
