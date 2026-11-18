package config

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/secrets"
)

// What a write changes in the company document, judged object by object.
//
// Leads write the org chart as well as admin (internal/authz's subtree rule): a
// lead may change what sits inside a unit they lead and nothing else. So a
// write is judged as the seats and units it adds, removes, moves, reorders and
// edits, and for each the PLACES it reaches: every unit that must be inside the
// writer's subtree for the change to be theirs ([DiffOrg]). Whether they are is
// the authority table's to decide (internal/api/configapi). Only what a write
// CHANGES is judged: a reference or key the object already carried was somebody
// else's decision, and rejudging it would make an admin-wired team uneditable
// by its own lead. PURE over two documents, never the running company, so a
// node behind on applies decides as a current one does.

// OrgKind is what an org change is about.
type OrgKind string

const (
	// OrgSeat is a seat, addressed by its handle.
	OrgSeat OrgKind = "seat"
	// OrgUnit is a unit, addressed by its key.
	OrgUnit OrgKind = "unit"
)

// OrgOp is what a write did to one seat or unit.
type OrgOp string

const (
	// OrgAdded is an object the after document holds and the before one
	// does not.
	OrgAdded OrgOp = "added"
	// OrgRemoved is the reverse.
	OrgRemoved OrgOp = "removed"
	// OrgMoved is an object whose place changed — a seat's unit, a unit's
	// parent — and perhaps its fields with it.
	OrgMoved OrgOp = "moved"
	// OrgChanged is an object that stayed in its place and changed: its
	// fields, or where it sits among its siblings.
	OrgChanged OrgOp = "changed"
)

// OrgSide is which of the two documents a place is read in.
type OrgSide string

const (
	// OrgBefore is the document a write replaces.
	OrgBefore OrgSide = "before"
	// OrgAfter is the document it proposes.
	OrgAfter OrgSide = "after"
)

// OrgDiff is everything a write changes, as the subtree rule judges it.
type OrgDiff struct {
	// Changes is every seat and unit the write adds, removes, moves or
	// edits, seats before units and each sorted by id.
	Changes []OrgChange

	// Settings is every top-level key outside `roles` and `units` whose
	// value differs, sorted.
	Settings []string

	// Widened is every MCP limit the write lifts or loosens for an agent
	// seat both documents hold, by seat, then server, then tool
	// ([OrgWidening]). A lead narrows a server and never widens one: lifting a
	// limit is a grant, admin's. Seats are compared as held, their own block
	// and every enclosing unit's together ([org.Role.Limits]), since moving a
	// seat out of its unit frees it of a limit as surely as editing the block.
	Widened []OrgWidening

	// Before and After are the two organizations every place was read in,
	// so a decision about a place reads the very tree it was found in.
	Before, After *org.Organization
}

// OrgChange is one seat or unit a write changes.
type OrgChange struct {
	Kind OrgKind
	// ID is the seat's handle or the unit's key.
	ID string
	Op OrgOp

	// Fields is every one of its own fields whose value differs, by JSON
	// name, sorted; empty for an addition and a removal. A unit's seats and
	// child units are not its fields: each is an object of its own.
	Fields []string

	// Credentials is every credential field whose value the change sets,
	// clears or alters — compared whole, its keys included — and every
	// `${VAR}` reference it sets, clears or alters in any other field, by
	// its path inside the object, sorted. Never a lead's: a `${VAR}` names any
	// variable the process can resolve, and a human seat's `email` and contact
	// identities resolve one, recited by `lookup_colleague` and the prompt
	// roster, so a lead could read a secret back as their own Slack id. A
	// setting outside `roles` and `units` is listed apart for the same reason.
	Credentials []string

	// Keys is every field another system finds the object by ([keyed])
	// that the change sets, clears or alters, by JSON name, sorted.
	Keys []string

	// Grants is every field carrying a role grant ([granting]) that the
	// change sets, clears or alters, by JSON name, sorted.
	Grants []string

	// Touches is every place the change reaches.
	Touches []OrgTouch
}

// OrgTouch is one place a change reaches: where an object is, and also what a
// new reference names, after, and who already names a new object, before.
// `lead:` and `manages:` are what authority derives from, so a lead naming an
// outsider as a sub-team's lead, or making their report the CEO's manager,
// would reach outside their subtree; a reference naming nothing reaches the
// root, which is nobody's. A reference may dangle, and one naming a unit's key
// resolves to a seat of that handle first, so a seat added as `ghost` would
// lead an outside team whose `lead: ghost` dangled.
type OrgTouch struct {
	Side OrgSide
	// Unit is the key of the unit the place is: the unit holding a seat,
	// the parent of a unit, the unit a reference names, the unit a referrer
	// is or sits in. Empty for the company root, which a seat or unit at
	// the top of the company sits in and a reference naming nothing reaches.
	Unit string
	// Why is what about the change reaches it: `place`, `self`, `lead`,
	// `manages`, `named` for another object's reference that already states
	// the id an added object takes, or `duplicate` for two objects on one id.
	Why string
	// Value is the reference or the id, as the change stated it; empty for
	// `place` and `self`.
	Value string
}

// OrgWidening is one MCP limit a write lifts or loosens for one agent seat: a
// server it turned off is on for the seat again, or one of its tools is held
// to less than it was.
type OrgWidening struct {
	// Seat is the agent seat's handle.
	Seat string
	// Server is the server's name, and Tool the server's own name for the
	// tool, "" where the server itself is turned back on.
	Server, Tool string
}

// Field is where the limit is, as a refusal names it:
// `mcp_limits.<server>.enabled` or `mcp_limits.<server>.tools.<tool>`, read as
// the seat is held, whichever block held it.
func (w OrgWidening) Field() string {
	if w.Tool == "" {
		return "mcp_limits." + w.Server + ".enabled"
	}
	return "mcp_limits." + w.Server + ".tools." + w.Tool
}

// DiffOrg reports what turning before into after changes. A nil before is a
// document with nothing in it. The places it reaches:
//   - where an object is, before and after (a seat's unit, a unit's parent): a
//     lead cannot remove or move the top unit they lead;
//   - where it sits among the siblings both documents hold there: order decides
//     [org.Organization.Manager] (the first manager in document order), so a
//     reorder reaches the unit the objects sit in, never the top level;
//   - a unit itself, when its fields change: handing it to another `lead:`
//     fails the AFTER side unless they also lead the unit above.
func DiffOrg(before, after *Company) OrgDiff {
	if before == nil {
		before = &Company{}
	}
	if after == nil {
		after = &Company{}
	}
	b, a := survey(before), survey(after)
	b.rankAgainst(a)
	a.rankAgainst(b)
	diff := OrgDiff{Settings: settingsChanged(before, after), Before: b.org, After: a.org,
		Widened: widened(b.org, a.org, after)}

	for _, kind := range []OrgKind{OrgSeat, OrgUnit} {
		ids := map[string]bool{}
		for id := range b.objects[kind] {
			ids[id] = true
		}
		for id := range a.objects[kind] {
			ids[id] = true
		}
		for _, id := range slices.Sorted(maps.Keys(ids)) {
			if change, changed := diffObject(kind, id, b, a); changed {
				diff.Changes = append(diff.Changes, change)
			}
		}
	}
	return diff
}

// diffObject is one seat's or unit's change, and whether it changed at all.
func diffObject(kind OrgKind, id string, b, a *surveyed) (OrgChange, bool) {
	was, had := b.objects[kind][id]
	is, has := a.objects[kind][id]
	change := OrgChange{Kind: kind, ID: id}

	// TWO OBJECTS ANSWERING TO ONE ID cannot be judged object by object —
	// which of them is the one a change is about is not a fact either side
	// states — so the change is placed at the root, which only admin
	// writes. Only a stored revision can carry one (the admission
	// rules refuse it in anything submitted), so this reaches a revert.
	if was.count > 1 || is.count > 1 {
		change.Op = OrgChanged
		change.Touches = []OrgTouch{{Side: OrgAfter, Why: "duplicate", Value: id}}
		return change, true
	}

	switch {
	case !had:
		change.Op = OrgAdded
		change.Touches = []OrgTouch{{Side: OrgAfter, Unit: is.place, Why: "place"}}
		for _, by := range b.referrers[id] {
			change.Touches = append(change.Touches, OrgTouch{
				Side: OrgBefore, Unit: by.place, Why: "named", Value: id,
			})
		}
	case !has:
		change.Op = OrgRemoved
		change.Touches = []OrgTouch{{Side: OrgBefore, Unit: was.place, Why: "place"}}
		return change, true
	default:
		change.Fields = fieldsChanged(was.document, is.document)
		moved := was.place != is.place
		reordered := !moved && b.rank[kind][id] != a.rank[kind][id]
		if !moved && !reordered && len(change.Fields) == 0 {
			return change, false
		}
		change.Op = OrgChanged
		if moved {
			change.Op = OrgMoved
		}
		if kind == OrgSeat || moved || reordered {
			change.Touches = append(change.Touches,
				OrgTouch{Side: OrgBefore, Unit: was.place, Why: "place"},
				OrgTouch{Side: OrgAfter, Unit: is.place, Why: "place"})
		}
		if kind == OrgUnit && len(change.Fields) > 0 {
			change.Touches = append(change.Touches,
				OrgTouch{Side: OrgBefore, Unit: id, Why: "self"},
				OrgTouch{Side: OrgAfter, Unit: id, Why: "self"})
		}
	}

	change.Credentials = credentialsChanged(was.credentials, is.credentials)
	for _, field := range fieldsChanged(was.document, is.document) {
		if slices.Contains(keyed[kind], field) {
			change.Keys = append(change.Keys, field)
		}
		if slices.Contains(granting[kind], field) {
			change.Grants = append(change.Grants, field)
		}
	}
	for _, ref := range is.refs {
		if !slices.Contains(was.refs, ref) {
			change.Touches = append(change.Touches, OrgTouch{
				Side: OrgAfter, Unit: a.placeOfRef(ref), Why: ref.why, Value: ref.to,
			})
		}
	}
	return change, true
}

// keyed is every field, by JSON name, another system finds an object of each
// kind by, so a lead never sets one: a project's or space's lead is whoever
// leads the unit declaring it (internal/authz/orgchart), a channel routes to
// its unit, a vendor attributes by address or account id, and who else holds
// the key is not a fact two documents state. A project outlives its unit in
// the tracker, so a lead declaring an orphan as their team's became its lead.
// A removal takes its keys with it, judged by where the object was: what it
// declared stays orphaned, which only admin may declare again.
var keyed = map[OrgKind][]string{
	OrgSeat: {"contact", "email", "project", "space"},
	OrgUnit: {"channel", "project", "space"},
}

// granting is every field, by JSON name, that carries a ROLE GRANT on an object
// of each kind: authority beyond the chart's subtree, which only admin sets.
// `health_tools` admits a seat to the administrator's reads of the deployment
// (internal/authz's seat grant), so a lead setting it on a team seat would read
// fleet health through it. Clearing is listed too: whether a seat watches the
// deployment is admin's decision either way.
var granting = map[OrgKind][]string{
	OrgSeat: {"health_tools"},
}

// surveyed is one document, read for a diff.
type surveyed struct {
	org *org.Organization
	// objects is every seat by handle and unit by key.
	objects map[OrgKind]map[string]object
	// referrers is who states each name in a `lead:` or `manages:` entry,
	// whether or not anything answers to it.
	referrers map[string][]referrer
	// seatPlaces is the unit holding each seat, by handle; "" for the root.
	seatPlaces map[string]string
	// siblings is every place's seats and child units, by id, in document
	// order; "" for the root.
	siblings map[OrgKind]map[string][]string
	// rank is where each object sits among the siblings the other document
	// also holds at its place ([surveyed.rankAgainst]).
	rank map[OrgKind]map[string]int
}

// object is one seat or unit as a diff reads it.
type object struct {
	// count is how many objects of the document answer to its id.
	count int
	// place is the unit holding a seat, or the parent of a unit; "" for the
	// root.
	place string
	// document is its own fields, encoded: a unit's without its seats and
	// child units.
	document []byte
	// credentials is every credential value it holds and every string
	// carrying a `${VAR}`, by path.
	credentials map[string]string
	// refs is every reference it states to another object.
	refs []reference
}

// reference is a name one object states for another: a unit's `lead:`, a
// seat's `manages:` entry.
type reference struct {
	why string
	to  string
}

// referrer is an object stating a reference, and the place it reaches.
type referrer struct {
	kind  OrgKind
	id    string
	place string
}

// survey reads one document.
func survey(c *Company) *surveyed {
	o, _ := c.organization()
	s := &surveyed{
		org:        o,
		objects:    map[OrgKind]map[string]object{OrgSeat: {}, OrgUnit: {}},
		referrers:  map[string][]referrer{},
		seatPlaces: map[string]string{},
		siblings:   map[OrgKind]map[string][]string{OrgSeat: {}, OrgUnit: {}},
	}
	unitParents := map[string]string{}
	placeSeats := func(roles []*org.Role, place string) {
		for _, r := range roles {
			if _, seen := s.seatPlaces[r.Handle()]; !seen {
				s.seatPlaces[r.Handle()] = place
				s.siblings[OrgSeat][place] = append(s.siblings[OrgSeat][place], r.Handle())
			}
		}
	}
	var walk func(units []*org.Unit, parent string)
	walk = func(units []*org.Unit, parent string) {
		for _, u := range units {
			key := u.Key()
			if _, seen := unitParents[key]; !seen {
				unitParents[key] = parent
				s.siblings[OrgUnit][parent] = append(s.siblings[OrgUnit][parent], key)
			}
			placeSeats(u.Roles, key)
			walk(u.Children, key)
		}
	}
	walk(o.Units, "")
	placeSeats(o.Roles, "")

	for role := range c.EachRole() {
		handle := role.IdentityKey()
		seen := s.objects[OrgSeat][handle]
		seen.count++
		if seen.count == 1 {
			seen.place = s.seatPlaces[handle]
			seen.document = encodeFields(role)
			seen.credentials = credentialsOf(role)
			for _, entry := range role.Manages {
				seen.refs = append(seen.refs, reference{why: "manages", to: strings.TrimSpace(entry)})
			}
		}
		s.objects[OrgSeat][handle] = seen
	}
	for unit := range c.EachUnit() {
		key := unit.IdentityKey()
		seen := s.objects[OrgUnit][key]
		seen.count++
		if seen.count == 1 {
			own := *unit
			own.Roles, own.Children = nil, nil
			seen.place = unitParents[key]
			seen.document = encodeFields(&own)
			seen.credentials = credentialsOf(&own)
			if lead := strings.TrimSpace(unit.Lead); lead != "" {
				seen.refs = append(seen.refs, reference{why: "lead", to: lead})
			}
		}
		s.objects[OrgUnit][key] = seen
	}
	for kind, objects := range s.objects {
		for id, o := range objects {
			place := o.place
			if kind == OrgUnit {
				place = id
			}
			for _, ref := range o.refs {
				s.referrers[ref.to] = append(s.referrers[ref.to], referrer{kind: kind, id: id, place: place})
			}
		}
	}
	return s
}

// rankAgainst sets where each object sits among the siblings of its kind that
// other holds at the same place: a sibling only one document holds there — an
// addition, a removal, a move in or out — is not counted, so the ranks of the
// rest differ between the two documents only where their order does.
func (s *surveyed) rankAgainst(other *surveyed) {
	s.rank = map[OrgKind]map[string]int{OrgSeat: {}, OrgUnit: {}}
	for kind, places := range s.siblings {
		for place, ids := range places {
			n := 0
			for _, id := range ids {
				if there, held := other.objects[kind][id]; held && there.place == place {
					s.rank[kind][id] = n
					n++
				}
			}
		}
	}
}

// placeOfRef is the unit a reference reaches: the unit a named seat sits in,
// or a named unit itself — a seat first, as a `manages:` entry is resolved —
// and the root for a seat at the root or a name nothing answers to.
func (s *surveyed) placeOfRef(ref reference) string {
	if place, isSeat := s.seatPlaces[ref.to]; isSeat {
		return place
	}
	if ref.why == "manages" {
		if unit := s.org.Unit(ref.to); unit != nil {
			return unit.Key()
		}
	}
	return ""
}

// encodeFields is an object's own fields as JSON, for comparison.
func encodeFields(v any) []byte {
	encoded, err := json.Marshal(v)
	if err != nil {
		// A document this build decoded always encodes; an object that
		// does not is one nothing can say is unchanged.
		return []byte(err.Error())
	}
	return encoded
}

// fieldsChanged is every top-level field whose encoding differs, sorted.
func fieldsChanged(was, is []byte) []string {
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal(was, &before)
	_ = json.Unmarshal(is, &after)
	return differingKeys(before, after)
}

// differingKeys is every key whose value differs between two objects, sorted.
func differingKeys(before, after map[string]json.RawMessage) []string {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if !bytes.Equal(before[k], after[k]) {
			out = append(out, k)
		}
	}
	return out
}

// settingsChanged is every top-level key outside the org chart whose value
// differs, sorted.
func settingsChanged(before, after *Company) []string {
	var b, a map[string]json.RawMessage
	_ = json.Unmarshal(encodeFields(before), &b)
	_ = json.Unmarshal(encodeFields(after), &a)
	for _, chart := range []string{"roles", "units"} {
		delete(b, chart)
		delete(a, chart)
	}
	return differingKeys(b, a)
}

// credentialsOf is everything in v only admin may change, by its path inside v:
// the WHOLE VALUE of every credential field, keys as much as strings, and every
// other string carrying a `${VAR}`.
//
// Whole value, because a key is a capability: an `mcp_env` block's keys overlay
// its server's values for the seat's child, so `{github: {TOKEN: ""}}` changes
// the identity the child acts with though it holds no string a walk would find.
//
// EMPTYING ONE IS STILL A CHANGE: the side that held a value lists its path,
// which [credentialsChanged] reports as cleared. An empty block where there was
// none is no change: the document is stored with empty fields omitted, so
// counting it would refuse a lead sending back what they read.
func credentialsOf(v any) map[string]string {
	out := map[string]string{}
	var walk func(v reflect.Value, path Path)
	walk = func(v reflect.Value, path Path) {
		switch v.Kind() {
		case reflect.String:
			if envref.Has(v.String()) {
				out[path.String()] = v.String()
			}
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem(), path)
			}
		case reflect.Struct:
			for i := range v.NumField() {
				field := v.Type().Field(i)
				if !field.IsExported() {
					continue
				}
				value, inner := v.Field(i), at(path, jsonName(field))
				if !secrets.Field(field) {
					walk(value, inner)
					continue
				}
				if empty := value.IsZero() || (value.Kind() == reflect.Map ||
					value.Kind() == reflect.Slice) && value.Len() == 0; !empty {
					out[inner.String()] = string(encodeFields(value.Interface()))
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i), idx(path, i))
			}
		case reflect.Map:
			// A KEY IS TEXT AS MUCH AS A VALUE, and the reference index
			// reads both ([References]).
			for _, key := range v.MapKeys() {
				inner := entry(path, key.String())
				walk(key, inner)
				walk(v.MapIndex(key), inner)
			}
		}
	}
	walk(reflect.ValueOf(v), nil)
	return out
}

// credentialsChanged is every path whose credential differs, sorted.
func credentialsChanged(was, is map[string]string) []string {
	var out []string
	for path, value := range is {
		if previous, held := was[path]; !held || previous != value {
			out = append(out, path)
		}
	}
	for path := range was {
		if _, held := is[path]; !held {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}
