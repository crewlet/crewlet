package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/textcut"
)

// WHAT MOVED — one rule, for every object whose apply writes a history row.
//
// # The rule
//
// THE DELTA IS THE APPLIER'S, computed inside the apply's own transaction from
// the two states that frame holds: the object as this node stored it, and the
// object the record says it should be. [Applier.writeHistory] states the same
// thing from the other end, and it is why the notification's own `Fields` is a
// compatibility rung rather than a second producer — a delta derived from a
// wake is a delta only a LOUD commit has, and a quiet commit is one that
// happened without an audience rather than one that did not happen.
//
// It follows that every history row says what changed, not only the rows for
// the kinds somebody was told about. Until this file existed only a task's
// did: [TaskDeltas] compared two task documents, and [Applier.applyDocument]
// passed a literal nil — so a project created, a project updated, a view
// saved, a tag set edited, a catalogue edited and every person write stored
// `{}`, and a fresh company's activity feed read as six rows of "project
// created" with a dash where the change should be.
//
// # Why the comparison functions are PURE OVER VALUES
//
// For `coerce.go`'s reason and [internal/textindex]'s: a rule that can only be
// exercised through a database is a rule nobody re-measures. [documentDeltas]
// is the one half that reads — it fetches the stored document and decodes the
// record's — and every comparison below it takes two values and returns a map.
//
// # Why the values are the LOG'S OWN TEXT and never a rendering
//
// A history row is one of the state log's N identical copies, so a delta may
// not carry anything that depends on live configuration, on a reader's zone or
// on a row this record's subject does not own. `wake.go` says it for a date —
// "rendering a day from it belongs to a surface, which knows the zone" — and
// the same sentence decides the harder case here: a relation names the other
// task by ITS ID, because a key is a fact about another task's row. A node
// that had not applied that task would store a different string, for ever, in
// a table nothing rewrites and nothing sweeps. The surface resolves it; the
// log records what it owns.
//
// # Why every collection is bounded
//
// The row is a log line rather than a snapshot. A project's field catalogue, a
// person's inbox and a saved view's query are each large enough to make one
// history row heavier than the record that produced it — and this table is
// never swept, so every byte is kept for the life of the company on every
// node. [MaxDeltaValue] bounds one side, [MaxDeltaElement] bounds one member
// of a collection, and a collection too big to show is reported WITH ITS COUNT
// rather than silently truncated.

// MaxDeltaValue is how much of one side of a delta a history row carries.
//
// SIX HUNDRED BYTES — [MaxExcerpt]'s own figure, for [MaxExcerpt]'s own
// reason. A delta side is what a reader sees for ONE field on ONE line,
// exactly as an excerpt is what a card shows of a body, and the two are read
// in the same places by the same people. Naming it separately rather than
// spelling the constant inline is what [MaxTagLabel] does with [MaxViewName]:
// the two numbers are equal because the argument is the same, and either may
// move without dragging the other.
const MaxDeltaValue = MaxExcerpt

// MaxDeltaElement bounds ONE member of a collection a delta carries.
//
// A QUARTER OF [MaxDeltaValue], so four members of a changed set are visible
// whatever one of them weighs. A saved view's query may put 32 KiB in a SINGLE
// parameter ([MaxViewParamsBytes]); bounding only the whole would let that one
// parameter spend the entire budget and push every other change the same save
// made into the dropped count.
const MaxDeltaElement = MaxDeltaValue / 4

// deltaSet is the fields a comparison found, and the ONE place the two rules
// every producer shares are written: a side that reads the same on both ends
// did not move ([deltaSet.add], and [deltaSet.mark] for the two fields that
// carry a marker instead of a value), and a set past [MaxDeltas] is trimmed
// deterministically.
//
// ONE TYPE RATHER THAN A CLOSURE PER FUNCTION, because "the same text is not a
// change" written seven times is six chances for one of them to say something
// else.
type deltaSet map[string]Delta

// add records one field's move, and ignores one that did not happen.
func (d deltaSet) add(field, from, to string) {
	if from != to {
		d.mark(field, from, to)
	}
}

// mark records a move the CALLER has already established, whatever the two
// sides read.
//
// THE ONE EXCEPTION TO "a side that reads the same on both ends did not move",
// and it exists because two of a task's fields carry a MARKER rather than the
// value — which is the only shape those two can honestly take in a log line.
// A body delta carries a SIZE and never the prose, so replacing one paragraph
// with another of the same length moves the task and leaves both markers
// identical; a custom-field delta carries each value CUT to
// [MaxDeltaElement], so two long values differing past the cut render the
// same. In both the KEY'S PRESENCE is the statement that the field moved and
// the sides say what can be said about it — which is strictly more honest than
// [deltaSet.add]'s silence, and is why the caller establishes the move from
// the VALUES rather than from their renderings.
//
// Every other field uses [deltaSet.add]: its text is the whole value, so two
// sides reading the same IS the field not having moved.
func (d deltaSet) mark(field, from, to string) {
	d[field] = Delta{From: from, To: to}
}

// done is the set as a delta map: NIL when nothing moved, and trimmed to
// [MaxDeltas] when more moved than a card shows.
//
// A NIL MAP FOR "NOTHING MOVED" is what [Applier.writeHistory]'s length guard
// reads, and it is why that guard exists: `jsonOf` is `json.Marshal` and a nil
// map marshals to `null`, so the column would hold two spellings of empty if
// the nil ever reached it.
//
// THE TRIM IS BY FIELD NAME, deterministically: a card that showed a different
// thirty-two on two nodes would be one screen disagreeing with another about
// what changed.
func (d deltaSet) done() map[string]Delta {
	switch {
	case len(d) == 0:
		return nil
	case len(d) <= MaxDeltas:
		return d
	}
	names := make([]string, 0, len(d))
	for name := range d {
		names = append(names, name)
	}
	sort.Strings(names)
	trimmed := make(map[string]Delta, MaxDeltas)
	for _, name := range names[:MaxDeltas] {
		trimmed[name] = d[name]
	}
	return trimmed
}

// documentDeltas is what one record moved on a whole-document object.
//
// THE STORED DOCUMENT IS READ IN THE APPLY'S OWN TRANSACTION, before the
// upsert that replaces it — the same frame [Applier.applyTask] reads `current`
// in, and the only frame that holds both states at once.
//
// It costs one indexed read and one decode per document apply. That is a price
// paid on the RARE subjects: a task apply is the hot path and does not come
// through here, while a project, a view, a catalogue, a tag set and a person
// record are written when somebody changes a setting. What the alternative
// cost was measured on screen — a history page whose every document row said
// nothing at all.
//
// A CATALOGUE NAME THIS BUILD DOES NOT KNOW YIELDS NO DELTAS RATHER THAN AN
// ERROR. A newer peer may declare a third catalogue, and its document still
// upserts here; refusing it would stall the fleet's log over a column that
// describes a change rather than makes it.
func documentDeltas(ctx context.Context, tx *sql.Tx, s Subject,
	mutation json.RawMessage) (map[string]Delta, error) {

	decode := func(into any) error {
		if err := decodePayload(mutation, into); err != nil {
			return fmt.Errorf("tracker: decode the %s payload to compare it "+
				"with the stored one: %w", s, err)
		}
		return nil
	}
	switch s.Kind {
	case KindProject:
		before, _, err := readProject(ctx, tx, s.ID)
		if err != nil {
			return nil, err
		}
		var after Project
		if err := decode(&after); err != nil {
			return nil, err
		}
		return projectDeltas(before, after), nil
	case KindView:
		before, _, err := readView(ctx, tx, s.ID)
		if err != nil {
			return nil, err
		}
		var after View
		if err := decode(&after); err != nil {
			return nil, err
		}
		return viewDeltas(before, after), nil
	case KindPerson:
		before, _, err := readPerson(ctx, tx, s.ID)
		if err != nil {
			return nil, err
		}
		var after Person
		if err := decode(&after); err != nil {
			return nil, err
		}
		return personDeltas(before, after), nil
	case KindTags:
		before, _, err := readTagSet(ctx, tx, s.ID)
		if err != nil {
			return nil, err
		}
		var after TagSet
		if err := decode(&after); err != nil {
			return nil, err
		}
		return tagSetDeltas(before, after), nil
	case KindCatalogue:
		switch s.ID {
		case CatalogueTypes:
			before, _, err := readTypeCatalogue(ctx, tx)
			if err != nil {
				return nil, err
			}
			var after TypeCatalogue
			if err := decode(&after); err != nil {
				return nil, err
			}
			return typeCatalogueDeltas(before, after), nil
		case CatalogueFields:
			before, _, err := readFieldCatalogue(ctx, tx)
			if err != nil {
				return nil, err
			}
			var after FieldCatalogue
			if err := decode(&after); err != nil {
				return nil, err
			}
			return fieldCatalogueDeltas(before, after), nil
		}
		return nil, nil
	}
	return nil, fmt.Errorf("tracker: %s is not a whole-document object, so "+
		"there are no two documents to compare", s.Kind)
}

// projectDeltas is what changed between two versions of a project.
//
// THE CHART-OWNED FIELDS AND THE LEAD-OWNED ONES TOGETHER, because one record
// may carry either: a chart apply rewrites the name, the purpose and the unit
// under an epoch stamp, and a lead's edit rewrites the field declarations, the
// default assignee and the archive flag. A comparison that named only one half
// would be empty for every write of the other.
//
// `chart_epoch` IS RECORDED, and it is the only thing a re-declaration at a
// new epoch moves: [Writer.applyChartProject] publishes when the epoch differs
// although the three chart fields are equal, so leaving it out would put one
// empty `project_updated` row per project into the feed on every config
// activation — the exact row this file exists to end.
//
// The two instants are NOT recorded. `created_at` and `updated_at` say WHEN,
// and the history row's own `created_at` and `effective_at` already answer
// that in the two spellings a reader needs.
func projectDeltas(before, after Project) map[string]Delta {
	moved := deltaSet{}
	moved.add("name", scalarText(before.Name), scalarText(after.Name))
	moved.add("purpose", scalarText(before.Purpose), scalarText(after.Purpose))
	moved.add("unit", scalarText(before.Unit), scalarText(after.Unit))
	moved.add("default_assignee", before.DefaultAssignee, after.DefaultAssignee)
	moved.add("archived", boolText(before.Archived), boolText(after.Archived))
	moved.add("policy_version",
		countText(before.PolicyVersion), countText(after.PolicyVersion))
	moved.add("chart_epoch", strconv.FormatInt(before.ChartEpoch, 10),
		strconv.FormatInt(after.ChartEpoch, 10))
	// THE DECLARATIONS BY SLUG, which is what names a field to a person
	// and what changes when one is added or withdrawn. An edit that leaves
	// the slugs alone — a renamed label, an archive, a new option — moves
	// `policy_version` instead, because that counter is exactly what
	// [applyProjectEdit] advances on a fields edit and on nothing else.
	moved.add("fields", identsText(before.Fields), identsText(after.Fields))
	return moved.done()
}

// viewDeltas is what changed between two versions of a saved view.
//
// `rank` IS ONE OF THEM, unlike a task's. A task's rank moves on a record of
// its own that carries no change kind at all — "a reposition is not history" —
// while a view's rank rides the ordinary save, so a strip somebody rearranged
// produces a `view_saved` row whose only content is the rank. Left out, that
// row would say nothing.
func viewDeltas(before, after View) map[string]Delta {
	moved := deltaSet{}
	moved.add("name", scalarText(before.Name), scalarText(after.Name))
	moved.add("type", string(before.Type), string(after.Type))
	moved.add("container", containerText(before.Container), containerText(after.Container))
	moved.add("owner", before.Owner, after.Owner)
	moved.add("protected", boolText(before.Protected), boolText(after.Protected))
	moved.add("default", boolText(before.Default), boolText(after.Default))
	moved.add("icon", scalarText(before.Icon), scalarText(after.Icon))
	moved.add("rank", string(before.Rank), string(after.Rank))
	// THE PARAMETERS THAT MOVED, AS `key=value`, and only those.
	//
	// The whole query is the wrong thing to carry twice: it is bounded at
	// [MaxViewParamsBytes], so a save that narrowed one filter would put
	// tens of kilobytes of unchanged predicate into a log line to show it.
	// The keys alone are the wrong thing too — re-pointing `status` from
	// `open` to `done` changes no key, so the row would be empty for the
	// commonest edit a saved view gets.
	from, to := paramsText(before.Params, after.Params)
	moved.add("params", from, to)
	return moved.done()
}

// personDeltas is what changed on one person's own record.
//
// THE THREE INBOX LISTS ARE COUNTED RATHER THAN LISTED, and that is the one
// place here where a count is the honest answer: each holds up to
// [MaxInboxEntries] entries, every entry is a record id and a log position,
// and neither is something a person reads. What a reader of this row wants to
// know is that two items were marked read — which the counts say, because
// every gesture that writes these lists MOVES an entry between them.
//
// `priorities` IS ORDERED where the other collections are sorted, and that is
// the whole content of the field: the list IS the instruction, so a
// re-ordering with the same members is precisely the change somebody made. An
// edge set is sorted because nothing orders it; this one is stored in the
// order somebody chose.
//
// `priorities_set_at` IS NOT RECORDED although `priorities_set_by` is. Who
// re-ordered somebody else's queue is news; when is what the history row's own
// instant already says, and a stamp that moves on every write would make every
// re-statement of an unchanged list look like a change.
func personDeltas(before, after Person) map[string]Delta {
	moved := deltaSet{}
	moved.add("priorities", listText(before.Priorities), listText(after.Priorities))
	moved.add("priorities_set_by", before.PrioritiesSetBy, after.PrioritiesSetBy)
	moved.add("pinned_views", listText(before.PinnedViews), listText(after.PinnedViews))
	moved.add("favorites", sortedText(favoriteIdents(before.Favorites)),
		sortedText(favoriteIdents(after.Favorites)))
	moved.add("primary_reasons", sortedText(reasonIdents(before.PrimaryReasons)),
		sortedText(reasonIdents(after.PrimaryReasons)))
	moved.add("read", countText(len(before.Read)), countText(len(after.Read)))
	moved.add("unread", countText(len(before.Unread)), countText(len(after.Unread)))
	moved.add("snoozed", countText(len(before.Snoozed)), countText(len(after.Snoozed)))
	moved.add("seen_through", positionText(before.SeenThrough), positionText(after.SeenThrough))
	moved.add("generation", before.Generation, after.Generation)
	return moved.done()
}

// tagSetDeltas is what changed in a project's tag set.
//
// `tags_version` IS RECORDED BESIDE THE SLUGS because the two answer different
// edits: adding or withdrawing a tag moves the slug list, and renaming one or
// recolouring it moves neither — the counter is then the only witness that
// anything happened at all.
func tagSetDeltas(before, after TagSet) map[string]Delta {
	moved := deltaSet{}
	moved.add("tags", identsText(before.Tags), identsText(after.Tags))
	moved.add("tags_version",
		countText(before.TagsVersion), countText(after.TagsVersion))
	return moved.done()
}

// typeCatalogueDeltas is what changed in the workspace's task types.
func typeCatalogueDeltas(before, after TypeCatalogue) map[string]Delta {
	moved := deltaSet{}
	moved.add("types", identsText(before.Types), identsText(after.Types))
	return moved.done()
}

// fieldCatalogueDeltas is what changed in the workspace's field declarations.
//
// `policy_version` MOVES ON EVERY FIELDS EDIT — [Writer.WriteFields] derives
// it from the stored one — so it is what says a declaration was reshaped where
// the slug list is unchanged: a renamed dropdown, a new option, an archive.
// The catalogue writers already promise "the feed still has to be able to say
// a dropdown was renamed", and until this existed the feed could not.
func fieldCatalogueDeltas(before, after FieldCatalogue) map[string]Delta {
	moved := deltaSet{}
	moved.add("fields", identsText(before.Fields), identsText(after.Fields))
	moved.add("policy_version",
		countText(before.PolicyVersion), countText(after.PolicyVersion))
	return moved.done()
}

// The text one side of a delta carries, in the one shape every producer uses.

// scalarText is one free-text value, bounded.
//
// [textcut.Within] rather than [textcut.Ellipsis], because [MaxDeltaValue] is
// a ceiling this file enforces rather than a guide: the marker has to fit
// inside the budget, exactly as a notification's excerpt does.
func scalarText(s string) string { return textcut.Within(s, MaxDeltaValue) }

// boolText is a flag as text.
//
// BOTH STATES ARE PRESENT, unlike an absent date or an unsized estimate, so
// `false` is written out rather than folded into the empty string a renderer
// draws as an em dash — "Archived: — → true" would read as a field that had no
// value before, and every project has always had this one.
func boolText(b bool) string { return strconv.FormatBool(b) }

// countText is a number as text, zero included.
//
// A ZERO IS A VALUE HERE, which is the opposite of [minutesText]'s rule and
// deliberate: an estimate of zero means "unsized" and has no other absence,
// while an inbox of zero unread items is a fact somebody has just brought
// about.
func countText(n int) string { return strconv.Itoa(n) }

// containerText is where an object lives, in the two-part form the container
// grammar already uses.
//
// THE WORKSPACE CARRIES NO ID, exactly as it carries none in the scope
// alphabet and none on a view's own container — so it renders as the bare word
// rather than as `workspace:`, which would be a second spelling of the top of
// the company.
func containerText(c Container) string {
	if c.ID == "" {
		return c.Kind
	}
	return c.Kind + ":" + c.ID
}

// positionText is an inbox's read mark, in the `stream@generation:seq` triple
// [ParseLogPosition] reads back.
//
// ONE VOCABULARY for a log position wherever a person sees one. An unset mark
// is the empty string rather than `@0:0`, because somebody who has read
// nothing has no position and a renderer draws the empty string as an em dash.
func positionText(p Position) string {
	if p.Seq == 0 {
		return ""
	}
	return fmt.Sprintf("%s@%d:%d", p.Stream, p.Generation, p.Seq)
}

// listText is a collection as the text one side of a delta carries.
//
// WHOLE MEMBERS AND THEN A COUNT OF WHAT IS LEFT, never a cut inside one. It
// is the shape the turn-start thread block already takes, for the same reason:
// a list cut mid-member reads as a member, and a list cut silently reads as
// the whole set. The dropped count is what makes the row honest about being a
// log line rather than a snapshot.
//
// ", " IS THE SEPARATOR every delta over a collection in this package already
// uses — [TaskDeltas] joins a task's tags with it — so one renderer splits
// them all.
//
// THE TAIL'S OWN BUDGET COMES OFF THE TOP, so the whole value is inside
// [MaxDeltaValue] rather than inside it plus a tail.
func listText(items []string) string {
	if len(items) == 0 {
		return ""
	}
	bounded := make([]string, 0, len(items))
	for _, item := range items {
		bounded = append(bounded, textcut.Within(item, MaxDeltaElement))
	}
	if whole := strings.Join(bounded, ", "); len(whole) <= MaxDeltaValue {
		return whole
	}
	// THE FIRST MEMBER IS ALWAYS KEPT, which is what leaves this with no
	// "nothing fit" case to answer: a value that was only a count would be
	// a number where every reader expects a list. It fits by construction
	// — see maxListTail.
	budget := MaxDeltaValue - maxListTail
	kept, width := 1, len(bounded[0])
	for _, item := range bounded[1:] {
		next := width + len(", ") + len(item)
		if next > budget {
			break
		}
		width, kept = next, kept+1
	}
	return strings.Join(bounded[:kept], ", ") +
		", +" + strconv.Itoa(len(bounded)-kept) + " more"
}

// maxListTail is what [listText] reserves for the count it ends with.
//
// THIRTY-TWO BYTES against a tail of `, +NNN more`, which is eleven: every
// collection this package records is capped in the hundreds — [MaxDependents],
// [MaxOtherRelations], [MaxInboxEntries] — so three digits is the worst case
// and the rest is headroom for a cap that grows.
//
// The build fails below if one member plus that tail ever stops fitting inside
// a delta side, which is the assumption the "always keep the first member"
// rule above rests on.
const maxListTail = 32

const _ uint = MaxDeltaValue - MaxDeltaElement - maxListTail

// sortedText is [listText] over a SET — a collection nothing orders.
//
// SORTED, because a delta over a set is a statement about MEMBERSHIP: a writer
// that re-stated the same members in another order would otherwise record a
// change that did not happen, and the two sides of a set's move are only
// comparable at a glance when both are in one order.
func sortedText(items []string) string {
	out := slices.Clone(items)
	slices.Sort(out)
	return listText(out)
}

// identsText is [sortedText] over anything this package addresses by a slug.
//
// THE SLUG rather than the label, for the reason the tracker addresses these
// objects by it everywhere else: a label is what a company calls the thing
// this week, and a delta is a durable record of what one record changed.
func identsText[T interface{ Ident() string }](items []T) string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Ident())
	}
	return sortedText(out)
}

// favoriteIdents is a person's stars in the two-part form they are stored in.
func favoriteIdents(favorites []Favorite) []string {
	out := make([]string, 0, len(favorites))
	for _, f := range favorites {
		out = append(out, f.Kind+":"+f.ID)
	}
	return out
}

// reasonIdents is a reason list as the words a feed filter uses for them.
func reasonIdents(reasons []Reason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, string(r))
	}
	return out
}

// paramsText is the saved-query parameters that MOVED, as `key=value` on each
// side.
//
// ONLY THE KEYS THAT MOVED, and both sides of each: a key that gained a value
// appears on the `to` side alone and one that lost it on the `from` side
// alone, so an addition, a removal and a re-pointing are three different
// pictures rather than one. Ordered by key, because a map has no order and two
// nodes must write one string — and the two sides keep that one order, so a
// reader can line them up member for member.
func paramsText(before, after map[string]string) (string, string) {
	keys := make([]string, 0, len(before)+len(after))
	for key, was := range before {
		if after[key] != was {
			keys = append(keys, key)
		}
	}
	for key, now := range after {
		if _, had := before[key]; !had && now != "" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	from := make([]string, 0, len(keys))
	to := make([]string, 0, len(keys))
	for _, key := range keys {
		if was, had := before[key]; had {
			from = append(from, key+"="+was)
		}
		if now, has := after[key]; has {
			to = append(to, key+"="+now)
		}
	}
	return listText(from), listText(to)
}
