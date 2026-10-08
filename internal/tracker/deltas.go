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
)

// WHAT MOVED — one rule, for every object whose apply writes a history row.
//
// # The rule
//
// THE DELTA IS THE APPLIER'S, computed inside the apply's own transaction from
// the two states that frame holds: the object as this node stored it, and the
// object the record says it should be. [Applier.writeHistory] states the same
// thing from the other end, and it is why the notification's own `Fields` is
// never a second producer — a delta derived from a wake is a delta only a LOUD
// commit has, a quiet commit is one that happened without an audience rather
// than one that did not happen, and a wake is built from the snapshot its
// writer read outside the write's transaction, so it can name a move another
// writer had already made. The wake's fields are consulted only where the apply
// has no comparison at all, a record the version guard skipped
// ([statedDeltas]).
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
// # Why nothing is cut, and what a long value carries instead
//
// The row is a log line rather than a snapshot, and this table is never
// swept, so every byte is kept for the life of the company on every node.
// That used to be answered by CUTTING: a side to six hundred bytes, a member
// of a collection to a quarter of that, a collection to what fit with a
// "+N more" count. Each cut kept an opening, which a reader takes for the
// value — and a cut set lost exactly the member the commit added whenever the
// set was large. So nothing here is cut:
//
//   - A SET records what it gained and lost ([deltaSet.set]), whole members,
//     sorted. What one commit changes in a set is small even when the set is
//     not, and the whole set is on the row's document.
//   - An ORDERED LIST — a person's queue, their pinned views, a task's
//     checklists — carries both sides whole: the order is the content, and
//     every one of them is capped in the tens where it is written.
//   - FREE TEXT is carried whole up to [MaxDeltaValue], and past it is
//     described by its SIZE, exactly as a body always has been ([bodyText]):
//     a size is not a fragment anybody can mistake for the text, and the text
//     itself is the mutation, on this row's document column. One member of a
//     list of values takes the same rule at [MaxDeltaElement].
//   - IDENTIFIERS — handles, ids, slugs, paths — are carried whole: each is
//     bounded where it is written, and a size is no use in place of a name.

// MaxDeltaValue is the longest free-text side a history row quotes; a longer
// one is described by its size ([proseText]).
//
// SIX HUNDRED BYTES — [MaxExcerpt]'s own figure, for [MaxExcerpt]'s own
// reason. A delta side is what a reader sees for ONE field on ONE line,
// exactly as an excerpt is what a card shows of a body, and the two are read
// in the same places by the same people. Naming it separately rather than
// spelling the constant inline is what [MaxTagLabel] does with [MaxViewName]:
// the two numbers are equal because the argument is the same, and either may
// move without dragging the other.
const MaxDeltaValue = MaxExcerpt

// MaxDeltaElement is the longest ONE member of a list of values a history row
// quotes — a custom field's value, a saved view's parameter; a longer one is
// described by its size.
//
// A QUARTER OF [MaxDeltaValue], because a list carries many: a write may move
// [MaxFieldValues] custom fields at once, and a saved view's query may put
// 32 KiB in a single parameter ([MaxViewParamsBytes]).
const MaxDeltaElement = MaxDeltaValue / 4

// deltaSet is the fields a comparison found, and the ONE place the rules
// every producer shares are written: a side that reads the same on both ends
// did not move ([deltaSet.add], and [deltaSet.mark] for the fields that carry
// a description instead of a value), and a set records its membership change
// ([deltaSet.set]).
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
// and it exists because some fields carry a DESCRIPTION rather than the value
// — which is the only shape a long one can honestly take in a log line. A
// body delta carries a SIZE and never the prose, so replacing one paragraph
// with another of the same length moves the task and leaves both sizes
// identical; so does any free text past [MaxDeltaValue] ([deltaSet.prose]) and
// a custom-field value past [MaxDeltaElement]. In each the KEY'S PRESENCE is
// the statement that the field moved and the sides say what can be said about
// it — which is strictly more honest than [deltaSet.add]'s silence, and is why
// the caller establishes the move from the VALUES rather than from their
// renderings.
func (d deltaSet) mark(field, from, to string) {
	d[field] = Delta{From: from, To: to}
}

// prose records a free-text field's move: whole up to [MaxDeltaValue], past it
// its size ([proseText]) — established from the values, so two long texts of
// one size still record that they differ.
func (d deltaSet) prose(field, from, to string) {
	if from != to {
		d.mark(field, proseText(from, MaxDeltaValue), proseText(to, MaxDeltaValue))
	}
}

// set records what a SET gained and lost: whole members, each list sorted,
// and nothing when neither moved — so a writer that re-stated the same
// members in another order records no change, which is what it made.
func (d deltaSet) set(field string, before, after []string) {
	added, removed := setMoves(before, after)
	if len(added) > 0 || len(removed) > 0 {
		d[field] = Delta{Added: added, Removed: removed}
	}
}

// setMoves is what after holds that before does not, and the reverse — each
// sorted, each member once.
func setMoves(before, after []string) (added, removed []string) {
	was := make(map[string]bool, len(before))
	for _, member := range before {
		was[member] = true
	}
	now := make(map[string]bool, len(after))
	for _, member := range after {
		now[member] = true
	}
	for member := range now {
		if !was[member] {
			added = append(added, member)
		}
	}
	for member := range was {
		if !now[member] {
			removed = append(removed, member)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

// done is the set as a delta map: NIL when nothing moved, and EVERY field that
// moved otherwise.
//
// A NIL MAP FOR "NOTHING MOVED" is what [Applier.writeHistory]'s length guard
// reads, and it is why that guard exists: `jsonOf` is `json.Marshal` and a nil
// map marshals to `null`, so the column would hold two spellings of empty if
// the nil ever reached it.
//
// NOT TRIMMED. It used to be cut to [MaxDeltas] by field name, here, for the
// history row as well as for a card — so a commit moving more recorded the
// alphabetically first of them and said nothing about the rest, in a table
// nothing repairs. A card is trimmed where it is built ([cardFields]), and
// says by how many.
func (d deltaSet) done() map[string]Delta {
	if len(d) == 0 {
		return nil
	}
	return d
}

// cardFields is what a notification card shows of a change's moves: all of
// them up to [MaxDeltas], past it the first [MaxDeltas] by field name, and how
// many were left off.
//
// BY FIELD NAME, deterministically: a card that showed a different thirty-two
// on two nodes would be one screen disagreeing with another about what
// changed.
func cardFields(all map[string]Delta) (map[string]Delta, int) {
	if len(all) <= MaxDeltas {
		return all, 0
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	shown := make(map[string]Delta, MaxDeltas)
	for _, name := range names[:MaxDeltas] {
		shown[name] = all[name]
	}
	return shown, len(all) - MaxDeltas
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
	case KindFile:
		before, _, err := readFile(ctx, tx, s)
		if err != nil {
			return nil, err
		}
		var after File
		if err := decode(&after); err != nil {
			return nil, err
		}
		return fileDeltas(before, after), nil
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
// default assignee, the target date and the archive flag. A comparison that
// named only one half would be empty for every write of the other.
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
	// FREE TEXT FROM THE ORG CHART, which bounds none of the three: whole
	// up to [MaxDeltaValue], its size past it.
	moved.prose("name", before.Name, after.Name)
	moved.prose("purpose", before.Purpose, after.Purpose)
	moved.prose("unit", before.Unit, after.Unit)
	moved.add("default_assignee", before.DefaultAssignee, after.DefaultAssignee)
	moved.add("target_date", before.TargetDate, after.TargetDate)
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
	moved.set("fields", idents(before.Fields), idents(after.Fields))
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
	moved.add("name", before.Name, after.Name) // [MaxViewName]
	moved.add("type", string(before.Type), string(after.Type))
	moved.add("container", containerText(before.Container), containerText(after.Container))
	moved.add("owner", before.Owner, after.Owner)
	moved.add("protected", boolText(before.Protected), boolText(after.Protected))
	moved.add("default", boolText(before.Default), boolText(after.Default))
	moved.prose("icon", before.Icon, after.Icon)
	moved.add("rank", string(before.Rank), string(after.Rank))
	// THE PARAMETERS THAT MOVED, AS `key=value`, and only those.
	//
	// The whole query is the wrong thing to carry twice: it is bounded at
	// [MaxViewParamsBytes], so a save that narrowed one filter would put
	// tens of kilobytes of unchanged predicate into a log line to show it.
	// The keys alone are the wrong thing too — re-pointing `status` from
	// `open` to `done` changes no key, so the row would be empty for the
	// commonest edit a saved view gets.
	//
	// MARKED rather than compared as text, for [deltaSet.mark]'s reason: a
	// value past [MaxDeltaElement] is carried as its size, so two long
	// values of one size render alike although the parameter moved.
	if from, to, changed := paramsText(before.Params, after.Params); changed {
		moved.mark("params", from, to)
	}
	return moved.done()
}

// fileDeltas is what changed on one file.
//
// THE CONTENT BY ITS HASH AND ITS SIZE, never by its object: a key is a name
// minted for one upload and says nothing a person reading the feed can use —
// two uploads of the same bytes are two keys — while "the hash moved and the
// size went from 12 KiB to 40 KiB" is the whole of "somebody rewrote the
// report".
func fileDeltas(before, after File) map[string]Delta {
	moved := deltaSet{}
	// WHOLE: both are bounded where they are written ([MaxFilePath],
	// [MaxContentType]), and a path is a name, which a size cannot stand in
	// for.
	moved.add("path", before.Path, after.Path)
	moved.add("content_type", before.ContentType, after.ContentType)
	moved.add("hash", string(before.Hash), string(after.Hash))
	moved.add("size", strconv.FormatInt(before.Size, 10), strconv.FormatInt(after.Size, 10))
	moved.add("removed", boolText(before.Removed()), boolText(after.Removed()))
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
// `priorities` AND `pinned_views` ARE ORDERED where the other collections are
// sets, and that is the whole content of each: the list IS the instruction, so
// a re-ordering with the same members is precisely the change somebody made.
// So they carry both sides whole — each is capped at [MaxPriorities] and
// [MaxPinnedViews] — where a set records only what joined and what left.
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
	moved.set("favorites", favoriteIdents(before.Favorites), favoriteIdents(after.Favorites))
	moved.set("primary_reasons", reasonIdents(before.PrimaryReasons),
		reasonIdents(after.PrimaryReasons))
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
	moved.set("tags", idents(before.Tags), idents(after.Tags))
	moved.add("tags_version",
		countText(before.TagsVersion), countText(after.TagsVersion))
	return moved.done()
}

// typeCatalogueDeltas is what changed in the workspace's task types.
func typeCatalogueDeltas(before, after TypeCatalogue) map[string]Delta {
	moved := deltaSet{}
	moved.set("types", idents(before.Types), idents(after.Types))
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
	moved.set("fields", idents(before.Fields), idents(after.Fields))
	moved.add("policy_version",
		countText(before.PolicyVersion), countText(after.PolicyVersion))
	return moved.done()
}

// The text one side of a delta carries, in the one shape every producer uses.

// proseText is one free-text value as a delta side: itself up to limit
// bytes, and past it its SIZE ([bodyText]) — never a fragment of it, which a
// reader would take for the value. The text is the mutation, on the row's
// document column.
func proseText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return bodyText(s)
}

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
// [statelog.ParsePosition] reads back.
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

// listText is an ORDERED collection as the text one side of a delta carries:
// every member, whole, in its order.
//
// ", " IS THE SEPARATOR every delta over a list in this package uses, so one
// renderer splits them all. Only lists whose order is their content take it —
// each is capped in the tens where it is written — and a set takes
// [deltaSet.set] instead, which records what moved rather than both sides.
func listText(items []string) string { return strings.Join(items, ", ") }

// idents is anything this package addresses by a slug, as the slugs.
//
// THE SLUG rather than the label, for the reason the tracker addresses these
// objects by it everywhere else: a label is what a company calls the thing
// this week, and a delta is a durable record of what one record changed.
func idents[T interface{ Ident() string }](items []T) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Ident())
	}
	return out
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
// side, and whether any did.
//
// ONLY THE KEYS THAT MOVED, and both sides of each: a key that gained a value
// appears on the `to` side alone and one that lost it on the `from` side
// alone, so an addition, a removal and a re-pointing are three different
// pictures rather than one. Ordered by key, because a map has no order and two
// nodes must write one string — and the two sides keep that one order, so a
// reader can line them up member for member. A value past [MaxDeltaElement]
// is carried as its size ([proseText]): one parameter may hold 32 KiB.
func paramsText(before, after map[string]string) (string, string, bool) {
	keys := make([]string, 0, len(before)+len(after))
	for key, was := range before {
		if now, has := after[key]; !has || now != was {
			keys = append(keys, key)
		}
	}
	for key, now := range after {
		if _, had := before[key]; !had && now != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return "", "", false
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	from := make([]string, 0, len(keys))
	to := make([]string, 0, len(keys))
	for _, key := range keys {
		if was, had := before[key]; had {
			from = append(from, key+"="+proseText(was, MaxDeltaElement))
		}
		if now, has := after[key]; has {
			to = append(to, key+"="+proseText(now, MaxDeltaElement))
		}
	}
	return listText(from), listText(to), true
}
