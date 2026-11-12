package api_test

import (
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/issuereport"
)

// apiOwnUnguarded are the routes served without a credential whose handler is in
// the API's own package or the standard library's. Each reads nothing past the
// request, and the outermost wrapping reports a panic on an unguarded path with its
// site alone, so no package tag is needed. A REVIEWED LIST: a new one is somebody's
// decision in a diff.
var apiOwnUnguarded = []string{
	"GET /health", "GET /ready", "GET /{$}", "GET /dashboard", "GET /favicon.ico", "GET /static/",
	"POST /otlp/{token}/v1/{signal}",
	// Dispatchers to Gitlet, which checks the box's token itself.
	host.PatternBoxInfoRefs, host.PatternBoxUploadPack, host.PatternBoxReceivePack,
	host.PatternBoxGitletTree, host.PatternBoxGitletBlob, host.PatternBoxGitletDiff,
}

// handlerPackage is the package whose code a mounted handler is, as a report spells
// a component, "net/http" for the standard library's.
func handlerPackage(h http.Handler) string {
	var name string
	if f, ok := h.(http.HandlerFunc); ok {
		name = runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
	} else {
		ty := reflect.TypeOf(h)
		for ty.Kind() == reflect.Pointer {
			ty = ty.Elem()
		}
		name = ty.PkgPath() + "."
	}
	name = strings.TrimPrefix(name, "github.com/crewlet/crewlet/")
	slash := strings.LastIndex(name, "/")
	dot := strings.Index(name[slash+1:], ".")
	if dot < 0 {
		return name
	}
	return name[:slash+1+dot]
}

// EVERY ROUTE SERVED WITHOUT A CREDENTIAL IS IN A PACKAGE A PANIC REPORT TREATS AS
// EXPOSED, so the report of a defect anybody can reach shows its site and never the
// way in. Walked over the whole table, the sign-in surface mounted, so a route added
// later is held without anybody adding it here.
//
// Mutation: drop internal/api/webhooks from the exposed packages, and every
// webhook route fails here.
func TestEveryUnauthenticatedRouteIsServedByAnExposedPackage(t *testing.T) {
	t.Parallel()
	opts := listenerOptions(t, false, false)
	opts.Auth = &authapi.Service{}
	a := newApp(t, opts)
	seen := map[string]bool{}
	walked := 0
	for _, pattern := range a.Routes() {
		_, path := requestFor(pattern)
		if !auth.Unguarded(path) {
			continue
		}
		walked++
		pkg := handlerPackage(a.HandlerOf(pattern))
		switch {
		case pkg == "internal/api" || pkg == "net/http":
			if !slices.Contains(apiOwnUnguarded, pattern) {
				t.Errorf("%s is served without a credential by %s and is not in apiOwnUnguarded: "+
					"serve it from a package issuereport.BeforeAuthentication names, or add it there "+
					"if it reads nothing past the request", pattern, pkg)
			}
			seen[pattern] = true
		case !issuereport.BeforeAuthentication(issuereport.Component(pkg)):
			t.Errorf("%s is served without a credential by %s, which issuereport.BeforeAuthentication "+
				"does not name: a panic there would publish the frames that reach it", pattern, pkg)
		}
	}
	if walked < len(apiOwnUnguarded)+10 {
		t.Fatalf("the walk met %d unguarded routes, so it certifies nothing", walked)
	}
	for _, pattern := range apiOwnUnguarded {
		if !seen[pattern] {
			t.Errorf("apiOwnUnguarded names %s, which is not an unguarded route of the API's own", pattern)
		}
	}
}
