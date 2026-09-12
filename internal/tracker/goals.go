package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
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
	// THE PROJECTS IT ALREADY COUNTS, so a target that DROPS one still
	// names it. The scope is what an apply may write, and this apply
	// rewrites every target reference the goal had — so a save that
	// removed project ENG from a target wrote ENG's rows out while naming
	// only the projects it kept, and a deferral covering ENG never saw it.
	was, err := w.goalProjects(ctx, goal.ID)
	if err != nil {
		return WriteResult{}, err
	}
	scope, err := goalScope(subject, goal, was)
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
			// AND THE SCOPE'S OWN PRE-READ IS VERIFIED HERE, inside
			// the snapshot it was taken outside of: a project this
			// goal counted at that moment and no longer counts now
			// would be a project the record fails to name. Refused
			// rather than published under-declared — the caller's
			// retry re-reads.
			if held {
				for _, project := range goalProjectsOf(current) {
					if !slices.Contains(was, project) {
						return statelog.Decision{}, fmt.Errorf("tracker: goal "+
							"%s started counting %s while this save was being "+
							"prepared, so the record would not name it — read "+
							"the goal again and save: %w",
							goal.ID, project, statelog.ErrConflict)
					}
				}
			}
			post := goal
			post.V = DocumentVersion
			// THE UPDATE HISTORY IS CARRIED FORWARD, never taken from
			// the caller. A goal save is a whole post-state replace, and
			// `write_work_goal` builds its Goal from the tool's own
			// arguments — which carry no updates — so every save through
			// the only shipped surface DESTROYED the whole health
			// history, silently, with `outcome: applied`.
			//
			// APPEND-ONLY, because an update is something somebody WROTE
			// on a date: a save that could rewrite one would let a
			// second writer edit a colleague's assessment of how the
			// quarter is going, and the timestamps would still read as
			// theirs.
			updates, err := appendUpdates(current.Updates, goal.Updates,
				w.Actor, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			post.Updates = updates
			if !held {
				post.CreatedAt, post.CreatedBy = at, w.Actor
			} else {
				// THE CREATION FACTS ARE THE STORED ROW'S. A save that
				// carried them would let a second writer re-attribute a
				// goal somebody else set.
				post.CreatedAt, post.CreatedBy = current.CreatedAt, current.CreatedBy
			}
			return w.decide(subject, OpPatch, ChangeGoalUpdated, scope, opID,
				post, goalWake(current, held, post), at)
		},
	})
}

// goalWake is what a goal save announces, or nil when it announces nothing.
//
// # Every owner and every member, and the actor drops out downstream
//
// D11's rule is that a goal has several owners and every one is woken. The
// members are woken on the same reason: a goal names them because the outcome
// is partly theirs, and a member who is not told the health moved to off_track
// learns it at the review. [Route] drops the actor, so a lead writing an
// update never wakes themselves.
//
// # A SAVE THAT CHANGES NOTHING WAKES NOBODY
//
// This verb is a whole post-state replace, so a form that submits every
// control saves on every submit — and eight owners paged for a no-op is how a
// company learns to ignore the one that mattered. The deltas are computed
// against the stored document inside the same snapshot the decision is formed
// in, which is the only place a single consistent read of it exists.
//
// # What counts as a change is DELIBERATELY narrow
//
// The name, the health, the owners, the members, the dates and the archive —
// facts about the commitment itself. A target's CURRENT VALUE is excluded, and
// that is the interesting one: a `tasks` target recomputes as the tasks under
// it move, so waking on it would page every owner on every status change in
// every project the goal counts. The wake is for the commitment moving, and
// the work moving has its own wakes already.
func goalWake(current Goal, held bool, post Goal) *Notify {
	fields := map[string]Delta{}
	delta := func(name, before, after string) {
		if before != after {
			fields[name] = Delta{From: before, To: after}
		}
	}
	if !held {
		// A NEW GOAL IS ALL NEWS. Its owners are hearing that they own
		// something, which is the one case where every field is a change.
		fields["goal"] = Delta{To: post.Name}
	} else {
		delta("name", current.Name, post.Name)
		delta("health", current.Health, post.Health)
		delta("owners", strings.Join(current.Owners, ", "),
			strings.Join(post.Owners, ", "))
		delta("members", strings.Join(current.Members, ", "),
			strings.Join(post.Members, ", "))
		delta("due", instantText(current.DueAt), instantText(post.DueAt))
		delta("start", instantText(current.StartAt), instantText(post.StartAt))
		delta("archived", boolText(current.Archived), boolText(post.Archived))
		if len(post.Updates) > len(current.Updates) {
			// A HEALTH UPDATE IS PROSE SOMEBODY WROTE, which is the
			// most useful thing a goal wake ever carries — and it can
			// arrive with no other field moving at all.
			fields["update"] = Delta{To: latestUpdateText(post)}
		}
	}
	if len(fields) == 0 {
		return nil
	}
	parties := append(append([]string{}, post.Owners...), post.Members...)
	if len(parties) == 0 {
		// NOBODY TO TELL. A goal with no owners is refused at
		// [checkGoal], so this is unreachable through the verb — but a
		// notification routed to nobody is a record carrying a routing
		// snapshot for no reason, and the check costs nothing.
		return nil
	}
	return &Notify{
		Kind:   ChangeGoalUpdated,
		Fields: fields,
		Snapshot: Snapshot{
			GoalName:    post.Name,
			GoalOwners:  post.Owners,
			GoalMembers: post.Members,
		},
		Excerpt: goalExcerpt(post, fields),
	}
}

// goalExcerpt is the line a card shows.
//
// THE HEALTH UPDATE WINS when there is one, because it is the only part of a
// goal that is somebody's own words — every other delta renders as "x → y",
// which the card already shows under its own heading.
func goalExcerpt(post Goal, fields map[string]Delta) string {
	if update, held := fields["update"]; held && update.To != "" {
		return update.To
	}
	if health, held := fields["health"]; held {
		return fmt.Sprintf("%s is now %s", post.Name, health.To)
	}
	return ""
}

// latestUpdateText is the newest health update's prose, cut to an excerpt.
func latestUpdateText(goal Goal) string {
	if len(goal.Updates) == 0 {
		return ""
	}
	last := goal.Updates[len(goal.Updates)-1]
	text := strings.TrimSpace(last.Text)
	if last.Health != "" && text != "" {
		text = last.Health + ": " + text
	} else if text == "" {
		text = last.Health
	}
	return textcut.Within(text, MaxExcerpt)
}

// instantText and boolText render an optional instant and a flag for a delta.
//
// A DELTA IS TEXT, because it is read by a card and by a history row rather
// than compared — so an absent date is the empty string rather than a zero
// instant that renders as the year one.
func instantText(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

func boolText(v bool) string {
	// BOTH SIDES RENDER, unlike [instantText]'s deliberate empty: a flag
	// that went false is a CHANGE, and rendering it as "true → " puts an
	// empty right-hand side on the card that reads as missing data rather
	// than as an un-archive.
	if v {
		return "true"
	}
	return "false"
}

// appendUpdates carries a goal's health history forward and adds what this
// save wrote.
//
// THE STAMP IS THE WRITER'S, not the caller's: an update carries who said it
// and when, and a caller that could set either would be able to file an
// assessment under somebody else's name on a date of its choosing.
//
// THE OLDEST GO FIRST at the cap, because the newest update is the one a card
// renders and the one anybody reads — a goal that stopped accepting updates at
// a hundred would freeze its own health at whatever it was that day.
func appendUpdates(stored, incoming []GoalUpdate, actor string, at time.Time) (
	[]GoalUpdate, error) {
	out := slices.Clone(stored)
	for _, update := range incoming {
		text := strings.TrimSpace(update.Text)
		if text == "" && strings.TrimSpace(update.Health) == "" {
			continue
		}
		if len(text) > MaxGoalUpdateText {
			// REFUSED, NOT CUT. An update is the STORED value rather
			// than a preview of one — there is nowhere to go and read
			// the rest — so cutting it would silently discard the end
			// of somebody's assessment and leave them believing they
			// had filed it. Cutting is the last resort, and a value
			// with a cap is refused naming the field.
			return nil, fmt.Errorf("tracker: a goal update is %d bytes and at "+
				"most %d are stored — say it shorter, or put the detail where "+
				"the work is and link to it", len(text), MaxGoalUpdateText)
		}
		out = append(out, GoalUpdate{
			At: at, Author: actor, Health: update.Health, Text: text,
		})
	}
	if len(out) > MaxGoalUpdates {
		out = out[len(out)-MaxGoalUpdates:]
	}
	return out, nil
}

// goalProjects is the projects a stored goal's targets already count.
//
// A SCOPE, NEVER AN EXPECTATION — [Writer.db]'s own rule. It only WIDENS the
// declared scope, and it is verified inside the decide snapshot all the same,
// because a scope formed from a stale read and never checked is an
// under-declaration waiting for a race.
//
// SORTED, so two nodes preparing the same save declare the same terms in the
// same order and the record is identical wherever it was written.
func (w *Writer) goalProjects(ctx context.Context, id string) ([]string, error) {
	if w.db == nil {
		return nil, nil
	}
	var out []string
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT DISTINCT ref FROM tracker_goal_target_refs
			 WHERE goal_id = ? AND kind = 'project' ORDER BY ref`, id)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var project string
			if err := rows.Scan(&project); err != nil {
				return err
			}
			out = append(out, project)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tracker: read the projects goal %s counts: %w", id, err)
	}
	return out, nil
}

// goalProjectsOf is the same list read off a decoded goal, sorted.
func goalProjectsOf(goal Goal) []string {
	seen := map[string]bool{}
	var out []string
	for _, target := range goal.Targets {
		for _, project := range target.Projects {
			if !seen[project] {
				seen[project] = true
				out = append(out, project)
			}
		}
	}
	slices.Sort(out)
	return out
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
func goalScope(subject Subject, goal Goal, was []string) (ScopeSet, error) {
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
	// AND THE PROJECTS IT COUNTED BEFORE. The apply rewrites every target
	// reference this goal has, so one it is DROPPING has its rows written
	// out by this record too — and a scope naming only what remains is an
	// under-declaration of exactly the same shape a project move makes.
	for _, project := range was {
		containers[project] = true
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
