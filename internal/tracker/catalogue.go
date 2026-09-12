package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The workspace catalogues: what a task may BE, and what it may carry.
//
// # Two objects, and therefore two subjects
//
// Types and fields are one family and two documents, arbitrated separately.
// Folding them into one would make a founder renaming `bug` contend with a
// lead adding a field, which are unrelated edits a company makes at unrelated
// times — and the loser of that contention is a person told to retry an edit
// that never conflicted with anything.
//
// # The builtin types cannot be removed, only archived
//
// A company's catalogue ADDS to what this build ships rather than replacing
// it. The applier writes exactly the document's rows, so the union happens at
// read time — and it has to, because the alternative is a catalogue that omits
// `task` and breaks every create that did not name a type. A builtin a company
// has stopped using is ARCHIVED, which keeps every task already filed under it
// rendering as what it is: a type that vanished would leave those tasks naming
// nothing.
//
// # A field's archive is one-way, and that is enforced here
//
// [FieldDef.Archived] says so at the field, and nothing made it true. A field
// that came back with its old id would silently re-admit values validated
// against a definition nobody has seen for a year — so un-archiving is refused
// naming the field, and restoring one means declaring a NEW field with a new
// id.

// The builtin task types, which every company has before it declares any.
//
// SEVEN, and each is a shape of work rather than a workflow state: a status
// says where a task IS and a type says what it is. They carry `builtin` so a
// catalogue screen can tell what a company chose from what it inherited.
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
	// A MILESTONE IS A DATE SOMEBODY COMMITTED TO, which is why it is a
	// TYPE rather than a status or a flag: it is an item on a board with
	// an owner, a due date and things blocking it, and everything the
	// tracker does to a task — a dependency, a watcher, a comment thread
	// — is exactly what a milestone needs. A separate object would have
	// been a second thing to file, route, notify about and report on.
	{Slug: "milestone", Name: "Milestone", Plural: "Milestones", Builtin: true,
		Description: "A date the company committed to, and what has to " +
			"land before it."},
}

// DefaultTaskType is what a create that names none files under.
//
// EVERY COMPANY HAS IT, because it is a builtin and a catalogue adds to the
// builtins rather than replacing them — so this default can never name a type
// the company does not declare. `create_work_item` has always told a model
// "`task` if you are unsure", and this is where that is true.
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
// WHOLE POST-STATE: the catalogue is small, edited rarely and read on every
// create, and a patch shape would need a merge rule per type. The BUILTINS
// need not appear — they are unioned at read time — but a company may carry
// one to archive it or to rename it for its own vocabulary.
func (w *Writer) WriteTypes(ctx context.Context, opID string, types []TaskType) (WriteResult, error) {
	clean, err := checkTypes(types)
	if err != nil {
		return WriteResult{}, err
	}
	subject := CatalogueSubject(CatalogueTypes)
	// THE FAMILY, which is what a catalogue subject resolves to anyway:
	// every task in the company is validated against this document, so a
	// deferred catalogue record concerns every read that could return one.
	scope := ScopeSet{Subject: true}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			post := TypeCatalogue{
				V: DocumentVersion, Types: clean, UpdatedAt: at,
			}
			// THE CATALOGUE IS EDITED QUIETLY AND STILL NAMES ITSELF: a
			// wake per catalogue edit would page the whole company for a
			// renamed dropdown, and the feed still has to be able to say
			// a dropdown was renamed.
			return w.decide(subject, OpPatch, ChangeCatalogue, scope, opID,
				post, nil, at)
		},
	})
}

// WriteFields replaces the workspace's field declarations.
//
// A SEPARATE SUBJECT from the types, so a field edit and a type edit never
// contend — see the file head.
func (w *Writer) WriteFields(ctx context.Context, opID string, fields []FieldDef) (WriteResult, error) {
	if err := checkFields(fields); err != nil {
		return WriteResult{}, err
	}
	subject := CatalogueSubject(CatalogueFields)
	scope := ScopeSet{Subject: true}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
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
				V: DocumentVersion, Fields: fields, UpdatedAt: at,
				// THE POLICY VERSION MOVES ON EVERY FIELDS EDIT, and it
				// is what a task's policy stamp records having validated
				// against. Derived from the stored one inside this
				// snapshot rather than sent by the caller: a version a
				// writer chose is one two writers can choose alike.
				PolicyVersion: current.PolicyVersion + 1,
			}
			// THE CATALOGUE IS EDITED QUIETLY AND STILL NAMES ITSELF: a
			// wake per catalogue edit would page the whole company for a
			// renamed dropdown, and the feed still has to be able to say
			// a dropdown was renamed.
			return w.decide(subject, OpPatch, ChangeCatalogue, scope, opID,
				post, nil, at)
		},
	})
}

// checkTypes refuses a catalogue a create could not be validated against.
func checkTypes(types []TaskType) ([]TaskType, error) {
	if len(types) > MaxTypes {
		return nil, fmt.Errorf("tracker: the type catalogue carries %d types "+
			"and the maximum is %d", len(types), MaxTypes)
	}
	out := make([]TaskType, 0, len(types))
	seen := make(map[string]bool, len(types))
	for _, t := range types {
		t.Slug = strings.ToLower(strings.TrimSpace(t.Slug))
		t.Name = strings.TrimSpace(t.Name)
		switch {
		case t.Slug == "":
			return nil, fmt.Errorf("tracker: a task type has no slug — the slug " +
				"is what a task's `type` field holds and what a model types")
		case !ValidSlug(t.Slug):
			return nil, fmt.Errorf("tracker: %q is not a slug — a type's slug is "+
				"what a query and a task row both carry", t.Slug)
		case seen[t.Slug]:
			// TWO TYPES UNDER ONE SLUG collide on the table's own
			// primary key, so the apply would write one and drop the
			// other with nothing to say so.
			return nil, fmt.Errorf("tracker: the type catalogue names %q twice",
				t.Slug)
		case t.Name == "":
			return nil, fmt.Errorf("tracker: type %s has no name", t.Slug)
		case len(t.Name) > MaxTypeName:
			return nil, fmt.Errorf("tracker: type %s's name is %d bytes and the "+
				"maximum is %d", t.Slug, len(t.Name), MaxTypeName)
		case len(t.Description) > MaxTypeDescription:
			return nil, fmt.Errorf("tracker: type %s's description is %d bytes "+
				"and the maximum is %d", t.Slug, len(t.Description),
				MaxTypeDescription)
		}
		seen[t.Slug] = true
		out = append(out, t)
	}
	return out, nil
}

// checkFields refuses a declaration a value could not be validated against.
func checkFields(fields []FieldDef) error {
	if len(fields) > MaxFieldsPerDocument {
		return fmt.Errorf("tracker: the field catalogue declares %d fields and "+
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
			// THE ID IS WHAT VALUES ARE KEYED BY, which is what lets a
			// field MOVE between the workspace and a project keeping
			// every stored value. A declaration with none would orphan
			// them all.
			return fmt.Errorf("tracker: field %q has no id — values are keyed "+
				"by id so a field can move between the workspace and a "+
				"project keeping them", f.Slug)
		case ids[f.ID]:
			return fmt.Errorf("tracker: the field catalogue names id %s twice",
				f.ID)
		case f.Slug == "" || !ValidSlug(f.Slug):
			return fmt.Errorf("tracker: %q is not a field slug — the slug is "+
				"what `f.<slug>` resolves and what a model types", f.Slug)
		case !f.Archived && slugs[f.Slug]:
			// A COLLIDING SLUG makes `f.<slug>` ambiguous, and the
			// resolution would pick whichever row the index reached
			// first.
			//
			// AN ARCHIVED FIELD'S SLUG IS FREE, which is what makes the
			// restore path this package documents possible at all: an
			// archive is one-way, so bringing a field back means a NEW
			// declaration under a new id — and it would carry the same
			// slug, because the slug is the word the company uses.
			// Nothing resolves against an archived field: its values
			// left the value table when it was archived.
			return fmt.Errorf("tracker: the field catalogue declares %q twice, "+
				"so f.%s would resolve to whichever row was read first "+
				"(an archived declaration does not count — its slug is free)",
				f.Slug, f.Slug)
		case f.Name == "":
			return fmt.Errorf("tracker: field %s has no name", f.Slug)
		case !f.Type.Valid():
			return fmt.Errorf("tracker: %q is not a field type", f.Type)
		case len(f.Config.Options) > MaxOptions:
			return fmt.Errorf("tracker: field %s declares %d options and the "+
				"maximum is %d", f.Slug, len(f.Config.Options), MaxOptions)
		}
		if err := checkOptions(f); err != nil {
			return err
		}
		ids[f.ID] = true
		if !f.Archived {
			slugs[f.Slug] = true
		}
		options += len(f.Config.Options)
	}
	if options > MaxOptionsPerDocument {
		return fmt.Errorf("tracker: the field catalogue declares %d options "+
			"across every field and the maximum is %d", options,
			MaxOptionsPerDocument)
	}
	return nil
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
			return fmt.Errorf("tracker: an option of field %s has no id — a "+
				"stored value holds the id, so a renamed option keeps every "+
				"task that chose it", f.Slug)
		case ids[o.ID]:
			return fmt.Errorf("tracker: field %s names option id %s twice",
				f.Slug, o.ID)
		case o.Slug == "" || !ValidSlug(o.Slug):
			return fmt.Errorf("tracker: %q is not an option slug on field %s",
				o.Slug, f.Slug)
		case !o.Archived && slugs[o.Slug]:
			// AN ARCHIVED OPTION'S SLUG IS FREE, on the field's own
			// rule above and for the same reason: nothing resolves
			// `f.<slug>=<option>` against a retired choice.
			return fmt.Errorf("tracker: field %s declares option %q twice "+
				"(an archived one does not count — its slug is free)",
				f.Slug, o.Slug)
		case o.Name == "":
			return fmt.Errorf("tracker: option %s of field %s has no name",
				o.Slug, f.Slug)
		}
		ids[o.ID] = true
		if !o.Archived {
			slugs[o.Slug] = true
		}
	}
	// OPTIONS BELONG TO THE THREE TYPES THAT HAVE THEM, and a list on any
	// other is a caller who believes they configured something.
	if len(f.Config.Options) > 0 {
		switch f.Type {
		case FieldDropdown, FieldLabels, FieldRelationship:
		default:
			return fmt.Errorf("tracker: field %s is a %s and declares %d "+
				"options — only %s, %s and %s have them", f.Slug, f.Type,
				len(f.Config.Options), FieldDropdown, FieldLabels,
				FieldRelationship)
		}
	}
	return nil
}

// archiveIsOneWay refuses a declaration that un-archives a field.
//
// The rule is [FieldDef.Archived]'s own, and this is what makes it true: a
// field that came back with its old id would silently re-admit values
// validated against a definition nobody has seen for a year. Restoring one
// means declaring a NEW field, with a new id, which is a decision somebody
// makes rather than a checkbox they clear.
func archiveIsOneWay(current, post []FieldDef) error {
	archived := make(map[string]string, len(current))
	for _, f := range current {
		if f.Archived {
			archived[f.ID] = f.Slug
		}
	}
	for _, f := range post {
		if slug, was := archived[f.ID]; was && !f.Archived {
			return fmt.Errorf("tracker: field %s (%s) is archived, and an "+
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
// THE UNION, and the order is builtins first: a company's own vocabulary is
// what it added, and a screen listing `task` after six bespoke types would
// bury the one every create defaults to. A declared type with a builtin's slug
// REPLACES it, which is how a company renames `bug` to its own word without
// losing the tasks already filed under the slug.
func EffectiveTypes(declared []TaskType) []TaskType {
	out := make([]TaskType, 0, len(builtinTypes)+len(declared))
	overridden := make(map[string]TaskType, len(declared))
	for _, t := range declared {
		overridden[t.Slug] = t
	}
	for _, t := range builtinTypes {
		if replacement, held := overridden[t.Slug]; held {
			// THE BUILTIN FLAG SURVIVES THE OVERRIDE, because what it
			// says is "this build ships this slug" — a fact about the
			// engine rather than about the company's wording.
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
