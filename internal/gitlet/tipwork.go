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

// A protected target's tip work (ADR-0098). Every landing on a protected
// target leaves a code_landings row, and moves the target's green row
// (code_green) only where its record proved the required checks: a queue's
// landing, or a merge or push into a target whose gate requires none. Any other
// landing (an override's merge, a `direct_push: persons` push) lies above the
// green until a tip check proves it, and a `checks_due` read answers that work
// on its first pages: [ReadAnswer.DueTips], paged by [ReadAnswer.TipsNext]
// before its heads.
//
// A landing is green by its position, at or below the green row, never by a
// flag: a green record writes one row however many landings lie beneath it, so
// a backlog behind a red tip greens on the first passing tip (replicated 0142).

// MaxTipLandings is the most landings above its green one target's tip work
// reads: a bisect over a whole window takes eight rounds of checks (log₂ 256),
// and its attempts (at most MaxRequiredChecks a landing, only where a dispatch
// or probe ran one) stay a small part of a page. A target holding more is read
// as its oldest MaxTipLandings and its newest ([TipDue.More]), so a dispatch
// always reaches the tip; a culprit past the window cannot be named until a
// green tip clears the landings before it (the home's bisect). JUDGMENT: 256
// unproven landings on one target are days of direct pushes with every tip
// red: a branch to revert, not a bisect to wait for.
const MaxTipLandings = 256

// TipDue is one protected target's tip work as one snapshot holds it
// ([ReadAnswer.DueTips]): everything its home decides the next step from. The
// home (internal/gitlet/home's tips) dispatches the newest landing's tip where
// it holds no attempt, one round at a time ([TipDue.InFlight]) so landings
// arriving meanwhile are measured together; moves the green onto it where every
// required check passed; and where one failed, bisects the failed checks over
// the landing rows (never git's history, which a merge's second parent would
// walk into) to the first landing they failed on, which it records broken.
type TipDue struct {
	TargetRef string `json:"target_ref"`
	// Green is the target's green row; nil before any of its tips passed, when
	// its tip checks are defined at its first landing's base ([TipDue.Base]).
	Green *GreenRow `json:"green,omitempty"`
	// Required is every check the target's gate requires, sorted: what a tip
	// dispatch queues and its green row moves on. None where the gate requires
	// none, when the newest landing is green as it stands.
	Required []string `json:"required,omitempty"`
	// Landings are the target's landings above its green, oldest first: all
	// of them, or past MaxTipLandings its oldest MaxTipLandings and then its
	// newest, More where unread landings lie between the two.
	Landings []TipLanding `json:"landings"`
	More     bool         `json:"more,omitempty"`
	// InFlight is a commit of the target's, other than its newest landing's,
	// that a tip attempt is still queued or running on: the round in flight,
	// which the newest waits for. Read by the index partial on exactly those
	// attempts (replicated 0143), so found past the window too; "" for none.
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
// record (the commit a green row may name), whether a bisect found it broken,
// and the latest attempt of each required check on its commit for the target,
// whatever the attempt's purpose: a check measures a commit, and a fast-forward
// lands the very commit its change's head checks measured.
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

// dueTips files into out, within pg, the tip work of repoID's targets after
// after ("" for the first): each target owing it, by name, its window read
// whole, required naming its gate's checks. It answers whether the page ended
// among them, [ReadAnswer.TipsNext] then naming the last target answered (a
// page APPENDS BEFORE IT ASKS whether it is full), and the turn's next page
// resumes after it, before any head: a later pass would list the same targets
// first again and never reach one past the page while they stay owed.
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

// dueTipTargetsIn is repoID's targets owing tip work after after (a landing
// above the green no bisect found broken), by name, at most limit. Each holds
// one code_tips_due row, which a landing's record inserts and the record that
// moves the green past it, or names the last of them broken, removes: a listing
// costs a row a target, never one a landing. A target whose culprit is named
// stays listed while landings lie above the culprit, until a tip passes: a few
// reads a minute for its home, so the backstop, not only its nudge, can
// dispatch a fix's landing.
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
// window of at most bound (MaxTipLandings) landings above the green, then its
// newest, and each one's latest attempts.
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
