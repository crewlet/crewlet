package artifactsapi_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/commentanchor"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// The comment threads the rig answers, and every comment write recorded as a call.

func (w *writes) Threads(_ context.Context, id artifacts.ID) ([]artifactsdomain.CommentThread, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.threads[id]), nil
}

func (w *writes) Thread(_ context.Context, id artifacts.ID, thread uuid.UUID) (artifactsdomain.CommentThread, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, th := range w.threads[id] {
		if th.ID == thread {
			return th, nil
		}
	}
	return artifactsdomain.CommentThread{}, fmt.Errorf("%w: %s", artifactsdomain.ErrCommentNotFound, thread)
}

func (w *writes) OpenThread(_ context.Context, c artifactsdomain.Call, in artifactsdomain.ThreadInput) (
	artifactsdomain.Commented, error) {
	out, err := w.record("open_thread", c, in.Artifact, in)
	return artifactsdomain.Commented{Thread: uuid.New(), Remark: uuid.New(), Result: out.Result}, err
}

func (w *writes) Reply(_ context.Context, c artifactsdomain.Call, in artifactsdomain.ReplyInput) (
	artifactsdomain.Commented, error) {
	out, err := w.record("reply", c, in.Artifact, in)
	return artifactsdomain.Commented{Thread: in.Thread, Remark: uuid.New(), Result: out.Result}, err
}

func (w *writes) EditRemark(_ context.Context, c artifactsdomain.Call, in artifactsdomain.RemarkEdit) (
	artifactsdomain.Written, error) {
	return w.record("edit_remark", c, in.Artifact, in)
}

func (w *writes) RemoveRemark(_ context.Context, c artifactsdomain.Call, in artifactsdomain.RemarkRemoval) (
	artifactsdomain.Written, error) {
	return w.record("remove_remark", c, in.Artifact, in)
}

func (w *writes) Resolve(_ context.Context, c artifactsdomain.Call, in artifactsdomain.ResolveInput) (
	artifactsdomain.Written, error) {
	return w.record("resolve_thread", c, in.Artifact, in)
}

func (w *writes) Reopen(_ context.Context, c artifactsdomain.Call, in artifactsdomain.ReopenInput) (
	artifactsdomain.Written, error) {
	return w.record("reopen_thread", c, in.Artifact, in)
}

// hold puts a thread on artifact id, made on version 1 at anchor, by author.
func (w *writes) hold(id artifacts.ID, anchor commentanchor.Anchor, author string) artifactsdomain.CommentThread {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.threads == nil {
		w.threads = map[artifacts.ID][]artifactsdomain.CommentThread{}
	}
	by := iam.Actor{Name: author, Kind: iam.ActorHuman}
	th := artifactsdomain.CommentThread{Artifact: id, ID: uuid.New(), Version: 1, Anchor: anchor,
		Status: artifactsdomain.ThreadOpen, CreatedBy: by, Crop: []byte("png"), CropType: "image/png",
		Remarks: []artifactsdomain.Remark{{ID: uuid.New(), Author: by, Body: "Look here."}}}
	w.threads[id] = append(w.threads[id], th)
	return th
}

// A comment is a commenter's, keyed, refused on a trashed artifact, and its
// routing is stamped at write. Mutation: mount the open on the read verb, or
// drop the routing from the input, and this goes red.
func TestACommentIsACommentersKeyedAndRoutedAtWrite(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	path := "/artifacts/" + page.String() + "/comments"
	body := `{"body":"Too dark.","anchor":{"kind":"html","page":"index.html","point":{"x":0.5,"y":0.2}},` +
		`"quote":"Revenue"}`
	if w := g.do(person("sam"), "POST", path, body, keyed()...); w.Code != http.StatusOK {
		t.Fatalf("an editor's comment answered %d %s", w.Code, w.Body)
	}
	calls := g.writes.all()
	if len(calls) != 1 || calls[0].verb != "open_thread" {
		t.Fatalf("the comment wrote %+v", calls)
	}
	in := calls[0].arg.(artifactsdomain.ThreadInput)
	if in.Body != "Too dark." || in.Quote != "Revenue" || in.Anchor.Kind != commentanchor.KindHTML ||
		calls[0].c.Authority.Actor.Name != "sam" {
		t.Errorf("the open was written as %+v by %+v", in, calls[0].c.Authority.Actor)
	}
	if len(in.Notify.People) != 1 || in.Notify.People[0].Seat != "jane" ||
		in.Notify.People[0].Reason != artifactsdomain.NotifyOwner || in.Notify.About == nil ||
		in.Notify.About.Title != "Page" {
		t.Errorf("the open's routing is %+v, want jane told as the owner", in.Notify)
	}
	if w := g.do(person("bob"), "POST", path, body, keyed()...); w.Code != http.StatusForbidden {
		t.Errorf("a viewer's comment answered %d %s", w.Code, w.Body)
	}
	if w := g.do(person("jane"), "POST", "/artifacts/"+binned.String()+"/comments", body,
		keyed()...); w.Code != http.StatusConflict || errorOf(t, w) != "artifact_trashed" {
		t.Errorf("a comment on a trashed artifact answered %d %s", w.Code, w.Body)
	}
	if w := g.do(person("sam"), "POST", path, body); w.Code < 400 {
		t.Errorf("a comment with no key answered %d", w.Code)
	}
	if w := g.do(person("sam"), "POST", path, `{"body":"x","crop":"not base64!"}`,
		keyed()...); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a crop that is no base64 answered %d %s", w.Code, w.Body)
	}
	if n := len(g.writes.all()); n != 1 {
		t.Errorf("refused comments wrote %d records", n)
	}
}

// A WRITER'S COMMENTS ON ONE ARTIFACT ARE HELD TO A BUDGET ON THIS NODE: past
// thirty at once the next is refused `429 rate_limited` naming when to come
// back, and nothing is written; another writer is not held to the first's.
func TestCommentsOnOneArtifactAreHeldToABudget(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	path := "/artifacts/" + page.String() + "/comments"
	for i := range 30 {
		if w := g.do(person("sam"), "POST", path, `{"body":"again"}`, keyed()...); w.Code != http.StatusOK {
			t.Fatalf("comment %d answered %d %s", i+1, w.Code, w.Body)
		}
	}
	w := g.do(person("sam"), "POST", path, `{"body":"again"}`, keyed()...)
	if w.Code != http.StatusTooManyRequests || errorOf(t, w) != "rate_limited" ||
		w.Header().Get("Retry-After") == "" || fields(t, w)["scope"] != "principal" {
		t.Fatalf("the thirty-first comment answered %d %s", w.Code, w.Body)
	}
	if n := len(g.writes.all()); n != 30 {
		t.Errorf("the budget let %d comments through, want 30", n)
	}
	if w := g.do(person("jane"), "POST", path, `{"body":"mine"}`, keyed()...); w.Code != http.StatusOK {
		t.Errorf("another writer was held to the first's budget: %d %s", w.Code, w.Body)
	}
}

// The threads are placed on the version asked: a deck's element labelled by its
// slide's number, an undeclared section outdated and counted, a crop a data URL,
// and whether the reader may comment answered with their role.
func TestTheThreadsArePlacedOnTheVersionAsked(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	g.writes.hold(deck, commentanchor.Anchor{Kind: commentanchor.KindSlides, Slide: "next", Path: "0",
		Tag: "h2", Text: "Next steps"}, "bob")
	g.writes.hold(page, commentanchor.Anchor{Kind: commentanchor.KindHTML, Page: "index.html",
		Name: "pricing"}, "sam")
	g.writes.hold(page, commentanchor.Whole(), "sam")

	got, err := g.svc.Comments(t.Context(), person("bob"), deck, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Threads) != 1 || got.Threads[0].Placement != commentanchor.StateHere ||
		got.Threads[0].Label != "Slide 2: “Next steps”" || got.CanComment || got.Role != artifacts.RoleViewer {
		t.Errorf("the deck's threads answered %+v", got)
	}
	if !strings.HasPrefix(got.Threads[0].Crop, "data:image/png;base64,") {
		t.Errorf("the crop is answered %q, want a data URL", got.Threads[0].Crop)
	}
	got, err = g.svc.Comments(t.Context(), person("sam"), page, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Counts != (artifactsapi.CommentCounts{Open: 1, Outdated: 1}) || !got.CanComment ||
		got.Version != 2 || got.Head != 2 {
		t.Errorf("the page's threads answered %+v", got)
	}
	if _, err := g.svc.Comments(t.Context(), person("jane"), binned, 0); err == nil {
		t.Error("a trashed artifact's threads were answered")
	}
}

// A REMOVAL BY AN ADMINISTRATOR IS MODERATED, and an author's is not: the
// domain takes anybody's remark from the one and only their own from the other.
func TestAnAdministratorsRemovalIsModerated(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	th := g.writes.hold(page, commentanchor.Whole(), "sam")
	path := "/artifacts/" + page.String() + "/comments/" + th.ID.String() + "/remarks/" +
		th.Remarks[0].ID.String()
	if w := g.do(person("sam"), "DELETE", path, "", keyed()...); w.Code != http.StatusOK {
		t.Fatalf("the author's removal answered %d %s", w.Code, w.Body)
	}
	if w := g.do(person("ops", iam.GrantAdmin), "DELETE", path, "", keyed()...); w.Code != http.StatusOK {
		t.Fatalf("an administrator's removal answered %d %s", w.Code, w.Body)
	}
	calls := g.writes.all()
	if len(calls) != 2 || calls[0].arg.(artifactsdomain.RemarkRemoval).Moderated ||
		!calls[1].arg.(artifactsdomain.RemarkRemoval).Moderated {
		t.Errorf("the removals were written %+v", calls)
	}
	if w := g.do(person("sam"), "POST", "/artifacts/"+page.String()+"/comments/"+th.ID.String()+
		"/resolve", `{"version":2}`, keyed()...); w.Code != http.StatusOK {
		t.Fatalf("a resolve answered %d %s", w.Code, w.Body)
	}
	if w := g.do(person("sam"), "POST", "/artifacts/"+page.String()+"/comments/"+uuid.NewString()+
		"/reopen", "", keyed()...); w.Code != http.StatusNotFound {
		t.Errorf("a reopen of a thread nobody made answered %d %s", w.Code, w.Body)
	}
}
