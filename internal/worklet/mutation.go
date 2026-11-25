package worklet

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// RecordVersion is the highest record version this build READS, never the one
// it stamps: a writer stamps the lowest version that reads the record
// ([minimumRecordVersion], computed by [MutationRecord.Encode]), so an old node
// in a rolling upgrade holds back only the records it would apply lossily.
//
// A record above it is RETAINED at its position, envelope decoded, never
// skipped (package doc, "Why the record decodes in two passes"); one installing
// a gate stops the applier instead, since a deferred gate licenses every later
// record on this node and the eviction gate has no inverse that repairs it.
// It moves ONLY with [versionedFields], whose rows name each version's fields,
// to that table's highest version: statelogtest refuses a build reading a
// version no field introduced, which would drop the field it was minted for.
const RecordVersion = 22

// AppsKeptVersion is the version from which the applier keeps a record's
// apps on its history row; every older record keeps an empty column.
const AppsKeptVersion = 4

// WorkingForVersion is the version from which a record carries whom it was
// written for ([MutationRecord.WorkingFor]), which the applier keeps on its
// history row; every older record keeps the column NULL.
const WorkingForVersion = 13

// NoticeAsksVersion is the version from which the applier keeps a comment
// author's statement on its notice rows; every older record leaves it NULL.
const NoticeAsksVersion = 6

// NoticeWokeVersion is the version from which the applier keeps on a notice row
// whether the change woke its recipient; every older record reads it as woken.
const NoticeWokeVersion = 7

// baseRecordVersion is version 1: what every build reads, and what a record
// carrying no versioned field is stamped at.
//
// THE BARRIER AND A REANCHOR'S GENERATION ARE PINNED AT IT FOR EVER, as a gate
// is at [GateRecordVersion], never left to the table: an older node retains a
// record it cannot read, a retained barrier is a deferral row for every
// linearizable read (and blocks its snapshots until it upgrades), and a
// retained generation is a transition it never makes. [MutationRecord.Encode]
// refuses either carrying a versioned field ([RecordEnvelope.readByEveryBuild]).
const baseRecordVersion = 1

// versionedFields is every field a tracker record has gained since the base
// format, with the version a reader must be at to apply a record carrying it
// (package doc, "Every new field is version-gated, and every new derived
// column re-derived").
//
// A new field on any record, payload or document, shipped by a tag or not,
// takes the next version above [RecordVersion], never one a build already
// reads (TestAClosedRecordVersionGainsNoField). Its row names the JSON path
// from the record's root (through `mutation` for a payload field), and a record
// in the domain's statelogtest candidate carrying it certifies that path. A
// changed rule for a field the applier already copies takes the same gate.
var versionedFields = statelog.RecordFields{
	{Name: "MutationRecord.ActorApps", Since: 2, Path: []string{"actor_apps"}},
	{Name: "Snapshot.ThreadOwing", Since: 3, Path: []string{"notify", "snapshot", "thread_owing"}},
	{Name: "Snapshot.ThreadAwaiting", Since: 3, Path: []string{"notify", "snapshot", "thread_awaiting"}},
	{Name: "MutationRecord.ActorAppsKept", Since: AppsKeptVersion, Path: []string{"actor_apps_kept"}},
	{Name: "Snapshot.CommentAsks", Since: 5, Path: []string{"notify", "snapshot", "comment_asks"}},
	{Name: "Snapshot.MentionedAgents", Since: 5, Path: []string{"notify", "snapshot", "mentioned_agents"}},
	{Name: "MutationRecord.NoticeAsksKept", Since: NoticeAsksVersion, Path: []string{"notice_asks_kept"}},
	{Name: "MutationRecord.NoticeWokeKept", Since: NoticeWokeVersion, Path: []string{"notice_woke_kept"}},
	{Name: "TaskPatch.AskWatch", Since: 8, Path: []string{"mutation", "ask_watch"}},
	{Name: "TaskPatch.WaitReason", Since: 9, Path: []string{"mutation", "wait_reason"}},
	{Name: "TaskPatch.ClearStart", Since: 10, Op: string(OpPatch), Path: []string{"mutation", "clear_start"}},
	{Name: "TaskPatch.ClearDue", Since: 10, Op: string(OpPatch), Path: []string{"mutation", "clear_due"}},
	{Name: "TaskPatch.ChangeSettled", Since: 11, Path: []string{"mutation", "change_settled"}},
	{Name: "Comment.Withdrawal", Since: 12, Op: string(OpPatch), Path: []string{"mutation", "comment", "withdrawal"}},
	{Name: "MutationRecord.WorkingFor", Since: WorkingForVersion, Path: []string{"working_for"}},
	{Name: "Comment.Act", Since: 14, Op: string(OpPatch), Path: []string{"mutation", "comment", "act"}},
	{Name: "TaskPatch.DeadWait", Since: 15, Op: string(OpPatch), Path: []string{"mutation", "dead_wait"}},
	{Name: "TaskPatch.DeadWaitEnded", Since: 15, Op: string(OpPatch), Path: []string{"mutation", "dead_wait_ended"}},
	{Name: "Relation.Kind atlas_entry", Since: 16, Path: []string{"mutation", "relations", "kind"},
		Equals: string(RelationEntry)},
	{Name: "View.DeletedAt", Since: 17, Op: string(OpPatch), Path: []string{"mutation", "deleted_at"}},
	{Name: "Comment.Removed", Since: 18, Op: string(OpPatch), Path: []string{"mutation", "comment", "removed"}},
	{Name: "Project.StuckAfterHours", Since: 19, Path: []string{"mutation", "stuck_after_hours"}},
	{Name: "Subject.Kind flow", Since: FlowRecordVersion, Path: []string{"subject", "kind"},
		Equals: string(KindFlow)},
	{Name: "Snapshot.Flow", Since: FlowRecordVersion, Path: []string{"notify", "snapshot", "flow"}},
	{Name: "FlowRecord.Past", Since: FlowPastVersion, Op: string(OpFlow),
		Path: []string{"mutation", "past"}},
	{Name: "Snapshot.ByOwner", Since: OwnershipVersion, Path: []string{"notify", "snapshot", "by_owner"}},
	{Name: "Snapshot.LeftUnassigned", Since: OwnershipVersion,
		Path: []string{"notify", "snapshot", "left_unassigned"}},
	{Name: "Snapshot.DepartureLead", Since: OwnershipVersion,
		Path: []string{"notify", "snapshot", "departure_lead"}},
}

// FlowRecordVersion is the version from which a record carries an item's flow
// (ADR-0162): the `flow` kind, and the notice of it a [Snapshot] names.
const FlowRecordVersion = 20

// FlowPastVersion is the version from which a flow record derived from an
// item's past carries what it read and the instant its past gives
// ([FlowRecord.Past]), which the applier dates the row by.
const FlowPastVersion = 21

// OwnershipVersion is the version from which a record says who owns the work
// once its change is made ([Snapshot.ByOwner], [Snapshot.LeftUnassigned]) and
// whom a departure tells ([Snapshot.DepartureLead]), which decide the lead's
// notice rows; every older record routes the lead as before.
const OwnershipVersion = 22

// VersionedFields is the table, for the conformance suite and for an operator
// surface that names why a record was held back.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// minimumRecordVersion is the lowest version an encoded record may be stamped
// at, under a field table.
func minimumRecordVersion(fields statelog.RecordFields, op OpKind, encoded []byte) (int, error) {
	return fields.Minimum(string(op), encoded)
}

// DocumentVersion is the object shape this build writes.
//
// SEPARATE FROM THE RECORD VERSION, because they move for different reasons: a
// record version is a change to the wire format every node reads, and a
// document version is a change to what an object IS. Collapsing them would
// make a new field on a task look like a new protocol.
const DocumentVersion = 1

// OpKind is what a record does.
type OpKind string

const (
	// OpCreate mints an object at an expectation of zero.
	OpCreate OpKind = "create"

	// OpPatch changes one, arbitrated against its own last sequence.
	OpPatch OpKind = "patch"

	// OpTombstone hides an object; OpRestore clears the tombstone at any
	// age. Neither deletes anything, and there is no horizon behind
	// either.
	OpTombstone OpKind = "tombstone"
	// OpRestore clears a tombstone.
	OpRestore OpKind = "restore"

	// OpPurge is the one operation that removes rows, and the second kind
	// that INSTALLS A GATE.
	OpPurge OpKind = "purge"

	// OpTurn records a turn's spend. ADDITIVE: no expectation, no version
	// bump, and an apply whose running totals are gated on its own id insert
	// affecting a row.
	OpTurn OpKind = "turn"

	// OpFlow records an item's flow or a notice of it. ADDITIVE as a turn
	// is, its apply gated on its derived key's insert ([Applier.applyFlow]).
	OpFlow OpKind = "flow"

	// OpGeneration is a reanchor's record.
	OpGeneration OpKind = "generation"

	// OpEviction is a node's eviction or readmission.
	OpEviction OpKind = "eviction"

	// OpBarrier is the read index's append, which writes nothing anywhere.
	OpBarrier OpKind = "barrier"

	// OpTrimmed is the trim's record that removes the anchors below a
	// floor (ADR-0080), which writes nothing of this domain's anywhere.
	OpTrimmed OpKind = "trimmed"
)

// OpKinds are the eleven, in the order they are documented.
var OpKinds = []OpKind{
	OpCreate, OpPatch, OpTombstone, OpRestore, OpPurge,
	OpTurn, OpFlow, OpGeneration, OpEviction, OpBarrier, OpTrimmed,
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// AuthorKind is who wrote something.
//
// `operator` is a TOKEN acting on the company's behalf and is never a seat
// handle: a tracker whose author field is chosen by the writer is not an audit
// trail, so the two are different kinds rather than two spellings of one.
type AuthorKind string

const (
	// AuthorAgent is a seat, acting inside a turn.
	AuthorAgent AuthorKind = "agent"
	// AuthorHuman is a person, through the dashboard or a chat surface.
	AuthorHuman AuthorKind = "human"
	// AuthorOperator is an API token, recorded under its own label.
	AuthorOperator AuthorKind = "operator"
	// AuthorSystem is the engine itself — a duty's repair, a chart apply.
	AuthorSystem AuthorKind = "system"
)

// AuthorKinds are the four.
var AuthorKinds = []AuthorKind{
	AuthorAgent, AuthorHuman, AuthorOperator, AuthorSystem,
}

// Valid reports whether an author kind off the wire is one this build knows.
func (a AuthorKind) Valid() bool { return slices.Contains(AuthorKinds, a) }

// Person reports an actor acting through a PERSON's own credential rather than
// as a seat: somebody at the dashboard, or the token they run the company
// through. It is the one predicate every authority gate asks, so no two gates
// can disagree on who is a person, and it sits on the closed set so a fifth
// kind is decided here.
//
// `doclet`'s `Actor.IsHuman` is a different closed set answering who a
// NOTIFICATION treats as a person; folding the two would make one name mean both.
func (a AuthorKind) Person() bool {
	return a == AuthorHuman || a == AuthorOperator
}

// TermKind is what a scope term names.
type TermKind string

const (
	// TermObject is one object by id, WITHIN its container. The id is the
	// uuid alone and never the kind: a turn commit about a task and that
	// task's own create must collide, and they only do if both name the
	// object rather than their own kind of record.
	TermObject TermKind = "object"

	// TermKey is an addressable NAME — "ENG-142" — because the key
	// directory is keyed on the key rather than on any object's id: the
	// alias claim, and every create, re-key and purge that claims, frees or
	// hands on a key, is filed under it (see [keyTerm]).
	TermKey TermKind = "key"

	// TermContainer is a project key, or the workspace.
	TermContainer TermKind = "container"

	// TermFamily is a set of objects with no container: "person",
	// "catalogue", "roster".
	TermFamily TermKind = "family"

	// TermDomain is everything on this domain's log. The fallback for an
	// unknown term kind, and the honest reading of an absent scope.
	TermDomain TermKind = "domain"
)

// TermKinds are the five.
var TermKinds = []TermKind{
	TermObject, TermKey, TermContainer, TermFamily, TermDomain,
}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// WorkspaceContainer is the container every object that is not a project's
// lives in.
//
// A REAL CONTAINER rather than an empty one, so "everything in the workspace"
// is a term a writer can state and a probe can match, and so no path in the
// alphabet has a hole in the middle of it.
const WorkspaceContainer = "workspace"

// The scope path alphabet.
//
// The paths NEST although the term kinds look flat: the framework computes only
// CONTAINMENT from a path, and a record deferred on a project must block a
// write to a task in it while one deferred on a task must not block its
// neighbour. Written flat ("container/ENG" beside "object/<uuid>") the first
// blocks nothing, and naming the container beside every object would make one
// undecodable task record block its whole project. So an object's path sits
// UNDER its container, which is why [ScopeTerm] carries one, and holds no KIND:
// a turn commit and its task's create both name o/<uuid>, and must collide.
const (
	pathDomain    = "t"
	pathContainer = "c"
	pathFamily    = "f"
	pathObject    = "o"
	pathKey       = "k"
)

// ScopeTerm is one element of a record's blast radius.
//
// DATA, NOT CODE: a build that has never heard of the op still reads every
// term, because a term names an object, a key, a container, a family or the
// domain and nothing about the operation that produced it.
type ScopeTerm struct {
	Kind TermKind `json:"k"`

	// Container is the project key an object or a key lives in, or
	// [WorkspaceContainer]. Required for TermObject and TermKey, empty
	// for the other three, and never guessed: see the alphabet above.
	Container string `json:"c,omitempty"`

	// ID is the object's uuid, the key's name, the container's key or the
	// family's name. Empty for TermDomain, which names everything.
	ID string `json:"i,omitempty"`
}

// Path renders the term as a scope path the framework can order.
func (t ScopeTerm) Path() string {
	switch t.Kind {
	case TermObject:
		return statelog.ScopePath(pathDomain, pathContainer, t.container(), pathObject, t.ID)
	case TermKey:
		return statelog.ScopePath(pathDomain, pathContainer, t.container(), pathKey, t.ID)
	case TermContainer:
		return statelog.ScopePath(pathDomain, pathContainer, t.named())
	case TermFamily:
		if t.ID == "" {
			return pathDomain
		}
		return statelog.ScopePath(pathDomain, pathFamily, t.ID)
	case TermDomain:
		return pathDomain
	}
	// AN UNKNOWN TERM KIND IS THE DOMAIN, NOT A SKIPPED TERM: it claims a
	// blast radius this build cannot bound, and dropping it would narrow a
	// newer peer's scope to what this build recognises.
	return pathDomain
}

// container is the term's container, defaulting to the workspace.
func (t ScopeTerm) container() string {
	if t.Container == "" {
		return WorkspaceContainer
	}
	return t.Container
}

// named is the container a CONTAINER term names: its ID, not its Container
// field, which would resolve every container term to the workspace and widen a
// project-wide deferral to the whole company. One with no ID names its field's.
func (t ScopeTerm) named() string {
	if t.ID == "" {
		return t.container()
	}
	return t.ID
}

// MaxScopeTerms is the cap on a record's declared scope: this design's
// universal fan-out batch (64 descendants per move batch, children per merge
// batch, dependents, watchers, a caller's moves per rank order). A writer whose
// affected set is larger states the smallest COVERING container or family term
// ([ScopeSet.capped]), so the field is bounded by construction; 64 ids on
// every loud record would cost ≈ 1 068 MB/yr, +31 % of the whole log.
const MaxScopeTerms = 64

// ScopeSet is the COMPLETE set of objects a record's apply may write, stated
// by the WRITER and readable at every version.
//
// NEVER EMPTY: the exactly-the-subject case encodes as a one-byte sentinel, so
// the field cannot be absent by accident. An absent or unreadable scope is the
// DOMAIN term, never "just the subject": assuming a V1 convention holds for a
// V2 operation is the bug this field closes.
type ScopeSet struct {
	// Subject is true for the sentinel: this record touches exactly the
	// object its subject names, and nothing else.
	Subject bool

	// Container is the project the subject lives in, riding the sentinel;
	// empty is [WorkspaceContainer].
	//
	// ON THE RECORD, not derived from the subject: a subject names an object,
	// not its home, and a task can move between projects, so only the writer
	// knows it when the record is written. Left out, every task record would
	// file its deferral under the workspace while project-scoped reads probe
	// their own, and the containment probe, where a wrong clause is data loss,
	// would never match.
	Container string

	// Terms is the enumeration, when the record touches more than its own
	// subject. A container rides the sentinel and never an enumeration:
	// every term already states its own.
	Terms []ScopeTerm
}

// ScopeSentinel is the one-byte encoding of "exactly the subject".
const ScopeSentinel = "s"

// MarshalJSON encodes the sentinel as a bare string and everything else as an
// array, so the common case is one byte on the wire.
//
// A container is appended to the sentinel with the path separator — "s/ENG" —
// which keeps the workspace case at one byte and costs a project key on the
// records that have one. The separator cannot appear inside a container: a
// project key is a slug, checked by [ValidSlug].
func (s ScopeSet) MarshalJSON() ([]byte, error) {
	if s.Subject || len(s.Terms) == 0 {
		if s.Container != "" {
			return json.Marshal(ScopeSentinel + statelog.ScopeSeparator + s.Container)
		}
		return json.Marshal(ScopeSentinel)
	}
	return json.Marshal(s.Terms)
}

// UnmarshalJSON accepts both encodings, and reads anything else as the domain.
//
// A SCOPE THAT DOES NOT DECODE IS THE WIDEST TERM, never an error and never an
// empty set. This runs on a node reading a record a newer build wrote: the
// only honest reading of a blast radius it cannot parse is "everything", and
// returning an error here would take the whole two-pass decode down with it.
func (s *ScopeSet) UnmarshalJSON(b []byte) error {
	var sentinel string
	if err := json.Unmarshal(b, &sentinel); err == nil {
		head, container, _ := strings.Cut(sentinel, statelog.ScopeSeparator)
		if head != ScopeSentinel {
			*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
			return nil
		}
		*s = ScopeSet{Subject: true, Container: container}
		return nil
	}
	var terms []ScopeTerm
	if err := json.Unmarshal(b, &terms); err != nil || len(terms) == 0 {
		// Widened for the reason above: the domain term defers against
		// everything, slow and correct where a refused record is neither.
		*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
		//nolint:nilerr // Deliberate: the widened scope IS the answer.
		return nil
	}
	*s = ScopeSet{Terms: terms}
	return nil
}

// Resolve renders the scope as the framework's own path set.
//
// It takes the subject alone, the container coming from the record itself, so
// the write path and the decode path cannot resolve one record to two paths.
func (s ScopeSet) Resolve(subject Subject) statelog.ScopeSet {
	if s.Subject || len(s.Terms) == 0 {
		return statelog.ScopeSet{Paths: []string{subjectPath(subject, s.Container)}}
	}
	paths := make([]string, 0, len(s.Terms))
	for _, t := range s.Terms {
		paths = append(paths, t.Path())
	}
	return statelog.ScopeSet{Paths: paths}.Normalised()
}

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject, container string) string {
	switch s.Kind {
	case KindProject, KindCounter, KindTags, KindRankOrder:
		// THE FOUR PROJECT-SCOPED OBJECTS whose id IS a container key.
		// Their own path is the container, which is what makes a
		// deferred settings edit block every write inside that project
		// — which is exactly what an archive is.
		return ScopeTerm{Kind: TermContainer, ID: s.ID}.Path()
	case KindAlias:
		// FILED UNDER THE KEY, with every other record that claims,
		// frees or hands it on — see [keyTerm].
		key, _, _ := strings.Cut(s.ID, ".")
		return keyTerm(key).Path()
	case KindCatalogue:
		return ScopeTerm{Kind: TermFamily, ID: string(KindCatalogue)}.Path()
	case KindRoster:
		return ScopeTerm{Kind: TermFamily, ID: string(KindRoster)}.Path()
	case KindPerson:
		return ScopeTerm{Kind: TermFamily, ID: string(KindPerson)}.Path()
	case KindBarrier, KindTrimmed:
		// THE FRAMEWORK'S OWN SCOPE, which intersects no read's closure:
		// neither writes a row of this domain's, so a wider scope would make
		// reads wait on a record that changed none of their rows (for a
		// barrier, every linearizable read behind every other).
		return statelog.FrameworkScope
	case KindGeneration, KindEviction:
		// A GATE IS ABOUT THE WHOLE DOMAIN. It licenses or drops
		// records on every subject, so anything narrower would be a
		// claim the record does not make.
		return pathDomain
	case KindFile:
		// THE PROJECT IS IN THE SUBJECT, so a file's path is computed
		// from it alone — under its project's container, which is what
		// makes an archive block a write into it — and the token is the
		// object. A token is lower-case hex and a task's id is a uuid,
		// so the two never name one path.
		project, token, ok := SplitFileID(s.ID)
		if !ok {
			return pathDomain
		}
		return ScopeTerm{Kind: TermObject, Container: project, ID: token}.Path()
	default:
		return ScopeTerm{Kind: TermObject, Container: container, ID: s.ID}.Path()
	}
}

// Validate refuses a scope a writer could not have meant.
func (s ScopeSet) Validate() error {
	if s.Subject {
		if len(s.Terms) != 0 {
			return fmt.Errorf("worklet: a scope carries the exactly-the-subject "+
				"sentinel and %d enumerated term(s): the two say different "+
				"things about the same record", len(s.Terms))
		}
		if s.Container != "" {
			// The sentinel encoding splits on the separator and the path
			// join drops a blank segment, so either decodes as a DIFFERENT
			// container and files the deferral where no probe looks: refused
			// where accepted rather than trusted where split.
			switch {
			case strings.Contains(s.Container, statelog.ScopeSeparator):
				return fmt.Errorf("worklet: container %q contains %q, which is "+
					"the path separator the sentinel encoding splits on — it "+
					"would decode as %q and file this record where no probe "+
					"for it looks", s.Container, statelog.ScopeSeparator,
					strings.SplitN(s.Container, statelog.ScopeSeparator, 2)[0])
			case strings.TrimSpace(s.Container) == "":
				return fmt.Errorf("worklet: container %q is blank, and a blank "+
					"segment is dropped from the path — the record would "+
					"resolve to its container's own path rather than to its "+
					"own", s.Container)
			}
		}
		return nil
	}
	if s.Container != "" {
		return fmt.Errorf("worklet: a scope enumerates %d term(s) and also "+
			"names container %q: every term states its own container, so the "+
			"field says nothing an enumeration has not already said",
			len(s.Terms), s.Container)
	}
	if len(s.Terms) == 0 {
		return fmt.Errorf("worklet: a scope is empty; a record that touches " +
			"nothing writes nothing, and an absent scope is read as the whole " +
			"domain rather than as the subject")
	}
	if len(s.Terms) > MaxScopeTerms {
		return fmt.Errorf("worklet: a scope enumerates %d terms and the cap is "+
			"%d — a writer whose affected set is larger states the smallest "+
			"covering container or family term instead, so the field stays "+
			"bounded by construction", len(s.Terms), MaxScopeTerms)
	}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermObject, TermKey:
			if t.ID == "" {
				return fmt.Errorf("worklet: a %s term names nothing", t.Kind)
			}
		case TermContainer, TermFamily:
			if t.ID == "" {
				return fmt.Errorf("worklet: a %s term names nothing", t.Kind)
			}
		case TermDomain:
		default:
			return fmt.Errorf("worklet: scope term kind %q is not one this build "+
				"writes; it is READ as the whole domain, which is safe, and "+
				"WRITING one is a writer that cannot say what it touched", t.Kind)
		}
	}
	return nil
}

// capped is this scope held to [MaxScopeTerms] by the cap's own rule: a writer
// whose affected set is larger states the smallest COVERING term instead.
//
// THE LARGEST CONTAINERS FOLD FIRST, the name breaking a tie, each into its
// container term until the enumeration fits, so one set always states one
// scope; one spread over more containers than the cap is the DOMAIN. A term
// under a container named whole, or named twice, is dropped; a sentinel is
// returned as it is. Every widened enumeration passes through it, since each
// widening is bounded alone and their sum is not: a task's 64 dependents plus
// its own term would fail [ScopeSet.Validate] inside every decide.
func (s ScopeSet) capped() ScopeSet {
	if s.Subject || len(s.Terms) == 0 {
		return s
	}
	domain := ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
	whole := map[string]bool{}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermDomain:
			return domain
		case TermContainer:
			whole[t.named()] = true
		case TermObject, TermKey, TermFamily:
		default:
			// A KIND THIS BUILD DOES NOT WRITE is read as the domain
			// ([ScopeTerm.Path]), so folding it is folding the domain.
			return domain
		}
	}
	seen := map[string]bool{}
	fixed := []ScopeTerm{}
	byContainer := map[string][]ScopeTerm{}
	kept := make([]ScopeTerm, 0, len(s.Terms))
	for _, t := range s.Terms {
		path := t.Path()
		if seen[path] {
			continue
		}
		seen[path] = true
		switch t.Kind {
		case TermContainer, TermFamily:
			fixed = append(fixed, t)
		default:
			if whole[t.container()] {
				continue
			}
			byContainer[t.container()] = append(byContainer[t.container()], t)
		}
		kept = append(kept, t)
	}
	if len(kept) <= MaxScopeTerms {
		return ScopeSet{Terms: kept}
	}
	containers := make([]string, 0, len(byContainer))
	for c := range byContainer {
		containers = append(containers, c)
	}
	if len(fixed)+len(containers) > MaxScopeTerms {
		return domain
	}
	slices.SortFunc(containers, func(a, b string) int {
		if c := cmp.Compare(len(byContainer[b]), len(byContainer[a])); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	count := len(kept)
	for _, c := range containers {
		if count <= MaxScopeTerms {
			break
		}
		count -= len(byContainer[c]) - 1
		fixed = append(fixed, ScopeTerm{Kind: TermContainer, ID: c})
		delete(byContainer, c)
	}
	terms := fixed
	for _, c := range containers {
		terms = append(terms, byContainer[c]...)
	}
	slices.SortFunc(terms, func(a, b ScopeTerm) int { return cmp.Compare(a.Path(), b.Path()) })
	return ScopeSet{Terms: terms}
}

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted.
//
// Its EIGHT keys are RESERVED at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one. It is
// the same split the event envelope makes between a known header and an opaque
// body, for the same reason — a rolling upgrade puts a record this build
// cannot read on the wire, and dropping it would make every upgrade an outage.
type RecordEnvelope struct {
	// V is the record version. Refused for the PAYLOAD when unknown,
	// never for this struct.
	V int `json:"v"`

	// OpID is a uuid7: the idempotency key, the Nats-Msg-Id, and the ops
	// table's key. EMPTY on a barrier, deliberately — an op id is what
	// invites a message id, and a duplicate ack is served out of the
	// dedupe window with no quorum round trip at all, which is the one
	// thing a read barrier must never be.
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record mutates, and the unit the broker
	// arbitrates it on.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is THE AUTHORED INSTANT, the writer's own clock. Reported,
	// never ORDERED ON, and differenced against exactly one thing — the
	// broker's stored instant, to publish a skew. Empty on a barrier,
	// because nothing renders one.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation. It is what makes an object's version a pure
	// function of the record — (Gen << 40) | the broker's own sequence —
	// so the applier needs no table lookup, no config read and no clock.
	// Present on a barrier too, because a barrier from a previous
	// generation must be refusable.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node's id, read by the eviction gate —
	// which runs before any kind rule, on a node that may not be able to
	// decode the payload at all. Empty on a barrier, which writes no rows
	// for a gate to drop.
	Writer string `json:"writer,omitempty"`

	// Scope is the complete set of objects this record's apply may write.
	Scope ScopeSet `json:"scope"`
}

// InstallsGate reports whether this record installs an apply gate.
//
// ANSWERED FROM THE ENVELOPE ALONE, because it must be answerable by a node
// that cannot decode the payload: it is what turns an unknown version into a
// STOP rather than a deferral.
//
// TWO CONDITIONS, NOT ONE. An eviction is a gate by its KIND; a purge is a
// gate by its OP, on an ordinary task subject. Reading only the kind would let
// a purge this build cannot decode be deferred — and a deferred purge is a
// node that goes on serving rows every other node has removed, with no inverse
// that repairs it.
func (e RecordEnvelope) InstallsGate() bool {
	return e.Subject.Kind.InstallsGate() || e.Op == OpPurge
}

// readByEveryBuild reports a record every build must read, each pinned at a
// version every build reads: a gate ([RecordEnvelope.InstallsGate]), the read
// index's barrier, the trim's trimmed record and a reanchor's generation.
//
// An older node cannot defer a gate (it would license every record above it,
// irreparably), so it halts there; a barrier it retained defers every
// linearizable read, a trimmed record keeps every anchor it names, and a
// generation is a transition never made. So none may carry a versioned field
// ([MutationRecord.Encode] refuses one), save a gate's own changed APPLY
// ([statelog.VersionedField.Gate]), where halting is the only non-divergence.
func (e RecordEnvelope) readByEveryBuild() bool {
	return e.InstallsGate() || e.Op == OpBarrier || e.Op == OpTrimmed || e.Op == OpGeneration
}

// MutationRecord is one committed mutation: the envelope plus everything a
// build at this version may read.
type MutationRecord struct {
	RecordEnvelope

	// Expect is the version this mutation was decided against. PROVENANCE
	// ONLY — the broker is what enforced it, and nothing reads this to
	// decide anything.
	Expect uint64 `json:"expect,omitempty"`

	// Mutation is the typed payload for (Subject.Kind, Op), left as
	// OPAQUE BYTES when the version is above this build's.
	Mutation json.RawMessage `json:"mutation,omitempty"`

	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`

	// ActorApps are the apps the credential behind an [AuthorOperator] record
	// used, as the node that took the write resolved them (iam.ActorApps), on
	// every record a later turn reads: an item's creation, every move of its
	// assignee, and any other record whose wake may start an agent's turn
	// ([Writer.carries]); absent otherwise, a gate always. The change feed and
	// the work pickup read it, and a turn the record wakes or hands work to
	// uses no app outside it (ADR-0109, ADR-0112, notify.FirstPartyWriter).
	// Version 2: an older build retains such a record rather than wake a seat
	// for a credential it would read as a stranger.
	ActorApps []iam.Grant `json:"actor_apps,omitempty"`

	// ActorAppsKept marks a record whose [MutationRecord.ActorApps] its
	// history row keeps (`tracker_history.actor_apps`, ADR-0112). Version 4,
	// so a record older than the rule keeps an empty column on every node.
	ActorAppsKept bool `json:"actor_apps_kept,omitempty"`

	// WorkingFor is whom an [AuthorAgent] record a later turn reads was
	// written for (ADR-0109): whoever the writing turn worked for, composed
	// from its bound ceiling, on the records [MutationRecord.ActorApps] rides;
	// absent on an agent's write made for nobody else, every other author's,
	// and any no later turn reads (a gate always). The change feed reads it,
	// and the turn the record wakes works for them (notify.FirstPartyWriter),
	// never for the writing seat, which holds every app. KEPT ON THE HISTORY
	// ROW (`tracker_history.working_for`, [WorkingForVersion]) for the work
	// pickup. Version 13: an older build retains such a record rather than
	// wake a seat for the writing agent.
	WorkingFor *types.WorkingFor `json:"working_for,omitempty"`

	// NoticeAsksKept marks a record whose comment author's statement its notice
	// rows keep (`tracker_notifications.asks`). Version 6, so a record older than
	// the rule leaves the column NULL on every node.
	NoticeAsksKept bool `json:"notice_asks_kept,omitempty"`

	// NoticeWokeKept marks a record that tells somebody it does not wake, whose
	// notice rows keep whether each recipient was woken
	// (`tracker_notifications.starts_turn`). Version 7, so a record older than
	// the rule reads as waking every recipient on every node.
	NoticeWokeKept bool `json:"notice_woke_kept,omitempty"`

	TurnID string   `json:"turn_id,omitempty"`
	Chain  []string `json:"chain,omitempty"`

	// Kind is WHAT THIS RECORD DID, stated by the writer, and a different fact
	// from whether anybody was told. Not read off the Notify: the history
	// row's `kind` is what the feed's `kinds=` filter, the report windows and
	// the unblocked repair select on, and the feed is a complete account of
	// what HAPPENED rather than of what was ANNOUNCED (`notified` tells the
	// two apart).
	//
	// REQUIRED exactly where [ObjectKind.RecordsHistory] is true and refused
	// everywhere else (a barrier, a turn or a rank order writes no row);
	// [Writer.decide] enforces both, and the applier stores it verbatim.
	Kind ChangeKind `json:"kind,omitempty"`

	// Notify is the routing snapshot; NIL means the commit wakes nobody, and
	// nothing else (see [Notify]). When it and [MutationRecord.Kind] are both
	// present they must agree, which [Writer.decide] checks.
	Notify *Notify `json:"notify,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// What is NOT in the envelope, deliberately: the BROKER'S instant. Every
// duration is measured against the broker's store timestamp, which the
// replication loop hands the applier as an argument; a field a writer could set
// is one it could lie about, so nobody "completes" the envelope with it.

// DecodeEnvelope is the FIRST pass, and it never fails on version.
//
// Every branch that makes an un-decodable record survivable turns on something
// here: the subject it is filed under, the scope a writer probes for, the kind
// a gate reads, and the version that decides whether there is a second pass at
// all.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("worklet: decode the record "+
			"envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("worklet: a record carries version "+
			"%d — every record states its version, and one that does not "+
			"cannot be told apart from a newer build's", env.V)
	}
	if err := env.Subject.Validate(); err != nil {
		return RecordEnvelope{}, err
	}
	return env, nil
}

// Decode is the SECOND pass, and it is the one that may refuse on version.
//
// It returns the envelope alongside the error whenever the envelope itself
// decoded, because that is precisely the case the applier retains: the caller
// needs the subject, the scope and the kind of a record it cannot read.
func Decode(payload []byte) (MutationRecord, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return MutationRecord{}, err
	}
	if env.V > RecordVersion {
		return MutationRecord{RecordEnvelope: env}, &ErrFutureVersion{
			Got: env.V, Want: RecordVersion, Subject: env.Subject,
		}
	}
	var rec MutationRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("worklet: decode "+
			"the record on %s: %w", env.Subject, err)
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(payload, &extra); err == nil {
		for _, known := range knownKeys {
			delete(extra, known)
		}
		if len(extra) > 0 {
			rec.Extra = extra
		}
	}
	return rec, nil
}

// knownKeys are the top-level keys this build writes, so Extra holds exactly
// what it does not. LISTED RATHER THAN REFLECTED, because the list is the
// format's own reserved set; TestTheRecordsOwnKeysAreExactlyTheKnownSet holds
// it to the struct in both directions.
var knownKeys = []string{
	"v", "op_id", "subject", "op", "created_at", "gen", "writer", "scope",
	"expect", "mutation", "actor", "actor_kind", "actor_apps", "actor_apps_kept",
	"notice_asks_kept", "notice_woke_kept", "working_for", "operator_id", "turn_id", "chain", "kind",
	"notify",
}

// ErrFutureVersion reports a record a newer build wrote.
type ErrFutureVersion struct {
	Got, Want int
	Subject   Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("worklet: the record on %s is version %d and this build "+
		"reads %d — it is RETAINED at its position rather than skipped, so a "+
		"build that can read it applies it later", e.Subject, e.Got, e.Want)
}

// Encode writes a record, stamping it with the lowest version that reads it.
//
// A ZERO VERSION IS "STAMP IT": this computes [minimumRecordVersion] over the
// bytes it publishes, so the stamp cannot drift from the content. A version the
// caller set is kept (a relay's, a gate's [GateRecordVersion], a barrier's or
// generation's [baseRecordVersion]) but refused below what the record's fields
// need, since the builds the stamp holds back would apply it lossily. A record
// every build must read ([RecordEnvelope.readByEveryBuild]) is refused carrying
// any versioned field, save [statelog.VersionedField.Gate]. The scope is
// validated here, not at the applier: an under-stated scope is a writer's
// mistake the applier has nothing to compare against.
func (r MutationRecord) Encode() ([]byte, error) { return r.encodeWith(versionedFields) }

// encodeWith is [MutationRecord.Encode] under a named field table — the seam
// the stamping rule is tested through with fields a later build would add,
// independent of whichever rows the production table holds today.
func (r MutationRecord) encodeWith(fields statelog.RecordFields) ([]byte, error) {
	stamp := r.V == 0
	if stamp {
		// THE LOWEST, never [RecordVersion], which would have every older
		// node retain it; the table raises it to what the fields need.
		r.V = baseRecordVersion
	}
	if err := r.Subject.Validate(); err != nil {
		return nil, err
	}
	if !r.Op.Valid() {
		return nil, fmt.Errorf("worklet: op %q is not one this build writes", r.Op)
	}
	if err := r.Scope.Validate(); err != nil {
		return nil, err
	}
	body, err := r.marshal()
	if err != nil {
		return nil, err
	}
	minimum, err := minimumRecordVersion(fields, r.Op, body)
	if err != nil {
		return nil, fmt.Errorf("worklet: stamp the record on %s: %w", r.Subject, err)
	}
	refused := fields.Carried(string(r.Op), body)
	if r.InstallsGate() {
		refused = fields.Ungated(string(r.Op), body)
	}
	switch {
	case r.readByEveryBuild() && minimum > baseRecordVersion && len(refused) > 0:
		// See [RecordEnvelope.readByEveryBuild]: anything else a gate needs
		// a newer reader for would halt every older node over a field rather
		// than a rule it cannot honour, and a barrier or a generation would
		// be retained.
		return nil, fmt.Errorf("worklet: the %s record on %s must be readable "+
			"by every build for ever — an older node halts at a gate, and retains "+
			"a barrier or a generation, that it cannot read — and it carries "+
			"%s, so a field it needs belongs somewhere else",
			r.Op, r.Subject, strings.Join(refused, ", "))
	case r.V >= minimum:
		// The common case, and the only one that marshals once: a record
		// carrying nothing newer than its stamp.
	case stamp:
		r.V = minimum
		if body, err = r.marshal(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("worklet: the %s record on %s is stamped version "+
			"%d and carries %s — a build reading %d would decode it, drop what it "+
			"has no field for and apply the rest; leave the version unset and "+
			"the encoder stamps the lowest one that reads it",
			r.Op, r.Subject, r.V, strings.Join(fields.Carried(string(r.Op), body), ", "), r.V)
	}
	return body, nil
}

// marshal renders the record with whatever a newer build wrote carried back.
func (r MutationRecord) marshal() ([]byte, error) {
	body, err := json.Marshal(r)
	if err != nil || len(r.Extra) == 0 {
		return body, err
	}

	// A RECORD FROM A NEWER BUILD RE-ENCODES WITH ITS UNKNOWN FIELDS: without
	// [Extra] put back, a relayed record loses what this build has no field
	// for, and a newer node's change-feed parser sees only what the relay
	// understood. Taken ONLY when there is something to add, so a record this
	// build wrote keeps the struct's own bytes: the barrier's literal stays
	// byte-identical, and its size is what the read index's cost model rests on.
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(body, &merged); err != nil {
		return nil, fmt.Errorf("worklet: re-encode a record carrying %d "+
			"field(s) this build does not know: %w", len(r.Extra), err)
	}
	for key, value := range r.Extra {
		// A KNOWN KEY WINS. Extra should never hold one — the decoder
		// only files what is not in the struct — and if it somehow does,
		// this build's own value is the one it can be held to.
		if _, taken := merged[key]; !taken {
			merged[key] = value
		}
	}
	return json.Marshal(merged)
}

// ChangeKind is what happened, as a recipient needs to be told it.
//
// THE SPLIT IS BY WHAT A RECIPIENT NEEDS, not by which column moved: a card
// saying "the status changed" and a card saying "you were assigned" are two
// different messages about one write. Every kind has exactly ONE writer and no
// kind has variants.
type ChangeKind string

// Each constant IS its wire value: a kind is written into
// every history row and onto every notification the log carries, so these
// spellings are stored data in every company already running this build.
// A kind added here goes into [ChangeKinds] in the same change: that slice is
// what [ChangeKind.Valid] answers from and what every walk over the set reads,
// so a constant missing from it is a kind the wire carries and nothing knows.
const (
	ChangeCreated         ChangeKind = "created"
	ChangeFields          ChangeKind = "fields"
	ChangeStatus          ChangeKind = "status"
	ChangeAssignee        ChangeKind = "assignee"
	ChangeCollaborators   ChangeKind = "collaborators"
	ChangeWatchers        ChangeKind = "watchers"
	ChangeTags            ChangeKind = "tags"
	ChangeRelations       ChangeKind = "relations"
	ChangeRouted          ChangeKind = "routed"
	ChangeMoved           ChangeKind = "moved"
	ChangeReparented      ChangeKind = "reparented"
	ChangeChecklist       ChangeKind = "checklist"
	ChangeArchived        ChangeKind = "archived"
	ChangeComment         ChangeKind = "comment"
	ChangeCommentEdited   ChangeKind = "comment_edited"
	ChangeCommentResolved ChangeKind = "comment_resolved"
	ChangeCommentRemoved  ChangeKind = "comment_removed"
	ChangeRemoved         ChangeKind = "removed"
	ChangeRestored        ChangeKind = "restored"
	ChangePurged          ChangeKind = "purged"
	ChangeProjectCreated  ChangeKind = "project_created"
	ChangeProjectUpdated  ChangeKind = "project_updated"
	ChangePolicyChanged   ChangeKind = "policy_changed"
	ChangeViewSaved       ChangeKind = "view_saved"
	// ChangeViewDeleted is a saved view taken out of its strip: a kind of
	// its own for [ChangeFileRemoved]'s reason, since "a view was saved"
	// and "a view is gone" are two lines in the company's account of itself.
	ChangeViewDeleted ChangeKind = "view_deleted"
	ChangeCatalogue   ChangeKind = "catalogue_updated"
	ChangePrioritised ChangeKind = "prioritised"

	// ChangePersonUpdated is every OTHER write to a person's own record — a
	// pin, an inbox mark, a snooze. Its own kind because the history row is
	// written either way, and would otherwise land under the OPERATION
	// (`patch`), which no filter can select and no card can render. NOT
	// ROUTABLE: a person's own bookkeeping wakes nobody, unlike
	// [ChangePrioritised], where somebody else reordered your day.
	ChangePersonUpdated ChangeKind = "person_updated"

	// ChangeFileWritten is a file put at its path — created, or its content
	// replaced — and ChangeFileRemoved is one taken away. Two kinds rather
	// than one with a flag, for the task's own reason: "a report appeared"
	// and "a report is gone" are two different lines in a project's
	// account of itself.
	ChangeFileWritten ChangeKind = "file_written"
	ChangeFileRemoved ChangeKind = "file_removed"

	// ChangeLinkSettled is a change (a pull request) the task waits on
	// merged or closed, written by the engine, never by an agent's turn.
	ChangeLinkSettled ChangeKind = "change_settled"

	// ChangeDeadWait records that nothing moves a change the task waits on,
	// and ChangeDeadWaitEnded that something does again; both by the engine.
	ChangeDeadWait      ChangeKind = "dead_wait"
	ChangeDeadWaitEnded ChangeKind = "dead_wait_ended"

	// ChangeFlow is a notice that an item in progress is not moving: due,
	// stuck or held (ADR-0162). Written by the flow duty on the item's flow
	// subject, never as history: recording a state moves nothing it reads.
	ChangeFlow ChangeKind = "flow"
)

// ChangeKinds are every change kind this build writes.
//
// MORE KINDS THAN SUBJECTS, and some subjects have none: five commit classes
// carry no notification at all — a turn, a generation, an eviction, a rank
// move and a barrier — because a reposition is not history and a barrier
// writes no rows whatever.
var ChangeKinds = []ChangeKind{
	ChangeCreated, ChangeFields, ChangeStatus, ChangeAssignee,
	ChangeCollaborators, ChangeWatchers, ChangeTags, ChangeRelations,
	ChangeRouted, ChangeMoved, ChangeReparented,
	ChangeChecklist, ChangeArchived, ChangeComment, ChangeCommentEdited,
	ChangeCommentResolved, ChangeCommentRemoved, ChangeRemoved,
	ChangeRestored, ChangePurged, ChangeProjectCreated, ChangeProjectUpdated,
	ChangePolicyChanged, ChangeViewSaved, ChangeViewDeleted, ChangeCatalogue,
	ChangePrioritised, ChangePersonUpdated, ChangeFileWritten, ChangeFileRemoved,
	ChangeLinkSettled, ChangeDeadWait, ChangeDeadWaitEnded, ChangeFlow,
}

// Valid reports whether a change kind off the wire is one this build knows.
func (k ChangeKind) Valid() bool { return slices.Contains(ChangeKinds, k) }

// MovedFields is the deltas a change of kind k records its move under
// ([TaskDeltas]), for the kinds that name one field of a task or one family of
// its sets, and nil for every other kind.
//
// A record carries ONE kind (update_work_item's is the assignee's, then the
// status's, then the others' only where nothing before set it), so a change of
// these kinds is found by its delta, whatever its record's kind: the pickup
// (readyAssignedArm), the router ([Notify.moved]) and a wake's prompt
// ([Notify.movedKinds]) all do. A kind naming no single field, `fields` among
// them, is found by kind; a checklist move is not always a delta, so its
// holders are told by [Snapshot.ChecklistAssignees], never by this.
func (k ChangeKind) MovedFields() []string {
	switch k {
	case ChangeStatus:
		return []string{"status"}
	case ChangeAssignee:
		return []string{"assignee"}
	case ChangeCollaborators:
		return []string{"collaborators"}
	case ChangeWatchers:
		return []string{"watchers"}
	case ChangeTags:
		return []string{"tags"}
	case ChangeRouted:
		return []string{"routing_unit"}
	case ChangeRelations:
		// EVERY LINK SET, and the mirror a blocker carries of the items
		// waiting on it ([TaskDeltas]).
		fields := make([]string, 0, len(RelationKinds)+1)
		for _, relation := range RelationKinds {
			fields = append(fields, string(relation))
		}
		return append(fields, "blocking")
	}
	return nil
}

// MaxDeltas caps what a notification DISPLAYS, and governs nothing else: a
// write touching 128 field values carries all 128 in its payload, which keeps
// the record reproducible, and the history row carries every field that moved.
// Only a card is trimmed, and it says by how many ([Notify.FieldsOmitted]).
const MaxDeltas = 32

// MaxExcerpt is how much of a body or a comment a card carries.
//
// Six hundred bytes, on the notification side only. There is no excerpt on the
// mutation side at all: a patch carries the COMPLETE new value, because a
// record that carried an excerpt could not rebuild the row.
const MaxExcerpt = 600

// Delta is one field's move, AS TEXT: a card, a person and a model all want
// "todo → in_progress", and the typed value is on the row.
//
// TWO SHAPES. A value moves From one text To another. A SET (the watchers, an
// edge kind, the tags) says what joined and what left ([Delta.Added],
// [Delta.Removed]), each sorted and each member whole, with From and To empty:
// that is always small, where both whole sides cut to a byte cap drop the
// member that moved from any large set. The whole set is on the row's document.
type Delta struct {
	From string `json:"from" untrusted:"name"`
	To   string `json:"to" untrusted:"name"`
	// Added and Removed are what a set gained and lost — see [Delta].
	Added   []string `json:"added,omitempty" untrusted:"name"`
	Removed []string `json:"removed,omitempty" untrusted:"name"`
}

// TaskParty is one task and who is on it, for the two lists that name
// somebody else's task.
type TaskParty struct {
	Task     string `json:"task"`
	Key      string `json:"key,omitempty"`
	Assignee string `json:"assignee,omitempty"`
}

// Snapshot is the routing state of a task at the moment of a change.
//
// COPIED RATHER THAN LOOKED UP, and that is what lets the node that wins a
// feed delivery route without reading anything: a node whose own replica had
// not caught up would otherwise route from a stale row, or block the feed
// until it had.
type Snapshot struct {
	Key         string      `json:"key"`
	Project     string      `json:"project"`
	Title       string      `json:"title"`
	Status      Status      `json:"status"`
	StatusGroup StatusGroup `json:"status_group,omitempty"`
	Assignee    string      `json:"assignee,omitempty"`
	Reporter    string      `json:"reporter,omitempty"`

	// Watchers is the set MINUS the muted, so the feed never has to
	// subtract and can never forget to. The MUTE ITSELF travels in the
	// mutation payload whenever the watcher collection is touched — a
	// replay that saw only this list could not tell "not a watcher" from
	// "watching but muted", and would silently re-add every unwatched
	// person on the next mention.
	Watchers      []string `json:"watchers,omitempty"`
	Collaborators []string `json:"collaborators,omitempty"`

	// ProjectLead and RoutingUnitLead are BOTH carried, because they are
	// two different fallbacks and which one applies depends on whether the
	// task has a routing unit at the moment of the change — a fact the
	// receiving node cannot reconstruct later.
	ProjectLead     string `json:"project_lead,omitempty"`
	RoutingUnitLead string `json:"routing_unit_lead,omitempty"`

	// ByOwner marks a change its author made to a task it keeps that only the
	// lead's fallback would hear: the lead is told, never woken. LeftUnassigned
	// marks a change that took the assignee away and named none: the fallback is
	// offered, and the previous assignee's notice is not its audience. Both
	// stated by the writer, which alone knows the author ([Notify.ownership]).
	ByOwner        bool `json:"by_owner,omitempty"`
	LeftUnassigned bool `json:"left_unassigned,omitempty"`

	// DepartureLead is the lead one record of a seat's departure tells of every
	// item it left unassigned in their charge, which its excerpt lists
	// ([Writer.RehomeDeparted]): ordinary, so the item's audience cannot keep it quiet.
	DepartureLead string `json:"departure_lead,omitempty"`

	// Unblocked names the dependent a late relations commit tells is now
	// workable, with its assignee. [Writer.TellUnblocked], the unblocked
	// repair, is its one writer, on the DEPENDENT's own subject; a close
	// fills nothing.
	//
	// It names NOBODY ELSE: the notice takes its place in the precedence like
	// any other reason ([Candidates]), and a record reaches one handle once,
	// its row and prompt naming the record's own task. Beside another reason
	// or task the assignee would hear that instead while the applier stamped
	// the dependent told, so the repair would never tell them again. A wake,
	// not a safeguard: nothing was refused.
	Unblocked []TaskParty `json:"unblocked,omitempty"`

	// Dependents names every dependent this change added or removed, for
	// the BLOCKER's side of a relations commit — a new dependent is a fact
	// to weigh rather than a question to answer.
	Dependents []TaskParty `json:"dependents,omitempty"`

	// PrevAssignee and PrevStatusGroup are what the row held BEFORE this
	// change.
	//
	// ON THE SNAPSHOT rather than looked up, because routing turns on the
	// TRANSITION and not on the destination: "entered or left a finished
	// group" is what wakes a reporter and a parent's assignee, and a node
	// deriving it later would be reading a row that has moved on.
	PrevAssignee    string      `json:"prev_assignee,omitempty"`
	PrevStatusGroup StatusGroup `json:"prev_status_group,omitempty"`

	// CommentAuthorKind decides whether the assignee is ADDRESSED by a
	// comment: a person's comment on your ticket is the ask, and another
	// agent's is not.
	CommentAuthorKind AuthorKind `json:"comment_author_kind,omitempty"`

	// CommentAsk is the handle a comment asked, set only at creation.
	CommentAsk string `json:"comment_ask,omitempty"`

	// CommentAsks is an agent's statement, on every comment it writes, of
	// whether the comment asks the seats it names; false makes a mention of
	// an agent a reference (ADR-0113). Nil on a person's and an older build's.
	CommentAsks *bool `json:"comment_asks,omitempty"`

	// MentionedAgents are the seats an agent's comment names that are
	// agents; a person it names is addressed whether or not it asks.
	MentionedAgents []string `json:"mentioned_agents,omitempty"`

	// AnsweredAuthor is the author of the ASKED comment.
	//
	// A SNAPSHOT FIELD RATHER THAN A LOOKUP, and the reason is that the
	// comment author on a reply is the ANSWERER: routing off that handle
	// would wake the seat that just answered and leave the person who
	// asked unwoken.
	AnsweredAuthor string `json:"answered_author,omitempty"`

	// ThreadParticipants are the people already in a comment thread, set
	// only on a reply.
	ThreadParticipants []string `json:"thread_participants,omitempty"`

	// ThreadOwing are the handles an open ask in this thread is addressed
	// to, and ThreadAwaiting the authors of those asks: a stake in the
	// thread, which any reply by somebody else wakes (ADR-0113). Set only
	// on a reply.
	ThreadOwing    []string `json:"thread_owing,omitempty"`
	ThreadAwaiting []string `json:"thread_awaiting,omitempty"`

	// RemovedWatchers are the handles a write dropped from the watchers,
	// whatever kind its record ends with ([Snapshot.moves]). They go to
	// the INBOX only: a human learns that a lead removed her watch, and an
	// agent is not woken about a decision that was not its own.
	RemovedWatchers []string `json:"removed_watchers,omitempty"`

	// ParentAssignee hears when a child enters or leaves a finished group:
	// the parent's assignee as the write's decide read it, filled only
	// across that edge ([Notify.restated]).
	ParentAssignee string `json:"parent_assignee,omitempty"`

	// ChecklistAssignees are the people whose checklist items changed.
	ChecklistAssignees []string `json:"checklist_assignees,omitempty"`

	// RoutedTo is the effective lead of a task's NEW routing unit, and it
	// is an ORDINARY candidate rather than a fallback.
	//
	// The promise is "the new unit's lead learns that work routes to them
	// now", and a fallback candidate cannot keep it: a fallback survives
	// only when no ordinary candidate did, so on any task that still has
	// an assignee, a collaborator or a watcher the new lead would be
	// dropped in silence.
	RoutedTo string `json:"routed_to,omitempty"`

	// Person is whose object a person-scoped change was about — the
	// handle whose priorities somebody else wrote.
	Person string `json:"person,omitempty"`

	// PrioritisedBy is who wrote somebody else's priority list, and
	// Position is where in it the task named by Key now sits, ONE-BASED
	// because that is how the excerpt reads it back: "put ENG-42 at
	// position 2 of your priorities".
	PrioritisedBy string `json:"prioritised_by,omitempty"`
	Position      int    `json:"position,omitempty"`

	// Task is the task this wake POINTS AT when the record's subject is not
	// one: `prioritised` is written on a person's object about the task that
	// reached the top of their list. A task-subject wake leaves it empty and
	// [Snapshot.TaskID] falls back to the subject, so the id has one carrier.
	Task string `json:"task,omitempty"`

	// Flow is what a flow notice tells ([ChangeFlow]): the one person it is
	// for and the item's state as the duty read it, which the wake's moot
	// rule compares the item with at dispatch. Nil on every other change.
	Flow *FlowNotice `json:"flow,omitempty"`

	// KeyCollision says [Snapshot.Key] opens ANOTHER task, one that claimed the
	// key first, so this wake's task is reached only by its id
	// ([Snapshot.Address]); without it a woken seat reads and answers on the
	// claimant.
	//
	// DERIVED BY THE WRITER, never a caller: [Writer.decide] reads the key
	// directory in its own snapshot ([keyHeldByAnother]) for every record
	// carrying a notification and overwrites the caller's value, a reading a
	// concurrent create can overtake (see that function). READ BY THE PARSER,
	// NEVER THE APPLIER: the inbox derives `subject_key_collision` from the
	// directory at read, so no row depends on it.
	KeyCollision bool `json:"key_collision,omitempty"`
}

// Address is the reference that opens the task this wake points at: its key,
// or its id where another task claimed the key first. See [ItemAddress].
func (s Snapshot) Address(subject Subject) string {
	return ItemAddress(s.TaskID(subject), s.Key, s.KeyCollision)
}

// TaskID is the task this wake points at, or empty when it points at none.
//
// THE SNAPSHOT'S OWN, falling back to the subject only when the subject IS a
// task. One of the two routable kinds is not a task, and handing the prompt a
// person's handle under `item_id` would render a "read this task first" block
// naming something `get_work_item` cannot resolve.
func (s Snapshot) TaskID(subject Subject) string {
	if s.Task != "" {
		return s.Task
	}
	if subject.Kind == KindTask {
		return subject.ID
	}
	return ""
}

// Notify is the routing snapshot a wake is derived from.
//
// Nil means the commit wakes nobody, and nothing else: a quiet commit is a full
// record, arbitrated like a loud one and writing its history row, so the
// activity feed accounts for what happened rather than what was announced. A
// POINTER RATHER THAN A quiet FLAG, so the wake filter asks about the record's
// own shape instead of reading a boolean a writer can forget to set.
type Notify struct {
	Kind ChangeKind `json:"kind"`

	// Fields is what this change MOVED, capped at MaxDeltas. It reaches the
	// woken seat as the wake prompt's "What changed" block: the parser renders
	// it with [changedText] and stamps it as [MetaDeltas], since the spine's
	// envelope is a string map. It is also the history row's fallback where
	// the apply found nothing to compare (`apply_history.go` prefers its own
	// comparison and reaches for this only where the two builds differ).
	Fields map[string]Delta `json:"fields,omitempty"`

	// FieldsOmitted is how many more fields this change moved than
	// [Notify.Fields] shows — the ones past [MaxDeltas] ([cardFields]).
	// SAID, because a card that dropped them silently reads as a change
	// that moved only what it lists, and the router reads it so
	// ([Notify.watchOnly]).
	FieldsOmitted int `json:"fields_omitted,omitempty"`

	CommentID string `json:"comment_id,omitempty"`

	// Excerpt is at most MaxExcerpt bytes of what a card should show, cut
	// rune-safely.
	Excerpt string `json:"excerpt,omitempty"`

	Mentions []string `json:"mentions,omitempty"`

	// Late marks a wake the repair duty issued rather than the write
	// itself — a one-sided relation whose blocker-side commit was never
	// written, so its assignee was never told. It is the only wake that
	// side gets, and saying so on the card is what stops it reading as a
	// duplicate.
	Late bool `json:"late,omitempty"`

	// Answered is the decision an answer closed — what the ASKER is woken
	// to read. Nil on every wake that answers no decision.
	Answered *AnsweredDecision `json:"answered,omitempty"`

	Snapshot Snapshot `json:"snapshot"`
}

// check bounds what an answered decision carries by the caps the decision it
// was copied from was written under, so the record cannot carry more of it
// than the ask itself could.
func (a *AnsweredDecision) check() error {
	if a == nil {
		return nil
	}
	if err := checkText("answered.question", a.Question, decision.MaxQuestion); err != nil {
		return err
	}
	if a.Choice != nil {
		if err := checkText("answered.choice.label", a.Choice.Label, decision.MaxOptionLabel); err != nil {
			return err
		}
		if err := checkText("answered.choice.detail", a.Choice.Detail, decision.MaxOptionDetail); err != nil {
			return err
		}
	}
	if a.Inform != nil {
		return checkText("answered.inform.channel", a.Inform.Channel, decision.MaxInformChannel)
	}
	return nil
}

// AnsweredDecision is what an answer to a decision tells the person who asked.
//
// ON THE NOTIFICATION rather than looked up, for the snapshot's reason: the
// question, the options' labels and the promised channel live on the ASK's
// row, which the node delivering the wake may not have applied, and the
// comment names its choice by id alone. NOT A ROUTING FACT AND NOT A ROW:
// [Candidates] never reads it and no applier stores it, so a build that does
// not know the key decodes around it with every node's rows agreeing, losing
// only the prompt's rendering of the choice, which the excerpt
// (`Chose “<label>”: …`) still carries.
type AnsweredDecision struct {
	// Question is the decision's own question, at most
	// [MaxDecisionQuestion] bytes.
	Question string `json:"question" untrusted:"name"`

	// Choice is the option the answer chose. Nil for an answer in prose,
	// which is an answer too — "none of these" is one.
	Choice *decision.Option `json:"choice,omitempty"`

	// Inform is where the asker said it would report the outcome.
	Inform *decision.Inform `json:"inform,omitempty"`
}

// Validate refuses a notification that would render wrong.
func (n *Notify) Validate() error {
	if n == nil {
		return nil
	}
	if !n.Kind.Valid() {
		return invalid("%q is not a change kind this build writes — "+
			"every kind has exactly one writer, so an unknown one is a wake "+
			"nothing renders a card for", n.Kind)
	}
	if n.FieldsOmitted < 0 {
		return invalid("a notification says %d fields were left off its card",
			n.FieldsOmitted)
	}
	if len(n.Fields) > MaxDeltas {
		return invalid("a notification carries %d deltas and a card "+
			"shows %d — the cap is on the DISPLAY, and a writer that hit it "+
			"should be trimming what it shows rather than what it recorded",
			len(n.Fields), MaxDeltas)
	}
	if len(n.Excerpt) > MaxExcerpt {
		return invalid("a notification excerpt is %d bytes against a "+
			"%d cap", len(n.Excerpt), MaxExcerpt)
	}
	if err := n.Answered.check(); err != nil {
		return err
	}
	return n.checkSnapshot()
}

// checkSnapshot bounds every collection the routing snapshot carries.
//
// AT THE PUBLISH BOUNDARY, not at each builder, because the cost lives on the
// record: a snapshot is held by each stream replica for its retention window
// and applied into every data node's rows. These caps sum to about 19 KiB
// worst, which sizes [MaxCommitBytes]. A REFUSAL, not a trim: a trim would
// silently tell fewer people than the writer named, and the bound is the one
// the object itself is held to, so a caller hitting it already has a refused
// object; the message names the field.
func (n *Notify) checkSnapshot() error {
	for _, c := range []struct {
		field string
		size  int
		max   int
	}{
		{"watchers", len(n.Snapshot.Watchers), MaxWatchers},
		{"removed_watchers", len(n.Snapshot.RemovedWatchers), MaxWatchers},
		{"collaborators", len(n.Snapshot.Collaborators), MaxCollaborators},
		{"unblocked", len(n.Snapshot.Unblocked), MaxDependents},
		{"dependents", len(n.Snapshot.Dependents), MaxDependents},
		{"thread_participants", len(n.Snapshot.ThreadParticipants), MaxThreadParticipants},
		{"thread_owing", len(n.Snapshot.ThreadOwing), MaxThreadParticipants},
		{"thread_awaiting", len(n.Snapshot.ThreadAwaiting), MaxThreadParticipants},
		{"checklist_assignees", len(n.Snapshot.ChecklistAssignees), MaxChecklists},
		{"mentions", len(n.Mentions), MaxMentions},
	} {
		if c.size > c.max {
			return invalid("a notification names %d %s and the "+
				"maximum is %d — a routing snapshot is copied onto the log, "+
				"held by each of the stream's replicas for its whole "+
				"retention window and applied into every data node's rows, "+
				"so an unbounded one is bytes the fleet stores for a year",
				c.size, c.field, c.max)
		}
	}
	return nil
}

// TurnSpend is a turn's cost, riding the turn record it is computed from: a
// FOLD, a record class this design does not have. It makes a task's running
// total a FUNCTION of the applied records: the applier adds these counters
// only when the turn's insert affected a row, in the same transaction, so a
// redelivery cannot double-count and a zero-row update is a malformed record
// that stops the loop.
//
// WHAT A TURN'S TOKENS ARE is the engine's to decide (ADR-0022): its own
// phases, its delegated workers and the extension judge, plus a collected
// executor's tokens on the segment that resumed from it. Workers and SentBack
// are COUNTS (delegated tasks run, reviews that sent work back), not tokens,
// and like every counter here are omitted at zero.
type TurnSpend struct {
	Turns      int `json:"turns,omitempty"`
	Rounds     int `json:"rounds,omitempty"`
	Input      int `json:"input,omitempty"`
	Output     int `json:"output,omitempty"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
	WallMs     int `json:"wall_ms,omitempty"`
	Workers    int `json:"workers,omitempty"`
	SentBack   int `json:"sent_back,omitempty"`
}

// Tokens is the derived eighth counter, so nothing else adds the two halves.
func (s TurnSpend) Tokens() int { return s.Input + s.Output }

// EncodeBarrier renders a read index's barrier as a record this domain's
// applier decodes, which [statelog.NewReadIndex] needs: the read index owns
// WHEN a barrier is appended and what it proves, the domain what a record on
// its log looks like.
//
// NO OP ID, asserted rather than assumed: an op id becomes the Nats-Msg-Id, a
// repeat inside the duplicate window is served with no quorum round trip, and
// its sequence is then a position nothing confirmed, which is the whole proof
// a linearizable read rests on.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("worklet: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("worklet: a barrier carries no op id and this one " +
			"has one — an op id becomes a message id, and a duplicate ack is " +
			"served with no quorum round trip at all")
	}
	return MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// PINNED, never [RecordVersion] and never left to the
			// table: see [baseRecordVersion]. The encoder refuses a
			// barrier carrying a versioned field.
			V:       baseRecordVersion,
			Subject: BarrierSubject(),
			Op:      OpBarrier,
			Scope:   ScopeSet{Subject: true},
			Gen:     env.Gen,
		},
	}.Encode()
}

// EncodeTrimmed renders the trim's trimmed record ([statelog.TrimmedEnvelope],
// ADR-0080) as a record this domain's applier decodes: the barrier's split of
// ownership, met here.
//
// THE WRITER TRAVELS, unlike a barrier's: the record removes rows, so the
// eviction gate must be able to drop an evicted node's. NO OP ID: one becomes a
// message id and a ledger row nothing resolves. The floor is CHECKED here at
// the write, not in [Subject.Validate], which runs on the read path too: a
// refusal there would stop the applier on every node, where the framework's
// apply faults the record instead.
func EncodeTrimmed(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.TrimmedKind || env.Subject.Kind != statelog.TrimmedKind {
		return nil, fmt.Errorf("worklet: a %q envelope on %q is not a trimmed record",
			env.Kind, env.Subject)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("worklet: a trimmed record carries no op id and this " +
			"one has one — an op id becomes a message id and a ledger row nothing " +
			"resolves")
	}
	floor, err := statelog.TrimmedFloor(Domain{}.Stream().Name, env.Subject)
	if err != nil {
		return nil, fmt.Errorf("worklet: %w", err)
	}
	return MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// PINNED, for the barrier's reason: see [baseRecordVersion].
			V:       baseRecordVersion,
			Subject: TrimmedSubject(floor),
			Op:      OpTrimmed,
			Scope:   ScopeSet{Subject: true},
			Gen:     env.Gen,
			Writer:  env.Writer,
		},
	}.Encode()
}
