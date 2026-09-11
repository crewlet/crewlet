package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Goals: the tier above projects, and the one number this package refuses to
// store.
//
// A goal is a name, owners, dates, a health value and a free-text group label,
// with TARGETS under it — and a target references work: a set of tasks, a set
// of projects, or a number somebody moves by hand.
//
// # Why there is no stored percentage
//
// What a goal is at is what its targets say. A stored number is a second
// answer to a question that already has one, and the two drift the moment a
// task in a target closes without anybody editing the goal — which is the
// ordinary case rather than the exotic one. So [GoalRow.Progress] is COMPUTED
// on every read, from the rows the read already holds, and nothing writes it.
//
// # Why the scope is the goal and the projects it names
//
// An apply writes the goal's own row plus its owners, targets and target
// references — every one of them keyed on the goal id, so the goal's own
// subject covers them. What it does NOT cover is the reverse question: a task
// filtered by `goal=<id>` reads `tracker_goal_target_refs`, and a read scoped
// to a project has to wait for a deferred goal record that would put one of
// its tasks in the answer. So a goal naming projects declares those containers
// too, and one naming tasks without a project declares the DOMAIN — because
// the only honest term for "a task somewhere in this company" is the widest
// one, and a target that names bare task ids gives the writer nothing narrower
// to say.

// MaxGoalName bounds a goal's name, and MaxGoalDescription its prose.
//
// The name is a heading rather than a sentence — a goal tile, a chart axis, a
// chat line — so it takes a task's TITLE bound; the description is the case
// somebody makes for the goal and takes a comment's.
const (
	MaxGoalName        = MaxTitle
	MaxGoalDescription = MaxCommentBody
	MaxGoalGroup       = 128
)

// GoalHealth is what the owner says the goal is doing.
//
// DELIBERATELY NOT INFERRED, and that is the whole of why it is a field. The
// targets say what has MOVED; only a person knows whether the movement is on
// track, and a value computed from the targets would be the stored percentage
// this package refuses under another name.
type GoalHealth string

const (
	// HealthUnset is a goal nobody has judged yet, and it is a real state
	// rather than a missing one: a fresh goal is not "on track".
	HealthUnset    GoalHealth = ""
	HealthOnTrack  GoalHealth = "on_track"
	HealthAtRisk   GoalHealth = "at_risk"
	HealthOffTrack GoalHealth = "off_track"
	HealthDone     GoalHealth = "done"
)

// GoalHealths is the closed set, for a refusal that can name it.
var GoalHealths = []GoalHealth{
	HealthUnset, HealthOnTrack, HealthAtRisk, HealthOffTrack, HealthDone,
}

// Valid reports whether this is one of them.
func (h GoalHealth) Valid() bool {
	for _, known := range GoalHealths {
		if h == known {
			return true
		}
	}
	return false
}

// The target types, and each one is a different arithmetic.
const (
	// TargetTasks is done when every task it names is finished, which is
	// the only target type the engine can move on its own.
	TargetTasks = "tasks"
	// TargetNumber is a start, a goal and a current somebody sets.
	TargetNumber = "number"
	// TargetPercent is the same with an implied unit.
	TargetPercent = "percent"
	// TargetBinary is done or not, with nothing in between.
	TargetBinary = "binary"
)

// TargetTypes is the closed set.
var TargetTypes = []string{TargetTasks, TargetNumber, TargetPercent, TargetBinary}

// WriteGoal saves a goal, creating it or replacing it whole.
//
// WHOLE POST-STATE, like every other document object here. The CALLER mints
// the id, which is what makes a retry idempotent.
func (w *Writer) WriteGoal(ctx context.Context, opID string, goal Goal) (WriteResult, error) {
	if err := checkGoal(&goal); err != nil {
		return WriteResult{}, err
	}
	subject := GoalSubject(goal.ID)
	scope, err := goalScope(subject, goal)
	if err != nil {
		return WriteResult{}, err
	}
	at := w.Now()
	goal.UpdatedAt = at
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readGoal(ctx, tx, goal.ID)
			if err != nil {
				return statelog.Decision{}, err
			}
			post := goal
			post.V = DocumentVersion
			if !held {
				post.CreatedAt, post.CreatedBy = at, w.Actor
			} else {
				// THE CREATION FACTS ARE THE STORED ROW'S. A save that
				// carried them would let a second writer re-attribute a
				// goal somebody else set.
				post.CreatedAt, post.CreatedBy = current.CreatedAt, current.CreatedBy
			}
			return w.decide(subject, OpPatch, scope, opID, post, nil, at)
		},
	})
}

// checkGoal refuses a goal that could not be rendered or could not be counted.
func checkGoal(goal *Goal) error {
	goal.Name = strings.TrimSpace(goal.Name)
	goal.Group = strings.TrimSpace(goal.Group)
	goal.Owners = cleanHandles(goal.Owners)
	goal.Members = cleanHandles(goal.Members)
	switch {
	case goal.ID == "":
		return fmt.Errorf("tracker: a goal write names no goal id")
	case goal.Name == "":
		return fmt.Errorf("tracker: goal %s has no name", goal.ID)
	case len(goal.Name) > MaxGoalName:
		return fmt.Errorf("tracker: goal %s's name is %d bytes and the maximum "+
			"is %d", goal.ID, len(goal.Name), MaxGoalName)
	case len(goal.Description) > MaxGoalDescription:
		return fmt.Errorf("tracker: goal %s's description is %d bytes and the "+
			"maximum is %d", goal.ID, len(goal.Description), MaxGoalDescription)
	case len(goal.Group) > MaxGoalGroup:
		return fmt.Errorf("tracker: goal %s's group label is %d bytes and the "+
			"maximum is %d", goal.ID, len(goal.Group), MaxGoalGroup)
	case len(goal.Owners) == 0:
		// A GOAL WITH NO OWNER IS A GOAL NOBODY REPORTS ON, which is the
		// one thing a goal is for. It is refused rather than defaulted
		// to the writer: a founder filing a goal for a lead means the
		// lead, and guessing would put every goal on whoever typed it.
		return fmt.Errorf("tracker: goal %s names no owner — a goal reports to "+
			"somebody, and one that reports to nobody is a note", goal.ID)
	case len(goal.Owners) > MaxGoalOwners:
		return fmt.Errorf("tracker: goal %s names %d owners and the maximum is "+
			"%d — an outcome more than %d people own is one nobody does",
			goal.ID, len(goal.Owners), MaxGoalOwners, MaxGoalOwners)
	case len(goal.Members) > MaxGoalMembers:
		return fmt.Errorf("tracker: goal %s names %d members and the maximum "+
			"is %d", goal.ID, len(goal.Members), MaxGoalMembers)
	case !GoalHealth(goal.Health).Valid():
		return fmt.Errorf("tracker: %q is not a health value — the four are "+
			"%s, %s, %s and %s, and an unset one is a goal nobody has judged",
			goal.Health, HealthOnTrack, HealthAtRisk, HealthOffTrack, HealthDone)
	case len(goal.Targets) > MaxGoalTargets:
		return fmt.Errorf("tracker: goal %s carries %d targets and the maximum "+
			"is %d", goal.ID, len(goal.Targets), MaxGoalTargets)
	case len(goal.Updates) > MaxGoalUpdates:
		return fmt.Errorf("tracker: goal %s carries %d updates and the maximum "+
			"is %d", goal.ID, len(goal.Updates), MaxGoalUpdates)
	case goal.StartAt != nil && goal.DueAt != nil && goal.DueAt.Before(*goal.StartAt):
		return fmt.Errorf("tracker: goal %s is due %s and starts %s — a window "+
			"that ends before it begins measures nothing",
			goal.ID, goal.DueAt.Format(time.RFC3339), goal.StartAt.Format(time.RFC3339))
	}
	seen := make(map[string]bool, len(goal.Targets))
	for i := range goal.Targets {
		target := &goal.Targets[i]
		target.Name = strings.TrimSpace(target.Name)
		target.Tasks = cleanHandles(target.Tasks)
		target.Projects = upperAll(cleanHandles(target.Projects))
		switch {
		case target.ID == "":
			return fmt.Errorf("tracker: a target of goal %s names no target id "+
				"— the id is what a progress note and a task's `goal=` filter "+
				"both address", goal.ID)
		case seen[target.ID]:
			// TWO TARGETS UNDER ONE ID would collide on the child
			// table's own primary key, so the apply would write one
			// and silently drop the other.
			return fmt.Errorf("tracker: goal %s names target %q twice",
				goal.ID, target.ID)
		case target.Name == "":
			return fmt.Errorf("tracker: target %s of goal %s has no name",
				target.ID, goal.ID)
		case len(target.Name) > MaxGoalName:
			return fmt.Errorf("tracker: target %s of goal %s has a %d-byte "+
				"name and the maximum is %d", target.ID, goal.ID,
				len(target.Name), MaxGoalName)
		case !validTargetType(target.Type):
			return fmt.Errorf("tracker: %q is not a target type — the four are "+
				"%s, %s, %s and %s", target.Type, TargetTasks, TargetNumber,
				TargetPercent, TargetBinary)
		case len(target.Tasks) > MaxTasksPerTarget:
			return fmt.Errorf("tracker: target %s of goal %s names %d tasks and "+
				"the maximum is %d — a target counting more than that is a "+
				"project, and a project is what the other kind of target names",
				target.ID, goal.ID, len(target.Tasks), MaxTasksPerTarget)
		case len(target.Projects) > MaxTasksPerTarget:
			return fmt.Errorf("tracker: target %s of goal %s names %d projects "+
				"and the maximum is %d", target.ID, goal.ID,
				len(target.Projects), MaxTasksPerTarget)
		case target.Type == TargetTasks && len(target.Tasks) == 0 &&
			len(target.Projects) == 0:
			return fmt.Errorf("tracker: target %s of goal %s counts tasks and "+
				"names none, so it would report done before anybody started",
				target.ID, goal.ID)
		case target.Type == TargetNumber && target.Goal == target.Start:
			return fmt.Errorf("tracker: target %s of goal %s starts and ends at "+
				"%v, so no movement could ever register",
				target.ID, goal.ID, target.Start)
		}
		seen[target.ID] = true
	}
	return nil
}

func validTargetType(kind string) bool {
	for _, known := range TargetTypes {
		if kind == known {
			return true
		}
	}
	return false
}

// goalScope is what an apply of this goal may write.
//
// See the file head: the goal's own subject covers every row keyed on its id,
// and the PROJECTS a target names are declared beside it so a project-scoped
// read waits for a deferred goal record that would change its answer. A target
// naming bare task ids resolves to the DOMAIN, because "a task somewhere" has
// no narrower honest term.
func goalScope(subject Subject, goal Goal) (ScopeSet, error) {
	containers := map[string]bool{}
	bare := false
	for _, target := range goal.Targets {
		for _, project := range target.Projects {
			containers[project] = true
		}
		if len(target.Tasks) > 0 {
			bare = true
		}
	}
	switch {
	case bare:
		return ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}, nil
	case len(containers) == 0:
		return ScopeSet{Subject: true}, nil
	case len(containers) >= MaxScopeTerms:
		// THE SMALLEST COVERING TERM rather than a refusal, which is
		// [MaxScopeTerms]'s own rule: a goal spanning more projects than
		// a record can enumerate is a legitimate company-wide goal, and
		// the domain is the honest description of its blast radius.
		return ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}, nil
	}
	terms := make([]ScopeTerm, 0, len(containers)+1)
	terms = append(terms, ScopeTerm{
		Kind: TermObject, Container: WorkspaceContainer, ID: subject.ID,
	})
	for project := range containers {
		terms = append(terms, ScopeTerm{Kind: TermContainer, ID: project})
	}
	scope := ScopeSet{Terms: terms}
	if err := scope.Validate(); err != nil {
		return ScopeSet{}, err
	}
	return scope, nil
}

// readGoal reads one goal inside a write's own snapshot.
func readGoal(ctx context.Context, tx *sql.Tx, id string) (Goal, bool, error) {
	return readDocument(ctx, tx, GoalSubject(id),
		func(g *Goal, version uint64) { g.Version = version })
}

// cleanHandles trims, drops empties and deduplicates, preserving order.
func cleanHandles(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func upperAll(in []string) []string {
	for i, v := range in {
		in[i] = strings.ToUpper(v)
	}
	return in
}
