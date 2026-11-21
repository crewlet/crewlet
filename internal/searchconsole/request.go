package searchconsole

import (
	"fmt"
	"maps"
	"net/url"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// listings are the operations that name no site: what they answer is cut to
// the seat's sites.
var listings = []string{"webmasters.sites.list", "siteVerification.webResource.list"}

// paramValid reports whether a path parameter's value is what its name holds:
// a property, a sitemap's URL, or a Site Verification resource's ID.
func paramValid(name, value string) bool {
	switch name {
	case "siteUrl":
		return config.SearchConsoleSite(value)
	case "feedpath":
		return absoluteURL(value)
	case "id":
		_, ok := resourceSite(value)
		return ok
	}
	return false
}

// newRequest is op's request from a call's path parameters, query and body,
// acting in the site it names, which the seat's scope is then checked
// against: a site in its path or body, the property a Site Verification
// resource verifies, or the narrowest of sites holding the page it names.
// A request that names none is refused, but for a listing.
func newRequest(sites []string, op growth.Operation, path, query map[string]string, body map[string]any) (
	googlegrowth.Request, error) {
	path = maps.Clone(path)
	if id, ok := path["id"]; ok {
		// GOOGLE HANDS AN ID BACK ENCODED; it is escaped once, here.
		if decoded, err := url.PathUnescape(id); err == nil {
			path["id"] = decoded
		}
	}
	req, err := googlegrowth.NewRequestWith(op, path, query, body, paramValid)
	if err != nil {
		return req, err
	}
	site, err := siteOf(sites, req)
	if err != nil {
		return googlegrowth.Request{}, err
	}
	if site == "" && !slices.Contains(listings, op.ID) {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, op.ID+" names no Search Console property: "+
			"give the site it acts in. No vendor was reached.")
	}
	req.Account = site
	return req, nil
}

// siteOf is the site req acts in, "" for none.
func siteOf(sites []string, req googlegrowth.Request) (string, error) {
	invalid := func(format string, args ...any) error {
		return refusal(growth.RefusedInvalid, fmt.Sprintf(format, args...)+" No vendor was reached.")
	}
	switch req.Op.ID {
	case "siteVerification.webResource.getToken", "siteVerification.webResource.insert":
		site, ok := bodySite(req.Body)
		if !ok {
			return "", invalid("%s's body.site is a SITE with a URL or an INET_DOMAIN with a domain, the "+
				"property to verify.", req.Op.ID)
		}
		return site, nil
	case "searchconsole.urlInspection.index.inspect":
		site, _ := req.Body["siteUrl"].(string)
		page, _ := req.Body["inspectionUrl"].(string)
		if !config.SearchConsoleSite(site) || !covers(site, page) {
			return "", invalid("URL inspection names the property as body.siteUrl and a page under it as "+
				"body.inspectionUrl; %q is not a page of %q.", page, site)
		}
		return site, nil
	case "indexing.urlNotifications.publish", "indexing.urlNotifications.getMetadata":
		page, _ := req.Body["url"].(string)
		if req.Op.Method == "GET" {
			page = req.Query.Get("url")
		}
		site, ok := coverOf(sites, page)
		if !ok {
			return "", refusal(growth.RefusedForbidden, fmt.Sprintf("%q is a page of none of this agent's "+
				"sites. No vendor was reached.", page))
		}
		return site, nil
	}
	if id, ok := req.Params["id"]; ok {
		site, _ := resourceSite(id)
		if named, given := req.Body["site"]; given && named != nil {
			if other, ok := bodySite(req.Body); !ok || other != site {
				return "", invalid("body.site names %q, but the resource %s verifies %q.", other, id, site)
			}
		}
		return site, nil
	}
	return req.Params["siteUrl"], nil
}
