package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/inbox"
)

// What moved: one rule for every object whose apply writes a history row
// (package doc, "What a history row's delta carries").
//
// The delta is the applier's, computed in the apply's own transaction from the
// stored object and the record's ([Applier.writeHistory]); a wake's fields are
// read only for a record the version guard skipped ([statedDeltas]). Every
// comparison is pure over two values; only [documentDeltas] reads. A side is the
// log's own text, never a rendering: a relation names the other task by its ID,
// because a key is a fact about another task's row. Nothing is cut: a set
// records what it gained and lost, an ordered list carries both sides whole,
// free text past [MaxDeltaValue] is its size, and an identifier is whole.

// MaxDeltaValue is the longest free-text side a history row quotes; a longer
// one is described by its size ([proseText]). It is [MaxExcerpt]'s figure for
// [MaxExcerpt]'s reason (one field on one line, read where an excerpt is),
// named apart so either may move without dragging the other.
const MaxDeltaValue = MaxExcerpt

// MaxDeltaElement is the longest one member of a list of values a history row
// quotes (a custom field's value, a saved view's parameter); a longer one is
// described by its size. A quarter of [MaxDeltaValue] because a list carries
// many: a write may move [MaxFieldValues] custom fields at once, and one view
// parameter may hold 32 KiB ([MaxViewParamsBytes]).
const MaxDeltaElement = MaxDeltaValue / 4

// deltaSet is the fields a comparison found, and the one place the shared
// rules live: a side that reads the same on both ends did not move
// ([deltaSet.add]; [deltaSet.mark] for fields carrying a description), and a
// set records its membership change ([deltaSet.set]). One type rather than a
// closure per function, so seven producers cannot drift apart.
type deltaSet map[string]Delta

// add records one field's move, and ignores one that did not happen.
func (d deltaSet) add(field, from, to string) {
	if from != to {
		d.mark(field, from, to)
	}
}

// mark records a move the caller has already established, whatever the two
// sides read: the one exception to [deltaSet.add]'s rule, for fields carrying a
// description rather than the value (a body's size, free text past
// [MaxDeltaValue], a custom-field value past [MaxDeltaElement]). Two
// descriptions can read alike although the value moved, so the key's presence
// says it moved, and the caller decides that from the values, never from their
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

// set records what a set gained and lost: whole members, each list sorted, and
// nothing when neither moved, so re-stating the same members in another order
// records no change.
func (d deltaSet) set(field string, before, after []string) {
	added, removed := setMoves(before, after)
	if len(added) > 0 || len(removed) > 0 {
		d[field] = Delta{Added: added, Removed: removed}
	}
}

// count records that a set moved as how many members it held before and
// after, for a set whose members a history row must not name.
func (d deltaSet) count(field string, before, after []string) {
	added, removed := setMoves(before, after)
	if len(added) > 0 || len(removed) > 0 {
		d[field] = Delta{From: strconv.Itoa(len(before)), To: strconv.Itoa(len(after))}
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

// done is the set as a delta map: nil when nothing moved, every moved field
// otherwise. Nil is what [Applier.writeHistory]'s length guard reads: `jsonOf`
// marshals a nil map to `null`, so the guard keeps the column to one spelling
// of empty. Never trimmed: the history row keeps every field, and a card is
// trimmed where it is built ([cardFields]).
func (d deltaSet) done() map[string]Delta {
	if len(d) == 0 {
		return nil
	}
	return d
}

// cardFields is what a notification card shows of a change's moves: all of
// them up to [MaxDeltas], past it the fields a route reads and then the rest by
// field name up to [MaxDeltas], and how many were left off. By name so two
// nodes' cards agree; routed fields ([routedFields]) first because
// [Candidates] decides a hand-off and the finished edge on them
// ([Notify.moved]), so the limit bounds only what a card displays.
func cardFields(all map[string]Delta) (map[string]Delta, int) {
	if len(all) <= MaxDeltas {
		return all, 0
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		if routedA, routedB := slices.Contains(routedFields, a), slices.Contains(routedFields, b); routedA != routedB {
			if routedA {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	shown := make(map[string]Delta, MaxDeltas)
	for _, name := range names[:MaxDeltas] {
		shown[name] = all[name]
	}
	return shown, len(all) - MaxDeltas
}

// routedFields are the deltas [Candidates] reads a task's change by
// ([Notify.moved]), which a card keeps whatever else it trims ([cardFields]).
var routedFields = []string{"assignee", "status"}

// documentDeltas is what one record moved on a whole-document object.
//
// The stored document is read in the apply's own transaction, before the
// upsert replaces it: the only frame holding both states (the one
// [Applier.applyTask] reads `current` in). That costs one indexed read and
// decode per document apply, paid only on these rarely written subjects; the
// hot task apply does not come through here.
//
// A catalogue name this build does not know yields no deltas rather than an
// error: a newer peer may declare one, and refusing it would stall the fleet's
// log over a column that describes a change rather than makes it.
func documentDeltas(ctx context.Context, tx *sql.Tx, s Subject,
	mutation json.RawMessage) (map[string]Delta, error) {

	decode := func(into any) error {
		if err := decodePayload(mutation, into); err != nil {
			return fmt.Errorf("worklet: decode the %s payload to compare it "+
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
	return nil, fmt.Errorf("worklet: %s is not a whole-document object, so "+
		"there are no two documents to compare", s.Kind)
}

// projectDeltas is what changed between two versions of a project.
//
// It compares the chart-owned fields and the lead-owned ones together, since
// one record may carry either half. `chart_epoch` is recorded because a
// re-declaration at a new activation moves only it ([Writer.applyChartProject]
// publishes on an epoch change alone): without it every activation would write
// an empty `project_updated` row per project. `created_at` and `updated_at` are
// not: the history row's own `created_at` and `effective_at` say when.
func projectDeltas(before, after Project) map[string]Delta {
	moved := deltaSet{}
	// Free text the org chart does not bound ([deltaSet.prose]).
	moved.prose("name", before.Name, after.Name)
	moved.prose("purpose", before.Purpose, after.Purpose)
	moved.prose("unit", before.Unit, after.Unit)
	moved.add("default_assignee", before.DefaultAssignee, after.DefaultAssignee)
	moved.add("target_date", before.TargetDate, after.TargetDate)
	moved.add("stuck_after_hours", windowText(before.StuckAfterHours),
		windowText(after.StuckAfterHours))
	moved.add("archived", boolText(before.Archived), boolText(after.Archived))
	moved.add("policy_version",
		countText(before.PolicyVersion), countText(after.PolicyVersion))
	moved.add("chart_epoch", strconv.FormatInt(before.ChartEpoch, 10),
		strconv.FormatInt(after.ChartEpoch, 10))
	// The declarations by slug; an edit that keeps the slugs (a relabel, an
	// archive, a new option) moves `policy_version` instead, which
	// [applyProjectEdit] advances on a fields edit and nothing else.
	moved.set("fields", idents(before.Fields), idents(after.Fields))
	return moved.done()
}

// viewDeltas is what changed between two versions of a saved view.
//
// `rank` is recorded, unlike a task's: a task's rank moves on a record of its
// own that is not history, while a view's rides the ordinary save, so a
// rearranged strip's `view_saved` row would otherwise say nothing.
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
	// Only the parameters that moved, as `key=value` ([paramsText]): the
	// whole query (up to [MaxViewParamsBytes]) is too much to carry twice,
	// and keys alone miss a re-pointed filter. Marked, for [deltaSet.mark]'s
	// reason.
	if from, to, changed := paramsText(before.Params, after.Params); changed {
		moved.mark("params", from, to)
	}
	return moved.done()
}

// fileDeltas is what changed on one file.
//
// The content is recorded by its hash and size, never its object key: a key is
// minted per upload and tells a reader nothing (two uploads of the same bytes
// are two keys).
func fileDeltas(before, after File) map[string]Delta {
	moved := deltaSet{}
	// Whole: both are bounded where written ([MaxFilePath],
	// [MaxContentType]), and a size cannot stand in for a name.
	moved.add("path", before.Path, after.Path)
	moved.add("content_type", before.ContentType, after.ContentType)
	moved.add("hash", string(before.Hash), string(after.Hash))
	moved.add("size", strconv.FormatInt(before.Size, 10), strconv.FormatInt(after.Size, 10))
	moved.add("removed", boolText(before.Removed()), boolText(after.Removed()))
	return moved.done()
}

// personDeltas is what changed on one person's own record.
//
// The three inbox lists are counted, not listed: each holds up to
// [inbox.MaxEntries] record ids and positions nobody reads, and every gesture
// that writes them moves an entry between them, so the counts say what
// happened. `priorities` and `pinned_views` are ordered, the order being the
// instruction, so both sides are carried whole (capped at [MaxPriorities] and
// [MaxPinnedViews]). `priorities_set_at` is not recorded, though
// `priorities_set_by` is: the row's own instant says when, and a stamp moving
// on every write would make re-stating an unchanged list look like a change.
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

// tagSetDeltas is what changed in a project's tag set. `tags_version` is
// recorded beside the slugs because renaming or recolouring a tag moves no
// slug, and the counter is then the only witness.
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
// `policy_version` moves on every fields edit ([Writer.WriteFields]), so it
// records a reshaped declaration the slug list misses: a renamed dropdown, a
// new option, an archive.
func fieldCatalogueDeltas(before, after FieldCatalogue) map[string]Delta {
	moved := deltaSet{}
	moved.set("fields", idents(before.Fields), idents(after.Fields))
	moved.add("policy_version",
		countText(before.PolicyVersion), countText(after.PolicyVersion))
	return moved.done()
}

// The text one side of a delta carries, in the one shape every producer uses.

// proseText is one free-text value as a delta side: itself up to limit bytes,
// past it its size ([bodyText]), never a fragment a reader would take for the
// value.
func proseText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return bodyText(s)
}

// boolText is a flag as text. `false` is written out rather than the empty
// string a renderer draws as an em dash: "— → true" would read as a field that
// had no value before.
func boolText(b bool) string { return strconv.FormatBool(b) }

// countText is a number as text, zero included: unlike [minutesText]'s
// estimate, where zero means unsized, a zero count is a fact.
func countText(n int) string { return strconv.Itoa(n) }

// windowText is a project's stuck window as a delta reads it: empty for none
// set, which is the default rather than a window of zero hours.
func windowText(hours int) string {
	if hours == 0 {
		return ""
	}
	return strconv.Itoa(hours)
}

// containerText is where an object lives, in the container grammar's two-part
// form. The workspace carries no ID, so it is the bare word rather than
// `workspace:`, a second spelling of the top of the company.
func containerText(c Container) string {
	if c.ID == "" {
		return c.Kind
	}
	return c.Kind + ":" + c.ID
}

// positionText is an inbox's read mark, in the `stream@generation:seq` form
// [statelog.ParsePosition] reads back. An unset mark is the empty string
// rather than `@0:0`: nothing read is no position, drawn as an em dash.
func positionText(p inbox.Position) string {
	if p.Seq == 0 {
		return ""
	}
	return fmt.Sprintf("%s@%d:%d", p.Stream, p.Generation, p.Seq)
}

// listText is an ordered collection as one delta side: every member, whole, in
// order, joined by ", ", the separator every list delta here uses so one
// renderer splits them all. Only a list whose order is its content takes it; a
// set takes [deltaSet.set].
func listText(items []string) string { return strings.Join(items, ", ") }

// idents is anything this package addresses by a slug, as the slugs: a label is
// what a company calls the thing this week, and a delta is a durable record.
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

// paramsText is the saved-query parameters that moved, as `key=value` on each
// side, and whether any did. A key that gained a value appears on the `to`
// side alone and one that lost it on the `from` side alone, so an addition, a
// removal and a re-pointing read differently. Both sides are ordered by key, so
// every node writes one string and a reader can line them up; a value past
// [MaxDeltaElement] is its size ([proseText]).
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
