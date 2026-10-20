package builtin

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY DOMAIN THE ESTATE ROUTER CARRIES A WRITE FOR IS DECLARED TO THE
// SURFACES, and nothing else is.
//
// The person's write surface classes a tool-less writer's refusal and names a
// failed call's object from [nativeDomains] alone, so a domain routed and not
// declared there answers every content refusal its tool-less verbs meet as a
// 500 and names nothing a failed call was about. Read off the router's own
// operation table ([estate.WriteDomains]), so the first write a new domain
// declares makes this red until the domain is declared.
//
// Mutation: drop any row of nativeDomains and this goes red.
func TestEveryRoutedDomainIsDeclaredToTheSurfaces(t *testing.T) {
	t.Parallel()
	routed := estate.WriteDomains()
	if len(routed) == 0 {
		t.Fatal("the router names no write domain, so this gate compares nothing")
	}
	declared := slices.Sorted(maps.Keys(nativeDomains))
	if !slices.Equal(declared, routed) {
		t.Errorf("nativeDomains declares %v and the estate router carries writes "+
			"for %v: declare each routed domain's refusal classes and object "+
			"arguments, and nothing it does not route", declared, routed)
	}
	for name, d := range nativeDomains {
		if d.class == nil || d.sentence == nil || len(d.objects) == 0 {
			t.Errorf("%s is declared with no class function, no sentence or no "+
				"object argument", name)
		}
	}
}

// classRow is what one declared error of a domain is, as a writer's refusal:
// its class, or — `why` set — the reason it is not a refusal at all.
type classRow struct {
	err   error
	class tools.Refusal
	why   string
}

// domainErrors is every exported sentinel and error type each native domain
// declares, with the class its writer's refusal is — keyed by the domain's
// state-log name, then by the Go identifier.
var domainErrors = map[string]map[string]classRow{
	artifactsdomain.Domain{}.Name(): {
		"ErrAboveOwnRole":      {err: artifactsdomain.ErrAboveOwnRole, class: tools.RefusalForbidden},
		"ErrAgentCompanyShare": {err: artifactsdomain.ErrAgentCompanyShare, class: tools.RefusalForbidden},
		"ErrBadCursor":         {err: artifactsdomain.ErrBadCursor, class: tools.RefusalInvalid},
		"ErrConflict":          {err: artifactsdomain.ErrConflict, class: tools.RefusalConflict},
		"ErrInvalid":           {err: artifactsdomain.ErrInvalid, class: tools.RefusalInvalid},
		"ErrLabelTaken":        {err: artifactsdomain.ErrLabelTaken, class: tools.RefusalConflict},
		"ErrNotFound":          {err: artifactsdomain.ErrNotFound, class: tools.RefusalNotFound},
		"ErrNotInTrash": {err: artifactsdomain.ErrNotInTrash,
			why: "the trash reaper's own purge, made as the engine, never a caller's request"},
		"ErrPurged":      {err: artifactsdomain.ErrPurged, class: tools.RefusalNotFound},
		"ErrPurging":     {err: artifactsdomain.ErrPurging, class: tools.RefusalConflict},
		"ErrSourceTaken": {err: artifactsdomain.ErrSourceTaken, class: tools.RefusalExists},
		"ErrTrashed":     {err: artifactsdomain.ErrTrashed, class: tools.RefusalConflict},
		"ErrUploadStale": {err: artifactsdomain.ErrUploadStale, class: tools.RefusalConflict},

		"CarriedObjectGone": {err: &artifactsdomain.CarriedObjectGone{}, class: tools.RefusalConflict},
		"ErrFutureVersion": {err: &artifactsdomain.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"QuotaExceeded": {err: &artifactsdomain.QuotaExceeded{}, class: tools.RefusalQuotaExceeded},
		"VersionMoved":  {err: &artifactsdomain.VersionMoved{}, class: tools.RefusalStaleVersion},
	},
	artifactdata.Domain{}.Name(): {
		"ErrBadCursor":         {err: artifactdata.ErrBadCursor, class: tools.RefusalInvalid},
		"ErrConflict":          {err: artifactdata.ErrConflict, class: tools.RefusalConflict},
		"ErrForbidden":         {err: artifactdata.ErrForbidden, class: tools.RefusalForbidden},
		"ErrGone":              {err: artifactdata.ErrGone, class: tools.RefusalNotFound},
		"ErrInvalid":           {err: artifactdata.ErrInvalid, class: tools.RefusalInvalid},
		"ErrMigrationTooLarge": {err: artifactdata.ErrMigrationTooLarge, class: tools.RefusalInvalid},
		"ErrNotFound":          {err: artifactdata.ErrNotFound, class: tools.RefusalNotFound},
		"ErrNotUndoable":       {err: artifactdata.ErrNotUndoable, class: tools.RefusalConflict},

		"ErrFutureVersion": {err: &artifactdata.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"RateLimited": {err: &artifactdata.RateLimited{}, class: tools.RefusalUnavailable},
	},
	worklet.Domain{}.Name(): {
		"ErrAlreadyAnswered":        {err: worklet.ErrAlreadyAnswered, class: tools.RefusalAlreadyAnswered},
		"ErrBadQuery":               {err: worklet.ErrBadQuery, class: tools.RefusalInvalid},
		"ErrForbidden":              {err: worklet.ErrForbidden, class: tools.RefusalForbidden},
		"ErrInboxFull":              {err: worklet.ErrInboxFull, class: tools.RefusalInboxFull},
		"ErrIndexBuilding":          {err: worklet.ErrIndexBuilding, class: tools.RefusalUnavailable},
		"ErrInvalid":                {err: worklet.ErrInvalid, class: tools.RefusalInvalid},
		"ErrNoChartActivation":      {err: worklet.ErrNoChartActivation, why: "a chart apply's wiring, never a caller's request"},
		"ErrNoComment":              {err: worklet.ErrNoComment, class: tools.RefusalNotFound},
		"ErrNoFile":                 {err: worklet.ErrNoFile, class: tools.RefusalNotFound},
		"ErrNoProject":              {err: worklet.ErrNoProject, class: tools.RefusalNotFound},
		"ErrNoTask":                 {err: worklet.ErrNoTask, class: tools.RefusalNotFound},
		"ErrNoType":                 {err: worklet.ErrNoType, class: tools.RefusalInvalid},
		"ErrNotAuthor":              {err: worklet.ErrNotAuthor, class: tools.RefusalForbidden},
		"ErrNothingToRestore":       {err: worklet.ErrNothingToRestore, class: tools.RefusalConflict},
		"ErrReassignmentBudget":     {err: worklet.ErrReassignmentBudget, class: tools.RefusalReassignmentBudget},
		"ErrReparentAcrossProjects": {err: worklet.ErrReparentAcrossProjects, class: tools.RefusalInvalid},
		"ErrRestoresOthers": {err: &worklet.Refusal{Mark: worklet.ErrForbidden,
			Also: worklet.ErrRestoresOthers, Err: errors.New("x")}, class: tools.RefusalForbidden},
		"ErrStaleVersion":   {err: worklet.ErrStaleVersion, class: tools.RefusalStaleVersion},
		"ErrStepUnresolved": {err: worklet.ErrStepUnresolved, why: "an unknown outcome, answered as one (UnknownCause)"},
		"ErrStepUnvouched": {err: errors.Join(worklet.ErrStepUnresolved, worklet.ErrStepUnvouched),
			why: "an unknown outcome, answered as one (UnknownCause)"},
		"ErrTooBroad":    {err: worklet.ErrTooBroad, class: tools.RefusalInvalid},
		"ErrUploadStale": {err: worklet.ErrUploadStale, class: tools.RefusalUnavailable},

		"AlreadyAnsweredError": {err: &worklet.AlreadyAnsweredError{}, class: tools.RefusalAlreadyAnswered},
		"ErrAmbiguous":         {err: &worklet.ErrAmbiguous{}, class: tools.RefusalInvalid},
		"ErrAmbiguousAnswer":   {err: &worklet.ErrAmbiguousAnswer{}, class: tools.RefusalInvalid},
		"ErrFutureVersion": {err: &worklet.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"MoveStopped": {err: &worklet.MoveStopped{Err: errors.New("x")},
			why: "a gesture that stopped part of the way, answered with what it did"},
		"Refusal": {err: &worklet.Refusal{Mark: worklet.ErrInvalid, Err: errors.New("x")},
			class: tools.RefusalInvalid},
		"SubtreeStopped": {err: &worklet.SubtreeStopped{Err: errors.New("x")},
			why: "a gesture that stopped part of the way, answered with what it did"},
		"TagClash":     {err: &worklet.TagClash{}, class: tools.RefusalInvalid},
		"TagsFull":     {err: &worklet.TagsFull{}, class: tools.RefusalInvalid},
		"TextCapError": {err: &worklet.TextCapError{}, class: tools.RefusalInvalid},
	},
	doclet.Domain{}.Name(): {
		"ErrBadCursor":    {err: doclet.ErrBadCursor, class: tools.RefusalInvalid},
		"ErrConflict":     {err: doclet.ErrConflict, class: tools.RefusalConflict},
		"ErrInvalid":      {err: doclet.ErrInvalid, class: tools.RefusalInvalid},
		"ErrNoActivation": {err: doclet.ErrNoActivation, why: "a configuration apply's wiring, never a caller's request"},
		"ErrNotFound":     {err: doclet.ErrNotFound, class: tools.RefusalNotFound},
		"ErrParent":       {err: doclet.ErrParent, class: tools.RefusalInvalid},
		"ErrReserved":     {err: doclet.ErrReserved, class: tools.RefusalForbidden},
		"ErrNotInTrash": {err: doclet.ErrNotInTrash,
			why: "the trash reaper's own purge, made as the engine, never a caller's request"},
		"ErrStaleVersion": {err: doclet.ErrStaleVersion, class: tools.RefusalStaleVersion},
		"ErrTitleTaken":   {err: doclet.ErrTitleTaken, class: tools.RefusalExists},
		// A LISTING ORDER IS AN ARGUMENT: newest first is served for the
		// tool-skill pages alone, so what fixes the call is the other order
		// or the skills filter, exactly as a cursor from another listing.
		"ErrUnservedOrder": {err: doclet.ErrUnservedOrder, class: tools.RefusalInvalid},

		"ErrFutureVersion": {err: &doclet.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"ErrUnknownVersion": {err: doclet.ErrUnknownVersion{},
			why: "a document version this build cannot read, a fault of the copy"},
		"InvalidError": {err: &doclet.InvalidError{}, class: tools.RefusalInvalid},
	},
	chatlet.Domain{}.Name(): {
		// AN ARCHIVED ROOM IS AN ARGUMENT TO CHANGE: it takes nothing more
		// from anybody, and what fixes the call is a room still open.
		"ErrArchived":   {err: chatlet.ErrArchived, class: tools.RefusalInvalid},
		"ErrConflict":   {err: chatlet.ErrConflict, class: tools.RefusalConflict},
		"ErrForbidden":  {err: chatlet.ErrForbidden, class: tools.RefusalForbidden},
		"ErrInvalid":    {err: chatlet.ErrInvalid, class: tools.RefusalInvalid},
		"ErrNameTaken":  {err: chatlet.ErrNameTaken, class: tools.RefusalExists},
		"ErrNotFound":   {err: chatlet.ErrNotFound, class: tools.RefusalNotFound},
		"ErrRoomExists": {err: chatlet.ErrRoomExists, class: tools.RefusalExists},
		// THIS NODE'S CONDITION, NOT A PERMISSION: the same call lands
		// once the node is upgraded.
		"ErrUnknownKind":  {err: chatlet.ErrUnknownKind, class: tools.RefusalUnavailable},
		"ErrNoActivation": {err: chatlet.ErrNoActivation, why: "a configuration apply's wiring, never a caller's request"},

		"ErrFutureVersion": {err: &chatlet.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"ErrUnknownVersion": {err: chatlet.ErrUnknownVersion{},
			why: "a document version this build cannot read, a fault of the copy"},
		"InvalidError": {err: &chatlet.InvalidError{}, class: tools.RefusalInvalid},
	},
}

// EVERY ERROR A NATIVE DOMAIN DECLARES IS CLASSED, or named as no refusal.
//
// A domain's writer refuses with its own sentinels, and a sentinel no class
// claims is answered as the node's fault — "a fault in this node itself", a
// 500 — wherever it reaches: a tool's failure ([writeFailure] falls back to
// [WriterClass]) and the person's write surface alike. Parsed from each
// domain's own source, as the estate wire's registration gate is, so a
// sentinel added to a domain is red here until somebody says what it is.
//
// Mutation: drop any arm from trackerClass, pageClass or chatClass and its
// rows go red;
// add an exported sentinel to either domain and the gate names it.
func TestEveryDomainRefusalIsClassed(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	packages := domainPackages()
	for _, domain := range slices.Sorted(maps.Keys(nativeDomains)) {
		rows, held := domainErrors[domain]
		if !held {
			t.Errorf("%s is a native domain with no rows here: name each error it "+
				"declares and the class its writer's refusal is", domain)
			continue
		}
		pkg, known := packages[domain]
		if !known {
			t.Errorf("%s is a native domain domainPackages does not name: add its "+
				"Domain, so its package is the one read", domain)
			continue
		}
		declared := exportedErrors(t, filepath.Join(root, "internal", filepath.FromSlash(pkg)))
		if len(declared) < 5 {
			t.Fatalf("%s declares %d errors; this is not reading the package", domain,
				len(declared))
		}
		for _, name := range declared {
			if _, ok := rows[name]; !ok {
				t.Errorf("%s.%s is an error the domain declares and nothing classes: "+
					"give it a class in its domain's function and a row here, or a "+
					"row saying why it is no refusal", domain, name)
			}
		}
		for name, row := range rows {
			if !slices.Contains(declared, name) {
				t.Errorf("%s.%s has a row and the domain declares no such error", domain, name)
				continue
			}
			class, ok := WriterClass(row.err)
			switch {
			case row.why != "" && ok:
				t.Errorf("%s.%s is classed %q, and its row says it is no refusal: %s",
					domain, name, class, row.why)
			case row.why == "" && (!ok || class != row.class):
				t.Errorf("%s.%s is classed %q (%v), want %q", domain, name, class, ok,
					row.class)
			}
		}
	}
	// THE FRAMEWORK'S OWN, which every domain's writer may return as
	// written.
	for err, want := range map[error]tools.Refusal{
		statelog.ErrExists: tools.RefusalExists, statelog.ErrConflict: tools.RefusalConflict,
	} {
		if class, ok := WriterClass(err); !ok || class != want {
			t.Errorf("WriterClass(%v) = %q, %v; want %q", err, class, ok, want)
		}
	}
}

// exportedErrors is every exported Err* package variable and every exported
// type with an `Error() string` method in one package directory, tests
// excluded.
func exportedErrors(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					for _, name := range spec.(*ast.ValueSpec).Names {
						if name.IsExported() && strings.HasPrefix(name.Name, "Err") {
							out = append(out, name.Name)
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || d.Name.Name != "Error" || len(d.Type.Params.List) != 0 {
					continue
				}
				recv := d.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if ident, ok := recv.(*ast.Ident); ok && ident.IsExported() {
					out = append(out, ident.Name)
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// domainPackages is each native domain's package, relative to internal/, read
// off the domain's own type rather than its state-log name: the two differ —
// the artifacts domain is `artifacts`, and internal/artifacts is the
// artifacts' vocabulary, not the package whose writer refuses.
func domainPackages() map[string]string {
	internal := path.Dir(reflect.TypeFor[statelog.Position]().PkgPath())
	out := map[string]string{}
	for _, d := range []statelog.Domain{worklet.Domain{}, doclet.Domain{}, chatlet.Domain{},
		artifactsdomain.Domain{}, artifactdata.Domain{}} {
		out[d.Name()] = strings.TrimPrefix(reflect.TypeOf(d).PkgPath(), internal+"/")
	}
	return out
}
