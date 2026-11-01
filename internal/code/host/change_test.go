package host_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PUSH'S CHANGE THROUGH THE RELAY, END TO END: a person's real git pushes with
// options to a solo fleet; receive-pack hands them to the production hook,
// whose relay reads them, and what the push's change came to is the line git
// prints as `remote: crewlet: …`.

// changeNumbered is change n of the case's repository, read at the log's head,
// and whether the repository holds one.
func changeNumbered(t *testing.T, c *codetest.Case, n int64) (*code.ChangeDetail, bool) {
	t.Helper()
	a, err := c.Fleet.Nodes()[0].Read(c.Ctx, code.ReadQuery{Asker: codetest.Reader, Kind: code.ReadChange,
		Repo: codetest.RepoName, Change: n, Fresh: codetest.Linearizable})
	if errors.Is(err, code.ErrChangeNotFound) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read %s!%d: %v", codetest.RepoName, n, err)
	}
	return a.Change, a.Change != nil
}

// remoteSaid reports whether git printed line among what the remote said.
func remoteSaid(res codetest.Pushed, line string) bool {
	return strings.Contains(res.Stderr, "remote: crewlet: "+line)
}

// THE RELAY READS PUSH OPTIONS FROM PROC-RECEIVE: a push carrying `-o change`
// and the options that shape it — beside an option of a forge's, read by
// nobody — lands, and its change is opened from what receive-pack handed the
// hook: titled, described and targeted as the options say, from the branch
// the push set, its pusher its owner; and git prints which change it opened.
//
// Mutation: answer git's version block `version=1` alone, and git hands the
// hook no option: no change opens.
func TestTheRelayReadsPushOptionsFromProcReceive(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main", "main:refs/heads/release")
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "Retry the upload")
	res := p.PushArgs(c.Ctx, work, []string{"-o", "ci.skip", "-o", "change", "-o", "change.title=Retry it once",
		"-o", "change.description=The upload is retried once.", "-o", "change.target=release"}, c.URL(0),
		"HEAD:refs/heads/feature")
	if !res.OK("refs/heads/feature") {
		t.Fatalf("a push with options did not land: %s", res)
	}
	if !remoteSaid(res, "change "+codetest.RepoName+"!1 opened") {
		t.Fatalf("git printed no change opened: %s", res)
	}
	got, ok := changeNumbered(t, c, 1)
	if !ok {
		t.Fatal("the repository holds no change the push opened")
	}
	if got.Title != "Retry it once" || got.Body != "The upload is retried once." || got.SourceRef !=
		"refs/heads/feature" || got.TargetRef != "refs/heads/release" || got.Head != head || got.Owner != "ceo" ||
		got.OwnerKind != code.PartyPerson || got.State != code.ChangeStateOpen {
		t.Fatalf("the change the push opened is %+v, want ceo's change from feature into release at %s, as its "+
			"options say", got.ChangeRow, head)
	}
}

// A CHANGE A PUSH OPENS WITHOUT A TITLE IS TITLED BY ITS HEAD COMMIT: its
// subject — the message's first paragraph, joined — what a person reads a
// change by first; and `-o change.draft` opens it a draft, into the
// repository's default branch.
func TestAChangeAPushOpensIsTitledByItsHeadCommit(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	p.Commit(work, map[string]string{"README.md": "two\n"}, "Retry the upload\nonce more\n\nThe body is no title.")
	res := p.PushArgs(c.Ctx, work, []string{"-o", "change", "-o", "change.draft"}, c.URL(0),
		"HEAD:refs/heads/feature")
	if !res.OK("refs/heads/feature") || !remoteSaid(res, "change "+codetest.RepoName+"!1 opened") {
		t.Fatalf("a push asking for a change: %s", res)
	}
	got, _ := changeNumbered(t, c, 1)
	if got == nil || got.Title != "Retry the upload once more" || got.State != code.ChangeStateDraft ||
		got.TargetRef != "refs/heads/main" {
		t.Fatalf("the change the push opened is %+v, want a draft titled by its head's subject into main", got)
	}
}

// A PUSHED CHANGE IS TITLED WHATEVER ITS BRANCH IS NAMED: a commit with no
// message titles its change by the branch's name, made a title — and a name
// git accepts may hold U+0085, a C1 control character a title refuses as a
// line break — so a push of such a commit to such a branch, asking for a
// change, lands and opens it, never refusing the push for a title nobody
// wrote.
//
// Mutation: title it by the branch's name raw (defaultTitle) — the receiver
// refuses its own proposal and nothing of the push lands, red.
func TestAPushedChangeIsTitledWhateverItsBranchIsNamed(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	p.Git().Run(t, work, "commit", "-q", "--allow-empty", "--allow-empty-message", "-m", "")
	const branch = "refs/heads/a\u0085b"
	res := p.PushArgs(c.Ctx, work, []string{"-o", "change"}, c.URL(0), "HEAD:"+branch)
	if !res.OK(branch) || !remoteSaid(res, "change "+codetest.RepoName+"!1 opened") {
		t.Fatalf("a push of a commit with no message to a branch holding U+0085, asking for a change: %s", res)
	}
	got, _ := changeNumbered(t, c, 1)
	if got == nil || got.Title != "a b" || got.SourceRef != branch {
		t.Fatalf("the change the push opened is %+v, want one from %q titled by its name, made a title", got,
			branch)
	}
}

// A PUSH ANSWERED PENDING NAMES THE CHANGE IT OPENED ONCE IT LANDS: the
// receiver waits for the push's row, and asks the push's outcome of the change
// it opened as well, which the rows say — an answer that lost the decision's
// word of it all the same. A push whose row is not found within the grace is
// told its change is not known yet, as the push itself is not.
//
// Mutation: ask the outcome nothing of the change, and the push that landed
// says what became of its change is not known.
func TestAPendingPushNamesTheChangeItOpenedOnceItLands(t *testing.T) {
	t.Run("its row is found", func(t *testing.T) {
		e := &recording{propose: func(ctx context.Context, inner host.Estate, p code.Proposal) (code.ProposeResult,
			error) {
			res, err := inner.Propose(ctx, p)
			if err != nil || res.Outcome != statelog.OutcomeApplied {
				return res, err
			}
			return code.ProposeResult{Outcome: statelog.OutcomePending, Position: res.Position, OpID: res.OpID}, nil
		}}
		c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Timing: fast, WrapEstate: e.wrap}},
			codetest.Settings())
		p := codetest.NewPerson(t, c.Fleet, "ceo")
		work := p.Init()
		p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
		c.MustPush(p, work, "main")
		p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
		res := p.PushArgs(c.Ctx, work, []string{"-o", "change"}, c.URL(0), "HEAD:refs/heads/feature")
		if !res.OK("refs/heads/feature") || !remoteSaid(res, "change "+codetest.RepoName+"!1 opened") {
			t.Fatalf("a pending push that opened a change, its row then found: %s", res)
		}
	})
	t.Run("its row is not found within the grace", func(t *testing.T) {
		e := &recording{
			propose: func(context.Context, host.Estate, code.Proposal) (code.ProposeResult, error) {
				return pendingAt(1 << 20), nil
			},
			outcome: func(context.Context, host.Estate, code.OutcomeQuery) (code.PushOutcome, error) {
				return code.PushOutcome{Position: statelog.Position{Stream: code.Domain{}.Stream().Name, Seq: 5}}, nil
			},
		}
		c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Timing: fast, WrapEstate: e.wrap}},
			codetest.Settings())
		p := codetest.NewPerson(t, c.Fleet, "ceo")
		work := p.Init()
		p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
		res := p.PushArgs(c.Ctx, work, []string{"-o", "change"}, c.URL(0), "HEAD:refs/heads/feature")
		if !remoteSaid(res, "change not known yet: ") {
			t.Fatalf("a push still pending told its change: %s", res)
		}
	})
}

// A BAD OPTION STILL LANDS THE PUSH: options that are no change's — a key the
// namespace does not hold — a push of two branches, and a change its rules
// refuse each land the push as one that asked for nothing, opening no change,
// and git prints why in the line it prints for the change.
func TestABadOptionStillLandsThePush(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	for _, tc := range []struct {
		name     string
		options  []string
		refspecs []string
		says     string
	}{
		{name: "a key it does not read", options: []string{"-o", "change", "-o", "change.titel=Retry"},
			refspecs: []string{"HEAD:refs/heads/one"}, says: "change not opened: bad_option: change.titel is no " +
				"option of a change"},
		{name: "two branches", options: []string{"-o", "change"},
			refspecs: []string{"HEAD:refs/heads/two", "HEAD:refs/heads/three"},
			says:     "change not opened: ambiguous_source: this push sets 2 branches"},
		{name: "a target it does not hold", options: []string{"-o", "change", "-o", "change.target=nowhere"},
			refspecs: []string{"HEAD:refs/heads/four"},
			says:     "change not opened: no_target: " + codetest.RepoName + " holds no live refs/heads/nowhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := p.PushArgs(c.Ctx, work, tc.options, c.URL(0), tc.refspecs...)
			if res.Err != nil {
				t.Fatalf("a push whose change was refused did not land: %s", res)
			}
			if !remoteSaid(res, tc.says) {
				t.Fatalf("git did not print %q: %s", tc.says, res)
			}
		})
	}
	if _, ok := changeNumbered(t, c, 1); ok {
		t.Fatal("a refused change was opened")
	}
}
