package googleanalytics

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// bodyScope is where an operation's body names what it acts in beyond its
// path, each a property or account resource name the seat's scope holds:
// Target is the account or property it is made in where its path names none,
// and Also the properties it reaches besides (a roll-up's sources).
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
	}
}

// actsInNone is the one write that acts in no existing account or property:
// it opens a new account, through a ticket a person accepts in a browser.
const actsInNone = "analyticsadmin.accounts.provisionAccountTicket"

// placeName is a property's or an account's resource name.
var placeName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(properties|accounts)/[0-9]{1,20}$`)
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
			if !placeName().MatchString(s) {
				return nil, refusal(growth.RefusedInvalid, fmt.Sprintf("body.%s names properties as "+
					"properties/<id>, each once. No vendor was reached.", field))
			}
			out = append(out, s)
		}
	}
	return out, nil
}
