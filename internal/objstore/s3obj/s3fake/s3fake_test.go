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
	"sync"
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

// A GET ANSWERS THE OBJECT'S TAG, AND AN If-Match IS HELD TO IT — the tag a
// put or a completed upload was answered with, a range included — so a reader
// that holds its later ranges to its first one's tag is refused, 412
// PreconditionFailed, once the object under the name is another, rather than
// handed the other's bytes. The precondition is judged before the range, as
// S3 judges it: a range a shorter replacement could not satisfy is still a
// precondition that failed. An untagged bucket answers no tag at all.
func TestAGetAnswersItsTagAndHoldsAnIfMatchToIt(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	do := func(method, target, body string, header http.Header) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	put := do(http.MethodPut, "/files/k", "the first object", nil)
	tag := put.Header.Get("ETag")
	if tag == "" {
		t.Fatal("a put answered no tag")
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if got := do(method, "/files/k", "", nil).Header.Get("ETag"); got != tag {
			t.Fatalf("a %s answered the tag %q, want the put's %q", method, got, tag)
		}
	}
	held := http.Header{"If-Match": {tag}, "Range": {"bytes=4-8"}}
	if resp := do(http.MethodGet, "/files/k", "", held); resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("a range held to the object's own tag answered %d, want 206", resp.StatusCode)
	}

	do(http.MethodPut, "/files/k", "x", nil)
	stale := do(http.MethodGet, "/files/k", "", held)
	refusal, _ := io.ReadAll(stale.Body)
	if stale.StatusCode != http.StatusPreconditionFailed || !strings.Contains(string(refusal), "PreconditionFailed") {
		t.Fatalf("a range held to the replaced object's tag answered %d: %s; want 412 PreconditionFailed",
			stale.StatusCode, refusal)
	}

	started := do(http.MethodPost, "/files/parts?uploads", "", nil)
	var initiated struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(started.Body).Decode(&initiated); err != nil {
		t.Fatal(err)
	}
	part := do(http.MethodPut, "/files/parts?partNumber=1&uploadId="+initiated.UploadID, "a part", nil)
	completed := do(http.MethodPost, "/files/parts?uploadId="+initiated.UploadID,
		"<CompleteMultipartUpload><Part><ETag>"+part.Header.Get("ETag")+"</ETag>"+
			"<PartNumber>1</PartNumber></Part></CompleteMultipartUpload>", nil)
	var result struct {
		ETag string `xml:"ETag"`
	}
	if err := xml.NewDecoder(completed.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if got := do(http.MethodGet, "/files/parts", "", nil).Header.Get("ETag"); got == "" || got != result.ETag {
		t.Fatalf("an object put in parts is got with the tag %q, and its completion answered %q",
			got, result.ETag)
	}

	untagged := s3fake.Start(t, "files")
	untagged.Untagged = true
	srv = untagged
	do(http.MethodPut, "/files/k", "untagged", nil)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if got := do(method, "/files/k", "", nil).Header.Get("ETag"); got != "" {
			t.Fatalf("an untagged bucket's %s answered the tag %q", method, got)
		}
	}
}

// A BODY CROSSES AT NO MORE THAN THE CONNECTION'S CEILING, AND A GET IS IN HAND
// UNTIL ITS LAST BYTE: an object's bytes are paced to
// [s3fake.Server.ConnBytesPerSecond] from the first, the get counted in hand
// for all of it — and no longer once its client gives up, so a reader that
// stops its ranges is seen to. The hook is called with each get's first byte
// before the get is answered.
func TestABodyCrossesAtTheConnectionsCeiling(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	const rate = 256 << 10
	srv.ConnBytesPerSecond = rate
	var starts []int64
	var mu sync.Mutex
	srv.GetHook = func(key string, start int64) {
		mu.Lock()
		defer mu.Unlock()
		if key == "k" {
			starts = append(starts, start)
		}
	}
	body := make([]byte, rate/2)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/files/k", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	get := func(ctx context.Context, rng string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/files/k", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		return srv.Client().Do(req)
	}
	began := time.Now()
	whole, err := get(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(whole.Body)
	_ = whole.Body.Close()
	if err != nil || len(got) != len(body) {
		t.Fatalf("the paced get read %d bytes, %v; want %d", len(got), err, len(body))
	}
	if took, least := time.Since(began), 450*time.Millisecond; took < least {
		t.Fatalf("%d bytes at %d a second crossed in %v, sooner than %v", len(body), rate, took, least)
	}

	ctx, giveUp := context.WithCancel(t.Context())
	defer giveUp()
	ranged, err := get(ctx, "bytes=1024-")
	if err != nil {
		t.Fatal(err)
	}
	defer ranged.Body.Close()
	if _, err := io.ReadFull(ranged.Body, make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	if n := srv.GetsInHand(); n != 1 {
		t.Fatalf("a get part of the way through its body is %d in hand, want 1", n)
	}
	giveUp()
	for deadline := time.Now().Add(10 * time.Second); srv.GetsInHand() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("the fake still holds a get its client gave up on")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if most := srv.MostGetsAtOnce(); most != 1 {
		t.Fatalf("two gets one after the other were %d in hand at once, want 1", most)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(starts, []int64{0, 1024}) {
		t.Fatalf("the hook heard the gets begin at %v, want [0 1024]", starts)
	}
}

// AN OBJECT PUT FROM OUTSIDE IS ONE THE BUCKET SERVES as if a client had put it
// — the bytes it was handed, the tag a put of them is answered with, in every
// listing taken after it — because a restore fills a fresh bucket that way
// and then hands it to a node that reads it through the SDK. And the bytes are
// the bucket's own: the caller's buffer, reused for the next file, rewrites
// nothing it stored.
func TestAnObjectPutFromOutsideIsOneTheBucketServes(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	do := func(method, target string, body []byte) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+target, bytes.NewReader(body))
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
	listed := func() []string {
		t.Helper()
		var page struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
		}
		if err := xml.NewDecoder(do(http.MethodGet, "/files?list-type=2&prefix=crewlet/", nil).Body).
			Decode(&page); err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, c := range page.Contents {
			keys = append(keys, c.Key)
		}
		return keys
	}

	if got := listed(); len(got) != 0 {
		t.Fatalf("a fresh bucket lists %q", got)
	}
	buf := []byte("the bytes of one restored file")
	want := slices.Clone(buf)
	srv.Put("crewlet/files/one", buf)
	copy(buf, "THE BYTES OF THE NEXT FILE, NOW")

	got := do(http.MethodGet, "/files/crewlet/files/one", nil)
	body, err := io.ReadAll(got.Body)
	if err != nil || got.StatusCode != http.StatusOK || !bytes.Equal(body, want) {
		t.Fatalf("the object put from outside reads %d %q, %v; want %q", got.StatusCode, body, err, want)
	}
	put := do(http.MethodPut, "/files/crewlet/files/two", want)
	if tag := got.Header.Get("ETag"); tag == "" || tag != put.Header.Get("ETag") {
		t.Fatalf("the object put from outside is tagged %q and a put of its bytes %q",
			tag, put.Header.Get("ETag"))
	}
	if got, want := listed(), []string{"crewlet/files/one", "crewlet/files/two"}; !slices.Equal(got, want) {
		t.Fatalf("the listing after the puts = %q, want %q", got, want)
	}
	srv.Put("crewlet/files/zero", nil)
	if got, want := listed(), []string{"crewlet/files/one", "crewlet/files/two",
		"crewlet/files/zero"}; !slices.Equal(got, want) {
		t.Fatalf("the listing after a later put from outside = %q, want %q", got, want)
	}
}
