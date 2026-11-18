package builtin

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/foldername"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY DOMAIN A SURFACE WRITES THROUGH THE ESTATE ROUTER IS DECLARED TO THE
// SURFACES, and nothing else is.
//
// The person's write surface classes a tool-less writer's refusal and names a
// failed call's object from [nativeDomains] alone, so a routed domain missing
// there answers every content refusal as a 500 naming nothing. Read off the
// router's own table ([estate.SurfaceWriteDomains]), so a new domain's first
// surface write is red until declared. The engine's own writes (the executor
// log) are no surface's, so nothing classes them (internal/estate's
// engine-originated class).
//
// Mutation: drop any row of nativeDomains and this goes red.
func TestEveryRoutedDomainIsDeclaredToTheSurfaces(t *testing.T) {
	t.Parallel()
	routed := estate.SurfaceWriteDomains()
	if len(routed) == 0 {
		t.Fatal("the router names no domain a surface writes, so this gate compares nothing")
	}
	declared := slices.Sorted(maps.Keys(nativeDomains))
	if !slices.Equal(declared, routed) {
		t.Errorf("nativeDomains declares %v and surfaces write %v through the estate "+
			"router: declare each such domain's refusal classes and object "+
			"arguments, and nothing no surface writes", declared, routed)
	}
	for name, d := range nativeDomains {
		if d.class == nil || d.sentence == nil || len(d.objects) == 0 {
			t.Errorf("%s is declared with no class function, no sentence or no "+
				"object argument", name)
		}
	}
}

// meaning is what a refusal a domain decided MEANS to its caller, as a row
// declares it; the class follows from [meaning.class] alone, the one rule
// (internal/mcp's [tools.Refusal]). A row stating a class would agree with
// whatever its domain decided, so one kind of refusal could be `invalid` on one
// domain and `conflict` on another.
type meaning int

const (
	// malformed is the REQUEST: an argument outside its grammar, a value
	// past a limit of its own, or an argument naming another object that
	// cannot take part. The same request never lands; another one does.
	malformed meaning = iota + 1
	// absent is the object the call is about not being there, or gone for good.
	absent
	// notPermitted is a gesture that is not this caller's to make.
	notPermitted
	// staleRead is an object that moved since the caller read it.
	staleRead
	// lostRace is a write that lost its compare-and-set rounds.
	lostRace
	// objectState is the object refusing the gesture by what it is now
	// (archived, trashed, purging, frozen, at its cap, nothing to restore);
	// the same request lands once somebody changes it.
	objectState
	// taken is a create, or a name, another object already holds.
	taken
	// answered is a question that already has its answer.
	answered
	// inboxCeiling, handOffs and companyQuota are limits with a class of
	// their own: a surface names the remedy, which is no change of the object.
	inboxCeiling
	handOffs
	companyQuota
	// nodeCondition is this node, which waiting clears.
	nodeCondition
)

// class is the one rule: a refusal's class by its meaning, on every domain and
// surface. A race and a state share one because the request is not wrong and
// reading the object again is the move; a malformed request is another, since
// nothing done to the object makes it land.
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

// brandAdoption is why the brand's refusals are no tool's or write surface's:
// adopting a brand is an administrator's gesture on the brand routes, which
// answer it themselves (422 `brand_contrast`, `policy_refused`).
const brandAdoption = "an administrator adopting a brand on the brand routes, " +
	"never a tool's call or the write surface's"

// classRow is one declared error of a domain: what it means to its caller, or,
// with `why` set, why it is no refusal at all.
type classRow struct {
	err   error
	means meaning
	why   string
}

// domainErrors is every exported sentinel and error type each native domain
// declares, with what its refusal MEANS, keyed by the domain's state-log name,
// then by the Go identifier.
var domainErrors = map[string]map[string]classRow{
	artifactsdomain.Domain{}.Name(): {
		"ErrAboveOwnRole":      {err: artifactsdomain.ErrAboveOwnRole, means: notPermitted},
		"ErrAgentCompanyShare": {err: artifactsdomain.ErrAgentCompanyShare, means: notPermitted},
		"ErrBadCursor":         {err: artifactsdomain.ErrBadCursor, means: malformed},
		"ErrConflict":          {err: artifactsdomain.ErrConflict, means: lostRace},
		"ErrExists":            {err: artifactsdomain.ErrExists, means: taken},
		"ErrInvalid":           {err: artifactsdomain.ErrInvalid, means: malformed},
		// A LABEL ANOTHER VERSION HOLDS is a collision; every kept label in
		// use is the artifact's contents at their cap.
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
		"AlreadyExists":     {err: &artifactsdomain.AlreadyExists{What: "a-1"}, means: taken},
		"ErrFutureVersion": {err: &artifactsdomain.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"QuotaExceeded": {err: &artifactsdomain.QuotaExceeded{}, means: companyQuota},
		"VersionMoved":  {err: &artifactsdomain.VersionMoved{}, means: staleRead},

		// A FOLDER CYCLE, DEPTH OR AMBIGUOUS PATH is malformed: another
		// destination, or the folder named by its id, lands instead.
		"ErrFolderNotFound":  {err: artifactsdomain.ErrFolderNotFound, means: absent},
		"ErrFolderNameTaken": {err: artifactsdomain.ErrFolderNameTaken, means: taken},
		"ErrFolderLimit":     {err: artifactsdomain.ErrFolderLimit, means: objectState},
		"ErrFolderCycle":     {err: artifactsdomain.ErrFolderCycle, means: malformed},
		"ErrFolderDepth":     {err: artifactsdomain.ErrFolderDepth, means: malformed},
		"FolderAmbiguous":    {err: &artifactsdomain.FolderAmbiguous{}, means: malformed},

		// COMMENTS: words carrying a credential are a malformed request.
		"ErrCommentNotFound":   {err: artifactsdomain.ErrCommentNotFound, means: absent},
		"ErrCommentLimit":      {err: artifactsdomain.ErrCommentLimit, means: objectState},
		"ErrNotTheAuthor":      {err: artifactsdomain.ErrNotTheAuthor, means: notPermitted},
		"ErrCommentCredential": {err: artifactsdomain.ErrCommentCredential, means: malformed},

		// THE BRAND'S: see brandAdoption.
		"ErrBrandContrast":   {err: artifactsdomain.ErrBrandContrast, why: brandAdoption},
		"ErrBrandIncomplete": {err: artifactsdomain.ErrBrandIncomplete, why: brandAdoption},
		"BrandContrast":      {err: &artifactsdomain.BrandContrast{}, why: brandAdoption},
		"BrandIncomplete":    {err: &artifactsdomain.BrandIncomplete{}, why: brandAdoption},
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
		"ErrBadCursor":              {err: worklet.ErrBadCursor, means: malformed},
		"ErrBadQuery":               {err: worklet.ErrBadQuery, means: malformed},
		"ErrForbidden":              {err: worklet.ErrForbidden, means: notPermitted},
		"ErrIndexBuilding":          {err: worklet.ErrIndexBuilding, means: nodeCondition},
		"ErrInvalid":                {err: worklet.ErrInvalid, means: malformed},
		"ErrNoChartActivation":      {err: worklet.ErrNoChartActivation, why: "a chart apply's wiring, never a caller's request"},
		"ErrNoComment":              {err: worklet.ErrNoComment, means: absent},
		"ErrNoFile":                 {err: worklet.ErrNoFile, means: absent},
		"ErrNoView":                 {err: worklet.ErrNoView, means: absent},
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
		"ErrTaken": {err: &worklet.Refusal{Mark: worklet.ErrTaken, Err: errors.New("x")},
			means: taken},
		// THE ITEM OR PROJECT refusing by what it is: archived, frozen in
		// the trash, its tags or pins at their cap.
		"ErrState": {err: &worklet.Refusal{Mark: worklet.ErrState, Err: errors.New("x")},
			means: objectState},
		"ErrStepUnresolved": {err: worklet.ErrStepUnresolved, why: "an unknown outcome, answered as one (UnknownCause)"},
		"ErrStepUnvouched": {err: errors.Join(worklet.ErrStepUnresolved, worklet.ErrStepUnvouched),
			why: "an unknown outcome, answered as one (UnknownCause)"},
		"ErrTooBroad": {err: worklet.ErrTooBroad, means: malformed},
		// AN UPLOAD TOO OLD TO NAME is its state, as the artifacts' is:
		// uploading again lands, waiting does not.
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
		// A LABEL ANOTHER TAG CARRIES, or an id the task already holds, is a
		// collision.
		"TagClash":     {err: &worklet.TagClash{}, means: taken},
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
		// tool-skill pages alone, so the other order or the skills filter
		// lands, as with a cursor from another listing.
		"ErrUnservedOrder": {err: doclet.ErrUnservedOrder, means: malformed},

		"ErrFutureVersion": {err: &doclet.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"ErrUnknownVersion": {err: doclet.ErrUnknownVersion{},
			why: "a document version this build cannot read, a fault of the copy"},
		"InvalidError": {err: &doclet.InvalidError{}, means: malformed},
	},
	chatlet.Domain{}.Name(): {
		// AN ARCHIVED ROOM, or a message at its emoji cap, is the object's
		// state: the request lands once somebody reopens the room or takes an
		// emoji off.
		"ErrArchived":      {err: chatlet.ErrArchived, means: objectState},
		"ErrReactionsFull": {err: chatlet.ErrReactionsFull, means: objectState},
		"ErrConflict":      {err: chatlet.ErrConflict, means: lostRace},
		// MEMBERS THAT MOVED since the caller read them: read them again.
		"ErrMembersChanged": {err: chatlet.ErrMembersChanged, means: staleRead},
		"ErrForbidden":      {err: chatlet.ErrForbidden, means: notPermitted},
		"ErrInvalid":        {err: chatlet.ErrInvalid, means: malformed},
		"ErrNameTaken":      {err: chatlet.ErrNameTaken, means: taken},
		"ErrNotFound":       {err: chatlet.ErrNotFound, means: absent},
		"ErrRoomExists":     {err: chatlet.ErrRoomExists, means: taken},
		// THIS NODE'S CONDITION, NOT A PERMISSION: the call lands once the
		// node is upgraded.
		"ErrUnknownKind":  {err: chatlet.ErrUnknownKind, means: nodeCondition},
		"ErrNoActivation": {err: chatlet.ErrNoActivation, why: "a configuration apply's wiring, never a caller's request"},

		"ErrFutureVersion": {err: &chatlet.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
		"ErrUnknownVersion": {err: chatlet.ErrUnknownVersion{},
			why: "a document version this build cannot read, a fault of the copy"},
		"InvalidError": {err: &chatlet.InvalidError{}, means: malformed},
	},
	gitlet.Domain{}.Name(): {
		"ErrRepoNotFound":   {err: gitlet.ErrRepoNotFound, means: absent},
		"ErrRefNotFound":    {err: gitlet.ErrRefNotFound, means: absent},
		"ErrChangeNotFound": {err: gitlet.ErrChangeNotFound, means: absent},
		"ErrCheckNotFound":  {err: gitlet.ErrCheckNotFound, means: absent},
		"ErrObjectNotFound": {err: gitlet.ErrObjectNotFound, means: absent},
		"ErrNotATree":       {err: gitlet.ErrNotATree, means: absent},
		"ErrNotABlob":       {err: gitlet.ErrNotABlob, means: absent},
		// A REPOSITORY PURGING, within its deleted retention, not deleted or
		// config-managed refuses by what it is now: the request lands once
		// somebody changes that.
		"ErrPurging":       {err: gitlet.ErrPurging, means: objectState},
		"ErrNotYet":        {err: gitlet.ErrNotYet, means: objectState},
		"ErrNotDeleted":    {err: gitlet.ErrNotDeleted, means: objectState},
		"ErrConfigManaged": {err: gitlet.ErrConfigManaged, means: objectState},
		"ErrSnapshotMoved": {err: gitlet.ErrSnapshotMoved, means: staleRead},
		"ErrNotPermitted":  {err: gitlet.ErrNotPermitted, means: notPermitted},
		// THE SEAT A TURN READS FOR, not the asker: whom the call is made
		// for, which the same turn cannot change.
		"ErrAboveCeiling":  {err: gitlet.ErrAboveCeiling, means: notPermitted},
		"ErrInvalid":       {err: gitlet.ErrInvalid, means: malformed},
		"ErrBadCursor":     {err: gitlet.ErrBadCursor, means: malformed},
		"ErrBadLevel":      {err: gitlet.ErrBadLevel, means: malformed},
		"ErrSliceTooLarge": {err: gitlet.ErrSliceTooLarge, means: malformed},
		"ErrRecordCap":     {err: gitlet.ErrRecordCap, means: malformed},
		// THIS NODE'S CONDITION: another node, or this one a moment later,
		// answers the same call.
		"ErrNotServing": {err: gitlet.ErrNotServing, means: nodeCondition},
		"ErrAnswerDue":  {err: gitlet.ErrAnswerDue, means: nodeCondition},
		"ErrGitUnready": {err: &gitlet.ErrGitUnready{}, means: nodeCondition},

		"ErrNoActivation":     {err: gitlet.ErrNoActivation, why: "a configuration apply's wiring, never a caller's request"},
		"ErrSuperseded":       {err: gitlet.ErrSuperseded, why: "a configuration reconcile a later activation fenced off, never a caller's request"},
		"ErrNotConfigManaged": {err: gitlet.ErrNotConfigManaged, why: "a configuration reconcile reaching a repository it does not manage, never a caller's request"},
		"ErrInvalidProposal": {err: gitlet.ErrInvalidProposal,
			why: "a proposal the domain's own committer built wrong, decided by the applier as a fault"},
		"TooWideError": {err: &gitlet.TooWideError{},
			why: "a push too wide for one entry, which the client pushes in parts and a committer never builds"},
		"ChartRefusal": {err: &gitlet.ChartRefusal{},
			why: "the deciding chart's refusal of whom a write is made for, answered only inside the class its " +
				"holder gives it, ErrInvalidProposal on a committer and ErrInvalid on a Gitlet node, which class it"},
		"ErrInvalidLayer": {err: gitlet.ErrInvalidLayer, why: "a published layer a Gitlet node built wrong, the cache's fault"},
		"ErrBadLayer":     {err: gitlet.ErrBadLayer, why: "a stored layer that is not what its row says, a fault of the store"},
		"ErrBadBundle":    {err: gitlet.ErrBadBundle, why: "a stored pack that is not what its row says, a fault of the store"},
		"BadBundle":       {err: &gitlet.BadBundle{Err: gitlet.ErrBadBundle}, why: "a stored pack that is not what its row says, a fault of the store"},
		"ErrMisplaced": {err: gitlet.ErrMisplaced,
			why: "a re-anchoring answer placing another set than its anchors, which no Gitlet node of this build " +
				"gives: the asking node's own fault, never a caller's request"},
		"ErrFutureVersion": {err: &gitlet.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
	},
	atlasdomain.Domain{}.Name(): {
		"ErrInvalid":      {err: atlasdomain.ErrInvalid, means: malformed},
		"ErrStaleVersion": {err: atlasdomain.ErrStaleVersion, means: staleRead},
		// A PURGED ENTRY OR VALUE is gone for good, on every node.
		"ErrPurged":   {err: atlasdomain.ErrPurged, means: absent},
		"ErrConflict": {err: atlasdomain.ErrConflict, means: lostRace},
		"ErrExists":   {err: atlasdomain.ErrExists, means: taken},
		// AN HOUR WITH NOTHING PAST THE RETENTION is its state: the reaper's
		// expiry, which its next pass decides again.
		"ErrNotDue": {err: atlasdomain.ErrNotDue, means: objectState},
		"ErrFutureVersion": {err: &atlasdomain.ErrFutureVersion{},
			why: "a record a newer build wrote, which the framework defers rather than refuses"},
	},
}

// EVERY ERROR A NATIVE DOMAIN DECLARES IS CLASSED, or named as no refusal: an
// unclassed one is a 500, the node's fault, from a tool ([writeFailure] falls
// back to [WriterClass]) and the write surface alike. Parsed from each domain's
// source, so a new sentinel is red until somebody says what it is. Every row
// states a MEANING, never a class: the class follows by [meaning.class] on the
// domains' table and their tools ([toolFailures]) alike, and a row is held to
// its error's name ([namedFor]).
//
// Mutation: drop an arm from trackerClass, pageClass or chatClass; class
// chatlet.ErrArchived, or worklet.TagsFull or TagClash in writeFailure alone,
// `invalid`; state TagClash `malformed`; or add an exported sentinel to a
// domain: each goes red.
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
			// AND BY THE DOMAIN'S OWN TOOLS, which state the class again:
			// one error is one class on every surface.
			for tool, failure := range toolFailures[domain] {
				if got := tools.RefusalOf(failure(row.err)); got != want {
					t.Errorf("%s.%s is classed %q by %s, and what it means is %q",
						domain, name, string(got), tool, string(want))
				}
			}
		}
	}
	// THE FRAMEWORK'S OWN, which any domain's writer may return as is, and
	// the inbox's ceiling, which Worklet and Gitlet refuse with.
	for err, want := range map[error]tools.Refusal{
		statelog.ErrExists: tools.RefusalExists, statelog.ErrConflict: tools.RefusalConflict,
		inbox.ErrFull: tools.RefusalInboxFull,
	} {
		if class, ok := WriterClass(err); !ok || class != want {
			t.Errorf("WriterClass(%v) = %q, %v; want %q", err, class, ok, want)
		}
	}
}

// NO DOMAIN REFUSAL READS AS A GO ERROR, on any surface: every error each
// native domain declares, answered by each of its tools and in the deciding
// domain's words a tool's fallback tells ([WriterSentence]), carries no Go
// package prefix and no framework error after it. The prefix is for a log, and
// what follows a framework error is its subject, a broker path.
//
// Mutation: print the tracker's lost race or a missing page with %v again, or
// drop the prefix strip from worklet.Sentence, and a row goes red.
func TestNoDomainRefusalReadsAsAGoError(t *testing.T) {
	t.Parallel()
	prefixes := []string{"worklet: ", "doclet: ", "chatlet: ", "gitlet: ", "statelog: ", "artifactsdomain: ",
		"artifactdata: ", "artifacts: ", "%!"}
	leaks := func(text string) string {
		for _, p := range prefixes {
			if strings.Contains(text, p) {
				return p
			}
		}
		return ""
	}
	for _, domain := range slices.Sorted(maps.Keys(domainErrors)) {
		for _, name := range slices.Sorted(maps.Keys(domainErrors[domain])) {
			row := domainErrors[domain][name]
			if row.why != "" {
				continue
			}
			for tool, failure := range toolFailures[domain] {
				if p := leaks(failure(row.err).Output); p != "" {
					t.Errorf("%s.%s, answered by %s, carries %q: %s", domain, name, tool, p,
						failure(row.err).Output)
				}
			}
			if said, ok := WriterSentence(row.err); ok && leaks(said) != "" {
				t.Errorf("%s.%s, in the deciding domain's words, carries %q: %s",
					domain, name, leaks(said), said)
			}
		}
	}
	// AND THE FRAMEWORK'S OWN TWO, bare and as a domain wraps them ("doclet:
	// the page kept changing under this write: <subject>: statelog: conflict").
	for _, err := range []error{
		fmt.Errorf("%w: crewlet.tracker.task.x kept changing under this write", statelog.ErrConflict),
		fmt.Errorf("%w: crewlet.tracker.task.x", statelog.ErrExists),
		fmt.Errorf("%w: crewlet.pages.page.x: %w", doclet.ErrConflict, statelog.ErrConflict),
		fmt.Errorf("%w: room-1: %w", chatlet.ErrConflict, statelog.ErrConflict),
	} {
		for domain, failures := range toolFailures {
			for tool, failure := range failures {
				if _, ok := WriterClass(err); !ok {
					continue
				}
				if out := failure(err).Output; leaks(out) != "" {
					t.Errorf("%v, answered by %s's %s, carries %q: %s", err, domain, tool,
						leaks(out), out)
				}
			}
		}
		if said, ok := WriterSentence(err); !ok || leaks(said) != "" {
			t.Errorf("%v is told in the deciding domain's words as %q (%v)", err, said, ok)
		}
	}
}

// NO ARTIFACT OR DATASTORE REFUSAL READS AS A GO ERROR, on any surface: a
// refusal wrapped as those writers and readers really build it (a sentinel
// around a sentence, around the deciding package's refusal, or around a lost
// race's framework error), answered by the domain's tools and in the deciding
// domain's words ([WriterSentence]), carries no package name, class word or
// framework error. [TestNoDomainRefusalReadsAsAGoError] sees only bare
// sentinels, whose cut text ("invalid") leaks nothing; wrapped, the class word
// survives a cut of the package name alone ("invalid: invalid: the path …").
//
// Mutation: drop a class word from the artifacts' or the datastore's
// sentence cuts, or word a tool's refusal with the error's text again, and a
// row goes red.
func TestNoArtifactRefusalReadsAsAGoError(t *testing.T) {
	t.Parallel()
	leaks := func(text string) string {
		for _, p := range []string{"artifactsdomain:", "artifactupload:", "artifactdata:",
			"manifest:", "artifacts:", "worklet:", "foldername:", "query:", "canon:", "canvasformat:",
			"statelog", "invalid:", "forbidden:", "%!"} {
			if strings.Contains(text, p) {
				return p
			}
		}
		return ""
	}
	_, badPath := manifest.CleanPath("pages//index.html")
	_, badAudience := artifacts.ParseAudience("nobody")
	_, _, badName := foldername.Check("..")
	if badPath == nil || badAudience == nil || badName == nil {
		t.Fatal("the refusals this gate wraps were not refused")
	}
	lostOn := fmt.Errorf("%w: crewlet.artifacts.artifact.a-1 kept changing under this write",
		statelog.ErrConflict)
	for domain, rows := range map[string]map[string]error{
		artifactsdomain.Domain{}.Name(): {
			"a field": fmt.Errorf("%w: kind %q is not one of %v",
				artifactsdomain.ErrInvalid, "zine", artifacts.Kinds),
			"a manifest's path": fmt.Errorf("%w: %w", artifactsdomain.ErrInvalid, badPath),
			"a folder's name":   fmt.Errorf("%w: %w", artifactsdomain.ErrInvalid, badName),
			"an audience":       fmt.Errorf("%w: %w", artifactsdomain.ErrInvalid, badAudience),
			"the pipeline's": fmt.Errorf("%w: a layout is a design's starter, and a "+
				"design starts from one", artifactupload.ErrInvalid),
			"a starter's": fmt.Errorf("%w: %w", artifactupload.ErrInvalid,
				fmt.Errorf("%w: boards: a board needs a path", canvasformat.ErrInvalid)),
			"a lost race": fmt.Errorf("%w: a-1: %w", artifactsdomain.ErrConflict, lostOn),
		},
		artifactdata.Domain{}.Name(): {
			"a field": fmt.Errorf("%w: collection %q is not in the schema",
				artifactdata.ErrInvalid, "rows"),
			"a standing": fmt.Errorf("%w: only an editor undoes another's write",
				artifactdata.ErrForbidden),
			"a query":     fmt.Errorf("%w: 9 filters is more than 8", query.ErrInvalid),
			"a lost race": fmt.Errorf("%w: a-1: %w", artifactdata.ErrConflict, lostOn),
		},
	} {
		for name, err := range rows {
			for tool, failure := range toolFailures[domain] {
				if out := failure(err).Output; leaks(out) != "" {
					t.Errorf("%s's %s, answered by %s, carries %q: %s", domain, name, tool,
						leaks(out), out)
				}
			}
			// THE DECIDING DOMAIN'S WORDS, for a refusal a domain writer
			// decided: the pipeline's and a query's are no domain's, and
			// their surfaces word them.
			if said, ok := WriterSentence(err); ok && leaks(said) != "" {
				t.Errorf("%s's %s is told in the deciding domain's words as %q",
					domain, name, said)
			}
		}
	}
}

// A STATE OR A COLLISION IS WORDED FOR A PERSON: every declared error meaning
// the object's state, a lost race or a collision, as [WriterSentence] words it,
// carries no em dash, no snake_case name outside backticks and no Go package
// prefix. The words are a person's, though a person's surfaces answer these at
// ADR-0060's floor rather than in them and a tool's fallback tells them to a
// model ([domainRefusal]); the tools' own words, a model's, are held to nothing.
//
// Mutation: tell a lost race in the tracker's tool words again, or put an em
// dash back in a domain's state sentence, and a row goes red.
func TestAStateOrACollisionIsWordedForAPerson(t *testing.T) {
	t.Parallel()
	snake := regexp.MustCompile(`\b[a-z]+_[a-z_]+\b`)
	for _, domain := range slices.Sorted(maps.Keys(domainErrors)) {
		for _, name := range slices.Sorted(maps.Keys(domainErrors[domain])) {
			row := domainErrors[domain][name]
			switch row.means {
			case objectState, lostRace, taken:
			default:
				continue
			}
			said, ok := WriterSentence(row.err)
			if !ok {
				t.Errorf("%s.%s is a refusal no domain words", domain, name)
				continue
			}
			prose := outsideBackticks(said)
			switch {
			case strings.Contains(said, "—"):
				t.Errorf("%s.%s is told with an em dash: %s", domain, name, said)
			case snake.MatchString(prose):
				t.Errorf("%s.%s names %q in words for a person: %s", domain, name,
					snake.FindString(prose), said)
			case strings.Contains(said, ": ") && strings.HasSuffix(strings.SplitN(said, ": ", 2)[0], domain):
				t.Errorf("%s.%s opens with its package: %s", domain, name, said)
			}
		}
	}
}

// outsideBackticks is text with every backticked span taken out: a value a
// sentence names on purpose is marked as one.
func outsideBackticks(text string) string {
	parts := strings.Split(text, "`")
	var prose strings.Builder
	for i := 0; i < len(parts); i += 2 {
		prose.WriteString(parts[i])
	}
	return prose.String()
}

// namedFor is why an error's NAME and its row's meaning disagree, or "" where
// they agree. The name is the one statement of meaning a reader of the domain
// sees, so the row is held to it, or it could state a wrong meaning and still
// agree with its domain: a …Taken, …Clash or …Exists is a collision and a
// collision is named so, and a …Full is the object's contents at their cap,
// its state, unless the vocabulary names a class for that limit (an inbox's).
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

// toolFailures is the fallback each native domain's write tools answer a
// writer's refusal with, keyed by the domain's state-log name, then by tool,
// since an arm can depend on which tool asked (write_project's own words for a
// tag it declares).
var toolFailures = map[string]map[string]func(error) tools.Result{
	worklet.Domain{}.Name(): {
		worklet.UpdateWorkItemTool: func(err error) tools.Result {
			return writeFailure(context.Background(), Actor{}, worklet.UpdateWorkItemTool, err)
		},
		worklet.WriteProjectTool: func(err error) tools.Result {
			return writeFailure(context.Background(), Actor{}, worklet.WriteProjectTool, err)
		},
	},
	doclet.Domain{}.Name(): {"save_page": func(err error) tools.Result {
		return pageWriteFailure(context.Background(), "save_page", err, "")
	}},
	chatlet.Domain{}.Name(): {PostMessageTool: func(err error) tools.Result {
		return chatWriteFailure(context.Background(), PostMessageTool, err, "")
	}},
	artifactsdomain.Domain{}.Name(): {"publish_artifact": func(err error) tools.Result {
		return publishFailure(context.Background(), "publish_artifact", artifactCaller{}, err)
	}},
	artifactdata.Domain{}.Name(): {"write_artifact_data": func(err error) tools.Result {
		return dataFailure(context.Background(), "write_artifact_data", err)
	}},
	atlasdomain.Domain{}.Name(): {AtlasCorrectTool: func(err error) tools.Result {
		return atlasWriteFailure(context.Background(), AtlasCorrectTool, Actor{}, atlas.Ref{}, err)
	}},
}

// domainPackages is each native domain's package, relative to internal/, read
// off the domain's type rather than its state-log name: the artifacts domain is
// `artifacts`, but internal/artifacts is the vocabulary, not its writer.
func domainPackages() map[string]string {
	internal := path.Dir(reflect.TypeFor[statelog.Position]().PkgPath())
	out := map[string]string{}
	for _, d := range []statelog.Domain{worklet.Domain{}, doclet.Domain{}, chatlet.Domain{},
		artifactsdomain.Domain{}, artifactdata.Domain{}, gitlet.Domain{}, atlasdomain.Domain{}} {
		out[d.Name()] = strings.TrimPrefix(reflect.TypeOf(d).PkgPath(), internal+"/")
	}
	return out
}
