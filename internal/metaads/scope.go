package metaads

import (
	"container/list"
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
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
)

// A seat names its ad accounts; every object a request names is held to them
// (package doc, "Scope"). An account's object is placed by the account_id Meta
// answers for it, read from the object itself and kept in a bounded cache,
// since an object never moves between ad accounts; never from a list, which
// can cross them.

// owners is a bounded cache of object ID to its ad account, per node.
type owners struct {
	mu    sync.Mutex
	max   int
	order *list.List
	byID  map[string]*list.Element
}

type owned struct{ id, account string }

func newOwners(max int) *owners {
	return &owners{max: max, order: list.New(), byID: map[string]*list.Element{}}
}

// maxOwners bounds the cache: the objects agents name in a day on a busy
// company, at about 60 bytes each, within a few MiB.
const maxOwners = 50_000

func (o *owners) put(id, account string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if e, ok := o.byID[id]; ok {
		o.order.MoveToFront(e)
		return
	}
	o.byID[id] = o.order.PushFront(owned{id: id, account: account})
	for o.order.Len() > o.max {
		last := o.order.Back()
		o.order.Remove(last)
		delete(o.byID, last.Value.(owned).id)
	}
}

func (o *owners) get(id string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e, ok := o.byID[id]
	if !ok {
		return "", false
	}
	o.order.MoveToFront(e)
	return e.Value.(owned).account, true
}

// ErrNotOwned is an object Meta answers no ad account for.
var ErrNotOwned = errors.New("metaads: the object names no ad account")

// ownerOf is the ad account object id belongs to, read from the object.
func (p *Product) ownerOf(ctx context.Context, c *conn, id string) (string, error) {
	if a, ok := p.owners.get(id); ok {
		return a, nil
	}
	tok, err := p.tokenFor(ctx, c, growth.ClassRead, []string{"ads_read"})
	if err != nil {
		return "", err
	}
	resp, err := c.client.Do(ctx, tok.valueNow(), Request{Method: http.MethodGet, Path: "/" + url.PathEscape(id),
		Params: url.Values{"fields": {"account_id"}}, Read: true, Reserved: true})
	var apiErr *APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Code == 100:
		// NO SUCH FIELD on this node type: it is no ad account's object.
		return "", ErrNotOwned
	case err != nil:
		return "", err
	}
	var doc struct {
		AccountID string `json:"account_id"`
	}
	if json.Unmarshal(resp.Body, &doc) != nil || doc.AccountID == "" {
		return "", ErrNotOwned
	}
	account := config.MetaAdAccountID(doc.AccountID)
	if account == "" {
		return "", ErrNotOwned
	}
	p.owners.put(id, account)
	return account, nil
}

// valueNow is the token in use, as its last check read it.
func (t *token) valueNow() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value == "" {
		return t.pasted
	}
	return t.value
}

// The shapes of the IDs a request names.
var (
	accountShape = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^act_[0-9]{1,20}$`) })
	graphID      = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{1,24}(_[0-9]{1,24})?$`) })
)

// accountUses are the parameters naming what no ad account owns but an ad
// account is connected to, with the account's edge that lists them: a Page it
// may advertise for, an app it may promote, its pixels and Instagram accounts.
var accountUses = map[string]string{"page_id": "/promote_pages", "application_id": "/advertisable_applications",
	"pixel_id": "/adspixels", "instagram_user_id": "/instagram_accounts", "instagram_actor_id": "/instagram_accounts",
	"object_id": "/promote_pages"}

// nonGraph are the ID-named parameters that name no Graph object: a catalog
// item's retailer ID, an event's own ID, a client's.
var nonGraph = map[string]bool{"retailer_id": true, "external_id": true, "event_id": true, "external_event_id": true,
	"client_id": true, "order_id": true, "item_group_id": true, "partner_id": true, "external_campaign_id": true,
	"transaction_id": true, "lead_id": false}

// maxRefs bounds the objects one request may name: each is read once before
// the request is made.
const maxRefs = 200

// ref is one object a request names beyond its path.
type ref struct {
	key, id string
}

// refsIn are the objects params name: every ad account ID anywhere, every
// value of a parameter named for an object, and every ID a filter compares an
// object's ID to.
func refsIn(params map[string]any) []ref {
	var out []ref
	seen := map[string]bool{}
	add := func(key string, v any) {
		id := strings.TrimSpace(fmt.Sprint(v))
		if v == nil || id == "" || seen[key+"/"+id] {
			return
		}
		seen[key+"/"+id] = true
		out = append(out, ref{key: key, id: id})
	}
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch x := v.(type) {
		case string:
			if accountShape().MatchString(x) {
				add("account_id", x)
				return
			}
			if t := strings.TrimSpace(x); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
				var parsed any
				if json.Unmarshal([]byte(t), &parsed) == nil {
					walk(key, parsed)
					return
				}
			}
			if named(key) && graphID().MatchString(x) {
				add(key, x)
			}
		case json.Number, float64:
			if s := fmt.Sprint(x); named(key) && graphID().MatchString(s) {
				add(key, s)
			}
		case []any:
			for _, item := range x {
				walk(key, item)
			}
		case map[string]any:
			if field, _ := x["field"].(string); field != "" && x["value"] != nil {
				// A FILTER on an object's ID: {"field": "campaign.id", "value": [...]}.
				if strings.HasSuffix(field, ".id") || strings.HasSuffix(field, "_id") {
					walk(strings.ReplaceAll(strings.TrimSuffix(field, ".id"), ".", "_")+"_id", x["value"])
				}
			}
			for k, child := range x {
				walk(k, child)
			}
		}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		walk(k, params[k])
	}
	return out
}

// named reports whether a parameter's name says it holds a Graph object's ID.
func named(key string) bool {
	if nonGraph[key] {
		return false
	}
	return key == "id" || key == "ids" || strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_ids")
}

// checkRefs holds every object params name to the seat: an ad account to its
// accounts, an account's object to the call's account, an object an ad account
// is connected to to that account's own list, anything else to a seat given
// business_resources.
func (p *Product) checkRefs(ctx context.Context, m *made, params map[string]any) error {
	refs := refsIn(params)
	if len(refs) > maxRefs {
		return invalid("the request names %d objects, and at most %d are checked before it is made.", len(refs), maxRefs)
	}
	accounts := seatAccounts(m.seat)
	for _, r := range refs {
		if r.key == "account_id" || r.key == "ad_account_id" || accountShape().MatchString(r.id) {
			id := config.MetaAdAccountID(r.id)
			if id == "" || !slices.Contains(accounts, id) {
				return outside(r.id, accounts)
			}
			if m.account != "" && id != m.account {
				return invalid("the request acts on ad account %s and names ad account %s: one ad account a call.", m.account, id)
			}
			continue
		}
		if strings.HasPrefix(r.id, "$") {
			continue
		}
		if edge, ok := accountUses[r.key]; ok && m.account != "" {
			if err := p.connected(ctx, m, edge, r); err != nil {
				return err
			}
			continue
		}
		account, err := p.ownerOf(ctx, m.conn, r.id)
		switch {
		case errors.Is(err, ErrNotOwned):
			if !m.seat.BusinessResources {
				return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s %s is no ad account's, and this agent's "+
					"access does not reach what no ad account owns (business_resources). No call was made.", r.key, r.id))
			}
			continue
		case err != nil:
			return said(err)
		case !slices.Contains(accounts, account):
			return outside(account, accounts)
		case m.account != "" && account != m.account:
			return invalid("%s %s is ad account %s's, and the request acts on ad account %s.", r.key, r.id, account, m.account)
		}
	}
	return nil
}

// usesTTL is how long an ad account's connected Pages, apps, pixels and
// Instagram accounts are reused: they are connected rarely.
const usesTTL = 10 * time.Minute

// uses caches each ad account's connected objects, by account and edge.
type uses struct {
	ids []string
	at  time.Time
}

// connected refuses r unless it is among m's ad account's objects on edge.
func (p *Product) connected(ctx context.Context, m *made, edge string, r ref) error {
	key := m.account + edge
	m.conn.mu.Lock()
	u, ok := m.conn.uses[key]
	m.conn.mu.Unlock()
	if !ok || p.deps.Now().Sub(u.at) >= usesTTL {
		tok, err := p.tokenFor(ctx, m.conn, growth.ClassRead, []string{"ads_read"})
		if err != nil {
			return err
		}
		var ids []string
		err = p.pages(ctx, m.conn, tok, "/"+m.account+edge, url.Values{"fields": {"id"}, "limit": {"100"}},
			objectOf(m.account), true, maxUsePages, func(raw json.RawMessage) error {
				var d struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(raw, &d) == nil && d.ID != "" {
					ids = append(ids, d.ID)
				}
				return nil
			})
		if err != nil {
			return said(err)
		}
		u = uses{ids: ids, at: p.deps.Now()}
		m.conn.mu.Lock()
		m.conn.uses[key] = u
		m.conn.mu.Unlock()
	}
	if !slices.Contains(u.ids, r.id) {
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s %s is not connected to ad account %s (its %s), "+
			"so this agent may not name it. A person connects it in Business Settings. No call was made.",
			r.key, r.id, m.account, strings.TrimPrefix(edge, "/")))
	}
	return nil
}

// maxUsePages bounds one connected-object list: 10 pages of 100.
const maxUsePages = 10

// pages reads a list page by page, handing each item to fn: Meta's
// {"data": [...]} pages followed by their after cursor, or a single object an
// edge answers whole. The paging links, which carry the token, are never kept.
func (p *Product) pages(ctx context.Context, c *conn, tok *token, path string, query url.Values, object string,
	reserved bool, maxPages int, fn func(json.RawMessage) error) error {
	q := url.Values{}
	for k, v := range query {
		q[k] = slices.Clone(v)
	}
	for range maxPages {
		resp, err := c.client.Do(ctx, tok.valueNow(), Request{Method: http.MethodGet, Path: path, Params: q,
			Object: object, Read: true, Reserved: reserved})
		if err != nil {
			return err
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(resp.Body, &top); err != nil {
			return fmt.Errorf("metaads: read %s: %w", path, err)
		}
		var doc struct {
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Next string `json:"next"`
			} `json:"paging"`
		}
		_ = json.Unmarshal(resp.Body, &doc)
		var items []json.RawMessage
		data, list := top["data"]
		switch {
		case !list:
			// A SINGLE OBJECT: an edge that answers one, not a list.
			items = []json.RawMessage{resp.Body}
		case string(data) == "null":
		case json.Unmarshal(data, &items) != nil:
			items = []json.RawMessage{data}
		}
		for _, it := range items {
			if err := fn(it); err != nil {
				return err
			}
		}
		if doc.Paging.Next == "" || doc.Paging.Cursors.After == "" {
			return nil
		}
		q.Set("after", doc.Paging.Cursors.After)
	}
	return fmt.Errorf("%w: %s", errTooLong, path)
}

// errTooLong is a list past the pages one read takes.
var errTooLong = errors.New("metaads: the list is longer than one read takes")
