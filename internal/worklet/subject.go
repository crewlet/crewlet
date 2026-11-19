// Package worklet is Worklet, the engine's own work tracker, as a state-log
// domain.
//
// It is the first domain on that framework, so ADR-0002 is the decision every
// rule below rests on: the stream is the write-ahead log and these SQL tables
// are derived from it. The record is at adr/0002; [internal/statelog] is its
// authority and this package restates none of it.
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
//
// # What the applier says after a commit
//
// Beside the rows, the applier reports WHOSE INBOX each committed batch moved
// ([inbox.Movement]) — the one fact a person's dashboard needs to
// learn about their work without polling, and one only the apply sees on every
// node. It is announced after the commit and never inside the transaction,
// whose body can re-run, and it carries identifiers and a count rather than
// the notice, which is read through the question that decides who may read it.
// A purge that deletes somebody's notices moves their inbox too, and names
// them with nothing unread added.
//
// # What a purge writes beside its own task
//
// A record's scope is the COMPLETE set of objects its apply may write, stated
// by the writer (see [ScopeSet]), and a purge is the record that writes the
// most objects that are not its subject. Every one of them is a CROSS-OBJECT
// effect the purge's scope has to name. What each effect writes is a DOCUMENT
// change as well as a row change, and a task reached by several of them is
// rewritten once with all of them:
//
//   - its DEPENDENTS: their `waiting_on` relation to it is taken out of their
//     documents, and their dependency edges naming it deleted;
//   - the BLOCKERS it waits on: it is taken out of the mirror their documents
//     keep of who waits on them, and out of their mirror rows;
//   - every task with any other RELATION to it: the relation is taken out of
//     that task's document and rows;
//   - every task whose body REFERENCES it: the reference row is deleted (the
//     key it named resolves to nothing afterwards, so no later record of that
//     task writes it back — the one effect with no document half);
//   - its SUBTREE: each direct child is re-parented onto the purged task's own
//     parent, document and pointer, and every descendant's ancestry is
//     rebuilt;
//   - its KEYS, current and former: its claim on each is released and handed
//     on to the lowest id among the tasks still holding that key as their
//     current one, and every holder's `key_collision` is re-derived — the one
//     effect named by KEY rather than by task, because which task a key opens
//     is decided by the records filed under that key, and every create,
//     re-key and purge that claims, frees or hands one on names it there
//     ([keyTerm]). So no co-holder of a key appears in the reach.
//
// The writer names all of them, each at its own project's path — a descendant
// may be filed in another project than the purged task ([purgeReach]), and a
// former key sits under the project that minted it — and the decide refuses a
// purge whose scope comes up short of the rows it decides on.
//
// WHAT NO SCOPE CAN NAME is a task that comes to name the purged one AFTER the
// purge was decided and before it lands — a relation, a dependency or a child
// written on that task's own subject, which does not contend with the purge's.
// The apply rewrites such a task too, since it reads the rows at its own
// position, and a node that deferred the record which made it name the purged
// task does not hold the purge back for it. The window is one decide-to-append
// round trip on each side, and it is the one every widened scope in this
// package shares ([Writer.scopeForDependents] has it for a dependent arriving
// the same way); only a write that contended with the purge's own subject
// could close it.
//
// # Every new field is version-gated, and every new derived column re-derived
//
// The version a record carries is the LOWEST one that reads it — the highest
// version among the fields in the versioned-field table it actually carries,
// or 1 — and the encoder stamps it, so no writer states one. A field added to
// a record, a payload or a document without a row in that table is decoded
// AROUND by an older build: it finds the version readable, drops the field and
// applies the rest, and its rows for that object differ from its peers' for
// good. With the row, the older build retains the record instead, and only
// that record and what its scope meets; everything carrying nothing new stays
// readable by every build. [RecordVersion] moves with the table and only with
// it (see its doc for the whole rule).
//
// THREE KINDS OF RECORD ARE PINNED rather than stamped, because an older node
// defers or retains what it cannot read and none of them survives that: an
// apply gate at [GateRecordVersion], and the read index's barrier and a
// reanchor's generation at the base version. The encoder refuses any of them
// carrying a versioned field at all, save the one a gate's own changed apply
// takes ([statelog.VersionedField.Gate]), at which an older node halts.
//
// A change to how the applier treats a field it ALREADY copies is the same
// hazard with no new field to carry it, and takes the same gate: a marker the
// writer sets and a row that stamps it, with the new rule applied only to a
// record at that version or above, so a record the build before the change
// wrote is applied by that build's rule on every data node.
//
// A column the applier COMPUTES from the rows it holds, rather than copies out
// of a record, is the other half: no record version can see it, because what
// differs between builds is the rule. Such a column is maintained under
// [statelog.Deriver] — the rule set is versioned on the checkpoint row, and
// the first boot of a build whose rules differ re-derives the column from the
// rows, in the applier's own Go, inside one transaction — never backfilled a
// second time in a migration's SQL.
//
// # A decision is a structured ask, and nothing more
//
// ADR-0023 is the record: a decision somebody needs is an ask comment carrying
// a [Decision], answered by a comment whose [Comment.Choice] names one of its
// options, and the engine enforces exactly its shape, that the choice names an
// option of the ask it answers, and that a promised inform is kept — never an
// approval chain, a quorum or a state machine. decision.go holds the type and
// the rules; the ADR says why the engine stops there. ADR-0160: a url this
// engine handed out for one of its artifacts is cited as the artifact.
//
// # A question and a review say whom they wait on
//
// ADR-0124 is the record: an asker withdraws a question saying why and what
// covers it ([Withdrawal]), an item in review its holder submitted itself is
// held by a question to its reviewer or comes back to it, and `holding_up`
// counts the agent work waiting on a person through what they hold or are asked.
//
// # Urgent ready work comes first
//
// ADR-0159 is the record: in a seat's ready work ([orderReady]) an urgent item
// comes before every item that is not, started work included; below urgent,
// and among urgent items, started work still comes first.
//
// # An item in progress says why it is not moving
//
// ADR-0162 is the record: an item in progress carries a read-only flow state
// (working, blocked, waiting, held, due, stuck, moving), derived by one SQL
// expression every read evaluates, recorded when it changes and told to a
// person when nobody is moving the item; no status changes on its own.
//
// # What a day costs the log, measured
//
// The reference company — 100 000 tasks a year, 300 000 comments, 1 000 edits
// and 3 000 turns a day, the corpus docs/guides/retention.md's forecast is
// stated for — written as a seat's tools write it, through the real writer
// onto a real broker: each task filed into the unit the chart gives its
// project, each change with the notification the seat's tools attach, which
// names the project's lead and that unit's lead for its fallback, and each
// gesture costed by the stream's own growth, which is what the broker counts
// against the ceiling (TestTheMutationLogsCeilingIsFiveYearsOfAMeasuredDay,
// which holds each figure here to within a hundredth of what it measures, and
// TestTheDocsPrintTheMeasuredDay, which holds every page that prints one).
// Every record is appended in the signed frame ADR-0031 puts round it — its
// magic, version and key id's length, the key id and a 32-byte MAC, forty bytes
// under the suite's two-byte key id — so each figure carries one frame a record:
//
//   - a task filed: 5 266 bytes, in two records — the project's counter and
//     the task — at a 2 KiB description, which the task record carries whole
//     and its notification's excerpt again up to [MaxExcerpt];
//   - a comment: 3 037 bytes at 1 KiB, carried twice the same way, with the
//     commenter's watch;
//   - an edit: 1 181 bytes for a status change — the change itself is a few
//     bytes, and the rest the record's envelope, its frame and the routing
//     snapshot every notification takes;
//   - a turn charged to its task: 1 608 bytes with its summary at
//     [MaxTurnSummary].
//
// So the day is 9.9 MB of records — 1.44 MB of tasks, 2.50 of comments, 1.18
// of edits and 4.82 of turns: half of it is the turns, whose every field is
// capped, so the text a company writes moves it less than its rate of turns
// does (a 1 KiB description and a 512-byte comment make it 9.2 MB). Its keys
// are a new project's, one or two digits, where a project of the reference
// company's writes five by its fifth year: three bytes more wherever a key is
// written, about 8 kB of the day. Beside the records the seats' 12 500
// linearizable reads and the object collector's 25 append 2.2 MB of barriers,
// at 176 bytes each from a log's second generation to its tenth (168 at its
// first; TestABarrierCostsTheMutationLogWhatTheForecastPricesItAt) — 12.1 MB a
// day in all, which the trim's seven-day min_age holds about 85 MB of on a
// fleet that trims. [TrackerLogMaxBytes] is nearly five years of the records,
// and nearly four years of the whole day, barriers included.
//
// # A task names what it touches in another domain
//
// A linked page and an atlas entry are edges on the task's own record whose
// other end is another domain's id ([RelationPage], [RelationEntry]), and that
// domain is never written. An entry is named by its id alone, counted rather
// than named in a history row, and left out of an answer for a reader who may
// not read the atlas ([TaskDetail.WithoutEntries]); the work on each entry is
// read from the tracker's own rows ([Reader.EntryMarks], [Reader.EntryWork]).
// ADR-0140 records the decision.
//
// # What a history row's delta carries
//
// The delta is the applier's, computed in the apply's own transaction from the
// object as this node stored it and the object the record says it should be
// ([Applier.writeHistory]). A wake's Fields is never a second producer: a
// quiet commit has no wake yet still happened, and a wake is built from a
// snapshot read outside the write's transaction, so it can name a move another
// writer already made. Its fields are read only where the apply has nothing to
// compare, a record the version guard skipped ([statedDeltas]). So every
// history row says what changed, not only those somebody was notified about.
//
// The comparisons are pure over two values, as coerce.go and
// [internal/textindex] are, because a rule reachable only through a database is
// one nobody re-measures; only [documentDeltas] reads.
//
// A side is the log's own text, never a rendering: a history row is one of the
// state log's N identical copies, so it may not depend on live configuration,
// a reader's time zone or a row its subject does not own. A relation names the
// other task by its ID, since a key is a fact about another task's row, and a
// node that had not applied that task would store a different string forever
// in a table nothing rewrites. The surface resolves the ID.
//
// Nothing is cut. The table is never swept, so every byte stays on every node
// for the company's life, and a cut keeps an opening a reader takes for the
// whole value, or loses from a set exactly the member the commit added:
//   - a set records what it gained and lost, as whole members, sorted
//     ([deltaSet.set]);
//   - an ordered list (a person's queue, their pinned views, a task's
//     checklists) carries both sides whole, since its order is its content and
//     each is capped in the tens where it is written;
//   - free text is whole up to [MaxDeltaValue] and past it is its size
//     ([proseText]), which nobody mistakes for the text, kept whole in the
//     row's mutation; one member of a list of values does the same at
//     [MaxDeltaElement];
//   - an identifier (a handle, ID, slug or path) is whole: each is bounded where
//     it is written, and a size cannot stand in for a name.
//
// # Every statement that reads a task's tables is registered and planned
//
// TestEveryIndexServesARegisteredQuery plans each registered statement over a
// task's own tables (the task, its tags, its dependency edges and their
// mirror, its relations, its field values) and the history, and holds the set
// complete: an SQL literal anywhere in the module's non-test source that reads
// one of them and is no registered statement fails it. So a read added here or
// in another package (internal/search exports its statements for this) is
// registered and planned, because an unplanned one sorted every live task per
// batch while nobody looked.
package worklet

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ObjectKind is what an object on the mutation log is.
//
// Valid is false for a kind this build does not know, and the literal is
// kept either way: a newer peer's unknown kind is deferred under a family
// term formed from that literal, and a sentinel would make the writer's own
// deferral probe miss the record.
type ObjectKind string

// The kinds.
//
// Exported because four readers compare against them: the publisher's
// subject, the wake feed's filter, the applier's dispatch and the domain's
// Tables declaration. A kind added to three of the four publishes, is
// delivered, wakes nobody and writes nothing.
const (
	// KindTask is one work item, and the overwhelming majority of records.
	KindTask ObjectKind = "task"

	// KindProject is a project's settings: its field declarations, its
	// default assignee and its archived flag.
	KindProject ObjectKind = "project"

	// KindCounter is a project's key sequence, on its own subject so a key
	// mint never contends with an edit to the project's settings.
	KindCounter ObjectKind = "counter"

	// KindTags is a project's tag set.
	KindTags ObjectKind = "tags"

	// KindCatalogue is a workspace catalogue — "types" or "fields".
	KindCatalogue ObjectKind = "catalogue"

	// KindView is a saved view.
	KindView ObjectKind = "view"

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
	// It installs a gate, so its version is pinned at 1 for ever: a node that
	// deferred an eviction would leave its gate table empty and keep applying
	// the evicted node's records, with no inverse to repair it.
	KindEviction ObjectKind = "eviction"

	// KindRankOrder is a project's manual order: a drag mutates a total order
	// over the project's tasks that no single task's version can protect.
	KindRankOrder ObjectKind = "rankorder"

	// KindBarrier is the read index's payload-free append, on ONE subject for
	// the whole domain. Like [KindTrimmed] it writes no row of this domain's,
	// and the applier has a case for it all the same: a kind with no case
	// faults (TestAKindNobodyDeclaredFaults).
	KindBarrier ObjectKind = "barrier"

	// KindFile is one file in a project: where it lives, what it is and which
	// object-store object holds it (internal/objstore, ADR-0026). The record
	// names the object, which keeps it alive, and never carries the bytes.
	//
	// Its subject is its address (the project and a token of the path), so two
	// writers of one path contend on one subject and exactly one wins, with no
	// claim table. A move is a put at the new address and a removal at the old,
	// and it uploads the bytes again: an object is named only by the write that
	// uploaded it (ADR-0027), and the collector's lock-free deletion rests on no
	// second write ever naming one key.
	KindFile ObjectKind = "file"

	// KindRoster is the order of the org chart's seats by their names, on
	// ONE subject ([RosterSubject]), reconciled from the chart by
	// [Writer.ApplyChart] and read by `sort=assignee` through
	// `tracker_seat_places`. Machinery: no audience and no history row.
	KindRoster ObjectKind = "roster"

	// KindFlow is an item's flow state recorded when it changes, and a
	// notice of it to a person (ADR-0162), on a subject per item
	// ([FlowSubject]) so recording a state never moves the item's own
	// version or `updated_at`. ADDITIVE like a turn: one writer, the flow
	// duty, and a derived key ([FlowRecord.ID]) that lands each once.
	KindFlow ObjectKind = "flow"

	// KindTrimmed is the trim's record that removes the arbitration anchors
	// below what the log no longer holds ([statelog.TrimmedKind], ADR-0080), on
	// one subject per floor, which the subject's id names. The framework
	// removes the anchors in the applying transaction; this domain writes
	// nothing and has a case saying so, as for [KindBarrier].
	KindTrimmed ObjectKind = statelog.TrimmedKind
)

// ObjectKinds are every kind this build writes, in the order they are
// documented.
//
// A kind is never dropped from this list by the build that stops writing
// it: its records stay in the log through the successor's rolling upgrade,
// and a successor with no case for them faults ([Applier.apply]). A
// successor that removes a kind reads its records past under an apply gate
// of its own.
var ObjectKinds = []ObjectKind{
	KindTask, KindProject, KindCounter, KindTags,
	KindCatalogue, KindView, KindPerson, KindAlias,
	KindTurn, KindGeneration, KindEviction, KindRankOrder, KindBarrier,
	KindFile, KindTrimmed, KindRoster, KindFlow,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation.
//
// All but four do. A turn and a flow are additive and race nobody. Every barrier
// shares one subject, so an expectation would serialise the company's
// linearizable reads and write an anchor row per read under the store's
// only writer. A trimmed record removes anchors and identical ones must each
// land, so an expectation would refuse all but the first, and its own
// anchor would be the row it exists not to leave.
func (k ObjectKind) Arbitrated() bool {
	switch k {
	case KindTurn, KindFlow, KindBarrier, KindTrimmed:
		return false
	}
	return k.Valid()
}

// InstallsGate reports whether a record on this kind installs an apply gate.
//
// Answered from the kind alone, so a node that cannot decode the payload
// still turns an unknown version into a STOP rather than a deferral.
func (k ObjectKind) InstallsGate() bool { return k == KindEviction }

// CatalogueTypes and CatalogueFields are the two catalogue objects, named so
// a third cannot appear by typo.
const (
	CatalogueTypes  = "types"
	CatalogueFields = "fields"
)

// Subject is one object on the mutation log.
//
// Its wire names are lowercase and its id is never omitted: the names are in
// every record for the life of the deployment, and an always-present id
// keeps the barrier's record byte-identical across builds, which matters
// because the barrier's size drives the read index's cost model.
type Subject struct {
	Kind ObjectKind `json:"kind"`
	ID   string     `json:"id"`
}

// One constructor per kind rather than a Subject literal at each call site:
// half the ids are composed (an alias claim's is "<KEY>.<n>"), and a
// composition written twice is a subject two writers disagree about.

// TaskSubject names one work item by its id: writers racing on one task
// contend at the broker, and writers on different tasks never contend.
func TaskSubject(id string) Subject { return Subject{Kind: KindTask, ID: id} }

// IsTaskID reports whether s is spelled as every task id the engine mints:
// a canonical uuid (lowercase, four dashes), as a task's row, its subject
// and its items (`native:<id>`) spell it.
//
// No key takes this form ([taskKeyPattern] has one dash), so a caller that
// needs no proof the task still exists (a listing of what carries the item,
// which a purged task's rows still carry) may take it as named; only a key
// needs [Reader.Task]'s resolution. Only the canonical form counts: braces,
// `urn:`, uppercase or no dashes parse to the same uuid but match no row,
// since [resolveTaskID] compares bytes, so such a spelling is resolved as a
// key, which refuses it. A task filed under an id of another shape is found
// the same way.
func IsTaskID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

// ProjectSubject names one project's settings by its key; its key counter
// mints on another subject ([CounterSubject]).
func ProjectSubject(key string) Subject { return Subject{Kind: KindProject, ID: key} }

// CounterSubject names one project's key sequence by its key, so minting
// "<KEY>-<n>" contends only with other mints in that project.
func CounterSubject(key string) Subject { return Subject{Kind: KindCounter, ID: key} }

// TagsSubject names one project's tag declarations by its key, so declaring
// a tag never contends with an edit to the project's settings.
func TagsSubject(key string) Subject { return Subject{Kind: KindTags, ID: key} }

// ViewSubject names one saved view by its id.
func ViewSubject(id string) Subject { return Subject{Kind: KindView, ID: id} }

// PersonSubject names one person's inbox, priorities and pins, keyed on the
// seat's handle (ADR-0013), which every caller reaching it holds. A name no
// seat answers to, such as a person's login, is its own key.
func PersonSubject(handle string) Subject {
	return Subject{Kind: KindPerson, ID: handle}
}

// RankOrderSubject names one project's manual order by its key
// ([KindRankOrder]).
func RankOrderSubject(key string) Subject {
	return Subject{Kind: KindRankOrder, ID: key}
}

// RosterSubject names the org chart's seat order, the one object of its kind.
func RosterSubject() Subject { return Subject{Kind: KindRoster, ID: "seats"} }

// EvictionSubject names one node's eviction or readmission by its node id:
// the record installs an apply gate on that node's records, so it is
// addressed to the node rather than to anything the node wrote.
func EvictionSubject(nodeID string) Subject {
	return Subject{Kind: KindEviction, ID: nodeID}
}

// FlowSubject names an item's flow records by the item's id, so they fall
// under its deletion marker ([markedTask]) as its turns do, and never contend
// with a write to the item itself.
func FlowSubject(task string) Subject { return Subject{Kind: KindFlow, ID: task} }

// TurnSubject names a turn's spend by the task it was spent on, never by
// the turn's own id (the payload carries that), so it falls under the
// task's deletion marker ([markedTask]). Turns are additive, so writers
// here never contend ([ObjectKind.Arbitrated]).
func TurnSubject(task string) Subject { return Subject{Kind: KindTurn, ID: task} }

// AliasSubject names a cross-project move's create-only claim on a former
// key. The attempt number is in the id so a second move of the same key is
// a new claim rather than a lost race on the first.
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

// TrimmedSubject names the trimmed record whose floor is floor, in the
// framework's ONE spelling of it ([statelog.TrimmedSubject]) — the framework
// reads the floor back from the id.
func TrimmedSubject(floor statelog.Position) Subject {
	return Subject{Kind: KindTrimmed, ID: statelog.TrimmedSubject(floor).ID}
}

// FileSubject names one file by its address, the project and its path
// there, which is what two writers of one path contend on.
//
// The path is a token, as a page title is: a subject may carry no space,
// `*` or `>` and a scope id no `/`, which paths routinely carry. The record
// carries the path, and the applier recomputes the token and refuses a
// record whose path and subject disagree. The path is normalised first
// ([NormalizeFilePath]), so one file has one token.
func FileSubject(project, path string) Subject {
	return Subject{Kind: KindFile, ID: ProjectKey(project) + "." + FileToken(path)}
}

// FileToken is the address a path is arbitrated on: the first sixteen bytes
// of SHA-256 over the normalised path, in lower-case hex (internal/doclet's
// TitleToken makes the same trade and prices the collision).
func FileToken(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:16])
}

// isFileToken reports whether id has a [FileToken]'s shape, thirty-two
// lower-case hex digits, which tells a file's object term from a task's or
// a view's hyphenated uuid.
func isFileToken(id string) bool {
	if len(id) != hex.EncodedLen(sha256.Size/2) {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// SplitFileID takes a file subject's id apart: the project and the token.
func SplitFileID(id string) (project, token string, ok bool) {
	project, token, ok = strings.Cut(id, ".")
	return project, token, ok && project != "" && token != ""
}

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
// An unknown kind is not refused here: it is refused where a record is
// written and accepted where one is read (package doc, "Why the record
// decodes in two passes").
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("worklet: a subject with no kind addresses the log's " +
			"own prefix, which is a real subject inside the stream's wildcard " +
			"that no applier has a case for")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("worklet: subject kind %q carries a separator or a "+
			"wildcard, so the kind and the id could not be told apart again",
			s.Kind)
	}
	if s.ID == "" && s.Kind != KindBarrier {
		return fmt.Errorf("worklet: a %s subject needs an id; only the barrier "+
			"is a kind with exactly one object", s.Kind)
	}
	if strings.ContainsAny(s.ID, " \t\n*>") {
		return fmt.Errorf("worklet: subject id %q carries whitespace or a "+
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

// HomedInAProject reports a kind whose scope path is qualified by a project
// it does not name itself.
//
// A scope path is a containment hierarchy, which is what makes a
// project-wide deferral cover its tasks. Other kinds derive their container
// from their own subject; these four cannot. A task's project is a mutable
// column and a turn's or a flow's subject is its task, so only the writer
// holds the container; a view's container is a choice, a project or the
// company's top. A task, a turn and a flow must have one: an empty container
// there is a writer that forgot, not a workspace-homed object.
func (k ObjectKind) HomedInAProject() bool {
	switch k {
	case KindTask, KindTurn, KindFlow, KindView:
		return true
	}
	return false
}

// Routable reports an object kind whose records can wake somebody.
//
// A task; a person, whose wake is about the person rather than a row
// (somebody else wrote their priority list), which no task's routing
// reaches; and a flow, whose notice tells one person an item is not moving
// (ADR-0162). Every other kind is machinery or read from its own surface, and a
// wake per catalogue edit would page the company for a renamed dropdown. A
// closed set, so a later kind is silently unroutable rather than silently
// routed: a missed wake costs one person, an unintended one wakes every seat
// for a bookkeeping append.
func (k ObjectKind) Routable() bool {
	switch k {
	case KindTask, KindPerson, KindFlow:
		return true
	}
	return false
}

// RecordsHistory reports a kind whose apply writes a `tracker_history` row.
//
// Such records must state their [ChangeKind]: every feed filter, report
// window and the unblocked repair's scan select on the row's `kind`, and one
// guessed from the operation is a different vocabulary no filter names.
// These are exactly the kinds [Applier.apply] routes to a path that writes a
// row; the rest are machinery with no audience. A closed set, for
// [ObjectKind.Routable]'s reason turned around: a later kind fails the
// writer's own check rather than publishing history filed under a guess.
func (k ObjectKind) RecordsHistory() bool {
	switch k {
	case KindTask, KindProject, KindTags, KindCatalogue,
		KindView, KindPerson, KindFile:
		return true
	}
	return false
}

// RequiresAProject reports a kind that cannot live at the top of the company.
func (k ObjectKind) RequiresAProject() bool {
	return k == KindTask || k == KindTurn || k == KindFlow || k == KindFile
}

// GatedByPurge reports a kind whose subject id is a task id, so a purge of
// that task's marker gates every record of it: the task's own, the turns
// charged to it and its flow.
//
// One predicate for both statements of the deletion gate ([Applier.Gated]
// and [Gates.GatedAt]): a kind added to one alone would drop a record on
// every node while its writer was told it applied.
func (k ObjectKind) GatedByPurge() bool {
	return k == KindTask || k == KindTurn || k == KindFlow
}

// NoticesItself reports a kind that writes no history row and tells people
// all the same: a flow notice, whose apply writes its own inbox row keyed on
// its own key ([Applier.applyFlow]).
func (k ObjectKind) NoticesItself() bool { return k == KindFlow }

// ProjectKey normalises what somebody typed into what the column stores.
//
// Keys are minted upper-case and every `project_key` comparison is exact,
// so `eng` would get an empty answer rather than a refusal, which a person
// acts on wrongly (filing a duplicate, concluding their work was lost).
// Lower-casing the column instead would defeat every index leading with it.
func ProjectKey(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}
