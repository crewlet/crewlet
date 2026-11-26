package metaads

import (
	"context"
	"encoding/json"
	"errors"
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
)

// The bounds of a call's arguments.
const (
	maxParamBytes = 64 << 10
	maxParams     = 100
	maxListLimit  = 500
	// maxQueryValue bounds a field list: every field of Meta's widest node
	// type, joined by commas, is under a kilobyte.
	maxQueryValue = 2000
	// maxTargets bounds the objects one pause or enable names: each is read
	// before and sent once, and a person reads every one on a card.
	maxTargets = 50
)

var (
	pathValue   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,128}$`) })
	paramKey    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`) })
	fieldList   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_,]*$`) })
	placeholder = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\{([a-z_]+)\}`) })
	digitsOnly  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{1,24}$`) })
)

// reservedParams are the parameters no tool sends: the credentials the engine
// adds itself, a method override, which would turn a read into a delete, a
// batch, which would reach anything, and a JSONP callback.
var reservedParams = []string{"access_token", "appsecret_proof", "appsecret_time", "method", "batch", "callback",
	"include_headers", "suppress_http_code"}

// target is one request of a call, made concrete.
type target struct {
	row    row
	path   string
	params map[string]any
	// node is the object the path names, "" for a root path.
	node string
	// chain names, by parameter, the earlier request whose answer's ID fills
	// it at send time: a curated tool's ad set made in the campaign it made.
	chain map[string]int
}

// made is one call made concrete: its requests, the ad account every one acts
// in ("" for a call on what no ad account owns) and the connection.
type made struct {
	tool    string
	targets []target
	conn    *conn
	account string
	seat    *config.RoleMetaAds
	// own is the seat's own system user's token, which signs the call; nil
	// for the company's ([Product.ownOf]).
	own *token
}

// toolClass is the class each tool calls with.
var toolClass = map[string]growth.Class{ToolOperations: growth.ClassRead, ToolRead: growth.ClassRead,
	ToolAccounts: growth.ClassRead, ToolCampaigns: growth.ClassRead, ToolAdSets: growth.ClassRead,
	ToolAds: growth.ClassRead, ToolInsights: growth.ClassRead, ToolOperate: growth.ClassOperate,
	ToolPause: growth.ClassOperate, ToolLowerBudget: growth.ClassOperate, ToolLowerBid: growth.ClassOperate,
	ToolCreateCampaign: growth.ClassOperate, ToolSpend: growth.ClassSpend, ToolEnable: growth.ClassSpend,
	ToolRaiseBudget: growth.ClassSpend, ToolRaiseBid: growth.ClassSpend, ToolTargeting: growth.ClassSpend,
	ToolCreateAd: growth.ClassSpend, ToolPublish: growth.ClassPublic, ToolAdmin: growth.ClassAdmin}

// genericTool is the generic tool each class is asked through.
var genericTool = map[growth.Class]string{growth.ClassRead: ToolRead, growth.ClassOperate: ToolOperate,
	growth.ClassSpend: ToolSpend, growth.ClassPublic: ToolPublish, growth.ClassAdmin: ToolAdmin}

// concrete makes args, a call of tool by handle, concrete: its requests held
// to their shapes, placed in one of the seat's ad accounts (or, for what no
// ad account owns, a seat given business_resources), with every object they
// name held to the seat.
func (p *Product) concrete(ctx context.Context, handle, tool string, args map[string]any) (made, error) {
	seat, err := p.seat(handle)
	if err != nil {
		return made{}, err
	}
	c, err := p.connNow()
	if err != nil {
		return made{}, err
	}
	// WHO SIGNS IS DECIDED FIRST: a seat whose own identity is refused is
	// refused before the engine reads anything for its call.
	own, err := p.ownOf(ctx, handle, seat)
	if err != nil {
		return made{}, err
	}
	m := made{tool: tool, seat: seat, conn: c, own: own}
	switch tool {
	case ToolPause:
		err = m.statuses(args, "PAUSED")
	case ToolEnable:
		err = m.statuses(args, "ACTIVE")
	case ToolLowerBudget, ToolRaiseBudget:
		err = p.budget(ctx, &m, args)
	case ToolLowerBid, ToolRaiseBid:
		err = p.bid(ctx, &m, args)
	case ToolTargeting:
		err = p.targeting(ctx, &m, args)
	case ToolCreateAd:
		err = m.createAd(args)
	case ToolCreateCampaign:
		err = p.createCampaign(ctx, &m, args)
	default:
		err = m.generic(args)
	}
	if err != nil {
		return made{}, err
	}
	if err := p.place(ctx, &m, args); err != nil {
		return made{}, err
	}
	for _, t := range m.targets {
		if err := p.checkRefs(ctx, &m, t.params); err != nil {
			return made{}, err
		}
	}
	return m, nil
}

// where names what m acts on, as a person reads it.
func (m made) where() string {
	if m.account == "" {
		return "the business portfolio's objects"
	}
	return "ad account " + m.account
}

// generic is a generic tool's one request.
func (m *made) generic(args map[string]any) error {
	id, _ := args["operation"].(string)
	def, _ := Catalog().Tool(m.tool)
	r, ok := rowOf()[id]
	if !ok || !slices.Contains(def.Operations, id) {
		return Catalog().Outside(m.tool, id, growth.LevelOf(m.seat.Level))
	}
	t := target{row: r}
	var err error
	if t.path, t.node, err = fillPath(r.op.Path, args["path"]); err != nil {
		return err
	}
	if t.params, err = paramsOf(args["params"], r.op.Method); err != nil {
		return err
	}
	m.targets = []target{t}
	return nil
}

// fillPath is template with its placeholder replaced by its value, and the
// object the path names: act_ and digits for an ad account.
func fillPath(template string, raw any) (string, string, error) {
	vals, _ := raw.(map[string]any)
	used, node := 0, ""
	var bad error
	out := placeholder().ReplaceAllStringFunc(template, func(ph string) string {
		name := strings.Trim(ph, "{}")
		v, _ := vals[name].(string)
		v = strings.TrimSpace(v)
		if name == "ad_account_id" {
			v = strings.TrimPrefix(v, "act_")
			if !digitsOnly().MatchString(v) {
				bad = invalid("`path.ad_account_id` is the ad account's ID: act_ and its digits.")
				return ph
			}
			used, node = used+1, "act_"+v
			return v
		}
		if !pathValue().MatchString(v) {
			bad = invalid("`path.%s` is the object's ID: letters, digits and . _ ~ : - only.", name)
			return ph
		}
		used, node = used+1, v
		return url.PathEscape(v)
	})
	if bad != nil {
		return "", "", bad
	}
	if used != len(vals) {
		return "", "", invalid("`path` names a placeholder %s does not have.", template)
	}
	return out, node, nil
}

// paramsOf reads a call's parameters, held to Meta's shapes: no reserved
// parameter, no field expansion (which could follow an edge out of the
// seat's ad accounts), and no field naming a token or a secret.
func paramsOf(raw any, method string) (map[string]any, error) {
	params := map[string]any{}
	if raw == nil {
		return params, nil
	}
	vals, ok := raw.(map[string]any)
	if !ok {
		return nil, invalid("`params` is an object of the operation's parameters by name.")
	}
	if data, _ := json.Marshal(vals); len(data) > maxParamBytes || len(vals) > maxParams {
		return nil, invalid("`params` holds at most %d parameters in %d KiB.", maxParams, maxParamBytes>>10)
	}
	for k, v := range vals {
		switch {
		case !paramKey().MatchString(k):
			return nil, invalid("`params.%s` is not a parameter's name.", k)
		case slices.Contains(reservedParams, strings.ToLower(k)):
			return nil, invalid("`params.%s` is set by the engine, never by a call.", k)
		case k == "fields":
			s, _ := v.(string)
			if len(s) > maxQueryValue || !fieldList().MatchString(s) {
				return nil, invalid("`params.fields` is plain field names joined by commas: no expansion `{}`, " +
					"modifier `.x()` or space. Read an edge with its own operation.")
			}
			for _, f := range strings.Split(s, ",") {
				if strings.Contains(f, "token") || strings.Contains(f, "secret") {
					return nil, invalid("`params.fields` names %s, which no tool reads.", f)
				}
			}
		case k == "limit":
			n, err := limitOf(v)
			if err != nil || n < 1 || n > maxListLimit {
				return nil, invalid("`params.limit` is 1 to %d.", maxListLimit)
			}
		}
		if s, ok := v.(string); ok && strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
			return nil, invalid("`params.%s` holds a control character.", k)
		}
		params[k] = v
	}
	_ = method
	return params, nil
}

func limitOf(v any) (int, error) {
	switch t := v.(type) {
	case float64:
		return int(t), nil
	case json.Number:
		n, err := t.Int64()
		return int(n), err
	case string:
		var n int
		_, err := fmt.Sscan(t, &n)
		return n, err
	}
	return 0, errors.New("not a number")
}

// place decides the ad account m acts in: the one each request's path names,
// directly or by its object's account_id, which must be one of the seat's and
// the same for every request; or none, for what no ad account owns, which
// needs business_resources.
func (p *Product) place(ctx context.Context, m *made, args map[string]any) error {
	accounts := seatAccounts(m.seat)
	business := false
	for _, t := range m.targets {
		account, err := p.accountOfTarget(ctx, m, t)
		switch {
		case errors.Is(err, ErrNotOwned):
			business = true
			continue
		case err != nil:
			return err
		case !slices.Contains(accounts, account):
			return outside(account, accounts)
		case m.account != "" && m.account != account:
			return invalid("the call names ad accounts %s and %s: one ad account a call.", m.account, account)
		}
		m.account = account
	}
	if business {
		if m.account != "" {
			return invalid("the call mixes ad account %s's objects with what no ad account owns: one a call.", m.account)
		}
		if !m.seat.BusinessResources {
			return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s reaches what no ad account owns, and this "+
				"agent's access does not include it (business_resources). No call was made.", m.targets[0].row.op.ID))
		}
	}
	if named := config.MetaAdAccountID(fmt.Sprint(args["account"])); args["account"] != nil && named != "" &&
		m.account != "" && named != m.account {
		return invalid("`account` is %s, and the call acts on ad account %s.", named, m.account)
	}
	return nil
}

// accountOfTarget is the ad account t's path places it in, [ErrNotOwned] for
// a path naming what no ad account owns.
func (p *Product) accountOfTarget(ctx context.Context, m *made, t target) (string, error) {
	switch {
	case strings.HasPrefix(t.row.op.Path, "/act_{ad_account_id}"):
		return t.node, nil
	case t.node == "" || !accountOwned[t.row.op.Resource]:
		return "", ErrNotOwned
	}
	account, err := p.ownerOf(ctx, m.conn, t.node)
	if errors.Is(err, ErrNotOwned) {
		return "", growth.Refuse(growth.RefusedNotFound, fmt.Sprintf("%s %s answers no ad account, so it is placed "+
			"in none of this agent's. No call was made.", t.row.op.Resource, t.node))
	}
	if err != nil {
		return "", said(err)
	}
	return account, nil
}

// The levels the curated tools name.
const (
	levelCampaign = "campaign"
	levelAdSet    = "ad_set"
	levelAd       = "ad"
)

// levelOps are the update operation of each level a curated tool changes.
var levelOps = map[string]string{levelCampaign: opCampaignUpdate, levelAdSet: opAdSetUpdate, levelAd: opAdUpdate}

// update is level's update of id setting params.
func update(level, id string, params map[string]any) (target, error) {
	opID, ok := levelOps[level]
	if !ok {
		return target{}, invalid("`level` is campaign, ad_set or ad.")
	}
	if !digitsOnly().MatchString(id) {
		return target{}, invalid("%q is not a Meta object ID: its digits.", id)
	}
	return target{row: rowOf()[opID], path: "/" + id, node: id, params: params}, nil
}

// statuses are the pause and enable tools' updates: status set, one per ID.
func (m *made) statuses(args map[string]any, status string) error {
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
		t, err := update(level, id, map[string]any{"status": status})
		if err != nil {
			return err
		}
		m.targets = append(m.targets, t)
	}
	return nil
}

// errAnswer is a Meta answer the engine could not read.
var errAnswer = errors.New("metaads: Meta's answer could not be read")

// readObject reads id's fields with a reserved read through the read path.
func (p *Product) readObject(ctx context.Context, c *conn, id string, fields []string) (map[string]any, error) {
	tok, err := p.tokenFor(ctx, c, nil, growth.ClassRead, []string{"ads_read"})
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(ctx, tok.valueNow(), Request{Method: http.MethodGet, Path: "/" + url.PathEscape(id),
		Params: url.Values{"fields": {strings.Join(fields, ",")}}, Read: true, Reserved: true})
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := decode(resp.Body, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errAnswer, id, err)
	}
	return doc, nil
}

// decode reads JSON keeping numbers exact.
func decode(data []byte, into any) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.UseNumber()
	return d.Decode(into)
}
