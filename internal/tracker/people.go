package tracker

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/seatnames"
)

// A PERSON IS WRITTEN AND READ BY THEIR SEAT'S IDENTITY, and shown by the
// handle the seat answers to now.
//
// # Why not by the handle
//
// A seat's identity is the handle it was CREATED under (ADR-0019): a rename
// moves the address people type and keeps the seat — its id, its mailbox, its
// lease, its memory. This domain stored the handle a person answered to AT THE
// WRITE, in every column that names somebody: the assignee, the reporter, the
// watchers and collaborators, a checklist item's owner, a comment's author,
// whose inbox, pins and priority list a person record is, a view's owner, a
// project's default assignee. And `my_work`, `work_inbox` and every filter
// asked for the handle the caller answered to at the READ. So a renamed seat's
// queue, inbox, priorities and pins went empty the moment the chart moved,
// its own comments stopped being its own to edit, and the work it held sat
// under an address nobody asked for.
//
// So every one of those values is the seat's IDENTITY — what [Identities]
// answers for any handle the seat answers to, its current one, the one it was
// created under or one a rename retired — and every read shows it back as the
// handle the seat answers to now. For every seat that was never renamed the
// identity IS its handle, so every row already written is keyed correctly and
// nothing is migrated. A value no seat answers to — a person's login, a Tier A
// token, the node a duty ran on, a seat since removed — is kept exactly as
// written, both ways.
//
// # Where, and where not
//
// AT THE WRITER, before its decide reads anything, because a decide compares a
// caller's value with the rows (is this person already watching, is this their
// own comment, their own record) and a comparison across two spellings of one
// seat is a comparison that fails. And AT THE READER, on the question on the
// way in and the answer on the way out. NEVER AT THE APPLIER: the identity is
// the chart's to say, and an applier that read a chart would write different
// rows on two nodes briefly on different epochs — which is why the RECORD
// carries the identity, resolved once, by the writer.
//
// # Which fields
//
// A field that names somebody says so on its declaration, with the struct tag
// `person:"seat"` ([seatnames.Tag]), and [people] rewrites exactly those: a
// string, a pointer to one, a list or a pointer to a list — a list
// deduplicated, since a set naming one seat under two spellings is one member.
// The tag is the classification, in the one place a new field is written,
// rather than a list somewhere beside the types that the next field would not
// reach; `TestEveryFieldThatNamesSomebodyIsTagged` holds the obvious names to
// it. Three shapes a tag cannot state are this domain's own: a change record's
// text deltas ([personDeltaKeys]), a custom field's value when its declared
// type is `people` ([FieldValue]) — the walker's two arms — and a grouped
// answer's column key when the axis is a person (the reader's own).

// Identities is the chart seam a person is written and read through.
//
// CONSUMER-DEFINED, as [Leads] and [FieldWorld] are: this package holds no
// org chart, and the engine answers from its live epoch.
type Identities interface {
	// Identity is the handle the seat answering to handle was created
	// under, for any handle it answers to, and handle itself when no seat
	// answers to it.
	Identity(handle string) string

	// Current is the handle the seat created under identity answers to
	// now, and identity itself when no seat was.
	Current(identity string) string
}

// identified is v with every person it names rewritten to their identity. A
// nil seam leaves v as it is, which is a build holding no chart.
func identified[T any](ids Identities, v T) T {
	if ids == nil {
		return v
	}
	return seatnames.Rewrite(people, v, ids.Identity)
}

// shown is v with every person it names rewritten to the handle they answer
// to now.
func shown[T any](ids Identities, v T) T {
	if ids == nil {
		return v
	}
	return seatnames.Rewrite(people, v, ids.Current)
}

// identityOf is one handle's identity, through a seam that may be nil.
func identityOf(ids Identities, handle string) string {
	if ids == nil || handle == "" {
		return handle
	}
	return ids.Identity(handle)
}

// currentOf is one identity's current handle, through a seam that may be nil.
func currentOf(ids Identities, identity string) string {
	if ids == nil || identity == "" {
		return identity
	}
	return ids.Current(identity)
}

// people is this domain's walker over every value that names somebody — see
// internal/seatnames, which holds the rule for the tag and does the walking.
//
// TWO SHAPES A TAG CANNOT STATE are its arms: a change record's text deltas,
// whose field NAME says which text is a person ([personDeltaKeys]), and a
// custom field's value, which names people only when its declared type is
// `people` ([FieldValue.withPeople]).
var people = seatnames.NewWalker(
	seatnames.Arm{
		Type: reflect.TypeFor[map[string]Delta](),
		Rewrite: func(v reflect.Value, f func(string) string) reflect.Value {
			return reflect.ValueOf(deltasOf(v.Interface().(map[string]Delta), f))
		},
	},
	seatnames.Arm{
		Type: reflect.TypeFor[FieldValue](),
		Rewrite: func(v reflect.Value, f func(string) string) reflect.Value {
			return reflect.ValueOf(v.Interface().(FieldValue).withPeople(f))
		},
	},
)

// personDeltaKeys are the change-record fields whose TEXT names people, and
// whether it names one or a list ([listText]).
//
// A delta is text rather than a typed value — see [Delta] — so the tag cannot
// reach it; the field name is what says what the text is.
var personDeltaKeys = map[string]bool{
	"assignee": false, "reporter": false, "owner": false,
	"default_assignee": false, "priorities_set_by": false,
	"watchers": true, "muted": true, "collaborators": true,
}

// deltasOf is a delta map with its person-valued text rewritten.
func deltasOf(in map[string]Delta, f func(string) string) map[string]Delta {
	if in == nil {
		return nil
	}
	out := make(map[string]Delta, len(in))
	for key, delta := range in {
		if list, named := personDeltaKeys[key]; named {
			delta = Delta{From: deltaText(delta.From, list, f),
				To: deltaText(delta.To, list, f)}
		}
		out[key] = delta
	}
	return out
}

// deltaText rewrites one side of a person delta: the whole text for one
// person, each member of a [listText] for a list — its `+N more` tail kept as
// it is, since it is a count rather than a name.
func deltaText(text string, list bool, f func(string) string) string {
	if !list {
		return f(text)
	}
	if text == "" {
		return text
	}
	members := strings.Split(text, ", ")
	for i, member := range members {
		if strings.HasPrefix(member, "+") && strings.HasSuffix(member, " more") {
			continue
		}
		members[i] = f(member)
	}
	return strings.Join(members, ", ")
}

// withPeople is a custom field's value with its people rewritten, when its
// declared type says the value names them.
//
// ONE PERSON OR A LIST OF THEM, because a people field is [MultiValued]: the
// write stores a single member as itself and several as a JSON list
// ([coerceMany]), so reading only the first shape left every task naming two
// colleagues in one field showing a renamed seat by its identity. A list is a
// set, and two spellings of one seat in it are one member — [mapNames]' rule.
// A value in neither shape is this build's to leave alone rather than guess at.
func (v FieldValue) withPeople(f func(string) string) FieldValue {
	if v.Type != FieldPeople || len(v.Value) == 0 {
		return v
	}
	var rewritten any
	var handle string
	var handles []string
	switch {
	case json.Unmarshal(v.Value, &handle) == nil:
		rewritten = f(handle)
	case json.Unmarshal(v.Value, &handles) == nil && handles != nil:
		rewritten = seatnames.Names(handles, f)
	default:
		return v
	}
	encoded, err := json.Marshal(rewritten)
	if err != nil {
		return v
	}
	v.Value = encoded
	return v
}

// identifiedByType is a query with the person values only its catalogue can
// name rewritten to identities: every people field's filter values, and the
// column a board pages when it is grouped by a person — the assignee, or a
// people field.
//
// INSIDE THE READ, because a field's type is a declaration the read's own
// snapshot resolves ([resolveFields]); every other person in a question is
// tagged and rewritten before the read begins.
func identifiedByType(ids Identities, q Query, fields map[string]resolvedField) Query {
	if ids == nil {
		return q
	}
	identity := seatnames.Name(ids.Identity)
	if len(q.Fields) > 0 {
		filters := slices.Clone(q.Fields)
		for i, filter := range filters {
			field, held := fields[filter.Ref]
			if !held || field.Type != FieldPeople || filter.Value == "" {
				continue
			}
			members := strings.Split(filter.Value, ",")
			for j, member := range members {
				members[j] = identity(strings.TrimSpace(member))
			}
			filters[i].Value = strings.Join(members, ",")
		}
		q.Fields = filters
	}
	if q.Group != nil && personAxis(q.GroupBy, fields) {
		group := identity(*q.Group)
		q.Group = &group
	}
	if q.Subgroup != nil && personAxis(q.GroupBy2, fields) {
		subgroup := identity(*q.Subgroup)
		q.Subgroup = &subgroup
	}
	if len(q.Any) > 0 {
		branches := make([]Query, len(q.Any))
		for i, branch := range q.Any {
			branches[i] = identifiedByType(ids, branch, fields)
		}
		q.Any = branches
	}
	return q
}

// personAxis reports whether a grouping's columns are people.
func personAxis(key string, fields map[string]resolvedField) bool {
	if key == "assignee" {
		return true
	}
	ref, ok := strings.CutPrefix(key, FieldKeyPrefix)
	if !ok {
		return false
	}
	field, held := fields[ref]
	return held && field.Type == FieldPeople
}

// shownGroups is a grouped answer with its person columns keyed by the handle
// each seat answers to now — the same spelling as the rows beneath them, so a
// board matches a card to its column and pages the column by the key it drew.
func shownGroups(ids Identities, groups []Group, columns, lanes bool) []Group {
	if ids == nil || (!columns && !lanes) {
		return groups
	}
	current := seatnames.Name(ids.Current)
	out := make([]Group, len(groups))
	for i, group := range groups {
		if columns {
			group.Key = current(group.Key)
		}
		if lanes && len(group.Subgroups) > 0 {
			group.Subgroups = shownGroups(ids, group.Subgroups, true, false)
		}
		out[i] = group
	}
	return out
}

// fieldWorld is the writer's people-field resolution, answering the seat's
// IDENTITY: a people field is a column a filter compares, exactly like an
// assignee, and the chart resolves a name to the handle the seat answers to
// now.
func (w *Writer) fieldWorld() FieldWorld {
	if w.World == nil || w.Identities == nil {
		return w.World
	}
	return identifiedWorld{world: w.World, ids: w.Identities}
}

// identifiedWorld is a [FieldWorld] whose every answer is an identity.
type identifiedWorld struct {
	world FieldWorld
	ids   Identities
}

// ResolveSeat implements [FieldWorld].
func (i identifiedWorld) ResolveSeat(ref string) (string, bool) {
	handle, held := i.world.ResolveSeat(ref)
	if !held {
		return handle, held
	}
	return identityOf(i.ids, handle), true
}
