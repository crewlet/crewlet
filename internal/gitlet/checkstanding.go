package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// WHERE EACH CHECK STANDS ([CheckQuery.Standing]): a `checks` listing of each key's latest attempt
// of a live subject, phase by phase, each phase a seek of an index that leads with its state.

// StandingOrder orders a standing listing of checks. A closed set ([StandingOrders]), with Valid.
type StandingOrder string

const (
	// StandingRecent is what runs now first, running then queued, then the endings newest first.
	StandingRecent StandingOrder = "recent"
	// StandingFailing is failed, errored, running, queued, cancelled, passed and skipped, each
	// newest ended first: the order a rollup judges a set's states in.
	StandingFailing StandingOrder = "failing"
)

// StandingOrders is the closed set.
func StandingOrders() []StandingOrder { return []StandingOrder{StandingRecent, StandingFailing} }

// Valid reports an order this build lists in.
func (o StandingOrder) Valid() bool { return slices.Contains(StandingOrders(), o) }

// StandingCounts is a standing listing's attempts by state; a state in AtLeast holds more than
// MaxStandingCounted places, so its count, of its newest places, is a floor.
type StandingCounts struct {
	Counts  CheckCounts  `json:"counts"`
	AtLeast []CheckState `json:"at_least,omitempty"`
}

// MaxStandingCounted is the most places of one state a standing count reads; past it the count is a
// floor. Every state at it, a first page reads about 900 places and their facts: about 60 ms on the
// pinned driver (BenchmarkTheStandingCountsAtTheirBound), near the 50 ms line a first page keeps.
const MaxStandingCounted = 128

// validateStanding is [ReadQuery.validateChecks] for a listing asked where each check stands: of
// a repository or of every repository, in a window that ends now, narrowed by a name and a state.
func (q ReadQuery) validateStanding() error {
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	c := q.Checks
	switch {
	case !c.Standing.Valid():
		return refused("checks.standing", fmt.Sprintf("%q is not one of %s", c.Standing,
			words.Or(StandingOrders())))
	case c.Change != 0 || c.OID != "" || c.Target != "":
		return refused("checks.standing", "where checks stand is listed of a repository or of every "+
			"repository; a change's or a commit's checks are read at their latest attempts (checks.latest)")
	case c.Latest:
		return refused("checks.latest", "a standing listing answers each check's latest attempt already")
	case c.Attempt != 0:
		return refused("checks.attempt", "a listing of checks names no attempt; a check read reads one")
	case !c.Until.IsZero():
		return refused("checks.until", "where a check stands is read now, so a standing listing's window "+
			"ends now")
	case c.Name != "" && !config.ValidCodeCheckName(c.Name):
		return refused("checks.name", fmt.Sprintf("%q is not a check name", c.Name))
	case c.State != "" && !c.State.Valid():
		return refused("checks.state", fmt.Sprintf("%q is not one of %s", c.State, words.Or(CheckStates())))
	}
	return nil
}

// standingPhases is the walks a standing listing takes, in its order: one state each, "" for the
// five endings merged newest ended first; the one state c narrows to where it names one.
func standingPhases(c CheckQuery) []CheckState {
	switch {
	case c.State != "":
		return []CheckState{c.State}
	case c.Standing == StandingFailing:
		return []CheckState{CheckStateFailed, CheckStateErrored, CheckStateRunning, CheckStateQueued,
			CheckStateCancelled, CheckStatePassed, CheckStateSkipped}
	}
	return []CheckState{CheckStateRunning, CheckStateQueued, ""}
}

// endedPhase is how a cursor names the merged endings' phase.
const endedPhase = "ended"

// standingScope is what a standing listing reads in: one repository (repoID and its name), or
// every one the asker and its ceiling read; and the window's start, 0 for none.
type standingScope struct {
	repoID, repoName string
	since            int64
	asker            Principal
	ceiling          string
	chart            Roster
}

// standingPlace is a place in a standing listing: a [checkPlace] and the state it was read in.
type standingPlace struct {
	checkPlace
	state CheckState
}

// standingChecks serves a page of a standing listing: the places of its phases from the cursor's,
// held to the grant, each kept where it is its key's latest attempt of a live subject, and on a
// first page the counts by state. A page is the places it READ, so it may be short, or empty with
// a cursor, as the window's is.
func (r *Reader) standingChecks(ctx context.Context, tx *sql.Tx, q ReadQuery, c CheckQuery, chart Roster,
	cp statelog.Position, limit int, out *ReadAnswer) error {

	sc := standingScope{asker: q.Asker, ceiling: q.CeilingSeat, chart: chart}
	if !c.Since.IsZero() {
		sc.since = max(store.EncodeTime(c.Since), 1)
	}
	if q.Repo != "" {
		repo, err := resolvingIn(ctx, tx, q, chart, cp, out)
		if err != nil {
			return err
		}
		sc.repoID, sc.repoName = repo.ref.ID, repo.ref.Name
	}
	phases := standingPhases(c)
	start, from := 0, checkPlace{at: math.MaxInt64}
	if q.Cursor != "" {
		phase, place, ok := cutStandingCursor(c, q.Cursor)
		if !ok || (sc.repoID != "" && place.key.RepoID != sc.repoID) {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		start, from = slices.Index(phases, phase), place
	}
	var places []standingPlace
	for _, phase := range phases[start:] {
		if len(places) == limit {
			break
		}
		got, err := sc.phasePlaces(ctx, tx, c.Name, phase, from, limit-len(places))
		if err != nil {
			return fmt.Errorf("gitlet: list where checks stand: %w", err)
		}
		places = append(places, got...)
		from = checkPlace{at: math.MaxInt64}
	}
	verdicts, err := sc.verdicts(ctx, tx, places)
	if err != nil {
		return err
	}
	var standing []CheckKey
	for i, p := range places {
		if verdicts[i].stands {
			standing = append(standing, p.key)
		}
	}
	whole, err := standingRowsIn(ctx, tx, standing)
	if err != nil {
		return err
	}
	rows := make([]CheckRow, 0, len(places))
	kept := map[CheckKey]bool{}
	for i, p := range places {
		v := verdicts[i]
		row, found := whole[p.key]
		switch {
		case !v.stands:
			rows = append(rows, CheckRow{CheckKey: p.key, State: p.state, FinishedAt: activityAt(p.at)})
			continue
		case !found:
			return fmt.Errorf("gitlet: check %s of %s for %s, attempt %d, was listed and holds no row",
				p.key.Name, p.key.OID, p.key.Target, p.key.Attempt)
		}
		row.Repo, row.Change, row.SourceRef, row.ChangeHead = v.repo, v.change, v.sourceRef, v.changeHead
		kept[p.key] = true
		rows = append(rows, row)
	}
	r.pageChecks(rows, func(row CheckRow) bool { return kept[row.CheckKey] }, limit, standingCursorOf(c), out)
	if q.Cursor == "" {
		if out.StandingCounts, err = r.standingCounts(ctx, tx, sc, c.Name); err != nil {
			return err
		}
	}
	return nil
}

// standingCounts is the standing attempts in scope by state, each state counted among at most
// r.standingCounted of its newest places, apart: a plentiful state never starves a rare one.
func (r *Reader) standingCounts(ctx context.Context, tx *sql.Tx, sc standingScope, name string) (*StandingCounts,
	error) {

	out := &StandingCounts{}
	for _, s := range CheckStates() {
		// ONE PLACE PAST THE BOUND says whether the count stopped short of a place.
		places, err := sc.phasePlaces(ctx, tx, name, s, checkPlace{at: math.MaxInt64}, r.standingCounted+1)
		if err != nil {
			return nil, fmt.Errorf("gitlet: count where checks stand: %w", err)
		}
		if len(places) > r.standingCounted {
			places = places[:r.standingCounted]
			out.AtLeast = append(out.AtLeast, s)
		}
		verdicts, err := sc.verdicts(ctx, tx, places)
		if err != nil {
			return nil, err
		}
		for _, v := range verdicts {
			if v.stands {
				out.Counts.count(s)
			}
		}
	}
	return out, nil
}

// phasePlaces is the first n places of phase after from, of the check name narrows to where it
// names one: the five endings merged where phase is "", else one state's. A repository's are
// sought on the indexes holding every state, the window's endings on those partial on them and
// its attempts not yet ended on code_checks_unended_idx; an attempt not yet ended is in every window.
func (sc standingScope) phasePlaces(ctx context.Context, tx *sql.Tx, name string, phase CheckState,
	from checkPlace, n int) ([]standingPlace, error) {

	states := []CheckState{phase}
	if phase == "" {
		states = endedCheckStates()
	}
	var out []standingPlace
	for _, s := range states {
		var (
			got []checkPlace
			err error
		)
		switch {
		case sc.repoID != "":
			got, err = seekPlaces(ctx, tx, sc.repoID, name, s, from, 0, n)
			// NEWEST ENDED FIRST, so the first place before the window ends the walk.
			if i := slices.IndexFunc(got, func(p checkPlace) bool { return p.at > 0 && p.at < sc.since }); i >= 0 {
				got = got[:i]
			}
		case s.Ended():
			got, err = seekPlaces(ctx, tx, "", name, s, from, max(sc.since, 1), n)
		default:
			got, err = unendedPlaces(ctx, tx, name, s, from, n)
		}
		if err != nil {
			return nil, err
		}
		for _, p := range got {
			out = append(out, standingPlace{checkPlace: p, state: s})
		}
	}
	slices.SortFunc(out, func(a, b standingPlace) int { return a.compare(b.checkPlace) })
	return out[:min(len(out), n)], nil
}

// unendedPlaces is the first n places after from of the window's attempts in state, queued or
// running, of the check name narrows to where it names one.
func unendedPlaces(ctx context.Context, tx *sql.Tx, name string, state CheckState, from checkPlace,
	n int) ([]checkPlace, error) {

	k := from.key
	rows, err := tx.QueryContext(ctx, windowUnendedPlacesSQL, string(state), from.at, k.RepoID, k.OID, k.Target,
		k.Name, k.Attempt, name, n)
	if err != nil {
		return nil, err
	}
	var out []checkPlace
	err = eachRow(rows, func(s scanner) (err error) {
		var p checkPlace
		if err = s.Scan(&p.at, &p.key.OID, &p.key.Target, &p.key.Attempt, &p.key.RepoID, &p.key.Name); err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	return out, err
}

// standingVerdict is whether a place's attempt stands, and where it does its repository's name
// and the change it judges: none for a tip's.
type standingVerdict struct {
	stands                bool
	repo                  string
	change                *ChangeName
	sourceRef, changeHead string
}

// standingFact is what a place's attempt was for, and whether it is its key's latest.
type standingFact struct {
	purpose            CheckPurpose
	candidateID, entry string
	latest             bool
}

// standingEntry is a queue entry a candidate's attempt names: its change, the head it was
// enqueued at, the candidate testing it, and whether it is its change's newest entry.
type standingEntry struct {
	changeID, head, candidateID string
	newest                      bool
}

// verdicts judges each of places: held to the grant (the window's by its repositories, as
// [Reader.narrowedWindow] holds its places), then by its facts and the subject they name. A head's
// is the live change into its branch at its commit, whichever change's dispatch queued it, as a
// merge gate reads one; a candidate's is its queue entry, its change's newest and still tested by
// that candidate, of a live change still into the candidate's branch; a tip's is its branch. Each
// kind is one statement over the places' list, never one a place.
func (sc standingScope) verdicts(ctx context.Context, tx *sql.Tx, places []standingPlace) ([]standingVerdict,
	error) {

	names := map[string]string{sc.repoID: sc.repoName}
	if sc.repoID == "" {
		ids := make([]string, len(places))
		for i, p := range places {
			ids[i] = p.key.RepoID
		}
		listed, err := listedReposAmong(ctx, tx, ids, sc.asker, sc.ceiling, sc.chart, "a page of checks")
		if err != nil {
			return nil, err
		}
		names = make(map[string]string, len(listed))
		for id, repo := range listed {
			names[id] = repo.ref.Name
		}
	}
	var granted []CheckKey
	for _, p := range places {
		if _, ok := names[p.key.RepoID]; ok {
			granted = append(granted, p.key)
		}
	}
	facts, err := standingFactsIn(ctx, tx, granted)
	if err != nil {
		return nil, err
	}
	var heads [][3]string
	var entries, tips [][2]string
	sought := map[[3]string]bool{}
	for _, key := range granted {
		switch f := facts[key]; {
		case !f.latest:
		case f.purpose == PurposeHead:
			// ONE SEEK A HEAD, however many of its checks the places hold.
			if head := [3]string{key.RepoID, key.Target, key.OID}; !sought[head] {
				sought[head] = true
				heads = append(heads, head)
			}
		case f.purpose == PurposeCandidate:
			entries = append(entries, [2]string{key.RepoID, f.entry})
		case f.purpose == PurposeTip:
			tips = append(tips, [2]string{key.RepoID, key.Target})
		}
	}
	atHead, err := standingHeadsIn(ctx, tx, heads)
	if err != nil {
		return nil, err
	}
	queued, err := standingEntriesIn(ctx, tx, entries)
	if err != nil {
		return nil, err
	}
	var entryChanges []string
	for _, e := range queued {
		entryChanges = append(entryChanges, e.changeID)
	}
	changes, err := changeRowsIn(ctx, tx, distinct(entryChanges))
	if err != nil {
		return nil, err
	}
	tipOf, err := standingTipsIn(ctx, tx, tips)
	if err != nil {
		return nil, err
	}
	out := make([]standingVerdict, len(places))
	for i, p := range places {
		f := facts[p.key]
		v := standingVerdict{repo: names[p.key.RepoID]}
		var change changeRow
		switch {
		case !f.latest:
		case f.purpose == PurposeHead:
			change, v.stands = atHead[[3]string{p.key.RepoID, p.key.Target, p.key.OID}]
			v.changeHead = p.key.OID
		case f.purpose == PurposeCandidate:
			// THE ENTRY'S OWN CANDIDATE: a train the queue gave up on and tested again leaves its
			// earlier candidate's attempts judging nothing, as a retargeted change leaves its old branch's.
			e := queued[[2]string{p.key.RepoID, f.entry}]
			change = changes[e.changeID]
			v.stands = e.newest && e.candidateID == f.candidateID && change.state.Live() &&
				change.target == p.key.Target
			v.changeHead = e.head
		case f.purpose == PurposeTip:
			v.stands = tipOf[[2]string{p.key.RepoID, p.key.Target}] == p.key.OID
		}
		switch {
		case !v.stands:
			v = standingVerdict{}
		case change.id != "":
			v.change = &ChangeName{Repo: v.repo, Ref: ChangeRef(v.repo, change.number), Title: change.title,
				State: change.state}
			v.sourceRef = change.source
		}
		out[i] = v
	}
	return out, nil
}

// keyList is keys as the one JSON argument a statement's json_each reads them from, each as
// [repository, commit, target, name, attempt].
func keyList(keys []CheckKey) (string, error) {
	list := make([][5]any, len(keys))
	for i, k := range keys {
		list[i] = [5]any{k.RepoID, k.OID, k.Target, k.Name, k.Attempt}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", fmt.Errorf("gitlet: encode a key list: %w", err)
	}
	return string(b), nil
}

// standingFactsIn is the facts of the attempt at each of keys, by its key.
func standingFactsIn(ctx context.Context, tx *sql.Tx, keys []CheckKey) (map[CheckKey]standingFact, error) {
	out := make(map[CheckKey]standingFact, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	list, err := keyList(keys)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, standingFactsSQL, list)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %d checks' facts: %w", len(keys), err)
	}
	err = eachRow(rows, func(s scanner) (err error) {
		var (
			i       int
			f       standingFact
			purpose string
		)
		if err = s.Scan(&i, &purpose, &f.candidateID, &f.entry, &f.latest); err != nil {
			return err
		}
		if i < 0 || i >= len(keys) {
			return fmt.Errorf("the list holds no key %d", i)
		}
		f.purpose = CheckPurpose(purpose)
		out[keys[i]] = f
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %d checks' facts: %w", len(keys), err)
	}
	return out, nil
}

// standingRowsIn is the attempt at each of keys, as a listing reads it, by its key.
func standingRowsIn(ctx context.Context, tx *sql.Tx, keys []CheckKey) (map[CheckKey]CheckRow, error) {
	out := make(map[CheckKey]CheckRow, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	list, err := keyList(keys)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, standingRowsSQL, list)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %d checks: %w", len(keys), err)
	}
	err = eachRow(rows, func(s scanner) (err error) {
		var row CheckRow
		if row, err = scanCheck(s.Scan); err != nil {
			return err
		}
		out[row.CheckKey] = row
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %d checks: %w", len(keys), err)
	}
	return out, nil
}

// standingHeadsIn is the live change into each branch at each commit keys name as [repository,
// branch, commit]: the first the index holds, where two share one.
func standingHeadsIn(ctx context.Context, tx *sql.Tx, keys [][3]string) (map[[3]string]changeRow, error) {
	out := map[[3]string]changeRow{}
	if len(keys) == 0 {
		return out, nil
	}
	list, err := json.Marshal(keys)
	if err != nil {
		return nil, fmt.Errorf("gitlet: encode a key list: %w", err)
	}
	rows, err := tx.QueryContext(ctx, standingHeadsSQL, string(list))
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the live changes at %d heads: %w", len(keys), err)
	}
	err = eachRow(rows, func(s scanner) (err error) {
		var (
			key   [3]string
			c     changeRow
			state string
		)
		if err = s.Scan(&key[0], &key[1], &key[2], &c.id, &c.number, &c.title, &state, &c.source); err != nil {
			return err
		}
		c.state = ChangeState(state)
		if _, seen := out[key]; !seen {
			out[key] = c
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the live changes at %d heads: %w", len(keys), err)
	}
	return out, nil
}

// standingEntriesIn is the queue entries keys name, each by its repository and id.
func standingEntriesIn(ctx context.Context, tx *sql.Tx, keys [][2]string) (map[[2]string]standingEntry, error) {
	out := map[[2]string]standingEntry{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, standingEntriesSQL, pairList(keys))
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %d queue entries: %w", len(keys), err)
	}
	err = eachRow(rows, func(s scanner) (err error) {
		var repo, entry string
		var e standingEntry
		if err = s.Scan(&repo, &entry, &e.changeID, &e.head, &e.candidateID, &e.newest); err != nil {
			return err
		}
		out[[2]string{repo, entry}] = e
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %d queue entries: %w", len(keys), err)
	}
	return out, nil
}

// standingTipsIn is the tip of each branch keys name by its repository and ref; a branch deleted
// has none.
func standingTipsIn(ctx context.Context, tx *sql.Tx, keys [][2]string) (map[[2]string]string, error) {
	out := map[[2]string]string{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, standingTipsSQL, pairList(keys))
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %d branches' tips: %w", len(keys), err)
	}
	err = eachRow(rows, func(s scanner) (err error) {
		var repo, ref, tip string
		if err = s.Scan(&repo, &ref, &tip); err != nil {
			return err
		}
		out[[2]string{repo, ref}] = tip
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %d branches' tips: %w", len(keys), err)
	}
	return out, nil
}

// pairList is keys, each once, as the one JSON argument a statement's json_each reads pairs from.
func pairList(keys [][2]string) string {
	seen := map[[2]string]bool{}
	var b strings.Builder
	b.WriteByte('[')
	for _, k := range keys {
		if seen[k] {
			continue
		}
		if len(seen) > 0 {
			b.WriteByte(',')
		}
		seen[k] = true
		b.WriteString("[" + strconv.Quote(k[0]) + "," + strconv.Quote(k[1]) + "]")
	}
	b.WriteByte(']')
	return b.String()
}

// standingCursorOf spells a standing listing's cursor after a row:
// "<phase>/<finished_at>/<repo>/<commit>/<target>/<name>/<attempt>", the phase the row's state or
// [endedPhase] for an ending of the recent order's merged phase, an attempt not yet ended at 0.
func standingCursorOf(c CheckQuery) func(CheckRow) string {
	merged := slices.Contains(standingPhases(c), "")
	return func(row CheckRow) string {
		phase, at := string(row.State), int64(0)
		if merged && row.State.Ended() {
			phase = endedPhase
		}
		if !row.FinishedAt.IsZero() {
			at = store.EncodeTime(row.FinishedAt)
		}
		return phase + "/" + strconv.FormatInt(at, 10) + "/" + row.RepoID + "/" + row.OID + "/" + targetCursor(row)
	}
}

// cutStandingCursor reads a [standingCursorOf] of c: a phase c's order walks, an ending's instant
// for an ended phase and 0 for one not yet ended, and a whole key of the name c narrows to.
func cutStandingCursor(c CheckQuery, s string) (CheckState, checkPlace, bool) {
	head, rest, ok := strings.Cut(s, "/")
	phase := CheckState(head)
	if head == endedPhase {
		phase = ""
	}
	if !ok || head == "" || !slices.Contains(standingPhases(c), phase) {
		return "", checkPlace{}, false
	}
	head, rest, ok = strings.Cut(rest, "/")
	at, err := strconv.ParseInt(head, 10, 64)
	if !ok || err != nil || at < 0 || at == math.MaxInt64 || (at == 0) != (phase != "" && !phase.Ended()) {
		return "", checkPlace{}, false
	}
	repo, rest, ok := strings.Cut(rest, "/")
	if !ok || !canonicalUUID(repo) {
		return "", checkPlace{}, false
	}
	oid, rest, ok := strings.Cut(rest, "/")
	if !ok || !gitref.ValidOID(oid) {
		return "", checkPlace{}, false
	}
	target, name, attempt, ok := cutTargetCursor(rest)
	if !ok || rest == "" || (c.Name != "" && name != c.Name) {
		return "", checkPlace{}, false
	}
	return phase, checkPlace{at: at, key: CheckKey{RepoID: repo, OID: oid, Target: target, Name: name,
		Attempt: attempt}}, true
}

// A STANDING LISTING'S READS, each registered in plan_test.go: the window's attempts not yet ended
// in one state after a place, a covering seek of the index partial on them (its predicate repeated
// verbatim), the name a narrowing inside the walk; and, each driven from a page's list, its places'
// facts and rows by code_checks' key, a later attempt by the key's prefix, the live changes at its
// heads by code_changes_target_idx, its entries and tips by their keys, and an entry's later entry
// by code_queue_change_idx. One statement a list, since the driver prepares each statement anew.
const (
	windowUnendedPlacesSQL = `SELECT finished_at, commit_oid, target_ref, attempt, repo_id, name
		FROM code_checks INDEXED BY code_checks_unended_idx
		WHERE state IN ('queued', 'running') AND state = ?1 AND finished_at <= ?2
			AND (finished_at < ?2 OR repo_id < ?3 OR (repo_id = ?3 AND (commit_oid < ?4
				OR (commit_oid = ?4 AND (target_ref < ?5 OR (target_ref = ?5 AND (name < ?6
					OR (name = ?6 AND attempt < ?7))))))))
			AND (?8 = '' OR name = ?8)
		ORDER BY finished_at DESC, repo_id DESC, commit_oid DESC, target_ref DESC, name DESC, attempt DESC
		LIMIT ?9`
	standingFactsSQL = `SELECT j.key, k.purpose, k.candidate_id, k.entry_id, NOT EXISTS (SELECT 1 FROM code_checks AS n INDEXED BY ` + checksKey + `
				WHERE n.repo_id = k.repo_id AND n.commit_oid = k.commit_oid AND n.target_ref = k.target_ref
					AND n.name = k.name AND n.attempt > k.attempt)
		FROM json_each(?) AS j CROSS JOIN code_checks AS k INDEXED BY ` + checksKey + `
		WHERE ` + standingKeyed
	standingRowsSQL = `SELECT ` + checkColumns + ` FROM json_each(?) AS j
		CROSS JOIN code_checks AS k INDEXED BY ` + checksKey + ` WHERE ` + standingKeyed
	standingKeyed = `k.repo_id = CAST(j.value ->> 0 AS TEXT) AND k.commit_oid = CAST(j.value ->> 1 AS TEXT)
			AND k.target_ref = CAST(j.value ->> 2 AS TEXT) AND k.name = CAST(j.value ->> 3 AS TEXT)
			AND k.attempt = CAST(j.value ->> 4 AS INTEGER)`
	standingHeadsSQL = `SELECT c.repo_id, c.target_ref, c.head_oid, c.id, c.number, c.title, c.state, c.source_ref
		FROM json_each(?) AS j CROSS JOIN code_changes AS c INDEXED BY code_changes_target_idx
		WHERE c.repo_id = CAST(j.value ->> 0 AS TEXT) AND c.target_ref = CAST(j.value ->> 1 AS TEXT)
			AND c.head_oid = CAST(j.value ->> 2 AS TEXT) AND c.` + liveChanges
	standingEntriesSQL = `SELECT q.repo_id, q.entry_id, q.change_id, q.head_oid, q.candidate_id,
			NOT EXISTS (SELECT 1 FROM code_queue AS n INDEXED BY code_queue_change_idx
				WHERE n.change_id = q.change_id AND n.seq > q.seq)
		FROM json_each(?) AS j CROSS JOIN code_queue AS q INDEXED BY ` + queueKey + `
		WHERE q.repo_id = CAST(j.value ->> 0 AS TEXT) AND q.entry_id = CAST(j.value ->> 1 AS TEXT)`
	standingTipsSQL = `SELECT r.repo_id, r.ref, r.target FROM json_each(?) AS j
		CROSS JOIN code_refs AS r INDEXED BY ` + refsKey + `
		WHERE r.repo_id = CAST(j.value ->> 0 AS TEXT) AND r.ref = CAST(j.value ->> 1 AS TEXT)`
)
