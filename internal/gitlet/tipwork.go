package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A PROTECTED TARGET'S TIP WORK, READ FROM THE ROWS (ADR-0098): every landing
// on a protected target leaves a row (code_landings) and moves its green row
// (code_green) only where its record proved the target's required checks —
// a queue's landing, which its candidates' checks proved, and a
// merge or a push into a target whose gate requires none. Every other landing
// — a merge a person's override landed, a person's own push under
// `direct_push: persons` — lies ABOVE ITS TARGET'S GREEN until a check of a
// tip proves it, and its home owes the target that work: a `checks_due` read
// of one repository answers it on its first pages ([ReadAnswer.DueTips], paged
// by [ReadAnswer.TipsNext] before its heads), one [TipDue] a target holding
// a landing neither green nor broken.
//
// A LANDING IS GREEN BY ITS POSITION — at or below its target's green row —
// never by a flag of its own: a green record moves the one row, so it writes
// the same few rows however many landings lie beneath it, and a target that
// gathered any backlog of unproven landings behind a red tip greens again on
// the first tip that passes (replicated 0142).
//
// A TipDue is everything the home decides its next step from, in ONE
// snapshot: the target's green row, the checks its gate requires, and its
// landings above the green, oldest first, each with the latest attempt of
// each required check recorded on its commit FOR THE TARGET — whatever the
// attempt's purpose, since a check is a measurement of a commit, and a
// fast-forward lands the very commit its change's head checks measured. The
// home then (internal/gitlet/home's tips) dispatches the newest landing's tip
// where it holds no attempt — coalesced, ONE ROUND AT A TIME: a landing that
// is no longer the newest is never dispatched, and the newest waits while a
// tip round on another of the target's commits is still queued or running
// ([TipDue.InFlight]), so the landings that arrive meanwhile are measured
// together by the next round — moves the green row onto it where every
// required check passed, and where one failed BISECTS the failed checks over
// the landing rows — never over git's history, which a merge's second parent
// would walk into — down to the first landing they failed on, which it
// records broken.
//
// THE WINDOW is [MaxTipLandings] landings above the green: a bisect over it
// takes log₂ of it rounds of checks. A target holding more is read as its
// oldest MaxTipLandings and its newest ([TipDue.More]), so a dispatch always
// reaches the tip; a culprit past the window is the one thing it cannot name
// until a green tip clears the landings before it (see the home's bisect).
//
// LISTED BY ONE ROW A TARGET: a target holding a landing above its green that
// no bisect found broken holds a row of tip work (code_tips_due), which the
// landing's record inserts and the record that moves the green past it, or
// names the last of them broken, removes. The listings walk those rows — the
// repositories owing tip work, and one repository's targets — so they cost a
// row a target, never one a landing above a stuck tip, and never the thirty
// days of green ones each target keeps behind it. A target whose culprit is
// named stays listed while landings lie above the culprit, until a tip passes
// — a home's pass reads its window and finds nothing owed — which costs a
// broken target's home a few reads a minute and is what lets a fix's landing
// be dispatched by the backstop as well as by its nudge.

// MaxTipLandings is the most landings above its green one target's tip work
// reads: 256, so a bisect over a whole window takes eight rounds of checks
// (log₂ 256), and a window's attempts — at most MaxRequiredChecks a landing,
// recorded only where a dispatch or a probe ran one — stay a small part of a
// page. JUDGMENT: 256 landings unproven on one target are days of a person's
// direct pushes with every tip red; a team that lands that much past a red
// tip has a branch to revert, not a bisect to wait for.
const MaxTipLandings = 256

// TipDue is one protected target's tip work as one snapshot holds it
// ([ReadAnswer.DueTips]).
type TipDue struct {
	TargetRef string `json:"target_ref"`
	// Green is the target's green row; nil for a target none of whose tips
	// has passed yet, whose first landing's base its tip checks are defined
	// at.
	Green *GreenRow `json:"green,omitempty"`
	// Required is every check the target's gate requires, sorted: what a tip
	// dispatch queues, and what its green row moves on. None where its gate
	// requires none, when the newest landing is green as it stands.
	Required []string `json:"required,omitempty"`
	// Landings are the target's landings above its green, oldest first: all
	// of them, or, for a target holding more than MaxTipLandings, its oldest
	// MaxTipLandings and then its newest — More where landings lie between
	// those two, which the window does not read.
	Landings []TipLanding `json:"landings"`
	More     bool         `json:"more,omitempty"`
	// InFlight is a commit of the target's other than its newest landing's
	// that a tip attempt is still queued or running on — the ROUND IN FLIGHT,
	// which the newest waits for before it is dispatched — read by the index
	// partial on exactly those attempts (replicated 0143), so it is found
	// wherever that commit lies, past the window included; "" for none.
	InFlight string `json:"in_flight,omitempty"`
}

// Newest is the target's newest landing, the tip its checks are dispatched
// at.
func (t TipDue) Newest() TipLanding { return t.Landings[len(t.Landings)-1] }

// Base is the commit the target's tip checks are DEFINED at: the base of its
// first landing above its green — the green itself, or, before any tip passed,
// the commit the first landing moved the target from (M5-D15). Never a
// landing's own commit: a landing that edits the checks file would define the
// measurement it is judged by.
func (t TipDue) Base() string { return t.Landings[0].Base }

// TipLanding is one landing above its target's green as tip work reads it:
// its key on the target, the target before and after it, whether it ends its
// record — the commit a green row may name — whether a bisect already found
// it broken, and the latest attempt of each required check recorded on its
// commit for the target, none for a name never queued there.
type TipLanding struct {
	Position statelog.Position `json:"position"`
	Ordinal  int               `json:"ordinal"`
	Base     string            `json:"base_oid"`
	OID      string            `json:"commit_oid"`
	Last     bool              `json:"last,omitempty"`
	Broken   bool              `json:"broken,omitempty"`
	Checks   []TipCheck        `json:"checks,omitempty"`
}

// Check is the latest attempt of name the landing holds, and false for none.
func (l TipLanding) Check(name string) (TipCheck, bool) {
	for _, c := range l.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return TipCheck{}, false
}

// TipCheck is the latest attempt of one required check on a landing's commit
// for its target: its attempt, where it stands and why it ended.
type TipCheck struct {
	Name    string      `json:"name"`
	Attempt int         `json:"attempt"`
	State   CheckState  `json:"state"`
	Reason  CheckReason `json:"reason,omitempty"`
}

// dueTips files the tip work of repository repoID's targets after after — ""
// for the first — into out, within pg: each target owing it, by name, its
// window read whole, with required, its gate's checks by target. It answers
// whether the page ended among them, when [ReadAnswer.TipsNext] names the
// last target it answered — the one that filled the page among them, which a
// page APPENDS BEFORE IT ASKS whether it is full, as every page here does —
// and the next page of the same turn goes on after it, before any head. A
// target past a page is never left to a later pass, which would list the same
// targets first again and never reach it while they stay owed.
func (r *Reader) dueTips(ctx context.Context, tx *sql.Tx, repoID, after string, required *requiredChecks,
	pg *page, out *ReadAnswer) (bool, error) {

	targets, err := dueTipTargetsIn(ctx, tx, repoID, after, r.pageRows)
	if err != nil {
		return false, err
	}
	for _, target := range targets {
		t, err := tipDueOf(ctx, tx, repoID, target, required, r.tipWindow)
		switch {
		case err != nil:
			return false, err
		case len(t.Landings) == 0:
			continue
		}
		encoded, err := json.Marshal(t)
		if err != nil {
			return false, fmt.Errorf("gitlet: encode %s's tip work: %w", target, err)
		}
		out.DueTips = append(out.DueTips, t)
		if pg.add(len(encoded) + 1) {
			out.TipsNext = target
			return true, nil
		}
	}
	if len(targets) == r.pageRows {
		// THE LISTING CUT AT ITS BOUND, every target it named answered.
		out.TipsNext = targets[len(targets)-1]
		return true, nil
	}
	return false, nil
}

// dueTipTargetsIn is repoID's targets owing tip work after after — each
// holding a landing above its green that no bisect found broken — by name, at
// most limit.
func dueTipTargetsIn(ctx context.Context, tx *sql.Tx, repoID, after string, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, dueTipTargetsSQL, repoID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("gitlet: list repository %s's targets owing tip work: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, fmt.Errorf("gitlet: list repository %s's targets owing tip work: %w", repoID, err)
		}
		out = append(out, target)
	}
	return out, rows.Err()
}

// tipDueOf is target's tip work on repoID: its green, its gate's checks, its
// window of at most bound landings above the green — MaxTipLandings — then
// its newest, and each one's latest attempts.
func tipDueOf(ctx context.Context, tx *sql.Tx, repoID, target string, required *requiredChecks,
	bound int) (TipDue, error) {
	t := TipDue{TargetRef: target}
	green, held, err := greenIn(ctx, tx, repoID, target)
	if err != nil {
		return TipDue{}, err
	}
	from, ordinal := int64(0), -1
	if held {
		t.Green = &green
		from, ordinal = green.Position.Packed(), math.MaxInt32
	}
	if t.Required, err = required.of(target); err != nil {
		return TipDue{}, fmt.Errorf("gitlet: read %s's required checks: %w", target, err)
	}
	// ONE ROW PAST THE WINDOW says whether more lie above it, and whether
	// the window's last landing ends its record.
	window, err := tipLandingsIn(ctx, tx, tipLandingsSQL, repoID, target, from, from, ordinal, bound+1)
	if err != nil {
		return TipDue{}, err
	}
	if len(window) > bound {
		next := window[bound]
		window = window[:bound]
		window[bound-1].Last = window[bound-1].Position != next.Position
		newest, err := tipLandingsIn(ctx, tx, newestLandingSQL, repoID, target)
		if err != nil {
			return TipDue{}, err
		}
		newest[0].Last = true
		// MORE only where a landing lies between the window and the newest:
		// a newest that is the very row past the window follows it with no
		// gap, and a bisect may name it as it would any landing it reads.
		t.More = newest[0].Position != next.Position || newest[0].Ordinal != next.Ordinal
		window = append(window, newest[0])
	} else if len(window) > 0 {
		window[len(window)-1].Last = true
	}
	for i := 0; i+1 < len(window) && (!t.More || i+2 < len(window)); i++ {
		window[i].Last = window[i].Position != window[i+1].Position
	}
	t.Landings = window
	if len(window) > 0 {
		switch err := tx.QueryRowContext(ctx, tipInFlightSQL, repoID, target, t.Newest().OID).Scan(
			&t.InFlight); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return TipDue{}, fmt.Errorf("gitlet: read whether %s's tip round is in flight: %w", target, err)
		}
	}
	return t, tipChecksIn(ctx, tx, repoID, target, t)
}

// tipLandingsIn reads the landings query answers, in its order.
func tipLandingsIn(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]TipLanding, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's landings above its green: %w", args[1], err)
	}
	defer func() { _ = rows.Close() }()
	var out []TipLanding
	for rows.Next() {
		var (
			l      TipLanding
			packed int64
		)
		if err := rows.Scan(&packed, &l.Ordinal, &l.Base, &l.OID, &l.Broken); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's landings above its green: %w", args[1], err)
		}
		l.Position = unpacked(packed)
		out = append(out, l)
	}
	switch err := rows.Err(); {
	case err != nil:
		return nil, err
	case query == newestLandingSQL && len(out) == 0:
		return nil, errors.New("gitlet: a target holding landings above its green answered no newest")
	}
	return out, nil
}

// tipChecksIn files the latest attempt of each of t's required checks on each
// of its landings' commits for target into t, in one statement over the
// checks' key ([gateChecksIn]).
func tipChecksIn(ctx context.Context, tx *sql.Tx, repoID, target string, t TipDue) error {
	if len(t.Required) == 0 {
		return nil
	}
	facts := make([]GateFacts, len(t.Landings))
	names := make([][]string, len(t.Landings))
	byHead := map[[3]string][]int{}
	var keys [][4]any
	for i, l := range t.Landings {
		names[i] = t.Required
		head := [3]string{repoID, l.OID, target}
		if _, seen := byHead[head]; !seen {
			for _, name := range t.Required {
				keys = append(keys, [4]any{repoID, l.OID, target, name})
			}
		}
		byHead[head] = append(byHead[head], i)
	}
	if err := gateChecksIn(ctx, tx, keys, byHead, names, facts); err != nil {
		return err
	}
	for i, f := range facts {
		for _, c := range f.Checks {
			t.Landings[i].Checks = append(t.Landings[i].Checks, TipCheck{Name: c.Name, Attempt: c.Attempt,
				State: c.State, Reason: c.Reason})
		}
	}
	return nil
}

// mergeRepoPages is two keyset pages of repository ids after one cursor, each
// ascending and at most limit — the repositories owing check work to their
// changes, and those owing it to their tips — as ONE page of the union:
// ascending, each id once, at most limit, and its cursor the last id kept
// where either page or the union was cut at its bound.
func mergeRepoPages(a, b []string, limit int) ([]string, string) {
	out := make([]string, 0, min(len(a)+len(b), limit))
	i, j := 0, 0
	for len(out) < limit && (i < len(a) || j < len(b)) {
		switch {
		case j == len(b) || (i < len(a) && a[i] < b[j]):
			out = append(out, a[i])
			i++
		case i == len(a) || b[j] < a[i]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i, j = i+1, j+1
		}
	}
	if len(out) == 0 || (len(a) < limit && len(b) < limit && i == len(a) && j == len(b)) {
		return out, ""
	}
	return out, out[len(out)-1]
}

// The tip work's reads, each registered in plan_test.go: the repositories,
// and one repository's targets, owing tip work, by the key of its rows — one a
// target, each repository tested by its key; a target's landings above its
// green by the landings' key, and its newest, the key read backwards; and a
// tip attempt not ended on another of its commits, by the index partial on
// exactly those attempts, its predicate spelled as the index declares it.
const (
	dueTipReposSQL = `SELECT d.repo_id FROM code_tips_due AS d
		WHERE d.repo_id > ?1
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = d.repo_id AND r.status = 'active')
		GROUP BY d.repo_id ORDER BY d.repo_id LIMIT ?2`
	dueTipTargetsSQL = `SELECT target_ref FROM code_tips_due WHERE repo_id = ?1 AND target_ref > ?2
		ORDER BY target_ref LIMIT ?3`
	tipLandingsSQL = `SELECT position, ordinal, base_oid, commit_oid, broken FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position >= ? AND (position > ? OR ordinal > ?)
		ORDER BY position, ordinal LIMIT ?`
	newestLandingSQL = `SELECT position, ordinal, base_oid, commit_oid, broken FROM code_landings
		WHERE repo_id = ? AND target_ref = ? ORDER BY position DESC, ordinal DESC LIMIT 1`
	tipInFlightSQL = `SELECT commit_oid FROM code_checks INDEXED BY code_checks_tip_live_idx
		WHERE repo_id = ? AND target_ref = ? AND state IN ('queued', 'running') AND purpose = 'tip'
			AND commit_oid != ? LIMIT 1`
)
