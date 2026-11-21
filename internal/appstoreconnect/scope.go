package appstoreconnect

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// A seat names its apps; every call is held to them (package doc, "Scope").
// A resource type an app owns is generated from Apple's document
// (ownedTypes); every owned resource a call names is placed in one of the
// seat's apps, and any other resource it names needs team_resources.

// ErrUnplaced is a resource the engine could not place in any of the seat's
// apps: nothing is sent.
var ErrUnplaced = errors.New("appstoreconnect: the resource is in none of this agent's apps")

// teamOwned is the app of an owned resource in no app (a bundle ID no app
// uses): the team's.
const teamOwned = ""

// owners is a bounded cache of resource to app, per node.
type owners struct {
	mu    sync.Mutex
	max   int
	order *list.List
	byKey map[string]*list.Element
}

type owned struct{ key, app string }

func newOwners(max int) *owners {
	return &owners{max: max, order: list.New(), byKey: map[string]*list.Element{}}
}

// maxOwners bounds the cache: ids of the resources agents read in a day on a
// busy team, at about 100 bytes each, within a few MiB.
const maxOwners = 50_000

func ownerKey(typ, id string) string { return typ + "/" + id }

func (o *owners) put(typ, id, app string) {
	if typ == "" || id == "" || app == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	k := ownerKey(typ, id)
	if e, ok := o.byKey[k]; ok {
		e.Value = owned{key: k, app: app}
		o.order.MoveToFront(e)
		return
	}
	o.byKey[k] = o.order.PushFront(owned{key: k, app: app})
	for o.order.Len() > o.max {
		last := o.order.Back()
		o.order.Remove(last)
		delete(o.byKey, last.Value.(owned).key)
	}
}

func (o *owners) get(typ, id string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e, ok := o.byKey[ownerKey(typ, id)]
	if !ok {
		return "", false
	}
	o.order.MoveToFront(e)
	return e.Value.(owned).app, true
}

// remember records the owned resources of an answer to a request rooted at
// app through owned hops as app's; any other resource is never remembered.
func (o *owners) remember(app string, body []byte) {
	if app == "" {
		return
	}
	for _, r := range resourcesOf(body) {
		if ownedTypes[r.Type] {
			o.put(r.Type, r.ID, app)
		}
	}
}

type resourceRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// resourcesOf are an answer's resources, its data and included.
func resourcesOf(body []byte) []resourceRef {
	var doc struct {
		Data     json.RawMessage `json:"data"`
		Included []resourceRef   `json:"included"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	return append(refsOf(doc.Data), doc.Included...)
}

// refsOf reads JSON:API data that is one resource, a list or null.
func refsOf(raw json.RawMessage) []resourceRef {
	var many []resourceRef
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	var one *resourceRef
	if json.Unmarshal(raw, &one) == nil && one != nil && one.Type != "" {
		return []resourceRef{*one}
	}
	return nil
}

// upLink is an owned type's instance read and the to-one relationships it
// includes toward its app, nearest first (topology_gen.go).
type upLink struct {
	path string
	rels []string
}

// hop is one related read below a parent; one answers a resource, not a list.
type hop struct {
	path string
	one  bool
}

// maxUp bounds an upward walk: the deepest owned type is six to-one links
// below its app.
const maxUp = 8

// searchPages bounds the pages one hop of a search reads per parent: twenty
// pages of 200 is 4,000 children, past which the agent is told to read the
// resource through its app's list first.
const searchPages = 20

// errElsewhere is a resource its team does not hold: another team may.
var errElsewhere = errors.New("appstoreconnect: the team does not hold the resource")

// placer places owned resources through one team's readers, never the Admin key.
type placer struct {
	owners *owners
	t      *team
}

// appOf is the app owned resource typ/id is in, [teamOwned] for one in no
// app, or [errElsewhere]: read up its to-one links where Apple's document has
// one, else searched down from apps through owned types alone.
func (pl placer) appOf(ctx context.Context, typ, id string, apps []string) (string, error) {
	var walked []resourceRef
	found := func(app string) (string, error) {
		for _, r := range walked {
			pl.owners.put(r.Type, r.ID, app)
		}
		return app, nil
	}
	for range maxUp {
		if typ == "apps" {
			return found(id)
		}
		if a, ok := pl.owners.get(typ, id); ok {
			return found(a)
		}
		link, ok := upward[typ]
		if !ok {
			a, err := pl.search(ctx, typ, id, apps)
			if err != nil {
				return "", err
			}
			walked = append(walked, resourceRef{Type: typ, ID: id})
			return found(a)
		}
		parent, err := pl.up(ctx, link, typ, id)
		if err != nil || parent == nil {
			if err == nil {
				// EVERY LINK TO AN APP IS EMPTY: the resource is in no app.
				return found(teamOwned)
			}
			return "", err
		}
		walked = append(walked, resourceRef{Type: typ, ID: id})
		typ, id = parent.Type, parent.ID
	}
	return "", fmt.Errorf("%w: %s %s", ErrUnplaced, typ, id)
}

// up reads typ/id's first non-empty to-one link toward its app.
func (pl placer) up(ctx context.Context, link upLink, typ, id string) (*resourceRef, error) {
	path := strings.Replace(link.path, "{id}", url.PathEscape(id), 1)
	slot, ok := readerOf(pl.t, path)
	if !ok {
		return nil, fmt.Errorf("%w: placing %s %s", ErrNoKey, typ, id)
	}
	rels := strings.Join(link.rels, ",")
	resp, err := pl.t.client.Do(ctx, slot, Request{Method: "GET", Path: path, Read: true, Reserved: true,
		Query: url.Values{"include": {rels}, "fields[" + typ + "]": {rels}}})
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil, errElsewhere
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Data struct {
			Relationships map[string]struct {
				Data json.RawMessage `json:"data"`
			} `json:"relationships"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return nil, fmt.Errorf("appstoreconnect: read %s %s: %w", typ, id, err)
	}
	for _, rel := range link.rels {
		if refs := refsOf(doc.Data.Relationships[rel].Data); len(refs) == 1 && refs[0].ID != "" {
			return &refs[0], nil
		}
	}
	return nil, nil
}

// search walks typ's related reads below each of apps looking for typ/id,
// remembering the owned resources it reads as their app's.
func (pl placer) search(ctx context.Context, typ, id string, apps []string) (string, error) {
	chain, ok := downward[typ]
	if !ok {
		return "", fmt.Errorf("%w: %s %s cannot be placed in an app", ErrUnplaced, typ, id)
	}
	for _, app := range apps {
		parents := []string{app}
		for depth, h := range chain {
			var next []string
			for _, parent := range parents {
				refs, err := pl.children(ctx, h, parent)
				if err != nil {
					return "", err
				}
				for _, r := range refs {
					pl.owners.put(r.Type, r.ID, app)
					if depth == len(chain)-1 && r.Type == typ && r.ID == id {
						return app, nil
					}
					next = append(next, r.ID)
				}
			}
			parents = next
		}
	}
	return "", errElsewhere
}

// children are the resources hop reads below parent: one, or a list paged.
func (pl placer) children(ctx context.Context, h hop, parent string) ([]resourceRef, error) {
	path := strings.Replace(h.path, "{id}", url.PathEscape(parent), 1)
	slot, ok := readerOf(pl.t, path)
	if !ok {
		return nil, fmt.Errorf("%w: reading %s", ErrNoKey, path)
	}
	var out []resourceRef
	cursor := ""
	for range searchPages {
		q := url.Values{}
		if !h.one {
			q.Set("limit", "200")
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		resp, err := pl.t.client.Do(ctx, slot, Request{Method: "GET", Path: path, Query: q, Read: true, Reserved: true})
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var doc struct {
			Data  json.RawMessage `json:"data"`
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		}
		if err := json.Unmarshal(resp.Body, &doc); err != nil {
			return nil, fmt.Errorf("appstoreconnect: read %s: %w", path, err)
		}
		// A TO-ONE HOP ANSWERS ONE RESOURCE, or null; a list hop a page.
		out = append(out, refsOf(doc.Data)...)
		if cursor = cursorOf(doc.Links.Next); cursor == "" || h.one {
			break
		}
	}
	return out, nil
}

// placeOwned is the app among the seat's that owned resource typ/id is in,
// and its team: [teamOwned] for one in no app, a refusal for one elsewhere.
func (p *Product) placeOwned(ctx context.Context, seat *config.RoleAppStoreConnect, typ, id string) (string, *team,
	error) {
	byTeam := map[*team][]string{}
	var order []*team
	for _, app := range seat.Apps {
		t, err := p.teamOf(ctx, app)
		if err != nil {
			continue
		}
		if _, seen := byTeam[t]; !seen {
			order = append(order, t)
		}
		byTeam[t] = append(byTeam[t], app)
	}
	for _, t := range order {
		app, err := placer{owners: p.owners, t: t}.appOf(ctx, typ, id, byTeam[t])
		switch {
		case errors.Is(err, errElsewhere):
			continue
		case errors.Is(err, ErrNoKey):
			return "", nil, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("no connected key but Admin may "+
				"read %s %s to find its app, and Crewlet never reads with the Admin key. No call was made.", typ, id))
		case err != nil:
			return "", nil, said(err)
		case app != teamOwned && !slices.Contains(seat.Apps, app):
			continue
		}
		return app, t, nil
	}
	return "", nil, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s %s is in none of this agent's apps (%s), "+
		"as far as the engine can find: read it through its app's list first. No call was made.", typ, id,
		words.And(seat.Apps)))
}

// cutApps removes from an answer every apps resource, and every link to one,
// outside allowed.
func cutApps(body []byte, allowed []string) ([]byte, error) {
	var doc map[string]any
	// NUMBERS KEPT AS WRITTEN: a float would round an ID or a size past 2^53.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("appstoreconnect: read the answer: %w", err)
	}
	outside := func(v any) bool {
		r, _ := v.(map[string]any)
		t, _ := r["type"].(string)
		id, _ := r["id"].(string)
		return t == "apps" && !slices.Contains(allowed, id)
	}
	cutLinks := func(v any) {
		r, _ := v.(map[string]any)
		rels, _ := r["relationships"].(map[string]any)
		for _, rel := range rels {
			m, _ := rel.(map[string]any)
			switch d := m["data"].(type) {
			case []any:
				m["data"] = slices.DeleteFunc(d, outside)
			case map[string]any:
				if outside(d) {
					m["data"] = nil
				}
			}
		}
	}
	for _, key := range []string{"data", "included"} {
		switch d := doc[key].(type) {
		case []any:
			d = slices.DeleteFunc(d, outside)
			for _, r := range d {
				cutLinks(r)
			}
			doc[key] = d
		case map[string]any:
			if outside(d) {
				return nil, growth.Refuse(growth.RefusedForbidden, "the answer is an app outside this agent's "+
					"apps. Nothing is shown.")
			}
			cutLinks(d)
		}
	}
	return json.Marshal(doc)
}

// namesOwned reports whether an answer holds or links to an owned resource.
func namesOwned(body []byte) bool {
	var doc struct {
		Data     json.RawMessage   `json:"data"`
		Included []json.RawMessage `json:"included"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return true
	}
	var all []json.RawMessage
	if json.Unmarshal(doc.Data, &all) != nil {
		all = []json.RawMessage{doc.Data}
	}
	for _, raw := range append(all, doc.Included...) {
		var r struct {
			Type          string `json:"type"`
			Relationships map[string]struct {
				Data json.RawMessage `json:"data"`
			} `json:"relationships"`
		}
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		if ownedTypes[r.Type] {
			return true
		}
		for _, rel := range r.Relationships {
			if slices.ContainsFunc(refsOf(rel.Data), func(x resourceRef) bool { return ownedTypes[x.Type] }) {
				return true
			}
		}
	}
	return false
}

// covers reports whether the seat's apps are every app t reaches.
func (p *Product) covers(ctx context.Context, seat *config.RoleAppStoreConnect, t *team) (bool, error) {
	apps, err := p.appsOf(ctx, t)
	if err != nil {
		return false, err
	}
	return !slices.ContainsFunc(apps, func(a appInfo) bool { return !slices.Contains(seat.Apps, a.ID) }), nil
}
