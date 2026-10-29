package code_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PUSH'S OPTIONS OPEN A CHANGE IN THE PUSH'S OWN RECORD: `git push -o change`
// asks for a change on the one branch the push sets, its decide opens it in
// the push's entry, and the record's apply writes it beside the refs —
// decided by the shipped committer, applied by the shipped applier on a real
// embedded broker, and read back from the rows every data node derives alike.

// pushOpening proposes one push of cmds to repo by who through c, asking for
// change, and answers what became of it. A seat's push git receives is its
// coding box's, so it names the run that made it — the turn that launched the
// run, the run's launch and its coding agent — as the engine stamps every one.
func pushOpening(t *testing.T, c *code.Committer, repo string, who code.Principal, change *code.ChangeOption,
	cmds ...code.Command) (code.Proposal, code.ProposeResult) {

	t.Helper()
	p := proposal(repo, cmds...)
	p.Principal, p.Change = who, change
	if who.Kind == code.PrincipalAgent {
		p.Principal.Run = code.RunProvenance{TurnID: uuid.Must(uuid.NewV7()).String(),
			RunID: uuid.NewString(), CodingAgent: "claude-code"}
	}
	res, err := c.Propose(t.Context(), p)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	return p, res
}

// body is change id's description as n's rows hold it, "" for none.
func (n *node) body(id string) string {
	n.t.Helper()
	var body string
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		err := tx.QueryRowContext(n.t.Context(), `SELECT body FROM code_change_bodies WHERE change_id = ?`,
			id).Scan(&body)
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}); err != nil {
		n.t.Fatalf("read the body of %s: %v", id, err)
	}
	return body
}

// A PUSH OPENS ITS CHANGE IN ITS OWN RECORD: a seat's push of its branch asking
// for a change appends ONE record, which moves the branch and opens the change
// at the head it set — numbered from the repository's next, into its default
// branch, the seat its owner and first author, the push its first revision,
// opened at the record's position with the turn and the coding run that
// pushed it — and the answer names the change by its number, read from its
// row, as does the push's outcome asked of it. The next change the repository
// opens takes the next number.
//
// Mutations: open the change in a record of its own after the push's, and the
// push appends two records and the change is opened at another position; open
// it without the push's coding run, and the change says no sandbox run opened
// it.
func TestAPushOpensItsChangeInItsOwnRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")

	before := n.end()
	p, res := pushOpening(t, c, repo, dev, &code.ChangeOption{Title: "Retry the upload",
		Body: "The upload is retried once."}, update(featureRef, oidOf("B"), oidOf("C")))
	requireOK(t, answer{res, nil})
	if appended := n.end() - before; appended != 1 {
		t.Fatalf("a push opening a change appended %d records, want ONE: the push's own", appended)
	}
	id := code.PushChangeID(p.PushID)
	if want := (&code.ChangeAnswer{ChangeID: id, Number: 1, Opened: true}); !reflect.DeepEqual(res.Answer.Change,
		want) {
		t.Fatalf("the push's answer of its change is %+v, want %+v", res.Answer.Change, want)
	}
	at := res.Position.Packed()
	got := n.change(id)
	if got.number != 1 || got.state != "open" || got.source != featureRef || got.target != mainRef ||
		got.head != oidOf("C") || got.revision != 1 || got.version != 1 || got.position != at ||
		got.owner != "dev" || got.ownerKind != "agent" || got.operator != "" ||
		got.turn != p.Principal.Run.TurnID || got.run != p.Principal.Run.RunID || got.title != "Retry the upload" ||
		got.bodyBytes != int64(len("The upload is retried once.")) {
		t.Fatalf("the change the push opened is %+v, want dev's web!1 from feature into main at C, revision 1, "+
			"version 1, at the push's record, opened by the turn and the coding run that pushed", got)
	}
	if opened := n.count(`SELECT COUNT(*) FROM code_changes WHERE id = ? AND opened_position = ?`, id,
		at); opened != 1 {
		t.Fatalf("the change was not opened at the push's own record (%d)", at)
	}
	if body := n.body(id); body != "The upload is retried once." {
		t.Fatalf("the change's description is %q", body)
	}
	wantRevisions := []storedRevision{{1, oidOf("C"), "dev", "agent", p.PushID, at}}
	if got := n.revisions(id); !slices.Equal(got, wantRevisions) {
		t.Fatalf("the change's revisions are %+v, want the push's own as its first: %+v", got, wantRevisions)
	}
	if got, want := n.authors(id), []storedAuthor{{"dev", "agent", 1, at}}; !slices.Equal(got, want) {
		t.Fatalf("the change's authors are %+v, want its pusher from revision 1: %+v", got, want)
	}

	// THE OUTCOME OF THE PUSH, asked of its change, names it — what a
	// receiver whose push was answered pending reads once it lands.
	out, err := n.reader.PushOutcome(ctx, code.OutcomeQuery{RepoID: repo, PushID: p.PushID, At: res.Position,
		Change: true})
	if err != nil || !out.Found || out.Opened != 1 {
		t.Fatalf("the push's outcome, asked of its change, is %+v, %v; want it found, opening web!1", out, err)
	}
	if out, err := n.reader.PushOutcome(ctx, code.OutcomeQuery{RepoID: repo, PushID: p.PushID,
		At: res.Position}); err != nil || out.Opened != 0 {
		t.Fatalf("the push's outcome, not asked of its change, named %d (%v)", out.Opened, err)
	}

	next, err := c.ProposeChange(ctx, opening(repo, releaseRef, mainRef, ada))
	requireChanged(t, next, err)
	if next.Number != 2 {
		t.Fatalf("the change opened after the push's is web!%d, want web!2", next.Number)
	}
}

// A PUSH TO AN OPEN HEAD NAMES ITS CHANGE: a push asking for a change from a
// branch that already heads a live change revises that one — its next
// revision, made by the push — and opens no other, whatever its options ask:
// the answer names the change it moved, by its number, its title and target
// as they were. The same push answered again, from the row its record wrote,
// opens nothing either.
//
// Mutation: open a change whatever the branch heads, and the branch heads
// two live changes.
func TestAPushToAnOpenHeadNamesItsChange(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	opened, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)

	p, res := pushOpening(t, c, repo, ada, &code.ChangeOption{Title: "Another title", Target: releaseRef},
		update(featureRef, oidOf("B"), oidOf("C")))
	requireOK(t, answer{res, nil})
	if want := (&code.ChangeAnswer{ChangeID: opened.ChangeID, Number: 1}); !reflect.DeepEqual(res.Answer.Change,
		want) {
		t.Fatalf("a push to a branch heading web!1 answered %+v of its change, want %+v", res.Answer.Change, want)
	}
	got := n.change(opened.ChangeID)
	if got.revision != 2 || got.head != oidOf("C") || got.title != "Retry the upload" || got.target != mainRef ||
		got.version != 2 {
		t.Fatalf("the change the push named is %+v, want its second revision at C, its title and target as "+
			"they were", got)
	}
	if live := n.count(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ?`, repo); live != 1 {
		t.Fatalf("the repository holds %d changes, want web!1 alone", live)
	}
	if held := n.count(`SELECT COUNT(*) FROM code_changes WHERE id = ?`, code.PushChangeID(p.PushID)); held != 0 {
		t.Fatalf("the push opened a change of its own beside web!1")
	}
}

// A CHANGE A PUSH OPENED IS NAMED WHEN THE PUSH IS ASKED AGAIN: a proposal of a
// push that already landed — forwarded twice, or decided again by another
// node after its answer was lost — is answered from its row, and the change
// it opened by the id the push derives, read from the change's row: no round
// of the second batch decided its change, and the rows say what it opened.
func TestAPushAskedAgainNamesTheChangeItOpened(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	p, first := pushOpening(t, c, repo, ada, &code.ChangeOption{Title: "Retry the upload"},
		update(featureRef, oidOf("B"), oidOf("C")))
	requireOK(t, answer{first, nil})

	before := n.end()
	again, err := c.Propose(t.Context(), p)
	if err != nil {
		t.Fatalf("Propose again: %v", err)
	}
	if !again.Answer.Collapsed || again.Position != first.Position {
		t.Fatalf("the push asked again answered %+v, want it collapsed onto its row at %s", again,
			first.Position)
	}
	if want := (&code.ChangeAnswer{ChangeID: code.PushChangeID(p.PushID), Number: 1,
		Opened: true}); !reflect.DeepEqual(again.Answer.Change, want) {
		t.Fatalf("the push asked again answered %+v of its change, want %+v", again.Answer.Change, want)
	}
	if appended := n.end() - before; appended != 0 {
		t.Fatalf("the push asked again appended %d records", appended)
	}
}

// A CHANGE THE PUSH'S RULES REFUSE LANDS THE PUSH: a target that is the source,
// no live target, a source at its target's commit and a source that did not
// move — refused, or already where the push set it — each answer the push's
// change with its code and a sentence, one of PushChangeRefusals, while the
// push lands as one that asked for nothing, and no change is opened. The
// pushes run in order on one repository, each from where the one before left
// it: main at A, feature at B.
func TestARefusedChangeLandsItsPush(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	tag := func(name string) code.Command {
		return code.Command{Ref: "refs/tags/" + name, Old: code.ZeroOID, New: oidOf(name)}
	}
	for _, tc := range []struct {
		name    string
		change  code.ChangeOption
		cmds    []code.Command
		refusal code.ChangeRefusal
		says    string
		// landed is each command's result as the push lands.
		landed map[string]code.CommandResult
	}{
		{name: "its source and target are one branch", cmds: []code.Command{update(mainRef, oidOf("A"),
			oidOf("A2"))}, refusal: code.ChangeRefusalSameRef, says: "is both the change's source and its target",
			landed: map[string]code.CommandResult{mainRef: code.ResultOK}},
		{name: "no live target", change: code.ChangeOption{Target: "refs/heads/nowhere"},
			cmds: []code.Command{update(featureRef, oidOf("B"), oidOf("C"))}, refusal: code.ChangeRefusalNoTarget,
			says: "holds no live refs/heads/nowhere", landed: map[string]code.CommandResult{featureRef: code.ResultOK}},
		{name: "its source at its target's commit", cmds: []code.Command{update(featureRef, oidOf("C"),
			oidOf("A2"))}, refusal: code.ChangeRefusalNoDifference, says: "are both at",
			landed: map[string]code.CommandResult{featureRef: code.ResultOK}},
		{name: "its source refused", cmds: []code.Command{update(featureRef, oidOf("X"), oidOf("D")), tag("v1")},
			refusal: code.ChangeRefusalNoSource, says: "was refused (stale)",
			landed: map[string]code.CommandResult{featureRef: code.ResultNG, "refs/tags/v1": code.ResultOK}},
		{name: "its source already where the push sets it", cmds: []code.Command{update(featureRef, oidOf("X"),
			oidOf("A2")), tag("v2")}, refusal: code.ChangeRefusalNoSource, says: "is already at",
			landed: map[string]code.CommandResult{featureRef: code.ResultNoop, "refs/tags/v2": code.ResultOK}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			change := tc.change
			change.Title = "A change refused"
			_, res := pushOpening(t, c, repo, ada, &change, tc.cmds...)
			if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() {
				t.Fatalf("the push answered %+v, want it applied", res)
			}
			for _, cmd := range res.Answer.Commands {
				if want := tc.landed[cmd.Ref]; cmd.Result != want {
					t.Errorf("%s landed %s (%s), want %s", cmd.Ref, cmd.Result, cmd.Reason, want)
				}
			}
			got := res.Answer.Change
			if got == nil || got.Refusal != tc.refusal || !strings.Contains(got.Detail, tc.says) ||
				got.ChangeID != "" || got.Opened {
				t.Fatalf("the push's change answered %+v, want %s saying %q", got, tc.refusal, tc.says)
			}
			if !slices.Contains(code.PushChangeRefusals(), got.Refusal) {
				t.Errorf("%s is no code PushChangeRefusals names", got.Refusal)
			}
			if held := n.count(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ?`, repo); held != 0 {
				t.Fatalf("a refused change opened %d", held)
			}
		})
	}
}

// ONE RECORD OPENS ITS CHANGES IN ITS ENTRIES' ORDER: a record whose entries
// open a change, revise it and open another applies them in order — the
// first numbered the repository's next and the second the one after, each
// at version 1 however many of the record's entries moved it, the push that
// opened it its first revision and the later push its second, the record's
// position on every row — and moves the repository's next number past both.
// An entry asking to open a change on a branch that already heads one, which
// no decide forms, applies its push and opens nothing.
//
// Mutation: open a change before its entry's own moves, and the push that
// opened it is its second revision as well as its first.
func TestOneRecordOpensItsChangesInItsEntriesOrder(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := changeRepo(t, n, "web")
	first := entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("B"), oidOf("C")))
	first.Change = &code.ChangeOption{Target: mainRef, Title: "One", Body: "the first"}
	second := entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("C"), oidOf("D")))
	third := entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/topic", code.ZeroOID, oidOf("T")))
	third.Change = &code.ChangeOption{Target: releaseRef, Title: "Two", Draft: true}
	res := n.push(repo, first, second, third)
	at := res.Position.Packed()

	one, two := code.PushChangeID(first.PushID), code.PushChangeID(third.PushID)
	if got := n.change(one); got.number != 1 || got.head != oidOf("D") || got.revision != 2 || got.version != 1 ||
		got.state != "open" || got.position != at || got.title != "One" {
		t.Fatalf("the change the record's first entry opened is %+v, want web!1 at D, revision 2, version 1", got)
	}
	if got := n.change(two); got.number != 2 || got.state != "draft" || got.target != releaseRef ||
		got.revision != 1 || got.version != 1 || got.bodyBytes != 0 {
		t.Fatalf("the change the record's third entry opened is %+v, want the draft web!2 into release", got)
	}
	want := []storedRevision{
		{1, oidOf("C"), "harness", "person", first.PushID, at},
		{2, oidOf("D"), "harness", "person", second.PushID, at},
	}
	if got := n.revisions(one); !slices.Equal(got, want) {
		t.Fatalf("web!1's revisions are %+v, want %+v", got, want)
	}
	if n.body(one) != "the first" || n.body(two) != "" {
		t.Fatalf("the descriptions are %q and %q", n.body(one), n.body(two))
	}
	if next := n.count(`SELECT next_change FROM code_repos WHERE id = ?`, repo); next != 3 {
		t.Fatalf("the repository's next change is %d, want 3", next)
	}

	// A RECORD NO DECIDE FORMS: the branch heads web!1, so the entry's
	// change is no change to open.
	fourth := entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("D"), oidOf("E")))
	fourth.Change = &code.ChangeOption{Target: mainRef, Title: "Three"}
	n.push(repo, fourth)
	if got := n.change(one); got.head != oidOf("E") || got.revision != 3 {
		t.Fatalf("the push to web!1's branch left it %+v, want its third revision at E", got)
	}
	if held := n.count(`SELECT COUNT(*) FROM code_changes WHERE id = ?`, code.PushChangeID(fourth.PushID)); held != 0 ||
		n.count(`SELECT next_change FROM code_repos WHERE id = ?`, repo) != 3 {
		t.Fatalf("an entry asking to open a change on web!1's branch opened one")
	}
}

// A PUSH'S CHANGE IS READ FROM THE CHANGE NAMESPACE: `change` asks for one, its
// four options shape it, every other option is read by nobody, the options
// shape nothing without `change`, and a key it does not read, a value its
// field refuses or a key given twice is no change — in words that name the
// option, never echoing a key a client spelled long.
func TestTheChangeOptionsAreReadFromTheChangeNamespace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		options []string
		want    *code.ChangeOption
		says    string
	}{
		{name: "none"},
		{name: "a forge's", options: []string{"ci.skip", "merge_request.create"}},
		{name: "shaping without asking", options: []string{"change.title=Retry", "change.target=dev"}},
		{name: "the change alone", options: []string{"change"}, want: &code.ChangeOption{}},
		{name: "every option", options: []string{"ci.skip", "change", "change.target=dev", "change.title=Retry it",
			"change.description=It retries once.", "change.draft"},
			want: &code.ChangeOption{Target: "refs/heads/dev", Title: "Retry it", Body: "It retries once.", Draft: true}},
		{name: "a target in full", options: []string{"change.target=refs/heads/dev", "change"},
			want: &code.ChangeOption{Target: "refs/heads/dev"}},
		{name: "an empty description", options: []string{"change", "change.description="},
			want: &code.ChangeOption{}},
		{name: "a key it does not read", options: []string{"change", "change.titel=Retry"},
			says: "change.titel is no option of a change"},
		{name: "a value for change", options: []string{"change=yes"}, says: "change takes no value"},
		{name: "a value for draft", options: []string{"change", "change.draft=true"},
			says: "change.draft takes no value"},
		{name: "a key twice", options: []string{"change", "change.title=a", "change.title=b"},
			says: "change.title is given twice"},
		{name: "an empty title", options: []string{"change", "change.title="}, says: "change.title: a change is named"},
		{name: "no title's value", options: []string{"change", "change.title"}, says: "names the change in one line"},
		{name: "a title over two lines", options: []string{"change", "change.title=a\tb"},
			says: "holds a control character"},
		{name: "an empty target", options: []string{"change", "change.target="}, says: "names the branch"},
		{name: "a target that is no branch", options: []string{"change", "change.target=a..b"},
			says: "change.target:"},
		{name: "a target past a ref", options: []string{"change", "change.target=" + strings.Repeat("x", 300)},
			says: "past the 255 a ref holds"},
		{name: "a description past its bound", options: []string{"change",
			"change.description=" + strings.Repeat("d", code.MaxChangeBodyBytes+1)}, says: "change.description:"},
		{name: "a key spelled long", options: []string{"change", "change." + strings.Repeat("k", 1<<20)},
			says: "is no option of a change"},
		{name: "a key holding a terminal's escape", options: []string{"change", "change.\x1b[2J"},
			says: `"change.\x1b[2J" is no option of a change`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := code.ParseChangeOptions(tc.options)
			switch {
			case tc.says != "" && (err == nil || !strings.Contains(err.Error(), tc.says)):
				t.Fatalf("ParseChangeOptions(%.80q) = %+v, %v; want a refusal saying %q", tc.options, got, err,
					tc.says)
			case tc.says != "" && len(err.Error()) > 512:
				t.Fatalf("the refusal is %d bytes: it echoes what the client spelled", len(err.Error()))
			case tc.says != "" && strings.ContainsFunc(err.Error(), unicode.IsControl):
				t.Fatalf("the refusal %q writes a control character to the pusher's terminal", err.Error())
			case tc.says == "" && (err != nil || !reflect.DeepEqual(got, tc.want)):
				t.Fatalf("ParseChangeOptions(%q) = %+v, %v; want %+v", tc.options, got, err, tc.want)
			}
		})
	}
}

// THE GUIDE NAMES EVERY OPTION OF A CHANGE: a person learns what a push may ask
// of a change from the guide's "Open a change from a push", so every option
// the code host reads is in its table, as a push spells it, and the table
// names no `change` option it does not read.
//
// Mutation: drop the guide's change.draft row and this fails naming it.
func TestTheGuideNamesEveryChangeOption(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "guides", "code.md"))
	if err != nil {
		t.Fatal(err)
	}
	const heading = "## Open a change from a push\n"
	page := string(raw)
	start := strings.Index(page, heading)
	if start < 0 {
		t.Fatalf("docs/guides/code.md has no %q section", strings.TrimSpace(heading))
	}
	section := page[start+len(heading):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	keys := code.ChangeOptionKeys()
	for _, key := range keys {
		if !strings.Contains(section, "`-o "+key+"`") {
			t.Errorf("the guide's table has no row for `-o %s`", key)
		}
	}
	for _, named := range regexp.MustCompile("`-o (change[^`]*)`").FindAllStringSubmatch(section, -1) {
		if !slices.Contains(keys, named[1]) {
			t.Errorf("the guide names `-o %s`, which is no option of a change the code host reads", named[1])
		}
	}
}

// A CHANGE IS OPENED FROM THE PUSH'S ONE BRANCH: the one command that sets a
// branch — a create or an update under refs/heads/ — beside any tag, and
// whatever became of it; a push of no branch, or one that only deletes
// branches, has no source, and one of several branches is ambiguous, naming
// them.
func TestAChangeIsOpenedFromThePushsOneBranch(t *testing.T) {
	t.Parallel()
	refused := update(featureRef, oidOf("B"), oidOf("C"))
	refused.Result, refused.Reason = code.ResultNG, code.RefusalStale
	for _, tc := range []struct {
		name    string
		cmds    []code.Command
		source  int
		refusal code.ChangeRefusal
		says    string
	}{
		{name: "a branch and a tag", cmds: []code.Command{update("refs/tags/v1", code.ZeroOID, oidOf("T")),
			update(featureRef, code.ZeroOID, oidOf("C"))}, source: 1},
		{name: "a branch beside a deleted one", cmds: []code.Command{update(releaseRef, oidOf("R"), code.ZeroOID),
			update(featureRef, oidOf("B"), oidOf("C"))}, source: 1},
		{name: "a refused branch", cmds: []code.Command{refused}, source: 0},
		{name: "a tag alone", cmds: []code.Command{update("refs/tags/v1", code.ZeroOID, oidOf("T"))},
			source: -1, refusal: code.ChangeRefusalNoSource, says: "this push sets no branch"},
		{name: "a deletion alone", cmds: []code.Command{update(featureRef, oidOf("B"), code.ZeroOID)},
			source: -1, refusal: code.ChangeRefusalNoSource, says: "this push sets no branch"},
		{name: "two branches", cmds: []code.Command{update(featureRef, oidOf("B"), oidOf("C")),
			update(mainRef, oidOf("A"), oidOf("C"))}, source: -1, refusal: code.ChangeRefusalAmbiguousSource,
			says: "sets 2 branches (refs/heads/feature and refs/heads/main)"},
		{name: "three branches", cmds: []code.Command{update(featureRef, oidOf("B"), oidOf("C")),
			update(mainRef, oidOf("A"), oidOf("C")), refused}, source: -1,
			refusal: code.ChangeRefusalAmbiguousSource, says: "and 1 more"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source, refusal, says := code.ChangeSource(tc.cmds)
			if source != tc.source || refusal != tc.refusal || !strings.Contains(says, tc.says) {
				t.Fatalf("ChangeSource = %d, %q, %q; want %d, %q saying %q", source, refusal, says, tc.source,
					tc.refusal, tc.says)
			}
		})
	}
}
