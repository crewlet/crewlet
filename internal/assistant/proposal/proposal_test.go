package proposal_test

import (
	"errors"
	"math/rand/v2"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

var noon = time.Date(2031, 4, 7, 12, 0, 0, 0, time.UTC)

// preview is a valid server write preview.
func preview() proposal.Preview {
	return proposal.Preview{
		Title: "Create task", Target: "ENG", ApproveLabel: "Approve and create",
		Changes: []wire.Change{
			proposal.Added("title", "Title", "Payroll review", false),
			proposal.Fact("project", "Project", "ENG"),
		},
		Request: exec.Request{Method: "POST", Path: "/operator/act/create_work_item",
			Body: []byte(`{"args":{"project":"ENG","title":"Payroll review"}}`)},
	}
}

// draft is a valid draft of preview.
func draft() proposal.Draft {
	return proposal.Draft{
		Owner: uuid.New(), Conversation: uuid.New(), Turn: uuid.New(),
		Tool: "create_task", Act: "create_work_item", Scope: wire.ScopeServer,
		Preview: preview(), Now: noon,
	}
}

// stored is a proposal as the domain reads it back after recording n.
func stored(d proposal.Draft, n assistantdomain.NewProposal) assistantdomain.Proposal {
	return assistantdomain.Proposal{
		ID: n.Proposal, Conversation: d.Conversation, Turn: n.Turn, Owner: d.Owner,
		Tool: n.Tool, Act: n.Act, Scope: n.Scope, Method: n.Method, Path: n.Path,
		Query: n.Query, Body: n.Body, RequestDigest: n.RequestDigest, Digest: n.Digest,
		OpKey: n.OpKey, SourceRevision: n.SourceRevision, StepUp: n.StepUp, Card: n.Card,
		State: wire.ProposalProposed, ExpiresAt: n.ExpiresAt, CreatedAt: noon,
	}
}

// THE CARD IS THE PREVIEW'S, and the proposal it is recorded with carries the
// request the preview built, a digest the card shows, and an expiry at the
// second.
func TestAProposalIsDrawnFromItsPreviewAlone(t *testing.T) {
	t.Parallel()
	d := draft()
	n, err := proposal.New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := n.Card.Validate(); err != nil {
		t.Fatalf("the card does not validate: %v", err)
	}
	card := n.Card.Proposal
	switch {
	case card.Digest != n.Digest || !wire.ValidDigest(n.Digest):
		t.Errorf("the card shows digest %q and the proposal holds %q", card.Digest, n.Digest)
	case card.State != wire.ProposalProposed:
		t.Errorf("a new proposal's card is %s", card.State)
	case !n.ExpiresAt.Equal(noon.Add(proposal.DefaultTTL)) || !card.ExpiresAt.Equal(n.ExpiresAt):
		t.Errorf("expires %v on the proposal and %v on the card", n.ExpiresAt, card.ExpiresAt)
	case card.Changes[0].Kind != wire.ChangeFact:
		t.Errorf("the first row is a %s: facts come first (D10)", card.Changes[0].Kind)
	case string(n.Body) != string(d.Preview.Request.Body) || n.Path != d.Preview.Request.Path:
		t.Errorf("the recorded request is not the preview's")
	case n.OpKey == "" || n.OpKey == n.Proposal.String():
		t.Errorf("the operation key %q is not minted apart from the proposal id", n.OpKey)
	}
	if err := proposal.Verify(stored(d, n), n.Digest); err != nil {
		t.Errorf("a fresh proposal does not verify against its own digest: %v", err)
	}
}

// THE DIGEST BINDS EVERY FIELD AN APPROVAL ACTS ON. Mutation: drop a field
// from Digest's preimage and its case passes Verify.
func TestAProposalWhoseDigestMovedIsRefused(t *testing.T) {
	t.Parallel()
	d := draft()
	n, err := proposal.New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := map[string]func(p *assistantdomain.Proposal){
		"body":            func(p *assistantdomain.Proposal) { p.Body = []byte(`{"args":{"title":"Other"}}`) },
		"path":            func(p *assistantdomain.Proposal) { p.Path = "/operator/act/update_work_item" },
		"method":          func(p *assistantdomain.Proposal) { p.Method = "PUT" },
		"query":           func(p *assistantdomain.Proposal) { p.Query = url.Values{"x": {"1"}} },
		"owner":           func(p *assistantdomain.Proposal) { p.Owner = uuid.New() },
		"conversation":    func(p *assistantdomain.Proposal) { p.Conversation = uuid.New() },
		"turn":            func(p *assistantdomain.Proposal) { p.Turn = uuid.New() },
		"id":              func(p *assistantdomain.Proposal) { p.ID = uuid.New() },
		"tool":            func(p *assistantdomain.Proposal) { p.Tool = "update_task" },
		"act":             func(p *assistantdomain.Proposal) { p.Act = "update_work_item" },
		"scope":           func(p *assistantdomain.Proposal) { p.Scope = wire.ScopePage },
		"source revision": func(p *assistantdomain.Proposal) { p.SourceRevision = "task:ENG-1@9" },
		"step up":         func(p *assistantdomain.Proposal) { p.StepUp = !p.StepUp },
		"expiry":          func(p *assistantdomain.Proposal) { p.ExpiresAt = p.ExpiresAt.Add(time.Hour) },
	}
	for name, mutate := range cases {
		p := stored(d, n)
		mutate(&p)
		if err := proposal.Verify(p, n.Digest); !errors.Is(err, proposal.ErrDigestMismatch) {
			t.Errorf("a proposal whose %s moved verifies: %v", name, err)
		}
	}
	other, err := proposal.New(draft())
	if err != nil {
		t.Fatal(err)
	}
	if err := proposal.Verify(stored(d, n), other.Digest); !errors.Is(err, proposal.ErrDigestMismatch) {
		t.Errorf("an approval echoing another proposal's digest verifies: %v", err)
	}
	if err := proposal.Verify(stored(d, n), "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Error("an approval echoing a digest nobody was shown verifies")
	}
}

// A WRITE THAT CHANGES WHO SEES SOMETHING NAMES ITS AUDIENCE (D09), and a
// preview may not mint the operation the write is sent under.
func TestAVisibilityChangeNamesItsAudience(t *testing.T) {
	t.Parallel()
	d := draft()
	d.ChangesVisibility = true
	if _, err := proposal.New(d); !errors.Is(err, proposal.ErrInvalid) {
		t.Errorf("a share with no audience was proposed: %v", err)
	}
	d.Preview.Audience = []string{"Everyone in the company"}
	d.Preview.StepUp = true
	n, err := proposal.New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := n.Card.Proposal.Audience; len(got) != 1 || !n.Card.Proposal.StepUp || !n.StepUp {
		t.Errorf("the card lost the audience row or the step-up: %v", n.Card.Proposal)
	}
	d.Preview.Request.IdempotencyKey = "x"
	if _, err := proposal.New(d); !errors.Is(err, proposal.ErrInvalid) {
		t.Errorf("a preview naming its own operation was proposed: %v", err)
	}
}

// A PAGE ACTION IS A PAGE REQUEST, and a server write is never one.
func TestAPageActionIsNeverSent(t *testing.T) {
	t.Parallel()
	d := draft()
	d.Scope = wire.ScopePage
	if _, err := proposal.New(d); !errors.Is(err, proposal.ErrInvalid) {
		t.Errorf("a page action with an HTTP request was proposed: %v", err)
	}
	d.Preview.Request = exec.Request{Method: proposal.PageMethod,
		Path: proposal.PageActionPath + "work.filters", Body: []byte(`{"status":"open"}`)}
	if _, err := proposal.New(d); err != nil {
		t.Errorf("a page action was refused: %v", err)
	}
	page, err := proposal.New(d)
	if err != nil {
		t.Fatal(err)
	}
	if ref := page.Card.Proposal.PageAction; ref == nil || ref.Name != "work.filters" ||
		string(ref.Payload) != `{"status":"open"}` {
		t.Errorf("a page proposal's card names its action as %+v", ref)
	}
	d.Scope = wire.ScopeServer
	if _, err := proposal.New(d); !errors.Is(err, proposal.ErrInvalid) {
		t.Errorf("a server write with a page request was proposed: %v", err)
	}
	if server, err := proposal.New(draft()); err != nil || server.Card.Proposal.PageAction != nil {
		t.Errorf("a server write's card names a page action: %v", err)
	}
}

// THE STATE MACHINE. Every move is the table's, a settled state has no way
// out, and unknown's one way out is a Retry. Mutation: add a move out of a
// settled state, or let unknown be approved, and this fails.
func TestEveryProposalMoveIsOneTheStateMachineAllows(t *testing.T) {
	t.Parallel()
	want := map[wire.ProposalState]map[proposal.Event]wire.ProposalState{
		wire.ProposalProposed: {
			proposal.EventApprove: wire.ProposalApproved, proposal.EventDecline: wire.ProposalDeclined,
			proposal.EventStop: wire.ProposalDeclined, proposal.EventExpire: wire.ProposalExpired,
			proposal.EventStale: wire.ProposalStale,
		},
		wire.ProposalApproved: {
			proposal.EventExecute: wire.ProposalExecuting, proposal.EventClaim: wire.ProposalApplying,
			proposal.EventUnknown: wire.ProposalUnknown,
		},
		wire.ProposalExecuting: {
			proposal.EventApplied: wire.ProposalApplied, proposal.EventRefused: wire.ProposalFailed,
			proposal.EventUnknown: wire.ProposalUnknown,
		},
		wire.ProposalUnknown: {proposal.EventRetry: wire.ProposalExecuting},
		wire.ProposalApplying: {
			proposal.EventApplied: wire.ProposalApplied, proposal.EventPageStale: wire.ProposalStale,
			proposal.EventRefused: wire.ProposalFailed,
		},
	}
	for _, from := range wire.ProposalStates {
		for _, e := range proposal.Events {
			to, err := proposal.Next(from, e)
			expected, legal := want[from][e]
			switch {
			case legal && (err != nil || to != expected):
				t.Errorf("%s from %s: got %q, %v; want %s", e, from, to, err, expected)
			case !legal && !errors.Is(err, proposal.ErrIllegalMove):
				t.Errorf("%s from %s moved to %s; the machine allows no such move", e, from, to)
			case legal && !proposal.Allowed(from, to):
				t.Errorf("Allowed(%s, %s) disagrees with Next", from, to)
			}
		}
		if from.Settled() && len(want[from]) != 0 {
			t.Errorf("settled state %s has a way out", from)
		}
	}
	if proposal.Allowed(wire.ProposalApplied, wire.ProposalExecuting) ||
		proposal.Allowed(wire.ProposalUnknown, wire.ProposalApproved) {
		t.Error("Allowed admits a move the machine has no event for")
	}
}

// THE STATES NOBODY RECORDS AS THEY BECOME TRUE are decided at read.
func TestAProposalsStateNowIsDecidedOnRead(t *testing.T) {
	t.Parallel()
	p := assistantdomain.Proposal{State: wire.ProposalProposed, ExpiresAt: noon,
		CreatedAt: noon.Add(-time.Minute)}
	if got := proposal.StateNow(p, noon.Add(-time.Second)); got != wire.ProposalProposed {
		t.Errorf("an unexpired proposal reads %s", got)
	}
	if got := proposal.StateNow(p, noon); got != wire.ProposalExpired {
		t.Errorf("a proposal at its expiry reads %s", got)
	}
	for _, state := range []wire.ProposalState{wire.ProposalApproved, wire.ProposalExecuting} {
		p := assistantdomain.Proposal{Scope: wire.ScopeServer, State: state, DecidedAt: noon}
		if got := proposal.StateNow(p, noon.Add(proposal.UnknownAfter-time.Second)); got != state {
			t.Errorf("a %s proposal still inside its grant reads %s", state, got)
		}
		if got := proposal.StateNow(p, noon.Add(proposal.UnknownAfter)); got != wire.ProposalUnknown {
			t.Errorf("a %s proposal with no settlement past UnknownAfter reads %s", state, got)
		}
	}
	page := assistantdomain.Proposal{Scope: wire.ScopePage, State: wire.ProposalApproved, DecidedAt: noon}
	if got := proposal.StateNow(page, noon.Add(time.Hour)); got != wire.ProposalApproved {
		t.Errorf("an unclaimed page action reads %s: nothing was sent, so it is never unknown", got)
	}
	if got := proposal.StateNow(assistantdomain.Proposal{State: wire.ProposalApplied},
		noon.Add(time.Hour)); got != wire.ProposalApplied {
		t.Errorf("a settled proposal reads %s", got)
	}
}

// A WORD DIFF IS EXACTLY THE CHANGE: what it keeps and removes is the old
// text, what it keeps and adds is the new one, whatever the texts are.
func TestAWordDiffJoinsToBothTexts(t *testing.T) {
	t.Parallel()
	words := []string{"the", "quarterly", "payroll", "review", "is", "due", "\n", "  "}
	rng := rand.New(rand.NewPCG(1, 2))
	text := func() string {
		var b strings.Builder
		for range rng.IntN(40) {
			b.WriteString(words[rng.IntN(len(words))])
			b.WriteString([]string{" ", "\n", "  "}[rng.IntN(3)])
		}
		return b.String()
	}
	for range 500 {
		from, to := text(), text()
		check(t, from, to, proposal.WordDiff(from, to))
	}
	long := strings.Repeat("word ", proposal.MaxDiffTokens+1)
	got := proposal.WordDiff(long, "short")
	check(t, long, "short", got)
	if len(got) != 2 {
		t.Errorf("a diff past the bound is %d runs, want the old text removed and the new added", len(got))
	}
	diff := proposal.WordDiff("Ship the report today", "Ship the final report tomorrow")
	if diff[0] != (wire.Word{Op: wire.WordSame, Text: "Ship the "}) {
		t.Errorf("the kept words are not one run: %v", diff)
	}
}

// check holds runs to joining to both texts, with no two neighbours alike.
func check(t *testing.T, from, to string, runs []wire.Word) {
	t.Helper()
	var old, now strings.Builder
	for i, w := range runs {
		if i > 0 && runs[i-1].Op == w.Op {
			t.Fatalf("two neighbouring runs are both %s: %v", w.Op, runs)
		}
		if w.Op != wire.WordAdded {
			old.WriteString(w.Text)
		}
		if w.Op != wire.WordRemoved {
			now.WriteString(w.Text)
		}
	}
	if old.String() != from || now.String() != to {
		t.Fatalf("runs %v join to %q and %q, want %q and %q", runs, old.String(),
			now.String(), from, to)
	}
}

// A ROW IS CHOSEN BY WHAT MOVED, and rows are in D10's order.
func TestAChangeRowSaysWhatMoved(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from, to string
		want     wire.ChangeKind
	}{
		{"", "Ada", wire.ChangeAdded},
		{"Ada", "", wire.ChangeRemoved},
		{"Ada", "Grace", wire.ChangeChanged},
		{"line one", "line one\nline two", wire.ChangeText},
		{strings.Repeat("a", proposal.LongTextRunes+1), "b", wire.ChangeText},
	}
	for _, c := range cases {
		got, changed := proposal.Value("f", "F", c.from, c.to)
		if !changed || got.Kind != c.want {
			t.Errorf("%q to %q is a %s row, want %s", c.from, c.to, got.Kind, c.want)
		}
	}
	if _, changed := proposal.Value("f", "F", "same", "same"); changed {
		t.Error("an unchanged value is a row")
	}
	rows := proposal.Rows([]wire.Change{
		proposal.Text("body", "Body", "a", "b"), proposal.Removed("x", "X", "1"),
		proposal.Changed("s", "Status", "open", "done"), proposal.Fact("p", "Project", "ENG"),
	})
	for i, want := range []wire.ChangeKind{wire.ChangeFact, wire.ChangeChanged,
		wire.ChangeRemoved, wire.ChangeText} {
		if rows[i].Kind != want {
			t.Errorf("row %d is a %s, want %s", i, rows[i].Kind, want)
		}
	}
}

// A CHANGE TOO LARGE FOR ITS CARD keeps its runs, and one too large even for
// that is never proposed.
func TestAChangeTooLargeToShowIsNotProposed(t *testing.T) {
	t.Parallel()
	d := draft()
	old := strings.Repeat("alpha ", 12000)
	d.Preview.Changes = []wire.Change{proposal.Text("body", "Body", old, old+"omega")}
	n, err := proposal.New(d)
	if err != nil {
		t.Fatalf("a change that fits as runs was refused: %v", err)
	}
	if c := n.Card.Proposal.Changes[0]; c.From != "" || len(c.Words) == 0 {
		t.Errorf("the oversized text change kept its whole values or lost its runs")
	}
	huge := strings.Repeat("x", assistantdomain.CardMaxBytes)
	d.Preview.Changes = []wire.Change{proposal.Added("body", "Body", huge, false)}
	if _, err := proposal.New(d); !errors.Is(err, proposal.ErrTooLarge) {
		t.Errorf("a change too large for any card was proposed: %v", err)
	}
}

// A REVISION READS BACK as it was written.
func TestARevisionReadsBackAsWritten(t *testing.T) {
	t.Parallel()
	for _, r := range []proposal.Revision{
		{Kind: proposal.RevisionTask, ID: "ENG-142", Version: "7"},
		{Kind: proposal.RevisionPerson, ID: "ada@home", Version: "3"},
	} {
		got, err := proposal.ParseRevision(r.String())
		if err != nil || got != r {
			t.Errorf("%s read back as %v, %v", r, got, err)
		}
	}
	for _, bad := range []string{"", "task", "task:ENG", "task:@1", "task:ENG@", "seat:x@1"} {
		if _, err := proposal.ParseRevision(bad); err == nil {
			t.Errorf("revision %q was read", bad)
		}
	}
}
