package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/textcut"
)

// The coercion table: what a field value may BE, decided once at the write.
//
// # Why here and not at the apply
//
// A value is checked where it can be REFUSED. The applier salvages — a value
// that does not decode writes no row and the record carries on, because
// refusing it would let one malformed field stop that task's every later
// change on every node in the fleet. That is right for the applier and useless
// as validation: the value is already durable by then, and the person who set
// it has been told nothing.
//
// So the write refuses it, naming the rule, and NORMALISES what it accepts —
// an option spelling becomes the option's id, a timestamp on a date-only field
// becomes the date. The record then carries one canonical form, which is what
// lets every node write identical rows from it without re-deciding anything.
//
// # PURE OVER VALUES
//
// Nothing here reads a database. It takes a declaration and a JSON value and
// answers, so the whole table is exercised without a store — which is what
// makes it a table somebody re-reads rather than a behaviour discovered from a
// failing board. The two lookups that genuinely need the world — a
// relationship's task and a people field's seat — are the caller's, declared
// as seams below.
//
// # NO SILENT ROUNDING, and that is the rule the whole table turns on
//
// A number outside Min/Max or carrying more decimals than Precision is
// REFUSED. Rounding it would store a value nobody typed, under a declaration
// that says the field is exact to two places — and the person who set 3.14159
// on a field declared to two would find 3.14 on the board with nothing saying
// why.

// FieldWorld is the ONE lookup this package genuinely cannot do itself.
//
// DECLARED HERE AND SATISFIED BY THE CALLER, because the tracker holds no org:
// a people field resolves through the company's chart, which belongs to the
// EPOCH rather than to a row, and the applier may not read one at all — two
// nodes briefly on different epochs would write different values from one
// record, which is exactly why the value is resolved at the WRITE and carried.
//
// A relationship's task is NOT here, deliberately: that one is a row this
// package can read, and reading it inside the decide's own transaction is
// strictly better than a caller's separate read — a target resolved outside
// could be a task this node does not hold.
//
// A nil world passes a people value through UNNORMALISED rather than refusing
// it, which is what [Writer.Leads] already does for the other seam of this
// kind: a build with no roster cannot turn a name into a handle, and refusing
// every people field it holds would fail writes for a reason unrelated to what
// the caller asked for.
type FieldWorld interface {
	// ResolveSeat turns what somebody typed into exactly one handle. The
	// second return is false for no match AND for an ambiguous one: both
	// mean "this does not name one person", which is the only answer a
	// stored value can be written from.
	ResolveSeat(ref string) (string, bool)
}

// fieldRefs is what one settlement resolves with: the caller's chart seam, and
// the task lookup this package reads from the decide's own snapshot.
type fieldRefs struct {
	world FieldWorld
	task  func(ref string) (string, bool)
}

// coerced is one field's value after the table has had it.
type coerced struct {
	// Value is the canonical form the record carries.
	Value json.RawMessage

	// Warnings are what the writer is TOLD and not refused for — today
	// the one case the design names, a timestamp truncated to its date.
	Warnings []string
}

// coerceFields runs the table over a whole field map.
//
// IT RETURNS A NEW MAP rather than writing through the one it was given: the
// decide runs again on a retry, and a normalisation folded into the captured
// patch would compound across attempts — the same rule [settleWatch] states.
//
// A FIELD NOTHING DECLARES IS LEFT ALONE, not refused. The applier stores it
// as a `foreign` value by design — a field declared on another project, or by
// a newer build — and refusing it here would make a rolling upgrade reject
// writes it is supposed to carry through.
func coerceFields(declared map[string]FieldDef, values map[string]json.RawMessage,
	taskType string, refs fieldRefs) (map[string]json.RawMessage, []string, error) {

	if len(values) == 0 {
		return values, nil, nil
	}
	if len(values) > MaxFieldValues {
		return nil, nil, fmt.Errorf("tracker: this write sets %d custom fields "+
			"and the maximum is %d", len(values), MaxFieldValues)
	}
	bySlug, byName := fieldIndex(declared)
	out := make(map[string]json.RawMessage, len(values))
	var warnings []string
	// SORTED, so a write that refuses names the same field on every node
	// and a warning list is the same list twice. A map's order is not a
	// property anything may depend on, and two nodes reporting different
	// first failures for one record is a fleet that disagrees about why.
	for _, key := range sortedRawKeys(values) {
		raw := values[key]
		// THE KEY IS RESOLVED HERE, in the same snapshot as the
		// declarations, because a model types the SLUG and a stored
		// value is keyed by the field's ID — that is what lets a field
		// move between the workspace and a project, or be renamed,
		// without re-pointing a single task's value.
		//
		// Resolving at the tool instead would take a second catalogue
		// read that could race this one, and a slug resolved against a
		// catalogue the write then lands beside is a value filed under
		// whichever field held that spelling a moment ago.
		field, held := resolveFieldKey(key, declared, bySlug, byName)
		if !held {
			// A KEY NOTHING DECLARES PASSES THROUGH. The applier
			// stores it as a `foreign` value by design — a field
			// declared on another project, or by a newer build — and
			// refusing it here would make a rolling upgrade reject
			// writes it is meant to carry.
			out[key] = raw
			continue
		}
		got, err := coerceField(field, raw, refs)
		if err != nil {
			return nil, nil, err
		}
		if previous, clash := out[field.ID]; clash && string(previous) != string(got.Value) {
			// TWO SPELLINGS OF ONE FIELD, disagreeing. Whichever won
			// would silently discard the other, and a map has no
			// order to appeal to.
			return nil, nil, fmt.Errorf("tracker: this write sets field %q "+
				"twice, under two spellings and two different values — state "+
				"it once", field.Slug)
		}
		out[field.ID] = got.Value
		warnings = append(warnings, got.Warnings...)
	}
	_ = taskType
	return out, warnings, nil
}

// fieldIndex is the slug and name lookups the three-tier resolution needs.
//
// AN ARCHIVED FIELD IS NOT RESOLVABLE BY SLUG OR NAME, which is the same rule
// the query side applies: its values are hidden from every index, so setting
// one by the spelling a person remembers would write a value nothing can ever
// see. By ID it still resolves, because a record replaying an older write has
// to land on the field it named.
func fieldIndex(declared map[string]FieldDef) (bySlug, byName map[string]FieldDef) {
	bySlug = make(map[string]FieldDef, len(declared))
	byName = make(map[string]FieldDef, len(declared))
	for _, field := range declared {
		if field.Archived {
			continue
		}
		if field.Slug != "" {
			bySlug[strings.ToLower(field.Slug)] = field
		}
		if field.Name != "" {
			byName[strings.ToLower(field.Name)] = field
		}
	}
	return bySlug, byName
}

// resolveFieldKey is the three tiers, earlier ones short-circuiting later —
// the same precedence every other resolution in this package uses, so a key
// that is exactly somebody's slug is never matched against a name instead.
func resolveFieldKey(key string, byID, bySlug, byName map[string]FieldDef) (FieldDef, bool) {
	trimmed := strings.TrimSpace(key)
	for _, candidate := range []FieldDef{
		byID[trimmed],
		bySlug[strings.ToLower(trimmed)],
		byName[strings.ToLower(trimmed)],
	} {
		if candidate.ID != "" {
			return candidate, true
		}
	}
	return FieldDef{}, false
}

// CoerceField runs the table over ONE value.
func coerceField(field FieldDef, raw json.RawMessage, refs fieldRefs) (coerced, error) {
	// NULL CLEARS ANY FIELD, whatever its type — so it is decided before
	// the type is, and no branch below has to remember it.
	if len(raw) == 0 || string(raw) == "null" {
		return coerced{Value: raw}, nil
	}
	// WHICH TYPES HOLD SEVERAL VALUES IS [MultiValued]'s ANSWER, not a
	// second one written here. The query side reads that function to
	// decide whether a group renders a task under each of its values, and
	// the applier's own [fieldRows] takes the same reading — so a third
	// rule here is how one of the three stops matching the others. It
	// did, immediately: gating on `Config.Multi` refused a one-element
	// list on a `people` field, which is exactly what the tree already
	// writes and the row layer already accepts.
	//
	// `Config.Multi` is the DECLARATION's own flag and is honoured below
	// as the cap, because "this field holds several" and "this field is
	// allowed to" are different questions.
	if MultiValued(field.Type) || field.Config.Multi {
		return coerceMany(field, raw, refs)
	}
	return coerceOne(field, raw, refs)
}

// coerceMany is a multi-valued field: every member through the table, and the
// result re-encoded as the list it came in as.
func coerceMany(field FieldDef, raw json.RawMessage, refs fieldRefs) (coerced, error) {
	var members []json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		// NOT A LIST IS ONE MEMBER, which is how a multi-valued field
		// carries exactly one — the same reading [fieldRows] takes, and
		// the two have to agree or the write and the row disagree about
		// what was set.
		return coerceOne(field, raw, refs)
	}
	if len(members) > MaxFieldValueSeq {
		return coerced{}, fmt.Errorf("tracker: field %q takes %d values and the "+
			"maximum is %d", field.Slug, len(members), MaxFieldValueSeq)
	}
	out := make([]json.RawMessage, 0, len(members))
	var warnings []string
	for _, member := range members {
		got, err := coerceOne(field, member, refs)
		if err != nil {
			return coerced{}, err
		}
		out = append(out, got.Value)
		warnings = append(warnings, got.Warnings...)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return coerced{}, fmt.Errorf("tracker: re-encode field %q: %w", field.Slug, err)
	}
	return coerced{Value: encoded, Warnings: warnings}, nil
}

// coerceOne is the table itself, one member at a time.
func coerceOne(field FieldDef, raw json.RawMessage, refs fieldRefs) (coerced, error) {
	switch field.Type {
	case FieldNumber, FieldProgress, FieldRollup:
		return coerceNumber(field, raw)
	case FieldCheckbox:
		return coerceCheckbox(field, raw)
	case FieldDate:
		return coerceDate(field, raw)
	case FieldDropdown, FieldLabels:
		return coerceOption(field, raw)
	case FieldRelationship:
		return coerceRelationship(field, raw, refs)
	case FieldPeople:
		return coercePeople(field, raw, refs)
	case FieldURL:
		return coerceURL(field, raw)
	case FieldEmail:
		return coerceEmail(field, raw)
	case FieldTextarea:
		return coerceText(field, raw, MaxTextareaBytes)
	}
	return coerceText(field, raw, MaxFieldValueBytes)
}

// coerceNumber is the rule with the sharpest edge: REFUSED, never rounded.
func coerceNumber(field FieldDef, raw json.RawMessage) (coerced, error) {
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		// A NUMBER TYPED AS TEXT IS ACCEPTED AND NORMALISED, because
		// that is what a model sends when it copies a value out of prose
		// — and the alternative is refusing "3" for not being 3. What is
		// NOT accepted is text that is not a number: the apply side used
		// to salvage a number out of any string, so "about 7 hours" was
		// stored as 7.
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return coerced{}, fmt.Errorf("tracker: field %q is a number and "+
				"this value is %s", field.Slug, clipRaw(raw))
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return coerced{}, fmt.Errorf("tracker: field %q is a number and "+
				"%q is not one", field.Slug, clip(text))
		}
		n = parsed
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return coerced{}, fmt.Errorf("tracker: field %q cannot hold %v", field.Slug, n)
	}
	if min := field.Config.Min; min != nil && n < *min {
		return coerced{}, fmt.Errorf("tracker: field %q has a minimum of %v and "+
			"this value is %v", field.Slug, *min, n)
	}
	if max := field.Config.Max; max != nil && n > *max {
		return coerced{}, fmt.Errorf("tracker: field %q has a maximum of %v and "+
			"this value is %v", field.Slug, *max, n)
	}
	if places := decimalsOf(n); places > field.Config.Precision {
		// NAMING THE RULE, and never rounding to fit: a field declared
		// exact to two places that quietly stored 3.14 for 3.14159 is a
		// number nobody typed, under a declaration that says it is
		// exact.
		return coerced{}, fmt.Errorf("tracker: field %q is exact to %d decimal "+
			"place(s) and %v has %d — set a value at that precision, or widen "+
			"the field's own", field.Slug, field.Config.Precision, n, places)
	}
	encoded, err := json.Marshal(n)
	if err != nil {
		return coerced{}, err
	}
	return coerced{Value: encoded}, nil
}

// decimalsOf is how many decimal places a float actually carries.
//
// THROUGH ITS SHORTEST ROUND-TRIPPING FORM ('g', -1), because that is the
// number a person typed rather than the binary approximation: 0.1 is one
// decimal place, though its float64 is 0.1000000000000000055511151231257827.
func decimalsOf(n float64) int {
	text := strconv.FormatFloat(n, 'f', -1, 64)
	dot := strings.IndexByte(text, '.')
	if dot < 0 {
		return 0
	}
	return len(text) - dot - 1
}

// coerceCheckbox accepts a bool ONLY.
//
// Not "true"/"yes"/"1": the read side coerces those because a FILTER is
// somebody typing a query, and a stored value is not. A checkbox set from the
// string "false" is the case this exists for — every truthy-string rule in
// every language disagrees about it, and the one that read it as set would
// store the opposite of what was meant.
func coerceCheckbox(field FieldDef, raw json.RawMessage) (coerced, error) {
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return coerced{}, fmt.Errorf("tracker: field %q is a checkbox and takes "+
			"true or false, not %s — a string is refused because every rule "+
			"for reading one disagrees about \"false\"",
			field.Slug, clipRaw(raw))
	}
	encoded, err := json.Marshal(b)
	if err != nil {
		return coerced{}, err
	}
	return coerced{Value: encoded}, nil
}

// coerceDate is the one rule that WARNS rather than refusing.
//
// A date-only field accepts a timestamp and truncates it to its date, saying
// so — because the truncation is what the declaration asked for and the
// information lost is the part the field does not have. A `Time` field refuses
// a bare date instead: there is nothing to truncate, and inventing midnight
// would put a value nobody typed on the board.
func coerceDate(field FieldDef, raw json.RawMessage) (coerced, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return coerced{}, fmt.Errorf("tracker: field %q is a date and this "+
			"value is %s — dates are strings, as 2026-03-04 or "+
			"2026-03-04T09:00:00Z", field.Slug, clipRaw(raw))
	}
	text = strings.TrimSpace(text)
	if at, err := time.Parse(time.RFC3339, text); err == nil {
		if !field.Config.Time {
			day := at.UTC().Format(time.DateOnly)
			encoded, err := json.Marshal(day)
			if err != nil {
				return coerced{}, err
			}
			return coerced{Value: encoded, Warnings: []string{fmt.Sprintf(
				"field %q holds a date and not a time, so %s was stored as %s",
				field.Slug, text, day)}}, nil
		}
		encoded, err := json.Marshal(at.UTC().Format(time.RFC3339))
		if err != nil {
			return coerced{}, err
		}
		return coerced{Value: encoded}, nil
	}
	day, err := time.Parse(time.DateOnly, text)
	if err != nil {
		return coerced{}, fmt.Errorf("tracker: field %q is a date and %q is not "+
			"one — write 2026-03-04, or 2026-03-04T09:00:00Z", field.Slug, clip(text))
	}
	if field.Config.Time {
		return coerced{}, fmt.Errorf("tracker: field %q holds a date AND a time "+
			"and %q carries no time — give one, as %sT09:00:00Z, rather than "+
			"letting the engine invent midnight",
			field.Slug, clip(text), day.Format(time.DateOnly))
	}
	encoded, err := json.Marshal(day.Format(time.DateOnly))
	if err != nil {
		return coerced{}, err
	}
	return coerced{Value: encoded}, nil
}

// coerceOption resolves a slug, a name or an id to the option's ID.
//
// THE ID IS WHAT IS STORED, which is what keeps every task that chose an
// option when somebody renames it — and resolving here rather than at the
// apply is what lets the write REFUSE a value no option matches. The applier
// resolves too, and must: a record from an older build carries the spelling.
func coerceOption(field FieldDef, raw json.RawMessage) (coerced, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return coerced{}, fmt.Errorf("tracker: field %q takes one of its "+
			"options and this value is %s", field.Slug, clipRaw(raw))
	}
	text = strings.TrimSpace(text)
	id, held := OptionIDs(field.Config.Options)[strings.ToLower(text)]
	if !held {
		return coerced{}, fmt.Errorf("tracker: field %q has no option %q. Its "+
			"options are: %s", field.Slug, clip(text), optionList(field))
	}
	encoded, err := json.Marshal(id)
	if err != nil {
		return coerced{}, err
	}
	return coerced{Value: encoded}, nil
}

// optionList is what a refusal names, so the next call is right rather than
// another guess.
func optionList(field FieldDef) string {
	out := make([]string, 0, len(field.Config.Options))
	for _, option := range field.Config.Options {
		if option.Archived {
			continue
		}
		out = append(out, option.Slug)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "(none declared)"
	}
	return strings.Join(out, ", ")
}

// coerceRelationship stores a task ID, resolved from a key, a former key or an
// id — for the reason every other relation gives: a key stored in one resolves
// to nothing on every node, for ever.
func coerceRelationship(field FieldDef, raw json.RawMessage, refs fieldRefs) (coerced, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return coerced{}, fmt.Errorf("tracker: field %q names a work item and "+
			"this value is %s", field.Slug, clipRaw(raw))
	}
	text = strings.TrimSpace(text)
	if refs.task == nil {
		return coerced{}, fmt.Errorf("tracker: field %q names work item %q and "+
			"this write cannot resolve one — a stored key resolves to nothing "+
			"on every node", field.Slug, clip(text))
	}
	id, held := refs.task(text)
	if !held {
		return coerced{}, fmt.Errorf("tracker: field %q names work item %q and "+
			"there is no such item", field.Slug, clip(text))
	}
	encoded, err := json.Marshal(id)
	if err != nil {
		return coerced{}, err
	}
	return coerced{Value: encoded}, nil
}

// coercePeople stores a handle, and only one a lookup resolved to EXACTLY one
// seat: an ambiguous spelling and an unknown one are the same fact here —
// neither names a person a filter could ever match.
func coercePeople(field FieldDef, raw json.RawMessage, refs fieldRefs) (coerced, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return coerced{}, fmt.Errorf("tracker: field %q names a colleague and "+
			"this value is %s", field.Slug, clipRaw(raw))
	}
	text = strings.TrimSpace(text)
	if refs.world == nil {
		// NO CHART DEGRADES RATHER THAN REFUSING, which is the rule
		// [Writer.Leads] already states for the other seam of the same
		// kind: nil "costs a wake its fallback recipient and never its
		// delivery". A build with no roster cannot normalise a name to a
		// handle, and refusing every people field it holds would be a
		// write failed for a reason unrelated to what the caller asked.
		//
		// The spelling is where a typo is caught in that build, exactly
		// as it is for an ASSIGNEE: the tool resolves that one against
		// the roster before the write, and a build without a roster does
		// not check it either.
		return coerced{Value: raw}, nil
	}
	handle, held := refs.world.ResolveSeat(text)
	if !held {
		return coerced{}, fmt.Errorf("tracker: field %q names %q and that is "+
			"not one colleague of this company", field.Slug, clip(text))
	}
	encoded, err := json.Marshal(handle)
	if err != nil {
		return coerced{}, err
	}
	return coerced{Value: encoded}, nil
}

// coerceURL needs a scheme, because a bare host is a string that renders as a
// dead link on every board that treats the field as one.
func coerceURL(field FieldDef, raw json.RawMessage) (coerced, error) {
	got, err := coerceText(field, raw, MaxFieldValueBytes)
	if err != nil {
		return coerced{}, err
	}
	var text string
	if err := json.Unmarshal(got.Value, &text); err != nil {
		return coerced{}, err
	}
	parsed, err := url.Parse(text)
	switch {
	case err != nil:
		return coerced{}, fmt.Errorf("tracker: field %q is a url and %q is not "+
			"one: %v", field.Slug, clip(text), err)
	case parsed.Scheme == "":
		return coerced{}, fmt.Errorf("tracker: field %q is a url and %q has no "+
			"scheme — write https://%s", field.Slug, clip(text), clip(text))
	case parsed.Host == "" && parsed.Opaque == "":
		return coerced{}, fmt.Errorf("tracker: field %q is a url and %q names "+
			"no host", field.Slug, clip(text))
	}
	return got, nil
}

// coerceEmail needs a parseable address.
func coerceEmail(field FieldDef, raw json.RawMessage) (coerced, error) {
	got, err := coerceText(field, raw, MaxFieldValueBytes)
	if err != nil {
		return coerced{}, err
	}
	var text string
	if err := json.Unmarshal(got.Value, &text); err != nil {
		return coerced{}, err
	}
	address, err := mail.ParseAddress(text)
	if err != nil {
		return coerced{}, fmt.Errorf("tracker: field %q is an email address and "+
			"%q is not one: %v", field.Slug, clip(text), err)
	}
	// THE ADDRESS, not the display name: `Ana <a@example.com>` parses and
	// is stored as the address, so two spellings of one person's mail are
	// one value a filter matches.
	encoded, err := json.Marshal(address.Address)
	if err != nil {
		return coerced{}, err
	}
	return coerced{Value: encoded}, nil
}

// coerceText is the fallback, and the one cap every stored string takes.
func coerceText(field FieldDef, raw json.RawMessage, limit int) (coerced, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return coerced{}, fmt.Errorf("tracker: field %q holds text and this "+
			"value is %s", field.Slug, clipRaw(raw))
	}
	if len(text) > limit {
		// REFUSED NAMING THE SIZE, never cut: a value silently truncated
		// is a value a person will look for later.
		return coerced{}, fmt.Errorf("tracker: field %q holds %d bytes and the "+
			"maximum is %d", field.Slug, len(text), limit)
	}
	encoded, err := json.Marshal(text)
	if err != nil {
		return coerced{}, err
	}
	return coerced{Value: encoded}, nil
}

// clipRaw is a raw JSON value as a refusal shows it.
//
// RUNE-SAFE, through [textcut.Ellipsis], because a refusal is text a model
// reads and a value cut mid-rune reaches it as a replacement character — the
// one shared rule [textcut] exists for.
func clipRaw(raw json.RawMessage) string { return clip(string(raw)) }

// clip shortens what a refusal quotes back.
func clip(s string) string { return textcut.Ellipsis(s, MaxRefusalQuote) }

// MaxRefusalQuote is how much of a rejected value a refusal echoes.
//
// 64 BYTES, which is enough to recognise what was sent and short enough that a
// refusal naming several fields stays one message a model can act on. The
// value itself is in the caller's own request; this is the identifying
// fragment, not a copy.
const MaxRefusalQuote = 64

// settleFields runs the table inside the decide, against the declarations this
// node holds.
//
// INSIDE THE DECIDE, for the reason every other settlement here gives: the
// declarations a value is judged against have to be the ones the record lands
// on. A field archived between a caller's read and its write is the case —
// the archive is itself a record, and a value validated against the older
// catalogue would be written under a declaration that no longer says what it
// said.
//
// It returns a COPY, because Decide runs again on a retry and a normalisation
// folded into the captured patch would compound across attempts.
func settleFields(ctx context.Context, tx *sql.Tx, project, taskType string,
	values map[string]json.RawMessage, world FieldWorld) (
	map[string]json.RawMessage, []string, error) {

	if len(values) == 0 {
		return values, nil, nil
	}
	declared, err := declaredFields(ctx, tx, project)
	if err != nil {
		return nil, nil, err
	}
	// THE TASK LOOKUP IS THIS SNAPSHOT'S, so a relationship resolves
	// against the rows the record lands beside rather than against a
	// caller's earlier read — and a target this node does not hold is
	// refused here rather than stored as an id that resolves to nothing.
	return coerceFields(declared, values, taskType, fieldRefs{
		world: world,
		task: func(ref string) (string, bool) {
			id, err := resolveTaskID(ctx, tx, ref)
			return id, err == nil
		},
	})
}

// refuseUnsetRequired refuses a patch that clears a field the task must have.
//
// THE OTHER HALF OF requiredFields, which only ever ran on a CREATE and on a
// project move: an update setting a required field to null left the task
// without it, valid by every check the engine makes and refused by the next
// create of the same shape. "Update may not unset one" is the design's own
// sentence, and nothing said it.
//
// A FIELD THAT DOES NOT APPLY CANNOT BE UNSET, so the type gate is the same
// one requiredFields applies — and a subtask is judged by the second flag for
// the same reason.
func refuseUnsetRequired(ctx context.Context, tx *sql.Tx, current Task,
	next map[string]json.RawMessage) error {

	declared, err := declaredFields(ctx, tx, current.Project)
	if err != nil {
		return err
	}
	var cleared []string
	for id, field := range declared {
		required := field.Required
		if current.Parent != nil && *current.Parent != "" {
			required = field.RequiredInSubtasks
		}
		if !required || field.Archived || !appliesTo(field, current.Type) {
			continue
		}
		raw, held := next[id]
		if !held || (len(raw) > 0 && string(raw) != "null") {
			continue
		}
		cleared = append(cleared, field.Slug)
	}
	if len(cleared) == 0 {
		return nil
	}
	sort.Strings(cleared)
	return fmt.Errorf("tracker: a %s in project %s requires %v, and this edit "+
		"clears them — set another value rather than emptying the field",
		current.Type, current.Project, cleared)
}
