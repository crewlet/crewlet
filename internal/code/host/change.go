package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A PUSH'S CHANGE, AS ITS RECEIVER HANDLES IT: the options git handed the hook
// read once (askChange), the change made ready for the push's proposal — its
// source judged again, its title filled in, its room in the push's record
// measured (readyChange) — and what became of it told the pusher in one line
// on the hook's stderr (changeLine). None of it ever refuses the push, which
// lands or not by its commands alone (internal/code's pushchange.go). A push
// the handler refused whole before receive-pack ran never reaches the relay
// that reads its options, and is told its refusal alone: nothing of it landed.

// changeAsk is what a push's options asked of a change, and what became of it.
type changeAsk struct {
	// asked: the options asked for a change (`-o change`), whatever then
	// became of it.
	asked bool
	// option is the change asked for, and source the index of the push's one
	// branch command it is opened from — nil and -1 where the options asked
	// for none, or its receiver answered it already.
	option *code.ChangeOption
	source int
	// answer is what became of it: the receiver's own refusal, made before
	// anything was proposed, and otherwise its committer's answer once the
	// push is decided (code.PushAnswer.Change) — nil where neither said.
	answer *code.ChangeAnswer
}

// askChange reads the change a push's options ask for, against the commands
// git handed the hook: nothing asked where they ask for none; the receiver's
// own refusal where they are no change's (bad_option) or the push sets no
// branch or several (code.ChangeSource); and otherwise the change, which the
// landing proposes once the push is to be proposed (readyChange).
func askChange(options []string, cmds []code.Command) changeAsk {
	opt, err := code.ParseChangeOptions(options)
	switch {
	case err != nil:
		return changeAsk{asked: true, source: -1,
			answer: &code.ChangeAnswer{Refusal: code.ChangeRefusalBadOption, Detail: err.Error()}}
	case opt == nil:
		return changeAsk{source: -1}
	}
	i, r, why := code.ChangeSource(cmds)
	if r != "" {
		return changeAsk{asked: true, source: -1, answer: &code.ChangeAnswer{Refusal: r, Detail: why}}
	}
	return changeAsk{asked: true, option: opt, source: i}
}

// askedChange is the change a landing opens: the one its builder formed
// (landing.Change, a revert_change's), or else what its push's options ask
// for (askChange) — one ask either way, made ready and answered by the same
// steps, so an engine write's change is opened in its push's record exactly
// as a person's `-o change` is.
func (l *landing) askedChange() changeAsk {
	if l.Change == nil {
		return askChange(l.PushOptions, l.Commands)
	}
	i, r, why := code.ChangeSource(l.Commands)
	if r != "" {
		return changeAsk{asked: true, source: -1, answer: &code.ChangeAnswer{Refusal: r, Detail: why}}
	}
	opt := *l.Change
	return changeAsk{asked: true, option: &opt, source: i}
}

// readyChange makes the change a push asks for ready for its proposal, once
// the push is to be proposed — cmds its commands as the landing has decided
// them so far, proposed the same as its proposal carries them — and answers
// it here, leaving it out, where:
//
//   - its source command was refused by now — stale, a protection, a blob:
//     no_source, since the push moves no branch for it to propose;
//   - the change is too wide for the push's record beside its commands at
//     their largest (code.Proposal.ValidateCommands): bad_option, since a
//     push of the change's branch alone always has room for it.
//
// Where the options named no title, the change takes the subject of the
// commit the push sets its source to (defaultTitle).
func (s *Server) readyChange(ctx context.Context, l *landing, ask *changeAsk, cmds,
	proposed []code.Command) *code.ChangeOption {

	if ask.option == nil || ask.answer != nil {
		return nil
	}
	source := cmds[ask.source]
	if source.Result == code.ResultNG {
		ask.answer = &code.ChangeAnswer{Refusal: code.ChangeRefusalNoSource, Detail: fmt.Sprintf("this push's "+
			"update of %s was refused (%s), so it moved no branch to open a change from", source.Ref,
			source.Reason)}
		return nil
	}
	opt := *ask.option
	if opt.Title == "" {
		opt.Title = s.defaultTitle(ctx, l, source)
	}
	err := code.Proposal{Principal: l.Principal, Via: l.Via, Node: s.o.NodeID, Commands: proposed,
		Change: &opt}.ValidateCommands()
	if err != nil {
		detail := "this push cannot carry the change it asks for: " +
			sentence(strings.TrimPrefix(err.Error(), code.ErrInvalidProposal.Error()+": "))
		var wide *code.TooWideError
		if errors.As(err, &wide) {
			detail = fmt.Sprintf("this push's ref updates leave its record no room for the change it asks for "+
				"(the two take up to %s of the %s one push may): push the change's branch alone with -o change",
				humanBytes(int64(wide.Bytes)), humanBytes(int64(wide.Room)))
		}
		ask.answer = &code.ChangeAnswer{Refusal: code.ChangeRefusalBadOption, Detail: detail}
		return nil
	}
	return &opt
}

// commitHeadBytes is the most of a commit object the title of a change is
// read from: 64 KiB — its headers (a tree, its parents at 48 bytes each, two
// identities, a signature of a few kibibytes) and the start of its message,
// which no commit a person or a coding agent writes takes past it before its
// subject ends. A commit whose subject it cuts is titled by what was read.
const commitHeadBytes = 64 << 10

// defaultTitle is the title of a change a push's options open without naming
// one: the subject of the commit the push sets its source to — what that
// commit says it does, which is what a change is first read by — read in the
// push's own scratch, whose pack or alternate holds every commit a push sets
// a ref to; and, where it cannot be read or is no title, the branch's name
// and then the commit's id, each made a title (code.DeliveredTitle) — never
// the name raw, which may hold what a title may not: a title always, since a
// push's change is never refused for its title.
func (s *Server) defaultTitle(ctx context.Context, l *landing, source code.Command) string {
	branch := strings.TrimPrefix(source.Ref, "refs/heads/")
	head := &headBytes{max: commitHeadBytes}
	if _, err := s.runGit(ctx, gitproc.Cmd{
		Dir: l.Scratch.Dir, Alternate: l.Scratch.Alternate, Args: []string{"cat-file", "commit", source.New},
		Stdout: head, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
	}); err != nil {
		s.log.Warn("code_change_title_unread", "repo", l.Synced.Repo.Name, "push_id", l.PushID,
			"commit", source.New, "error", err)
		return code.DeliveredTitle("", branch, source.New)
	}
	return code.DeliveredTitle(logSubject(head.buf), branch, source.New)
}

// logSubject is the subject of raw, a commit object as `git cat-file commit`
// prints it, whole or its head, as `git log --format=%s` shows it: its
// message's first paragraph from where the message's subject starts — past
// lines of nothing but git's white space, and with nothing from a NUL on
// (commitSubject) — each line without its trailing white space and the lines
// joined by a space (pretty.c's format_subject), or "" where raw holds no
// message. Its headers end at the first blank line: a header's continuation
// lines (a signature's) each start with a space. Its bytes are the commit's,
// never re-encoded: a subject in another encoding is no title
// (code.DeliveredTitle).
//
// NOT commitSubject's subject (message.go), which is a message's first LINE —
// the subject a revert quotes, as the sequencer finds it — since a change is
// first read by what `git log` shows of its commit: a subject wrapped over two
// lines titles it whole. It STARTS where that one does, as git's two do, so a
// message opened with blank lines (`git commit --cleanup=verbatim`, a commit
// another tool wrote) is titled by its first paragraph, as `git log` shows it,
// rather than by its branch's name, and a NUL ends it where it ends git's
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

// resolvedChange is what the change of a push answered pending came to, once
// its row was found: the change it opened, which its outcome read beside the
// row (code.PushOutcome.Opened); otherwise the decision's (tentative) — why
// it opened none, or the live change it named where the decision knew its
// number; and nil where nothing says, or the push asked for no change.
func resolvedChange(asked bool, tentative *code.ChangeAnswer, out code.PushOutcome,
	pushID string) *code.ChangeAnswer {

	switch {
	case !asked:
		return nil
	case out.Opened > 0:
		return &code.ChangeAnswer{ChangeID: code.PushChangeID(pushID), Number: out.Opened, Opened: true}
	case tentative == nil || tentative.Opened || (tentative.Refusal == "" && tentative.Number < 1):
		return nil
	}
	resolved := *tentative
	return &resolved
}

// The lines a push's change is told in, each after `crewlet: ` on the hook's
// stderr — what git prints as `remote: crewlet: …` — which the concept page's
// table holds whole (TestWhatGitPrintsNamesEveryChangeAnswer).
const (
	// changeOpenedLine and changeUpdatedLine name the change a push opened,
	// and the live one its source already headed, by its name on its
	// repository (code.ChangeRef).
	changeOpenedLine  = "change %s opened"
	changeUpdatedLine = "change %s updated"
	// changeRefusedLine is why a push opened none, its code and sentence.
	changeRefusedLine = "change not opened: %s: %s"
	// changeUnlanded is a push that did not land, and so opened nothing.
	changeUnlanded = "change not opened: nothing of this push landed"
	// changePending is a push whose outcome is not known here — pending, or
	// unknown — with which its change lands or not.
	changePending = "change not known yet: this push has not applied here, and the change it asked for " +
		"lands with it or not at all"
	// changeUnknown is a push that landed, whose answer does not say what
	// became of its change: one that had landed in an earlier record, whose
	// own answer was lost.
	changeUnknown = "change not known: this push landed, and the answer it was given does not say what " +
		"became of its change"
)

// changeLine is the one line a push that asked for a change is told of it, ""
// for one that asked for none:
//
//   - why it opened none, the receiver's or the decide's word, which holds
//     whatever becomes of the push (changeRefusedLine);
//   - for a push that landed, the change it opened or the one it named
//     (changeOpenedLine, changeUpdatedLine);
//   - for one whose outcome is not known here, that its change is decided
//     with it (changePending);
//   - for one that did not land, that nothing of it did (changeUnlanded);
//   - and for one that landed with no word of its change, that it is not
//     known (changeUnknown).
func changeLine(c changeAsk, r pushResult, repo string) string {
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
	case a.Opened:
		return fmt.Sprintf(changeOpenedLine, code.ChangeRef(repo, a.Number))
	}
	return fmt.Sprintf(changeUpdatedLine, code.ChangeRef(repo, a.Number))
}
