package searchconsole

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// Run makes a read call; every other call is prepared and applied
// ([Product.Prepare], [Product.Apply]).
func (p *Product) Run(ctx context.Context, call growth.Call) (any, error) {
	switch call.Tool {
	case toolSitesList:
		return p.listSites(ctx, call)
	case toolSearchAnalytics:
		return p.searchAnalytics(ctx, call)
	case toolInspectURL:
		return p.inspect(ctx, call)
	case toolVerificationToken:
		return p.verificationToken(ctx, call)
	}
	req, class, err := p.requestOf(call)
	if err != nil {
		return nil, err
	}
	if class != growth.ClassRead {
		return nil, refusal(growth.RefusedInvalid, call.Tool+" changes Search Console, so it is prepared and "+
			"applied, never run as a read. No vendor was reached.")
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
// built from its typed arguments, a generic tool's from the operation it
// names. It reaches no vendor, so a deny or a lapse names it as built.
func (p *Product) requestOf(call growth.Call) (googlegrowth.Request, growth.Class, error) {
	site := argString(call.Args, "site")
	sites := p.sites(call.Handle)
	switch call.Tool {
	case toolSitemapsList:
		op, _ := p.Catalog().Operation("webmasters.sitemaps.list")
		var query map[string]string
		if index := argString(call.Args, "sitemap_index"); index != "" {
			query = map[string]string{"sitemapIndex": index}
		}
		req, err := newRequest(sites, op, map[string]string{"siteUrl": site}, query, nil)
		return req, growth.ClassRead, err
	case toolSitemapSubmit:
		op, _ := p.Catalog().Operation("webmasters.sitemaps.submit")
		req, err := newRequest(sites, op, map[string]string{"siteUrl": site,
			"feedpath": argString(call.Args, "sitemap")}, nil, nil)
		return req, growth.ClassOperate, err
	case toolIndexNotify:
		op, _ := p.Catalog().Operation("indexing.urlNotifications.publish")
		kind := argString(call.Args, "type")
		if kind == "" {
			kind = notifyUpdated
		}
		req, err := newRequest(sites, op, nil, nil, map[string]any{"url": argString(call.Args, "url"), "type": kind})
		return req, growth.ClassOperate, err
	case toolVerifySite:
		req, err := p.verifyRequest("siteVerification.webResource.insert", call.Handle, site,
			argString(call.Args, "method"))
		return req, growth.ClassAdmin, err
	case toolOwnersUpdate:
		req, err := p.ownersRequest(call, site)
		return req, growth.ClassAdmin, err
	}
	return p.generic(call)
}

// generic builds a generic tool's request and the class its tool makes. A
// method of another class is refused with the tool that may call it.
func (p *Product) generic(call growth.Call) (googlegrowth.Request, growth.Class, error) {
	tool, ok := p.Catalog().Tool(call.Tool)
	if !ok {
		return googlegrowth.Request{}, "", refusal(growth.RefusedNotFound, call.Tool+" is not a Search Console tool.")
	}
	id, _ := call.Args["operation"].(string)
	if !slices.Contains(tool.Operations, id) {
		return googlegrowth.Request{}, "", p.Catalog().Outside(call.Tool, id, p.Level(call.Handle))
	}
	op, _ := p.Catalog().Operation(id)
	req, err := newRequest(p.sites(call.Handle), op, stringMap(call.Args["path"]), stringMap(call.Args["query"]),
		objectArg(call.Args["body"]))
	return req, tool.Class, err
}

// cut is how an answer to req is cut to handle's sites: only where req lists
// properties beyond them.
func (p *Product) cut(handle string, req googlegrowth.Request) func(any) any {
	if !slices.Contains(listings, req.Op.ID) {
		return nil
	}
	sites := p.sites(handle)
	return func(v any) any { return cutToSites(sites, v) }
}

// cutToSites drops every property of a listing that is not one of sites: a
// site list's entries, and a Site Verification list's resources read back as
// the property each verifies.
func cutToSites(sites []string, v any) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	keep := func(list any, siteOf func(map[string]any) (string, bool)) []any {
		entries, _ := list.([]any)
		return slices.DeleteFunc(entries, func(e any) bool {
			m, _ := e.(map[string]any)
			s, ok := siteOf(m)
			return !ok || !slices.Contains(sites, s)
		})
	}
	if list, ok := obj["siteEntry"]; ok {
		obj["siteEntry"] = keep(list, func(m map[string]any) (string, bool) {
			s, ok := m["siteUrl"].(string)
			return s, ok
		})
	}
	if list, ok := obj["items"]; ok {
		obj["items"] = keep(list, bodySite)
	}
	return obj
}

// ownersRequest is search_console_owners_update's request: the property's
// resource, its owners changed as the call asks. Google takes the owner list
// whole, so the list sent is made from the resource as the approval binds it
// ([ownersShape]), read under the lease; until then the body says the change.
// An owner never removes itself here: that is relinquishing,
// webResource.delete.
func (p *Product) ownersRequest(call growth.Call, site string) (googlegrowth.Request, error) {
	add, remove := stringArgs(call.Args["add"]), stringArgs(call.Args["remove"])
	invalid := func(said string) (googlegrowth.Request, error) {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, said+" No vendor was changed.")
	}
	if len(add)+len(remove) == 0 {
		return invalid("name the emails to add or remove as owners.")
	}
	for _, e := range append(slices.Clone(add), remove...) {
		if !strings.Contains(e, "@") || strings.ContainsAny(e, " ,;") {
			return invalid(fmt.Sprintf("%q is not an email.", e))
		}
	}
	kind, identifier, id := resourceOf(site)
	update, _ := p.Catalog().Operation("siteVerification.webResource.update")
	req, err := newRequest(p.sites(call.Handle), update, map[string]string{"id": id}, nil, map[string]any{
		"site": map[string]any{"type": kind, "identifier": identifier}, "add": stringsAny(add),
		"remove": stringsAny(remove)})
	if err != nil {
		return req, err
	}
	req.Shape = ownersShape(req, site, add, remove)
	return req, nil
}

// ownersShape makes the owners update sent from the resource as read: its
// owners with remove taken out and add put in, refused where an owner to
// remove is none or every one to add is one already.
func ownersShape(req googlegrowth.Request, site string, add, remove []string) func([]byte) (googlegrowth.Request,
	error) {
	return func(before []byte) (googlegrowth.Request, error) {
		invalid := func(said string) (googlegrowth.Request, error) {
			return googlegrowth.Request{}, refusal(growth.RefusedInvalid, said+" No vendor was changed.")
		}
		var resource map[string]any
		if err := json.Unmarshal(before, &resource); err != nil || resource["site"] == nil {
			return invalid(site + " holds no verified resource to change the owners of.")
		}
		var owners []string
		list, _ := resource["owners"].([]any)
		for _, o := range list {
			if s, ok := o.(string); ok {
				owners = append(owners, s)
			}
		}
		changed := slices.Clone(owners)
		for _, e := range remove {
			i := slices.IndexFunc(changed, func(o string) bool { return strings.EqualFold(o, e) })
			if i < 0 {
				return invalid(fmt.Sprintf("%s is not an owner of %s.", e, site))
			}
			changed = slices.Delete(changed, i, i+1)
		}
		for _, e := range add {
			if !slices.ContainsFunc(changed, func(o string) bool { return strings.EqualFold(o, e) }) {
				changed = append(changed, e)
			}
		}
		if slices.Equal(changed, owners) {
			return invalid("every email to add is an owner of " + site + " already.")
		}
		req.Body = map[string]any{"site": resource["site"], "owners": stringsAny(changed)}
		return req, nil
	}
}

// stringsAny is a list of strings as JSON holds one.
func stringsAny(s []string) []any {
	out := make([]any, 0, len(s))
	for _, v := range s {
		out = append(out, v)
	}
	return out
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
