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
// The org chart is written by its LEADS as well as by whoever holds
// `config:write` (internal/authz's subtree rule): a lead may change what sits
// inside a unit they lead, and nothing else. So a write is not judged as a
// document but as the seats and units it adds, removes, moves and edits, and
// for each one the PLACES it reaches — every unit that must be inside the
// writer's subtree for the change to be theirs. [DiffOrg] states those places;
// whether they ARE inside somebody's subtree is the authority table's to
// decide (internal/api/configapi asks it).
//
// # The places, and what each closes
//
//   - WHERE AN OBJECT IS, before and after. A seat is placed in the unit that
//     holds it, a unit in its parent — so a lead adds, removes and moves
//     objects only within their subtree, and cannot remove or move the top
//     unit they lead, whose parent is not theirs.
//   - A UNIT ITSELF, before and after, when its own fields change. A lead
//     edits their own team's name, purpose and channel, while handing the
//     unit to another `lead:` fails the AFTER side unless they also lead the
//     unit above it.
//   - WHAT A NEW REFERENCE NAMES, after: a unit's `lead:` and a seat's
//     `manages:` entries. Leadership and management are what authority is
//     derived from, so a lead who could name an outsider as their sub-team's
//     lead, or make their report the CEO's manager, would be reaching outside
//     their subtree through the relations it is defined by. A reference that
//     names nothing reaches the root, which is nobody's.
//   - WHO ELSE CLAIMS A NEW KEY, before: a project, a page space and a
//     channel, a seat's email and its contact identities. A lead's authority
//     over a project or a space is derived from the unit that declares it,
//     and vendor attribution from an address, so declaring a key another team
//     already holds would be taking that team's.
//   - WHO ALREADY NAMES A NEW OBJECT, before: every `lead:` and `manages:`
//     entry in the document that states the id an added seat or unit takes.
//     A reference may name nothing — a unit can land before the seat that
//     leads it — and one that names a unit's key resolves to a seat of that
//     handle first, so adding a seat under such a name hands the referrer's
//     authority to whatever the lead configures: a seat added as `ghost`
//     becomes the lead of an outside team whose `lead: ghost` dangled, with
//     the lead who added it above that whole team.
//
// Only what a write CHANGES is judged: a reference or a claim the object
// already carried was somebody else's decision, and judging it again would
// make an admin-wired team uneditable by its own lead.
//
// A CREDENTIAL IS NEVER A LEAD'S, AND NEITHER IS A `${VAR}`. Every credential
// field a change sets, clears or alters — compared whole, so a key with nothing
// under it counts, since an `mcp_env` key alone starts its server for the seat
// — and every `${VAR}` reference it sets, clears or alters in ANY field, is
// listed apart ([OrgChange.Credentials]) and is the company grant's. A reference names any variable the engine's process
// can resolve — the company's secrets and Tier A's own keyring and tokens alike
// — and a credential field is not the only place one is resolved: a human
// seat's `email` and contact identities resolve a whole `${VAR}` too, and are
// then recited by `lookup_colleague` and a lead's prompt roster and matched by
// the party registry. So a lead could otherwise read any secret back by naming
// it as their own Slack id. A setting outside `roles:` and `units:` is listed
// apart for the same grant ([OrgDiff.Settings]).
//
// PURE, over two documents and never the running company: a node behind on
// applies decides a write exactly as a current one does.

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
	// OrgChanged is an object that stayed where it was and changed.
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
	// its path inside the object, sorted.
	Credentials []string

	// Touches is every place the change reaches.
	Touches []OrgTouch
}

// OrgTouch is one place a change reaches.
type OrgTouch struct {
	Side OrgSide
	// Unit is the key of the unit the place is: the unit holding a seat,
	// the parent of a unit, the unit a reference names, the unit a claimant
	// is or sits in. Empty for the company root, which a seat or unit at
	// the top of the company sits in and a reference naming nothing reaches.
	Unit string
	// Why is what about the change reaches it: `place`, `self`, `lead`,
	// `manages`, the key a claim is about (`project`, `space`, `channel`,
	// `email`, `contact`), or `named` for another object's reference that
	// already states the id an added object takes.
	Why string
	// Value is the reference or the claimed key, as the change stated it;
	// empty for `place` and `self`.
	Value string
}

// DiffOrg reports what turning before into after changes. A nil before is a
// document with nothing in it.
func DiffOrg(before, after *Company) OrgDiff {
	if before == nil {
		before = &Company{}
	}
	if after == nil {
		after = &Company{}
	}
	b, a := survey(before), survey(after)
	diff := OrgDiff{Settings: settingsChanged(before, after), Before: b.org, After: a.org}

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
	// states — so the change is placed at the root, which only the company
	// grant writes. Only a stored revision can carry one (the admission
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
		for _, referrer := range b.referrers[id] {
			change.Touches = append(change.Touches, OrgTouch{
				Side: OrgBefore, Unit: referrer.place, Why: "named", Value: id,
			})
		}
	case !has:
		change.Op = OrgRemoved
		change.Touches = []OrgTouch{{Side: OrgBefore, Unit: was.place, Why: "place"}}
		return change, true
	default:
		change.Fields = fieldsChanged(was.document, is.document)
		moved := was.place != is.place
		if !moved && len(change.Fields) == 0 {
			return change, false
		}
		change.Op = OrgChanged
		if moved {
			change.Op = OrgMoved
		}
		if kind == OrgSeat || moved {
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
	for _, ref := range is.refs {
		if !slices.Contains(was.refs, ref) {
			change.Touches = append(change.Touches, OrgTouch{
				Side: OrgAfter, Unit: a.placeOfRef(ref), Why: ref.why, Value: ref.to,
			})
		}
	}
	for _, claim := range is.claims {
		if slices.Contains(was.claims, claim) {
			continue
		}
		for _, holder := range b.claimants[claim] {
			if holder.kind == kind && holder.id == id {
				continue
			}
			change.Touches = append(change.Touches, OrgTouch{
				Side: OrgBefore, Unit: holder.place, Why: claim.key, Value: claim.value,
			})
		}
	}
	return change, true
}

// surveyed is one document, read for a diff.
type surveyed struct {
	org *org.Organization
	// objects is every seat by handle and unit by key.
	objects map[OrgKind]map[string]object
	// claimants is who states each claimed key.
	claimants map[claim][]claimant
	// referrers is who states each name in a `lead:` or `manages:` entry,
	// whether or not anything answers to it.
	referrers map[string][]claimant
	// seatPlaces is the unit holding each seat, by handle; "" for the root.
	seatPlaces map[string]string
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
	// claims is every key it claims.
	claims []claim
}

// reference is a name one object states for another: a unit's `lead:`, a
// seat's `manages:` entry.
type reference struct {
	why string
	to  string
}

// claim is a key an object states as its own, folded so two spellings of one
// key are one claim.
type claim struct {
	key   string
	value string
}

// claimant is an object stating a claim or a reference, and the place it
// reaches.
type claimant struct {
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
		claimants:  map[claim][]claimant{},
		referrers:  map[string][]claimant{},
		seatPlaces: map[string]string{},
	}
	unitParents := map[string]string{}
	var walk func(units []*org.Unit, parent string)
	walk = func(units []*org.Unit, parent string) {
		for _, u := range units {
			key := u.Key()
			if _, seen := unitParents[key]; !seen {
				unitParents[key] = parent
			}
			for _, r := range u.Roles {
				if _, seen := s.seatPlaces[r.Handle()]; !seen {
					s.seatPlaces[r.Handle()] = key
				}
			}
			walk(u.Children, key)
		}
	}
	walk(o.Units, "")
	for _, r := range o.Roles {
		if _, seen := s.seatPlaces[r.Handle()]; !seen {
			s.seatPlaces[r.Handle()] = ""
		}
	}

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
			seen.claims = seatClaims(role)
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
			seen.claims = unitClaims(unit)
		}
		s.objects[OrgUnit][key] = seen
	}
	for kind, objects := range s.objects {
		for id, o := range objects {
			place := o.place
			if kind == OrgUnit {
				place = id
			}
			for _, c := range o.claims {
				s.claimants[c] = append(s.claimants[c], claimant{kind: kind, id: id, place: place})
			}
			for _, ref := range o.refs {
				s.referrers[ref.to] = append(s.referrers[ref.to], claimant{kind: kind, id: id, place: place})
			}
		}
	}
	return s
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

// seatClaims is every key a seat states as its own.
func seatClaims(r *Role) []claim {
	claims := claimsOf(map[string]string{
		"project": r.Project, "space": r.Space, "email": r.Email,
	})
	for _, identity := range r.Contact.Identities() {
		claims = append(claims, claim{key: "contact",
			value: string(identity.Transport) + ":" + fold(identity.ExternalID)})
	}
	return claims
}

// unitClaims is every key a unit states as its own.
func unitClaims(u *Unit) []claim {
	return claimsOf(map[string]string{
		"project": u.Project, "space": u.Space, "channel": u.Channel,
	})
}

// claimsOf is each non-empty value, folded.
func claimsOf(values map[string]string) []claim {
	var out []claim
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if value := fold(values[key]); value != "" {
			out = append(out, claim{key: key, value: value})
		}
	}
	return out
}

// fold is the one spelling two claims are compared in. CASE-FOLDED for every
// key, which is stricter than some of them need — a channel id is matched as
// written elsewhere — and that is the safe direction for a check that refuses.
func fold(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

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

// credentialsOf is everything in v only the company's grant may change, by
// its path inside v: the WHOLE VALUE of every credential field — its keys as
// much as its strings — and every other string that carries a `${VAR}`.
//
// THE WHOLE VALUE, because a credential field's key is a grant of its own. An
// `mcp_env` block naming a per-seat server is what starts that server for the
// seat, with the template's own resolved environment and headers, whatever the
// block holds — `{github: {}}` and `{github: {TOKEN: ""}}` included — so a walk
// of the strings alone, which finds nothing in either, admitted a lead
// attaching any per-seat tool server to their agents.
//
// A credential field holding nothing — an empty string, map or list — is
// none, so a block whose credential fields are unset holds none.
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
