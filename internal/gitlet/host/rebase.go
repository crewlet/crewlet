package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
)

// A CHANGE REBASED ONTO ITS TARGET, ON THE GITLET NODE (gitlet.WriteRebaseChange):
// the commits its target lacks, replayed oldest first onto the target's tip as
// `git rebase` replays them, and its source moved to the last as ONE forced
// push through the one landing (write.go), so its protections, its grant and
// the committer's decide hold it as a force-push of that branch, and the
// push's apply records the change's new revision. The first conflict stops it
// with nothing stored; a commit whose change the tip holds is dropped. Each
// replayed commit keeps its author (its date's zone too) and message byte for
// byte, committed by the engine (U43 a); one git would not write back as it
// was is refused, never rewritten: a rebase replays a history, never edits it.

// rebaseReadBatch is how many commits one `cat-file --batch` reads: SIXTEEN,
// a judgment. Each message is held whole until replayed, up to
// gitlet.MaxCommitPayloadBytes, so a batch holds at most 16 MiB where a whole
// gitlet.MaxRebaseCommits history read at once could hold a gigabyte; and one
// process per sixteen commits, each running two of its own, adds little.
const rebaseReadBatch = 16

// rebasing is one rebase's replay: the commit it is replaying onto, its tree
// and its committer date, and how many commits it replayed and dropped.
type rebasing struct {
	b                 *building
	tip, tree         string
	tipDate           time.Time
	replayed, dropped int
}

// rebase is a rebase_change's build: see this file's head. It answers the new
// head of the change's source — or a refusal of the change, nothing stored.
func (b *building) rebase(ctx context.Context) (string, gitlet.EngineWriteResult, bool, error) {
	wr := b.wr
	w := wr.w
	c, err := wr.rebasedChange(ctx)
	if err != nil {
		return "", gitlet.EngineWriteResult{}, true, err
	}
	named := gitlet.ChangeRef(wr.repoName(), c.Number)
	target, err := wr.headOf(ctx, c.TargetRef)
	switch {
	case err != nil:
		return "", gitlet.EngineWriteResult{}, true, err
	case target == "":
		return "", gitlet.EngineWriteResult{}, true, fmt.Errorf("%w: change: %s's target %s does not exist, so "+
			"there is nothing to rebase it onto", gitlet.ErrInvalid, named, c.TargetRef)
	}
	on, err := b.ancestor(ctx, target, w.ExpectedHead)
	switch {
	case err != nil:
		return "", gitlet.EngineWriteResult{}, true, err
	case on:
		detail := fmt.Sprintf("%s's head %s holds %s's tip %s already: there is nothing to rebase", named,
			shortID(w.ExpectedHead), c.TargetRef, shortID(target))
		return "", wr.changeRefused(gitlet.WriteNothingChanged, detail), true, nil
	}
	commits, res, done, err := b.toReplay(ctx, named, target)
	if done || err != nil {
		return "", res, true, err
	}
	base := &commitHeader{}
	if err := b.readCommits(ctx, []string{target}, []*commitHeader{base}); err != nil {
		return "", gitlet.EngineWriteResult{}, true, err
	}
	r := &rebasing{b: b, tip: target, tree: base.tree, tipDate: base.committed}
	for start := 0; start < len(commits); start += rebaseReadBatch {
		batch := commits[start:min(start+rebaseReadBatch, len(commits))]
		headers := make([]*commitHeader, len(batch))
		ids := make([]string, len(batch))
		for i, cm := range batch {
			headers[i], ids[i] = &commitHeader{keep: gitlet.MaxCommitPayloadBytes}, cm.id
		}
		if err := b.readCommits(ctx, ids, headers); err != nil {
			return "", gitlet.EngineWriteResult{}, true, err
		}
		for i, cm := range batch {
			if res, done, err := r.replay(ctx, named, cm, headers[i], start+i, len(commits)); done || err != nil {
				return "", res, true, err
			}
		}
	}
	if r.replayed == 0 {
		return "", wr.changeRefused(gitlet.WriteNothingChanged, fmt.Sprintf("every commit of %s is in %s already: "+
			"rebased, it would hold nothing of its own", named, c.TargetRef)), true, nil
	}
	return r.tip, gitlet.EngineWriteResult{}, false, nil
}

// rebasedChange is the change a rebase names, read at the copy's position as
// its caller, its detail alone (no revision is read): live, and from the
// branch the write moves.
func (wr *writing) rebasedChange(ctx context.Context) (*gitlet.ChangeDetail, error) {
	w, repo := wr.w, wr.synced.Repo
	ans, err := wr.s.o.Estate.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadChange, Asker: w.Principal, Repo: repo.Name,
		Change: w.Change, DetailOnly: true, Fresh: statelog.FloorAt(statelog.ReadSession, wr.synced.Position)})
	if err != nil {
		return nil, err
	}
	c := ans.Change
	named := gitlet.ChangeRef(repo.Name, w.Change)
	switch {
	case c == nil:
		return nil, fmt.Errorf("host: the read of %s answered no change", named)
	case !c.State.Live():
		return nil, fmt.Errorf("%w: change: %s is %s, and only a live change is rebased", gitlet.ErrInvalid, named,
			c.State)
	case c.SourceRef != w.Ref:
		return nil, fmt.Errorf("%w: ref: %s proposes %s, not %s: a rebase moves its change's source", gitlet.ErrInvalid,
			named, c.SourceRef, w.Ref)
	}
	return c, nil
}

// replayed is one commit a rebase replays: its id and its one parent.
type replayed struct{ id, parent string }

// toReplay is the commits the head holds and target does not, oldest first,
// from ONE `rev-list --topo-order --parents` of at most
// gitlet.MaxRebaseCommits+1 — or the refusal of a longer history
// (too_many_commits) or one holding a merge, which has no mainline to replay
// (merge_commit).
func (b *building) toReplay(ctx context.Context, named, target string) ([]replayed, gitlet.EngineWriteResult,
	bool, error) {

	wr := b.wr
	var (
		out   []replayed
		merge string
	)
	lines := gitproc.NewLines(func(line []byte) error {
		fields := bytes.Fields(line)
		if len(fields) == 0 || !gitref.ValidOID(fields[0]) {
			return fmt.Errorf("host: rev-list answered %q, which names no commit", line)
		}
		switch {
		case len(fields) > 2 && merge == "":
			merge = string(fields[0])
		case len(fields) == 2:
			out = append(out, replayed{id: string(fields[0]), parent: string(fields[1])})
		case len(fields) == 1:
			// A ROOT COMMIT the target does not hold: replayed onto the tip
			// from the empty tree, as git's own rebase replays one.
			out = append(out, replayed{id: string(fields[0]), parent: emptyTreeID})
		}
		return nil
	})
	left, err := wr.budget()
	if err != nil {
		return nil, gitlet.EngineWriteResult{}, true, err
	}
	res, err := wr.s.runGit(ctx, gitproc.Cmd{
		Dir: wr.synced.Dir, Config: wr.s.o.Cache.ServeConfig(),
		Args: []string{"rev-list", "--topo-order", "--parents", "--max-count=" +
			strconv.Itoa(gitlet.MaxRebaseCommits+1), wr.w.ExpectedHead, "^" + target},
		Stdout: lines, Class: gitproc.ClassRead, Deadline: left,
	})
	if err == nil {
		err = lines.Close()
	}
	if err = wr.failed(ctx, "listing of the commits to rebase", res, err); err != nil {
		return nil, gitlet.EngineWriteResult{}, true, err
	}
	switch n := len(out); {
	case merge != "":
		return nil, wr.changeRefused(gitlet.WriteMergeCommit, fmt.Sprintf("%s's commit %s merges others: a "+
			"rebase replays each commit onto the one before it, and a merge needs a mainline to replay — rebase "+
			"it in a checkout of the repository ("+toolname.CodeCheckout+")", named, shortID(merge))), true, nil
	case n > gitlet.MaxRebaseCommits:
		return nil, wr.changeRefused(gitlet.WriteTooManyCommits, fmt.Sprintf("%s holds more than %d commits its "+
			"target does not (gitlet.MaxRebaseCommits): rebase it in a checkout of the repository ("+toolname.CodeCheckout+")", named,
			gitlet.MaxRebaseCommits)), true, nil
	}
	slices.Reverse(out)
	return out, gitlet.EngineWriteResult{}, false, nil
}

// replay is commit cm (the i-th of n, header h) cherry-picked onto the
// rebase's tip: ONE merge-tree against its parent and ONE commit-tree keeping
// its author and message — or the rebase stopped at its conflict, or refused
// for a commit git would not write back as it was. Its committer date is the
// latest of the rebase's instant, its new parent's committer date and its
// author date (U18 c), so no commit is committed before it is authored or
// dated before its parent, and a rebase asked again builds the same commits.
func (r *rebasing) replay(ctx context.Context, named string, cm replayed, h *commitHeader, i, n int) (
	gitlet.EngineWriteResult, bool, error) {

	b := r.b
	wr := b.wr
	id, why := replayIdentity(h, wr.sig)
	switch {
	case why != "":
		detail := fmt.Sprintf("%s's commit %s %s, so it cannot be replayed as it was written: rebase it in a "+
			"checkout of the repository ("+toolname.CodeCheckout+")", named, shortID(cm.id), why)
		return wr.changeRefused(gitlet.WriteRejectedObject, detail), true, nil
	case h.cut:
		return wr.changeRefused(gitlet.WriteTooLarge, fmt.Sprintf("%s's commit %s carries a message past the %s one "+
			"engine write carries (gitlet.MaxCommitPayloadBytes): rebase it in a checkout of the repository ("+toolname.CodeCheckout+")", named,
			shortID(cm.id), humanBytes(gitlet.MaxCommitPayloadBytes))), true, nil
	}
	m, err := b.merge(ctx, cm.parent, r.tip, cm.id)
	if err != nil {
		return gitlet.EngineWriteResult{}, true, err
	}
	if !m.clean {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := b.conflicted(ctx, m, fmt.Sprintf("%s's commit %s (%d of %d) does not apply cleanly on %s: %d "+
			"paths conflict, and the rebase stops there with nothing stored; resolve them in a checkout of the repository "+
			"("+toolname.CodeCheckout+")", named, shortID(cm.id), i+1, n, shortID(r.tip), len(m.conflicts)+m.omitted))
		return res, true, err
	}
	if m.tree == r.tree {
		// ITS CHANGE IS THE TIP'S ALREADY (held under another id, or empty):
		// dropped, as git's own rebase drops a commit that becomes empty.
		r.dropped++
		return gitlet.EngineWriteResult{}, false, nil
	}
	_, id.CommitterWhen = commitDates(wr.w.Instant(), later(r.tipDate, id.AuthorWhen))
	commit, err := b.commitOf(ctx, m.tree, []string{r.tip}, id, string(h.message))
	if err != nil {
		return gitlet.EngineWriteResult{}, true, err
	}
	r.tip, r.tree, r.tipDate = commit, m.tree, id.CommitterWhen
	r.replayed++
	return gitlet.EngineWriteResult{}, false, nil
}

// replayIdentity is the identity a replayed commit is written with — its
// author's name, address and date (with its zone) as h names them, and the
// engine's committer from sig — or why git would not write the commit back as
// it was: an author line it cannot read, a part git trims or refuses
// (gitproc.IdentityRefusal), a date before the epoch, or a message in an
// encoding other than UTF-8, which commit-tree would label UTF-8.
func replayIdentity(h *commitHeader, sig Signature) (*gitproc.Identity, string) {
	if enc := h.encoding; enc != "" && !utf8Encoding(enc) {
		return nil, fmt.Sprintf("is written in %q", enc)
	}
	name, email, when, ok := parseIdent(h.author)
	if !ok {
		return nil, "names an author git's line does not read"
	}
	for _, part := range []struct{ what, value string }{{"author name", name}, {"author address", email}} {
		if why := gitproc.IdentityRefusal(part.value); why != "" {
			return nil, fmt.Sprintf("has an %s that %s", part.what, why)
		}
	}
	if when.Unix() < 0 {
		return nil, "is authored before the epoch"
	}
	return &gitproc.Identity{AuthorName: name, AuthorEmail: email, AuthorWhen: when,
		CommitterName: sig.CommitterName, CommitterEmail: sig.CommitterEmail}, ""
}

// errBadIdent is an ident line parseIdent does not read.
var errBadIdent = errors.New("host: not an ident line")

// parseIdent reads an ident line as git writes one — `<name> <<email>>
// <seconds> <±hhmm>` — into its name, its address and its instant in its own
// zone.
func parseIdent(line string) (name, email string, when time.Time, ok bool) {
	open := strings.LastIndexByte(line, '<')
	end := strings.LastIndexByte(line, '>')
	if open < 1 || end < open || line[open-1] != ' ' {
		return "", "", time.Time{}, false
	}
	name, email = line[:open-1], line[open+1:end]
	fields := strings.Fields(line[end+1:])
	if len(fields) != 2 {
		return "", "", time.Time{}, false
	}
	secs, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return "", "", time.Time{}, false
	}
	zone, err := parseZone(fields[1])
	if err != nil {
		return "", "", time.Time{}, false
	}
	return name, email, time.Unix(secs, 0).In(zone), true
}

// parseZone is a git zone, `±hhmm`, as a fixed zone of that offset.
func parseZone(z string) (*time.Location, error) {
	if len(z) != 5 || (z[0] != '+' && z[0] != '-') {
		return nil, errBadIdent
	}
	hh, err1 := strconv.Atoi(z[1:3])
	mm, err2 := strconv.Atoi(z[3:5])
	if err1 != nil || err2 != nil || mm > 59 {
		return nil, errBadIdent
	}
	offset := hh*3600 + mm*60
	if z[0] == '-' {
		offset = -offset
	}
	return time.FixedZone("", offset), nil
}
