// Package gather reads the atlas: it asks each place the engine already keeps
// what it knows, turns the answers into claims ([derive]) and folds them
// ([atlas.Fold]). Nothing here is stored and nothing is written; every answer
// says which sources it read, which it could not, and how many values it left
// out, so a partial read is never drawn as a whole one.
//
// Each source is a narrow interface defined here, the consumer, and a nil one
// is a source this node does not serve or this reader may not read. A source
// that fails is reported as unread and the rest of the atlas still answers:
// one slow store does not blank the map.
//
// The decision is ADR-0126.
package gather

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// GitletReads is Gitlet's read as the engine serves it to every surface
// (estate.CodeSeam satisfies it): the reader's own repositories, narrowed by
// Gitlet to what the reader may read.
type GitletReads interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
}

// Log is the atlas's own log: what people told the engine (atlasdomain.Reader,
// or the estate router's reads of one).
type Log interface {
	Live(ctx context.Context, fresh statelog.Freshness) (atlasdomain.Live, error)
}

// Sources is where the atlas reads what the engine keeps, and what it was told.
// A nil source is one this node does not serve, or the reader may not read.
type Sources struct {
	Log          Log
	Gitlet       GitletReads
	Integrations IntegrationStates
	SeatAccess   SeatAccess
	ToolServers  ToolServers
	Nodes        Nodes
	ServerTests  ServerTests
	AccessLog    AccessLog
	Usage        Usage
	Alarms       Alarms
}

// Request is one read of the atlas.
type Request struct {
	// Company is the active company settings and Revision their revision;
	// they are read only through the redacting copy.
	Company  *config.Company
	Revision string
	// Deployment is the addresses the deployment answers at.
	Deployment derive.Deployment
	// Asker is who reads. Gitlet lists only the repositories they may read.
	Asker code.Principal
	// Fresh is how fresh the replicated reads must be.
	Fresh statelog.Freshness
	// Now is the instant the atlas is read at.
	Now time.Time
}

// SourceName names one place the atlas reads.
type SourceName string

// The places the atlas reads.
const (
	SourceSettings     SourceName = "settings"
	SourceLog          SourceName = "log"
	SourceGitlet       SourceName = "gitlet"
	SourceIntegrations SourceName = "integrations"
	SourceSeatAccess   SourceName = "seat_access"
	SourceToolServers  SourceName = "tool_servers"
	SourceNodes        SourceName = "nodes"
	SourceServerTests  SourceName = "server_tests"
	SourceAccessLog    SourceName = "access_log"
	SourceUsage        SourceName = "usage"
	SourceAlarms       SourceName = "alarms"
)

// SourceNames is every place the atlas reads, in the order an answer's
// coverage lists them.
var SourceNames = []SourceName{
	SourceAccessLog, SourceAlarms, SourceGitlet, SourceIntegrations, SourceLog, SourceNodes,
	SourceSeatAccess, SourceServerTests, SourceSettings, SourceToolServers, SourceUsage,
}

// Coverage is one source's part in a read: whether it was read in full, and
// why not where it was not.
type Coverage struct {
	Source   SourceName `json:"source"`
	Complete bool       `json:"complete"`
	Reason   string     `json:"reason,omitempty"`
}

// Answer is one read of the atlas.
type Answer struct {
	View     atlas.View        `json:"view"`
	Revision string            `json:"revision"`
	Coverage []Coverage        `json:"coverage"`
	Withheld []derive.Withheld `json:"withheld,omitempty"`

	// Recorded is every live claim the log answered, folded into View, for a
	// caller that decides on one the view does not show (a person's "not the
	// same"), and RecordedAt where that answer is current through. Never
	// sent anywhere: the view is the answer.
	Recorded   []atlas.Claim     `json:"-"`
	RecordedAt statelog.Position `json:"-"`
}

// Complete reports whether name was read in full.
func (a Answer) Complete(name SourceName) bool {
	i := slices.IndexFunc(a.Coverage, func(c Coverage) bool { return c.Source == name })
	return i >= 0 && a.Coverage[i].Complete
}

// Unread is a with the source name marked as not read in full, for why: a
// reason the caller has for not reading it, rather than the read's.
func (a Answer) Unread(name SourceName, why string) Answer {
	a.Coverage = slices.Clone(a.Coverage)
	for i := range a.Coverage {
		if a.Coverage[i].Source == name {
			a.Coverage[i].Complete, a.Coverage[i].Reason = false, why
		}
	}
	return a
}

// maxRepoPage is the page a repository listing is read in: Gitlet's own
// largest, so a company of a few hundred repositories is read in one or two.
const maxRepoPage = 500

// ErrNoSource is a source this node does not serve or the reader may not read.
var ErrNoSource = errors.New("not served here, or not readable by this reader")

// Read reads the atlas.
func Read(ctx context.Context, src Sources, req Request) Answer {
	settings := derive.FromSettings(req.Company, req.Revision, req.Deployment)
	var derived []derive.Derived
	answer := Answer{Revision: req.Revision}
	take := func(d derive.Derived) {
		derived = append(derived, d)
		answer.Withheld = append(answer.Withheld, d.Withheld...)
	}
	take(settings.Claims(req.Now))
	answer.Coverage = append(answer.Coverage, Coverage{Source: SourceSettings, Complete: true})

	repos, checks, gitlet := readGitlet(ctx, src.Gitlet, settings, req)
	take(settings.Gitlet(repos, checks, req.Now))
	answer.Coverage = append(answer.Coverage, gitlet)

	pushes := make([]derive.RepoPush, 0, len(repos))
	for _, r := range repos {
		pushes = append(pushes, derive.RepoPush{Repo: r.Name, Last: r.LastPush})
	}
	records, covered := readRecords(ctx, src, settings, req, pushes, gitlet.Complete)
	answer.Coverage = append(answer.Coverage, covered...)
	recorded, joined := settings.Records(records, req.Now)
	take(recorded)

	told, cov := readLog(ctx, src.Log, req)
	answer.Recorded, answer.RecordedAt = told.Claims, told.Position
	answer.Coverage = append(answer.Coverage, cov)

	total := 0
	for _, d := range derived {
		total += len(d.Claims)
	}
	claims := make([]atlas.Claim, 0, total+len(told.Claims))
	for _, d := range derived {
		claims = append(claims, d.Claims...)
	}
	claims = append(claims, told.Claims...)
	answer.View = atlas.Join(atlas.Fold(claims, req.Now), joined, req.Now)
	slices.SortFunc(answer.Coverage, func(a, b Coverage) int { return cmp.Compare(a.Source, b.Source) })
	return answer
}

// readLog reads every live claim the atlas's log holds, at the read's level. A
// log this node cannot answer from, or holding a record this build cannot
// decode, is reported as read in part, and its claims still fold.
func readLog(ctx context.Context, log Log, req Request) (atlasdomain.Live, Coverage) {
	cov := Coverage{Source: SourceLog}
	if log == nil {
		cov.Reason = ErrNoSource.Error()
		return atlasdomain.Live{}, cov
	}
	live, err := log.Live(ctx, req.Fresh)
	switch {
	case err != nil:
		cov.Reason = fmt.Sprintf("reading what people recorded: %v", err)
		return atlasdomain.Live{}, cov
	case !live.Complete:
		cov.Reason = "this node holds a record of the atlas's log a newer build wrote, which it cannot read yet"
	default:
		cov.Complete = true
	}
	return live, cov
}

// readGitlet lists every repository the asker may read, the other hosts of
// those the settings do not declare, and the checks each default branch runs
// at its head. A repository whose detail or checks cannot be read is still
// listed, and the read says it is partial.
func readGitlet(ctx context.Context, reads GitletReads, s derive.Settings, req Request) (
	[]derive.GitletRepo, []derive.GitletCheck, Coverage) {
	cov := Coverage{Source: SourceGitlet}
	if reads == nil {
		cov.Reason = ErrNoSource.Error()
		return nil, nil, cov
	}
	var summaries []code.RepoSummary
	for cursor := ""; ; {
		page, err := reads.Read(ctx, code.ReadQuery{Kind: code.ReadRepos, Asker: req.Asker, Fresh: req.Fresh,
			Cursor: cursor, Limit: maxRepoPage})
		if err != nil {
			cov.Reason = fmt.Sprintf("listing repositories: %v", err)
			return nil, nil, cov
		}
		summaries = append(summaries, page.Repos...)
		if cursor = page.Next; cursor == "" {
			break
		}
	}
	var repos []derive.GitletRepo
	var checks []derive.GitletCheck
	var failed []error
	for _, r := range summaries {
		repo := derive.GitletRepo{Name: r.Name, Status: string(r.Status), DefaultBranch: r.DefaultBranch,
			Visibility: string(r.Visibility), OwnerKind: string(r.Owner.Kind), Owner: r.Owner.Ref,
			LastPush: r.LastPushAt}
		if !s.Declares(r.Name) {
			if err := remotes(ctx, reads, req, &repo); err != nil {
				failed = append(failed, fmt.Errorf("the other hosts of %s: %w", r.Name, err))
			}
		}
		repos = append(repos, repo)
		if r.Head == "" || r.DefaultBranch == "" {
			continue
		}
		found, err := headChecks(ctx, reads, req, r)
		if err != nil {
			failed = append(failed, fmt.Errorf("the checks of %s: %w", r.Name, err))
			continue
		}
		checks = append(checks, found...)
	}
	cov.Complete = len(failed) == 0
	if len(failed) > 0 {
		cov.Reason = errors.Join(failed...).Error()
	}
	return repos, checks, cov
}

// remotes fills a repository's other hosts from its detail. The credential
// beside each is a reference the domain holds, and is never read.
func remotes(ctx context.Context, reads GitletReads, req Request, repo *derive.GitletRepo) error {
	a, err := reads.Read(ctx, code.ReadQuery{Kind: code.ReadRepo, Asker: req.Asker, Fresh: req.Fresh, Repo: repo.Name})
	if err != nil {
		return err
	}
	if a.Repo == nil {
		return nil
	}
	if m := a.Repo.Remotes.MirrorFrom; m != nil {
		repo.MirrorFrom = m.URL
	}
	if m := a.Repo.Remotes.MirrorTo; m != nil {
		repo.MirrorTo = m.URL
	}
	if i := a.Repo.Remotes.Import; i != nil {
		repo.ImportedFrom = i.Source.URL
	}
	return nil
}

// headChecks is each check's latest attempt on the default branch's head.
func headChecks(ctx context.Context, reads GitletReads, req Request, r code.RepoSummary) ([]derive.GitletCheck, error) {
	a, err := reads.Read(ctx, code.ReadQuery{Kind: code.ReadChecks, Asker: req.Asker, Fresh: req.Fresh, Repo: r.Name,
		Checks: &code.CheckQuery{OID: r.Head, Target: "refs/heads/" + r.DefaultBranch, Latest: true}})
	if err != nil {
		return nil, err
	}
	out := make([]derive.GitletCheck, 0, len(a.Checks))
	for _, c := range a.Checks {
		out = append(out, derive.GitletCheck{Repo: r.Name, Name: c.Name, State: string(c.State),
			Required: c.Required, External: c.Reporter != ""})
	}
	return out, nil
}
