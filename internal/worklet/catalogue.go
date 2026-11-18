package worklet

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The workspace catalogues: what a task may BE, and what it may carry.
//
// Types and fields are two documents on two subjects, so a founder renaming
// `bug` never contends with a lead adding a field: the loser would be told to
// retry an edit that conflicted with nothing. A company's catalogue ADDS to the
// builtins, unioned at read time ([EffectiveTypes]), so no catalogue can omit
// `task` and break every create that named no type; a builtin is archived,
// never removed, so the tasks filed under it still render. A field's archive
// is one-way ([archiveIsOneWay]).

// builtinTypes are the task types every company has before it declares any:
// shapes of work, not workflow states (a status says where a task IS). They
// carry `builtin` so a catalogue screen can tell what a company inherited.
var builtinTypes = []TaskType{
	{Slug: "task", Name: "Task", Plural: "Tasks", Builtin: true,
		Description: "Ordinary work with an owner and an end."},
	{Slug: "bug", Name: "Bug", Plural: "Bugs", Builtin: true,
		Description: "Something is wrong and somebody has to make it right."},
	{Slug: "epic", Name: "Epic", Plural: "Epics", Builtin: true,
		Description: "A body of work with tasks under it."},
	{Slug: "story", Name: "Story", Plural: "Stories", Builtin: true,
		Description: "A change stated as what somebody gets from it."},
	{Slug: "spike", Name: "Spike", Plural: "Spikes", Builtin: true,
		Description: "Time boxed to answer a question, not to deliver."},
	{Slug: "chore", Name: "Chore", Plural: "Chores", Builtin: true,
		Description: "Upkeep nobody asked for and everybody needs."},
	// A milestone is a TYPE, not a status or a flag: it needs everything a
	// task has (owner, due date, blockers, watchers, comments), and a separate
	// object would be a second thing to file, route, notify about and report on.
	{Slug: "milestone", Name: "Milestone", Plural: "Milestones", Builtin: true,
		Description: "A date the company committed to, and what has to " +
			"land before it."},
}

// DefaultTaskType is what a create that names none files under. It is a
// builtin, and a catalogue adds to the builtins, so it always names a declared
// type; `create_work_item` tells a model "`task` if you are unsure".
const DefaultTaskType = "task"

// BuiltinTypes is what this build ships, copied so a caller cannot edit it.
func BuiltinTypes() []TaskType {
	return append([]TaskType(nil), builtinTypes...)
}

// MaxTypeName and MaxTypeDescription bound one type's text.
const (
	MaxTypeName        = 64
	MaxTypeDescription = 512
)

// WriteTypes replaces the workspace's task-type catalogue.
//
// WHOLE POST-STATE: the catalogue is small, rarely edited and read on every
// create, and a patch would need a merge rule per type. Builtins need not
// appear (they are unioned at read time), but one may be carried to archive or
// rename it.
func (w *Writer) WriteTypes(ctx context.Context, opID string, types []TaskType) (WriteResult, error) {
	clean, err := checkTypes(types)
	if err != nil {
		return WriteResult{}, err
	}
	subject := CatalogueSubject(CatalogueTypes)
	// The family: every task is validated against this document, so a
	// deferred catalogue record concerns every read that could return one.
	scope := ScopeSet{Subject: true}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			post := TypeCatalogue{
				V: DocumentVersion, Types: clean, UpdatedAt: at,
			}
			// Quiet but named: a wake per catalogue edit would page the whole
			// company for a renamed dropdown, yet the feed must still say one was.
			return w.decide(ctx, tx, stamp, subject, OpPatch, ChangeCatalogue, scope, opID,
				post, nil, at)
		},
	})
}

// WriteFields replaces the workspace's field declarations, on a subject of
// its own so a field edit never contends with a type edit (see the file head).
func (w *Writer) WriteFields(ctx context.Context, opID string, fields []FieldDef) (WriteResult, error) {
	if err := checkFields(fields); err != nil {
		return WriteResult{}, err
	}
	subject := CatalogueSubject(CatalogueFields)
	scope := ScopeSet{Subject: true}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			current, held, err := readFieldCatalogue(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			if held {
				if err := archiveIsOneWay(current.Fields, fields); err != nil {
					return statelog.Decision{}, err
				}
			}
			post := FieldCatalogue{
				V: DocumentVersion, UpdatedAt: at,
				Fields: stampFields(current.Fields, fields, w.Actor, at),
				// Moves on every fields edit; a task's policy stamp records the
				// version it validated against. Derived inside this snapshot, never
				// sent by the caller: two writers could choose the same one.
				PolicyVersion: current.PolicyVersion + 1,
			}
			// Quiet but named, as in [Writer.WriteTypes].
			return w.decide(ctx, tx, stamp, subject, OpPatch, ChangeCatalogue, scope, opID,
				post, nil, at)
		},
	})
}

// checkTypes refuses a catalogue a create could not be validated against.
func checkTypes(types []TaskType) ([]TaskType, error) {
	if len(types) > MaxTypes {
		return nil, invalid("the type catalogue carries %d types "+
			"and the maximum is %d", len(types), MaxTypes)
	}
	out := make([]TaskType, 0, len(types))
	seen := make(map[string]bool, len(types))
	for _, t := range types {
		t.Slug = strings.ToLower(strings.TrimSpace(t.Slug))
		t.Name = strings.TrimSpace(t.Name)
		switch {
		case t.Slug == "":
			return nil, invalid("a task type has no slug — the slug " +
				"is what a task's `type` field holds and what a model types")
		case !ValidSlug(t.Slug):
			return nil, invalid("%q is not a slug — a type's slug is "+
				"what a query and a task row both carry", t.Slug)
		case seen[t.Slug]:
			// Two types under one slug collide on the table's primary key, so
			// the apply would write one and silently drop the other.
			return nil, invalid("the type catalogue names %q twice",
				t.Slug)
		case t.Name == "":
			return nil, invalid("type %s has no name", t.Slug)
		case len(t.Name) > MaxTypeName:
			return nil, invalid("type %s's name is %d bytes and the "+
				"maximum is %d", t.Slug, len(t.Name), MaxTypeName)
		case len(t.Description) > MaxTypeDescription:
			return nil, invalid("type %s's description is %d bytes "+
				"and the maximum is %d", t.Slug, len(t.Description),
				MaxTypeDescription)
		}
		seen[t.Slug] = true
		out = append(out, t)
	}
	// Names collide too, case-insensitively (the rule `name_norm`'s index
	// is annotated with): a type resolves by name as well as slug, since a model
	// writes what it read off a board. Over the EFFECTIVE set, which is what
	// resolves: a declared `defect` named "Bug" collides with the shipped `bug`.
	// A declared type with a builtin's SLUG replaces it ([EffectiveTypes]).
	named := make([]namedDeclaration, 0, len(out)+len(builtinTypes))
	for _, t := range EffectiveTypes(out) {
		named = append(named, namedDeclaration{
			ident: t.Slug, name: t.Name, archived: t.Archived,
		})
	}
	if err := checkNames("task types", named); err != nil {
		return nil, err
	}
	return out, nil
}

// stampFields carries a declaration's creation facts.
//
// The STORED ones, never the caller's (the rule [Writer.SaveView] states for
// the same columns): a whole post-state carrying its own `created_by` would let
// the next edit re-attribute a field somebody else declared. A field the
// snapshot does not hold is new and takes this write's actor and clock.
//
// Inside the decide, because only the snapshot the record lands beside knows
// what was declared. It returns a copy for [coerceFields]'s reason: the decide
// reruns on a retry and must not start from a slice the last attempt changed.
func stampFields(current, post []FieldDef, actor string, at time.Time) []FieldDef {
	held := make(map[string]FieldDef, len(current))
	for _, field := range current {
		held[field.ID] = field
	}
	out := make([]FieldDef, len(post))
	copy(out, post)
	for i := range out {
		if was, declared := held[out[i].ID]; declared {
			out[i].CreatedBy, out[i].CreatedAt = was.CreatedBy, was.CreatedAt
			continue
		}
		out[i].CreatedBy, out[i].CreatedAt = actor, at
	}
	return out
}

// checkFields refuses a declaration a value could not be validated against.
func checkFields(fields []FieldDef) error {
	if len(fields) > MaxFieldsPerDocument {
		return invalid("the field catalogue declares %d fields and "+
			"one document's maximum is %d — a task's effective set is the "+
			"workspace's plus its project's, so this bound is half of it",
			len(fields), MaxFieldsPerDocument)
	}
	ids := make(map[string]bool, len(fields))
	slugs := make(map[string]bool, len(fields))
	options := 0
	for i := range fields {
		f := &fields[i]
		f.Slug = strings.ToLower(strings.TrimSpace(f.Slug))
		f.Name = strings.TrimSpace(f.Name)
		switch {
		case f.ID == "":
			// Values are keyed by id, so a field can move between the workspace
			// and a project keeping them; a declaration with none orphans them all.
			return invalid("field %q has no id — values are keyed "+
				"by id so a field can move between the workspace and a "+
				"project keeping them", f.Slug)
		case ids[f.ID]:
			return invalid("the field catalogue names id %s twice",
				f.ID)
		case f.Slug == "" || !ValidSlug(f.Slug):
			return invalid("%q is not a field slug — the slug is "+
				"what `f.<slug>` resolves and what a model types", f.Slug)
		case !f.Archived && slugs[f.Slug]:
			// A colliding slug makes `f.<slug>` resolve to whichever row the
			// index reaches first. An archived field's slug is free: a restore is a
			// NEW declaration under a new id ([archiveIsOneWay]) carrying the
			// company's same word, and nothing resolves against an archived field.
			return invalid("the field catalogue declares %q twice, "+
				"so f.%s would resolve to whichever row was read first "+
				"(an archived declaration does not count — its slug is free)",
				f.Slug, f.Slug)
		case f.Name == "":
			return invalid("field %s has no name", f.Slug)
		case !f.Type.Valid():
			return invalid("%q is not a field type", f.Type)
		case len(f.Config.Options) > MaxOptions:
			return invalid("field %s declares %d options and the "+
				"maximum is %d", f.Slug, len(f.Config.Options), MaxOptions)
		}
		if err := checkOptions(f); err != nil {
			return err
		}
		if err := checkConfig(f); err != nil {
			return err
		}
		ids[f.ID] = true
		if !f.Archived {
			slugs[f.Slug] = true
		}
		options += len(f.Config.Options)
	}
	if options > MaxOptionsPerDocument {
		return invalid("the field catalogue declares %d options "+
			"across every field and the maximum is %d", options,
			MaxOptionsPerDocument)
	}
	// Field names too, on the types' rule: `f.<name>` is the third tier of
	// the resolution every filter, grouping and sort goes through.
	named := make([]namedDeclaration, 0, len(fields))
	for _, f := range fields {
		named = append(named, namedDeclaration{
			ident: f.Slug, name: f.Name, archived: f.Archived,
		})
	}
	return checkNames("fields", named)
}

// checkOptions refuses a choice list a value could not resolve against.
func checkOptions(f *FieldDef) error {
	ids := make(map[string]bool, len(f.Config.Options))
	slugs := make(map[string]bool, len(f.Config.Options))
	for i := range f.Config.Options {
		o := &f.Config.Options[i]
		o.Slug = strings.ToLower(strings.TrimSpace(o.Slug))
		o.Name = strings.TrimSpace(o.Name)
		switch {
		case o.ID == "":
			return invalid("an option of field %s has no id — a "+
				"stored value holds the id, so a renamed option keeps every "+
				"task that chose it", f.Slug)
		case ids[o.ID]:
			return invalid("field %s names option id %s twice",
				f.Slug, o.ID)
		case o.Slug == "" || !ValidSlug(o.Slug):
			return invalid("%q is not an option slug on field %s",
				o.Slug, f.Slug)
		case !o.Archived && slugs[o.Slug]:
			// An archived option's slug is free, as a field's is: nothing
			// resolves `f.<slug>=<option>` against a retired choice.
			return invalid("field %s declares option %q twice "+
				"(an archived one does not count — its slug is free)",
				f.Slug, o.Slug)
		case o.Name == "":
			return invalid("option %s of field %s has no name",
				o.Slug, f.Slug)
		case len(o.Name) > MaxOptionName:
			return invalid("option %s of field %s has a %d-byte "+
				"name and the maximum is %d", o.Slug, f.Slug, len(o.Name),
				MaxOptionName)
		}
		ids[o.ID] = true
		if !o.Archived {
			slugs[o.Slug] = true
		}
	}
	// Option names too, the sharpest case: [OptionIDs] maps a lowercased NAME
	// to an id, so names differing only in case would collapse to whichever came
	// last, and a value written by name would change when the list was reordered.
	named := make([]namedDeclaration, 0, len(f.Config.Options))
	for _, o := range f.Config.Options {
		named = append(named, namedDeclaration{
			ident: o.Slug, name: o.Name, archived: o.Archived,
		})
	}
	if err := checkNames("options of field "+f.Slug, named); err != nil {
		return err
	}
	// Only the three types that have options take a list; one on any other
	// is a caller who believes they configured something.
	if len(f.Config.Options) > 0 {
		switch f.Type {
		case FieldDropdown, FieldLabels, FieldRelationship:
		default:
			return invalid("field %s is a %s and declares %d "+
				"options — only %s, %s and %s have them", f.Slug, f.Type,
				len(f.Config.Options), FieldDropdown, FieldLabels,
				FieldRelationship)
		}
	}
	return nil
}

// archiveIsOneWay refuses a declaration that un-archives a field, which is
// what makes [FieldDef.Archived]'s rule true: a field back under its old id
// would silently re-admit values validated against a definition nobody has
// seen for a year. Restoring one means declaring a NEW field with a new id.
func archiveIsOneWay(current, post []FieldDef) error {
	archived := make(map[string]string, len(current))
	for _, f := range current {
		if f.Archived {
			archived[f.ID] = f.Slug
		}
	}
	for _, f := range post {
		if slug, was := archived[f.ID]; was && !f.Archived {
			return invalid("field %s (%s) is archived, and an "+
				"archive is one-way: its values left the value table, so "+
				"bringing the id back would re-admit them against a "+
				"definition nobody has seen since. Declare a new field with a "+
				"new id instead: %w", slug, f.ID, statelog.ErrConflict)
		}
	}
	return nil
}

// readTypeCatalogue reads the workspace types inside a write's own snapshot.
func readTypeCatalogue(ctx context.Context, tx *sql.Tx) (TypeCatalogue, bool, error) {
	return readDocument(ctx, tx, CatalogueSubject(CatalogueTypes),
		func(c *TypeCatalogue, version uint64) { c.Version = version })
}

// readFieldCatalogue reads the workspace fields inside a write's own snapshot.
func readFieldCatalogue(ctx context.Context, tx *sql.Tx) (FieldCatalogue, bool, error) {
	return readDocument(ctx, tx, CatalogueSubject(CatalogueFields),
		func(c *FieldCatalogue, version uint64) { c.Version = version })
}

// EffectiveTypes is the builtins plus whatever the company declared.
//
// Builtins first, so `task`, which every create defaults to, is not buried
// under bespoke types. A declared type with a builtin's slug REPLACES it, which
// is how a company renames `bug` without losing the tasks filed under it.
func EffectiveTypes(declared []TaskType) []TaskType {
	out := make([]TaskType, 0, len(builtinTypes)+len(declared))
	overridden := make(map[string]TaskType, len(declared))
	for _, t := range declared {
		overridden[t.Slug] = t
	}
	for _, t := range builtinTypes {
		if replacement, held := overridden[t.Slug]; held {
			// Builtin survives the override: it says this build ships the slug,
			// a fact about the engine rather than the company's wording.
			replacement.Builtin = true
			out = append(out, replacement)
			continue
		}
		out = append(out, t)
	}
	for _, t := range declared {
		if _, isBuiltin := builtinSlug(t.Slug); !isBuiltin {
			out = append(out, t)
		}
	}
	return out
}

func builtinSlug(slug string) (TaskType, bool) {
	for _, t := range builtinTypes {
		if t.Slug == slug {
			return t, true
		}
	}
	return TaskType{}, false
}
