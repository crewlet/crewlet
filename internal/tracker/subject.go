// Package tracker is the engine's own work tracker as a state-log domain.
//
// # What a record is, and what the subject is for
//
// Every change is one RECORD on one ordered log, published to the subject of
// the object it changes and conditioned on that object's last sequence there.
// The subject is therefore the ARBITRATION UNIT, not a routing label: two
// writers racing on one task contend at the broker and exactly one wins, and
// two writers on different tasks never contend at all. That is the whole
// concurrency design, and it is why a rank move — which changes a project's
// ORDER, an object no single task owns — has a subject of its own rather than
// riding one of the tasks it moves.
//
// # Why the record decodes in two passes
//
// The envelope decodes at EVERY version, before the version is consulted; the
// payload decodes only at a version this build knows. A record a node cannot
// read is therefore still a record it can file under its subject, probe for,
// drop through an eviction gate and reprocess later — none of which is
// possible for a record that failed to decode at all, because such a record
// yields no id, no kind and no subject to file it under. A rolling upgrade
// puts exactly that record on the wire.
package tracker

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// ObjectKind is what an object on the mutation log is.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// heard of — and the literal is RETAINED either way. A newer peer publishes a
// kind this build does not know, and the deferral this build files it under
// forms a family term out of that literal: replacing it with a sentinel would
// make the writer's own deferral probe miss the record it is meant to see.
type ObjectKind string

// The fifteen kinds.
//
// EXPORTED AND ENUMERATED because four readers that cannot see each other all
// compare against them: the publisher builds the subject, the wake feed's
// filter includes or excludes the kind, the applier's dispatch switches on it,
// and the domain's own Tables declaration must classify every one. A kind
// added to three of the four is a record that publishes, is delivered, wakes
// nobody and writes nothing.
const (
	// KindTask is one work item, and the overwhelming majority of records.
	KindTask ObjectKind = "task"

	// KindProject is a project's settings, its sprint policy and its
	// active-sprint pointer.
	KindProject ObjectKind = "project"

	// KindSprint is one sprint of one project. Its id is
	// "<PROJECT>.<number>", so THE SUBJECT IS THE IDENTITY: minting one
	// is a create-only append at an expectation of zero, and two nodes
	// minting number 7 collide harmlessly at the broker with nothing to
	// repair afterwards.
	KindSprint ObjectKind = "sprint"

	// KindCounter is a project's key sequence.
	//
	// ITS OWN SUBJECT, for the reason the bucket design already gave:
	// putting the counter on the project object would make every key mint
	// contend with every edit to that project's settings.
	KindCounter ObjectKind = "counter"

	// KindTags is a project's tag set.
	KindTags ObjectKind = "tags"

	// KindCatalogue is a workspace catalogue — "types" or "fields".
	KindCatalogue ObjectKind = "catalogue"

	// KindView is a saved view.
	KindView ObjectKind = "view"

	// KindGoal is a goal and its targets.
	KindGoal ObjectKind = "goal"

	// KindPerson is one person's inbox, priorities and pins.
	KindPerson ObjectKind = "person"

	// KindAlias is a cross-project move's create-only claim on a former
	// key, where the unique tuple IS the subject.
	KindAlias ObjectKind = "alias"

	// KindTurn is a turn's spend, and the ONE ADDITIVE kind: it carries no
	// expectation, bumps no object's version, and its apply's running
	// totals are gated on its own id insert affecting a row.
	KindTurn ObjectKind = "turn"

	// KindGeneration is a reanchor's record, create-only at an expectation
	// of zero: two operators deriving the same number race there and
	// exactly one wins.
	KindGeneration ObjectKind = "generation"

	// KindEviction is a node's eviction or its readmission.
	//
	// ONE OF THE TWO KINDS THAT INSTALL A GATE, which is why its version
	// is pinned at 1 for ever: a node that deferred an eviction would
	// leave its own gate table empty and go on applying every record the
	// evicted node appends, and there is no inverse that repairs it.
	KindEviction ObjectKind = "eviction"

	// KindRankOrder is a project's manual order.
	//
	// Its own kind because the object a drag mutates is the project's
	// ORDER — a total order over its tasks that no single task owns and
	// no single task's version can protect.
	KindRankOrder ObjectKind = "rankorder"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain.
	//
	// The only kind that writes no row on any node, which is why its
	// table declaration is the EMPTY set stated explicitly rather than
	// left out: a kind that writes nothing must not be able to slip
	// through the completeness walk by writing nothing.
	KindBarrier ObjectKind = "barrier"
)

// ObjectKinds are the fifteen, in the order they are documented.
var ObjectKinds = []ObjectKind{
	KindTask, KindProject, KindSprint, KindCounter, KindTags,
	KindCatalogue, KindView, KindGoal, KindPerson, KindAlias,
	KindTurn, KindGeneration, KindEviction, KindRankOrder, KindBarrier,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation.
//
// THIRTEEN OF FIFTEEN DO. A turn is ADDITIVE — it records spend that happened
// and races nobody — and a barrier is arbitrated by nothing at all: every
// barrier shares one subject, so an expectation there would serialise the
// whole company's linearizable reads behind one another and write an anchor
// row per read into the transaction holding this store's only writer.
func (k ObjectKind) Arbitrated() bool {
	switch k {
	case KindTurn, KindBarrier:
		return false
	}
	return k.Valid()
}

// InstallsGate reports whether a record on this kind installs an apply gate.
//
// Answered from the KIND ALONE, because it has to be answerable by a node
// that cannot decode the payload: it is what turns an unknown version into a
// STOP rather than a deferral.
func (k ObjectKind) InstallsGate() bool { return k == KindEviction }

// CatalogueTypes and CatalogueFields are the two catalogue objects. The
// catalogue kind is the one kind with a closed, tiny id set, and naming them
// is what stops a third appearing by typo.
const (
	CatalogueTypes  = "types"
	CatalogueFields = "fields"
)

// Subject is one object on the mutation log.
//
// ITS WIRE NAMES ARE LOWERCASE AND ITS ID IS NEVER OMITTED. Both are format
// decisions rather than style: the field names are inside every record on the
// log for the life of the deployment, and the empty id is what makes the
// barrier's own record byte-identical whatever a build's struct tags happen to
// do — which matters because the barrier's size is the figure the read
// index's whole cost model is derived from.
type Subject struct {
	Kind ObjectKind `json:"kind"`
	ID   string     `json:"id"`
}

// TaskSubject, ProjectSubject and the rest are the constructors.
//
// ONE PER KIND rather than a single Subject{Kind, ID} literal at every call
// site, because half the ids are composed — a sprint's is "<PROJECT>.<n>",
// an alias claim's is "<KEY>.<n>" — and a composition written twice is a
// subject two writers disagree about.
func TaskSubject(id string) Subject     { return Subject{Kind: KindTask, ID: id} }
func ProjectSubject(key string) Subject { return Subject{Kind: KindProject, ID: key} }
func CounterSubject(key string) Subject { return Subject{Kind: KindCounter, ID: key} }
func TagsSubject(key string) Subject    { return Subject{Kind: KindTags, ID: key} }
func ViewSubject(id string) Subject     { return Subject{Kind: KindView, ID: id} }
func GoalSubject(id string) Subject     { return Subject{Kind: KindGoal, ID: id} }
func PersonSubject(handle string) Subject {
	return Subject{Kind: KindPerson, ID: handle}
}
func RankOrderSubject(key string) Subject {
	return Subject{Kind: KindRankOrder, ID: key}
}
func EvictionSubject(nodeID string) Subject {
	return Subject{Kind: KindEviction, ID: nodeID}
}
func TurnSubject(id string) Subject { return Subject{Kind: KindTurn, ID: id} }

// SprintSubject names one sprint of one project.
func SprintSubject(project string, number int) Subject {
	return Subject{Kind: KindSprint, ID: fmt.Sprintf("%s.%d", project, number)}
}

// AliasSubject names a cross-project move's create-only claim on a former key.
//
// The attempt NUMBER is part of the id because the claim is create-only: a
// second move of the same key is a second claim, and re-using the first
// subject would make it a lost race rather than a new fact.
func AliasSubject(formerKey string, attempt int) Subject {
	return Subject{Kind: KindAlias, ID: fmt.Sprintf("%s.%d", formerKey, attempt)}
}

// CatalogueSubject names one of the two workspace catalogues.
func CatalogueSubject(which string) Subject {
	return Subject{Kind: KindCatalogue, ID: which}
}

// GenerationSubject names one generation transition.
func GenerationSubject(gen uint32) Subject {
	return Subject{Kind: KindGeneration, ID: fmt.Sprintf("%d", gen)}
}

// BarrierSubject is the read index's one subject.
func BarrierSubject() Subject { return Subject{Kind: KindBarrier} }

// String renders the subject's own path — what the framework appends to the
// domain's subject prefix, and what a scope term names.
func (s Subject) String() string {
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + "." + s.ID
}

// Wire is the full subject the record is published to.
func (s Subject) Wire() string {
	return topics.TrackerLogSubject(string(s.Kind), s.ID)
}

// Validate refuses a subject that cannot address an object.
//
// A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED HERE. It is refused where a
// record is WRITTEN and accepted where one is READ, which is the asymmetry
// the whole two-pass decode exists for: this build must be able to hold a
// newer peer's record under its own subject without being able to act on it.
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("tracker: a subject with no kind addresses the log's " +
			"own prefix, which is a real subject inside the stream's wildcard " +
			"that no applier has a case for")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("tracker: subject kind %q carries a separator or a "+
			"wildcard, so the kind and the id could not be told apart again",
			s.Kind)
	}
	if s.ID == "" && s.Kind != KindBarrier {
		return fmt.Errorf("tracker: a %s subject needs an id — only the barrier "+
			"is a kind with exactly one object", s.Kind)
	}
	if strings.ContainsAny(s.ID, " \t\n*>") {
		return fmt.Errorf("tracker: subject id %q carries whitespace or a "+
			"wildcard, which the broker would read as a subject pattern", s.ID)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the mutation log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.TrackerLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}

// HomedInAProject reports a kind whose scope path is qualified by a project it
// does not name itself.
//
// # The four, and why they are the four
//
// A scope path is a containment hierarchy, so an object's own path sits under
// its container's — which is what makes a project-wide deferral cover its
// tasks. Most kinds derive that container from their own subject: a project,
// counter, tag set or rank order IS a container key; a sprint and an alias
// carry the project in their id; a catalogue and a person live in a family;
// and the three fleet-wide kinds are about the whole domain.
//
// These four do not. A task's subject is a uuid and its project is a mutable
// column, and a turn's subject is the task it is about — so for both the
// container is a fact only the writer holds. A view and a goal may live in a
// project or at the top of the company, so their container is a choice rather
// than a lookup.
//
// [KindTask] and [KindTurn] additionally REQUIRE one: there is no such thing
// as a task outside a project, so an empty container there is a writer that
// forgot rather than a workspace-homed object.
func (k ObjectKind) HomedInAProject() bool {
	switch k {
	case KindTask, KindTurn, KindView, KindGoal:
		return true
	}
	return false
}

// RequiresAProject reports a kind that cannot live at the top of the company.
func (k ObjectKind) RequiresAProject() bool {
	return k == KindTask || k == KindTurn
}

// splitSprintID takes a sprint subject's id apart.
//
// The id IS "<PROJECT>.<number>", which is what makes minting sprint 7 a
// create-only append two nodes collide harmlessly on — so taking it apart is
// the inverse of [SprintSubject] and lives beside it rather than at the one
// call site that needs it.
func splitSprintID(id string) (project string, number int, err error) {
	project, rest, ok := strings.Cut(id, ".")
	if !ok || project == "" {
		return "", 0, fmt.Errorf("tracker: %q is not a sprint id — a sprint's "+
			"id is its project key, a dot and its number", id)
	}
	number, err = strconv.Atoi(rest)
	if err != nil || number < 1 {
		return "", 0, fmt.Errorf("tracker: sprint id %q names no number", id)
	}
	return project, number, nil
}
