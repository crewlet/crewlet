package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/words"
)

// THE MERGE GATE, JUDGED: whether a live change meets the gate of the branch
// it targets ([MergeGate]) — answered as the same [Readiness] M4's readiness
// is, from the rows ONE snapshot holds and the chart ONE epoch holds, by ONE
// pure function, [JudgeGate], which every reader and every decide of a
// change's merge calls (ADR-0008): the change read and each row of a listing,
// the readiness a mergeability record stamps, and the decides that queue a
// change for its target's landing. Never a workflow somebody has to remember
// to run, and never a decision of the tracker's: what it counts is the
// change's own reviews, findings and checks.
//
// # The gate a target is held to
//
// Every rule of the repository whose pattern matches the target holds it, as
// every rule matching a ref holds a push to it ([ProtectionRefusal]), so the
// gate is the rules' COMBINATION, the strictest of each term ([GateRulesOf]):
// merge_by the strictest of the rules' — a rule with no gate reading as
// writers — and of code.merge_by_floor, on a protected target alone; an
// approval where any gate asks one, exempted only by a class every gate
// asking one exempts; every gate's human paths and required checks — a gate
// requiring checks naming ChecksDirGlob among its human paths unless it sets
// agents_edit_checks, so a change to the checks a target is measured with
// waits for a person (U44), the default added here and never stored; path
// owners gating where any gate makes them so; the smallest queue depth and
// batch of the gates requiring checks. A branch no rule matches has no gate:
// a change into it merges on M4's preconditions and its merger's right to
// push the branch.
//
// # What it judges, beside M4's preconditions
//
// M4's draft, facts, blocker and mergeable always ([ReadinessOf]); its
// approval only where the gate asks one — the gate decides whether a merge
// waits for an approval, and a branch no gate asks one of merges on none —
// and then exempted where every path the head revision touches is of a class
// approvals_unless names. Then the gate's own, each where the gate imposes it:
//
//   - HUMAN_APPROVAL, where it names human paths: a change touching a path one
//     of them matches — unless every path that entry matched is of a class its
//     unless names — waits for a PERSON'S counted approval. Which entries hold
//     the head revision is its RECORDED match ([Revise.HumanPaths]), made by
//     its home as the owners' is (humanpaths.go): up to MaxTargetHumanPaths
//     globs against MaxTouchedPaths paths is the same cost on every round, and
//     NO HUMAN PATH'S GLOB IS MATCHED HERE either. A snapshot whose repository
//     row digests its rules' human paths to another digest than the match
//     recorded leaves the term COMPUTING until the home matches again, which
//     the change is due at once.
//   - OWNERS_APPROVAL, where it sets owners_approve: ONE counted approval from
//     somebody eligible for EVERY path owner the head revision touches —
//     a unit owner's effective lead and every lead above it, a seat owner and
//     its unit's lead chain, and any person (PLAN's "the owners' common lead
//     chain or a person"). The touched owners are the head revision's
//     RECORDED match ([Revise.RequestedOwners]): its home matched the globs,
//     and NO GLOB IS MATCHED HERE — up to MaxPathOwners × MaxPathOwnerPaths
//     globs against MaxTouchedPaths paths on every judgment would put the
//     home's work on every decide's round. A snapshot whose repository row
//     digests its owners to another digest than the match recorded is owners
//     the home has not matched yet, and leaves the term COMPUTING until its
//     next revise — which the change is due at once ([ReadChangesDue]).
//   - CHECK, where it requires checks: every required check passed — or
//     skipped, its paths untouched — on the head commit, by its latest
//     attempt whatever ran it.
//   - MERGE_AUTHORITY, where a merger is named ([GateFacts.Merger]): the
//     asker of a read, the principal of a decide. A person always holds it;
//     a seat and a credential the gate's merge_by on a protected target, and
//     a fast-forward of the branch on one no rule protects.
//   - QUEUE, where the target's merge queue holds the change (queue.go):
//     COMPUTING, waiting on nobody, its place named — the queue lands it. A
//     change waiting in a queue is not READY to merge: it is merging.
//
// Every approval these count is counted by M4's one rule ([approvalTally]):
// an eligible reviewer's standing verdict, never an author's, on the head
// revision or carried to it unchanged — so an approval of an earlier revision
// whose code moved since counts for none of them.
//
// # Read in one snapshot, and only as far as the gate reaches
//
// [GateFacts] is what one snapshot holds of a change and its repository —
// M4's rows, the repository's rules, and what of the head revision's facts,
// its owners' digest and its checks the gate reaches — read for a whole page of
// changes a kind of row at a time ([gateFactsIn]), and for one change by a
// decide ([LoadGateFacts]). What a target's gate does not reach is not read:
// the head revision's recorded matches and classes only for a gate naming
// human paths, exempting classes or making path owners gating — and never its
// touched list, up to MaxTouchedBytes a change, which no judgment reads —
// the digests its matches are held to only for one naming human paths or
// making path owners gating, the check rows only for one requiring checks. And
// a target's gate is combined from its rules once a page, every change into
// it sharing it ([GateFacts.Gate]).
//
// The gate is a set of PRECONDITIONS judged on one snapshot, and the race
// between that snapshot and the record that moves the branch is the
// deliberate one: a record judges again from its own.

// GateRules is the merge gate a change's target is held to: every rule of its
// repository whose pattern matches the target, combined term by term into
// the strictest of them ([GateRulesOf]) — see the file's head.
type GateRules struct {
	// Target is the branch the gate holds, refs/heads/<name>.
	Target string
	// Patterns is every rule matching Target, in the rules' order; Gated
	// those of them carrying a merge gate.
	Patterns []string
	Gated    []string
	// MergeBy is who may merge into Target: the strictest merge_by of the
	// rules matching it — a rule with no gate reading as writers — and of
	// code.merge_by_floor; "" for a target no rule matches, which the floor
	// never reaches (a merge into it takes its merger's right to push it).
	MergeBy MergeBy
	// Approvals is one where any gate asks one, and none otherwise;
	// ApprovalsUnless the path classes EVERY gate asking one exempts.
	Approvals       Approvals
	ApprovalsUnless []ChangeClass
	// HumanPaths is every gate's human paths, each entry as its gate wrote
	// it, by glob — and, for every gate requiring checks that does not set
	// agents_edit_checks, ChecksDirGlob, which no record stores (U44);
	// Checks every gate's required checks, sorted and each once.
	HumanPaths []HumanPath
	Checks     []string
	// QueueDepth and QueueBatch are how many candidates the target's queue
	// tests at once and how many changes one candidate takes: the SMALLEST
	// of each over the gates requiring checks — throughput, never what is
	// tested (M5-D3), so the smallest is the one that keeps every matching
	// rule's bound — and zero where no such gate matches.
	QueueDepth int
	QueueBatch int
	// OwnersApprove is whether any gate makes path owners gating.
	OwnersApprove bool
	// KeepsSource is whether any gate matching Target keeps a seat's source
	// branch when a change merges (delete_source: none): a merge whose caller
	// said nothing deletes a source in a seat's namespace unless it is so,
	// and keeps every other source (Q4), decided by the Gitlet node that judges
	// the merge (internal/gitlet/host's deletesSource) — a gate that keeps it
	// wins over one that deletes it, since a deleted branch is the one of the
	// two no later write undoes.
	KeepsSource bool
}

// GateRulesOf is the gate target is held to by rules — a repository's rules,
// each with its gate — and floor, code.merge_by_floor on the judging epoch
// ("" reads as no floor beyond the rules'): the strictest of each term over
// every rule matching target (the file's head). A rule whose pattern this
// build cannot read is refused, never read as a rule matching nothing: every
// stored rule passed validation before a record carried it, and a gate read as
// absent is a protected branch merged on nothing. A value of a term this
// build does not know reads as its strictest, FAIL CLOSED, as a loosening is
// judged ([MergeGate.LoosenedBy]).
func GateRulesOf(rules []Protection, target string, floor MergeBy) (GateRules, error) {
	out := GateRules{Target: target, Approvals: ApprovalsNone}
	var (
		asked  int
		unless []ChangeClass
	)
	// THE FIRST MATCHING RULE SETS MERGE_BY and every later one can only
	// tighten it — never "" as a start, which ranks as a value this build does
	// not know (MergeBy.Rank) and so past every one it does.
	hold := func(by MergeBy) {
		if len(out.Patterns) == 1 {
			out.MergeBy = by
			return
		}
		out.MergeBy = out.MergeBy.Stricter(by)
	}
	// EACH HUMAN PATH AND EACH CHECK ONCE, BY A SET: the union of every
	// matching rule's is up to MaxProtectionsPerRepo × MaxHumanPaths entries,
	// which a scan of the list for each would make some four million
	// comparisons.
	var (
		humanSeen = map[string]bool{}
		checkSeen = map[string]bool{}
	)
	for _, rule := range rules {
		pattern, ok := gitref.ParsePattern(rule.Pattern)
		if !ok {
			return GateRules{}, fmt.Errorf("code: protection %q is not a protection pattern this build reads, so "+
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
	// SORTED WHOLLY, by glob and then by the classes it waives, so the list is
	// a function of the rules' human paths and never of the rules' order.
	slices.SortFunc(out.HumanPaths, compareHumanPaths)
	slices.Sort(out.Checks)
	return out, nil
}

// humanPathsOf is the human paths gate g holds a target to: those it names
// and, for a gate requiring checks that does not set agents_edit_checks,
// ChecksDirGlob.
//
// A RULE REQUIRING CHECKS HOLDS ITS CHECKS TO A PERSON (U44): ChecksDirGlob is
// a human path of its gate unless it sets agents_edit_checks — added here,
// where every reader and every decide takes its gate from, and never stored,
// so the rule's record says what its writer wrote and the default reaches
// every rule without a record rewriting one.
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
// one requiring a check (M5-D9), its queue testing each change merged on it
// before it lands. Every other merge is a direct one. Its required checks
// are what hold a change's head ([JudgeGate]'s check term), so a target is a
// queue's exactly where they do.
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
	// OwnerRowsDigest is the digest of the repository's path owners as this
	// snapshot's row of it carries it ([OwnersDigest];
	// code_repos.owners_digest) — read only where the target's gate makes
	// path owners gating — against which the head revision's recorded match
	// is current or not.
	OwnerRowsDigest string
	// HumanPathsDigest is the digest of the repository's rules' human paths
	// as this snapshot's row of it carries it ([HumanPathsDigest];
	// code_repos.human_paths_digest) — read only where the target's gate
	// names human paths — against which the head revision's recorded match
	// of them is current or not.
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
	// principal — whose authority the gate judges ([PreconditionMergeAuthority]);
	// the zero Principal names nobody, and the term is not judged: what a
	// change's stored readiness says is whether it is ready, for whoever
	// merges it.
	Merger Principal
	// Queue is the change's LIVE entry in its target's merge queue and its
	// place there, nil for none ([PreconditionQueue]).
	Queue *QueuedFacts

	// gate is the gate the change's target is held to under no floor, as the
	// read that gathered these facts combined it — once a (repository,
	// target) of the page, every change into it sharing it — for
	// [GateFacts.Gate] to apply the floor to rather than combine the rules
	// again; nil where nothing combined it.
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

// HeadFacts is what a change's merge gate reads of its head revision's
// recorded facts beside M4's: whether they are recorded against the change's
// CURRENT target at all; how many paths the revision touches — never the
// paths, up to MaxTouchedBytes a change, which no judgment reads — or that it
// touches all, and their classes; the path owners its home matched them to,
// with the digest of the owner rows it matched against; and which of its
// target's human paths its home found holding it or waived, with the digest
// of the rules' human paths it matched against ([Revise]).
type HeadFacts struct {
	Recorded         bool
	TouchedCount     int
	TouchesAll       bool
	Classes          RevisionClasses
	RequestedOwners  []Owner
	SkippedOwners    []SkippedOwner
	OwnersDigest     string
	HumanPaths       HumanPathMatch
	HumanPathsDigest string
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

// Gate is the gate f's change's target is held to, by f's rules and floor
// ([GateRulesOf]) — the combination the read that gathered f made, the floor
// applied to it as GateRulesOf applies it, where that read combined the
// change's current target's; the rules combined again otherwise. A
// combination parses up to MaxProtectionsPerRepo rules' patterns and gathers
// up to MaxTargetHumanPaths entries, which a page of changes into one target
// made twice a change.
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

// gateFactsIn reads what each change of subjects' merge gate judges, in tx —
// ONE snapshot — a kind of row at a time over every change at once, as M4's
// readiness rows are ([readinessRowsIn]): those rows; the rules of every
// repository the changes are of, in one statement; and then only what each
// change's gate reaches — its head revision's classes and its recorded owner
// and human-path matches, driven from the list of (change, head revision)
// pairs whose gate judges paths or path owners; the digests those matches are
// held to of every repository one of whose changes' gates names human paths
// or makes path owners gating, in one statement; and every attempt
// of a required check on a head commit checked for the change's target, driven
// from the list of (repository, commit, target, check) keys, of which the
// latest is kept. The answer is subjects' order.
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
	// EACH (REPOSITORY, TARGET)'S GATE COMBINED ONCE, every change of the
	// page into it sharing it, and handed on with the facts (GateFacts.Gate).
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
		// EACH (REPOSITORY, COMMIT, TARGET, CHECK) ASKED ONCE, whichever
		// changes into that target share the commit. An attempt checks a
		// commit FOR A BRANCH, under that branch's checks file, and is keyed
		// by it (CheckKey.Target): two changes of one head into two branches
		// are judged by two sets of attempts, each its own target's, never
		// by the attempts another branch's file defined — and each keeps the
		// checks its OWN target requires.
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
	// EACH CHANGE'S LIVE QUEUE ENTRY, of every change at once, and the place
	// of each a queue holds — ONE WALK A (REPOSITORY, TARGET) the page's
	// entries are queued on ([placesIn]), never a count a change: a page of
	// MaxChangesPerPage changes queued on one busy target counted each one's
	// place over the target's live entries before it, MaxChangesPerPage
	// statements and as many walks of up to MaxQueueEntries.
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
		return nil, fmt.Errorf("code: read the rules a merge gate judges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]Protection, len(ids))
	for rows.Next() {
		var (
			id, direct, gate string
			p                Protection
		)
		if err = rows.Scan(&id, &p.Pattern, &p.AllowForce, &p.AllowDelete, &direct, &gate); err != nil {
			return nil, fmt.Errorf("code: read the rules a merge gate judges: %w", err)
		}
		p.DirectPush = DirectPush(direct)
		if p.Gate, err = gateFrom(gate); err != nil {
			return nil, fmt.Errorf("code: read repository %s's rule %s: %w", id, p.Pattern, err)
		}
		out[id] = append(out[id], p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("code: read the rules a merge gate judges: %w", err)
	}
	// THE STATEMENT ORDERS BY PATTERN, stated again as the property a
	// combination's sentences rely on.
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
		return fmt.Errorf("code: encode %d head revisions' keys: %w", len(heads), err)
	}
	rows, err := tx.QueryContext(ctx, gateHeadsSQL, string(encoded))
	if err != nil {
		return fmt.Errorf("code: read the head revisions' facts a merge gate judges: %w", err)
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
			return fmt.Errorf("code: read the head revisions' facts a merge gate judges: %w", err)
		}
		i, ok := at[id]
		if !ok {
			return fmt.Errorf("code: read the head revisions' facts a merge gate judges: a row of change %s, "+
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
				return fmt.Errorf("code: read change %s's head revision's %s: %w", id, wide.what, err)
			}
		}
		out[i].Head = h
	}
	return rows.Err()
}

// matchDigestsOf sets the digests each repository of owned's row carries of
// what its revisions' touched paths are matched against — its path owners
// ([OwnersDigest]; code_repos.owners_digest) and its rules' human paths
// ([HumanPathsDigest]; code_repos.human_paths_digest) — on every change of it
// in out: one statement over the repositories, one seek of each one's row,
// each digest written beside the rows it digests by every record that writes
// them, never those rows read and hashed again. A page of changes of
// repositories at their bound of owners read and hashed up to a quarter of a
// mebibyte of owner rows a repository, and an enqueue's and a mergeability's
// decide as much on every round.
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
		return fmt.Errorf("code: read what a merge gate's matches are held to: %w", err)
	}
	defer func() { _ = rows.Close() }()
	of := make(map[string]matchDigests, len(owned))
	for rows.Next() {
		var (
			id string
			m  matchDigests
		)
		if err := rows.Scan(&id, &m.owners, &m.humanPaths); err != nil {
			return fmt.Errorf("code: read what a merge gate's matches are held to: %w", err)
		}
		of[id] = m
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("code: read what a merge gate's matches are held to: %w", err)
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
		return fmt.Errorf("code: encode %d checks' keys: %w", len(keys), err)
	}
	rows, err := tx.QueryContext(ctx, gateChecksSQL, string(encoded))
	if err != nil {
		return fmt.Errorf("code: read the checks a merge gate judges: %w", err)
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
			return fmt.Errorf("code: read the checks a merge gate judges: %w", err)
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

// JudgeGate is a change's readiness under its target's merge gate — see the
// file's head — judged over f, one snapshot's rows, the chart and the floor
// of one epoch and the merger, and r, the gate f's change's target is held to
// ([GateFacts.Gate]): PURE, reading nothing else, so a decide and a read that
// hold the same facts answer the same readiness. Its preconditions are listed
// in [PreconditionKinds]' order: M4's draft, facts, blocker and mergeable
// always, and each other only where it holds the change — the approval where
// the gate asks one, a person's where it names human paths, the owners' where
// it makes them gating, the checks where it requires them, the merge
// authority where f names a merger.
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
// COMPUTING, waiting on nobody — the queue lands it — its place and its state
// named, and its entry carried whole ([Precondition.Queue]).
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
// holds the head revision — none matches a path it touches, or every path an
// entry matched is of a class its unless waives — or a PERSON'S approval
// counts; computing until the head revision's facts against its target are
// recorded, and while the rules' human paths this snapshot's repository row
// digests are not the ones its home matched them against; unmet otherwise,
// waiting on the persons asked to review it, or on its owner to ask one.
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
		// an added one never — and no glob is matched here (humanpaths.go).
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

// humanPathsHolding is the globs of the gate's human paths that hold the head
// revision to a person and those its unless waived, as its home matched them
// ([HeadFacts.HumanPaths]; humanpaths.go), each glob once — a glob some entry
// holds is held, whichever other entry of it was waived — and whether that
// match is CURRENT: made against the human paths this snapshot's repository
// row digests, every place it names one of the gate's. A match of other rules
// names its entries by places in a list that is not the gate's, which no
// place may be read against.
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
// classified COMPLETELY — an incomplete classification leaves paths no lexer
// reached, which no list names — and every path classed by one of them, a
// whitespace_only change being a comment_only one too.
//
// BY COUNTING, NEVER BY THE PATHS: each touched path is listed under its
// narrowest class alone, each once and each a place of the list
// ([classesProblem], which every record passed), so every path is classed
// exactly where the two lists together are as long as the touched list — and
// the gate reads its length (code_revision_facts.touched_count), never the
// list of up to MaxTouchedBytes.
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
// eligible for every path owner it touches — a person, or a seat on every one
// of their lead chains ([gateJudge.commonEligible]); computing until the head
// revision's facts against its target are recorded, and while the owners this
// snapshot's repository row digests are not the ones its home matched them
// against; unmet
// otherwise, waiting on the common seats that may review it and the persons
// asked, or on its owner to ask one.
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
		// an added one never — and no glob is matched here (the file's head).
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
			p.Detail = fmt.Sprintf("%s's approval counts for every path owner revision %d touches (%s)", a.named(),
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
func (g gateJudge) commonEligible(owners []Owner) []string {
	if g.roster == nil {
		return nil
	}
	var common []string
	for i, o := range owners {
		eligible := g.eligibleFor(o)
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

// MergeAuthority is the merge_authority precondition of m, merging into
// target of repo under rules — the gate target is held to, combined with
// code.merge_by_floor ([GateRulesOf]) — judged on roster: a person always
// holds it; on a target no rule protects, whoever may push the branch — a
// fast-forward of it ([Grant.RefRule]); on a protected one, the standing the
// gate's merge_by and the company's floor ask — writers write the repository,
// maintainers maintain it, persons are persons. Unmet, it waits on the seat
// that maintains the repository where one may merge; a merge only a person
// makes names nobody, since any person may.
//
// PURE OVER VALUES, AND THE ONE RULE OF THE TERM: a readiness judges it for
// its merger, and the Gitlet node judges it for the colleague a seat's merge is
// made for (internal/gitlet/host's mergeCeiling) from the chart, the row and
// the gate it already holds, with no read of the change made again as that
// colleague. Neither restates it.
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
	// Declared by its type, never as need := AccessMaintain: AccessMaintain
	// repeats the specification before it (const ( AccessNone Access = iota;
	// … )), a constant internal/sourcetree/decltype cannot type, and the
	// cache's writers gate (TestNothingButSyncWritesACache) refuses a value
	// it cannot type handed to a call it cannot type either, fmt.Sprintf
	// among them, since it cannot tell it from a gitproc.Cmd.
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

// ownersNamed is path owners in a sentence: each by its kind and reference.
func ownersNamed(owners []Owner) string {
	said := make([]words.Said, len(owners))
	for i, o := range owners {
		said[i] = words.Said(string(o.Kind) + " " + o.Ref)
	}
	return words.And(said)
}

// appendOnce is list with v appended where it holds no v yet.
func appendOnce[T comparable](list []T, v T) []T {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// The merge gate's reads, each registered in plan_test.go — beside M4's
// readiness rows and the repositories' rules, read by their own statements:
// the head revisions' facts the gate reaches, driven from the list of
// (change, revision) pairs, one seek of their key a pair; what a list of
// repositories' matches are held to, one seek of each one's key; and every
// attempt of
// a required check on a head commit checked for the change's target, driven
// from the list of (repository, commit, target, check) keys, one seek of the
// checks' key a key — the key NAMED, since code_checks_finished_idx leads with
// the repository too and the planner takes it for the repository alone
// (planner.go).
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
