package s3fake_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
)

// A COMPLETION IS CHECKED AGAINST THE PARTS IT NAMES.
//
// The fake keeps the entity tag it answered for each part when the part
// arrived, rather than working it out again from the part's bytes when the
// upload is completed — which is what keeps a completion from paying for every
// byte of the object a second time. This holds that the check survived the
// move: a completion naming a part by a tag it was never given is refused as
// S3 refuses it, and one naming the tag the part was answered with assembles
// exactly the part's bytes.
//
// Raw requests rather than the SDK, so the case reaches the fake's own
// routing and nothing in between can quietly supply the right tag.
func TestACompletionIsCheckedAgainstThePartsItNames(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	client := srv.Client()
	do := func(method, target string, body []byte) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+target, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	started := do(http.MethodPost, "/files/k?uploads", nil)
	var initiated struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(started.Body).Decode(&initiated); err != nil || initiated.UploadID == "" {
		t.Fatalf("starting an upload answered %d, %+v, %v", started.StatusCode, initiated, err)
	}
	data := []byte("the only part of this upload")
	part := do(http.MethodPut, "/files/k?partNumber=1&uploadId="+initiated.UploadID, data)
	tag := part.Header.Get("ETag")
	if part.StatusCode != http.StatusOK || tag == "" {
		t.Fatalf("a part answered %d with tag %q", part.StatusCode, tag)
	}

	complete := func(tag string) *http.Response {
		t.Helper()
		return do(http.MethodPost, "/files/k?uploadId="+initiated.UploadID, []byte(
			"<CompleteMultipartUpload><Part><ETag>"+tag+"</ETag>"+
				"<PartNumber>1</PartNumber></Part></CompleteMultipartUpload>"))
	}
	wrong := complete(`"0-00000000"`)
	refusal, _ := io.ReadAll(wrong.Body)
	if wrong.StatusCode != http.StatusBadRequest || !strings.Contains(string(refusal), "InvalidPart") {
		t.Fatalf("a completion naming a tag the part was never given answered %d: %s; "+
			"want 400 InvalidPart", wrong.StatusCode, refusal)
	}
	if right := complete(tag); right.StatusCode != http.StatusOK {
		t.Fatalf("a completion naming the part's own tag answered %d", right.StatusCode)
	}
	got, err := io.ReadAll(do(http.MethodGet, "/files/k", nil).Body)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the completed object reads %q, %v; want exactly the part, %q", got, err, data)
	}
}

// AN UPLOAD BEGUN AND NEVER FINISHED HOLDS ITS PART UNDER THE TAG IT WAS
// ANSWERED WITH, as every part a request uploaded does — so a completion
// naming it by no tag at all is refused, as S3 refuses it, rather than
// matching a part the fake held with none.
func TestABegunUploadRefusesACompletionNamingNoTag(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	id := srv.Begin("begun")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/files/begun?uploadId="+id, strings.NewReader(
			"<CompleteMultipartUpload><Part><ETag></ETag>"+
				"<PartNumber>1</PartNumber></Part></CompleteMultipartUpload>"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	refusal, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(refusal), "InvalidPart") {
		t.Fatalf("a completion naming the begun part by no tag answered %d: %s; "+
			"want 400 InvalidPart", resp.StatusCode, refusal)
	}
	if got := srv.Uploads(); len(got) != 1 {
		t.Fatalf("the refused completion left the uploads %q, want the begun one still there", got)
	}
}

// A HELD PART IS ANSWERED TO NOBODY, AND ITS CLIENT GIVING UP IS NEWS: the
// fake takes the held part's bytes and sends nothing back for as long as its
// client waits, and closes the channel [s3fake.Server.HoldPart] answered once
// the client goes away — while a part of another number is answered as ever.
// A hold the client could wait out would let a case read an answer as the
// upload acting on a failure; a giving-up the fake never noticed would hang it.
func TestAHeldPartIsAnsweredToNobodyUntilItsClientGivesUp(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	id := srv.Begin("held")
	givenUp := srv.HoldPart(2)
	put := func(ctx context.Context, number string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut,
			srv.URL+"/files/held?partNumber="+number+"&uploadId="+id, strings.NewReader("a part"))
		if err != nil {
			t.Fatal(err)
		}
		return srv.Client().Do(req)
	}

	answered, err := put(t.Context(), "3")
	if err != nil {
		t.Fatal(err)
	}
	_ = answered.Body.Close()
	if answered.StatusCode != http.StatusOK {
		t.Fatalf("a part of a number nobody holds answered %d", answered.StatusCode)
	}

	ctx, giveUp := context.WithCancel(t.Context())
	sent := make(chan error, 1)
	go func() {
		resp, err := put(ctx, "2")
		if err == nil {
			_ = resp.Body.Close()
		}
		sent <- err
	}()
	select {
	case err := <-sent:
		t.Fatalf("the held part was answered (%v) while its client still waited", err)
	case <-givenUp:
		t.Fatal("the hold reported a client giving up before it had")
	case <-time.After(200 * time.Millisecond):
	}
	giveUp()
	if err := <-sent; !errors.Is(err, context.Canceled) {
		t.Fatalf("the client that gave up on the held part got %v, want its own cancellation", err)
	}
	select {
	case <-givenUp:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake never noticed the held part's client give up")
	}
}

// EVERY REQUEST IS ANSWERED NO SOONER THAN THE BUCKET'S LATENCY, AND COUNTED:
// the two things a benchmark of a far-away bucket reads off the fake.
func TestARequestWaitsOutTheLatencyAndIsCounted(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	const latency = 100 * time.Millisecond
	srv.Latency = latency
	before := srv.Requests()
	for _, method := range []string{http.MethodHead, http.MethodPut} {
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+"/files/k",
			strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if took := time.Since(began); took < latency {
			t.Errorf("a %s was answered after %s, sooner than the bucket's %s", method, took, latency)
		}
	}
	if got := srv.Requests() - before; got != 2 {
		t.Fatalf("two requests counted as %d", got)
	}
}

// A LISTING FOLLOWS EVERY WRITE MADE SINCE THE LAST ONE. The fake pages from
// an index it sorts once between writes, so a put or a delete that left the
// index as it was would answer for a bucket that no longer exists.
func TestAListingFollowsEveryWriteSinceTheLast(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.PageSize = 2
	do := func(method, target, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+target,
			strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	list := func() []string {
		t.Helper()
		var keys []string
		for token := ""; ; {
			var page struct {
				Truncated bool   `xml:"IsTruncated"`
				Next      string `xml:"NextContinuationToken"`
				Contents  []struct {
					Key string `xml:"Key"`
				} `xml:"Contents"`
			}
			resp := do(http.MethodGet, "/files?list-type=2&prefix=p/&continuation-token="+token, "")
			if err := xml.NewDecoder(resp.Body).Decode(&page); err != nil {
				t.Fatal(err)
			}
			for _, c := range page.Contents {
				keys = append(keys, c.Key)
			}
			if !page.Truncated {
				return keys
			}
			token = page.Next
		}
	}
	for _, k := range []string{"p/c", "p/a", "q/x", "p/b"} {
		do(http.MethodPut, "/files/"+k, k)
	}
	if got, want := list(), []string{"p/a", "p/b", "p/c"}; !slices.Equal(got, want) {
		t.Fatalf("the first listing = %q, want %q", got, want)
	}
	do(http.MethodDelete, "/files/p/b", "")
	do(http.MethodPut, "/files/p/d", "d")
	if got, want := list(), []string{"p/a", "p/c", "p/d"}; !slices.Equal(got, want) {
		t.Fatalf("the listing after a delete and a put = %q, want %q", got, want)
	}
}
