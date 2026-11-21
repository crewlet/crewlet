package googleanalytics

import (
	"strings"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// newRequest is op's request, acting in the property or account its path
// names, which the seat's scope is checked against.
func newRequest(op growth.Operation, path, query map[string]string, body map[string]any) (googlegrowth.Request,
	error) {
	req, err := googlegrowth.NewRequest(op, path, query, body)
	if err != nil {
		return req, err
	}
	switch {
	case path["propertiesId"] != "":
		req.Account = "properties/" + path["propertiesId"]
	case path["accountsId"] != "":
		req.Account = "accounts/" + path["accountsId"]
	}
	return req, nil
}

// propertyOf is the property req acts in, "" where none.
func propertyOf(req googlegrowth.Request) string {
	id, ok := strings.CutPrefix(req.Account, "properties/")
	if !ok {
		return ""
	}
	return id
}

// accountIDOf is the account req acts in, "" where none.
func accountIDOf(req googlegrowth.Request) string {
	id, ok := strings.CutPrefix(req.Account, "accounts/")
	if !ok {
		return ""
	}
	return id
}
