package github

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// What a branch requires to pass before a merge, and how one commit stands
// against it, read as GitHub judges it.
//
// # Required is read from both places GitHub keeps it
//
// A branch's required checks live in its classic protection and in every
// active ruleset that targets it, at the repository or the organization, and
// GitHub enforces the union. So [Client.RequiredChecks] reads both and unions
// them: either alone answers "none" for a branch the other protects.
//
// # A requirement can name the app that must report it
//
// Protection (`checks[].app_id`) and a ruleset (`integration_id`) can bind a
// context to one GitHub App, and GitHub then counts only that app's report.
// The binding is kept ([RequiredCheck.AppID]) because an agent's own app may
// hold Checks: Read and write, which lets it post a check run of any name on
// any commit: matched by name alone, its report would pass a check the
// repository's own CI never ran.
//
// # Every source must pass
//
// GitHub's rule for a name reported both as a check run and as a commit
// status is that both must pass. So a requirement passes here only when every
// source reporting it passed: the newest check run of each app, and the
// commit status. A newer success from one app does not hide another app's
// failure.
//
// # An agent never reports its own change as passing
//
// An agent's app that may write checks can post a check run of any name, and
// a commit status of any context where it may write statuses, on its own
// head. So a report from one of the company's own agents' apps
// ([Reporters]) counts for nothing here, whatever GitHub's own merge makes of
// it: a merge or deploy decision stands only on a report its requester could
// not have made, and a requirement bound to an agent's app reads missing.
//
// # What could not be read is said, never guessed
//
// When GitHub refuses the credential one of the two sources (403: the
// installation lacks Checks for check runs, or Commit statuses for statuses),
// a requirement that source could report is [CheckUnread] rather than
// missing, unless what could be read already failed it. "Missing" is a fact
// about the commit; "unread" is a fact about the credential, and only a
// person can change that one. Any other failure (a timeout, a 5xx, a rate
// limit) fails the whole read, since a retry may answer it.

// RequiredCheck is one status check a branch requires before a merge.
type RequiredCheck struct {
	// Context is the check's name as GitHub matches it: a check run's
	// name or a commit status's context.
	Context string `json:"context" untrusted:"name"`

	// AppID is the GitHub App that must report it, or zero for any source.
	AppID int64 `json:"app_id,omitempty"`
}

// CheckState is how one required check stands on one commit.
type CheckState string

const (
	// CheckPassed is every source reporting it finished with success,
	// neutral or skipped, the three GitHub counts as passing.
	CheckPassed CheckState = "passed"

	// CheckFailed is a source reporting it finished any other way.
	CheckFailed CheckState = "failed"

	// CheckPending is a source reporting it has not finished, and none
	// failed.
	CheckPending CheckState = "pending"

	// CheckMissing is no source reporting it at all.
	CheckMissing CheckState = "missing"

	// CheckUnread is a source that could report it being refused to this
	// credential, and nothing read failing it ([CheckResult.Unreadable]).
	CheckUnread CheckState = "unread"
)

// Valid reports one of the five states.
func (s CheckState) Valid() bool {
	switch s {
	case CheckPassed, CheckFailed, CheckPending, CheckMissing, CheckUnread:
		return true
	}
	return false
}

// CheckResult is one required check on one commit.
type CheckResult struct {
	RequiredCheck

	State CheckState `json:"state"`

	// Unreadable is each GitHub permission, by GitHub's name, the credential
	// lacked to read a source that could report this check: "Checks" for
	// check runs, "Commit statuses" for commit statuses. Set whenever such a
	// source was refused; the state is [CheckUnread] unless what could be
	// read already failed the check.
	Unreadable []string `json:"unreadable,omitempty"`
}

// The permissions [CheckResult.Unreadable] names, by GitHub's names: Checks as
// the Agents tab offers it, and Commit statuses, which no agent's app is
// offered, so a status a private repository's CI reports reads unread.
var (
	unreadableChecks   = catalogueName(PermissionChecks)
	unreadableStatuses = "Commit statuses"
)

// Reporters are the sources whose report of a check counts for nothing: the
// company's own agents' apps, by app id for a check run and by bot login for
// a commit status.
type Reporters struct {
	AppIDs []int64
	Logins []string
}

// app reports whether a check run by app id is one of r's.
func (r Reporters) app(id int64) bool { return id != 0 && slices.Contains(r.AppIDs, id) }

// login reports whether a status its creator login posted is one of r's.
func (r Reporters) login(login string) bool {
	login = NormalizeLogin(login)
	return login != "" && slices.ContainsFunc(r.Logins, func(l string) bool { return NormalizeLogin(l) == login })
}

// catalogueName is one catalogue permission's GitHub name.
func catalogueName(key string) string {
	perm, _ := LookupPermission(key)
	return perm.Name
}

// factPageSize is GitHub's per_page maximum for every list a merge decision
// reads.
const factPageSize = 100

// factPageLimit bounds those lists, in pages.
//
// A thousand rows, and past it the read is REFUSED rather than cut: a partial
// list of reviews could miss a later change request, and a partial list of
// check runs a failure, so a short answer would be a wrong one. GitHub's own
// check-run listing stops at the thousand most recent check suites, and a
// pull request with a thousand reviews is not one a delegate should decide
// on.
const factPageLimit = 10

// ErrListTooLong reports a list a merge decision reads that runs past
// [factPageLimit] pages.
var ErrListTooLong = errors.New(
	"github: GitHub lists more here than this engine reads, and part of the list " +
		"is not an answer about all of it")

// walk reads a paginated list until a page comes back short, as
// [Client.listWebhooks] does, decoding each page as P and taking its rows.
//
// A list still going after [factPageLimit] pages is refused with
// [ErrListTooLong]. One more page is read to tell a list of exactly that many
// full pages from a longer one, for the reason [hookWalkCeiling] compares
// with >: refusing the first would say "more than" of a list that is not.
func walk[P, R any](
	ctx context.Context, c *Client, path string, params url.Values, rows func(P) []R,
) ([]R, error) {
	out := []R{}
	for page := 1; ; page++ {
		query := maps.Clone(params)
		if query == nil {
			query = url.Values{}
		}
		query.Set("per_page", strconv.Itoa(factPageSize))
		query.Set("page", strconv.Itoa(page))
		var got P
		if err := c.get(ctx, path, query, &got); err != nil {
			return nil, err
		}
		batch := rows(got)
		if page > factPageLimit {
			if len(batch) == 0 {
				return out, nil
			}
			return nil, fmt.Errorf("%w: %s lists more than %d", ErrListTooLong, path,
				factPageLimit*factPageSize)
		}
		out = append(out, batch...)
		if len(batch) < factPageSize {
			return out, nil
		}
	}
}

// repoPath is a repository's REST path.
func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// branchWire is what a branch read decodes: its classic protection's
// required checks, which GitHub shows to any reader of the repository.
type branchWire struct {
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
	Protected  bool `json:"protected"`
	Protection *struct {
		RequiredStatusChecks *struct {
			EnforcementLevel string   `json:"enforcement_level"`
			Contexts         []string `json:"contexts"`
			Checks           []struct {
				Context string `json:"context"`
				AppID   *int64 `json:"app_id"`
			} `json:"checks"`
		} `json:"required_status_checks"`
	} `json:"protection"`
}

// ruleWire is one active rule on a branch. Parameters are decoded per type.
type ruleWire struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

// BranchTip is the commit branch points at now, a full SHA.
func (c *Client) BranchTip(ctx context.Context, owner, repo, branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", errors.New("github: no branch to read the tip of")
	}
	var b branchWire
	if err := c.get(ctx, repoPath(owner, repo)+"/branches/"+url.PathEscape(branch), nil, &b); err != nil {
		return "", fmt.Errorf("github: read %s/%s's %s: %w", owner, repo, branch, err)
	}
	if !fullSHA(b.Commit.SHA) {
		return "", fmt.Errorf("github: %s/%s's %s answered no commit", owner, repo, branch)
	}
	return strings.ToLower(b.Commit.SHA), nil
}

// RequiredChecks is every status check branch requires before a merge: the
// union of its classic protection and the active rulesets targeting it,
// sorted. Empty, with no error, is a branch that requires none.
func (c *Client) RequiredChecks(ctx context.Context, owner, repo, branch string) ([]RequiredCheck, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil, errors.New("github: no branch to read the required checks of")
	}
	base := repoPath(owner, repo)
	var b branchWire
	if err := c.get(ctx, base+"/branches/"+url.PathEscape(branch), nil, &b); err != nil {
		return nil, fmt.Errorf("github: read the protection of %s/%s's %s: %w", owner, repo, branch, err)
	}
	var out []RequiredCheck
	if b.Protected {
		if b.Protection == nil {
			// NOT "NONE". GitHub shows a protected branch's protection to
			// every reader, so a protected branch without one is a shape
			// nothing documents, and reading it as unprotected would pass
			// every check it requires.
			return nil, fmt.Errorf("github: %s/%s's %s is protected and GitHub did not say "+
				"what its protection requires", owner, repo, branch)
		}
		// "off" IS GITHUB SAYING THE CHECKS ARE NOT ENFORCED, which is the
		// shape of a protected branch that requires reviews alone.
		if req := b.Protection.RequiredStatusChecks; req != nil && req.EnforcementLevel != "off" {
			bound := map[string]bool{}
			for _, check := range req.Checks {
				out = append(out, RequiredCheck{Context: check.Context, AppID: appOf(check.AppID)})
				bound[check.Context] = true
			}
			// contexts is the older spelling of the same list; a name it
			// has that checks does not is required from any source.
			for _, name := range req.Contexts {
				if !bound[name] {
					out = append(out, RequiredCheck{Context: name})
				}
			}
		}
	}

	rules, err := walk(ctx, c, base+"/rules/branches/"+url.PathEscape(branch), nil,
		func(rows []ruleWire) []ruleWire { return rows })
	switch {
	case notFound(err):
		// NO RULES ENDPOINT, so no rulesets. GitHub answers a repository
		// without rulesets with an empty list, and the branch read above
		// proved this credential sees the repository, so a 404 here is not
		// GitHub hiding it: it is a GitHub Enterprise Server from before
		// rulesets, which has none to enforce.
	case err != nil:
		return nil, fmt.Errorf("github: read the rules on %s/%s's %s: %w", owner, repo, branch, err)
	}
	for _, rule := range rules {
		if rule.Type != "required_status_checks" {
			continue
		}
		var params struct {
			RequiredStatusChecks []struct {
				Context       string `json:"context"`
				IntegrationID *int64 `json:"integration_id"`
			} `json:"required_status_checks"`
		}
		if decodeErr := json.Unmarshal(rule.Parameters, &params); decodeErr != nil {
			return nil, fmt.Errorf("github: decode a required-checks rule on %s/%s's %s: %w",
				owner, repo, branch, decodeErr)
		}
		for _, check := range params.RequiredStatusChecks {
			out = append(out, RequiredCheck{Context: check.Context, AppID: appOf(check.IntegrationID)})
		}
	}
	return unionChecks(out), nil
}

// appOf reads a requirement's app: null is any source, and so is -1, which
// is how protection is told to accept any app explicitly.
func appOf(id *int64) int64 {
	if id == nil || *id <= 0 {
		return 0
	}
	return *id
}

// unionChecks drops blank and repeated requirements and sorts the rest.
//
// One context bound to an app and also required from any source stays TWO
// requirements, because GitHub enforces each rule that names it. A context is
// kept exactly as GitHub spells it, since a report matches it exactly.
func unionChecks(in []RequiredCheck) []RequiredCheck {
	out := make([]RequiredCheck, 0, len(in))
	for _, check := range in {
		if strings.TrimSpace(check.Context) != "" {
			out = append(out, check)
		}
	}
	slices.SortFunc(out, func(a, b RequiredCheck) int {
		if n := strings.Compare(a.Context, b.Context); n != 0 {
			return n
		}
		return cmp.Compare(a.AppID, b.AppID)
	})
	return slices.Compact(out)
}

// checkRunsWire is one page of a commit's check runs.
type checkRunsWire struct {
	CheckRuns []checkRunWire `json:"check_runs"`
}

// checkRunWire is one check run on a commit.
type checkRunWire struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	App        *struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

func (r checkRunWire) appID() int64 {
	if r.App == nil {
		return 0
	}
	return r.App.ID
}

// state is a check run as a requirement reads it.
func (r checkRunWire) state() CheckState {
	if r.Status != "completed" {
		return CheckPending
	}
	switch r.Conclusion {
	case "success", "neutral", "skipped":
		return CheckPassed
	}
	return CheckFailed
}

// statusWire is one commit status, as the list of a commit's statuses gives
// it, newest first, with who posted it.
type statusWire struct {
	Context string `json:"context"`
	State   string `json:"state"`
	Creator *struct {
		Login string `json:"login"`
	} `json:"creator"`
}

// statusState is a commit status's state as a requirement reads it: error
// and failure fail, pending waits, success passes.
func statusState(state string) CheckState {
	switch state {
	case "success":
		return CheckPassed
	case "pending":
		return CheckPending
	}
	return CheckFailed
}

// CommitChecks is how each of required stands on the commit sha, in the
// order given, counting no report untrusted made. sha is a full commit id,
// never a branch: a fact about a ref is about whatever it points at by the
// time anyone acts on it.
func (c *Client) CommitChecks(
	ctx context.Context, owner, repo, sha string, required []RequiredCheck, untrusted Reporters,
) ([]CheckResult, error) {
	if !fullSHA(sha) {
		return nil, fmt.Errorf("github: %q is not a full commit id, so there is no one "+
			"commit to read the checks of", sha)
	}
	if len(required) == 0 {
		return []CheckResult{}, nil
	}
	commit := repoPath(owner, repo) + "/commits/" + url.PathEscape(sha)

	runs, runsErr := walk(ctx, c, commit+"/check-runs", url.Values{"filter": {"latest"}},
		func(page checkRunsWire) []checkRunWire { return page.CheckRuns })
	runsRead := true
	switch {
	case forbidden(runsErr):
		runsRead = false
	case runsErr != nil:
		return nil, fmt.Errorf("github: read the check runs on %s/%s@%s: %w", owner, repo, sha, runsErr)
	}
	runs = slices.DeleteFunc(runs, func(r checkRunWire) bool { return untrusted.app(r.appID()) })

	// STATUSES ONLY WHERE ONE CAN COUNT. A status names no app the API
	// reports, so an app-bound requirement is never met by one here. That is
	// stricter than GitHub where the bound app itself posts a status rather
	// than a check run: the requirement reads missing, which keeps a
	// decision from standing on it rather than letting one stand on an
	// unproven source. A branch whose every requirement is bound costs no
	// read of them.
	var statuses map[string]CheckState
	statusesRead := true
	if slices.ContainsFunc(required, func(r RequiredCheck) bool { return r.AppID == 0 }) {
		rows, statusesErr := walk(ctx, c, commit+"/statuses", nil,
			func(page []statusWire) []statusWire { return page })
		switch {
		case forbidden(statusesErr):
			statusesRead = false
		case statusesErr != nil:
			return nil, fmt.Errorf("github: read the commit statuses on %s/%s@%s: %w",
				owner, repo, sha, statusesErr)
		}
		// THE NEWEST OF EACH CONTEXT a trusted source posted, the list being
		// newest first.
		statuses = make(map[string]CheckState, len(rows))
		for _, row := range rows {
			if row.Creator != nil && untrusted.login(row.Creator.Login) {
				continue
			}
			if _, seen := statuses[row.Context]; !seen {
				statuses[row.Context] = statusState(row.State)
			}
		}
	}

	out := make([]CheckResult, 0, len(required))
	for _, req := range required {
		out = append(out, judge(req, runs, runsRead, statuses, statusesRead))
	}
	return out, nil
}

// judge is one requirement against what was read: the newest check run of
// each app it accepts, and the commit status where any source counts.
func judge(
	req RequiredCheck, runs []checkRunWire, runsRead bool,
	statuses map[string]CheckState, statusesRead bool,
) CheckResult {
	newest := map[int64]checkRunWire{}
	for _, run := range runs {
		if run.Name != req.Context || (req.AppID != 0 && run.appID() != req.AppID) {
			continue
		}
		// THE NEWEST PER APP, by id, which GitHub allocates in order: a
		// re-run supersedes the run it repeats, and another app's run is
		// another source rather than a newer answer.
		if held, ok := newest[run.appID()]; !ok || run.ID > held.ID {
			newest[run.appID()] = run
		}
	}
	var seen []CheckState
	for _, run := range newest {
		seen = append(seen, run.state())
	}
	result := CheckResult{RequiredCheck: req}
	if !runsRead {
		result.Unreadable = append(result.Unreadable, unreadableChecks)
	}
	if req.AppID == 0 {
		state, reported := statuses[req.Context]
		switch {
		case !statusesRead:
			result.Unreadable = append(result.Unreadable, unreadableStatuses)
		case reported:
			seen = append(seen, state)
		}
	}
	switch {
	case slices.Contains(seen, CheckFailed):
		result.State = CheckFailed
	case len(result.Unreadable) > 0:
		result.State = CheckUnread
	case slices.Contains(seen, CheckPending):
		result.State = CheckPending
	case len(seen) > 0:
		result.State = CheckPassed
	default:
		result.State = CheckMissing
	}
	return result
}

// forbidden reports GitHub refusing this credential, as opposed to its rate
// limit ([APIError.Forbidden]).
func forbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Forbidden()
}

// fullSHA reports a full commit id: forty hex digits, the only length GitHub
// names a commit with.
func fullSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, r := range sha {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}
