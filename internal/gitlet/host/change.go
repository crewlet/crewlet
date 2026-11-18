package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// A PUSH'S CHANGE: read from the hook's options (askChange), made ready for the
// push's proposal (readyChange), titled beside its upload (titleChange), and
// told the pusher in one stderr line (changeLine). None of it refuses the push,
// which lands by its commands alone (internal/gitlet's pushchange.go). A push
// refused whole before receive-pack ran never reaches the relay that reads its
// options, so it is told its refusal alone.

// changeAsk is what a push's options asked of a change, and what became of it.
type changeAsk struct {
	// asked: the options asked for a change (`-o change`), whatever became of it.
	asked bool
	// option is the change asked for, and source the index of the push's one
	// branch command it opens from: nil and -1 where none was asked or the
	// receiver already answered it.
	option *gitlet.ChangeOption
	source int
	// answer is the receiver's own refusal, made before anything was proposed,
	// else the committer's once the push is decided (gitlet.PushAnswer.Change);
	// nil where neither said.
	answer *gitlet.ChangeAnswer
}

// askChange reads the change a push's options ask for, against the commands
// git handed the hook: nothing where they ask for none; the receiver's own
// refusal where they are no change's (bad_option) or the push sets no branch
// or several (gitlet.ChangeSource); otherwise the change (readyChange).
func askChange(options []string, cmds []gitlet.Command) changeAsk {
	opt, err := gitlet.ParseChangeOptions(options)
	switch {
	case err != nil:
		return changeAsk{asked: true, source: -1,
			answer: &gitlet.ChangeAnswer{Refusal: gitlet.ChangeRefusalBadOption, Detail: err.Error()}}
	case opt == nil:
		return changeAsk{source: -1}
	}
	i, r, why := gitlet.ChangeSource(cmds)
	if r != "" {
		return changeAsk{asked: true, source: -1, answer: &gitlet.ChangeAnswer{Refusal: r, Detail: why}}
	}
	return changeAsk{asked: true, option: opt, source: i}
}

// askedChange is the change a landing opens: its builder's (landing.Change, a
// revert_change's), else what its push's options ask for (askChange). Either
// is one ask, readied and answered by the same steps, so an engine write's
// change opens exactly as a person's `-o change` does.
func (l *landing) askedChange() changeAsk {
	if l.Change == nil {
		return askChange(l.PushOptions, l.Commands)
	}
	i, r, why := gitlet.ChangeSource(l.Commands)
	if r != "" {
		return changeAsk{asked: true, source: -1, answer: &gitlet.ChangeAnswer{Refusal: r, Detail: why}}
	}
	opt := *l.Change
	return changeAsk{asked: true, option: &opt, source: i}
}

// readyChange makes the change a push asks for ready for its proposal — cmds
// the commands as the landing has decided them so far, proposed as the
// proposal carries them — and instead answers it here, returning nil, where:
//
//   - its source command was refused: no_source, as the push moves no
//     branch for it;
//   - it is too wide for the push's record beside its commands at their
//     largest (gitlet.Proposal.ValidateCommands): bad_option, since a push of
//     the change's branch alone always has room for it.
//
// An untitled change is measured with a title at its longest and titled later
// (titleChange), so no push waits on a git process before its upload starts.
func (s *Server) readyChange(l *landing, ask *changeAsk, cmds, proposed []gitlet.Command) *gitlet.ChangeOption {
	if ask.option == nil || ask.answer != nil {
		return nil
	}
	source := cmds[ask.source]
	if source.Result == gitlet.ResultNG {
		ask.answer = &gitlet.ChangeAnswer{Refusal: gitlet.ChangeRefusalNoSource, Detail: fmt.Sprintf("this push's "+
			"update of %s was refused (%s), so it moved no branch to open a change from", source.Ref,
			source.Reason)}
		return nil
	}
	// Measured as land's proposal carries it: Principal and WrittenFor
	// included, which its committer stamps on a seat's entry.
	opt := *ask.option
	err := gitlet.Proposal{Principal: l.Principal, WrittenFor: l.WrittenFor, Via: l.Via, Node: s.o.NodeID,
		Commands: proposed, Change: &opt}.ValidateCommands()
	if err != nil {
		detail := "this push cannot carry the change it asks for: " +
			sentence(strings.TrimPrefix(err.Error(), gitlet.ErrInvalidProposal.Error()+": "))
		var wide *gitlet.TooWideError
		if errors.As(err, &wide) {
			detail = fmt.Sprintf("this push's ref updates leave its record no room for the change it asks for "+
				"(the two take up to %s of the %s one push may): push the change's branch alone with -o change",
				humanBytes(int64(wide.Bytes)), humanBytes(int64(wide.Room)))
		}
		ask.answer = &gitlet.ChangeAnswer{Refusal: gitlet.ChangeRefusalBadOption, Detail: detail}
		return nil
	}
	return &opt
}

// titleChange is the work that titles change from source (defaultTitle) where
// its options named none, nil where nothing needs filling. The landing runs
// it beside the push's upload and waits for it before the proposal is checked
// (introduceAndStore): the read refuses nothing, so the upload need not wait.
// source is a copy, so the work reads none of the landing's commands.
func (s *Server) titleChange(l *landing, change *gitlet.ChangeOption, source gitlet.Command) func(context.Context) {
	if change == nil || change.Title != "" {
		return nil
	}
	return func(ctx context.Context) { change.Title = s.defaultTitle(ctx, l, source) }
}

// commitHeadBytes is the most of a commit object a change's title is read
// from: 64 KiB holds its headers (a tree, 48-byte parents, two identities, a
// signature of a few KiB) and any subject a person or a harness writes. A
// subject it cuts is titled by what was read.
const commitHeadBytes = 64 << 10

// defaultTitle titles a change whose options named none: the subject of the
// commit its source is set to, read in the push's scratch (whose pack or
// alternate holds every commit a push sets a ref to); else the branch's name,
// then the commit's id. Each is made a title (gitlet.DeliveredTitle), never
// used raw, since a branch name may hold what a title may not and a push's
// change is never refused for its title.
func (s *Server) defaultTitle(ctx context.Context, l *landing, source gitlet.Command) string {
	branch := strings.TrimPrefix(source.Ref, "refs/heads/")
	head := &headBytes{max: commitHeadBytes}
	if _, err := s.runGit(ctx, gitproc.Cmd{
		Dir: l.Scratch.Dir, Alternate: l.Scratch.Alternate, Args: []string{"cat-file", "commit", source.New},
		Stdout: head, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
	}); err != nil {
		// A read ended with its landing (introduceAndStore's upload or
		// listing failed) says nothing of the title: the push fails, and
		// is logged, for what ended it.
		if ctx.Err() == nil {
			s.log.Warn("code_change_title_unread", "repo", l.Synced.Repo.Name, "push_id", l.PushID,
				"commit", source.New, "error", err)
		}
		return gitlet.DeliveredTitle("", branch, source.New)
	}
	return gitlet.DeliveredTitle(logSubject(head.buf), branch, source.New)
}

// logSubject is the subject `git log --format=%s` shows of raw, a commit
// object (whole or its head) as `git cat-file commit` prints it: the message's
// first paragraph from where commitSubject starts it (past white-space lines,
// nothing from a NUL on), each line right-trimmed and joined by a space
// (pretty.c's format_subject); "" for no message. Headers end at the first
// blank line, as a continuation line starts with a space. Bytes are never
// re-encoded: a subject in another encoding is no title (gitlet.DeliveredTitle).
// Not commitSubject's first LINE, which a revert quotes: a change is first read
// by what `git log` shows, so a wrapped subject titles it whole
// (TestAChangesDefaultTitleIsGitLogsSubject).
func logSubject(raw []byte) string {
	_, msg, ok := bytes.Cut(raw, []byte("\n\n"))
	if !ok {
		return ""
	}
	_, from := commitSubject(string(msg))
	var b strings.Builder
	for line := range strings.Lines(from) {
		line = trimGitSpace(line)
		if line == "" {
			break
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(line)
	}
	return b.String()
}

// headBytes keeps the first max bytes written to it and drops the rest,
// refusing no write: git writes all it has and ends as it would, and only what
// is kept is bounded.
type headBytes struct {
	buf []byte
	max int
}

func (h *headBytes) Write(p []byte) (int, error) {
	if room := h.max - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// resolvedChange is what the change of a push answered pending came to once
// its row was found: the change it opened (gitlet.PushOutcome.Opened); else
// the decision's tentative answer, why it opened none or the live change it
// named; nil where nothing says or no change was asked for.
func resolvedChange(asked bool, tentative *gitlet.ChangeAnswer, out gitlet.PushOutcome,
	pushID string) *gitlet.ChangeAnswer {

	switch {
	case !asked:
		return nil
	case out.Opened > 0:
		return &gitlet.ChangeAnswer{ChangeID: gitlet.PushChangeID(pushID), Number: out.Opened, Opened: true}
	case tentative == nil || tentative.Opened || (tentative.Refusal == "" && tentative.Number < 1):
		return nil
	}
	resolved := *tentative
	return &resolved
}

// The lines a push's change is told in, after `crewlet: ` on the hook's stderr
// (git prints `remote: crewlet: …`); the concept page's table holds them all
// (TestWhatGitPrintsNamesEveryChangeAnswer).
const (
	// changeOpenedLine and changeUpdatedLine name the change a push opened, or
	// the live one its source headed (gitlet.ChangeRef), and link its page
	// (changeLink), so the pusher opens it from what git printed.
	changeOpenedLine  = "change %s opened: %s"
	changeUpdatedLine = "change %s updated: %s"
	// changeOpenedBare and changeUpdatedBare are those lines from a host told
	// no base (Options.ExternalBase): no link that could not be opened. No
	// engine node serving a push is one, as pushes arrive through the API
	// listener, which requires `api.external_url`.
	changeOpenedBare  = "change %s opened"
	changeUpdatedBare = "change %s updated"
	// changeRefusedLine is why a push opened none, its code and sentence.
	changeRefusedLine = "change not opened: %s: %s"
	// changeUnlanded is a push that did not land, and so opened nothing.
	changeUnlanded = "change not opened: nothing of this push landed"
	// changePending is a push whose outcome is not known here (pending or
	// unknown); its change lands with it or not.
	changePending = "change not known yet: this push has not applied here, and the change it asked for " +
		"lands with it or not at all"
	// changeUnknown is a push that landed in an earlier record and lost its
	// own answer, so nothing says what became of its change.
	changeUnknown = "change not known: this push landed, and the answer it was given does not say what " +
		"became of its change"
)

// changeLine is the one line a push that asked for a change is told of it (""
// for none), base (Options.ExternalBase) where its page is linked:
//
//   - a refusal, the receiver's or the decide's, whatever becomes of the push
//     (changeRefusedLine);
//   - landed: the change opened or named (changeOpenedLine, changeUpdatedLine);
//   - outcome not known here: decided with the push (changePending);
//   - not landed: nothing of it did (changeUnlanded);
//   - landed with no word of its change: not known (changeUnknown).
func changeLine(c changeAsk, r pushResult, repo, base string) string {
	if !c.asked {
		return ""
	}
	a := c.answer
	switch {
	case a != nil && a.Refusal != "":
		return fmt.Sprintf(changeRefusedLine, a.Refusal, sentence(a.Detail))
	case r.outcome == types.AuditPending, r.outcome == types.AuditUnknown:
		return changePending
	case r.outcome != types.AuditApplied || r.position.IsZero():
		return changeUnlanded
	case a == nil || a.ChangeID == "" || a.Number < 1:
		return changeUnknown
	}
	name, link := gitlet.ChangeRef(repo, a.Number), changeLink(base, repo, a.Number)
	switch {
	case link == "" && a.Opened:
		return fmt.Sprintf(changeOpenedBare, name)
	case link == "":
		return fmt.Sprintf(changeUpdatedBare, name)
	case a.Opened:
		return fmt.Sprintf(changeOpenedLine, name, link)
	}
	return fmt.Sprintf(changeUpdatedLine, name, link)
}

// changeLink is the address of change number of repo on base
// (`<api.external_url>/dashboard#/gitlet/repos/web/changes/42`), "" for no
// base: the ONE place the link git prints is composed, from the dashboard's
// shell (auth.PathDashboard) and the change's route (gitlet.ChangeAddress,
// which the dashboard's links suite holds to its router).
func changeLink(base, repo string, number int64) string {
	if base == "" {
		return ""
	}
	return base + auth.PathDashboard + gitlet.ChangeAddress(repo, number)
}
