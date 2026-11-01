package home

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WorkerChecks is the check work's worker's name, as crewlet.code.home.passes
// counts its turns.
const WorkerChecks = "checks"

// ChecksFileRefresh is how long a target's reading of its checks file stands
// before its home reads the file again at the target's moved tip
// ([code.ChecksDueQuery.RefreshBefore]): TEN MINUTES, [MergeabilityRefresh]'s.
// A target moves on every push to it and every landing into it, and its file
// seldom does; read again at every move, a hot target would cost its home a
// git read and a record on the code log a push. Only heads ALREADY dispatched
// wait on it: a head never dispatched is dispatched at the tip it is listed at
// whatever its target's reading, and its dispatch record moves that reading
// to the tip it read — so a check a target's file gains reaches a new change
// at once, and the changes already waiting on the target within the refresh.
// Ten minutes is how stale a change's page may grow before it says so again
// — about the time a person takes to read a change — and keeps a hot
// target's readings to six records an hour. PROVISIONAL, as
// MergeabilityRefresh is: what moves it is a team that adds a check to a
// target's file and finds its waiting changes slow to run it.
const ChecksFileRefresh = 10 * time.Minute

// checks is the home's CHECK WORK, a worker every loop is built with beside
// the change work: for each live change of a repository whose head owes its
// checks something — as the `checks_due` listing reads the rows (internal/
// code's CheckWork) — the head's checks DISPATCHED from the checks file its
// TARGET'S TIP holds, the attempts its change moved on from cancelled, and an
// attempt its infrastructure ended queued again; and each target whose
// branch moved past the tip its checks file was read at READ AGAIN, at the
// home's pace ([ChecksFileRefresh]). Every write is a record on the
// repository's checks subject, asked of its committing data node's checks
// writer (code.checks) and decided there; this worker decides nothing of a
// row, and the listing it reads next is the rows those records made.
type checks struct {
	estate Estate
	node   string
	log    *slog.Logger
	now    func() time.Time
}

// Name is [WorkerChecks].
func (k *checks) Name() string { return WorkerChecks }

// due is the `checks_due` question of the repositories owing check work — or,
// with repoID, of that repository's — a target's reading due again once it is
// [ChecksFileRefresh] old.
func (k *checks) due(repoID string) *code.ChecksDueQuery {
	return &code.ChecksDueQuery{RepoID: repoID, RefreshBefore: k.now().Add(-ChecksFileRefresh)}
}

// Due is a page of the repositories whose live changes owe their checks work,
// or whose targets are due a reading of their checks file: the `checks_due`
// listing (internal/code's ReadChecksDue).
func (k *checks) Due(ctx context.Context, floor statelog.Position, cursor string) ([]string, string, error) {
	a, err := k.estate.Read(ctx, code.ReadQuery{Kind: code.ReadChecksDue, Cursor: cursor, Fresh: freshAt(floor),
		ChecksDue: k.due("")})
	if err != nil {
		return nil, "", err
	}
	return a.DueRepos, a.Next, nil
}

// Work is one repository's check work, every page: on the first, each target
// due a reading of its checks file read at its tip, and each protected
// target's tip work worked ([checks.tip], tips.go); then each change owing
// its checks work, worked in the order the listing names them
// ([checks.change]) — a failure of one left for the next pass while the
// others are worked — and then the page's dispatches RECORDED, each target's
// heads in as few records as [code.MaxDispatchHeads] allows, after every
// write of theirs: a dispatch cut short is the next turn's, which finds what
// it queued answered from its rows. The checks file of each target tip the
// turn meets is read once, and parsed once a blob ([defsMemo]).
func (k *checks) Work(ctx context.Context, repoID string, floor statelog.Position) error {
	q := code.ReadQuery{Kind: code.ReadChecksDue, Fresh: freshAt(floor), ChecksDue: k.due(repoID)}
	memo := newDefsMemo()
	var failed []error
	for {
		a, err := k.estate.Read(ctx, q)
		if err != nil {
			return errors.Join(append(failed, fmt.Errorf("home: read repository %s's check work: %w", repoID,
				err))...)
		}
		page := newDispatches()
		for _, due := range a.DueTargets {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failed, err)...)
			}
			defs, err := memo.at(ctx, k.estate, repoID, due.Tip, a.Position)
			if err != nil {
				failed = append(failed, fmt.Errorf("home: read the checks file of target %s at %s: %w",
					due.TargetRef, short(due.Tip), err))
				continue
			}
			page.of(repoID, due.TargetRef, due.Tip, due.TipPosition, defs)
		}
		for _, due := range a.DueTips {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failed, err)...)
			}
			if err := k.tip(ctx, repoID, due, memo, a.Position); err != nil {
				failed = append(failed, err)
			}
		}
		for _, due := range a.DueChanges {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failed, err)...)
			}
			if due.Checks == nil {
				continue
			}
			defs, err := k.change(ctx, repoID, due, memo, a.Position)
			switch {
			case err != nil:
				failed = append(failed, err)
			case defs != nil:
				page.dispatched(repoID, due, defs)
			}
		}
		failed = append(failed, k.record(ctx, page, a.Position)...)
		if a.Next == "" {
			return errors.Join(failed...)
		}
		q.Cursor = a.Next
	}
}

// change is what one change's head owes its checks, in this order:
//
//   - its REQUEUES, each the next attempt of a check its infrastructure ended,
//     under the definition it ran under;
//   - where its DISPATCH is owed: the attempts its change moved on from
//     cancelled superseded, then every check the target's tip's checks file
//     defines that the head holds no attempt of queued — or skipped where the
//     change touches none of its paths — as its next attempt where an earlier
//     dispatch errored it for a file that did not run; and where the file is
//     none that runs, every name the target's gate requires that the head
//     holds no attempt of errored defs_invalid.
//
// It answers the checks file the head was dispatched under — nil where it owes
// no dispatch, or its dispatch is not whole — which its page records once
// every write of it was decided.
//
// A refusal saying the rows moved — the repository deleted or archived, an
// attempt already past where this one would follow — is the rows answering,
// left to the next listing; one that decided nothing for a reason of the
// writer's or the log's fails the change, listed again by the next pass.
func (k *checks) change(ctx context.Context, repoID string, due code.ChangeDue, memo *defsMemo,
	at statelog.Position) (*checksDefs, error) {

	w := due.Checks
	about := fmt.Sprintf("change %d", due.Number)
	for _, q := range w.Requeue {
		if err := k.queue(ctx, about, q, at); err != nil {
			return nil, err
		}
	}
	if !w.Dispatch {
		return nil, nil
	}
	for _, key := range w.Stale {
		if err := k.cancel(ctx, due, key, at); err != nil {
			return nil, err
		}
	}
	defs, err := memo.at(ctx, k.estate, repoID, due.Tip, at)
	if err != nil {
		return nil, fmt.Errorf("home: read the checks file of change %d's target %s at %s: %w", due.Number,
			due.TargetRef, short(due.Tip), err)
	}
	whole, err := k.dispatch(ctx, repoID, due, defs, at)
	if err != nil || !whole {
		return nil, err
	}
	return defs, nil
}

// dispatch queues what the head owes of defs, the checks file at its target's
// tip: every check the file defines that the head holds no attempt of for its
// target — the NEXT attempt of one an earlier dispatch errored for a file that
// did not run, the first of any other — queued or skipped by its paths, a
// name its target's gate requires marked required; and where the file is none
// that runs, every name the gate requires that the head holds no attempt of
// errored. It answers whether its dispatch is whole, so the dispatch may be
// recorded: not where the head is in its target, which the change work lands.
func (k *checks) dispatch(ctx context.Context, repoID string, due code.ChangeDue, defs *checksDefs,
	at statelog.Position) (bool, error) {

	w := due.Checks
	about := fmt.Sprintf("change %d", due.Number)
	// next is the attempt the dispatch queues of a check, and false where the
	// head holds one it queues nothing after.
	next := func(name string) (int, bool) {
		if slices.Contains(w.Held, name) {
			return 0, false
		}
		if i := slices.IndexFunc(w.Invalid, func(k code.CheckKey) bool { return k.Name == name }); i >= 0 {
			return w.Invalid[i].Attempt + 1, true
		}
		return 1, true
	}
	base := code.CheckQueued{CheckKey: code.CheckKey{RepoID: repoID, OID: due.Head, Target: due.TargetRef},
		Purpose: code.PurposeHead, ChangeID: due.ID, Revision: int(due.Revision), DefsOID: defs.oid,
		Reason: code.QueueDispatch}
	if defs.invalid != "" {
		// NO FILE THAT RUNS: every required name the head holds no attempt
		// of is errored, saying why — a gate waiting on a check nobody can
		// run says so on the change rather than wait in silence. One an
		// earlier dispatch errored already says so, and is left.
		for _, name := range w.Required {
			if attempt, owed := next(name); !owed || attempt > 1 {
				continue
			}
			q := base
			q.Name, q.Attempt, q.Required, q.Invalid = name, 1, true, defs.invalid
			if err := k.queue(ctx, about, q, at); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	var touched *touchedPaths
	for _, def := range defs.file.Checks {
		attempt, owed := next(def.Name)
		if !owed {
			continue
		}
		q := base
		q.Name, q.Attempt, q.Def, q.Required = def.Name, attempt, &def, slices.Contains(w.Required, def.Name)
		if len(def.Paths) > 0 {
			if touched == nil {
				t, err := k.touched(ctx, repoID, due, at)
				if err != nil {
					return false, err
				}
				touched = &t
			}
			if touched.landed {
				// ITS HEAD IS IN ITS TARGET: the change is landed by its
				// home's change work, and no check of it is owed.
				return false, nil
			}
			q.Skipped = !touched.matches(def)
		}
		if err := k.queue(ctx, about, q, at); err != nil {
			return false, err
		}
	}
	return true, nil
}

// touchedPaths is what a change's head touches against its target's tip:
// every path, or everything (all), or nothing to measure — its head in its
// target (landed).
type touchedPaths struct {
	paths  []string
	all    bool
	landed bool
}

// matches reports whether the change touches a path one of def's globs
// matches: every check where the change touches everything.
func (t touchedPaths) matches(def code.CheckDef) bool {
	if t.all {
		return true
	}
	for _, glob := range def.Paths {
		g, err := gitref.ParsePathGlob(glob)
		if err != nil {
			// A DEFINITION'S GLOBS PASSED THE PARSE that read it, which holds
			// them to this grammar; one that does not is matched to every
			// path rather than skip a check nobody can tell was skipped.
			return true
		}
		if slices.ContainsFunc(t.paths, g.Match) {
			return true
		}
	}
	return false
}

// touched reads what due's head touches against its target's tip: the
// revision's facts, the engine's own git read (internal/code's
// GitRevisionFacts) — the same read the change work records, which the
// answering git node holds for the pair — asked at the listing's position, so
// the git node answering holds both commits.
func (k *checks) touched(ctx context.Context, repoID string, due code.ChangeDue, at statelog.Position) (
	touchedPaths, error) {

	a, err := k.estate.GitRead(ctx, code.GitQuery{Kind: code.GitRevisionFacts, RepoID: repoID, Fresh: freshAt(at),
		RevisionFacts: &code.RevisionFactsQuery{Head: due.Head, Target: due.Tip}})
	switch {
	case err != nil:
		return touchedPaths{}, fmt.Errorf("home: read what change %d's head touches: %w", due.Number, err)
	case a.RevisionFacts == nil:
		return touchedPaths{}, fmt.Errorf("home: what change %d's head touches was answered with no facts",
			due.Number)
	}
	f := a.RevisionFacts
	return touchedPaths{paths: f.Touched, all: f.TouchesAll, landed: f.HeadInTarget}, nil
}

// queue asks the repository's checks writer to queue q at the instant now.
func (k *checks) queue(ctx context.Context, about string, q code.CheckQueued, at statelog.Position) error {
	q.QueuedAt = k.now().UTC()
	return k.write(ctx, about, code.ChecksRequest{OpID: statelog.NewOpID(k.now(), ""), Seen: at, Queued: &q},
		"queue check "+q.Name)
}

// cancel asks the repository's checks writer to end key cancelled superseded:
// an attempt of a head its change moved on from, or for a target it was
// retargeted from.
func (k *checks) cancel(ctx context.Context, due code.ChangeDue, key code.CheckKey, at statelog.Position) error {
	return k.write(ctx, fmt.Sprintf("change %d", due.Number), code.ChecksRequest{
		OpID: statelog.NewOpID(k.now(), ""), Seen: at,
		Finished: &code.CheckFinished{CheckKey: key, State: code.CheckStateCancelled,
			Reason: code.CheckReasonSuperseded, FinishedAt: k.now().UTC(),
			Summary: fmt.Sprintf("change %d moved on to %s for %s", due.Number, short(due.Head), due.TargetRef)}},
		fmt.Sprintf("cancel check %s of %s for %s", key.Name, short(key.OID), key.Target))
}

// dispatches is one page's dispatch records: each target's reading at a tip,
// and the heads dispatched under it, in the order the page met them.
type dispatches struct {
	order []dispatchKey
	by    map[dispatchKey]*code.CheckDispatched
}

// dispatchKey is a target's tip as a dispatch reads it: the branch, its tip
// and the position that moved it there.
type dispatchKey struct {
	target, tip string
	at          statelog.Position
}

func newDispatches() *dispatches { return &dispatches{by: map[dispatchKey]*code.CheckDispatched{}} }

// of is the record of target's reading at tip — with no heads, a refresh of
// the reading alone — made where the page has none.
func (p *dispatches) of(repoID, target, tip string, at statelog.Position, defs *checksDefs) *code.CheckDispatched {
	key := dispatchKey{target: target, tip: tip, at: at}
	d, ok := p.by[key]
	if !ok {
		d = &code.CheckDispatched{RepoID: repoID, Target: target, Tip: tip, TipPosition: at, DefsOID: defs.oid}
		p.by[key] = d
		p.order = append(p.order, key)
	}
	return d
}

// dispatched names due's head among those dispatched under its target's tip —
// once: two changes into one target sharing a head share its dispatch.
func (p *dispatches) dispatched(repoID string, due code.ChangeDue, defs *checksDefs) {
	d := p.of(repoID, due.TargetRef, due.Tip, due.TipPosition, defs)
	if slices.ContainsFunc(d.Heads, func(h code.DispatchedHead) bool { return h.OID == due.Head }) {
		return
	}
	d.Heads = append(d.Heads, code.DispatchedHead{OID: due.Head, ChangeID: due.ID, Revision: int(due.Revision)})
}

// record asks the repository's checks writer to record each of the page's
// dispatches: a target's heads in records of at most [code.MaxDispatchHeads],
// and a reading with none in one. A record refused for the rows' reason is the
// next pass's to read; one that decided nothing fails, and its heads are
// listed again.
func (k *checks) record(ctx context.Context, p *dispatches, at statelog.Position) []error {
	var failed []error
	for _, key := range p.order {
		d := *p.by[key]
		heads := d.Heads
		for first := true; first || len(heads) > 0; first = false {
			n := min(len(heads), code.MaxDispatchHeads)
			rec := d
			rec.Heads, heads = heads[:n:n], heads[n:]
			rec.DispatchedAt = k.now().UTC()
			what := fmt.Sprintf("record the dispatch of %d heads", len(rec.Heads))
			if len(rec.Heads) == 0 {
				what = "record its reading of the checks file"
			}
			if err := k.write(ctx, fmt.Sprintf("target %s at %s", key.target, short(key.tip)),
				code.ChecksRequest{OpID: statelog.NewOpID(k.now(), ""), Seen: at, Dispatched: &rec},
				what); err != nil {
				failed = append(failed, err)
			}
		}
	}
	return failed
}

// write asks one write of the repository's checks writer, about what it is
// for: an answer that the rows moved since is the next pass's to read, and
// one that decided nothing for a reason of the writer's or the log's fails.
func (k *checks) write(ctx context.Context, about string, r code.ChecksRequest, what string) error {
	res, err := k.estate.Checks(ctx, r)
	switch {
	case err != nil:
		return fmt.Errorf("home: %s for %s: %w", what, about, err)
	case slices.Contains(undecidedChecks, res.Refusal):
		return fmt.Errorf("home: %s for %s: %s: %s", what, about, res.Refusal, res.Detail)
	case res.Refusal != "":
		k.log.Debug("code_check_write_refused", "about", about, "write", what, "refusal", res.Refusal,
			"detail", res.Detail)
	}
	return nil
}

// undecidedChecks is every refusal of a check write that decided NOTHING for
// a reason of the writer's or of the log's — busy, the log refusing the record
// or dropping it everywhere — which fails the change, listed again by the next
// pass; every other refusal is the rows answering.
var undecidedChecks = []code.CheckRefusal{code.CheckRefusalBusy, code.CheckRefusalNotApplied,
	code.CheckRefusalUnavailable}

// checksDefs is what a target's tip holds at the checks file's path, as a
// dispatch reads it: the blob (oid, "" for none — and for something at the
// path that is no blob), and either the file parsed — no file at all reads as
// one defining no check — or why it is none that runs (invalid).
type checksDefs struct {
	oid     string
	file    code.ChecksFile
	invalid string
}

// defsMemo is one turn's checks files: each tip read once, and each blob
// parsed once — two tips holding one file, the common case of a target that
// moved without touching it, share the parse. A TURN'S, never longer: a turn
// is one repository's, and the rows list a repository's changes again only
// while they owe work, so a memo kept past the turn would hold files for
// nobody.
type defsMemo struct {
	byTip  map[string]*checksDefs
	byBlob map[string]*checksDefs
}

func newDefsMemo() *defsMemo {
	return &defsMemo{byTip: map[string]*checksDefs{}, byBlob: map[string]*checksDefs{}}
}

// at is the checks file tip holds, read through the repository's git reads
// at the listing's position — the engine's own read (internal/code's
// GitChecksFile), at the TARGET'S tip and never the change's head: a change
// never defines the measurement it is judged by.
func (m *defsMemo) at(ctx context.Context, estate Estate, repoID, tip string, at statelog.Position) (*checksDefs,
	error) {

	if d, ok := m.byTip[tip]; ok {
		return d, nil
	}
	a, err := estate.GitRead(ctx, code.GitQuery{Kind: code.GitChecksFile, RepoID: repoID, Fresh: freshAt(at),
		ChecksFile: &code.ChecksFileQuery{Commit: tip}})
	switch {
	case err != nil:
		return nil, err
	case a.ChecksFile == nil:
		return nil, errors.New("answered with no checks file")
	}
	f := a.ChecksFile
	blob := ""
	if f.Type == "blob" {
		blob = f.OID
	}
	d, ok := m.byBlob[blob]
	if !ok || blob == "" {
		d = parseDefs(f)
		if blob != "" {
			m.byBlob[blob] = d
		}
	}
	m.byTip[tip] = d
	return d, nil
}

// parseDefs is what the answer f says the tip defines: no check where it
// holds nothing at the path, and otherwise the file the parse reads — or why
// it is none that runs, within what a row's summary keeps.
func parseDefs(f *code.ChecksFileAnswer) *checksDefs {
	d := &checksDefs{}
	switch {
	case f.OID == "":
	case f.Type != "blob":
		d.invalid = fmt.Sprintf("%s holds a %s at %s, not a checks file", short(f.Commit), f.Type,
			code.ChecksFilePath)
	case f.Size > code.MaxChecksFileBytes:
		// A SIZE ALONE, no bytes, which the parse refuses by: said as it
		// says it.
		d.oid = f.OID
		d.invalid = fmt.Sprintf("%s at %s is %d bytes, past the %d a checks file may hold "+
			"(code.MaxChecksFileBytes)", code.ChecksFilePath, short(f.Commit), f.Size, code.MaxChecksFileBytes)
	default:
		d.oid = f.OID
		file, err := code.ParseChecks(f.Raw)
		if err != nil {
			d.invalid = summarize(fmt.Sprintf("%s at %s is no checks file that runs: %v", code.ChecksFilePath,
				short(f.Commit), err))
			return d
		}
		d.file = file
	}
	return d
}

// summarize is s within code.CheckSummaryBytes, cut at a rune boundary.
func summarize(s string) string {
	if len(s) <= code.CheckSummaryBytes {
		return s
	}
	const more = " …"
	cut := code.CheckSummaryBytes - len(more)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + more
}

// short is an object id's first twelve digits, as a sentence names it.
func short(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}
