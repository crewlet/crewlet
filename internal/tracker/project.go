package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// What a project carries that a TOOL may write, and who may write each.
//
// A project has four kinds of state and only one of them is here:
//
//   - CHART-OWNED — its name, purpose and owning unit. Written by
//     [Writer.ApplyChart] and by nothing else, because they are facts a founder
//     wrote in the config: a tool that could set them would let a model rename
//     the company's projects out from under the file they came from.
//   - ITS TAG SET — its own object on its own subject, so that every seat's add
//     never contends with a lead's policy edit. [Writer.WriteTags].
//   - ITS POLICY — the field declarations, the default assignee and the
//     archived flag. Those are here.
//
// # Why the policy facets are one verb and the tags are not
//
// Because they share an authority and a contention profile. All three below are
// a LEAD's decision about how their project runs, edited a handful of times a
// quarter; a tag is every seat's, several times a day. One verb over the first
// three costs nothing and keeps "may you set your project's policy" a single
// question; folding the fourth in would make every tag add contend with them.

// ProjectEdit is one change to a project's own policy.
//
// EVERY FACET IS A POINTER, because the zero value of two of them is a
// meaningful SETTING: no fields declared and no default assignee are each a
// state a project is deliberately left in, and a struct that could not tell
// "leave this alone" from "set it to nothing" would clear a project's field
// list every time somebody set its default assignee.
type ProjectEdit struct {
	// Fields replaces the project's own field declarations. WHOLE
	// POST-STATE, like the workspace catalogue's, and for the same reason:
	// a patch shape would need a merge rule per field, and the list is
	// small, read on every create and edited rarely.
	Fields *[]FieldDef

	// DefaultAssignee is who unassigned work in this project lands on. The
	// empty string is a real setting: it means triage.
	DefaultAssignee *string

	// Archived stops the project taking new work. An OPERATOR's, not a
	// lead's — a project holds the company's tasks, and taking one out of
	// circulation is a decision about the company.
	Archived *bool
}

// Empty reports an edit that sets nothing.
func (e ProjectEdit) Empty() bool {
	return e.Fields == nil && e.DefaultAssignee == nil && e.Archived == nil
}

// ProjectAuthority is what a caller may do to a project's policy.
//
// TWO AUTHORITIES, EITHER OF WHICH IS ENOUGH for the three facets a lead owns,
// and this is [PersonAuthority]'s shape rather than a ladder. An OPERATOR is a
// person acting through their own credential — the founder at the dashboard,
// or the assistant they run the company through — and they are not a seat at
// all, so a gate that admitted only the lead refused the one actor the whole
// operator surface exists for. It was reachable: `/operator/mcp` resolves the
// lead from the ORG CHART by handle, an operator token carries its own name
// rather than a seat's, so the lookup answered false and `write_project` was
// refused for every operator in every company — including the founder
// declaring the fields a project files work under.
type ProjectAuthority struct {
	// Lead reports whether the actor leads the project or sits above it.
	Lead bool

	// Operator reports whether the actor is acting through a person's own
	// credential rather than as a seat. It is authority over the policy in
	// its own right, and archiving takes it SPECIFICALLY — a lead may say
	// how their project's work is filed, and only a person may take the
	// project out of circulation.
	Operator bool
}

// WriteProject applies one edit to a project's own policy.
func (w *Writer) WriteProject(ctx context.Context, opID, key string,
	edit ProjectEdit, authority ProjectAuthority) (WriteResult, error) {

	key = ProjectKey(key)
	switch {
	case key == "":
		return WriteResult{}, fmt.Errorf("tracker: a project edit names no project")
	case edit.Empty():
		return WriteResult{}, fmt.Errorf("tracker: a project edit of %s sets "+
			"nothing — the name, purpose and owning unit come from the org "+
			"chart, and the tags are write_project(tags.add)", key)
	case !authority.Lead && !authority.Operator:
		return WriteResult{}, fmt.Errorf("tracker: %s's field declarations "+
			"and default assignee are the project lead's or a person's own — "+
			"they decide how everybody's work in it is filed, which is not a "+
			"call one other seat makes for the team", key)
	case edit.Archived != nil && !authority.Operator:
		// NAMED SEPARATELY FROM THE LEAD GATE, because a lead who hit
		// this one did have authority over the other two facets and
		// the useful answer is which facet, not "no".
		return WriteResult{}, fmt.Errorf("tracker: archiving %s takes a "+
			"person's own credential — a project holds the company's tasks, "+
			"and putting one out of circulation is a decision about the "+
			"company rather than about how its work is filed", key)
	}
	if edit.Fields != nil {
		// BEFORE THE SNAPSHOT, because it is a property of the argument:
		// a malformed declaration is malformed whatever the project
		// currently holds, and refusing it here keeps a doomed edit off
		// the log entirely.
		if err := checkFields(*edit.Fields); err != nil {
			return WriteResult{}, err
		}
	}
	subject := ProjectSubject(key)
	// THE CONTAINER, not just the subject: a field declaration and an
	// archive both change what every read of every task in this project
	// answers, so a deferred record here concerns the whole project.
	scope := ScopeSet{Subject: true, Container: key}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readProject(ctx, tx, key)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !held:
				return statelog.Decision{}, fmt.Errorf("tracker: project %s is "+
					"not on this node — a project is created by the org chart, "+
					"so check the key or wait for the config to apply: %w",
					key, statelog.ErrUnavailable)
			}
			next, changed, err := applyProjectEdit(current, edit, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !changed {
				return statelog.Decision{}, nil
			}
			decision, err := w.decide(subject, OpPatch, ChangeProjectUpdated,
				scope, opID, next, nil, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
}

// applyProjectEdit is the whole rule, as a pure function over one project.
//
// PURE for [applyTagEdit]'s reason: the one refusal a lead actually meets — an
// un-archive of a field — is a comparison between two lists, and a rule only
// reachable through a published record is a rule nobody re-measures.
func applyProjectEdit(current Project, edit ProjectEdit, at time.Time) (
	Project, bool, error) {

	next := current
	changed := false
	// POLICY MOVES ON A FIELDS EDIT and on nothing else, which is what
	// [Project.PolicyVersion] records having been validated against: a
	// default assignee is not something a task is checked against, and an
	// archive does not change what a task must carry.
	policy := false

	if edit.Fields != nil {
		if err := archiveIsOneWay(current.Fields, *edit.Fields); err != nil {
			return Project{}, false, err
		}
		if !sameFields(current.Fields, *edit.Fields) {
			next.Fields = *edit.Fields
			changed, policy = true, true
		}
	}
	if edit.DefaultAssignee != nil {
		want := strings.TrimSpace(*edit.DefaultAssignee)
		if want != current.DefaultAssignee {
			next.DefaultAssignee = want
			changed = true
		}
	}
	if edit.Archived != nil && *edit.Archived != current.Archived {
		next.Archived = *edit.Archived
		changed = true
	}
	if !changed {
		// NOTHING TO SAY. Setting a field list to what it already is is
		// the ordinary outcome of a form that submits every control, and
		// an empty decision is a legitimate outcome.
		return current, false, nil
	}
	if policy {
		next.PolicyVersion = current.PolicyVersion + 1
	}
	next.UpdatedAt = at
	return next, true, nil
}

// sameFields reports a facet that would be written back as what it already
// is.
//
// DEEP EQUALITY OVER THE WHOLE VALUE rather than an enumeration of the fields
// that matter, and the reason is what an enumeration does when somebody adds a
// field: it keeps compiling, keeps passing, and silently stops noticing the new
// one — so a project's policy version would stop moving for a change that is
// exactly what the version records. A comparison that is total by construction
// has no such day.
func sameFields(a, b []FieldDef) bool { return reflect.DeepEqual(a, b) }
