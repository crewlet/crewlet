package artifactsdomain_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A LISTING PAGE HOLDS ITS ROWS, SHARES AND ALL, TO THE BYTES ITS READ
// CARRIES, short of its limit where they run out, and says so with a token
// that resumes at the first row it left out: paging to the end lists every
// artifact exactly once.
//
// Mutations, each red: drop the Take (a page holds all five); leave the page
// untruncated on a cut (paging stops after the first); cut the token from the
// refused row (one is skipped).
func TestAListingPageStopsAtItsBytesAndResumesWhereItStopped(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	about := strings.Repeat("x", artifacts.MaxDescriptionBytes)
	unlisted := map[artifacts.ID]bool{}
	for i := range 5 {
		got, err := n.writer.Create(t.Context(), jane, artifactsdomain.CreateInput{
			Kind: artifacts.KindHTML, Title: fmt.Sprintf("Artifact %d", i), Description: about,
			Owner: "jane", OwnerKind: jane.Authority.Actor.Kind,
			First: page(upload(t, "index.html", fmt.Sprintf("<p>%d</p>", i))),
		})
		if err != nil {
			t.Fatalf("create artifact %d: %v", i, err)
		}
		unlisted[got.Artifact] = true
	}
	admin := artifactsdomain.Viewer{Admin: true}
	whole, err := n.reader.List(t.Context(), admin, artifactsdomain.ListQuery{MaxKeys: 10})
	if err != nil || len(whole.Contents) != 5 {
		t.Fatalf("the unbounded listing: %d rows (%v), want five", len(whole.Contents), err)
	}
	row := 0
	for _, item := range whole.Contents {
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("encode a row: %v", err)
		}
		row = max(row, len(raw))
	}

	// Room for two rows of nearly equal size, never three.
	ctx := statelog.WithAnswerBytes(t.Context(), row*5/2)
	token, pages := "", 0
	for {
		got, err := n.reader.List(ctx, admin, artifactsdomain.ListQuery{MaxKeys: 10, ContinuationToken: token})
		if err != nil {
			t.Fatalf("read page %d of the listing: %v", pages+1, err)
		}
		pages++
		if len(got.Contents) > 2 {
			t.Fatalf("page %d held %d rows within two and a half rows' bytes", pages, len(got.Contents))
		}
		for _, item := range got.Contents {
			if !unlisted[item.ID] {
				t.Fatalf("page %d listed %s twice, or one never created", pages, item.ID)
			}
			delete(unlisted, item.ID)
		}
		if got.IsTruncated != (got.NextContinuationToken != "") {
			t.Fatalf("page %d: truncated %v beside token %q", pages, got.IsTruncated, got.NextContinuationToken)
		}
		if got.NextContinuationToken == "" {
			break
		}
		if pages == 5 {
			t.Fatal("paging five artifacts did not end within five pages")
		}
		token = got.NextContinuationToken
	}
	if len(unlisted) != 0 {
		t.Errorf("paging to the end left %d artifacts unlisted", len(unlisted))
	}
	if pages != 3 {
		t.Errorf("read %d pages, want three: two rows, two, then one", pages)
	}
}

// AN ARTIFACT'S THREADS PAGE AT THEIR BYTES, A THREAD'S REMARKS INCLUDED: under
// a bound one row fits, every thread comes back once and every remark once, in
// order, a thread cut inside its remarks carrying on, Continued, on the next
// page, so the pages joined are the whole read. Who opened it pages alike.
//
// Mutations, each red: drop the Take check over the threads (a page holds the
// next thread's row past its bytes) or over the remarks (one page holds them
// all); resume a cut thread after its refused
// remark (a remark skipped); leave the next page's thread unmarked Continued
// (the join makes it two threads); drop the Take check in SeenByPage.
func TestAnArtifactsThreadsPageAtTheirBytesRemarksIncluded(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane, bob := person("jane", artifacts.RoleOwner), person("bob", artifacts.RoleCommenter)
	id := n.commented(jane, "Paged")
	for i := range 2 {
		opened, err := n.writer.OpenThread(ctx, bob, artifactsdomain.ThreadInput{Artifact: id,
			Body: fmt.Sprintf("thread %d", i)})
		if err != nil {
			t.Fatalf("open thread %d: %v", i, err)
		}
		for r := range 2 {
			if _, err := n.writer.Reply(ctx, jane, artifactsdomain.ReplyInput{Artifact: id,
				Thread: opened.Thread, Body: fmt.Sprintf("reply %d.%d", i, r)}); err != nil {
				t.Fatalf("reply %d.%d: %v", i, r, err)
			}
		}
	}
	whole, err := n.reader.Threads(ctx, id)
	if err != nil || len(whole) != 2 {
		t.Fatalf("the threads are %d, %v; want two", len(whole), err)
	}
	// ONE THREAD WHOLE AND HALF THE NEXT ONE'S OWN ROW: the page is the first
	// thread, its remarks all, and stops before the second.
	size := func(v any) int {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return len(raw)
	}
	base := func(th artifactsdomain.CommentThread) int {
		th.Remarks = []artifactsdomain.Remark{}
		return size(th)
	}
	first := base(whole[0])
	for _, r := range whole[0].Remarks {
		first += size(r)
	}
	page, _, err := n.reader.ThreadsPage(statelog.WithAnswerBytes(ctx, first+base(whole[1])/2), id, "")
	if err != nil || len(page) != 1 || page[0].RemarksCut || len(page[0].Remarks) != len(whole[0].Remarks) {
		t.Fatalf("a page holding one thread whole held %d threads (%v), want the first whole", len(page), err)
	}
	one := statelog.WithAnswerBytes(ctx, 1)
	var joined []artifactsdomain.CommentThread
	pages := 0
	for cursor := ""; pages < 20; {
		threads, next, err := n.reader.ThreadsPage(one, id, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		pages++
		joined = artifactsdomain.JoinThreads(joined, threads)
		if cursor = next; cursor == "" {
			break
		}
	}
	if len(joined) != len(whole) || pages < 4 {
		t.Fatalf("joined %d threads over %d pages, want %d over at least four", len(joined), pages, len(whole))
	}
	for i := range whole {
		var got, want []string
		for _, r := range joined[i].Remarks {
			got = append(got, r.Body)
		}
		for _, r := range whole[i].Remarks {
			want = append(want, r.Body)
		}
		if joined[i].ID != whole[i].ID || strings.Join(got, "|") != strings.Join(want, "|") || joined[i].RemarksCut {
			t.Errorf("thread %d joined as %s with remarks %v, want %s with %v", i, joined[i].ID, got,
				whole[i].ID, want)
		}
	}

	for _, who := range []string{"amy", "ben", "cat"} {
		if _, err := n.writer.Opened(ctx, person(who, artifacts.RoleViewer), id); err != nil {
			t.Fatalf("%s opens it: %v", who, err)
		}
	}
	seen, err := n.reader.SeenBy(ctx, id)
	if err != nil || len(seen) < 3 {
		t.Fatalf("seen by %d, %v; want at least three", len(seen), err)
	}
	var paged int
	pages = 0
	for after := ""; pages < 10; {
		reads, next, err := n.reader.SeenByPage(one, id, after)
		if err != nil {
			t.Fatal(err)
		}
		if paged, pages = paged+len(reads), pages+1; len(reads) != 1 {
			t.Errorf("a page of readers under one row's bytes held %d", len(reads))
		}
		if after = next; after == "" {
			break
		}
	}
	if paged != len(seen) {
		t.Errorf("paged %d readers, want the %d one read answers", paged, len(seen))
	}
}
