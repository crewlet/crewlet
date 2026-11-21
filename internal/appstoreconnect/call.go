package appstoreconnect

import (
	"context"
	"encoding/json"
	"fmt"
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
	app  string
	team *team
	slot string
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
	op, ok := Catalog().Operation(id)
	if !ok {
		return made{}, invalid("%q is not an App Store Connect operation: %s lists them.", id, ToolOperations)
	}
	if op.Class != class && !slices.ContainsFunc(op.States, func(s growth.StateRule) bool { return s.Class == class }) {
		return made{}, invalid("%s is a %s operation: call it with %s.", op.ID, op.Class, toolFor(op.Class))
	}
	m := made{op: op, class: class, query: url.Values{}}
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
	if err := p.place(ctx, seat, &m, args); err != nil {
		return made{}, err
	}
	slot, ok := keyFor(op, m.team.slots, class == growth.ClassAdmin)
	if !ok {
		return made{}, noKey(op, m.team.slots)
	}
	m.slot = slot
	if class != op.Class {
		if err := p.needState(ctx, &m); err != nil {
			return made{}, err
		}
	}
	return m, nil
}

// noKey is the refusal of a call no connected key may make, saying why where
// the Admin key could: it is kept for account changes an administrator allows.
func noKey(op growth.Operation, connected []string) error {
	others := slices.DeleteFunc(slices.Clone(op.Roles), func(r string) bool { return r == RoleAdmin })
	switch {
	case slices.Contains(connected, adminSlot) && len(rolesNames(others)) == 0:
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("only the Admin role may make %s, and Crewlet "+
			"uses the Admin key for account changes an administrator allows alone. No call was made.", op.ID))
	case slices.Contains(connected, adminSlot):
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s needs a key with the %s role: Crewlet uses "+
			"the Admin key for account changes an administrator allows alone. A person adds one on the "+
			"integration's page. No call was made.", op.ID, words.Or(rolesNames(others))))
	}
	return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("no connected key may make %s: it needs a key with "+
		"the %s role. A person adds one on the integration's page. No call was made.", op.ID,
		words.Or(rolesNames(op.Roles))))
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

// place decides the app m acts on, or that it is a team call, and its team.
func (p *Product) place(ctx context.Context, seat *config.RoleAppStoreConnect, m *made, args map[string]any) error {
	typ, id := typeAndID(m.path)
	switch {
	case id != "":
		return p.placeResource(ctx, seat, m, typ, id, args)
	case m.op.Method == "POST":
		ref, ok := bodyTarget(m.body)
		if !ok {
			return p.placeTeam(seat, m, args)
		}
		return p.placeResource(ctx, seat, m, ref.Type, ref.ID, args)
	case typ == "apps":
		// THE TEAM'S APP LIST, filtered to the seat's apps when answered.
		return p.placeAnyApp(ctx, seat, m)
	case m.query.Get("filter[app]") != "":
		return p.placeApp(ctx, seat, m, m.query.Get("filter[app]"))
	}
	if _, owned := downward[typ]; owned {
		return invalid("a list of %s names its app: add query filter[app] with one of this agent's apps, or read it "+
			"through the app (operation apps_..._getToManyRelated).", typ)
	}
	return p.placeTeam(seat, m, args)
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

func (p *Product) placeResource(ctx context.Context, seat *config.RoleAppStoreConnect, m *made, typ, id string,
	args map[string]any) error {
	if typ == "apps" {
		return p.placeApp(ctx, seat, m, id)
	}
	if _, owned := downward[typ]; !owned {
		return p.placeTeam(seat, m, args)
	}
	if app, ok := p.owners.get(typ, id); ok {
		return p.placeApp(ctx, seat, m, app)
	}
	for _, app := range seat.Apps {
		t, err := p.teamOf(ctx, app)
		if err != nil {
			continue
		}
		pl := &placer{owners: p.owners, list: teamLister{t: t}, down: downward}
		found, err := pl.appOf(ctx, typ, id, []string{app})
		if err == nil && found == app {
			return p.placeApp(ctx, seat, m, app)
		}
	}
	return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s %s is in none of this agent's apps (%s), as far as the "+
		"engine can find: read it through its app's list first. No call was made.", typ, id, words.And(seat.Apps)))
}

func (p *Product) placeApp(ctx context.Context, seat *config.RoleAppStoreConnect, m *made, app string) error {
	if !slices.Contains(seat.Apps, app) {
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("app %s is not one of this agent's apps (%s). A "+
			"person adds it on the integration's Agents tab. No call was made.", app, words.And(seat.Apps)))
	}
	t, err := p.teamOf(ctx, app)
	if err != nil {
		return err
	}
	m.app, m.team = app, t
	return nil
}

// placeAnyApp is the team of the seat's first reachable app, for the app list.
func (p *Product) placeAnyApp(ctx context.Context, seat *config.RoleAppStoreConnect, m *made) error {
	for _, app := range seat.Apps {
		if t, err := p.teamOf(ctx, app); err == nil {
			m.team = t
			return nil
		}
	}
	return growth.Refuse(growth.RefusedNotFound, "none of this agent's apps is in a connected team. No call was made.")
}

func (p *Product) placeTeam(seat *config.RoleAppStoreConnect, m *made, args map[string]any) error {
	if !seat.TeamResources {
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s reaches the team's resources no app owns, "+
			"and this agent's access does not include them (team_resources). No call was made.", m.op.ID))
	}
	name, _ := args["connection"].(string)
	t, err := p.teamNamed(name)
	if err != nil {
		return err
	}
	m.team = t
	return nil
}

// teamLister lists related resources through a team's least-privileged reader.
type teamLister struct{ t *team }

func (l teamLister) listPage(ctx context.Context, path, cursor string) ([]resourceRef, string, error) {
	slot, ok := readerOf(l.t, path)
	if !ok {
		return nil, "", fmt.Errorf("%w: reading %s", ErrNoKey, path)
	}
	q := url.Values{"limit": {"200"}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	resp, err := l.t.client.Do(ctx, slot, Request{Method: "GET", Path: path, Query: q, Read: true, Reserved: true})
	if err != nil {
		return nil, "", err
	}
	var doc struct {
		Data  []resourceRef `json:"data"`
		Links struct {
			Next string `json:"next"`
		} `json:"links"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return nil, "", fmt.Errorf("appstoreconnect: read %s: %w", path, err)
	}
	return doc.Data, cursorOf(doc.Links.Next), nil
}

// readByTemplate is every read operation by its path template.
var readByTemplate = sync.OnceValue(func() map[string]growth.Operation {
	out := map[string]growth.Operation{}
	for _, op := range operations {
		if op.Method == "GET" {
			out[op.Path] = op
		}
	}
	return out
})

// readerOf is the least-privileged connected key, never the Admin key, whose
// role may read path: a read, a placement search or a target's state.
func readerOf(t *team, path string) (string, bool) {
	parts := strings.Split(path, "/")
	if len(parts) >= 4 {
		parts[3] = "{id}"
	}
	op, ok := readByTemplate()[strings.Join(parts, "/")]
	if !ok {
		return "", false
	}
	return keyFor(op, t.slots, false)
}
