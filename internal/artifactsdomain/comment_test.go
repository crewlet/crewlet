package artifactsdomain_test

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/commentanchor"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
)

// summary is the anchor every thread on a commented page points at: a name the
// page declares.
var summary = commentanchor.Anchor{Kind: commentanchor.KindHTML, Page: "index.html", Name: "summary"}

// commented is an html artifact whose page declares the name `summary`.
func (n *node) commented(c artifactsdomain.Call, title string) artifacts.ID {
	n.t.Helper()
	return n.create(c, title, upload(n.t, "index.html",
		`<section data-crewlet-anchor="summary"><p>Total: 42</p></section>`)).Artifact
}

// recordAt is the record the log holds at a write's position, decoded.
func (n *node) recordAt(res statelog.Result) artifactsdomain.MutationRecord {
	n.t.Helper()
	_, payload, _, ok, err := n.log.At(n.t.Context(), res.Position.Seq)
	if err != nil || !ok {
		n.t.Fatalf("read the record at %s: %v (held %v)", res.Position, err, ok)
	}
	body, verdict := n.verifier.Open(payload)
	if verdict != recordseal.Verified {
		n.t.Fatalf("the record at %s did not verify: %s", res.Position, verdict)
	}
	rec, err := artifactsdomain.Decode(body)
	if err != nil {
		n.t.Fatalf("decode the record at %s: %v", res.Position, err)
	}
	return rec
}

// thread reads one thread back.
func (n *node) thread(id artifacts.ID, thread uuid.UUID) artifactsdomain.CommentThread {
	n.t.Helper()
	got, err := n.reader.Thread(n.t.Context(), id, thread)
	if err != nil {
		n.t.Fatalf("read thread %s of %s: %v", thread, id, err)
	}
	return got
}

// pngOf is a w by h PNG, as a dashboard's crop is drawn.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A COMMENT RIDES ITS THREAD'S SUBJECT, NEVER THE ARTIFACT'S (ADR-0153), and
// writes no artifact row, so a remark never contends with a publish.
func TestACommentRidesItsThreadsSubjectNeverTheArtifacts(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane, bob := person("jane", artifacts.RoleOwner), person("bob", artifacts.RoleCommenter)
	id := n.commented(jane, "Ride")
	before := n.head(id)

	opened, err := n.writer.OpenThread(ctx, bob, artifactsdomain.ThreadInput{
		Artifact: id, Body: "Is **42** right?", Anchor: summary, Quote: "Total: 42",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	replied, err := n.writer.Reply(ctx, jane, artifactsdomain.ReplyInput{
		Artifact: id, Thread: opened.Thread, Body: "Yes", ReplyTo: opened.Remark,
	})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	results := []statelog.Result{opened.Result, replied.Result}
	for name, write := range map[string]func() (artifactsdomain.Written, error){
		"edit": func() (artifactsdomain.Written, error) {
			return n.writer.EditRemark(ctx, jane, artifactsdomain.RemarkEdit{Body: "Yes, checked",
				RemarkRef: artifactsdomain.RemarkRef{Artifact: id, Thread: opened.Thread, Remark: replied.Remark}})
		},
		"resolve": func() (artifactsdomain.Written, error) {
			return n.writer.Resolve(ctx, bob, artifactsdomain.ResolveInput{Artifact: id, Thread: opened.Thread})
		},
	} {
		w, err := write()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		results = append(results, w.Result)
	}
	reopened, err := n.writer.Reopen(ctx, bob, artifactsdomain.ReopenInput{Artifact: id, Thread: opened.Thread})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	removed, err := n.writer.RemoveRemark(ctx, jane, artifactsdomain.RemarkRemoval{
		RemarkRef: artifactsdomain.RemarkRef{Artifact: id, Thread: opened.Thread, Remark: replied.Remark}})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	results = append(results, reopened.Result, removed.Result)

	want := artifactsdomain.CommentSubject(id, opened.Thread)
	var ops []artifactsdomain.OpKind
	for _, res := range results {
		rec := n.recordAt(res)
		ops = append(ops, rec.Op)
		if rec.Subject != want {
			t.Errorf("%s was published on %s, want the thread's own subject %s", rec.Op, rec.Subject, want)
		}
		path := rec.Scope.Resolve(rec.Subject).Paths
		if len(path) != 1 || !strings.HasPrefix(path[0], artifactsdomain.ArtifactScope(id.String()).Paths[0]+"/") {
			t.Errorf("%s is scoped %v, not beneath its artifact", rec.Op, path)
		}
		if rec.V != artifactsdomain.RecordVersion {
			t.Errorf("%s is stamped version %d, want %d: an older build must retain it", rec.Op, rec.V,
				artifactsdomain.RecordVersion)
		}
	}
	if !slices.Equal(slices.Sorted(slices.Values(ops)), slices.Sorted(slices.Values(artifactsdomain.CommentOps))) {
		t.Errorf("the gestures published %v, want every comment op once: %v", ops, artifactsdomain.CommentOps)
	}

	after := n.head(id)
	if after.Version != before.Version || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("the artifact's row moved under its comments: version %d -> %d", before.Version, after.Version)
	}
	history, err := n.reader.History(ctx, id, "", artifactsdomain.MaxListLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Entries) != 1 {
		t.Errorf("the artifact's history holds %d rows after its comments, want its create alone", len(history.Entries))
	}
	// A PUBLISH BASED ON THE HEAD BEFORE EVERY COMMENT STILL LANDS.
	next := page(upload(t, "index.html", `<section data-crewlet-anchor="summary"><p>Total: 43</p></section>`))
	next.Artifact, next.Base = id, before.HeadNumber
	if _, err := n.writer.Publish(ctx, jane, next); err != nil {
		t.Errorf("a publish based on the head the comments were made on: %v", err)
	}
	var comments int
	for _, m := range n.moved {
		if m.Artifact == id && m.What == artifactsdomain.MoveComment {
			comments++
		}
	}
	if comments != len(results) {
		t.Errorf("the applier told %d comment movements for %d comment records", comments, len(results))
	}
}

// A THREAD KEEPS THE ANCHOR AND THE VERSION IT WAS MADE ON (ADR-0153), with its
// quote and crop, through every later publish, which writes nothing to it.
func TestAThreadKeepsTheAnchorAndVersionItWasMadeOn(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane := person("jane", artifacts.RoleOwner)
	id := n.commented(jane, "Kept")
	for v := 1; v <= 2; v++ {
		next := page(upload(t, "index.html", "<p>no anchors any more</p>"))
		next.Artifact, next.Base = id, v
		if _, err := n.writer.Publish(ctx, jane, next); err != nil {
			t.Fatalf("publish %d: %v", v+1, err)
		}
	}
	crop := pngOf(t, 40, 20)
	opened, err := n.writer.OpenThread(ctx, jane, artifactsdomain.ThreadInput{
		Artifact: id, Body: "The total moved", Version: 1, Anchor: summary, Quote: "Total: 42",
		Crop: crop, CropType: "image/png", Mentions: []string{"pm", "agent-a", "pm"},
	})
	if err != nil {
		t.Fatalf("open on version 1: %v", err)
	}
	made := n.thread(id, opened.Thread)
	for v := 3; v <= 4; v++ {
		next := page(upload(t, "index.html", "<p>still none</p>"))
		next.Artifact, next.Base = id, v
		if _, err := n.writer.Publish(ctx, jane, next); err != nil {
			t.Fatalf("publish %d: %v", v+1, err)
		}
	}
	got := n.thread(id, opened.Thread)
	switch {
	case got.Version != 1:
		t.Errorf("the thread says it was made on version %d, want 1", got.Version)
	case got.Anchor != summary:
		t.Errorf("the thread's anchor is %+v, want %+v as made", got.Anchor, summary)
	case got.Quote != "Total: 42" || got.CropType != "image/png" || len(got.Crop) == 0:
		t.Errorf("the thread kept quote %q and a %d-byte %q crop", got.Quote, len(got.Crop), got.CropType)
	case got.Position != made.Position || !got.UpdatedAt.Equal(made.UpdatedAt):
		t.Errorf("a publish moved the thread's row: position %d -> %d", made.Position, got.Position)
	case got.Status != artifactsdomain.ThreadOpen || got.CreatedBy.Name != "jane":
		t.Errorf("the thread is %s by %s", got.Status, got.CreatedBy.Name)
	case len(got.Remarks) != 1 || got.Remarks[0].ID != opened.Remark ||
		!slices.Equal(got.Remarks[0].Mentions, []string{"agent-a", "pm"}):
		t.Errorf("the opening remark reads back as %+v", got.Remarks)
	}
	// THE ZERO VERSION IS THE HEAD, and the zero anchor the whole version.
	head, err := n.writer.OpenThread(ctx, jane, artifactsdomain.ThreadInput{Artifact: id, Body: "Overall"})
	if err != nil {
		t.Fatal(err)
	}
	if th := n.thread(id, head.Thread); th.Version != 5 || th.Anchor != commentanchor.Whole() {
		t.Errorf("a thread naming no version and no anchor was made on %d at %+v, want the head, 5, "+
			"and the whole version", th.Version, th.Anchor)
	}
	// A REPLAY FROM NOTHING READS THE SAME THREAD.
	replay := newNode(t, n.log, openStore(t, "replay.db"), "node-c")
	replay.drain()
	if again := replay.thread(id, opened.Thread); again.Anchor != got.Anchor || again.Version != 1 ||
		!bytes.Equal(again.Crop, got.Crop) || again.Position != got.Position {
		t.Errorf("a replay reads the thread as %+v", again)
	}
	for name, in := range map[string]artifactsdomain.ThreadInput{
		"a version the artifact never had": {Artifact: id, Body: "?", Version: 9},
		"an anchor its kind does not take": {Artifact: id, Body: "?", Anchor: commentanchor.Anchor{
			Kind: commentanchor.KindSlides, Slide: "s1"}},
	} {
		if _, err := n.writer.OpenThread(ctx, jane, in); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	_, err = n.writer.OpenThread(ctx, jane, artifactsdomain.ThreadInput{Artifact: id, Body: "?", Anchor: commentanchor.Anchor{
		Kind: commentanchor.KindDesign, Board: "a.html"}})
	if !errors.Is(err, artifactsdomain.ErrInvalid) || !errors.Is(err, commentanchor.ErrInvalid) {
		t.Errorf("a design anchor on a page answered %v, want both packages' refusal", err)
	}
}

// ONLY ITS AUTHOR CHANGES A REMARK, and a removal by anybody else is one the
// API marked moderated: an administrator's.
func TestOnlyItsAuthorChangesARemark(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane, bob := person("jane", artifacts.RoleOwner), person("bob", artifacts.RoleCommenter)
	id := n.commented(jane, "Authors")
	opened, err := n.writer.OpenThread(ctx, bob, artifactsdomain.ThreadInput{Artifact: id, Body: "Bob's"})
	if err != nil {
		t.Fatal(err)
	}
	ref := artifactsdomain.RemarkRef{Artifact: id, Thread: opened.Thread, Remark: opened.Remark}
	if _, err := n.writer.EditRemark(ctx, jane, artifactsdomain.RemarkEdit{RemarkRef: ref, Body: "Jane's now"}); !errors.Is(err, artifactsdomain.ErrNotTheAuthor) {
		t.Errorf("an edit of somebody else's remark answered %v", err)
	}
	if _, err := n.writer.RemoveRemark(ctx, jane, artifactsdomain.RemarkRemoval{RemarkRef: ref}); !errors.Is(err, artifactsdomain.ErrNotTheAuthor) {
		t.Errorf("an unmoderated removal of somebody else's remark answered %v", err)
	}
	if _, err := n.writer.EditRemark(ctx, bob, artifactsdomain.RemarkEdit{RemarkRef: ref, Body: "Bob's, edited"}); err != nil {
		t.Fatalf("the author's edit: %v", err)
	}
	if r := n.thread(id, opened.Thread).Remarks[0]; r.Body != "Bob's, edited" || r.EditedAt.IsZero() {
		t.Errorf("the edited remark reads %q, edited at %v", r.Body, r.EditedAt)
	}
	if w, err := n.writer.EditRemark(ctx, bob, artifactsdomain.RemarkEdit{RemarkRef: ref, Body: "Bob's, edited"}); err != nil || !w.Result.Position.IsZero() {
		t.Errorf("an edit that changes nothing published %v (%v)", w.Result.Position, err)
	}
	if _, err := n.writer.RemoveRemark(ctx, jane, artifactsdomain.RemarkRemoval{RemarkRef: ref, Moderated: true}); err != nil {
		t.Fatalf("a moderated removal: %v", err)
	}
	r := n.thread(id, opened.Thread).Remarks[0]
	if !r.Removed || r.Body != "" || r.Author.Name != "bob" {
		t.Errorf("the removed remark reads %+v, want its place and author with no words", r)
	}
	if _, err := n.writer.EditRemark(ctx, bob, artifactsdomain.RemarkEdit{RemarkRef: ref, Body: "Back"}); !errors.Is(err, artifactsdomain.ErrCommentNotFound) {
		t.Errorf("an edit of a removed remark answered %v", err)
	}
	if _, err := n.writer.Reply(ctx, jane, artifactsdomain.ReplyInput{Artifact: id, Thread: uuid.New(), Body: "?"}); !errors.Is(err, artifactsdomain.ErrCommentNotFound) {
		t.Errorf("a reply to no thread answered %v", err)
	}
}

// A RESOLVE NAMES THE VERSION THAT ADDRESSED THE THREAD, from the one it was
// made on to the head, and a reopen clears it.
func TestAResolveNamesAVersionFromTheThreadsToTheHead(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane, pm := person("jane", artifacts.RoleOwner), agent("agent-pm", artifacts.RoleEditor)
	id := n.commented(jane, "Resolve")
	next := page(upload(t, "index.html", "<p>2</p>"))
	next.Artifact, next.Base = id, 1
	if _, err := n.writer.Publish(ctx, jane, next); err != nil {
		t.Fatal(err)
	}
	opened, err := n.writer.OpenThread(ctx, jane, artifactsdomain.ThreadInput{Artifact: id, Body: "Fix it", Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{1, 3} {
		if _, err := n.writer.Resolve(ctx, pm, artifactsdomain.ResolveInput{Artifact: id, Thread: opened.Thread,
			Version: v}); !errors.Is(err, artifactsdomain.ErrInvalid) {
			t.Errorf("a resolve by version %d of a thread made on 2 at head 2 answered %v", v, err)
		}
	}
	if _, err := n.writer.Resolve(ctx, pm, artifactsdomain.ResolveInput{Artifact: id, Thread: opened.Thread, Version: 2,
		Notify: artifactsdomain.CommentNotify{People: []artifactsdomain.Recipient{{Seat: "jane",
			Reason: artifactsdomain.NotifyParticipant}}}}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	th := n.thread(id, opened.Thread)
	if th.Status != artifactsdomain.ThreadResolved || th.ResolvedVersion != 2 || th.ResolvedBy.Name != "agent-pm" {
		t.Errorf("the resolved thread reads %s at %d by %s", th.Status, th.ResolvedVersion, th.ResolvedBy.Name)
	}
	if _, err := n.writer.Reopen(ctx, jane, artifactsdomain.ReopenInput{Artifact: id, Thread: opened.Thread}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if th := n.thread(id, opened.Thread); th.Status != artifactsdomain.ThreadOpen || th.ResolvedVersion != 0 ||
		th.ResolvedBy.Name != "" || !th.ResolvedAt.IsZero() {
		t.Errorf("the reopened thread still reads resolved: %+v", th)
	}
}

// A COMMENT THAT CARRIES A CREDENTIAL IS REFUSED, NEVER REDACTED, and so is
// every remark the domain does not store: empty, too long, a crop that is not
// a still image of its type within its bounds.
func TestACommentTheDomainDoesNotStoreIsRefused(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane := person("jane", artifacts.RoleOwner)
	id := n.commented(jane, "Refused")
	secret := "the key is sk-ant-api03-" + strings.Repeat("x", 40)
	for name, in := range map[string]artifactsdomain.ThreadInput{
		"a credential in the body":  {Artifact: id, Body: secret},
		"a credential in the quote": {Artifact: id, Body: "?", Quote: secret},
	} {
		if _, err := n.writer.OpenThread(ctx, jane, in); !errors.Is(err, artifactsdomain.ErrCommentCredential) {
			t.Errorf("%s: answered %v", name, err)
		}
	}
	for name, in := range map[string]artifactsdomain.ThreadInput{
		"an empty body":            {Artifact: id, Body: " \n "},
		"a body past its bound":    {Artifact: id, Body: strings.Repeat("a", int(artifacts.MaxCommentBytes)+1)},
		"a crop of no type":        {Artifact: id, Body: "?", Crop: pngOf(t, 4, 4)},
		"a crop that is not a PNG": {Artifact: id, Body: "?", Crop: []byte("GIF89a"), CropType: "image/png"},
		"a crop past its side":     {Artifact: id, Body: "?", Crop: pngOf(t, artifacts.MaxCommentCropSide+1, 2), CropType: "image/png"},
		"a crop of another type":   {Artifact: id, Body: "?", Crop: []byte("GIF89a"), CropType: "image/gif"},
		"too many mentions": {Artifact: id, Body: "?", Mentions: func() []string {
			var out []string
			for i := range artifacts.MaxCommentMentions + 1 {
				out = append(out, "seat-"+string(rune('a'+i%26))+string(rune('a'+i/26)))
			}
			return out
		}()},
	} {
		if _, err := n.writer.OpenThread(ctx, jane, in); !errors.Is(err, artifactsdomain.ErrInvalid) {
			t.Errorf("%s: answered %v", name, err)
		}
	}
	threads, err := n.reader.Threads(ctx, id)
	if err != nil || len(threads) != 0 {
		t.Errorf("a refused comment left %d threads (%v)", len(threads), err)
	}
}

// A TRASHED ARTIFACT REFUSES EVERY COMMENT WRITE, and a purge takes its threads
// with it: a later write is gated on every node.
func TestATrashedArtifactTakesNoCommentAndAPurgeTakesItsThreads(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane := person("jane", artifacts.RoleOwner)
	id := n.commented(jane, "Trashed")
	opened, err := n.writer.OpenThread(ctx, jane, artifactsdomain.ThreadInput{Artifact: id, Body: "Before"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.Trash(ctx, jane, id); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.OpenThread(ctx, jane, artifactsdomain.ThreadInput{Artifact: id, Body: "After"}); !errors.Is(err, artifactsdomain.ErrTrashed) {
		t.Errorf("an open on a trashed artifact answered %v", err)
	}
	if _, err := n.writer.Reply(ctx, jane, artifactsdomain.ReplyInput{Artifact: id, Thread: opened.Thread, Body: "After"}); !errors.Is(err, artifactsdomain.ErrTrashed) {
		t.Errorf("a reply on a trashed artifact answered %v", err)
	}
	if _, err := n.writer.Resolve(ctx, jane, artifactsdomain.ResolveInput{Artifact: id, Thread: opened.Thread}); !errors.Is(err, artifactsdomain.ErrTrashed) {
		t.Errorf("a resolve on a trashed artifact answered %v", err)
	}
	admin := person("root", artifacts.RoleOwner)
	admin.Authority.Elevated = true
	if _, err := n.writer.Purge(ctx, admin, id); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, table := range []string{"artifact_comment_threads", "artifact_comments"} {
		if got := n.table(table); got != "" {
			t.Errorf("%s keeps a purged artifact's rows:\n%s", table, got)
		}
	}
	if _, err := n.writer.Reply(ctx, jane, artifactsdomain.ReplyInput{Artifact: id, Thread: opened.Thread, Body: "Gone"}); !errors.Is(err, artifactsdomain.ErrPurged) {
		t.Errorf("a reply on a purged artifact answered %v", err)
	}
}

// THE THREAD BOUND IS THE ONE THE RECORD WAS STAMPED WITH: two opens that
// raced past one snapshot are told apart by the apply, on every node alike,
// and the writer whose open the apply refused reads that back as the bound.
func TestTheThreadBoundIsTheOneTheRecordWasStampedWith(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane := person("jane", artifacts.RoleOwner)
	id := n.commented(jane, "Bound")
	for i := range 2 {
		if _, err := n.writer.OpenThread(ctx, jane, artifactsdomain.ThreadInput{Artifact: id,
			Body: strings.Repeat("x", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	over, err := artifactsdomain.OpenThreadStamped(ctx, n.writer, jane,
		artifactsdomain.ThreadInput{Artifact: id, Body: "third", Version: 1}, 2)
	if !errors.Is(err, artifactsdomain.ErrCommentLimit) {
		t.Errorf("an open past the stamped bound answered %v", err)
	}
	if _, err := n.reader.Thread(ctx, id, over.Thread); !errors.Is(err, artifactsdomain.ErrCommentNotFound) {
		t.Errorf("the refused open left its thread: %v", err)
	}
	if _, err := artifactsdomain.OpenThreadStamped(ctx, n.writer, jane,
		artifactsdomain.ThreadInput{Artifact: id, Body: "third, again", Version: 1}, 3); err != nil {
		t.Errorf("an open inside its stamped bound: %v", err)
	}
	if threads, err := n.reader.Threads(ctx, id); err != nil || len(threads) != 3 {
		t.Errorf("the artifact holds %d threads (%v), want 3", len(threads), err)
	}
}

// A RETRY UNDER ONE KEY IS ONE THREAD, whatever routing snapshot each attempt
// computed: who is told is not what the write says.
func TestARetryUnderOneKeyIsOneThread(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane := person("jane", artifacts.RoleOwner)
	jane.OpKey = statelog.NewOpID(monday, "")
	id := n.commented(person("jane", artifacts.RoleOwner), "Retry")
	in := artifactsdomain.ThreadInput{Artifact: id, Body: "Once"}
	first, err := n.writer.OpenThread(ctx, jane, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Notify = artifactsdomain.CommentNotify{Agents: []artifactsdomain.Recipient{{Seat: "agent-a",
		Reason: artifactsdomain.NotifyAssignee}}}
	again, err := n.writer.OpenThread(ctx, jane, in)
	if err != nil || again.Thread != first.Thread || again.Remark != first.Remark {
		t.Errorf("a retry opened %s (%v), want the first attempt's %s", again.Thread, err, first.Thread)
	}
	if threads, err := n.reader.Threads(ctx, id); err != nil || len(threads) != 1 {
		t.Errorf("a retry left %d threads (%v)", len(threads), err)
	}
}
