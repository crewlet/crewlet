package googleanalytics

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// bodyScope is where an operation's body names what it acts in beyond its
// path, each a resource name the seat's scope holds: Target is the account or
// property it is made in where its path names none, and Also the properties
// it reaches besides (a roll-up's sources, an audience of a property).
type bodyScope struct {
	Target string
	Also   []string
}

// bodyScopes are the operations whose body names what they act in, by ID,
// with each such field in lowerCamel (TestEveryWriteNamesWhatItActsIn).
func bodyScopes() map[string]bodyScope {
	return map[string]bodyScope{
		"analyticsadmin.properties.create":                           {Target: "parent"},
		"analyticsadmin.properties.createRollupProperty":             {Target: "rollupProperty.parent", Also: []string{"sourceProperties"}},
		"analyticsadmin.properties.provisionSubproperty":             {Target: "subproperty.parent"},
		"analyticsadmin.properties.rollupPropertySourceLinks.create": {Also: []string{"sourceProperty"}},
		"analyticsadmin.properties.subpropertyEventFilters.create":   {Also: []string{"applyToProperty"}},
		"analyticsadmin.properties.subpropertyEventFilters.patch":    {Also: []string{"applyToProperty"}},
		"analyticsdata.properties.audienceExports.create":            {Also: []string{"audience"}},
		"analyticsdata.properties.audienceLists.create":              {Also: []string{"audience"}},
		"analyticsdata.properties.recurringAudienceLists.create":     {Also: []string{"audience"}},
	}
}

// actsInNone is the one write that acts in no existing account or property:
// it opens a new account, through a ticket a person accepts in a browser.
const actsInNone = "analyticsadmin.accounts.provisionAccountTicket"

// placeName is a property's or an account's resource name.
var placeName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(properties|accounts)/[0-9]{1,20}$`)
})

// underPlace is a resource name within a property or an account
// (`properties/1/audiences/2`), its first two segments the place.
var underPlace = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^((properties|accounts)/[0-9]{1,20})(/[A-Za-z0-9_.~-]+/[A-Za-z0-9_.~:-]+)*$`)
})

// newRequest is op's request, acting in the property or account its path
// template names, or for an operation on a collection the one its body names
// ([bodyScopes]): the seat's scope is checked against it.
func newRequest(op growth.Operation, path, query map[string]string, body map[string]any) (googlegrowth.Request,
	error) {
	req, err := googlegrowth.NewRequest(op, path, query, body)
	if err != nil {
		return req, err
	}
	switch {
	case strings.Contains(op.Path, "{propertiesId}"):
		req.Account = "properties/" + path["propertiesId"]
	case strings.Contains(op.Path, "{accountsId}"):
		req.Account = "accounts/" + path["accountsId"]
	case bodyScopes()[op.ID].Target != "":
		field := bodyScopes()[op.ID].Target
		name, _ := req.Field(field).(string)
		if !placeName().MatchString(name) {
			return req, refusal(growth.RefusedInvalid, fmt.Sprintf("%s is made in the account or property "+
				"body.%s names, as accounts/<id> or properties/<id>, once. No vendor was reached.", op.ID, field))
		}
		req.Account = name
	}
	return req, nil
}

// placesOf is every property and account req acts in: its own, and those its
// body names besides; a body naming one in another form is refused.
func placesOf(req googlegrowth.Request) ([]string, error) {
	var out []string
	if req.Account != "" {
		out = append(out, req.Account)
	}
	for _, field := range bodyScopes()[req.Op.ID].Also {
		v := req.Field(field)
		var names []any
		switch t := v.(type) {
		case nil:
		case string:
			names = []any{t}
		case []any:
			names = t
		default:
			names = []any{v}
		}
		for _, n := range names {
			s, _ := n.(string)
			m := underPlace().FindStringSubmatch(s)
			if m == nil {
				return nil, refusal(growth.RefusedInvalid, fmt.Sprintf("body.%s names resources as "+
					"properties/<id> or a resource name within one, each once. No vendor was reached.", field))
			}
			out = append(out, m[1])
		}
	}
	return out, nil
}

// batchRead is how a batch that names its targets in its body reads them
// back: the batchGet of its parent, and where in each of body.requests the
// target's name sits.
type batchRead struct{ get, name string }

// batchReads are the writes changing access bindings their body names, which
// a person approves on those bindings as they read now (who holds them, at
// which roles): no one resource holds them. A batchCreate makes new bindings
// and a reorder names every rule in its new order, so each binds its request.
func batchReads() map[string]batchRead {
	return map[string]batchRead{
		"analyticsadmin.accounts.accessBindings.batchUpdate":   {"analyticsadmin.accounts.accessBindings.batchGet", "accessBinding.name"},
		"analyticsadmin.accounts.accessBindings.batchDelete":   {"analyticsadmin.accounts.accessBindings.batchGet", "name"},
		"analyticsadmin.properties.accessBindings.batchUpdate": {"analyticsadmin.properties.accessBindings.batchGet", "accessBinding.name"},
		"analyticsadmin.properties.accessBindings.batchDelete": {"analyticsadmin.properties.accessBindings.batchGet", "name"},
	}
}

// beforeRead reads a batch's targets back through its parent's batchGet, each
// held to the call's own parent, so no binding outside it is named.
func (p *Product) beforeRead(ctx context.Context, handle string, req googlegrowth.Request) ([]byte, bool, error) {
	br, ok := batchReads()[req.Op.ID]
	if !ok {
		return nil, false, nil
	}
	requests, _ := req.Field("requests").([]any)
	names := make([]string, 0, len(requests))
	for _, r := range requests {
		m, _ := r.(map[string]any)
		name, _ := googlegrowth.Lookup(m, br.name).(string)
		id, under := strings.CutPrefix(name, req.Account+"/accessBindings/")
		if !under || !googlegrowth.IDParam("", id) {
			return nil, false, refusal(growth.RefusedInvalid, fmt.Sprintf("each of "+
				"body.requests names a binding of %s as %s/accessBindings/<id> in its %s. No vendor was reached.",
				req.Account, req.Account, br.name))
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, false, refusal(growth.RefusedInvalid, req.Op.ID+" names no binding in "+
			"body.requests. No vendor was reached.")
	}
	op, _ := p.Catalog().Operation(br.get)
	get, err := newRequest(op, req.Params, nil, nil)
	if err != nil {
		return nil, false, err
	}
	get.Query["names"] = names
	raw, err := p.client.Send(ctx, handle, get)
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}
