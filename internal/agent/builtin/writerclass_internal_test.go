package builtin

import (
	"context"
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

// meaning is what a refusal a domain decided MEANS to its caller, as a row
// declares it: the one rule every domain's refusals are classed by
// (internal/mcp's [tools.Refusal]), held here as values so that a row states
// what its error IS and the class follows from [meaning.class] alone. A row
// stating a class instead let one domain class the state of the object a call
// is about as `invalid` while another classed the same kind of refusal
// `conflict`, and both rows passed — the table agreed with whatever its domain
// decided.
type meaning int

const (
	// malformed is the REQUEST: an argument outside its grammar, a value
	// past a limit of its own, or an argument naming another object that
	// cannot take part. The same request never lands; another one does.
	malformed meaning = iota + 1
	// absent is the object the call is about not being there, or gone for
	// good.
	absent
	// notPermitted is a gesture that is not this caller's to make.
	notPermitted
	// staleRead is an object that moved since the caller read it.
	staleRead
	// lostRace is a write that lost its compare-and-set rounds.
	lostRace
	// objectState is the object the call is about refusing the gesture by
	// what it is now — archived, in the trash, being purged, frozen, its own
	// contents at their cap, nothing to restore — until somebody changes
	// it, after which the same request lands.
	objectState
	// taken is a create, or a name, another object already holds.
	taken
	// answered is a question that already has its answer.
	answered
	// inboxCeiling, handOffs and companyQuota are the limits the
	// vocabulary names a class of its own for, because a surface names the
	// remedy and it is not a change of the object.
	inboxCeiling
	handOffs
	companyQuota
	// nodeCondition is this node, which waiting clears.
	nodeCondition
)

// class is the one rule: the class a refusal of this meaning is, on every
// domain and every surface. A race and a state are one class for one reason —
// the request is not wrong, the object is what refuses it, and reading it again
// is the move — while a malformed request is another, because nothing done to
// the object makes it land.
func (m meaning) class() tools.Refusal {
	switch m {
	case malformed:
		return tools.RefusalInvalid
	case absent:
		return tools.RefusalNotFound
	case notPermitted:
		return tools.RefusalForbidden
	case staleRead:
		return tools.RefusalStaleVersion
	case lostRace, objectState:
		return tools.RefusalConflict
	case taken:
		return tools.RefusalExists
	case answered:
		return tools.RefusalAlreadyAnswered
	case inboxCeiling:
		return tools.RefusalInboxFull
	case handOffs:
		return tools.RefusalReassignmentBudget
	case companyQuota:
		return tools.RefusalQuotaExceeded
	case nodeCondition:
		return tools.RefusalUnavailable
	}
	return ""
}

// classRow is what one declared error of a domain is, as a writer's refusal:
// what it means to its caller, or — `why` set — the reason it is not a refusal
// at all.
type classRow struct {
	err   error
	means meaning
	why   string
}

// domainErrors is every exported sentinel and error type each native domain
// declares, with what its writer's refusal MEANS — keyed by the domain's
// state-log name, then by the Go identifier.
var domainErrors = map[string]map[string]classRow{
	artifactsdomain.Domain{}.Name(): {
		"ErrAboveOwnRole":      {err: artifactsdomain.ErrAboveOwnRole, means: notPermitted},
		"ErrAgentCompanyShare": {err: artifactsdomain.ErrAgentCompanyShare, means: notPermitted},
		"ErrBadCursor":         {err: artifactsdomain.ErrBadCursor, means: malformed},
		"ErrConflict":          {err: artifactsdomain.ErrConflict, means: lostRace},
		"ErrInvalid":           {err: artifactsdomain.ErrInvalid, means: malformed},
		"ErrLabelTaken":        {err: artifactsdomain.ErrLabelTaken, means: objectState},
		"ErrNotFound":          {err: artifactsdomain.ErrNotFound, means: absent},
		"ErrNotInTrash": {err: artifactsdomain.ErrNotInTrash,
			why: "the trash reaper's own purge, made as the engine, never a caller's request"},
		"ErrPurged":      {err: artifactsdomain.ErrPurged, means: absent},
		"ErrPurging":     {err: artifactsdomain.ErrPurging, means: objectState},
		"ErrSourceTaken": {err: artifactsdomain.ErrSourceTaken, means: taken},
		"ErrTrashed":     {err: artifactsdomain.ErrTrashed, means: objectState},
		"ErrUploadStale": {err: artifactsdomain.ErrUploadStale, means: objectState},

		"CarriedObjectGone": {err: &artifactsdomain.CarriedObjectGone{}, means: objectState},
		"ErrFutureVersion": {err: &artifactsdomain.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"QuotaExceeded": {err: &artifactsdomain.QuotaExceeded{}, means: companyQuota},
		"VersionMoved":  {err: &artifactsdomain.VersionMoved{}, means: staleRead},
	},
	artifactdata.Domain{}.Name(): {
		"ErrBadCursor":         {err: artifactdata.ErrBadCursor, means: malformed},
		"ErrConflict":          {err: artifactdata.ErrConflict, means: lostRace},
		"ErrForbidden":         {err: artifactdata.ErrForbidden, means: notPermitted},
		"ErrGone":              {err: artifactdata.ErrGone, means: absent},
		"ErrInvalid":           {err: artifactdata.ErrInvalid, means: malformed},
		"ErrMigrationTooLarge": {err: artifactdata.ErrMigrationTooLarge, means: malformed},
		"ErrNotFound":          {err: artifactdata.ErrNotFound, means: absent},
		"ErrNotUndoable":       {err: artifactdata.ErrNotUndoable, means: objectState},

		"ErrFutureVersion": {err: &artifactdata.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"RateLimited": {err: &artifactdata.RateLimited{}, means: nodeCondition},
	},
	worklet.Domain{}.Name(): {
		"ErrAlreadyAnswered":        {err: worklet.ErrAlreadyAnswered, means: answered},
		"ErrBadQuery":               {err: worklet.ErrBadQuery, means: malformed},
		"ErrForbidden":              {err: worklet.ErrForbidden, means: notPermitted},
		"ErrInboxFull":              {err: worklet.ErrInboxFull, means: inboxCeiling},
		"ErrIndexBuilding":          {err: worklet.ErrIndexBuilding, means: nodeCondition},
		"ErrInvalid":                {err: worklet.ErrInvalid, means: malformed},
		"ErrNoChartActivation":      {err: worklet.ErrNoChartActivation, why: "a chart apply's wiring, never a caller's request"},
		"ErrNoComment":              {err: worklet.ErrNoComment, means: absent},
		"ErrNoFile":                 {err: worklet.ErrNoFile, means: absent},
		"ErrNoProject":              {err: worklet.ErrNoProject, means: absent},
		"ErrNoTask":                 {err: worklet.ErrNoTask, means: absent},
		"ErrNoType":                 {err: worklet.ErrNoType, means: malformed},
		"ErrNotAuthor":              {err: worklet.ErrNotAuthor, means: notPermitted},
		"ErrNothingToRestore":       {err: worklet.ErrNothingToRestore, means: objectState},
		"ErrReassignmentBudget":     {err: worklet.ErrReassignmentBudget, means: handOffs},
		"ErrReparentAcrossProjects": {err: worklet.ErrReparentAcrossProjects, means: malformed},
		"ErrRestoresOthers": {err: &worklet.Refusal{Mark: worklet.ErrForbidden,
			Also: worklet.ErrRestoresOthers, Err: errors.New("x")}, means: notPermitted},
		"ErrStaleVersion": {err: worklet.ErrStaleVersion, means: staleRead},
		// THE ITEM OR PROJECT THE WRITE IS ABOUT, refusing by what it is:
		// archived, frozen in the trash, its tags or pins at their cap.
		"ErrState": {err: &worklet.Refusal{Mark: worklet.ErrState, Err: errors.New("x")},
			means: objectState},
		"ErrStepUnresolved": {err: worklet.ErrStepUnresolved, why: "an unknown outcome, answered as one (UnknownCause)"},
		"ErrStepUnvouched": {err: errors.Join(worklet.ErrStepUnresolved, worklet.ErrStepUnvouched),
			why: "an unknown outcome, answered as one (UnknownCause)"},
		"ErrTooBroad": {err: worklet.ErrTooBroad, means: malformed},
		// AN UPLOAD TOO OLD TO NAME is the upload's state, as the
		// artifacts' is: no wait changes it, and uploading again does.
		"ErrUploadStale": {err: worklet.ErrUploadStale, means: objectState},

		"AlreadyAnsweredError": {err: &worklet.AlreadyAnsweredError{}, means: answered},
		"ErrAmbiguous":         {err: &worklet.ErrAmbiguous{}, means: malformed},
		"ErrAmbiguousAnswer":   {err: &worklet.ErrAmbiguousAnswer{}, means: malformed},
		"ErrFutureVersion": {err: &worklet.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"MoveStopped": {err: &worklet.MoveStopped{Err: errors.New("x")},
			why: "a gesture that stopped part of the way, answered with what it did"},
		"Refusal": {err: &worklet.Refusal{Mark: worklet.ErrInvalid, Err: errors.New("x")},
			means: malformed},
		"SubtreeStopped": {err: &worklet.SubtreeStopped{Err: errors.New("x")},
			why: "a gesture that stopped part of the way, answered with what it did"},
		"TagClash":     {err: &worklet.TagClash{}, means: malformed},
		"TagsFull":     {err: &worklet.TagsFull{}, means: objectState},
		"TextCapError": {err: &worklet.TextCapError{}, means: malformed},
	},
	doclet.Domain{}.Name(): {
		"ErrBadCursor":    {err: doclet.ErrBadCursor, means: malformed},
		"ErrConflict":     {err: doclet.ErrConflict, means: lostRace},
		"ErrInvalid":      {err: doclet.ErrInvalid, means: malformed},
		"ErrNoActivation": {err: doclet.ErrNoActivation, why: "a configuration apply's wiring, never a caller's request"},
		"ErrNotFound":     {err: doclet.ErrNotFound, means: absent},
		"ErrParent":       {err: doclet.ErrParent, means: malformed},
		"ErrReserved":     {err: doclet.ErrReserved, means: notPermitted},
		"ErrNotInTrash": {err: doclet.ErrNotInTrash,
			why: "the trash reaper's own purge, made as the engine, never a caller's request"},
		"ErrStaleVersion": {err: doclet.ErrStaleVersion, means: staleRead},
		"ErrTitleTaken":   {err: doclet.ErrTitleTaken, means: taken},
		// A LISTING ORDER IS AN ARGUMENT: newest first is served for the
		// tool-skill pages alone, so what fixes the call is the other order
		// or the skills filter, exactly as a cursor from another listing.
		"ErrUnservedOrder": {err: doclet.ErrUnservedOrder, means: malformed},

		"ErrFutureVersion": {err: &doclet.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"ErrUnknownVersion": {err: doclet.ErrUnknownVersion{},
			why: "a document version this build cannot read, a fault of the copy"},
		"InvalidError": {err: &doclet.InvalidError{}, means: malformed},
	},
	chatlet.Domain{}.Name(): {
		// AN ARCHIVED ROOM IS THE ROOM'S STATE, and a message carrying all
		// the emoji it may is the message's: each refuses the same request
		// until somebody reopens the room or takes an emoji off, as an
		// artifact in the trash does.
		"ErrArchived":      {err: chatlet.ErrArchived, means: objectState},
		"ErrReactionsFull": {err: chatlet.ErrReactionsFull, means: objectState},
		"ErrConflict":      {err: chatlet.ErrConflict, means: lostRace},
		"ErrForbidden":     {err: chatlet.ErrForbidden, means: notPermitted},
		"ErrInvalid":       {err: chatlet.ErrInvalid, means: malformed},
		"ErrNameTaken":     {err: chatlet.ErrNameTaken, means: taken},
		"ErrNotFound":      {err: chatlet.ErrNotFound, means: absent},
		"ErrRoomExists":    {err: chatlet.ErrRoomExists, means: taken},
		// THIS NODE'S CONDITION, NOT A PERMISSION: the same call lands
		// once the node is upgraded.
		"ErrUnknownKind":  {err: chatlet.ErrUnknownKind, means: nodeCondition},
		"ErrNoActivation": {err: chatlet.ErrNoActivation, why: "a configuration apply's wiring, never a caller's request"},

		"ErrFutureVersion": {err: &chatlet.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"ErrUnknownVersion": {err: chatlet.ErrUnknownVersion{},
			why: "a document version this build cannot read, a fault of the copy"},
		"InvalidError": {err: &chatlet.InvalidError{}, means: malformed},
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
// AND EVERY ROW SAYS WHAT ITS ERROR MEANS, never which class it is: the class
// follows from the meaning by the one rule ([meaning.class]), on the domains'
// table and on every domain's own tools ([toolFailures]) alike — so a room
// archived before a post, a project archived before a task is filed into it
// and an artifact in the trash cannot be three classes, which they were.
//
// Mutation: drop any arm from trackerClass, pageClass or chatClass and its
// rows go red; class chatlet.ErrArchived `invalid` again, in chatClass or in a
// row, and it goes red; class worklet.TagsFull `invalid` in writeFailure alone
// and the tools' half goes red; add an exported sentinel to any domain and the
// gate names it.
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
			want := row.means.class()
			switch {
			case row.why != "" && ok:
				t.Errorf("%s.%s is classed %q, and its row says it is no refusal: %s",
					domain, name, class, row.why)
			case row.why != "":
				continue
			case want == "":
				t.Errorf("%s.%s states no meaning: say what the refusal means to "+
					"its caller, and the class follows", domain, name)
				continue
			case !ok || class != want:
				t.Errorf("%s.%s is classed %q (%v) by the domains' table, and what it "+
					"means is %q", domain, name, string(class), ok, string(want))
			}
			// AND BY THE DOMAIN'S OWN TOOLS, which word each refusal and
			// state its class again where they do: the same error is one
			// class whichever surface it reaches, so a seat, an assistant
			// and a person's write surface are told the same thing.
			if got := tools.RefusalOf(toolFailures[domain](row.err)); got != want {
				t.Errorf("%s.%s is classed %q by the domain's tools, and what it "+
					"means is %q", domain, name, string(got), string(want))
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

// toolFailures is the failure each native domain's tools answer a writer's
// refusal with — the function its write tools fall back to — keyed by the
// domain's state-log name.
var toolFailures = map[string]func(error) tools.Result{
	worklet.Domain{}.Name(): func(err error) tools.Result {
		return writeFailure(context.Background(), Actor{}, worklet.UpdateWorkItemTool, err)
	},
	doclet.Domain{}.Name(): func(err error) tools.Result {
		return pageWriteFailure(context.Background(), "save_page", err, "")
	},
	chatlet.Domain{}.Name(): func(err error) tools.Result {
		return chatWriteFailure(context.Background(), PostMessageTool, err, "")
	},
	artifactsdomain.Domain{}.Name(): func(err error) tools.Result {
		return publishFailure(context.Background(), "publish_artifact", artifactCaller{}, err)
	},
	artifactdata.Domain{}.Name(): func(err error) tools.Result {
		return dataFailure(context.Background(), "write_artifact_data", err)
	},
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
