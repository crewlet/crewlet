package builtin

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// releaseFacts is what a release request's conditions read beside the code
// host: the labels of a work item a Gitlet change is on.
type releaseFacts struct {
	labels func(ctx context.Context, item *types.WorkItem) ([]string, error)
}

// unmet is every condition a merge or a deploy does not meet now, read from
// its code host when a delegate allows it: as the delegate's turn on Gitlet,
// whose grant the reads are held to, and as the requesting seat's app on
// GitHub, which the merge is made with. None where the allow may stand. A
// condition the engine cannot read is unmet: a person decides.
func (d ReleaseDeps) unmet(ctx context.Context, turn *turnctx.Turn, requester string, kind delegated.Kind,
	args map[string]any, rulePaths []string, f releaseFacts) []delegated.Unmet {

	switch kind {
	case delegated.KindPRMerge:
		a, refusal := d.mergeAskOf(ReviewDelegatedTool, args)
		if refusal != nil {
			return []delegated.Unmet{unread("the request's arguments no longer read as a merge")}
		}
		if a.github {
			return d.pullMergeUnmet(ctx, requester, a, rulePaths)
		}
		return d.gitletMergeUnmet(ctx, turn, requester, a, rulePaths, f)
	case delegated.KindDeploy:
		a, refusal := d.deployAskOf(ReviewDelegatedTool, args)
		if refusal != nil {
			return []delegated.Unmet{unread("the request's arguments no longer read as a deploy")}
		}
		if a.github {
			return d.pullDeployUnmet(ctx, requester, a, rulePaths)
		}
		return d.gitletDeployUnmet(ctx, turn, a, rulePaths, f)
	}
	return nil
}

// unread is a condition the engine could not read.
func unread(why string) delegated.Unmet {
	return delegated.Unmet{Word: delegated.UnmetUnread, Says: "The engine could not read " + why +
		", so a person decides."}
}

// pathsUnmet are the always-escalate paths among paths, or the one unmet
// condition of a list the engine could not read whole.
func pathsUnmet(ref string, paths []string, complete bool, rulePaths []string) []delegated.Unmet {
	if !complete {
		return []delegated.Unmet{{Word: delegated.UnmetUnclassified, Says: ref + " touches more paths than " +
			"the engine reads, so the paths no delegate allows cannot be ruled out."}}
	}
	var out []delegated.Unmet
	for _, m := range delegated.EscalatingPaths(paths, rulePaths) {
		out = append(out, delegated.Unmet{Word: m.Class, Says: fmt.Sprintf("%s %s (%s): no delegate allows "+
			"that.", ref, m.Says, m.Path)})
	}
	return out
}

// checksUnmet is what a commit's required checks leave unmet: none required,
// or each that did not pass.
func checksUnmet(what string, required []CheckFact) []delegated.Unmet {
	if len(required) == 0 {
		return []delegated.Unmet{{Word: delegated.UnmetNoChecks, Says: what + "'s branch requires no check, so " +
			"nothing shows CI passed."}}
	}
	var failing []string
	for _, c := range required {
		if c.Result != CheckPassed {
			failing = append(failing, c.Name+" ("+c.Result+")")
		}
	}
	if len(failing) == 0 {
		return nil
	}
	return []delegated.Unmet{{Word: delegated.UnmetChecks, Says: "Not every required check passed on " + what +
		": " + words.And(failing) + "."}}
}

// ---- GitHub ------------------------------------------------------------ //

// pullMergeUnmet is a pull request's merge conditions, as the requester reads
// it.
func (d ReleaseDeps) pullMergeUnmet(ctx context.Context, requester string, a mergeAsk,
	rulePaths []string) []delegated.Unmet {

	f, err := d.Pulls.Facts(ctx, requester, a.pull)
	if err != nil {
		log.WarnContext(ctx, "release_facts_unread", "change", a.ref, "error", err.Error())
		return []delegated.Unmet{unread(a.ref + " from GitHub")}
	}
	var out []delegated.Unmet
	add := func(word, says string) { out = append(out, delegated.Unmet{Word: word, Says: says}) }
	switch {
	case !f.Open:
		add(delegated.UnmetNotOpen, a.ref+" is not open.")
	case f.Head != a.head:
		add(delegated.UnmetHeadMoved, fmt.Sprintf("%s's head is %s now, not %s, the one asked about.", a.ref,
			f.Head, a.head))
	case f.Draft:
		add(delegated.UnmetNotReady, a.ref+" is a draft.")
	}
	if !f.RequiredRead {
		out = append(out, unread("which checks "+a.ref+"'s branch requires"))
	} else {
		out = append(out, checksUnmet(a.ref+"'s head", f.Required)...)
	}
	switch {
	case f.Mergeable == nil:
		add(delegated.UnmetMergeable, "GitHub has not yet worked out whether "+a.ref+" merges cleanly.")
	case !*f.Mergeable || f.MergeableState == "dirty":
		add(delegated.UnmetMergeable, a.ref+" does not merge cleanly into "+f.Base+".")
	case f.MergeableState == "blocked" || f.MergeableState == "behind":
		add(delegated.UnmetNotReady, fmt.Sprintf("GitHub's rules for %s hold %s back (%s).", f.Base, a.ref,
			f.MergeableState))
	}
	if !slices.ContainsFunc(f.Approvers, func(by string) bool { return by != f.Author && by != requester }) {
		add(delegated.UnmetApproval, "Nobody but its author and the requester approved "+a.ref+" at "+
			abbrevOID(a.head)+".")
	}
	if label, sensitive := delegated.SensitiveLabel(f.Labels); sensitive {
		add(delegated.OverruledLabel, a.ref+" is labelled "+label+".")
	}
	return append(out, pathsUnmet(a.ref, f.Paths, f.PathsComplete, rulePaths)...)
}

// pullDeployUnmet is a deploy's conditions on GitHub, as the requester reads
// them.
func (d ReleaseDeps) pullDeployUnmet(ctx context.Context, requester string, a deployAsk,
	rulePaths []string) []delegated.Unmet {

	var out []delegated.Unmet
	add := func(word, says string) { out = append(out, delegated.Unmet{Word: word, Says: says}) }
	base := ""
	for _, n := range a.changes {
		ref := PullRef{Owner: a.pulls.Owner, Repo: a.pulls.Repo, Number: int(n)}
		f, err := d.Pulls.Facts(ctx, requester, ref)
		if err != nil {
			log.WarnContext(ctx, "release_facts_unread", "change", ref.String(), "error", err.Error())
			return append(out, unread(ref.String()+" from GitHub"))
		}
		switch {
		case !f.Merged || f.MergeCommit == "":
			add(delegated.UnmetNotMerged, ref.String()+" has not merged.")
			continue
		case base != "" && f.Base != base:
			add(delegated.UnmetNotContained, fmt.Sprintf("%s merged into %s and another change into %s: a "+
				"deploy ships one branch.", ref, f.Base, base))
			continue
		}
		base = f.Base
		held, err := d.Pulls.Contains(ctx, requester, a.pulls, a.commit, f.MergeCommit)
		switch {
		case err != nil:
			return append(out, unread("whether "+abbrevOID(a.commit)+" holds "+ref.String()))
		case !held:
			add(delegated.UnmetNotContained, fmt.Sprintf("%s does not hold %s (merged as %s).",
				abbrevOID(a.commit), ref, abbrevOID(f.MergeCommit)))
		}
		if label, sensitive := delegated.SensitiveLabel(f.Labels); sensitive {
			add(delegated.OverruledLabel, ref.String()+" is labelled "+label+".")
		}
		out = append(out, pathsUnmet(ref.String(), f.Paths, f.PathsComplete, rulePaths)...)
	}
	if base == "" {
		return out
	}
	// ON THE BRANCH THE CHANGES MERGED INTO: its tip holds the commit.
	onBranch, err := d.Pulls.OnBranch(ctx, requester, a.pulls, base, a.commit)
	switch {
	case err != nil:
		return append(out, unread("whether "+base+" holds "+abbrevOID(a.commit)))
	case !onBranch:
		add(delegated.UnmetNotContained, abbrevOID(a.commit)+" is not on "+base+".")
	}
	required, read, err := d.Pulls.Checks(ctx, requester, a.pulls, base, a.commit)
	if err != nil || !read {
		return append(out, unread("which checks "+base+" requires, and their results on "+abbrevOID(a.commit)))
	}
	return append(out, checksUnmet(abbrevOID(a.commit), required)...)
}

// ---- Gitlet ------------------------------------------------------------ //

// gitletMergeUnmet is a Gitlet change's merge conditions, read as the
// delegate: the change as its target's gate judges it, its reviews, its work
// item's labels and its head revision's paths.
func (d ReleaseDeps) gitletMergeUnmet(ctx context.Context, turn *turnctx.Turn, requester string, a mergeAsk,
	rulePaths []string, facts releaseFacts) []delegated.Unmet {

	g, refusal := d.changeRead(ctx, turn, ReviewDelegatedTool, a)
	if refusal != nil {
		return []delegated.Unmet{unread(a.ref + ": " + refusal.Output)}
	}
	ch, rev, at, refusal := g.detail(ctx, statelog.Position{})
	if refusal != nil {
		return []delegated.Unmet{unread(a.ref + ": " + refusal.Output)}
	}
	var out []delegated.Unmet
	add := func(word, says string) { out = append(out, delegated.Unmet{Word: word, Says: says}) }
	switch {
	case !ch.State.Live():
		add(delegated.UnmetNotOpen, fmt.Sprintf("%s is %s.", a.ref, ch.State))
	case ch.Head != a.head:
		add(delegated.UnmetHeadMoved, fmt.Sprintf("%s's head is %s now, not %s, the one asked about.", a.ref,
			ch.Head, a.head))
	}
	out = append(out, gateUnmet(a.ref, ch.Readiness)...)
	approved, err := g.headApproved(ctx, ch, at, requester)
	switch {
	case err != nil:
		out = append(out, unread(a.ref+"'s reviews"))
	case !approved:
		add(delegated.UnmetApproval, fmt.Sprintf("Nobody but its authors and the requester approved revision %d "+
			"of %s.", ch.Revision, a.ref))
	}
	out = append(out, facts.itemLabels(ctx, a.ref, ch.WorkItem)...)
	paths, complete, err := g.revisionPaths(ctx, &ch.ChangeSummary, rev)
	if err != nil {
		return append(out, unread(a.ref+"'s paths"))
	}
	return append(out, pathsUnmet(a.ref, paths, complete, rulePaths)...)
}

// gateUnmet is what a change's readiness leaves unmet: the target's required
// checks on its head, whether it merges cleanly, and every other precondition
// of its gate, but the merger's authority and its queue, which the merge
// itself is judged on as the requesting seat.
func gateUnmet(ref string, r *gitlet.Readiness) []delegated.Unmet {
	if r == nil {
		return []delegated.Unmet{unread(ref + "'s readiness")}
	}
	var out []delegated.Unmet
	checked := false
	for _, p := range r.Preconditions {
		met := p.State == gitlet.PreconditionMet
		switch p.Kind {
		case gitlet.PreconditionMergeAuthority, gitlet.PreconditionQueue:
		case gitlet.PreconditionCheck:
			checked = true
			if !met {
				out = append(out, delegated.Unmet{Word: delegated.UnmetChecks, Says: "Not every required " +
					"check passed on " + ref + "'s head: " + p.Detail + "."})
			}
		case gitlet.PreconditionMergeable:
			if !met {
				out = append(out, delegated.Unmet{Word: delegated.UnmetMergeable, Says: ref + " does not " +
					"merge cleanly: " + p.Detail + "."})
			}
		default:
			if !met {
				out = append(out, delegated.Unmet{Word: delegated.UnmetNotReady, Says: fmt.Sprintf("%s's "+
					"target holds it back (%s): %s.", ref, p.Kind, p.Detail)})
			}
		}
	}
	if !checked {
		out = append(out, delegated.Unmet{Word: delegated.UnmetNoChecks, Says: ref + "'s target requires no " +
			"check, so nothing shows CI passed on its head."})
	}
	return out
}

// headApproved reports whether somebody other than ch's authors and requester
// approved its head revision, among its newest reviews.
func (g *changeRead) headApproved(ctx context.Context, ch *gitlet.ChangeDetail, at statelog.Position,
	requester string) (bool, error) {

	authors := []string{ch.Owner, requester}
	for _, a := range ch.Authors {
		authors = append(authors, a.Author)
	}
	cursor := ""
	for range releaseReviewPages {
		page, refusal := g.read(ctx, gitlet.ReadQuery{Part: gitlet.PartReviews, Cursor: cursor,
			Limit: changeReviewsPage}, at)
		if refusal != nil {
			return false, fmt.Errorf("builtin: %s", refusal.Output)
		}
		for _, r := range page.Reviews {
			if r.Revision == ch.Revision && r.Verdict == gitlet.VerdictApprove &&
				!slices.Contains(authors, r.Reviewer) {
				return true, nil
			}
		}
		if cursor = page.Next; cursor == "" {
			return false, nil
		}
	}
	return false, nil
}

// revisionPaths are the paths revision rev of ch touches, from its base, and
// whether the list is whole: a revision whose files are not recorded, or that
// touches more than [releaseFilePages] pages of them, is not.
func (g *changeRead) revisionPaths(ctx context.Context, ch *gitlet.ChangeSummary, rev gitlet.RevisionRow) ([]string,
	bool, error) {

	if rev.FilesGap(ch.State, true) != "" {
		return nil, false, nil
	}
	q, err := gitlet.RevisionDiff(g.repo.ID, rev, nil, "", "")
	if err != nil {
		return nil, false, err
	}
	q.Fresh = g.floor(rev)
	var paths []string
	for range releaseFilePages {
		page, refusal := g.deps.gitRead(ctx, g.turn, g.tool, g.c, g.repo, q)
		if refusal != nil {
			return nil, false, fmt.Errorf("builtin: %s", refusal.Output)
		}
		if page.Diff == nil {
			return nil, false, fmt.Errorf("builtin: Gitlet answered %s's files with no diff", g.ref())
		}
		for _, f := range page.Diff.Files {
			paths = append(paths, diffPath(f.Path, f.PathB64))
			if f.OldPath != "" || f.OldPathB64 != "" {
				paths = append(paths, diffPath(f.OldPath, f.OldPathB64))
			}
		}
		if page.Next == "" {
			return paths, true, nil
		}
		q.Cursor = page.Next
	}
	return paths, false, nil
}

// diffPath is a file's path as its bytes spell it: text, or the base64 of a
// path that is not UTF-8.
func diffPath(path, b64 string) string {
	if path != "" || b64 == "" {
		return path
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return b64
	}
	return string(raw)
}

// itemLabels is the sensitive label of the work item a change is on, or the
// condition unmet where its labels cannot be read.
func (f releaseFacts) itemLabels(ctx context.Context, ref string, item *types.WorkItem) []delegated.Unmet {
	if item == nil || f.labels == nil {
		return nil
	}
	labels, err := f.labels(ctx, item)
	if err != nil {
		return []delegated.Unmet{unread("the labels of " + ref + "'s work item")}
	}
	if label, sensitive := delegated.SensitiveLabel(labels); sensitive {
		return []delegated.Unmet{{Word: delegated.OverruledLabel, Says: ref + "'s work item is labelled " +
			label + "."}}
	}
	return nil
}

// gitletDeployUnmet is a deploy's conditions on Gitlet, read as the delegate:
// every change merged into one branch, the commit holding each and on that
// branch at or behind its newest green commit, and no change's paths
// always escalating.
func (d ReleaseDeps) gitletDeployUnmet(ctx context.Context, turn *turnctx.Turn, a deployAsk,
	rulePaths []string, facts releaseFacts) []delegated.Unmet {

	var out []delegated.Unmet
	add := func(word, says string) { out = append(out, delegated.Unmet{Word: word, Says: says}) }
	target := ""
	var g *changeRead
	for _, n := range a.changes {
		ref := gitlet.ChangeRef(a.repo, n)
		read, refusal := d.changeRead(ctx, turn, ReviewDelegatedTool, mergeAsk{repo: a.repo, number: n})
		if refusal != nil {
			return append(out, unread(ref+": "+refusal.Output))
		}
		g = read
		ch, rev, _, refusal := g.head(ctx, statelog.Position{})
		if refusal != nil {
			return append(out, unread(ref+": "+refusal.Output))
		}
		switch {
		case ch.State != gitlet.ChangeStateMerged || ch.MergeOID == "":
			add(delegated.UnmetNotMerged, fmt.Sprintf("%s has not merged (it is %s).", ref, ch.State))
			continue
		case target != "" && ch.TargetRef != target:
			add(delegated.UnmetNotContained, fmt.Sprintf("%s merged into %s and another change into %s: a "+
				"deploy ships one branch.", ref, ch.TargetRef, target))
			continue
		}
		target = ch.TargetRef
		switch held, err := g.holds(ctx, a.commit, ch.MergeOID); {
		case err != nil:
			return append(out, unread("whether "+abbrevOID(a.commit)+" holds "+ref))
		case !held:
			add(delegated.UnmetNotContained, fmt.Sprintf("%s does not hold %s (merged as %s).",
				abbrevOID(a.commit), ref, abbrevOID(ch.MergeOID)))
		}
		out = append(out, facts.itemLabels(ctx, ref, ch.WorkItem)...)
		paths, complete, err := g.revisionPaths(ctx, ch, rev)
		if err != nil {
			return append(out, unread(ref+"'s paths"))
		}
		out = append(out, pathsUnmet(ref, paths, complete, rulePaths)...)
	}
	if target == "" {
		return out
	}
	return append(out, g.greenUnmet(ctx, a.commit, target)...)
}

// holds reports whether commit holds ancestor on the change's repository: the
// two the same, or ancestor behind commit.
func (g *changeRead) holds(ctx context.Context, commit, ancestor string) (bool, error) {
	if commit == ancestor {
		return true, nil
	}
	page, refusal := g.deps.gitRead(ctx, g.turn, g.tool, g.c, g.repo, gitlet.GitQuery{Kind: gitlet.GitCompare,
		RepoID: g.repo.ID, Fresh: g.deps.gitFresh(g.repo),
		Compare: &gitlet.CompareQuery{Base: ancestor, Head: commit}})
	if refusal != nil {
		return false, fmt.Errorf("builtin: %s", refusal.Output)
	}
	if page.Compare == nil {
		return false, fmt.Errorf("builtin: Gitlet answered a compare of %s with none", g.ref())
	}
	return page.Compare.BaseIsAncestor, nil
}

// greenUnmet is what keeps commit from counting as green on target: the
// branch has no green commit (it requires no check), or commit is not at or
// behind it.
func (g *changeRead) greenUnmet(ctx context.Context, commit, target string) []delegated.Unmet {
	answer, err := g.deps.read(ctx, g.c, gitlet.ReadQuery{Kind: gitlet.ReadGreen, Repo: g.repo.Name, Target: target,
		Fresh: seatRead})
	if err != nil {
		return []delegated.Unmet{unread(strings.TrimPrefix(target, branchRoot) + "'s green commit")}
	}
	branch := strings.TrimPrefix(target, branchRoot)
	i := slices.IndexFunc(answer.Green, func(r gitlet.GreenRow) bool { return r.TargetRef == target })
	if i < 0 {
		return []delegated.Unmet{{Word: delegated.UnmetNoChecks, Says: branch + " has no green commit: it " +
			"requires no check, so nothing shows CI passed on " + abbrevOID(commit) + "."}}
	}
	green := answer.Green[i].CommitOID
	switch held, err := g.holds(ctx, green, commit); {
	case err != nil:
		return []delegated.Unmet{unread("whether " + branch + "'s green commit holds " + abbrevOID(commit))}
	case !held:
		return []delegated.Unmet{{Word: delegated.UnmetChecks, Says: fmt.Sprintf("%s is not at or behind %s's "+
			"newest green commit %s, so its required checks have not passed on it.", abbrevOID(commit), branch,
			abbrevOID(green))}}
	}
	return nil
}
