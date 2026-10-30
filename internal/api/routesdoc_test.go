package api_test

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE ARCHITECTURE PAGE NAMES EVERY ROUTE A NODE SERVES, AND ONLY THOSE.
//
// docs/concepts/architecture.md's table of the routes `ingress` terminates is
// where an operator learns what a node answers, and what a reverse proxy in
// front of one is allow-listed from. Its REST row is a hand-kept list of the
// API's prefixes, and it fell behind the day `GET /link-hosts` was mounted: a
// proxy built from the page refused a route the node serves, and nothing
// failed. So the table is held to the route table the App keeps with every
// surface mounted ([everySurfaceApp]), as the page's other gated tables are
// held to theirs (the workers' duties in internal/engine, the node estate in
// internal/store): every route the App mounts is named by exactly one row, and
// every route a row names is one the App mounts.
//
// A NAME COVERS WHAT IS BENEATH IT, so the REST row names prefixes (`/worklet`
// is every route under it, `/doclet/pages` every route under that) while the
// webhook row names whole routes. A placeholder segment (`HANDLE`, `{token}`)
// stands for any one segment, and the bare root names itself alone, since
// every path is beneath it.
//
// Mutation: drop `/link-hosts` from the REST row, mount a read under a prefix
// no row names, or name a route nothing mounts, and this names it.
func TestTheArchitecturePageNamesEveryRouteANodeServes(t *testing.T) {
	t.Parallel()
	rows := ingressRoutes(t)
	app := everySurfaceApp(t)
	mounted := slices.Concat(app.Routes(), slices.Collect(maps.Keys(app.Policies())))
	slices.Sort(mounted)
	served := map[string]bool{}
	for _, pattern := range slices.Compact(mounted) {
		path := servedPath(pattern)
		var naming []string
		for _, row := range rows {
			named := false
			for _, name := range row {
				if namesPath(name, path) {
					served[name], named = true, true
				}
			}
			if named {
				naming = append(naming, row[0])
			}
		}
		switch len(naming) {
		case 0:
			t.Errorf("the App mounts %s, and no row of %s's routes table names it",
				pattern, routesPage)
		case 1:
		default:
			t.Errorf("%s is named by %d rows of %s's routes table (the ones opening %s), "+
				"and a route is one row's", pattern, len(naming), routesPage,
				strings.Join(naming, ", "))
		}
	}
	for _, row := range rows {
		for _, name := range row {
			if !served[name] {
				t.Errorf("%s's routes table names %s, which the App does not mount",
					routesPage, name)
			}
		}
	}
}

// routesPage is the page whose routes table is held to the App, and
// routesLead the paragraph the table follows.
const (
	routesPage = "docs/concepts/architecture.md"
	routesLead = "**The routes `ingress` terminates.**"
)

// backticked is one backticked span of a table cell.
var backticked = regexp.MustCompile("`([^`]+)`")

// ingressRoutes reads the routes table: each row as the routes its first cell
// names, in backticks.
//
// A ROW THAT NAMES NO ROUTE FAILS rather than being skipped: skipped, its
// routes would read as named by nobody, and the failure would point at the
// App's routes rather than at the row this could not read.
func ingressRoutes(t *testing.T) [][]string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(sourcetree.Root(t), filepath.FromSlash(routesPage)))
	if err != nil {
		t.Fatalf("read %s: %v", routesPage, err)
	}
	_, after, found := strings.Cut(string(body), "\n"+routesLead+"\n")
	lines := strings.Split(strings.TrimLeft(after, "\n"), "\n")
	if !found || len(lines) < 3 || !strings.HasPrefix(lines[0], "|") ||
		!strings.HasPrefix(lines[1], "|-") {
		t.Fatalf("%s has no table after %q, so this guard would read nothing",
			routesPage, routesLead)
	}
	var rows [][]string
	for _, line := range lines[2:] {
		if !strings.HasPrefix(line, "|") {
			break
		}
		cell, _, _ := strings.Cut(strings.TrimPrefix(line, "|"), "|")
		var names []string
		for _, span := range backticked.FindAllStringSubmatch(cell, -1) {
			if !strings.HasPrefix(span[1], "/") {
				t.Fatalf("%s's routes table names %q, which is not a path: %s",
					routesPage, span[1], line)
			}
			names = append(names, span[1])
		}
		if len(names) == 0 {
			t.Fatalf("a row of %s's routes table names no route in backticks: %s",
				routesPage, line)
		}
		rows = append(rows, names)
	}
	if len(rows) == 0 {
		t.Fatalf("%s's routes table has no rows", routesPage)
	}
	return rows
}

// servedPath is the path a mounted pattern serves: its method dropped, and the
// `{$}` that pins a pattern to its own path read as that path.
func servedPath(pattern string) string {
	if _, path, method := strings.Cut(pattern, " "); method {
		pattern = path
	}
	return strings.TrimSuffix(pattern, "{$}")
}

// wildcardSegment is a path segment that stands for any one segment: a
// wildcard as the mux spells it (`{token}`), or as the page does (`HANDLE`).
var wildcardSegment = regexp.MustCompile(`^(\{[^}]+\}|[A-Z][A-Z0-9_]*)$`)

// namesPath reports whether a route the table names covers a path the App
// serves: the path is the named route or beneath it, with a wildcard segment
// of the name standing for any one segment of the path. The bare root covers
// itself alone, or it would cover every path there is.
func namesPath(name, path string) bool {
	named, served := pathSegments(name), pathSegments(path)
	if len(named) == 0 {
		return len(served) == 0
	}
	if len(served) < len(named) {
		return false
	}
	for i, segment := range named {
		if segment != served[i] && !wildcardSegment.MatchString(segment) {
			return false
		}
	}
	return true
}

// pathSegments is a path's segments, less the trailing slash that makes a
// pattern a subtree.
func pathSegments(path string) []string {
	if path = strings.Trim(path, "/"); path == "" {
		return nil
	}
	return strings.Split(path, "/")
}
