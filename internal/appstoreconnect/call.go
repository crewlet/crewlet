package appstoreconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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

// The bounds of a generic call's arguments.
const (
	maxQueryValueBytes = 1000
	maxBodyBytes       = 64 << 10
	maxListLimit       = 200
)

var (
	pathValue = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,128}$`) })
	queryKey  = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,40}(\[[A-Za-z0-9_.]{1,60}\])?$`)
	})
	placeholder = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\{([A-Za-z]+)\}`) })
)

// made is one call made concrete.
type made struct {
	op    growth.Operation
	class growth.Class
	path  string
	query url.Values
	body  map[string]any
	// app is the app the call acts on, "" for a team call.
	app string
	// team is the team of key, the calling agent's own key, which signs the
	// call and every read the engine makes for it.
	team *team
	key  agentKey
	// level is the calling seat's level, which a refusal names the way up from.
	level growth.Level
}

// toolClass is the class each generic tool calls with.
var toolClass = map[string]growth.Class{ToolRead: growth.ClassRead, ToolOperate: growth.ClassOperate,
	ToolPublish: growth.ClassPublic, ToolAdmin: growth.ClassAdmin}

// toolFor is the generic tool a class is called through.
func toolFor(c growth.Class) string {
	for t, tc := range toolClass {
		if tc == c {
			return t
		}
	}
	return ToolAdmin
}

func invalid(format string, args ...any) error {
	return growth.Refuse(growth.RefusedInvalid, fmt.Sprintf(format, args...)+" No call was made.")
}

// concrete makes args, a call of tool by handle, concrete: its operation in
// the tool's class, its path and query held to their shapes, its app among the
// seat's, its team and key, and the state its class needs.
func (p *Product) concrete(ctx context.Context, handle, tool string, args map[string]any) (made, error) {
	seat, err := p.seat(handle)
	if err != nil {
		return made{}, err
	}
	class := toolClass[tool]
	id, _ := args["operation"].(string)
	op, _ := Catalog().Operation(id)
	if def, _ := Catalog().Tool(tool); !slices.Contains(def.Operations, id) {
		return made{}, Catalog().Outside(tool, id, growth.LevelOf(seat.Level))
	}
	m := made{op: op, class: class, query: url.Values{}, level: growth.LevelOf(seat.Level)}
	if m.path, err = fillPath(op.Path, args["path"]); err != nil {
		return made{}, err
	}
	if m.query, err = queryOf(args["query"]); err != nil {
		return made{}, err
	}
	if op.Method != "GET" && op.Method != "DELETE" {
		body, _ := args["body"].(map[string]any)
		if data, _ := json.Marshal(body); len(data) > maxBodyBytes || body == nil {
			return made{}, invalid("`body` is the JSON:API document of at most %d KiB the operation takes.", maxBodyBytes>>10)
		}
		m.body = body
	}
	if m.key, err = p.keyOf(ctx, handle); err != nil {
		return made{}, err
	}
	m.team = m.key.team
	if err := sign(&m, growth.LevelOf(seat.Level)); err != nil {
		return made{}, err
	}
	if err := p.place(ctx, seat, &m); err != nil {
		return made{}, err
	}
	if class != op.Class {
		if err := p.needState(ctx, &m); err != nil {
			return made{}, err
		}
	}
	return m, nil
}

// rolesNames are roles as setup names them, the Account Holder aside.
func rolesNames(ids []string) []string {
	var out []string
	for _, r := range roles() {
		if slices.Contains(ids, r.ID) {
			out = append(out, r.Name)
		}
	}
	return out
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
			bad = invalid("`path.%s` is the resource's ID: letters, digits and . _ ~ : - only.", name)
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

// queryOf reads the query, held to Apple's parameter shapes.
func queryOf(raw any) (url.Values, error) {
	vals, _ := raw.(map[string]any)
	q := url.Values{}
	for k, rv := range vals {
		v, ok := rv.(string)
		switch {
		case !queryKey().MatchString(k):
			return nil, invalid("`query.%s` is not a parameter's shape (filter[name], fields[type], include, sort, limit, cursor).", k)
		case !ok || len(v) > maxQueryValueBytes || strings.ContainsFunc(v, unicode.IsControl):
			return nil, invalid("`query.%s` is text of at most %d bytes.", k, maxQueryValueBytes)
		}
		q.Set(k, v)
	}
	if l := q.Get("limit"); l != "" {
		var n int
		if _, err := fmt.Sscan(l, &n); err != nil || n < 1 || n > maxListLimit {
			return nil, invalid("`query.limit` is 1 to %d.", maxListLimit)
		}
	}
	return q, nil
}

// typeAndID are a path's resource type and ID, "" for a collection.
func typeAndID(path string) (typ, id string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 {
		typ = parts[1]
	}
	if len(parts) >= 3 {
		id, _ = url.PathUnescape(parts[2])
	}
	return typ, id
}

// referenceTypes are Apple's own reference data, the same for every team: a
// price or availability naming a territory names nothing of the team's.
var referenceTypes = map[string]bool{"territories": true, "appCategories": true}

// place decides the app m acts on, or that it is a team call, in the team of
// the agent's key: every owned resource the call names is placed in one of
// the seat's apps, and any other resource it names needs team_resources.
func (p *Product) place(ctx context.Context, seat *config.RoleAppStoreConnect, m *made) error {
	named, err := namedBy(m)
	if err != nil {
		return err
	}
	teamWide := false
	for _, r := range named {
		app := r.ID
		switch {
		case r.Type == "apps":
		case topology().owned[r.Type]:
			if app, err = p.placeOwned(ctx, seat, m, r.Type, r.ID); err != nil {
				return err
			}
		case referenceTypes[r.Type]:
			continue
		default:
			teamWide = true
			continue
		}
		if app == teamOwned {
			teamWide = true
			continue
		}
		if m.app == "" {
			if err := p.placeApp(ctx, seat, m, app); err != nil {
				return err
			}
			continue
		}
		// A SECOND APP of the seat's is in the key's team too, or refused.
		if err := p.placeApp(ctx, seat, &made{team: m.team, key: m.key}, app); err != nil {
			return err
		}
	}
	typ, id := typeAndID(m.path)
	switch {
	case teamWide && !seat.TeamResources:
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s names the team's resources no app owns, "+
			"and this agent's access does not include them (team_resources). No call was made.", m.op.ID))
	case m.app != "" || teamWide:
		return nil
	case typ == "apps" && id == "":
		// THE TEAM'S APP LIST, cut to the seat's apps when answered.
		return nil
	case topology().owned[typ] && id == "":
		return invalid("a list of %s names its app: add query filter[app] with one of this agent's apps, or read it "+
			"through the app (operation apps_..._getToManyRelated).", typ)
	case !seat.TeamResources:
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s reaches the team's resources no app owns, "+
			"and this agent's access does not include them (team_resources). No call was made.", m.op.ID))
	}
	return nil
}

// namedBy is every resource m names: its path's, its query's ID filters' and
// its body's data and relationships', to-one and to-many alike.
func namedBy(m *made) ([]resourceRef, error) {
	var out []resourceRef
	add := func(typ, id string) error {
		// A LOCAL ID names a resource the body itself creates (`${...}`).
		if typ == "" || id == "" || strings.HasPrefix(id, "${") {
			return nil
		}
		if !pathValue().MatchString(id) {
			return invalid("%s ID %q is not an App Store Connect ID.", typ, id)
		}
		if !slices.Contains(out, resourceRef{Type: typ, ID: id}) {
			out = append(out, resourceRef{Type: typ, ID: id})
		}
		return nil
	}
	if err := add(typeAndID(m.path)); err != nil {
		return nil, err
	}
	filters := topology().idFilters[m.op.Path]
	names := slices.Sorted(maps.Keys(m.query))
	for _, k := range names {
		f, ok := strings.CutPrefix(k, "filter[")
		if typ := filters[strings.TrimSuffix(f, "]")]; ok && typ != "" {
			for _, id := range strings.Split(m.query.Get(k), ",") {
				if err := add(typ, strings.TrimSpace(id)); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, r := range bodyRefs(m.body) {
		if err := add(r.Type, r.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// bodyRefs are the resources a write body's data and its relationships name.
func bodyRefs(body map[string]any) []resourceRef {
	var out []resourceRef
	refs := func(v any) {
		items, ok := v.([]any)
		if !ok {
			items = []any{v}
		}
		for _, it := range items {
			r, _ := it.(map[string]any)
			t, _ := r["type"].(string)
			id, _ := r["id"].(string)
			out = append(out, resourceRef{Type: t, ID: id})
		}
	}
	refs(body["data"])
	data, _ := body["data"].(map[string]any)
	rels, _ := data["relationships"].(map[string]any)
	for _, n := range slices.Sorted(maps.Keys(rels)) {
		r, _ := rels[n].(map[string]any)
		refs(r["data"])
	}
	return out
}

// bodyTarget is the first resource a POST body's relationships name.
func bodyTarget(body map[string]any) (resourceRef, bool) {
	data, _ := body["data"].(map[string]any)
	rels, _ := data["relationships"].(map[string]any)
	names := make([]string, 0, len(rels))
	for n := range rels {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		r, _ := rels[n].(map[string]any)
		d, _ := r["data"].(map[string]any)
		t, _ := d["type"].(string)
		i, _ := d["id"].(string)
		if t != "" && i != "" {
			return resourceRef{Type: t, ID: i}, true
		}
	}
	return resourceRef{}, false
}

// placeApp places m in app: one of the seat's apps, in the team of its key.
func (p *Product) placeApp(ctx context.Context, seat *config.RoleAppStoreConnect, m *made, app string) error {
	if !slices.Contains(seat.Apps, app) {
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("app %s is not one of this agent's apps (%s). A "+
			"person adds it on the integration's Agents tab. No call was made.", app, words.And(seat.Apps)))
	}
	apps, err := p.appsOf(ctx, m.team, m.key.client)
	if err != nil {
		return said(err)
	}
	if !slices.ContainsFunc(apps, func(a appInfo) bool { return a.ID == app }) {
		return growth.Refuse(growth.RefusedNotFound, fmt.Sprintf("app %s is not in the %s team, whose key this "+
			"agent holds. No call was made.", app, m.team.id))
	}
	m.app = app
	return nil
}

// readByTemplate is every read operation by its path template.
var readByTemplate = sync.OnceValue(func() map[string]growth.Operation {
	out := map[string]growth.Operation{}
	for _, op := range operations() {
		if op.Method == "GET" {
			out[op.Path] = op
		}
	}
	return out
})

// mayRead reports whether role may make the read of path: a placement
// search or a target's state, made with the agent's own key. A read the table
// does not hold goes to Apple, which answers for the key itself.
func mayRead(role, path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) >= 4 {
		parts[3] = "{id}"
	}
	op, ok := readByTemplate()[strings.Join(parts, "/")]
	return !ok || growth.Permits(op, []string{role})
}
