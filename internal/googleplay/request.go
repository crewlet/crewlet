package googleplay

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// appParams are the path parameters that name the Play app a call acts on,
// in the order a call's app is read: a third-party store's own app first,
// since its hosted app is not on Play.
var appParams = []string{"appStorePackageName", "packageName", "applicationsId", "appsId", "name"}

// editSentinel stands in for an edit's ID in a staged request until the
// engine opens the edit, so a request's digest is the same in every edit.
const editSentinel = "crewlet-edit"

var (
	// tokenValue is a purchase token: longer than an ID, the same alphabet.
	tokenValue = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_.:~-]+$`) })
	// emailValue is a user's email, as users and grants name a user.
	emailValue = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,190}$`)
	})
	// objectValue is a reports bucket object's name: its folders and file.
	objectValue = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_.\-/]+$`) })
)

// validParam reports whether value may fill the path parameter name.
func validParam(name, value string) bool {
	switch {
	case slices.Contains(appParams, name) || name == "playAppPackageName" || name == "grantsId":
		return config.GooglePlayPackage(value)
	case name == "editId":
		return value == editSentinel
	case name == "token" || name == "purchaseToken":
		return len(value) <= 2000 && tokenValue().MatchString(value)
	case name == "usersId":
		return emailValue().MatchString(value)
	case name == "object":
		// Cloud Storage holds an object's name to 1,024 bytes.
		return len(value) <= 1024 && objectValue().MatchString(value) &&
			!slices.Contains(strings.Split(value, "/"), "..")
	}
	return googlegrowth.IDParam(name, value)
}

// newRequest is op's request, its Account the app, developer account or
// bucket it acts in, which the seat's scope is checked against. A staged
// request holds the edit sentinel until the engine opens its edit.
func newRequest(op growth.Operation, path, query map[string]string, body map[string]any) (googlegrowth.Request,
	error) {
	if _, named := path["editId"]; named {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "the engine opens, fills and commits or "+
			"discards each edit itself: leave editId out. No vendor was reached.")
	}
	if strings.Contains(op.Path, "{editId}") {
		path = withParam(path, "editId", editSentinel)
	}
	req, err := googlegrowth.NewRequestWith(op, path, query, body, validParam)
	if err != nil {
		return req, err
	}
	switch {
	case path["developersId"] != "":
		req.Account = "developers/" + path["developersId"]
	case path["bucket"] != "":
		req.Account = "b/" + path["bucket"]
	default:
		req.Account = appOf(req)
	}
	return req, nil
}

// withParam is path with name set, a copy.
func withParam(path map[string]string, name, value string) map[string]string {
	out := make(map[string]string, len(path)+1)
	for k, v := range path {
		out[k] = v
	}
	out[name] = value
	return out
}

// appOf is the package name req acts on, "" where it names none.
func appOf(req googlegrowth.Request) string {
	for _, name := range appParams {
		if v := req.Params[name]; v != "" {
			return v
		}
	}
	return ""
}

// inEdit is a staged request's address in edit id.
func inEdit(req googlegrowth.Request, id string) googlegrowth.Request {
	req.Address = strings.Replace(req.Address, "/edits/"+editSentinel, "/edits/"+id, 1)
	return req
}

// staged reports whether op is staged into, or read from, an open edit:
// every edit method but the session's own (open, read, validate, commit,
// discard).
func staged(op growth.Operation) bool { return strings.Contains(op.Path, "/edits/{editId}/") }
