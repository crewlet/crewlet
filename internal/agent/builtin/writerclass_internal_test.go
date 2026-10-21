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
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
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
		"ErrExists":            {err: artifactsdomain.ErrExists, means: taken},
		"ErrInvalid":           {err: artifactsdomain.ErrInvalid, means: malformed},
		// A LABEL ANOTHER VERSION HOLDS is a collision, and every kept
		// label in use is the artifact's own contents at their cap.
		"ErrLabelTaken": {err: artifactsdomain.ErrLabelTaken, means: taken},
		"ErrLabelsFull": {err: artifactsdomain.ErrLabelsFull, means: objectState},
		"ErrNotFound":   {err: artifactsdomain.ErrNotFound, means: absent},
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
	tracker.Domain{}.Name(): {
		"ErrAlreadyAnswered":        {err: tracker.ErrAlreadyAnswered, means: answered},
		"ErrBadQuery":               {err: tracker.ErrBadQuery, means: malformed},
		"ErrForbidden":              {err: tracker.ErrForbidden, means: notPermitted},
		"ErrInboxFull":              {err: tracker.ErrInboxFull, means: inboxCeiling},
		"ErrIndexBuilding":          {err: tracker.ErrIndexBuilding, means: nodeCondition},
		"ErrInvalid":                {err: tracker.ErrInvalid, means: malformed},
		"ErrNoChartActivation":      {err: tracker.ErrNoChartActivation, why: "a chart apply's wiring, never a caller's request"},
		"ErrNoComment":              {err: tracker.ErrNoComment, means: absent},
		"ErrNoFile":                 {err: tracker.ErrNoFile, means: absent},
		"ErrNoProject":              {err: tracker.ErrNoProject, means: absent},
		"ErrNoTask":                 {err: tracker.ErrNoTask, means: absent},
		"ErrNoType":                 {err: tracker.ErrNoType, means: malformed},
		"ErrNotAuthor":              {err: tracker.ErrNotAuthor, means: notPermitted},
		"ErrNothingToRestore":       {err: tracker.ErrNothingToRestore, means: objectState},
		"ErrReassignmentBudget":     {err: tracker.ErrReassignmentBudget, means: handOffs},
		"ErrReparentAcrossProjects": {err: tracker.ErrReparentAcrossProjects, means: malformed},
		"ErrRestoresOthers": {err: &tracker.Refusal{Mark: tracker.ErrForbidden,
			Also: tracker.ErrRestoresOthers, Err: errors.New("x")}, means: notPermitted},
		"ErrStaleVersion": {err: tracker.ErrStaleVersion, means: staleRead},
		"ErrTaken": {err: &tracker.Refusal{Mark: tracker.ErrTaken, Err: errors.New("x")},
			means: taken},
		// THE ITEM OR PROJECT THE WRITE IS ABOUT, refusing by what it is:
		// archived, frozen in the trash, its tags or pins at their cap.
		"ErrState": {err: &tracker.Refusal{Mark: tracker.ErrState, Err: errors.New("x")},
			means: objectState},
		"ErrStepUnresolved": {err: tracker.ErrStepUnresolved, why: "an unknown outcome, answered as one (UnknownCause)"},
		"ErrStepUnvouched": {err: errors.Join(tracker.ErrStepUnresolved, tracker.ErrStepUnvouched),
			why: "an unknown outcome, answered as one (UnknownCause)"},
		"ErrTooBroad": {err: tracker.ErrTooBroad, means: malformed},
		// AN UPLOAD TOO OLD TO NAME is the upload's state, as the
		// artifacts' is: no wait changes it, and uploading again does.
		"ErrUploadStale": {err: tracker.ErrUploadStale, means: objectState},

		"AlreadyAnsweredError": {err: &tracker.AlreadyAnsweredError{}, means: answered},
		"ErrAmbiguous":         {err: &tracker.ErrAmbiguous{}, means: malformed},
		"ErrAmbiguousAnswer":   {err: &tracker.ErrAmbiguousAnswer{}, means: malformed},
		"ErrFutureVersion": {err: &tracker.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"MoveStopped": {err: &tracker.MoveStopped{Err: errors.New("x")},
			why: "a gesture that stopped part of the way, answered with what it did"},
		"Refusal": {err: &tracker.Refusal{Mark: tracker.ErrInvalid, Err: errors.New("x")},
			means: malformed},
		"SubtreeStopped": {err: &tracker.SubtreeStopped{Err: errors.New("x")},
			why: "a gesture that stopped part of the way, answered with what it did"},
		// A LABEL ANOTHER TAG CARRIES, on an add or a rename, and an id the
		// task already holds: collisions, as a page title another page
		// holds is.
		"TagClash":     {err: &tracker.TagClash{}, means: taken},
		"TagsFull":     {err: &tracker.TagsFull{}, means: objectState},
		"TextCapError": {err: &tracker.TextCapError{}, means: malformed},
	},
	pages.Domain{}.Name(): {
		"ErrBadCursor":    {err: pages.ErrBadCursor, means: malformed},
		"ErrConflict":     {err: pages.ErrConflict, means: lostRace},
		"ErrInvalid":      {err: pages.ErrInvalid, means: malformed},
		"ErrNoActivation": {err: pages.ErrNoActivation, why: "a configuration apply's wiring, never a caller's request"},
		"ErrNotFound":     {err: pages.ErrNotFound, means: absent},
		"ErrParent":       {err: pages.ErrParent, means: malformed},
		"ErrReserved":     {err: pages.ErrReserved, means: notPermitted},
		"ErrNotInTrash": {err: pages.ErrNotInTrash,
			why: "the trash reaper's own purge, made as the engine, never a caller's request"},
		"ErrStaleVersion": {err: pages.ErrStaleVersion, means: staleRead},
		"ErrTitleTaken":   {err: pages.ErrTitleTaken, means: taken},
		// A LISTING ORDER IS AN ARGUMENT: newest first is served for the
		// tool-skill pages alone, so what fixes the call is the other order
		// or the skills filter, exactly as a cursor from another listing.
		"ErrUnservedOrder": {err: pages.ErrUnservedOrder, means: malformed},

		"ErrFutureVersion": {err: &pages.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"ErrUnknownVersion": {err: pages.ErrUnknownVersion{},
			why: "a document version this build cannot read, a fault of the copy"},
		"InvalidError": {err: &pages.InvalidError{}, means: malformed},
	},
	chat.Domain{}.Name(): {
		// AN ARCHIVED ROOM IS THE ROOM'S STATE, and a message carrying all
		// the emoji it may is the message's: each refuses the same request
		// until somebody reopens the room or takes an emoji off, as an
		// artifact in the trash does.
		"ErrArchived":      {err: chat.ErrArchived, means: objectState},
		"ErrReactionsFull": {err: chat.ErrReactionsFull, means: objectState},
		"ErrConflict":      {err: chat.ErrConflict, means: lostRace},
		"ErrForbidden":     {err: chat.ErrForbidden, means: notPermitted},
		"ErrInvalid":       {err: chat.ErrInvalid, means: malformed},
		"ErrNameTaken":     {err: chat.ErrNameTaken, means: taken},
		"ErrNotFound":      {err: chat.ErrNotFound, means: absent},
		"ErrRoomExists":    {err: chat.ErrRoomExists, means: taken},
		// THIS NODE'S CONDITION, NOT A PERMISSION: the same call lands
		// once the node is upgraded.
		"ErrUnknownKind":  {err: chat.ErrUnknownKind, means: nodeCondition},
		"ErrNoActivation": {err: chat.ErrNoActivation, why: "a configuration apply's wiring, never a caller's request"},

		"ErrFutureVersion": {err: &chat.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"ErrUnknownVersion": {err: chat.ErrUnknownVersion{},
			why: "a document version this build cannot read, a fault of the copy"},
		"InvalidError": {err: &chat.InvalidError{}, means: malformed},
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
// AND A ROW IS HELD TO ITS ERROR'S NAME where the name says what it is
// ([namedFor]): a collision is a …Taken, a …Clash or an …Exists and nothing
// else is, so a collision cannot be stated as a malformed request, which
// tracker.TagClash was, nor a full set be one sentinel with a collision, which
// artifactsdomain.ErrLabelTaken was.
//
// Mutation: drop any arm from trackerClass, pageClass or chatClass and its
// rows go red; class chat.ErrArchived `invalid` again, in chatClass or in a
// row, and it goes red; class tracker.TagsFull `invalid` in writeFailure alone
// and the tools' half goes red; state TagClash `malformed` in its row, or class
// it `invalid` in writeFailure, and it goes red; add an exported sentinel to
// any domain and the gate names it.
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
			if why := namedFor(name, row.means); why != "" {
				t.Errorf("%s.%s: %s", domain, name, why)
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
			for tool, failure := range toolFailures[domain] {
				if got := tools.RefusalOf(failure(row.err)); got != want {
					t.Errorf("%s.%s is classed %q by %s, and what it means is %q",
						domain, name, string(got), tool, string(want))
				}
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

// namedFor is why an error's NAME and the meaning its row states disagree, or
// "" where they agree.
//
// THE NAME IS THE ONE STATEMENT OF MEANING A READER OF THE DOMAIN SEES, so where
// it says what the error is the row is held to it, in both directions for a
// collision: a …Taken, …Clash or …Exists is a value another object holds, and a
// collision is named so. A row stating a meaning was otherwise free to state
// the wrong one and agree with its domain — a tag label another tag carries
// was `malformed`, and a label another version holds `objectState` because
// one sentinel also covered every label in use. A …Full is the object's own
// contents at their cap, its state, unless the vocabulary names a class for
// that limit (an inbox's marks).
func namedFor(name string, means meaning) string {
	collision := false
	for _, suffix := range []string{"Taken", "Clash", "Exists"} {
		collision = collision || strings.HasSuffix(name, suffix)
	}
	switch {
	case collision && means != taken:
		return "named for a collision, and its row says it is something else: a " +
			"value another object holds is `taken`, whatever the domain classes it"
	case !collision && means == taken:
		return "its row says it is a collision, and its name does not: name it " +
			"…Taken, …Clash or …Exists, so the domain says what it is where it is " +
			"declared"
	case strings.HasSuffix(name, "Full") && means != objectState && means != inboxCeiling:
		return "named for contents at their cap, and its row says it is something " +
			"else: the object's own contents at their cap are its state"
	}
	return ""
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
// domain's state-log name, then by the tool it is answered for, where an arm
// depends on which tool asked (write_project's own words for a tag it
// declares).
var toolFailures = map[string]map[string]func(error) tools.Result{
	tracker.Domain{}.Name(): {
		tracker.UpdateWorkItemTool: func(err error) tools.Result {
			return writeFailure(context.Background(), Actor{}, tracker.UpdateWorkItemTool, err)
		},
		tracker.WriteProjectTool: func(err error) tools.Result {
			return writeFailure(context.Background(), Actor{}, tracker.WriteProjectTool, err)
		},
	},
	pages.Domain{}.Name(): {"save_page": func(err error) tools.Result {
		return pageWriteFailure(context.Background(), "save_page", err, "")
	}},
	chat.Domain{}.Name(): {PostMessageTool: func(err error) tools.Result {
		return chatWriteFailure(context.Background(), PostMessageTool, err, "")
	}},
	artifactsdomain.Domain{}.Name(): {"publish_artifact": func(err error) tools.Result {
		return publishFailure(context.Background(), "publish_artifact", artifactCaller{}, err)
	}},
	artifactdata.Domain{}.Name(): {"write_artifact_data": func(err error) tools.Result {
		return dataFailure(context.Background(), "write_artifact_data", err)
	}},
}

// domainPackages is each native domain's package, relative to internal/, read
// off the domain's own type rather than its state-log name: the two differ —
// the artifacts domain is `artifacts`, and internal/artifacts is the
// artifacts' vocabulary, not the package whose writer refuses.
func domainPackages() map[string]string {
	internal := path.Dir(reflect.TypeFor[statelog.Position]().PkgPath())
	out := map[string]string{}
	for _, d := range []statelog.Domain{tracker.Domain{}, pages.Domain{}, chat.Domain{},
		artifactsdomain.Domain{}, artifactdata.Domain{}} {
		out[d.Name()] = strings.TrimPrefix(reflect.TypeOf(d).PkgPath(), internal+"/")
	}
	return out
}
