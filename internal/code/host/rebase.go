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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
)

// A CHANGE REBASED ONTO ITS TARGET, ON THE GIT NODE (code.WriteRebaseChange):
// the change's commits its target does not hold, replayed onto the target's
// tip in order, and its source branch moved to the last of them as ONE PUSH —
// forced, since the branch no longer holds its old head — through the one
// landing every engine write takes (write.go): its protections, its grant and
// the committer's decide hold a rebase as they hold a force-push of the same
// branch, and the push's apply records the change's new revision.
//
// # Replayed as git's own rebase replays them
//
// ONE `rev-list --topo-order --parents` of the head less the target's tip, at
// most code.MaxRebaseCommits+1 of them — more is too_many_commits, a merge
// among them merge_commit, since a merge needs a mainline to replay — then,
// for each commit C on its parent P, oldest first: ONE `merge-tree
// --write-tree -z --merge-base=P <tip> C` — the cherry-pick of C onto what is
// rebased so far — and ONE `commit-tree` of its tree onto that tip. The FIRST
// that conflicts stops the rebase, naming the commit and its conflicted paths,
// and nothing is stored; a commit whose change the tip already holds (its
// merge leaves the tip's tree as it was) is dropped, as `git rebase` drops it.
// The commits are read in batches of rebaseReadBatch, so what a rebase holds
// of their messages is bounded whatever its length.
//
// # Each commit its author's (U43 a)
//
// A replayed commit keeps its AUTHOR — the name, the address and the author
// date with its zone, as git wrote them — and its message, byte for byte; its
// COMMITTER is the engine, at U18 c's date: the instant the rebase was asked,
// or its new parent's committer date, or its own author date, whichever is
// latest, so no commit is committed before it is authored or dated before its
// parent, and a rebase asked again builds the same commits. A commit git would
// not write back as it was — an author it names in a form git trims or
// refuses, a message in another encoding than UTF-8, or one past
// code.MaxCommitPayloadBytes — is refused rather than rewritten
// (rejected_object, too_large): the rebase replays a history, it never edits
// one.

// rebaseReadBatch is how many commits a rebase reads in one `cat-file
// --batch`: SIXTEEN, a JUDGMENT — each message is held whole until its commit
// is replayed, up to code.MaxCommitPayloadBytes, so a batch holds at most 16
// MiB of messages at the bound where a whole history of code.MaxRebaseCommits
// read at once could hold a gigabyte; and one process for sixteen replayed
// commits, each of which runs two of its own, is a sixteenth of a process
// more a commit.
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
func (b *building) rebase(ctx context.Context) (string, code.EngineWriteResult, bool, error) {
	wr := b.wr
	w := wr.w
	c, err := wr.rebasedChange(ctx)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	named := code.ChangeRef(wr.repoName(), c.Number)
	target, err := wr.headOf(ctx, c.TargetRef)
	switch {
	case err != nil:
		return "", code.EngineWriteResult{}, true, err
	case target == "":
		return "", code.EngineWriteResult{}, true, fmt.Errorf("%w: change: %s's target %s does not exist, so "+
			"there is nothing to rebase it onto", code.ErrInvalid, named, c.TargetRef)
	}
	on, err := b.ancestor(ctx, target, w.ExpectedHead)
	switch {
	case err != nil:
		return "", code.EngineWriteResult{}, true, err
	case on:
		detail := fmt.Sprintf("%s's head %s holds %s's tip %s already: there is nothing to rebase", named,
			shortID(w.ExpectedHead), c.TargetRef, shortID(target))
		return "", wr.changeRefused(code.WriteNothingChanged, detail), true, nil
	}
	commits, res, done, err := b.toReplay(ctx, named, target)
	if done || err != nil {
		return "", res, true, err
	}
	base := &commitHeader{}
	if err := b.readCommits(ctx, []string{target}, []*commitHeader{base}); err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	r := &rebasing{b: b, tip: target, tree: base.tree, tipDate: base.committed}
	for start := 0; start < len(commits); start += rebaseReadBatch {
		batch := commits[start:min(start+rebaseReadBatch, len(commits))]
		headers := make([]*commitHeader, len(batch))
		ids := make([]string, len(batch))
		for i, cm := range batch {
			headers[i], ids[i] = &commitHeader{keep: code.MaxCommitPayloadBytes}, cm.id
		}
		if err := b.readCommits(ctx, ids, headers); err != nil {
			return "", code.EngineWriteResult{}, true, err
		}
		for i, cm := range batch {
			if res, done, err := r.replay(ctx, named, cm, headers[i], start+i, len(commits)); done || err != nil {
				return "", res, true, err
			}
		}
	}
	if r.replayed == 0 {
		return "", wr.changeRefused(code.WriteNothingChanged, fmt.Sprintf("every commit of %s is in %s already: "+
			"rebased, it would hold nothing of its own", named, c.TargetRef)), true, nil
	}
	return r.tip, code.EngineWriteResult{}, false, nil
}

// rebasedChange is the change a rebase names, read at the copy's position as
// its caller: live, and from the branch the write moves.
func (wr *writing) rebasedChange(ctx context.Context) (*code.ChangeDetail, error) {
	w, repo := wr.w, wr.synced.Repo
	ans, err := wr.s.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadChange, Asker: w.Principal, Repo: repo.Name,
		Change: w.Change, Fresh: statelog.FloorAt(statelog.ReadSession, wr.synced.Position)})
	if err != nil {
		return nil, err
	}
	c := ans.Change
	named := code.ChangeRef(repo.Name, w.Change)
	switch {
	case c == nil:
		return nil, fmt.Errorf("host: the read of %s answered no change", named)
	case !c.State.Live():
		return nil, fmt.Errorf("%w: change: %s is %s, and only a live change is rebased", code.ErrInvalid, named,
			c.State)
	case c.SourceRef != w.Ref:
		return nil, fmt.Errorf("%w: ref: %s proposes %s, not %s: a rebase moves its change's source", code.ErrInvalid,
			named, c.SourceRef, w.Ref)
	}
	return c, nil
}

// replayed is one commit a rebase replays: its id and its one parent.
type replayed struct{ id, parent string }

// toReplay is the commits the head holds and target does not, oldest first —
// ONE `rev-list --topo-order --parents` in the copy, at most one past
// code.MaxRebaseCommits — or the refusal of a history too long, or holding a
// merge.
func (b *building) toReplay(ctx context.Context, named, target string) ([]replayed, code.EngineWriteResult,
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
		return nil, code.EngineWriteResult{}, true, err
	}
	res, err := wr.s.runGit(ctx, gitproc.Cmd{
		Dir: wr.synced.Dir, Config: wr.s.o.Cache.ServeConfig(),
		Args: []string{"rev-list", "--topo-order", "--parents", "--max-count=" +
			strconv.Itoa(code.MaxRebaseCommits+1), wr.w.ExpectedHead, "^" + target},
		Stdout: lines, Class: gitproc.ClassRead, Deadline: left,
	})
	if err == nil {
		err = lines.Close()
	}
	if err := wr.failed(ctx, "listing of the commits to rebase", res, err); err != nil {
		return nil, code.EngineWriteResult{}, true, err
	}
	switch n := len(out); {
	case merge != "":
		return nil, wr.changeRefused(code.WriteMergeCommit, fmt.Sprintf("%s's commit %s merges others: a "+
			"rebase replays each commit onto the one before it, and a merge needs a mainline to replay — rebase "+
			"it in a checkout of the repository ("+toolname.CodeCheckout+")", named, shortID(merge))), true, nil
	case n > code.MaxRebaseCommits:
		return nil, wr.changeRefused(code.WriteTooManyCommits, fmt.Sprintf("%s holds more than %d commits its "+
			"target does not (code.MaxRebaseCommits): rebase it in a checkout of the repository ("+toolname.CodeCheckout+")", named,
			code.MaxRebaseCommits)), true, nil
	}
	slices.Reverse(out)
	return out, code.EngineWriteResult{}, false, nil
}

// replay is commit cm — the i-th of n, its header h — cherry-picked onto the
// rebase's tip: ONE merge-tree of it against its parent, and ONE commit-tree
// keeping its author and its message, committed by the engine — or the
// rebase stopped at its conflict, or refused for a commit git would not write
// back as it was.
func (r *rebasing) replay(ctx context.Context, named string, cm replayed, h *commitHeader, i, n int) (
	code.EngineWriteResult, bool, error) {

	b := r.b
	wr := b.wr
	id, why := replayIdentity(h, wr.sig)
	switch {
	case why != "":
		detail := fmt.Sprintf("%s's commit %s %s, so it cannot be replayed as it was written: rebase it in a "+
			"checkout of the repository ("+toolname.CodeCheckout+")", named, shortID(cm.id), why)
		return wr.changeRefused(code.WriteRejectedObject, detail), true, nil
	case h.cut:
		return wr.changeRefused(code.WriteTooLarge, fmt.Sprintf("%s's commit %s carries a message past the %s one "+
			"engine write carries (code.MaxCommitPayloadBytes): rebase it in a checkout of the repository ("+toolname.CodeCheckout+")", named,
			shortID(cm.id), humanBytes(code.MaxCommitPayloadBytes))), true, nil
	}
	m, err := b.merge(ctx, cm.parent, r.tip, cm.id)
	if err != nil {
		return code.EngineWriteResult{}, true, err
	}
	if !m.clean {
		res, err := b.conflicted(ctx, m, fmt.Sprintf("%s's commit %s (%d of %d) does not apply cleanly on %s: %d "+
			"paths conflict, and the rebase stops there with nothing stored; resolve them in a checkout of the repository "+
			"("+toolname.CodeCheckout+")", named, shortID(cm.id), i+1, n, shortID(r.tip), len(m.conflicts)+m.omitted))
		return res, true, err
	}
	if m.tree == r.tree {
		// ITS CHANGE IS THE TIP'S ALREADY — a commit the target holds under
		// another id, or one that changed nothing: dropped, as git's own
		// rebase drops a commit that becomes empty.
		r.dropped++
		return code.EngineWriteResult{}, false, nil
	}
	_, id.CommitterWhen = commitDates(wr.w.Instant(), later(r.tipDate, id.AuthorWhen))
	commit, err := b.commitOf(ctx, m.tree, []string{r.tip}, id, string(h.message))
	if err != nil {
		return code.EngineWriteResult{}, true, err
	}
	r.tip, r.tree, r.tipDate = commit, m.tree, id.CommitterWhen
	r.replayed++
	return code.EngineWriteResult{}, false, nil
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
