package appleads

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// The bounds of a call's arguments.
const (
	maxQueryValueBytes = 1000
	maxBodyBytes       = 64 << 10
	// maxUploadBytes bounds an image an agent uploads: Apple's Maps assets
	// are photos and logos a few MiB at most.
	maxUploadBytes = 8 << 20
	// maxTargets bounds the objects one pause or bid change names: each is
	// read before and sent once, and a person reads every one on a card.
	maxTargets = 50
)

var (
	pathValue   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,128}$`) })
	queryKey    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,40}$`) })
	digits      = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{1,20}$`) })
	placeholder = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\{([A-Za-z]+)\}`) })
)

// target is one request of a write or a read, made concrete.
type target struct {
	op     growth.Operation
	path   string
	query  url.Values
	body   any
	upload *Upload
}

// made is one call made concrete: its requests, where they go and the roles
// the connection holds there.
type made struct {
	tool    string
	targets []target
	conn    *conn
	// account is the ad account every request carries in X-AP-Context, ""
	// for an organization call.
	account string
	roles   []string
	seat    *config.RoleAppleAds
}

// toolClass is the class each tool calls with.
var toolClass = map[string]growth.Class{ToolRead: growth.ClassRead, ToolReport: growth.ClassRead,
	ToolOperations: growth.ClassRead, ToolOperate: growth.ClassOperate, ToolPause: growth.ClassOperate,
	ToolSpend: growth.ClassSpend, ToolSetBudget: growth.ClassSpend, ToolSetBids: growth.ClassSpend,
	ToolPublish: growth.ClassPublic, ToolAdmin: growth.ClassAdmin}

// genericTool is the generic tool each class is asked through.
var genericTool = map[growth.Class]string{growth.ClassRead: ToolRead, growth.ClassOperate: ToolOperate,
	growth.ClassSpend: ToolSpend, growth.ClassPublic: ToolPublish, growth.ClassAdmin: ToolAdmin}

// orgOps are the operations of the organization, sent with no X-AP-Context.
var orgOps = map[string]bool{"getUserAcls": true, "getMe": true, "orgsIdGet": true, "getAdvertiserResources": true,
	"adAccountsPost": true, "sharedBudgetsPost": true, "sharedBudgetsQueryPost": true, "sharedBudgetsIdGet": true,
	"sharedBudgetsIdPut": true, "sharedBudgetsIdDelete": true}

// budgetOrder reports whether op acts on budget orders, which name their ad
// accounts in their own body.
func budgetOrder(op growth.Operation) bool { return strings.HasPrefix(op.Path, "/v1/shared-budgets") }

func invalid(format string, args ...any) error {
	return growth.Refuse(growth.RefusedInvalid, fmt.Sprintf(format, args...)+" No call was made.")
}

// concrete makes args, a call of tool by handle, concrete: its requests in the
// tool's operations, their paths, queries and bodies held to their shapes, the
// ad account among the seat's and the connection reaching it, whose roles
// there must allow every request.
func (p *Product) concrete(ctx context.Context, handle, tool string, args map[string]any) (made, error) {
	seat, err := p.seat(handle)
	if err != nil {
		return made{}, err
	}
	m := made{tool: tool, seat: seat}
	switch tool {
	case ToolPause:
		err = m.pauses(args)
	case ToolSetBids:
		err = m.bids(args)
	case ToolSetBudget:
		err = m.budget(args)
	default:
		err = m.generic(args)
	}
	if err != nil {
		return made{}, err
	}
	if err := p.place(ctx, &m, args); err != nil {
		return made{}, err
	}
	if err := p.fill(ctx, m); err != nil {
		return made{}, err
	}
	for _, t := range m.targets {
		if !growth.Permits(t.op, m.roles) {
			return made{}, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("the connected API user's roles on %s "+
				"(%s) cannot make %s: it needs %s. A person changes the user's role in Apple Ads. No call was made.",
				m.where(), words.And(rolesNames(m.roles)), t.op.ID, words.Or(rolesNames(t.op.Roles))))
		}
	}
	return m, nil
}

// where names what m acts on, as a person reads it.
func (m made) where() string {
	if m.account == "" {
		return "the organization of connection " + m.conn.id
	}
	return "ad account " + m.account
}

// generic is a generic tool's one request.
func (m *made) generic(args map[string]any) error {
	id, _ := args["operation"].(string)
	op, ok := Catalog().Operation(id)
	def, _ := Catalog().Tool(m.tool)
	if !ok || !slices.Contains(def.Operations, op.ID) {
		return Catalog().Outside(m.tool, id, growth.LevelOf(m.seat.Level))
	}
	t := target{op: op}
	var err error
	if t.path, err = fillPath(op.Path, args["path"]); err != nil {
		return err
	}
	if t.query, err = queryOf(args["query"], op.Method == http.MethodGet); err != nil {
		return err
	}
	switch {
	case op.ID == "uploadAsset":
		if t.upload, err = uploadOf(args["body"]); err != nil {
			return err
		}
	case op.Method == http.MethodPost || op.Method == http.MethodPut:
		body := args["body"]
		_, isObject := body.(map[string]any)
		_, isList := body.([]any)
		if data, _ := json.Marshal(body); len(data) > maxBodyBytes || (!isObject && !isList) {
			return invalid("`body` is the JSON object or list of at most %d KiB the operation takes.", maxBodyBytes>>10)
		}
		t.body = body
	}
	m.targets = []target{t}
	return nil
}

// uploadOf is uploadAsset's form: the image, base64, and its fields.
func uploadOf(raw any) (*Upload, error) {
	b, _ := raw.(map[string]any)
	name, _ := b["file_name"].(string)
	data, _ := b["file_base64"].(string)
	id, _ := b["promotedObjectId"].(string)
	kind, _ := b["promotedObjectType"].(string)
	file, err := base64.StdEncoding.DecodeString(data)
	switch {
	case err != nil || len(file) == 0 || len(file) > maxUploadBytes:
		return nil, invalid("`body.file_base64` is the image, standard base64, of at most %d MiB.", maxUploadBytes>>20)
	case !pathValue().MatchString(name) || !slices.ContainsFunc([]string{".png", ".jpg", ".jpeg", ".heic"},
		func(ext string) bool { return strings.HasSuffix(strings.ToLower(name), ext) }):
		return nil, invalid("`body.file_name` is the image's name, ending .png, .jpg or .heic.")
	case !pathValue().MatchString(id) || kind == "":
		return nil, invalid("`body.promotedObjectId` and `body.promotedObjectType` name the brand the image is for.")
	}
	return &Upload{Name: name, Data: file, Fields: map[string]string{"promotedObjectId": id, "promotedObjectType": kind}}, nil
}

// The levels the curated tools name.
const (
	levelCampaign   = "campaign"
	levelAdGroup    = "ad_group"
	levelKeyword    = "keyword"
	levelSearchTerm = "search_term"
	levelAd         = "ad"
)

// levelOps are the update operation and path of each level a curated tool
// changes.
var levelOps = map[string]struct{ op, path string }{
	levelCampaign: {opCampaignPut, "/v1/campaigns/"}, levelAdGroup: {opAdGroupPut, "/v1/adgroups/"},
	levelKeyword: {opKeywordPut, "/v1/keywords/"}, levelAd: {opAdPut, "/v1/ads/"},
}

// update is level's update of id with body.
func update(level, id string, body map[string]any) (target, error) {
	lo, ok := levelOps[level]
	if !ok {
		return target{}, invalid("`level` is campaign, ad_group, keyword or ad.")
	}
	if !digits().MatchString(id) {
		return target{}, invalid("%q is not an Apple Ads ID: its digits.", id)
	}
	op, _ := Catalog().Operation(lo.op)
	return target{op: op, path: lo.path + id, body: body}, nil
}

// pauses are the pause tool's updates: status PAUSED, one per ID.
func (m *made) pauses(args map[string]any) error {
	level, _ := args["level"].(string)
	ids, _ := args["ids"].([]any)
	if len(ids) == 0 || len(ids) > maxTargets {
		return invalid("`ids` names 1 to %d objects.", maxTargets)
	}
	seen := map[string]bool{}
	for _, raw := range ids {
		id, _ := raw.(string)
		if seen[id] {
			return invalid("`ids` names %s twice.", id)
		}
		seen[id] = true
		t, err := update(level, id, map[string]any{"status": statusPaused})
		if err != nil {
			return err
		}
		m.targets = append(m.targets, t)
	}
	return nil
}

// moneyBody is {"amount", "currency"} under path's keys; the currency is
// filled once the account is placed.
func moneyBody(amount string, keys ...string) (map[string]any, error) {
	if _, err := growth.ParseAmount(amount); err != nil {
		return nil, invalid("%q is not an amount: a decimal such as 50.00.", amount)
	}
	out := map[string]any{"amount": strings.TrimSpace(amount)}
	for i := len(keys) - 1; i >= 0; i-- {
		out = map[string]any{keys[i]: out}
	}
	return out, nil
}

// budget is the set-budget tool's update of one campaign's daily budget.
func (m *made) budget(args map[string]any) error {
	id, _ := args["campaign"].(string)
	amount, _ := args["daily_budget"].(string)
	body, err := moneyBody(amount, "dailyBudget", "value")
	if err != nil {
		return err
	}
	t, err := update(levelCampaign, id, body)
	if err != nil {
		return err
	}
	m.targets = []target{t}
	return nil
}

// bids are the set-bids tool's updates: a keyword's bid, an ad group's
// default bid.
func (m *made) bids(args map[string]any) error {
	items, _ := args["bids"].([]any)
	if len(items) == 0 || len(items) > maxTargets {
		return invalid("`bids` names 1 to %d bids.", maxTargets)
	}
	seen := map[string]bool{}
	for _, raw := range items {
		b, _ := raw.(map[string]any)
		level, _ := b["level"].(string)
		id, _ := b["id"].(string)
		amount, _ := b["bid"].(string)
		var body map[string]any
		var err error
		switch level {
		case levelKeyword:
			body, err = moneyBody(amount, "bid")
		case levelAdGroup:
			body, err = moneyBody(amount, "bidStrategy", "bid")
		default:
			return invalid("a bid's `level` is keyword or ad_group.")
		}
		if err != nil {
			return err
		}
		if seen[level+"/"+id] {
			return invalid("`bids` names %s %s twice.", level, id)
		}
		seen[level+"/"+id] = true
		t, err := update(level, id, body)
		if err != nil {
			return err
		}
		m.targets = append(m.targets, t)
	}
	return nil
}

// fillCurrency writes the account's currency beside every amount a curated
// tool's body sets.
func fillCurrency(v any, currency string) {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["amount"]; ok {
			if _, set := t["currency"]; !set {
				t["currency"] = currency
			}
		}
		for _, child := range t {
			fillCurrency(child, currency)
		}
	case []any:
		for _, child := range t {
			fillCurrency(child, currency)
		}
	}
}

// fillPath is template with each {name} replaced by its value.
func fillPath(template string, raw any) (string, error) {
	vals, _ := raw.(map[string]any)
	used := 0
	var bad error
	out := placeholder().ReplaceAllStringFunc(template, func(ph string) string {
		name := strings.Trim(ph, "{}")
		v, _ := vals[name].(string)
		if !pathValue().MatchString(v) {
			bad = invalid("`path.%s` is the object's ID: letters, digits and . _ ~ : - only.", name)
			return ph
		}
		used++
		return url.PathEscape(v)
	})
	if bad != nil {
		return "", bad
	}
	if used != len(vals) {
		return "", invalid("`path` names a parameter %s does not have.", template)
	}
	return out, nil
}

// queryOf reads a GET's query, held to plain parameter shapes; every other
// method takes its arguments in its body.
func queryOf(raw any, get bool) (url.Values, error) {
	vals, _ := raw.(map[string]any)
	if len(vals) > 0 && !get {
		return nil, invalid("only a GET takes `query`: this operation takes its arguments in `body`.")
	}
	q := url.Values{}
	for k, rv := range vals {
		v, ok := rv.(string)
		switch {
		case !queryKey().MatchString(k):
			return nil, invalid("`query.%s` is not a parameter's name.", k)
		case !ok || len(v) > maxQueryValueBytes || strings.ContainsFunc(v, unicode.IsControl):
			return nil, invalid("`query.%s` is text of at most %d bytes.", k, maxQueryValueBytes)
		}
		q.Set(k, v)
	}
	return q, nil
}

// place decides the ad account m acts on, among the seat's, or the
// organization, and the connection and its roles there.
func (p *Product) place(ctx context.Context, m *made, args map[string]any) error {
	if orgOps[m.targets[0].op.ID] {
		name, _ := args["connection"].(string)
		c, err := p.connNamed(name)
		if err != nil {
			return err
		}
		accounts, err := p.aclsOf(ctx, c)
		if err != nil {
			return said(err)
		}
		// AN ORGANIZATION CALL HOLDS THE ROLES the user holds on the seat's own
		// ad accounts, never those it holds on accounts the seat may not touch.
		for _, a := range accounts {
			if slices.Contains(m.seat.AdAccounts, a.ID) {
				for _, r := range a.Roles {
					if !slices.Contains(m.roles, r) {
						m.roles = append(m.roles, r)
					}
				}
			}
		}
		m.conn = c
		return nil
	}
	account, _ := args["account"].(string)
	if account == "" && len(m.seat.AdAccounts) == 1 {
		account = m.seat.AdAccounts[0]
	}
	switch {
	case account == "":
		return invalid("name the ad account in `account`: %s.", words.Or(m.seat.AdAccounts))
	case !slices.Contains(m.seat.AdAccounts, account):
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("ad account %s is not one of this agent's (%s). A "+
			"person adds it on the integration's Agents tab. No call was made.", account, words.And(m.seat.AdAccounts)))
	}
	for _, t := range m.targets {
		if t.op.ID == "adAccountsIdGet" || t.op.ID == "adAccountsIdPut" {
			if _, id := typeAndID(t.path); id != account {
				return invalid("%s acts on the ad account `account` names: path.id is %s.", t.op.ID, account)
			}
		}
	}
	c, acct, err := p.reach(ctx, account)
	if err != nil {
		return said(err)
	}
	m.conn, m.account, m.roles = c, account, acct.Roles
	return nil
}

// typeAndID are a path's resource and its ID, "" for a collection.
func typeAndID(path string) (typ, id string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 {
		typ = parts[1]
	}
	if len(parts) == 3 {
		id, _ = url.PathUnescape(parts[2])
	}
	return typ, id
}

// rolesNames are roles as setup names them.
func rolesNames(ids []string) []string {
	var out []string
	for _, r := range roles() {
		if slices.Contains(ids, r.ID) {
			out = append(out, r.Name)
		}
	}
	if len(out) == 0 {
		return []string{"no API role"}
	}
	return out
}
