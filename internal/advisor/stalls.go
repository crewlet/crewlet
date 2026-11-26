package advisor

import (
	"cmp"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The classes of work that stalls where nothing else says so: runs ending on their round cap,
// seats stopped on their budget, open work piled on one agent, the item most work waits on, ready
// agent work held behind a person, and delegate-allowed merges waiting on a person for want of CI.
const (
	ClassRoundCap         Class = "run.round_cap"
	ClassBudgetStopped    Class = "budget.stopped"
	ClassWorkConcentrated Class = "work.concentrated"
	ClassWorkBottleneck   Class = "work.bottleneck"
	ClassWorkHeldByPerson Class = "work.held_by_person"
	ClassMergeNoChecks    Class = "merge.no_required_checks"
)

// The thresholds, each where a week's pattern stops being one bad day.
const (
	// minCappedRuns: two runs a week stopping at the cap is a cap below the
	// work, and one at [minSeatTokens] is the cost of it already.
	minCappedRuns = 2
	// minStoppedHours: half a working day of a seat holding open work and
	// taking no turn.
	minStoppedHours = 4
	// minHeldOpen and heldSharePercent: 20 open items, half of all the open
	// work agents hold, while another agent holds at most idleOpen.
	minHeldOpen      = 20
	heldSharePercent = 50
	idleOpen         = 2
	// minBlocked: five open items standing still behind one.
	minBlocked = 5
	// minReady: three agent items that need no decision of their own.
	minReady = 3
	// minUncheckedMerges: three merges a week, so it is the branch, not a slip.
	minUncheckedMerges = 3
)

// BudgetStop is one turn a token budget refused: the seat's own, or the
// company's, in the period it ran out in, and when that window resets.
type BudgetStop struct {
	Seat, Turn string
	Company    bool
	Period     string
	// Ceiling is the budget the turn was refused at, and At and Resets when
	// it was refused and when the window resets; Resets is zero where unread.
	Ceiling    int64
	At, Resets time.Time
}

// UncheckedMerge is one merge a delegate allowed and the engine put to people
// because the target branch requires no check: its request, the change by its
// address or reference, and the repository on its code host.
type UncheckedMerge struct {
	Request, Change, Repo string
	// GitHub says the repository is on GitHub, else on Gitlet.
	GitHub bool
}

// documentedDetector raises a class whose fix reads the company document.
type documentedDetector struct {
	class  Class
	source source
	detect func(c *config.Company, ev Evidence) []Finding
	// silent, where set, says in words what of the company the class could
	// not judge from ev and why, "" where it judged all of it (ADR-0166).
	silent func(c *config.Company, ev Evidence) string
}

// documented is every class whose fix the document decides.
var documented = []documentedDetector{
	{ClassRoundCap, sourceRuns, roundCap, nil},
	{ClassBudgetStopped, sourceStops, budgetStopped, nil},
	{ClassWorkConcentrated, sourceWork, workConcentrated, nil},
	{ClassWorkImbalance, sourceWork, workImbalance, nil},
	{ClassBlockedNoCause, sourceWork, blockedNoCause, nil},
	{ClassAlarmNoop, sourceLedger, alarmNoop, nil},
	{ClassModelHeld, sourceFlow, modelHeld, nil},
	{ClassModelLimit, sourceCapacity, modelLimit, nil},
	{ClassWakeReasonWaste, sourceLedger, wakeReasonWaste, nil},
	{ClassTurnCostOutlier, sourceLedger, turnCostOutlier, nil},
	{ClassScheduleLoad, sourceFires, scheduleLoad, nil},
	{ClassScheduleOverdue, sourceSkips, scheduleOverdue, nil},
	{ClassAddSeat, sourceStaffing, addSeat, func(c *config.Company, ev Evidence) string {
		return ev.StaffingWeek.unjudged(c, 1)
	}},
	{ClassRemoveSeat, sourceStaffing, removeSeat, func(c *config.Company, ev Evidence) string {
		return ev.StaffingFortnight.unjudged(c, 2)
	}},
}

// stallWords is each class's words, the same for every subject but the why of
// [ClassWorkConcentrated], which reads its seat's turns at once ([concentratedWhy]).
var stallWords = map[Class]said{
	ClassRoundCap: {"Agent runs ended on their round cap before they finished.",
		"What those runs spent bought no finished work, and the item starts again. A cap below what " +
			"the work takes costs more than it saves."},
	ClassBudgetStopped: {"Agents stopped on their token budget while holding open work.",
		"That work waits until the budget window resets, however pressing it is. A seat whose budget " +
			"is spent takes no turn until then."},
	ClassWorkConcentrated: {"One agent holds most of the open work.", concentratedWhyAt(1)},
	ClassWorkBottleneck: {"Open work waits on one item.",
		"Nothing that waits on it, directly or through other items, moves until it does. It is the " +
			"company's critical path."},
	ClassWorkHeldByPerson: {"Agent work that needs no decision waits behind a person.",
		"Agents could do this work now, but it moves only when the person does. It waits on their " +
			"answer to a question, or on an item they hold."},
	ClassAlarmNoop: {"An agent woke on the engine's alarms for turns that changed nothing.",
		"It pays for alarms another agent already answers. Every agent holding the health grant wakes " +
			"each time an alarm fires or clears."},
	ClassAddSeat: {"Work waited for a group of seats that was busy all week.",
		"The work waits because there are too few of these seats. Day after day work wanted them while " +
			"every one was busy most of the time it could take a turn."},
	ClassWakeReasonWaste: {"A kind of wake mostly changed nothing.",
		"Most of what these wakes spend buys nothing, on every seat they reach. Every wake starts a " +
			"turn on the seat's model."},
	ClassTurnCostOutlier: {"Some seats' turns cost several times the others'.",
		"The extra is paid on every turn the seat takes. It is usually a long context, a large answer " +
			"read again, or a run that loops."},
	ClassScheduleLoad: {"A seat's schedules keep it busy while its work waits.",
		"Each check takes a turn its own work could have used."},
	ClassModelLimit: {"A model's usage limit kept the seats on it from working.",
		"While the limit stands, every seat on it waits, whatever it holds."},
	ClassModelHeld: {"An agent's work waited on its model's limit.",
		"Its work stands still until the limit resets, however pressing it is. While its model is at " +
			"its limit, the agent takes no turn."},
	ClassPoolWaits: {"Callers queued for a database connection on a node.",
		"Every request, turn and write on that node waited with them. Its database pool had no " +
			"connection free when they asked."},
	ClassMergeNoChecks: {"Merges a delegate allowed still went to a person.",
		"Every merge into this repository waits on a person. A delegate's allow stands only where the " +
			"code host shows the change passed CI."},
	ClassWorkImbalance: {"One seat holds most of a job's open work while another has little.",
		"Its queue waits while a seat that could take the work sits idle. Moving the work not yet " +
			"started shortens the wait without adding a seat."},
	ClassBlockedNoCause: {"Work is blocked on something nobody holds.",
		"No seat's turn will end the wait, so it lasts until someone steps in. Each waits on an item " +
			"nobody holds, an item that was removed, or a change nothing is moving. The item records " +
			"nothing more about why."},
}

// stallFinding is an owner's finding of class in its class's words.
func stallFinding(class Class, area Area, subject Subject, item string, facts []Fact,
	records []Record, fix Fix) Finding {
	w := stallWords[class]
	return Finding{Class: class, Severity: SeverityCosts, Actor: ActorOwner, Source: SourceEvidence,
		Area: area, Subject: subject, Title: w.title, Why: w.why, Item: item, Evidence: facts,
		Records: records, Fix: fix}
}

// roleOf is the seat handle names in c, and its path.
func roleOf(c *config.Company, handle string) (*config.Role, config.Path) {
	for role, path := range c.EachRole() {
		if role.Seat().Handle() == handle {
			return role, path
		}
	}
	return nil, nil
}

// roundCapOf is the round cap a seat's runs take now, zero for uncapped.
func roundCapOf(c *config.Company, r *config.Role) int {
	if r.Sandbox != nil && r.Sandbox.MaxTurns != nil {
		return *r.Sandbox.MaxTurns
	}
	return c.Providers.Sandbox.MaxTurns()
}

// roundCap is one finding per agent seat whose runs ended on their round cap
// [minCappedRuns] times, or at [minSeatTokens], while a cap at or under the
// rounds they reached still holds: a seat whose cap was raised since is fixed.
func roundCap(c *config.Company, ev Evidence) []Finding {
	bySeat := map[string][]EndedRun{}
	for _, r := range ev.Runs {
		if r.Kind == types.ExecutorErrorMaxTurns {
			bySeat[r.Seat] = append(bySeat[r.Seat], r)
		}
	}
	var out []Finding
	for _, seat := range slices.Sorted(maps.Keys(bySeat)) {
		runs := bySeat[seat]
		role, path := roleOf(c, seat)
		if role == nil || role.Kind == org.KindHuman {
			continue
		}
		var tokens, reached int64
		for _, r := range runs {
			tokens += r.Tokens
			reached = max(reached, r.Rounds)
		}
		limit := roundCapOf(c, role)
		if (len(runs) < minCappedRuns && tokens < minSeatTokens) || limit == 0 || (reached > 0 && int64(limit) > reached) {
			continue
		}
		slices.SortFunc(runs, func(a, b EndedRun) int { return cmp.Compare(b.Tokens, a.Tokens) })
		turns := make([]string, 0, len(runs))
		for _, r := range runs {
			turns = append(turns, r.Turn)
		}
		f := stallFinding(ClassRoundCap, AreaSetup, Subject{Kind: SubjectSeat, Seat: seat},
			fmt.Sprintf("%s at a cap of %d rounds, %s tokens", words.Count(len(runs), "run", "runs"),
				limit, tokensSaid(tokens)),
			[]Fact{{Key: "runs", Value: int64(len(runs))}, {Key: "tokens", Value: tokens},
				{Key: "cap", Value: int64(limit)}},
			recordsOf(RecordTurn, turns), roundCapFix(role, limit))
		f.Path = at(path, "sandbox.max_turns")
		out = append(out, f)
	}
	return out
}

// roundCapFix offers the seat twice its cap, at most [config.RoundCapCeiling],
// or none. A seat already at the ceiling is offered no raise the config would
// refuse: its items split, or its cap lifted.
func roundCapFix(r *config.Role, limit int) Fix {
	handle := r.Seat().Handle()
	capTo := func(n int, label string) Option {
		return Option{Label: label, Change: Change{Entity: EntityRoles, ID: handle,
			Set:     map[string]any{},
			Merge:   map[string]any{"sandbox": map[string]any{"max_turns": n}},
			Summary: fmt.Sprintf("Set %s's round cap to %d", handle, n)}}
	}
	lift := capTo(0, "Lift the cap for this agent")
	if limit >= config.RoundCapCeiling {
		return Fix{Kind: FixConfigChange,
			Sentence: fmt.Sprintf("%s's cap is at the most a round cap may be, %d: split its items into smaller "+
				"ones, or lift its cap.", r.Name, config.RoundCapCeiling),
			Unchanged: "Every other agent's cap stays as it is.",
			Options:   []Option{lift}}
	}
	raised := min(2*limit, config.RoundCapCeiling)
	return Fix{Kind: FixConfigChange,
		Sentence:  fmt.Sprintf("Give %s's runs more rounds, or split its items into smaller ones.", r.Name),
		Unchanged: "Every other agent's cap stays as it is.",
		Options:   []Option{capTo(raised, fmt.Sprintf("Raise the cap to %d rounds", raised)), lift}}
}

// at is path with fields under it, as the document names it.
func at(path config.Path, fields string) string {
	return path.String() + "." + fields
}

// budgetStopped is one finding per agent seat its own budget stopped for [minStoppedHours] or
// more while it held open work, and one naming every seat the company's budget stopped. A
// ceiling raised past the one a turn was refused at is fixed.
func budgetStopped(c *config.Company, ev Evidence) []Finding {
	held := map[string]worklet.HeldOpen{}
	for _, h := range ev.Work.Open.Holders {
		held[h.Handle] = h
	}
	bySeat := map[string][]BudgetStop{}
	var company []BudgetStop
	for _, s := range ev.Stops {
		if s.Company {
			company = append(company, s)
		} else {
			bySeat[s.Seat] = append(bySeat[s.Seat], s)
		}
	}
	var out []Finding
	for _, seat := range slices.Sorted(maps.Keys(bySeat)) {
		stops := bySeat[seat]
		role, path := roleOf(c, seat)
		h, holding := held[seat]
		if role == nil || role.Kind == org.KindHuman || !holding {
			continue
		}
		last := stops[len(stops)-1]
		ceiling := ceilingOf(role.TokenBudget, last.Period)
		hours := stoppedHours(stops, ev.Until)
		if hours < minStoppedHours || ceiling == 0 || int64(ceiling) > last.Ceiling {
			continue
		}
		f := stallFinding(ClassBudgetStopped, AreaSetup, Subject{Kind: SubjectSeat, Seat: seat},
			fmt.Sprintf("stopped %d hours, holding %s, %d urgent or high", hours,
				words.Count(h.Open, "open item", "open items"), h.Pressing),
			[]Fact{{Key: "hours", Value: hours}, {Key: "refused", Value: int64(len(stops))},
				{Key: "open", Value: int64(h.Open)}, {Key: "pressing", Value: int64(h.Pressing)}},
			recordsOf(RecordTurn, turnsOf(stops)), budgetFix(role, last.Period, ceiling))
		f.Path = at(path, "token_budget."+last.Period)
		out = append(out, f)
	}
	if hours := stoppedHours(company, ev.Until); hours >= minStoppedHours {
		var names []string
		open := 0
		seen := map[string]bool{}
		for _, s := range company {
			if role, _ := roleOf(c, s.Seat); role != nil && !seen[s.Seat] && held[s.Seat].Open > 0 {
				seen[s.Seat] = true
				names = append(names, role.Name)
				open += held[s.Seat].Open
			}
		}
		if len(names) > 0 {
			f := stallFinding(ClassBudgetStopped, AreaSetup, Subject{Kind: SubjectCompany},
				fmt.Sprintf("the company's budget stopped them %d hours, holding %s", hours,
					words.Count(open, "open item", "open items")),
				[]Fact{{Key: "hours", Value: hours}, {Key: "refused", Value: int64(len(company))},
					{Key: "open", Value: int64(open)}},
				recordsOf(RecordTurn, turnsOf(company)),
				Fix{Kind: FixOpenSetting, Setting: "token_budget", Options: []Option{},
					Sentence: "Raise the company's token budget, or give the seats that hold the work a share of it."})
			f.Names, f.Path = names, "token_budget"
			out = append(out, f)
		}
	}
	return out
}

// ceilingOf is a budget's ceiling for period, zero where it sets none.
func ceilingOf(b config.TokenBudget, period string) int {
	var p *int
	switch period {
	case "day":
		p = b.Day
	case "week":
		p = b.Week
	case "month":
		p = b.Month
	}
	if p == nil {
		return 0
	}
	return *p
}

// stoppedHours is how many whole hours before until the stops held their
// seats, each from its refusal to its window's reset, overlaps once.
func stoppedHours(stops []BudgetStop, until time.Time) int64 {
	spans := make([][2]time.Time, 0, len(stops))
	for _, s := range stops {
		if s.Resets.After(s.At) {
			spans = append(spans, [2]time.Time{s.At, minTime(s.Resets, until)})
		}
	}
	slices.SortFunc(spans, func(a, b [2]time.Time) int { return a[0].Compare(b[0]) })
	var total time.Duration
	var end time.Time
	for _, sp := range spans {
		start := maxTime(sp[0], end)
		if sp[1].After(start) {
			total += sp[1].Sub(start)
		}
		end = maxTime(end, sp[1])
	}
	return int64(total / time.Hour)
}

func minTime(a, b time.Time) time.Time {
	if !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func turnsOf(stops []BudgetStop) []string {
	out := make([]string, 0, len(stops))
	for i := len(stops) - 1; i >= 0; i-- {
		out = append(out, stops[i].Turn)
	}
	return out
}

// budgetFix offers the seat's ceiling for period at one and a half and twice.
func budgetFix(r *config.Role, period string, ceiling int) Fix {
	handle := r.Seat().Handle()
	to := func(n int) Option {
		return Option{Label: fmt.Sprintf("Raise its %s budget to %s tokens", periodWord(period), tokensSaid(int64(n))),
			Change: Change{Entity: EntityRoles, ID: handle, Set: map[string]any{},
				Merge:   map[string]any{"token_budget": map[string]any{period: n}},
				Summary: fmt.Sprintf("Set %s's %s token budget to %d", handle, periodWord(period), n)}}
	}
	return Fix{Kind: FixConfigChange,
		Sentence:  fmt.Sprintf("Give %s a larger %s budget, or hand its pressing work to another agent.", r.Name, periodWord(period)),
		Unchanged: "The company's own budget stays as it is.",
		Options:   []Option{to(roundedUp(ceiling * 3 / 2)), to(roundedUp(ceiling * 2))}}
}

// roundedUp is n to the next thousand.
func roundedUp(n int) int { return (n + 999) / 1000 * 1000 }

func periodWord(period string) string {
	switch period {
	case "day":
		return "daily"
	case "week":
		return "weekly"
	case "month":
		return "monthly"
	}
	return period
}

// workConcentrated is a finding for an agent seat holding [minHeldOpen] open
// items or more and at least half of all agents hold, while another agent
// seat holds [idleOpen] or fewer.
func workConcentrated(c *config.Company, ev Evidence) []Finding {
	agents := AgentHandles(c)
	open := map[string]worklet.HeldOpen{}
	total := 0
	for _, h := range ev.Work.Open.Holders {
		if slices.Contains(agents, h.Handle) {
			open[h.Handle] = h
			total += h.Open
		}
	}
	var out []Finding
	twinned := imbalances(c, ev)
	for _, seat := range agents {
		h := open[seat]
		if h.Open < minHeldOpen || h.Open*100 < heldSharePercent*total ||
			slices.ContainsFunc(twinned, func(i imbalance) bool { return i.busy.Handle == seat }) {
			continue // a seat whose twin holds little is that class's, with a fix moving the work
		}
		idle := 0
		for _, other := range agents {
			if other != seat && open[other].Open <= idleOpen {
				idle++
			}
		}
		if idle == 0 {
			continue
		}
		item := fmt.Sprintf("%d of %d open items agents hold", h.Open, total)
		facts := []Fact{{Key: "open", Value: int64(h.Open)}, {Key: "agents_open", Value: int64(total)},
			{Key: "idle", Value: int64(idle)}}
		if waiting, known := ev.Waiting[seat]; known {
			item += fmt.Sprintf(", %s queued", words.Count(waiting, "wake", "wakes"))
			facts = append(facts, Fact{Key: "waiting", Value: int64(waiting)})
		}
		f := stallFinding(ClassWorkConcentrated, AreaWork, Subject{Kind: SubjectSeat, Seat: seat},
			item, facts, recordsOf(RecordItem, h.Keys),
			Fix{Kind: FixOpenPage, Page: PageSeat, Target: seat, Options: []Option{},
				Sentence: "Hand some of its items to another agent, or add a seat for this work."})
		f.Why = concentratedWhy(c, seat)
		out = append(out, f)
	}
	return out
}

// concentratedWhy is [ClassWorkConcentrated]'s why for seat, read with the
// turns it runs at once (sandbox.max_concurrent_runs) as the document states
// them now.
func concentratedWhy(c *config.Company, seat string) string {
	role, _ := roleOf(c, seat)
	if role == nil {
		return concentratedWhyAt(1)
	}
	return concentratedWhyAt(role.Seat().Sandbox.ConcurrentRuns())
}

// concentratedWhyAt is the why for an agent that runs runs turns at once.
func concentratedWhyAt(runs int) string {
	const lead = "Its work waits in line while other agents have little to do."
	if runs <= 1 {
		return lead + " An agent takes one turn at a time."
	}
	return fmt.Sprintf("%s This agent takes at most %d turns at once (its sandbox.max_concurrent_runs).", lead, runs)
}

// workBottleneck is one finding per open item an agent seat, or nobody, holds
// that [minBlocked] or more open items wait on.
func workBottleneck(ev Evidence, kind func(string) SeatKind) []Finding {
	person := func(h string) bool { return kind(h) == SeatPerson }
	var out []Finding
	for _, b := range ev.Work.Open.Blockers {
		if b.Blocks < minBlocked || b.HeldByPerson(person) {
			continue
		}
		subject := Subject{Kind: SubjectCompany}
		if kind(b.Holder) == SeatAgent {
			subject = Subject{Kind: SubjectSeat, Seat: b.Holder}
		}
		f := stallFinding(ClassWorkBottleneck, AreaWork, subject,
			fmt.Sprintf("%s: %s wait on it", b.Key, words.Count(b.Blocks, "item", "items")),
			[]Fact{{Key: "waiting", Value: int64(b.Blocks)}, {Key: "ready", Value: int64(b.Ready)}},
			recordsOf(RecordItem, append([]string{b.Key}, b.Waiting...)),
			Fix{Kind: FixOpenPage, Page: PageItem, Target: b.Key, Options: []Option{},
				Sentence: "Open the item: move it first, or take off the waits that need not be there."})
		f.Focus = ItemFocus(b.Key)
		out = append(out, f)
	}
	return out
}

// workHeldByPerson is one finding per item a person holds that [minReady] or
// more agent items with no question of their own wait on.
func workHeldByPerson(ev Evidence, kind func(string) SeatKind) []Finding {
	person := func(h string) bool { return kind(h) == SeatPerson }
	var out []Finding
	for _, b := range ev.Work.Open.Blockers {
		if b.Ready < minReady || !b.HeldByPerson(person) {
			continue
		}
		who := b.Holder
		if !person(who) {
			who = b.Asked[0]
		}
		item := fmt.Sprintf("%s: %s wait on it", b.Key, words.Count(b.Ready, "agent item", "agent items"))
		if len(b.Asks) > 0 {
			item += ", " + words.Count(len(b.Asks), "open question", "open questions")
		}
		f := stallFinding(ClassWorkHeldByPerson, AreaWork, Subject{Kind: SubjectSeat, Seat: who},
			item,
			[]Fact{{Key: "ready", Value: int64(b.Ready)}, {Key: "waiting", Value: int64(b.Blocks)},
				{Key: "questions", Value: int64(len(b.Asks))}},
			recordsOf(RecordItem, append([]string{b.Key}, b.ReadyKeys...)),
			Fix{Kind: FixOpenPage, Page: PageItem, Target: b.Key, Options: []Option{},
				Sentence: "Open the item: answer what it waits on, or take the wait off the work behind it."})
		f.Focus = ItemFocus(b.Key)
		out = append(out, f)
	}
	return out
}

// mergeNoChecks is one finding per repository [minUncheckedMerges] merges or
// more into which a delegate allowed and the engine put to people because the
// branch requires no check.
func mergeNoChecks(ev Evidence, _ func(string) SeatKind) []Finding {
	type repo struct {
		github  bool
		changes []string
	}
	byRepo := map[string]*repo{}
	seen := map[string]bool{}
	for _, m := range ev.Merges {
		if seen[m.Request] || m.Repo == "" {
			continue
		}
		seen[m.Request] = true
		r := byRepo[m.Repo]
		if r == nil {
			r = &repo{github: m.GitHub}
			byRepo[m.Repo] = r
		}
		r.changes = append(r.changes, m.Change)
	}
	var out []Finding
	for _, name := range slices.Sorted(maps.Keys(byRepo)) {
		r := byRepo[name]
		if len(r.changes) < minUncheckedMerges {
			continue
		}
		fix := Fix{Kind: FixOpenSetting, Setting: "code.repos", Options: []Option{},
			Sentence: "Require a check on the repository's protected branch, so a merge that passed it can stand."}
		if r.github {
			fix = Fix{Kind: FixExternalStep, Setting: "github", Options: []Option{},
				URL: (&url.URL{Scheme: "https", Host: "github.com", Path: "/" + name + "/settings/branches"}).String(),
				Sentence: "Run CI on its pull requests and require that check on the branch, or name the " +
					"check in the delegation rule's required_checks.",
				Unchanged: "Until then every merge into it waits on a person."}
		}
		out = append(out, stallFinding(ClassMergeNoChecks, AreaApprovals,
			Subject{Kind: SubjectRepository, Name: name},
			words.Count(len(r.changes), "merge", "merges"),
			[]Fact{{Key: "merges", Value: int64(len(r.changes))}},
			recordsOf(RecordChange, r.changes), fix))
	}
	return out
}

// ChangeRepo is the repository a merge's change names: a GitHub pull
// request's owner/name, or a Gitlet change reference's repository (web!42).
func ChangeRepo(change string) (repo string, github bool, ok bool) {
	change = strings.TrimSpace(change)
	if u, err := url.Parse(change); err == nil && u.Scheme != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if strings.EqualFold(u.Host, "github.com") && len(parts) >= 4 && parts[2] == "pull" {
			return parts[0] + "/" + parts[1], true, true
		}
		return "", false, false
	}
	if name, _, found := strings.Cut(change, "!"); found && name != "" {
		return name, false, true
	}
	return "", false, false
}
