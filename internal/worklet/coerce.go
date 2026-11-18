package worklet

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

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/words"
)

// The coercion table: what a field value may BE, decided once at the write,
// where it can be REFUSED. The applier salvages instead (a value that does not
// decode writes no row, so one malformed field cannot stop the task's later
// changes fleet-wide), when the value is already durable and its writer told
// nothing. The write refuses naming the rule and NORMALISES what it accepts (an
// option spelling to its id, a timestamp on a date-only field to the date), so
// every node writes identical rows from one canonical form.
//
// Pure over values: nothing here reads a database; the lookups that need the
// world are seams ([fieldRefs]). NO SILENT ROUNDING: a number outside Min/Max
// or past Precision is refused, since rounding stores a value nobody typed
// under a declaration that says it is exact.

// FieldWorld is the ONE lookup this package cannot do itself: a people field
// resolves through the company's chart, which belongs to the epoch, and the
// applier may not read it (two nodes on different epochs would write different
// values from one record), so the value is resolved at the write and carried.
//
// A relationship's task is not here: it is a row this package reads inside the
// decide's own transaction, where a target this node does not hold is caught.
//
// A nil world passes a people value through unnormalised, as [Writer.Leads]
// does: a build with no roster cannot resolve a name, and refusing would fail
// writes for a reason unrelated to the request.
type FieldWorld interface {
	// ResolveSeat turns what somebody typed into exactly one handle, the
	// stored value. False means no match OR an ambiguous one: neither names
	// one person.
	ResolveSeat(ref string) (string, bool)
}

// fieldRefs is what one settlement resolves with: the caller's chart seam, and
// the task lookup this package reads from the decide's own snapshot.
type fieldRefs struct {
	world FieldWorld
	task  func(ref string) (string, bool)

	// zone is the company's clock, which a date-only value truncates an
	// instant in — see [coerceDay]. Nil is UTC.
	zone *time.Location
}

// coerced is one field's value after the table has had it.
type coerced struct {
	// Value is the canonical form the record carries.
	Value json.RawMessage

	// Caveats are what the writer is TOLD and not refused for: today only a
	// timestamp truncated to its date.
	Caveats []caveat.Caveat
}

// coerceFields runs the table over a whole field map.
//
// IT RETURNS A NEW MAP: the decide runs again on a retry, and a normalisation
// folded into the captured patch would compound across attempts (as
// [settleWatch]).
func coerceFields(declared map[string]FieldDef, values map[string]json.RawMessage,
	taskType string, refs fieldRefs) (map[string]json.RawMessage, []caveat.Caveat, error) {

	if len(values) == 0 {
		return values, nil, nil
	}
	if len(values) > MaxFieldValues {
		return nil, nil, invalid("this write sets %d custom fields "+
			"and the maximum is %d", len(values), MaxFieldValues)
	}
	bySlug, byName := fieldIndex(declared)
	out := make(map[string]json.RawMessage, len(values))
	var warnings []caveat.Caveat
	// SORTED, so every node refusing one write names the same field and a
	// warning list is the same list twice.
	for _, key := range sortedRawKeys(values) {
		raw := values[key]
		// THE KEY IS RESOLVED HERE, in the declarations' own snapshot: a
		// model types the SLUG and a value is keyed by the field's ID (so a
		// field moves or is renamed without re-pointing values), and a
		// second catalogue read at the tool could race this one.
		field, held := resolveFieldKey(key, declared, bySlug, byName)
		if !held {
			// A KEY NOTHING DECLARES PASSES THROUGH: the applier keeps it
			// as a `foreign` value (another project's field, or a newer
			// build's), which a rolling upgrade must carry.
			out[key] = raw
			continue
		}
		got, err := coerceField(field, raw, refs)
		if err != nil {
			return nil, nil, err
		}
		if previous, clash := out[field.ID]; clash && string(previous) != string(got.Value) {
			// TWO SPELLINGS OF ONE FIELD, disagreeing: a map has no
			// order to pick a winner by.
			return nil, nil, invalid("this write sets field %q "+
				"twice, under two spellings and two different values — state "+
				"it once", field.Slug)
		}
		out[field.ID] = got.Value
		warnings = append(warnings, got.Caveats...)
	}
	_ = taskType
	return out, warnings, nil
}

// fieldIndex is the slug and name lookups the three-tier resolution needs.
//
// AN ARCHIVED FIELD RESOLVES ONLY BY ID, as on the query side: its values are
// hidden from every index, so setting one by slug or name writes a value
// nothing sees, while a replayed older write must still land on its ID.
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

// resolveFieldKey is the three tiers (ID, slug, name), earlier ones winning as
// in every resolution here, so a key that is a slug never matches a name.
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

// coerceField runs the table over ONE value.
func coerceField(field FieldDef, raw json.RawMessage, refs fieldRefs) (coerced, error) {
	// NULL CLEARS ANY FIELD, whatever its type, so it is decided first.
	if len(raw) == 0 || string(raw) == "null" {
		return coerced{Value: raw}, nil
	}
	// WHICH TYPES HOLD SEVERAL VALUES IS [MultiValued]'s ANSWER, which the
	// query side and the applier's [fieldRows] also read; a rule of its own
	// here would drift from theirs. `Config.Multi` is the declaration's flag
	// and counts too: "holds several" and "is allowed to" differ.
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
		// NOT A LIST IS ONE MEMBER, the reading [fieldRows] takes; the two
		// must agree or the write and the row disagree about what was set.
		return coerceOne(field, raw, refs)
	}
	if len(members) > MaxFieldValueSeq {
		return coerced{}, invalid("field %q takes %d values and the "+
			"maximum is %d", field.Slug, len(members), MaxFieldValueSeq)
	}
	out := make([]json.RawMessage, 0, len(members))
	var warnings []caveat.Caveat
	for _, member := range members {
		got, err := coerceOne(field, member, refs)
		if err != nil {
			return coerced{}, err
		}
		out = append(out, got.Value)
		warnings = append(warnings, got.Caveats...)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return coerced{}, fmt.Errorf("worklet: re-encode field %q: %w", field.Slug, err)
	}
	return coerced{Value: encoded, Caveats: warnings}, nil
}

// coerceOne is the table itself, one member at a time.
func coerceOne(field FieldDef, raw json.RawMessage, refs fieldRefs) (coerced, error) {
	switch field.Type {
	case FieldNumber, FieldProgress, FieldRollup:
		return coerceNumber(field, raw)
	case FieldCheckbox:
		return coerceCheckbox(field, raw)
	case FieldDate:
		return coerceDate(field, raw, refs.zone)
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
		// A NUMBER TYPED AS TEXT IS ACCEPTED AND NORMALISED, as a model
		// copying from prose sends "3". Text that is not a number is
		// refused, never salvaged: "about 7 hours" is not 7.
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return coerced{}, invalid("field %q is a number and "+
				"this value is %s", field.Slug, clipRaw(raw))
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return coerced{}, invalid("field %q is a number and "+
				"%q is not one", field.Slug, clip(text))
		}
		n = parsed
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return coerced{}, invalid("field %q cannot hold %v", field.Slug, n)
	}
	if min := field.Config.Min; min != nil && n < *min {
		return coerced{}, invalid("field %q has a minimum of %v and "+
			"this value is %v", field.Slug, *min, n)
	}
	if max := field.Config.Max; max != nil && n > *max {
		return coerced{}, invalid("field %q has a maximum of %v and "+
			"this value is %v", field.Slug, *max, n)
	}
	if places := decimalsOf(n); places > field.Config.Precision {
		// Never rounded to fit: NO SILENT ROUNDING, in the table's doc.
		return coerced{}, invalid("field %q is exact to %d decimal "+
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
// THROUGH ITS SHORTEST ROUND-TRIPPING FORM (precision -1), the number a
// person typed rather than the binary approximation: 0.1 is one place,
// though its float64 is 0.1000000000000000055511151231257827.
func decimalsOf(n float64) int {
	text := strconv.FormatFloat(n, 'f', -1, 64)
	dot := strings.IndexByte(text, '.')
	if dot < 0 {
		return 0
	}
	return len(text) - dot - 1
}

// coerceCheckbox accepts a bool ONLY, not "true"/"yes"/"1": a FILTER coerces
// those because it is somebody typing a query, and a stored value is not.
// Truthy-string rules disagree about "false", and one reading it as set would
// store the opposite of what was meant.
func coerceCheckbox(field FieldDef, raw json.RawMessage) (coerced, error) {
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return coerced{}, invalid("field %q is a checkbox and takes "+
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

// coerceDate is the one rule that WARNS rather than refusing: a date-only field
// truncates a timestamp to its date, saying so, since what is lost is what the
// field does not hold. A `Time` field refuses a bare date: inventing midnight
// puts a value nobody typed on the board. The date-only half is [coerceDay],
// shared with a project's target date so the two cannot drift.
func coerceDate(field FieldDef, raw json.RawMessage, zone *time.Location) (coerced, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return coerced{}, invalid("field %q is a date and this "+
			"value is %s — dates are strings, as 2026-03-04 or "+
			"2026-03-04T09:00:00Z", field.Slug, clipRaw(raw))
	}
	text = strings.TrimSpace(text)
	if !field.Config.Time {
		day, truncated, err := coerceDay(fmt.Sprintf("field %q", field.Slug), text, zone)
		if err != nil {
			return coerced{}, err
		}
		encoded, err := json.Marshal(day)
		if err != nil {
			return coerced{}, err
		}
		out := coerced{Value: encoded}
		if truncated {
			out.Caveats = []caveat.Caveat{caveat.FieldDateTruncated(field.Slug,
				field.Name, text, day, zoneName(zone))}
		}
		return out, nil
	}
	if at, err := time.Parse(time.RFC3339, text); err == nil {
		encoded, err := json.Marshal(at.UTC().Format(time.RFC3339))
		if err != nil {
			return coerced{}, err
		}
		return coerced{Value: encoded}, nil
	}
	day, err := time.Parse(time.DateOnly, text)
	if err != nil {
		return coerced{}, invalid("field %q is a date and %q is not "+
			"one — write 2026-03-04, or 2026-03-04T09:00:00Z", field.Slug, clip(text))
	}
	return coerced{}, invalid("field %q holds a date AND a time "+
		"and %q carries no time — give one, as %sT09:00:00Z, rather than "+
		"letting the engine invent midnight",
		field.Slug, clip(text), day.Format(time.DateOnly))
}

// coerceDay is THE rule for a value that holds a DAY: a calendar date is kept,
// an instant is truncated to the date it falls on (truncated, which the caller
// reports with its own [caveat]), and anything else is refused naming both
// spellings. `what` names the value in the refusal: `field "launch"`, `the
// target date`.
//
// The date an instant falls on is the COMPANY'S (ADR-0018), in its one
// calendar ([period]) a relative `due=` resolves in: in UTC, the evening of
// the fourth on Pacific time would be stored as the fifth. Nil zone is UTC.
func coerceDay(what, text string, zone *time.Location) (day string, truncated bool, err error) {
	if zone == nil {
		zone = time.UTC
	}
	text = strings.TrimSpace(text)
	if at, perr := time.Parse(time.RFC3339, text); perr == nil {
		return at.In(zone).Format(time.DateOnly), true, nil
	}
	parsed, perr := time.Parse(time.DateOnly, text)
	if perr != nil {
		return "", false, invalid("%s is a date and %q is not one: "+
			"write 2026-03-04, or 2026-03-04T09:00:00Z", what, clip(text))
	}
	return parsed.Format(time.DateOnly), false, nil
}

// zoneName is the calendar a truncated day was read in, as a caveat names it:
// [coerceDay]'s nil is UTC.
func zoneName(zone *time.Location) string {
	if zone == nil {
		return time.UTC.String()
	}
	return zone.String()
}

// coerceOption resolves a slug, a name or an id to the option's ID.
//
// THE ID IS STORED, so a rename keeps every task that chose the option, and
// resolving at the write lets it REFUSE a value no option matches. The applier
// resolves too, and must: a value written before a declaration covered its key
// is stored as written and re-exploded on the task's next write.
func coerceOption(field FieldDef, raw json.RawMessage) (coerced, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return coerced{}, invalid("field %q takes one of its "+
			"options and this value is %s", field.Slug, clipRaw(raw))
	}
	text = strings.TrimSpace(text)
	id, held := OptionIDs(field.Config.Options)[strings.ToLower(text)]
	if !held {
		return coerced{}, invalid("field %q has no option %q. Its "+
			"options are: %s", field.Slug, clip(text), optionList(field))
	}
	encoded, err := json.Marshal(id)
	if err != nil {
		return coerced{}, err
	}
	return coerced{Value: encoded}, nil
}

// optionList is what a refusal names, so the next call is not another guess.
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
// id: as for every relation, a stored key resolves to nothing on every node.
func coerceRelationship(field FieldDef, raw json.RawMessage, refs fieldRefs) (coerced, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return coerced{}, invalid("field %q names a work item and "+
			"this value is %s", field.Slug, clipRaw(raw))
	}
	text = strings.TrimSpace(text)
	if refs.task == nil {
		return coerced{}, invalid("field %q names work item %q and "+
			"this write cannot resolve one — a stored key resolves to nothing "+
			"on every node", field.Slug, clip(text))
	}
	id, held := refs.task(text)
	if !held {
		return coerced{}, invalid("field %q names work item %q and "+
			"there is no such item", field.Slug, clip(text))
	}
	encoded, err := json.Marshal(id)
	if err != nil {
		return coerced{}, err
	}
	return coerced{Value: encoded}, nil
}

// coercePeople stores a handle a lookup resolved to EXACTLY one seat: an
// ambiguous spelling and an unknown one alike name nobody a filter can match.
func coercePeople(field FieldDef, raw json.RawMessage, refs fieldRefs) (coerced, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return coerced{}, invalid("field %q names a colleague and "+
			"this value is %s", field.Slug, clipRaw(raw))
	}
	text = strings.TrimSpace(text)
	if refs.world == nil {
		// NO CHART DEGRADES RATHER THAN REFUSING (see [FieldWorld]); such
		// a build checks this spelling no more than it checks an ASSIGNEE.
		return coerced{Value: raw}, nil
	}
	handle, held := refs.world.ResolveSeat(text)
	if !held {
		return coerced{}, invalid("field %q names %q and that is "+
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
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := json.Unmarshal(got.Value, &text); err != nil {
		return coerced{}, err
	}
	parsed, err := url.Parse(text)
	switch {
	case err != nil:
		return coerced{}, invalid("field %q is a url and %q is not "+
			"one: %v", field.Slug, clip(text), err)
	case parsed.Scheme == "":
		// THE PRESCRIPTION IS THE WHOLE VALUE, `text` and not `clip(text)`:
		// a shortened address the caller is told to write is a dead link
		// (see [MaxRefusalQuote]). The quote is whole too, as [coerceText]
		// bounded `text` at [MaxFieldValueBytes], which [MaxRefusalQuote] equals.
		return coerced{}, invalid("field %q is a url and %q has no "+
			"scheme — write https://%s", field.Slug, clip(text), text)
	case parsed.Host == "" && parsed.Opaque == "":
		return coerced{}, invalid("field %q is a url and %q names "+
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
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := json.Unmarshal(got.Value, &text); err != nil {
		return coerced{}, err
	}
	address, err := mail.ParseAddress(text)
	if err != nil {
		return coerced{}, invalid("field %q is an email address and "+
			"%q is not one: %v", field.Slug, clip(text), err)
	}
	// THE ADDRESS, not the display name (`Ana <a@example.com>`), so two
	// spellings of one person's mail are one value a filter matches.
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
		return coerced{}, invalid("field %q holds text and this "+
			"value is %s", field.Slug, clipRaw(raw))
	}
	if len(text) > limit {
		// REFUSED NAMING THE SIZE, never cut: a silently truncated value
		// is one a person will look for later.
		return coerced{}, invalid("field %q holds %d bytes and the "+
			"maximum is %d", field.Slug, len(text), limit)
	}
	encoded, err := json.Marshal(text)
	if err != nil {
		return coerced{}, err
	}
	return coerced{Value: encoded}, nil
}

// clipRaw is a raw JSON value as a refusal shows it — see [clip].
func clipRaw(raw json.RawMessage) string { return clip(string(raw)) }

// clip is what a refusal quotes back of a rejected value: the value WHOLE
// within [MaxRefusalQuote], and past it a statement of its size, never a
// fragment: the caller reads a fragment as the value it sent, making the
// refusal about a value nobody wrote.
func clip(s string) string {
	if len(s) <= MaxRefusalQuote {
		return s
	}
	return fmt.Sprintf("[a %d-byte value, too large to quote]", len(s))
}

// MaxRefusalQuote is the largest rejected value a refusal echoes whole:
// [MaxFieldValueBytes], a guard against a document pasted into a scalar field
// rather than a budget. A refusal that PRESCRIBES a value ([coerceURL],
// [coerceEmail]) quotes one [coerceText] already bounded at that cap, so EQUAL
// suffices, and builds the prescription from the unclipped value rather than
// trust this cap to stay generous. Every other refusal quotes a value nothing
// bounds (prose on a number field, a JSON non-string, via [clipRaw]) and names
// it by size past the cap, as it only identifies what was sent; an oversized
// [MaxTextareaBytes] value is refused by size, never quoted. A shorter cap
// echoes a fragment the caller retries against (as builtin's `clip` says) and
// turns a URL prescription into a dead link nobody typed.
const MaxRefusalQuote = MaxFieldValueBytes

// settleFields runs the table inside the decide, so a value is judged against
// the declarations the record lands on (a field archived between a caller's
// read and its write is the case). It returns a COPY, for the retry reason
// [coerceFields] gives.
func settleFields(ctx context.Context, tx *sql.Tx, project, taskType string,
	values map[string]json.RawMessage, world FieldWorld, zone *time.Location) (
	map[string]json.RawMessage, []caveat.Caveat, error) {

	if len(values) == 0 {
		return values, nil, nil
	}
	declared, err := declaredFields(ctx, tx, project)
	if err != nil {
		return nil, nil, err
	}
	// THE TASK LOOKUP IS THIS SNAPSHOT'S, so a target this node does not
	// hold is refused rather than stored as an id resolving to nothing.
	return coerceFields(declared, values, taskType, fieldRefs{
		world: world,
		zone:  zone,
		task: func(ref string) (string, bool) {
			id, err := resolveTaskID(ctx, tx, ref)
			return id, err == nil
		},
	})
}

// refuseUnsetRequired refuses a patch that clears a field the task must have:
// the update half of requiredFields, which runs only on a create and a project
// move. A field that does not apply cannot be unset, so its type gate and a
// subtask's RequiredInSubtasks flag are requiredFields' own.
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
	return invalid("a %s in project %s requires %s, and this edit "+
		"clears them — set another value rather than emptying the field",
		current.Type, current.Project, words.And(cleared))
}
