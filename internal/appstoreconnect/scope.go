package appstoreconnect

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

// A seat names its apps; every call is held to them (package doc, "Scope").
// A resource is placed in its app by the request's own path where it names
// one, else by the owners cache, which every answer an agent was handed fills
// (an agent learns an ID only through a read the engine scoped), else by a
// bounded search down from each of the seat's apps through Apple's related
// lists. A resource no app owns is the team's.

// ErrUnplaced is a resource the engine could not place in any of the seat's
// apps: nothing is sent.
var ErrUnplaced = errors.New("appstoreconnect: the resource is in none of this agent's apps")

// teamOwned marks a type no app owns.
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

// remember records every resource of a JSON:API answer as app's.
func (o *owners) remember(app string, body []byte) {
	var doc struct {
		Data     json.RawMessage `json:"data"`
		Included []resourceRef   `json:"included"`
	}
	if app == "" || json.Unmarshal(body, &doc) != nil {
		return
	}
	var many []resourceRef
	if json.Unmarshal(doc.Data, &many) != nil {
		var one resourceRef
		if json.Unmarshal(doc.Data, &one) == nil {
			many = []resourceRef{one}
		}
	}
	for _, r := range append(many, doc.Included...) {
		o.put(r.Type, r.ID, app)
	}
}

type resourceRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// lister reads one page of a related list, answering its resources and the
// next page's cursor.
type lister interface {
	listPage(ctx context.Context, path string, cursor string) (refs []resourceRef, next string, err error)
}

// placer places resources in apps for one connection.
type placer struct {
	owners *owners
	list   lister
	// down is, per type, the related-list paths from an app down to it.
	down map[string][]string
}

// searchPages bounds the pages one hop of a search reads per parent: twenty
// pages of 200 is 4,000 children, past which the agent is told to read the
// resource through its app's list first.
const searchPages = 20

// appOf is the app of resource typ/id among apps, [teamOwned] for a type no
// app owns, or [ErrUnplaced].
func (p *placer) appOf(ctx context.Context, typ, id string, apps []string) (string, error) {
	if typ == "apps" {
		return id, nil
	}
	if a, ok := p.owners.get(typ, id); ok {
		return a, nil
	}
	chain, ok := p.down[typ]
	if !ok {
		return teamOwned, nil
	}
	for _, a := range apps {
		found, err := p.search(ctx, a, chain, typ, id)
		if err != nil {
			return "", err
		}
		if found {
			return a, nil
		}
	}
	return "", fmt.Errorf("%w: %s %s", ErrUnplaced, typ, id)
}

// search walks chain below app looking for typ/id, remembering what it reads.
func (p *placer) search(ctx context.Context, app string, chain []string, typ, id string) (bool, error) {
	parents := []string{app}
	for depth, tmpl := range chain {
		var next []string
		for _, parent := range parents {
			path := strings.Replace(tmpl, "{id}", url.PathEscape(parent), 1)
			cursor := ""
			for range searchPages {
				refs, more, err := p.list.listPage(ctx, path, cursor)
				if err != nil {
					return false, err
				}
				for _, r := range refs {
					p.owners.put(r.Type, r.ID, app)
					if depth == len(chain)-1 && r.Type == typ && r.ID == id {
						return true, nil
					}
					next = append(next, r.ID)
				}
				if more == "" {
					break
				}
				cursor = more
			}
		}
		parents = next
	}
	return false, nil
}
