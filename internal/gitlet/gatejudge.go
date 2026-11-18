package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/words"
)

// THE MERGE GATE, JUDGED: whether a live change meets its target's gate
// ([MergeGate]), as a [Readiness] answered by ONE pure function, [JudgeGate],
// from one snapshot's rows and one epoch's chart — called by every read and
// every decide of a merge (ADR-0008), never a workflow somebody must run. It
// counts the change's own reviews, findings and checks, never a tracker
// decision (TestTheGateReadsNoDecision). The gate is the strictest
// combination of every matching rule ([GateRulesOf]); a branch no rule
// matches has none, and merges on M4's preconditions and the merger's right
// to push it. The gate is PRECONDITIONS judged on one snapshot: the race with
// the record that moves the branch is deliberate, since that record judges
// again from its own.

// GateRules is the merge gate a change's target is held to: every rule of its
// repository whose pattern matches the target, combined term by term into
// the strictest of them ([GateRulesOf]).
type GateRules struct {
	// Target is the branch the gate holds, refs/heads/<name>.
	Target string
	// Patterns is every rule matching Target, in the rules' order; Gated
	// those of them carrying a merge gate.
	Patterns []string
	Gated    []string
	// MergeBy is the strictest merge_by of the rules matching Target (a rule
	// with no gate reading as writers) and of code.merge_by_floor; "" for a
	// target no rule matches, which the floor never reaches (a merge into it
	// takes its merger's right to push it).
	MergeBy MergeBy
	// Approvals is one where any gate asks one, and none otherwise;
	// ApprovalsUnless the path classes EVERY gate asking one exempts.
	Approvals       Approvals
	ApprovalsUnless []ChangeClass
	// HumanPaths is every gate's human paths as its gate wrote them, by glob,
	// with the ChecksDirGlob default no record stores ([humanPathsOf]);
	// Checks every gate's required checks, sorted and each once.
	HumanPaths []HumanPath
	Checks     []string
	// QueueDepth and QueueBatch are how many candidates the target's queue
	// tests at once and how many changes one candidate takes: the SMALLEST
	// over the gates requiring checks, since they bound throughput, never what
	// is tested (M5-D3); zero where no such gate matches.
	QueueDepth int
	QueueBatch int
	// OwnersApprove is whether any gate makes path owners gating.
	OwnersApprove bool
	// KeepsSource is whether any gate matching Target keeps a seat's source
	// branch on merge (delete_source: none), winning over a gate that deletes
	// it since a deletion is what no later write undoes. A merge whose caller
	// said nothing deletes a source in a seat's namespace unless it is so, and
	// keeps every other (Q4; internal/gitlet/host's deletesSource).
	KeepsSource bool
}

// GateRulesOf is the gate target is held to by rules and floor
// (code.merge_by_floor; "" is none beyond the rules'): the strictest of each
// term over every rule matching target, as every rule matching a ref holds a
// push to it ([ProtectionRefusal]). A pattern this build cannot read is
// refused, never read as matching nothing: every stored rule passed
// validation, and a gate read as absent merges a protected branch on nothing.
// A term's value this build does not know reads as its strictest, FAIL
// CLOSED, as a loosening is judged ([MergeGate.LoosenedBy]).
func GateRulesOf(rules []Protection, target string, floor MergeBy) (GateRules, error) {
	out := GateRules{Target: target, Approvals: ApprovalsNone}
	var (
		asked  int
		unless []ChangeClass
	)
	// THE FIRST MATCHING RULE SETS MERGE_BY, later ones only tighten it: ""
	// as a start would rank past every known value (MergeBy.Rank).
	hold := func(by MergeBy) {
		if len(out.Patterns) == 1 {
			out.MergeBy = by
			return
		}
		out.MergeBy = out.MergeBy.Stricter(by)
	}
	// EACH HUMAN PATH AND EACH CHECK ONCE, BY A SET: the union is up to
	// MaxProtectionsPerRepo × MaxHumanPaths entries, some four million
	// comparisons by a scan of the list for each.
	var (
		humanSeen = map[string]bool{}
		checkSeen = map[string]bool{}
	)
	for _, rule := range rules {
		pattern, ok := gitref.ParsePattern(rule.Pattern)
		if !ok {
			return GateRules{}, fmt.Errorf("gitlet: protection %q is not a protection pattern this build reads, so "+
				"no merge into %s can be judged against it", rule.Pattern, target)
		}
		if !pattern.Match(target) {
			continue
		}
		out.Patterns = append(out.Patterns, rule.Pattern)
		g := rule.Gate
		if g == nil {
			hold(MergeByWriters)
			continue
		}
		out.Gated = append(out.Gated, rule.Pattern)
		hold(g.MergeBy)
		if g.Approvals != ApprovalsNone {
			// ONE, OR A COUNT THIS BUILD DOES NOT KNOW, which asks at least
			// the approval this build counts.
			out.Approvals = ApprovalsOne
			if asked == 0 {
				unless = slices.Clone(g.ApprovalsUnless)
			} else {
				unless = slices.DeleteFunc(unless, func(c ChangeClass) bool {
					return !slices.Contains(g.ApprovalsUnless, c)
				})
			}
			asked++
		}
		for _, h := range humanPathsOf(g) {
			if key := h.key(); !humanSeen[key] {
				humanSeen[key] = true
				out.HumanPaths = append(out.HumanPaths, h)
			}
		}
		for _, name := range g.Checks {
			if !checkSeen[name] {
				checkSeen[name] = true
				out.Checks = append(out.Checks, name)
			}
		}
		out.OwnersApprove = out.OwnersApprove || g.OwnersApprove
		// A VALUE THIS BUILD DOES NOT KNOW KEEPS the branch, as an unknown
		// term reads as its strictest.
		out.KeepsSource = out.KeepsSource || g.DeleteSource != DeleteSeats
		if len(g.Checks) > 0 {
			out.QueueDepth = smallest(out.QueueDepth, g.Landing.Depth)
			out.QueueBatch = smallest(out.QueueBatch, g.Landing.Batch)
		}
	}
	if len(out.Patterns) > 0 && floor != "" {
		out.MergeBy = out.MergeBy.Stricter(floor)
	}
	if len(unless) > 0 {
		out.ApprovalsUnless = unless
	}
	// SORTED WHOLLY, by glob then the classes it waives, so the list depends
	// on the rules' human paths, never on their order.
	slices.SortFunc(out.HumanPaths, compareHumanPaths)
	slices.Sort(out.Checks)
	return out, nil
}

// humanPathsOf is the human paths gate g holds a target to: those it names
// and, for a gate requiring checks without agents_edit_checks, ChecksDirGlob,
// so a change to the checks a target is measured with waits for a person
// (U44). The default is added here, where every read and decide takes its
// gate, and never stored: the rule's record says what its writer wrote, and
// the default reaches every rule with no record rewritten.
func humanPathsOf(g *MergeGate) []HumanPath {
	if len(g.Checks) == 0 || g.AgentsEditChecks {
		return g.HumanPaths
	}
	return append(slices.Clone(g.HumanPaths), HumanPath{Glob: ChecksDirGlob})
}

// key is h as one string, its glob and the classes its unless names: equal
// for two human paths [HumanPath.Equal] calls equal, and for no others — no
// glob or class holds a NUL.
func (h HumanPath) key() string {
	var b strings.Builder
	b.WriteString(h.Glob)
	for _, c := range h.Unless {
		b.WriteByte(0)
		b.WriteString(string(c))
	}
	return b.String()
}

// compareHumanPaths orders human paths by glob, and then by the classes their
// unless names.
func compareHumanPaths(a, b HumanPath) int {
	if c := strings.Compare(a.Glob, b.Glob); c != 0 {
		return c
	}
	return slices.Compare(a.Unless, b.Unless)
}

// smallest is the smaller of held and next, held zero reading as none yet.
func smallest(held, next int) int {
	if held == 0 {
		return next
	}
	return min(held, next)
}

// Protected reports a target some rule matches: a merge into it is held to
// MergeBy, and the company's floor reaches it.
func (r GateRules) Protected() bool { return len(r.Patterns) > 0 }

// QueueTarget reports a target whose merges land through its QUEUE: a gated
// one requiring a check (M5-D9), since its required checks are what hold a
// change's head ([JudgeGate]'s check term). Every other merge is direct.
func (r GateRules) QueueTarget() bool { return len(r.Checks) > 0 }

// readsPaths reports a gate that judges the paths a change touches: one naming
// human paths, or classes that exempt a change from its approval.
func (r GateRules) readsPaths() bool { return len(r.HumanPaths) > 0 || len(r.ApprovalsUnless) > 0 }

// GateFacts is everything a change's merge gate judges, as ONE snapshot holds
// it ([LoadGateFacts], [gateFactsIn]), beside the chart and the floor of ONE
// epoch and who would merge it — judged by [JudgeGate] with no row read again.
type GateFacts struct {
	// Rows is what M4's readiness judges: the change, its authors, its
	// revisions' facts, its approvals, its asks, its blockers and its
	// mergeability.
	Rows ReadinessRows
	// Rules is every protection rule of the change's repository, each with
	// its merge gate: what [GateFacts.Gate] combines for the change's target.
	Rules []Protection
	// Head is what the gate reads of the head revision's facts beyond M4's —
	// read only where the target's gate judges paths or path owners.
	Head HeadFacts
	// OwnerRowsDigest is this snapshot's digest of the repository's path
	// owners ([OwnersDigest]; code_repos.owners_digest), read only where the
	// gate makes path owners gating: whether the head revision's recorded
	// match of them is current.
	OwnerRowsDigest string
	// HumanPathsDigest is this snapshot's digest of the repository's rules'
	// human paths ([HumanPathsDigest]; code_repos.human_paths_digest), read
	// only where the gate names human paths: whether the head revision's
	// recorded match of them is current.
	HumanPathsDigest string
	// Checks is the latest attempt of each check the target's gate requires
	// on the head commit, by name — none for a check no attempt of which is
	// recorded there.
	Checks []GateCheck
	// Roster is the judging epoch's chart: whose approval counts, who leads
	// whom, what a seat may do on the repository.
	Roster Roster
	// Floor is code.merge_by_floor on the judging epoch.
	Floor MergeBy
	// Merger is who would merge the change — a read's asker, a decide's
	// principal ([PreconditionMergeAuthority]). The zero Principal leaves the
	// term unjudged, so a stored readiness says whether the change is ready
	// for whoever merges it.
	Merger Principal
	// Queue is the change's LIVE entry in its target's merge queue and its
	// place there, nil for none ([PreconditionQueue]).
	Queue *QueuedFacts

	// gate is the target's gate under no floor as the gathering read combined
	// it, once per (repository, target) of a page, for [GateFacts.Gate] to
	// apply the floor to rather than combine again; nil where none was.
	gate *GateRules
}

// QueuedFacts is a change's live entry in its target's merge queue as the gate
// reads it: the entry, where it stands, and its place in its target's queue,
// from 1 — answered on the queue precondition ([Precondition.Queue]).
type QueuedFacts struct {
	EntryID   string     `json:"entry_id"`
	TargetRef string     `json:"target_ref"`
	State     QueueState `json:"state"`
	Place     int        `json:"place"`
}

// HeadFacts is what a merge gate reads of the head revision's recorded facts
// beyond M4's: whether they are recorded against the change's CURRENT
// target; how many paths it touches, or all, and their classes — never the
// paths (up to MaxTouchedBytes), which no judgment reads; the path owners its
// home matched ([TouchedOwner]) and the human paths it found holding or
// waiving it, each with the digest of the rows it matched against ([Revise]).
type HeadFacts struct {
	Recorded         bool
	TouchedCount     int
	TouchesAll       bool
	Classes          RevisionClasses
	RequestedOwners  []TouchedOwner
	SkippedOwners    []SkippedOwner
	OwnersDigest     string
	HumanPaths       HumanPathMatch
	HumanPathsDigest string
}

// TouchedOwner is a path owner a revision touches as its facts keep it (code_revision_facts.owners)
// and its merge gate reads it: the first of its globs the touched paths matched, and how many more;
// neither where the facts name none (a revision touching all, or a home before [OwnedPathsVersion]).
type TouchedOwner struct {
	Owner
	FirstPath string `json:"first_path,omitempty"`
	MorePaths int    `json:"more_paths,omitempty"`
}

// GateCheck is the latest attempt of one check on a change's head commit, as
// the gate reads it: its name and attempt, where it stands, why it ended, and
// what ran it — whatever its purpose, since a check is a measurement of the
// commit.
type GateCheck struct {
	Name      string
	Attempt   int
	State     CheckState
	Reason    CheckReason
	Placement CheckPlacement
}

// Gate is the gate f's change's target is held to ([GateRulesOf]): the
// gathering read's combination with the floor applied, where it was made for
// the change's current target; the rules combined again otherwise. Reuse
// spares a page of changes into one target a combination each, which parses
// up to MaxProtectionsPerRepo patterns and gathers up to MaxTargetHumanPaths.
func (f GateFacts) Gate() (GateRules, error) {
	if f.gate == nil || f.gate.Target != f.Rows.Change.TargetRef {
		return GateRulesOf(f.Rules, f.Rows.Change.TargetRef, f.Floor)
	}
	r := *f.gate
	if r.Protected() && f.Floor != "" {
		r.MergeBy = r.MergeBy.Stricter(f.Floor)
	}
	return r, nil
}

// LoadGateFacts reads what changeID's merge gate judges, in tx — ONE snapshot,
// a decide's — and false where repository repoID holds no such change. The
// chart, the floor and the merger are the caller's to set: they are the
// judging epoch's and the decide's, never a row.
func LoadGateFacts(ctx context.Context, tx *sql.Tx, repoID, changeID string) (GateFacts, bool, error) {
	row, found, err := changeRowIn(ctx, tx, changeID)
	if err != nil || !found || row.repoID != repoID {
		return GateFacts{}, false, err
	}
	repo, err := grantRefIn(ctx, tx, repoID)
	if errors.Is(err, ErrRepoNotFound) {
		return GateFacts{}, false, nil
	}
	if err != nil {
		return GateFacts{}, false, err
	}
	facts, err := gateFactsIn(ctx, tx, []readinessSubject{{repo: repo, row: row}})
	if err != nil {
		return GateFacts{}, false, err
	}
	return facts[0], true, nil
}

// gateFactsIn reads what each of subjects' merge gates judges, in tx — ONE
// snapshot — a kind of row at a time over every change, as
// [readinessRowsIn] does, in subjects' order. Beyond M4's rows and the
// repositories' rules it reads only what each change's gate reaches: the
// head revision's classes and recorded matches where the gate judges paths
// or owners, the digests those are held to where it names human paths or
// makes owners gating, and attempts of required checks where it requires
// them. The touched list, up to MaxTouchedBytes, is never read.
func gateFactsIn(ctx context.Context, tx *sql.Tx, subjects []readinessSubject) ([]GateFacts, error) {
	rows, err := readinessRowsIn(ctx, tx, subjects)
	if err != nil {
		return nil, err
	}
	out := make([]GateFacts, len(subjects))
	if len(subjects) == 0 {
		return out, nil
	}
	rules, err := rulesOfRepos(ctx, tx, subjects)
	if err != nil {
		return nil, err
	}
	var (
		heads  [][2]any
		owned  []string
		checks [][4]any
	)
	at := make(map[string]int, len(subjects))
	asked := map[[4]string]bool{}
	required := make([][]string, len(subjects))
	byHead := map[[3]string][]int{}
	// EACH (REPOSITORY, TARGET)'S GATE COMBINED ONCE a page, handed on with
	// the facts (GateFacts.Gate).
	gates := map[[2]string]*GateRules{}
	digested := map[string]bool{}
	for i, s := range subjects {
		out[i] = GateFacts{Rows: rows[i], Rules: rules[s.repo.ID]}
		at[s.row.id] = i
		key := [2]string{s.repo.ID, s.row.target}
		gate, ok := gates[key]
		if !ok {
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			combined, err := GateRulesOf(out[i].Rules, s.row.target, "")
			if err != nil {
				return nil, err
			}
			gate = &combined
			gates[key] = gate
		}
		out[i].gate = gate
		if gate.readsPaths() || gate.OwnersApprove {
			heads = append(heads, [2]any{s.row.id, s.row.revision})
		}
		if (gate.OwnersApprove || len(gate.HumanPaths) > 0) && !digested[s.repo.ID] {
			digested[s.repo.ID] = true
			owned = append(owned, s.repo.ID)
		}
		if !gate.QueueTarget() {
			continue
		}
		// EACH (REPOSITORY, COMMIT, TARGET, CHECK) ASKED ONCE. An attempt
		// checks a commit FOR A BRANCH, under its checks file
		// (CheckKey.Target): one head into two branches is judged by each
		// target's own attempts and required checks.
		required[i] = gate.Checks
		head := [3]string{s.repo.ID, s.row.head, s.row.target}
		byHead[head] = append(byHead[head], i)
		for _, name := range gate.Checks {
			key := [4]string{s.repo.ID, s.row.head, s.row.target, name}
			if !asked[key] {
				asked[key] = true
				checks = append(checks, [4]any{s.repo.ID, s.row.head, s.row.target, name})
			}
		}
	}
	if err = headFactsIn(ctx, tx, heads, at, subjects, out); err != nil {
		return nil, err
	}
	if err = matchDigestsOf(ctx, tx, owned, subjects, out); err != nil {
		return nil, err
	}
	if err = gateChecksIn(ctx, tx, checks, byHead, required, out); err != nil {
		return nil, err
	}
	// EVERY CHANGE'S LIVE QUEUE ENTRY at once, and places by ONE WALK a
	// (repository, target) ([placesIn]), never a count a change: that is up
	// to MaxChangesPerPage statements, each walking up to MaxQueueEntries.
	ids := make([]string, len(subjects))
	for i, s := range subjects {
		ids[i] = s.row.id
	}
	live, err := liveEntriesOf(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	entries := make([]queueEntry, 0, len(live))
	for _, s := range subjects {
		if e, queued := live[s.row.id]; queued {
			entries = append(entries, e)
		}
	}
	places, err := placesIn(ctx, tx, entries)
	if err != nil {
		return nil, err
	}
	for i, s := range subjects {
		e, queued := live[s.row.id]
		if !queued {
			continue
		}
		out[i].Queue = &QueuedFacts{EntryID: e.EntryID, TargetRef: e.TargetRef, State: e.State,
			Place: places[e.EntryID]}
	}
	return out, nil
}

// rulesOfRepos is the rules of every repository subjects are of, by id, each
// repository's sorted by pattern — one statement over the distinct ids.
func rulesOfRepos(ctx context.Context, tx *sql.Tx, subjects []readinessSubject) (map[string][]Protection, error) {
	var ids []string
	for _, s := range subjects {
		if !slices.Contains(ids, s.repo.ID) {
			ids = append(ids, s.repo.ID)
		}
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, protectionsOfSQL, list)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the rules a merge gate judges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]Protection, len(ids))
	for rows.Next() {
		var (
			id, direct, gate string
			p                Protection
		)
		if err = rows.Scan(&id, &p.Pattern, &p.AllowForce, &p.AllowDelete, &direct, &gate); err != nil {
			return nil, fmt.Errorf("gitlet: read the rules a merge gate judges: %w", err)
		}
		p.DirectPush = DirectPush(direct)
		if p.Gate, err = gateFrom(gate); err != nil {
			return nil, fmt.Errorf("gitlet: read repository %s's rule %s: %w", id, p.Pattern, err)
		}
		out[id] = append(out[id], p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read the rules a merge gate judges: %w", err)
	}
	// SORTED BY PATTERN though the statement orders so: a combination's
	// sentences rely on it.
	for id := range out {
		slices.SortFunc(out[id], func(a, b Protection) int { return strings.Compare(a.Pattern, b.Pattern) })
	}
	return out, nil
}

// headFactsIn reads the head revision's facts of each (change, revision) pair
// of heads into out — Recorded where they are against the change's current
// target, which only then say what the head revision touches.
func headFactsIn(ctx context.Context, tx *sql.Tx, heads [][2]any, at map[string]int,
	subjects []readinessSubject, out []GateFacts) error {

	if len(heads) == 0 {
		return nil
	}
	encoded, err := json.Marshal(heads)
	if err != nil {
		return fmt.Errorf("gitlet: encode %d head revisions' keys: %w", len(heads), err)
	}
	rows, err := tx.QueryContext(ctx, gateHeadsSQL, string(encoded))
	if err != nil {
		return fmt.Errorf("gitlet: read the head revisions' facts a merge gate judges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id, target, classes, owners, skipped, digest, humanDigest, humanPaths string
			touchesAll                                                            bool
			touchedCount                                                          int
		)
		if err := rows.Scan(&id, &target, &touchesAll, &touchedCount, &classes, &owners, &skipped, &digest,
			&humanDigest, &humanPaths); err != nil {
			return fmt.Errorf("gitlet: read the head revisions' facts a merge gate judges: %w", err)
		}
		i, ok := at[id]
		if !ok {
			return fmt.Errorf("gitlet: read the head revisions' facts a merge gate judges: a row of change %s, "+
				"which the read did not ask", id)
		}
		if target != subjects[i].row.target {
			// FACTS OF ANOTHER BRANCH — the change retargeted since — say
			// nothing of what it merges into now.
			continue
		}
		h := HeadFacts{Recorded: true, TouchedCount: touchedCount, TouchesAll: touchesAll, OwnersDigest: digest,
			HumanPathsDigest: humanDigest}
		for _, wide := range []struct {
			what string
			text string
			into any
		}{
			{"classes", classes, &h.Classes}, {"matched owners", owners, &h.RequestedOwners},
			{"skipped owners", skipped, &h.SkippedOwners}, {"human paths", humanPaths, &h.HumanPaths},
		} {
			if wide.text == "" {
				continue
			}
			if err := json.Unmarshal([]byte(wide.text), wide.into); err != nil {
				return fmt.Errorf("gitlet: read change %s's head revision's %s: %w", id, wide.what, err)
			}
		}
		out[i].Head = h
	}
	return rows.Err()
}

// matchDigestsOf sets on every change in out of a repository in owned that
// row's digests of what touched paths are matched against: its path owners
// ([OwnersDigest]) and its rules' human paths ([HumanPathsDigest]). Every
// record writing those rows writes the digest beside them, so one seek a
// repository reads it; hashing the rows here would read up to a quarter
// mebibyte of owners a repository on every decide's round.
func matchDigestsOf(ctx context.Context, tx *sql.Tx, owned []string, subjects []readinessSubject,
	out []GateFacts) error {

	if len(owned) == 0 {
		return nil
	}
	list, err := jsonList(owned)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, matchDigestsOfSQL, list)
	if err != nil {
		return fmt.Errorf("gitlet: read what a merge gate's matches are held to: %w", err)
	}
	defer func() { _ = rows.Close() }()
	of := make(map[string]matchDigests, len(owned))
	for rows.Next() {
		var (
			id string
			m  matchDigests
		)
		if err := rows.Scan(&id, &m.owners, &m.humanPaths); err != nil {
			return fmt.Errorf("gitlet: read what a merge gate's matches are held to: %w", err)
		}
		of[id] = m
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("gitlet: read what a merge gate's matches are held to: %w", err)
	}
	for i, s := range subjects {
		if m, ok := of[s.repo.ID]; ok {
			out[i].OwnerRowsDigest, out[i].HumanPathsDigest = m.owners, m.humanPaths
		}
	}
	return nil
}

// gateChecksIn reads every attempt of each (repository, commit, target,
// check) key of keys, keeping each check's LATEST attempt on every change
// byHead names for its repository, head commit and target whose target
// requires it — at most MaxCheckAttempts rows a key, one seek of the checks'
// key each.
func gateChecksIn(ctx context.Context, tx *sql.Tx, keys [][4]any, byHead map[[3]string][]int,
	required [][]string, out []GateFacts) error {

	if len(keys) == 0 {
		return nil
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("gitlet: encode %d checks' keys: %w", len(keys), err)
	}
	rows, err := tx.QueryContext(ctx, gateChecksSQL, string(encoded))
	if err != nil {
		return fmt.Errorf("gitlet: read the checks a merge gate judges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			repoID, commit, target   string
			c                        GateCheck
			state, reason, placement string
		)
		if err := rows.Scan(&repoID, &commit, &target, &c.Name, &c.Attempt, &state, &reason,
			&placement); err != nil {
			return fmt.Errorf("gitlet: read the checks a merge gate judges: %w", err)
		}
		c.State, c.Reason, c.Placement = CheckState(state), CheckReason(reason), CheckPlacement(placement)
		for _, i := range byHead[[3]string{repoID, commit, target}] {
			if !slices.Contains(required[i], c.Name) {
				continue
			}
			j := slices.IndexFunc(out[i].Checks, func(held GateCheck) bool { return held.Name == c.Name })
			switch {
			case j < 0:
				out[i].Checks = append(out[i].Checks, c)
			case out[i].Checks[j].Attempt < c.Attempt:
				out[i].Checks[j] = c
			}
		}
	}
	return rows.Err()
}

// JudgeGate is a change's readiness under r, its target's merge gate
// ([GateFacts.Gate]), judged over f alone: PURE, so a decide and a read
// holding the same facts answer the same. Its preconditions are in
// [PreconditionKinds]' order: M4's draft, facts, blocker and mergeable
// always, and each other only where it holds the change — the approval where
// the gate asks one, a person's where it names human paths, the owners' where
// it makes them gating, the checks where it requires them, the merge
// authority where f names a merger, the queue where one holds the change.
// Every approval is counted by M4's one rule ([approvalTally]): an eligible
// non-author's standing verdict on the head revision or carried to it
// unchanged, so one on code that moved since counts for none.
func JudgeGate(f GateFacts, r GateRules) Readiness {
	j := newReadinessJudge(f.Rows, f.Roster)
	g := gateJudge{readinessJudge: j, f: f, rules: r, tally: j.tally()}
	pre := []Precondition{j.draft(), j.facts()}
	if r.Approvals != ApprovalsNone {
		pre = append(pre, g.approval())
	}
	if len(r.HumanPaths) > 0 {
		pre = append(pre, g.humanApproval())
	}
	if r.OwnersApprove {
		pre = append(pre, g.ownersApproval())
	}
	pre = append(pre, j.blocker())
	if r.QueueTarget() {
		pre = append(pre, g.check())
	}
	pre = append(pre, j.mergeable())
	if f.Merger != (Principal{}) {
		pre = append(pre, g.mergeAuthority())
	}
	if f.Queue != nil {
		pre = append(pre, g.queued())
	}
	return j.readiness(pre)
}

// queued is the queue precondition of a change its target's merge queue holds:
// COMPUTING, waiting on nobody since the queue lands it — a queued change is
// merging, not READY — its place and state named and its entry carried whole
// ([Precondition.Queue]).
func (g gateJudge) queued() Precondition {
	q := *g.f.Queue
	doing := "waits"
	if q.State == QueueTesting {
		doing = "is being tested"
	}
	return Precondition{Kind: PreconditionQueue, State: PreconditionComputing, Queue: &q, Detail: fmt.Sprintf(
		"%s %s at place %d in %s's queue: the queue lands it once its gate holds on what it is merged with",
		g.ref, doing, q.Place, q.TargetRef)}
}

// gateJudge is one judgment of a change's merge gate: M4's judgment of its
// rows, the facts the gate reads, the gate itself and the approvals that
// count on the head revision.
type gateJudge struct {
	readinessJudge
	f     GateFacts
	rules GateRules
	tally approvalTally
}

// approval is M4's approval precondition under a gate that asks one: met where
// an approval counts, and EXEMPT where every path the head revision touches is
// of a class every gate asking one exempts — computing, where no approval
// counts and the head revision's facts that would say so are not recorded.
func (g gateJudge) approval() Precondition {
	p := g.approvalBy(g.tally)
	if p.State == PreconditionMet || len(g.rules.ApprovalsUnless) == 0 {
		return p
	}
	n, target := g.rows.Change.Revision, g.rows.Change.TargetRef
	if !g.f.Head.Recorded {
		return Precondition{Kind: PreconditionApproval, State: PreconditionComputing, Detail: fmt.Sprintf(
			"whether revision %d needs an approval waits for its facts against %s, which say whether every "+
				"path it touches is %s, which %s's approvals_unless exempts", n, target,
			words.Or(g.rules.ApprovalsUnless), target)}
	}
	if classed, ok := g.everyPathOf(g.rules.ApprovalsUnless); ok {
		return Precondition{Kind: PreconditionApproval, State: PreconditionMet, Detail: fmt.Sprintf(
			"revision %d needs no approval: every path it touches is %s, which %s's approvals_unless exempts", n,
			words.And(classed), target)}
	}
	return p
}

// humanApproval is the human_approval precondition: met where no human path
// holds the head revision (none matches a touched path, or its unless waives
// every path it matched) or a PERSON'S approval counts; computing until the
// head revision's facts against its target are recorded, and while this
// snapshot's human-paths digest is not the one its home matched against (the
// change is due at once, [ReadChangesDue]); unmet otherwise, waiting on the
// persons asked or on its owner to ask one. No glob is matched here: the
// home's recorded match ([Revise.HumanPaths]; humanpaths.go) keeps
// MaxTargetHumanPaths globs × MaxTouchedPaths paths off every judgment.
func (g gateJudge) humanApproval() Precondition {
	p := Precondition{Kind: PreconditionHumanApproval}
	n, target := g.rows.Change.Revision, g.rows.Change.TargetRef
	if !g.f.Head.Recorded {
		p.State = PreconditionComputing
		p.Detail = fmt.Sprintf("which paths revision %d touches waits for its facts against %s, whose human paths "+
			"hold a change touching them to a person's approval", n, target)
		return p
	}
	held, waived, current := g.humanPathsHolding()
	if !current {
		// THE HOME MATCHED OTHER RULES than this snapshot's: judged on the
		// stale match, a removed human path would still hold the change and
		// an added one never.
		p.State = PreconditionComputing
		p.Detail = fmt.Sprintf("%s's human paths changed since revision %d's facts matched its paths to them: its "+
			"home matches them again", g.rows.Repo.Name, n)
		return p
	}
	if len(held) == 0 {
		p.State = PreconditionMet
		p.Detail = fmt.Sprintf("revision %d touches none of %s's human paths", n, target)
		if len(waived) > 0 {
			p.Detail = fmt.Sprintf("revision %d touches %s only with changes of a class its unless waives", n,
				words.And(waived))
		}
		return p
	}
	for _, a := range g.tally.counted() {
		if a.Kind == PartyPerson {
			p.State = PreconditionMet
			p.Detail = fmt.Sprintf("revision %d touches %s, held to a person's approval, and %s's approval counts",
				n, words.And(held), a.named())
			return p
		}
	}
	p.State = PreconditionUnmet
	for _, asked := range g.rows.Asked {
		if asked.Kind == PartyPerson && !g.author(asked) {
			p.WaitsOn = append(p.WaitsOn, asked)
		}
	}
	if len(p.WaitsOn) == 0 {
		p.WaitsOn = []Party{g.owner}
		p.Detail = fmt.Sprintf("revision %d touches %s, which %s holds to a person's approval, and no person who "+
			"may approve it is asked: its owner %s asks one", n, words.And(held), target, g.owner.named())
		return p
	}
	p.Detail = fmt.Sprintf("revision %d touches %s, which %s holds to a person's approval: it waits on %s", n,
		words.And(held), target, namedAll(p.WaitsOn))
	return p
}

// humanPathsHolding is the globs of the gate's human paths holding the head
// revision to a person and those its unless waived, as its home matched them
// ([HeadFacts.HumanPaths]), each once — a glob some entry holds is held,
// whichever other entry of it was waived — and whether the match is CURRENT:
// made against this snapshot's digest, every place in the gate's list. A
// match of other rules names places in another list, never to be read here.
func (g gateJudge) humanPathsHolding() (held, waived []string, current bool) {
	h := g.f.Head
	if h.HumanPathsDigest != g.f.HumanPathsDigest {
		return nil, nil, false
	}
	entries := g.rules.HumanPaths
	for _, at := range h.HumanPaths.Held {
		if at >= len(entries) {
			return nil, nil, false
		}
		held = appendOnce(held, entries[at].Glob)
	}
	for _, at := range h.HumanPaths.Waived {
		if at >= len(entries) {
			return nil, nil, false
		}
		if !slices.Contains(held, entries[at].Glob) {
			waived = appendOnce(waived, entries[at].Glob)
		}
	}
	return held, waived, true
}

// everyPathOf reports a head revision every path of which is of one of
// classes, and the classes its paths are: its facts touch a list, not all,
// classified COMPLETELY (an incomplete one leaves paths no lexer reached),
// a whitespace_only change being a comment_only one too.
//
// BY COUNTING, NEVER BY THE PATHS: each touched path is listed once, under
// its narrowest class ([classesProblem], which every record passed), so every
// path is classed exactly where the two lists sum to
// code_revision_facts.touched_count; the list, up to MaxTouchedBytes, is
// never read.
func (g gateJudge) everyPathOf(classes []ChangeClass) ([]ChangeClass, bool) {
	h := g.f.Head
	if h.TouchesAll || !h.Classes.Complete {
		return nil, false
	}
	ws, co := len(h.Classes.WhitespaceOnly), len(h.Classes.CommentOnly)
	if ws+co != h.TouchedCount {
		return nil, false
	}
	var used []ChangeClass
	if co > 0 {
		if !waives(classes, ChangeClassCommentOnly) {
			return nil, false
		}
		used = append(used, ChangeClassCommentOnly)
	}
	if ws > 0 {
		if !waives(classes, ChangeClassWhitespaceOnly) {
			return nil, false
		}
		used = append(used, ChangeClassWhitespaceOnly)
	}
	return used, true
}

// ownersApproval is the owners_approval precondition: met where the head
// revision touches no owned path, or ONE counted approval comes from somebody
// eligible for every path owner it touches — a person, or a seat on all their
// lead chains ([gateJudge.commonEligible]); computing until the head
// revision's facts against its target are recorded, and while this
// snapshot's owners digest is not the one its home matched against (the
// change is due at once, [ReadChangesDue]); unmet otherwise, waiting on the
// common seats and the persons asked, or on its owner to ask one. No glob is
// matched here: the recorded match ([Revise.RequestedOwners]) keeps
// MaxPathOwners × MaxPathOwnerPaths globs × MaxTouchedPaths paths off every
// decide's round.
func (g gateJudge) ownersApproval() Precondition {
	p := Precondition{Kind: PreconditionOwnersApproval}
	n, target := g.rows.Change.Revision, g.rows.Change.TargetRef
	h := g.f.Head
	switch {
	case !h.Recorded:
		p.State = PreconditionComputing
		p.Detail = fmt.Sprintf("which path owners revision %d touches waits for its facts against %s", n, target)
		return p
	case h.OwnersDigest != g.f.OwnerRowsDigest:
		// THE HOME MATCHED OTHER OWNERS than this snapshot's: judged on
		// the stale match, a removed owner would still hold the change and
		// an added one never.
		p.State = PreconditionComputing
		p.Detail = fmt.Sprintf("%s's path owners changed since revision %d's facts matched its paths to them: its "+
			"home matches them again", g.rows.Repo.Name, n)
		return p
	case len(h.RequestedOwners) == 0:
		p.State = PreconditionMet
		p.Detail = fmt.Sprintf("revision %d touches no path a path owner owns", n)
		return p
	}
	common := g.commonEligible(h.RequestedOwners)
	for _, a := range g.tally.counted() {
		if a.Kind == PartyPerson || (a.Kind == PartyAgent && slices.Contains(common, a.Handle)) {
			p.State = PreconditionMet
			p.Detail = fmt.Sprintf("%s's approval counts for every path owner revision %d touches: %s", a.named(),
				n, ownersNamed(h.RequestedOwners))
			return p
		}
	}
	p.State = PreconditionUnmet
	for _, seat := range common {
		candidate := Party{Handle: seat, Kind: PartyAgent}
		if !g.author(candidate) && g.eligible(candidate) {
			p.WaitsOn = append(p.WaitsOn, candidate)
		}
	}
	for _, asked := range g.rows.Asked {
		if asked.Kind == PartyPerson && !g.author(asked) && !slices.Contains(p.WaitsOn, asked) {
			p.WaitsOn = append(p.WaitsOn, asked)
		}
	}
	if len(p.WaitsOn) == 0 {
		p.WaitsOn = []Party{g.owner}
		p.Detail = fmt.Sprintf("revision %d touches the paths of %s, and only a person may approve for all of "+
			"them: its owner %s asks one", n, ownersNamed(h.RequestedOwners), g.owner.named())
		return p
	}
	p.Detail = fmt.Sprintf("revision %d touches the paths of %s, and no one approval counts for all of them: it "+
		"waits on one from %s, or a person's", n, ownersNamed(h.RequestedOwners), namedAll(p.WaitsOn))
	return p
}

// commonEligible is the seats eligible for EVERY one of owners: for a unit,
// its effective lead and every lead above it; for a seat, the seat and its
// unit's lead chain — the first owner's order kept, nearest first. A person is
// eligible for every owner and is never listed here; a nil chart makes no seat
// eligible.
func (g gateJudge) commonEligible(owners []TouchedOwner) []string {
	if g.roster == nil {
		return nil
	}
	var common []string
	for i, o := range owners {
		eligible := g.eligibleFor(o.Owner)
		if i == 0 {
			common = eligible
			continue
		}
		common = slices.DeleteFunc(common, func(seat string) bool { return !slices.Contains(eligible, seat) })
	}
	return common
}

// eligibleFor is the seats eligible for one path owner (commonEligible's
// rule): a unit's lead chain, or a seat and its unit's — none for a seat the
// chart does not hold.
func (g gateJudge) eligibleFor(o Owner) []string {
	if o.Kind == OwnerUnit {
		return g.roster.Leads(o.Ref)
	}
	seat, ok := g.roster.Seat(o.Ref)
	if !ok {
		return nil
	}
	out := []string{seat.Handle}
	for _, lead := range g.roster.Leads(seat.Unit) {
		out = appendOnce(out, lead)
	}
	return out
}

// check is the check precondition: met where every check the gate requires
// passed — or was skipped, its paths untouched — on the head commit by its
// latest attempt; unmet where one failed, errored past the home's requeue or
// was cancelled, waiting on the change's owner to fix the change or run it
// again; computing while one is queued or running, the home requeues one its
// infrastructure lost, or none is recorded on the commit yet.
func (g gateJudge) check() Precondition {
	p := Precondition{Kind: PreconditionCheck}
	head := abbrev(g.rows.Change.Head)
	var failing, pending, passed []string
	for _, name := range g.rules.Checks {
		i := slices.IndexFunc(g.f.Checks, func(c GateCheck) bool { return c.Name == name })
		if i < 0 {
			pending = append(pending, name+" (no attempt yet)")
			continue
		}
		c := g.f.Checks[i]
		switch {
		case c.State == CheckStatePassed, c.State == CheckStateSkipped:
			passed = append(passed, fmt.Sprintf("%s (%s)", name, c.State))
		case c.State == CheckStateQueued, c.State == CheckStateRunning:
			pending = append(pending, fmt.Sprintf("%s (%s, attempt %d)", name, c.State, c.Attempt))
		case c.State == CheckStateErrored && c.Reason.Requeued() && c.Attempt < AutoRequeueAttempts:
			// THE INFRASTRUCTURE'S, which the home queues again on its own.
			pending = append(pending, fmt.Sprintf("%s (errored %s, attempt %d, queued again)", name, c.Reason,
				c.Attempt))
		case !c.State.Valid():
			// A STATE A LATER BUILD RECORDS: no ending this build can read
			// as a pass, and no failure it can name.
			pending = append(pending, fmt.Sprintf("%s (%s)", name, c.State))
		default:
			failing = append(failing, fmt.Sprintf("%s (%s, attempt %d)", name, c.State, c.Attempt))
		}
	}
	switch {
	case len(failing) > 0:
		p.State, p.WaitsOn = PreconditionUnmet, []Party{g.owner}
		p.Detail = fmt.Sprintf("%s did not pass on %s: its owner %s fixes the change, or runs again a check "+
			"that did not measure it", strings.Join(failing, ", "), head, g.owner.named())
	case len(pending) > 0:
		p.State = PreconditionComputing
		p.Detail = fmt.Sprintf("the checks %s requires are not all done on %s: %s — a check the target's checks "+
			"file defines is queued by the repository's home, and one it does not waits for a person's CI to "+
			"report it", g.rules.Target, head, strings.Join(pending, ", "))
	default:
		p.State = PreconditionMet
		p.Detail = fmt.Sprintf("every check %s requires is done on %s: %s", g.rules.Target, head,
			strings.Join(passed, ", "))
	}
	return p
}

// mergeAuthority is the merge_authority precondition, of the merger f names
// ([MergeAuthority]).
func (g gateJudge) mergeAuthority() Precondition {
	return MergeAuthority(g.f.Merger, g.rows.Repo, g.rows.Change.TargetRef, g.rules, g.roster)
}

// MergeAuthority is the merge_authority precondition of m merging into target
// of repo under rules (floor included, [GateRulesOf]), judged on roster: a
// person always holds it; on a target no rule protects, whoever may
// fast-forward the branch ([Grant.RefRule]); on a protected one, the standing
// merge_by asks — writers write, maintainers maintain, persons are persons.
// Unmet, it waits on the repository's maintaining seat where one may merge,
// and on nobody where only a person may, since any person may.
//
// PURE OVER VALUES, THE TERM'S ONE RULE: a readiness judges it for its
// merger, and the Gitlet node for the colleague a seat's merge is made for
// (internal/gitlet/host's mergeCeiling) from what it already holds, with no
// read made again as that colleague. Neither restates it.
func MergeAuthority(m Principal, repo RepoRef, target string, rules GateRules, roster Roster) Precondition {
	p := Precondition{Kind: PreconditionMergeAuthority}
	who := "this credential, bound to nobody,"
	if handle, kind := m.party(); kind != "" {
		who = Party{Handle: handle, Kind: kind}.named()
		if kind == PartyPerson {
			p.State = PreconditionMet
			p.Detail = fmt.Sprintf("%s merges into %s: a person maintains every repository", who, target)
			return p
		}
	}
	grant := GrantOf(m, repo, roster)
	if !rules.Protected() {
		if refusal, why := grant.RefRule(fastForward(target)); refusal != "" {
			p.State, p.WaitsOn = PreconditionUnmet, seatParties(maintainersOf(repo, roster))
			p.Detail = fmt.Sprintf("%s may not merge into %s, which no rule protects, as it may not push it: %s",
				who, target, why)
			return p
		}
		p.State = PreconditionMet
		p.Detail = fmt.Sprintf("%s merges into %s, which no rule protects, as it may push it", who, target)
		return p
	}
	// Declared by its type, never need := AccessMaintain: AccessMaintain
	// repeats the iota spec before it, which internal/sourcetree/decltype
	// cannot type, and TestNothingButSyncWritesACache refuses an untyped value
	// handed to a call it cannot type (fmt.Sprintf among them), which it
	// cannot tell from a gitproc.Cmd.
	var need Access
	switch rules.MergeBy {
	case MergeByWriters:
		need = AccessWrite
	case MergeByMaintainers:
		need = AccessMaintain
	default:
		// PERSONS, OR A MERGE_BY THIS BUILD DOES NOT KNOW, which ranks past
		// every one it does (MergeBy.Rank): only a person merges.
		p.State = PreconditionUnmet
		p.Detail = fmt.Sprintf("a merge into %s is a person's (merge_by %s, the stricter of its rules' and "+
			"code.merge_by_floor), and %s is none", target, rules.MergeBy, who)
		return p
	}
	if grant.Access >= need {
		p.State = PreconditionMet
		p.Detail = fmt.Sprintf("%s holds %s on %s, and a merge into %s takes %s (merge_by %s)", who, grant.Access,
			repo.Name, target, need, rules.MergeBy)
		return p
	}
	p.State, p.WaitsOn = PreconditionUnmet, seatParties(maintainersOf(repo, roster))
	p.Detail = fmt.Sprintf("%s holds %s on %s, and a merge into %s takes %s (merge_by %s, the stricter of its "+
		"rules' and code.merge_by_floor): %s or a person merges it", who, grant.Access, repo.Name,
		target, need, rules.MergeBy, maintainedBy(repo, roster))
	return p
}

// mergingAuthors is the authors of f's change who may merge it into its target
// under gate r — [PreconditionMergeAuthority] judged for each as the merger,
// in the order f reads them: whom a change that became ready is announced to
// (concerns.go). A person always may; a seat as its grant and the gate's
// merge_by say.
func mergingAuthors(f GateFacts, r GateRules) []Reviewer {
	g := gateJudge{readinessJudge: newReadinessJudge(f.Rows, f.Roster), f: f, rules: r}
	var out []Reviewer
	for _, a := range f.Rows.Authors {
		g.f.Merger = a.principal()
		if g.f.Merger != (Principal{}) && g.mergeAuthority().State == PreconditionMet {
			out = append(out, a.reviewer())
		}
	}
	return out
}

// principal is p as the principal its rows name: a seat by its handle, a
// person by the human seat they hold — the zero Principal for neither.
func (p Party) principal() Principal {
	switch p.Kind {
	case PartyAgent:
		return Principal{Kind: PrincipalAgent, Seat: p.Handle}
	case PartyPerson:
		return Principal{Kind: PrincipalOperator, Seat: p.Handle}
	}
	return Principal{}
}

// seatParties is seats as the parties a readiness names.
func seatParties(seats []string) []Party {
	var out []Party
	for _, s := range seats {
		out = append(out, Party{Handle: s, Kind: PartyAgent})
	}
	return out
}

// maintainedBy is who maintains repo on chart r, in a sentence: its maintainer
// seat, or the lead of its owner where no seat may.
func maintainedBy(repo RepoRef, r Roster) string {
	if seats := maintainersOf(repo, r); len(seats) > 0 {
		return namedAll(seatParties(seats))
	}
	return "a principal that maintains the repository"
}

// maxOwnersNamed is the most path owners an owners_approval sentence names, each with its first glob:
// a reader acts on a handful, and a listed change's readiness stays a few kilobytes (MaxReadinessBlockers'
// reason).
const maxOwnersNamed = 8

// ownersNamed is path owners in a sentence: each by its kind and reference and, where its facts
// keep one, its first glob that matched and how many more; at most maxOwnersNamed of them.
func ownersNamed(owners []TouchedOwner) string {
	said := make([]words.Said, len(owners))
	for i, o := range owners {
		named := string(o.Kind) + " " + o.Ref
		switch {
		case o.FirstPath == "":
		case o.MorePaths > 0:
			named += fmt.Sprintf(" (%q and %d more)", o.FirstPath, o.MorePaths)
		default:
			named += " (" + strconv.Quote(o.FirstPath) + ")"
		}
		said[i] = words.Said(named)
	}
	return words.AndMore(said, maxOwnersNamed)
}

// appendOnce is list with v appended where it holds no v yet.
func appendOnce[T comparable](list []T, v T) []T {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// The merge gate's own reads, each registered in plan_test.go: the head
// revisions' facts, one seek a (change, revision) pair; the repositories'
// match digests, one seek a repository; and required checks' attempts, one
// seek a (repository, commit, target, check) key — that index NAMED, since
// code_checks_finished_idx also leads with the repository and the planner
// takes it for the repository alone (planner.go).
const (
	gateHeadsSQL = `SELECT f.change_id, f.target_ref, f.touches_all, f.touched_count, f.classes, f.owners,
			f.skipped, f.owners_digest, f.human_paths_digest, f.human_paths
		FROM json_each(?) AS j CROSS JOIN code_revision_facts AS f
		WHERE f.change_id = CAST(j.value ->> 0 AS TEXT) AND f.n = CAST(j.value ->> 1 AS INTEGER)`
	matchDigestsOfSQL = `SELECT id, owners_digest, human_paths_digest FROM code_repos
		WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	gateChecksSQL = `SELECT c.repo_id, c.commit_oid, c.target_ref, c.name, c.attempt, c.state, c.reason,
			c.placement
		FROM json_each(?) AS j CROSS JOIN code_checks AS c INDEXED BY ` + checksKey + `
		WHERE c.repo_id = CAST(j.value ->> 0 AS TEXT) AND c.commit_oid = CAST(j.value ->> 1 AS TEXT)
			AND c.target_ref = CAST(j.value ->> 2 AS TEXT) AND c.name = CAST(j.value ->> 3 AS TEXT)`
)
