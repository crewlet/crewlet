package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/statelog"
)

// What a tool may write on a project. Its name, purpose and owning unit are
// the chart's ([Writer.ApplyChart] only), so a model cannot rename a project out
// from under the config. Its tag set is its own object on its own subject
// ([Writer.WriteTags]). Its policy is here: field declarations, default
// assignee, target date and archived flag — one verb, because all four are a
// lead's rare decision, while a tag is every seat's, several times a day, and
// folding tags in would make every tag add contend with a policy edit.

// ProjectEdit is one change to a project's own policy.
//
// EVERY FACET IS A POINTER: an empty field list and no default assignee are
// real settings, so nil ("leave alone") must differ from "set to nothing".
type ProjectEdit struct {
	// Fields replaces the project's own field declarations: WHOLE
	// POST-STATE, like the workspace catalogue's, since a patch would need a
	// merge rule per field and the list is small and rarely edited.
	Fields *[]FieldDef

	// DefaultAssignee is who unassigned work in this project lands on. The
	// empty string is a real setting: it means triage.
	DefaultAssignee *string

	// TargetDate is when the lead means the project to be finished: a
	// calendar date, or an instant stored as its day on the company's clock,
	// with a caveat ([coerceDay]). The empty string clears it. The lead's,
	// not the chart's: it is re-set whenever the plan moves.
	TargetDate *string

	// Archived stops the project taking new work. It takes a person, never an
	// agent ([ProjectAuthority.Archive]): it is a decision about the company.
	Archived *bool
}

// Empty reports an edit that sets nothing.
func (e ProjectEdit) Empty() bool {
	return e.Fields == nil && e.DefaultAssignee == nil && e.TargetDate == nil &&
		e.Archived == nil
}

// ProjectAuthority is what a caller may do to a project's policy.
//
// A value per call rather than a bool on the writer: the answer is about a
// pair, who asks and about which project. It states the DECISION, not its
// inputs: the caller asks [authz.ActionProjectPolicy] and
// [authz.ActionProjectArchive] and states the answers, so the policy lives
// once, in internal/authz; this package enforces only which facet each
// answer unlocks.
type ProjectAuthority struct {
	// Policy reports a caller authorized to declare this project's
	// fields, its default assignee and its target date: its lead, or an
	// admin, whose permission overrides every relation.
	Policy bool

	// Archive reports a caller authorized to take the project out of
	// circulation: the same relation AND a principal that is not an agent.
	// Separate from Policy so a refusal can name the facet, not just "no".
	Archive bool
}

// WriteProject applies one edit to a project's own policy.
func (w *Writer) WriteProject(ctx context.Context, opID, key string,
	edit ProjectEdit, authority ProjectAuthority) (WriteResult, error) {

	key = ProjectKey(key)
	switch {
	case key == "":
		return WriteResult{}, invalid("a project edit names no project")
	case edit.Empty():
		// Names the tool that adds a tag, for a caller who sent one here.
		return WriteResult{}, invalid("a project edit of %s sets nothing. Its "+
			"name, purpose and owning unit come from the org chart, and a tag "+
			"is added with `tags_add`", key)
	case !authority.Policy:
		return WriteResult{}, forbidden("%s's field declarations, default "+
			"assignee and target date are the project lead's, or an admin's — "+
			"they decide how everybody's work in it is filed and when it is "+
			"due, which is not a call anybody else makes for the team", key)
	case edit.Archived != nil && !authority.Archive:
		// Separate from the lead gate: this refusal names the facet.
		return WriteResult{}, forbidden("archiving %s takes the project's "+
			"lead or an admin, acting as a person rather than as an agent — a "+
			"project holds the company's tasks, and putting one out of "+
			"circulation is a decision about the company rather than about how "+
			"its work is filed", key)
	}
	if edit.Fields != nil {
		// Before the snapshot: a malformed declaration is malformed whatever
		// the project holds, and refusing it here keeps it off the log.
		if err := checkFields(*edit.Fields); err != nil {
			return WriteResult{}, err
		}
	}
	// The target date is coerced here for the same reason, so the decide
	// applies the canonical day; on a copy, leaving the caller's value alone.
	var caveats []caveat.Caveat
	if edit.TargetDate != nil {
		target := strings.TrimSpace(*edit.TargetDate)
		if target != "" {
			day, truncated, err := coerceDay("the target date", target, w.zone())
			if err != nil {
				return WriteResult{}, err
			}
			if truncated {
				caveats = append(caveats, caveat.TargetDateTruncated(target, day,
					zoneName(w.zone())))
			}
			target = day
		}
		edit.TargetDate = &target
	}
	subject := ProjectSubject(key)
	// The container too: field declarations and an archive change what every
	// read of every task in this project answers.
	scope := ScopeSet{Subject: true, Container: key}
	at := w.Now()
	result, err := w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			current, held, err := readProject(ctx, tx, key)
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !held:
				return statelog.Decision{}, fmt.Errorf("worklet: project %s is "+
					"not on this node — a project is created by the org chart, "+
					"so check the key or wait for the config to apply: %w",
					key, statelog.ErrUnavailable)
			}
			next, changed, err := applyProjectEdit(current, edit, w.Actor, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !changed {
				return statelog.Decision{}, nil
			}
			decision, err := w.decide(ctx, tx, stamp, subject, OpPatch, ChangeProjectUpdated,
				scope, opID, next, nil, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
	if err != nil {
		return result, err
	}
	result.Caveats = append(result.Caveats, caveats...)
	return result, nil
}

// applyProjectEdit is the whole rule, as a pure function over one project.
//
// Pure for [applyTagEdit]'s reason: the refusal a lead meets (un-archiving a
// field) is a comparison of two lists, testable without a published record.
func applyProjectEdit(current Project, edit ProjectEdit, actor string,
	at time.Time) (Project, bool, error) {

	next := current
	changed := false
	// Only a fields edit moves [Project.PolicyVersion]: it is what a task is
	// validated against, and neither an assignee nor an archive changes that.
	policy := false

	if edit.Fields != nil {
		if err := archiveIsOneWay(current.Fields, *edit.Fields); err != nil {
			return Project{}, false, err
		}
		// Stamped before the comparison, or an unchanged resubmission would
		// differ by the two facts a caller never sends and write an empty record.
		stamped := stampFields(current.Fields, *edit.Fields, actor, at)
		if !sameFields(current.Fields, stamped) {
			next.Fields = stamped
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
	if edit.TargetDate != nil && *edit.TargetDate != current.TargetDate {
		next.TargetDate = *edit.TargetDate
		changed = true
	}
	if edit.Archived != nil && *edit.Archived != current.Archived {
		next.Archived = *edit.Archived
		changed = true
	}
	if !changed {
		// A form that submits every control lands here; no record is right.
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
// Deep equality rather than a list of fields that matter: a list would
// silently miss a newly added field, and the policy version would stop moving
// for exactly the change it records.
func sameFields(a, b []FieldDef) bool { return reflect.DeepEqual(a, b) }
