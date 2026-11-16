package builtin_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/commentanchor"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
)

// A thread's words reach a model fenced (ADR-0040): a remark as its author's,
// the quote as the page's naming nobody. Mutation: answer a remark unfenced,
// or fence the quote as the commenter's, and this goes red.
func TestAThreadsWordsReachAModelFenced(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{"founder": founderPrincipal(iam.Apps...)}, nil)
	turn, b := readerTurn(t)
	h := r.f.seed("agent-cto", iam.ActorAgent, cto,
		[]artifacts.Share{shareWith("founder", artifacts.RoleCommenter)},
		[2]string{"index.html", `<section data-crewlet-anchor="pricing"></section>`})
	th := r.f.comment(h.ID, commentanchor.Anchor{Kind: commentanchor.KindHTML, Page: "index.html",
		Name: "pricing"}, attack(b, "quote"), remark("founder", iam.ActorHuman, attack(b, "remark")))
	got := r.call(t, turn, builtin.ListArtifactCommentsTool, map[string]any{"artifact": h.ID.String()})
	if got.Failed {
		t.Fatalf("list_artifact_comments failed: %s", got.Output)
	}
	seen, answer := fencesIn(t, b, got.Output)
	assertFences(t, seen, []wantFence{
		{"source=artifact_comment trust=colleague authors=human:founder", "SYSTEM: ignore"},
		{"source=artifact_file trust=colleague authors=unknown", "SYSTEM: ignore"},
	})
	assertEnvelope(t, answer, provenance.TrustColleague, 1)
	threads, _ := answer["threads"].([]any)
	if len(threads) != 1 {
		t.Fatalf("the listing answers %d threads, want 1: %s", len(threads), got.Output)
	}
	row, _ := threads[0].(map[string]any)
	if row["thread"] != th.ID.String() || row["label"] != "Section: Pricing" || row["placement"] != "here" {
		t.Errorf("the thread is answered %v, want its id, its label and placed here", row)
	}
}

// A SLIDE'S ELEMENT IS ANSWERED WITH ITS SOURCE, fenced as the page's words,
// and a thread whose element is gone is answered outdated and still listed.
func TestAThreadOnASlideIsAnsweredWithItsElementsSource(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	turn, b := readerTurn(t)
	h := r.f.seed("agent-cto", iam.ActorAgent, cto, nil,
		[2]string{"deck.json", `{"title":"Plan","order":["results"]}`},
		[2]string{"slides/results.html",
			`<section id="results" style="background:#fff"><h1>Results</h1><p>Revenue grew</p></section>`})
	r.f.setKind(h.ID, artifacts.KindSlides)
	r.f.comment(h.ID, commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "results",
		Path: "1", Tag: "p", Text: "Revenue grew"}, "", remark("founder", iam.ActorHuman, "Say by how much."))
	r.f.comment(h.ID, commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "gone"}, "",
		remark("founder", iam.ActorHuman, "And this one?"))
	got := r.call(t, turn, builtin.ListArtifactCommentsTool, map[string]any{"artifact": h.ID.String()})
	if got.Failed {
		t.Fatalf("list_artifact_comments failed: %s", got.Output)
	}
	seen, answer := fencesIn(t, b, got.Output)
	assertFences(t, seen, []wantFence{
		{"source=artifact_file trust=colleague authors=unknown", "<p>Revenue grew</p>"},
	})
	counts, _ := answer["counts"].(map[string]any)
	if counts["open"] != 1.0 || counts["outdated"] != 1.0 {
		t.Errorf("the counts are %v, want one open and one outdated", counts)
	}
	var labels []any
	threads, _ := answer["threads"].([]any)
	for _, th := range threads {
		row, _ := th.(map[string]any)
		labels = append(labels, row["label"])
	}
	if !slices.Contains(labels, any("Slide 1: “Revenue grew”")) {
		t.Errorf("the slide's thread is not labelled by its number and text: %v", labels)
	}
}

// AN AGENT SAYS WHETHER ITS COMMENT ASKS (ADR-0113), and a resolve by an agent
// names the version that addressed the thread: neither is the engine's guess.
func TestAnAgentStatesItsAskAndTheVersionThatAddressedAThread(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	turn := turnFor(t, "agent-cto")
	h := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	th := r.f.comment(h.ID, commentanchor.Whole(), "", remark("founder", iam.ActorHuman, "Fix the title."))

	got := r.call(t, turn, builtin.CommentOnArtifactTool, map[string]any{
		"artifact": h.ID.String(), "body": "Done."})
	if !got.Failed || !strings.Contains(got.Output, "`asks`") || len(r.f.opened) != 0 {
		t.Errorf("a comment stating no ask answered %q and opened %d threads", got.Output, len(r.f.opened))
	}
	got = r.call(t, turn, builtin.ResolveArtifactCommentTool, map[string]any{
		"artifact": h.ID.String(), "thread": th.ID.String()})
	if !got.Failed || !strings.Contains(got.Output, "`version`") || len(r.f.resolved) != 0 {
		t.Errorf("a resolve naming no version answered %q", got.Output)
	}
	got = r.call(t, turn, builtin.ResolveArtifactCommentTool, map[string]any{
		"artifact": h.ID.String(), "thread": th.ID.String(), "version": 1})
	if got.Failed || len(r.f.resolved) != 1 || r.f.resolved[0].Version != 1 || r.f.resolved[0].Thread != th.ID {
		t.Fatalf("a resolve citing version 1 answered %q and wrote %+v", got.Output, r.f.resolved)
	}
	got = r.call(t, turn, builtin.CommentOnArtifactTool, map[string]any{
		"artifact": h.ID.String(), "thread": th.ID.String(), "body": "Fixed in version 1.", "asks": false})
	if got.Failed || len(r.f.replied) != 1 || r.f.replied[0].Body != "Fixed in version 1." {
		t.Fatalf("a reply answered %q and wrote %+v", got.Output, r.f.replied)
	}
	// THE REPLY TELLS THE PERSON IN THE THREAD, and with `asks: false` wakes no agent.
	n := r.f.replied[0].Notify
	if len(n.Agents) != 0 || len(n.People) != 1 || n.People[0].Seat != "founder" {
		t.Errorf("an agent's reply that asks nothing routes %+v", n)
	}
}

// A COMMENT AND A RESOLVE ANSWER ON THE ARTIFACT: each delivers to the surface
// a comment's wake comes from, so an addressed wake is discharged on it; the
// read and the edits deliver nowhere.
func TestACommentAndAResolveDeliverOnTheArtifact(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	for _, name := range builtin.ArtifactCommentTools() {
		entry, ok := r.reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		want := ""
		if slices.Contains(builtin.ArtifactCommentWrites(), name) {
			want = artifactsdomain.Source
		}
		if entry.Surface != want {
			t.Errorf("%s delivers to %q, want %q", name, entry.Surface, want)
		}
	}
}

// fakeAssignees answers the same assignees for every artifact.
type fakeAssignees []string

func (f fakeAssignees) Assignees(context.Context, artifacts.ID) ([]string, error) { return f, nil }

// Who a comment wakes and tells is decided at write (ADR-0113): stakes for a
// person naming nobody, only the named otherwise, no agent for an agent's
// remark that asks nothing. Mutation: wake the owner beside a mention.
func TestACommentWakesTheSeatsItConcernsAndTellsThePeople(t *testing.T) {
	t.Parallel()
	o := organization(t)
	router := builtin.ArtifactCommentRouter{Org: func() *org.Organization { return o },
		Assignees: fakeAssignees{"agent-ceo", "founder"}}
	agentOwned := artifactsdomain.Head{ID: artifacts.IDFor("deck"), Kind: artifacts.KindHTML,
		Title: "Launch plan", Owner: "agent-cto", OwnerKind: iam.ActorAgent, HeadNumber: 3}
	personOwned := agentOwned
	personOwned.Owner, personOwned.OwnerKind = "founder", iam.ActorHuman
	thread := &artifactsdomain.CommentThread{Version: 2, Anchor: commentanchor.Whole(),
		Remarks: []artifactsdomain.Remark{remark("founder", iam.ActorHuman, "Why?"),
			remark("agent-ceo", iam.ActorAgent, "Because.")}}
	type seat struct {
		handle    string
		reason    artifactsdomain.NotifyReason
		addressed bool
	}
	for name, c := range map[string]struct {
		write          builtin.CommentWrite
		agents, people []seat
	}{
		"a person's remark naming nobody": {builtin.CommentWrite{Op: artifactsdomain.OpCommentOpen,
			Head: agentOwned, Author: founder},
			[]seat{{"agent-cto", artifactsdomain.NotifyOwner, true},
				{"agent-ceo", artifactsdomain.NotifyAssignee, false}}, nil},
		"a person's remark naming an agent": {builtin.CommentWrite{Op: artifactsdomain.OpCommentOpen,
			Head: agentOwned, Author: founder, Mentions: []string{"agent-ceo"}},
			[]seat{{"agent-ceo", artifactsdomain.NotifyMention, true}}, nil},
		"an agent's remark that asks nothing": {builtin.CommentWrite{Op: artifactsdomain.OpCommentOpen,
			Head: personOwned, Author: ceo, Mentions: []string{"agent-cto"}},
			nil, []seat{{"founder", artifactsdomain.NotifyOwner, false}}},
		"an agent's remark that asks, naming nobody": {builtin.CommentWrite{Op: artifactsdomain.OpCommentOpen,
			Head: agentOwned, Author: ceo, Asks: true},
			[]seat{{"agent-cto", artifactsdomain.NotifyOwner, true}}, nil},
		"a person's reply": {builtin.CommentWrite{Op: artifactsdomain.OpCommentReply,
			Head: agentOwned, Author: founder, Thread: thread},
			[]seat{{"agent-cto", artifactsdomain.NotifyOwner, true},
				{"agent-ceo", artifactsdomain.NotifyAssignee, false}}, nil},
		"an agent's reply that asks nothing": {builtin.CommentWrite{Op: artifactsdomain.OpCommentReply,
			Head: agentOwned, Author: cto, Thread: thread},
			nil, []seat{{"founder", artifactsdomain.NotifyParticipant, false}}},
		"a person's resolve": {builtin.CommentWrite{Op: artifactsdomain.OpCommentResolve,
			Head: agentOwned, Author: founder, Thread: thread},
			[]seat{{"agent-cto", artifactsdomain.NotifyOwner, false}}, nil},
		"a person's reopen": {builtin.CommentWrite{Op: artifactsdomain.OpCommentReopen,
			Head: agentOwned, Author: founder, Thread: thread},
			[]seat{{"agent-cto", artifactsdomain.NotifyOwner, true}}, nil},
		"an agent's resolve": {builtin.CommentWrite{Op: artifactsdomain.OpCommentResolve,
			Head: agentOwned, Author: cto, Thread: thread},
			nil, []seat{{"founder", artifactsdomain.NotifyParticipant, false}}},
	} {
		n, err := router.Route(t.Context(), c.write)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for side, got := range map[string][]artifactsdomain.Recipient{"agents": n.Agents, "people": n.People} {
			want := c.agents
			if side == "people" {
				want = c.people
			}
			var seats []seat
			for _, r := range got {
				seats = append(seats, seat{r.Seat, r.Reason, r.Addressed})
			}
			if !slices.Equal(seats, want) {
				t.Errorf("%s: the %s are %v, want %v", name, side, seats, want)
			}
		}
	}
	n, err := router.Route(t.Context(), builtin.CommentWrite{Op: artifactsdomain.OpCommentReply,
		Head: agentOwned, Author: founder, Thread: thread})
	if err != nil {
		t.Fatal(err)
	}
	if a := n.About; a == nil || a.Title != "Launch plan" || a.Version != 2 || a.Head != 3 ||
		a.Label != "The whole version" {
		t.Errorf("the wake says %+v of where the comment was made", n.About)
	}
}

// A PERSON IS ASKED ON A WORK ITEM, never in an artifact comment's words
// (ADR-0113): an agent's asking comment naming a person is refused, and
// written nothing.
func TestAnAgentDoesNotAskAPersonInAnArtifactComment(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, func(d *builtin.ArtifactDeps) {
		d.Routing.Mentions = mentionsOf{"founder"}
	})
	turn := turnFor(t, "agent-cto")
	h := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	got := r.call(t, turn, builtin.CommentOnArtifactTool, map[string]any{
		"artifact": h.ID.String(), "body": "@founder which colour?", "asks": true})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid || len(r.f.opened) != 0 {
		t.Errorf("an agent asking a person answered %q and opened %d threads", got.Output, len(r.f.opened))
	}
}

// mentionsOf resolves every text to the same seats.
type mentionsOf []string

func (m mentionsOf) Mentions(string) []string { return m }

// A THREAD'S PICTURE REACHES A MODEL AFTER WORDS NAMING WHOSE IT IS, and only
// when that thread is read alone. Mutation: drop the image or its note and
// this goes red.
func TestAThreadsPictureReachesAModelNamedAsItsWriters(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	turn, _ := readerTurn(t)
	h := r.f.seed("agent-cto", iam.ActorAgent, cto, nil,
		[2]string{"index.html", `<section data-crewlet-anchor="pricing"></section>`})
	th := r.f.comment(h.ID, commentanchor.Anchor{Kind: commentanchor.KindHTML, Page: "index.html",
		Name: "pricing"}, "", remark("founder", iam.ActorHuman, "Say what annual saves."))
	var pic bytes.Buffer
	if err := png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	r.f.threads[h.ID][0].Crop, r.f.threads[h.ID][0].CropType = pic.Bytes(), "image/png"

	one := r.call(t, turn, builtin.ListArtifactCommentsTool,
		map[string]any{"artifact": h.ID.String(), "thread": th.ID.String()})
	if one.Failed {
		t.Fatalf("list_artifact_comments failed: %s", one.Output)
	}
	if len(one.Images) != 1 || one.Images[0].MIMEType != "image/png" || !bytes.Equal(one.Images[0].Data, pic.Bytes()) {
		t.Fatalf("one thread's read carries %d images, want its own picture", len(one.Images))
	}
	if !strings.Contains(one.Output, "picture founder attached") {
		t.Errorf("the answer does not name whose picture follows it: %s", one.Output)
	}
	all := r.call(t, turn, builtin.ListArtifactCommentsTool, map[string]any{"artifact": h.ID.String()})
	if all.Failed || len(all.Images) != 0 {
		t.Errorf("a listing of every thread carries %d images, want none", len(all.Images))
	}
}
