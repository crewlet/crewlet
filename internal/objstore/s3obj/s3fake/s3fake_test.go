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

// THE NEXT REQUESTS ARE REFUSED AS ASKED, AND THE REST ANSWERED AS EVER: what
// [s3fake.Server.FailNext] arms is a bucket that throttles for a while and
// recovers, so each of the next n requests — a HEAD, a put — is answered the
// S3 error named and changes nothing, a HEAD's with no body as S3 answers
// one, and every request after them is handled as if none had been refused.
// Each refused request is still counted, as the round trip it cost.
func TestTheNextRequestsAreRefusedAndTheRestAnswered(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	do := func(method string, body string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+"/files/k", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		defer resp.Body.Close()
		answer, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, string(answer)
	}

	srv.FailNext(2, http.StatusServiceUnavailable, "SlowDown")
	if resp, answer := do(http.MethodHead, ""); resp.StatusCode != http.StatusServiceUnavailable || answer != "" {
		t.Fatalf("the first refused request, a HEAD, answered %d %q; want 503 with no body",
			resp.StatusCode, answer)
	}
	if resp, answer := do(http.MethodPut, "refused"); resp.StatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(answer, "<Code>SlowDown</Code>") {
		t.Fatalf("the second refused request, a put, answered %d %q; want 503 SlowDown",
			resp.StatusCode, answer)
	}
	if keys := srv.Keys(); len(keys) != 0 {
		t.Fatalf("a refused put stored %q", keys)
	}
	if resp, _ := do(http.MethodPut, "stored"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the request after the refused ones answered %d, want 200", resp.StatusCode)
	}
	if resp, answer := do(http.MethodGet, ""); resp.StatusCode != http.StatusOK || answer != "stored" {
		t.Fatalf("the object put after the refusals reads %d %q, want \"stored\"", resp.StatusCode, answer)
	}
	if got := srv.Requests(); got != 4 {
		t.Fatalf("four requests, two of them refused, counted as %d", got)
	}
	if got := srv.RequestsOf("PutObject"); got != 2 {
		t.Fatalf("two puts, one of them refused, counted as %d", got)
	}
}

// A FULL BUCKET REFUSES EVERY WRITE AND SERVES THE REST, as a provider with no
// room does: once [s3fake.Server.Fill] is called, a put, a part and a
// completion are answered the S3 error named and store nothing, while a read,
// an upload's start, an abandonment and a deletion are answered as ever — the
// deletions an operator frees room with among them — and a status of zero
// makes room again.
//
// Mutation: refuse every request while full, or route a completion past the
// refusal, and this fails.
func TestAFullBucketRefusesEveryWriteAndServesTheRest(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+"/files/"+path,
			strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		answer, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(answer)
	}
	srv.Put("kept", []byte("before"))
	id := srv.Begin("big")
	if status, _ := do(http.MethodPut, "big?partNumber=1&uploadId="+id, "part one"); status != http.StatusOK {
		t.Fatalf("a part sent with room answered %d, want 200", status)
	}

	srv.Fill(http.StatusInsufficientStorage, "XMinioStorageFull")
	for _, w := range []struct{ what, method, path, body string }{
		{"a put", http.MethodPut, "new", "refused"},
		{"a part", http.MethodPut, "big?partNumber=2&uploadId=" + id, "part two"},
		{"a completion", http.MethodPost, "big?uploadId=" + id,
			"<CompleteMultipartUpload><Part><PartNumber>1</PartNumber></Part></CompleteMultipartUpload>"},
	} {
		if status, answer := do(w.method, w.path, w.body); status != http.StatusInsufficientStorage ||
			!strings.Contains(answer, "<Code>XMinioStorageFull</Code>") {
			t.Errorf("%s on a full bucket answered %d %q, want 507 XMinioStorageFull", w.what, status, answer)
		}
	}
	if keys := srv.Keys(); !slices.Equal(keys, []string{"kept"}) {
		t.Errorf("a full bucket holds %q, want only what it held before", keys)
	}
	if parts := srv.Parts("big"); parts != 0 {
		t.Errorf("a refused completion assembled %d parts", parts)
	}
	if status, answer := do(http.MethodGet, "kept", ""); status != http.StatusOK || answer != "before" {
		t.Errorf("a read of a full bucket answered %d %q, want 200 \"before\"", status, answer)
	}
	if status, _ := do(http.MethodPost, "other?uploads", ""); status != http.StatusOK {
		t.Errorf("an upload's start on a full bucket answered %d, want 200: it stores nothing", status)
	}
	if status, _ := do(http.MethodDelete, "big?uploadId="+id, ""); status != http.StatusNoContent {
		t.Errorf("an abandonment on a full bucket answered %d, want 204", status)
	}
	if status, _ := do(http.MethodDelete, "kept", ""); status != http.StatusNoContent {
		t.Errorf("a deletion on a full bucket answered %d, want 204", status)
	}

	srv.Fill(0, "")
	if status, _ := do(http.MethodPut, "new", "stored"); status != http.StatusOK {
		t.Fatalf("a put once room was made answered %d, want 200", status)
	}
	if keys := srv.Keys(); !slices.Equal(keys, []string{"new"}) {
		t.Errorf("the bucket holds %q once room was made, want the put's key", keys)
	}
}

// A PART IS REFUSED AS ASKED, ONCE ITS HOOK LETS IT GO: every upload of the
// part [s3fake.Server.FailPart] names is answered the S3 error named, a part of
// another number as ever — and [s3fake.Server.PartHook] runs with the part's
// number before either is answered, so a case decides when a refusal arrives.
func TestAPartIsRefusedAsAskedOnceItsHookLetsItGo(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	id := srv.Begin("parts")
	srv.FailPart(2, http.StatusForbidden, "AccessDenied")
	release := make(chan struct{})
	heard := make(chan int32, 2)
	srv.PartHook = func(number int32) {
		heard <- number
		if number == 2 {
			<-release
		}
	}
	put := func(number string) chan int {
		answered := make(chan int, 1)
		go func() {
			status := 0
			defer func() { answered <- status }()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
				srv.URL+"/files/parts?partNumber="+number+"&uploadId="+id, strings.NewReader("a part"))
			if err != nil {
				return
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				return
			}
			_ = resp.Body.Close()
			status = resp.StatusCode
		}()
		return answered
	}

	if status := <-put("3"); status != http.StatusOK {
		t.Fatalf("a part of a number nobody refuses answered %d, want 200", status)
	}
	if n := <-heard; n != 3 {
		t.Fatalf("the hook heard part %d, want 3", n)
	}
	refused := put("2")
	if n := <-heard; n != 2 {
		t.Fatalf("the hook heard part %d, want 2", n)
	}
	select {
	case status := <-refused:
		t.Fatalf("the refused part was answered (%d) while its hook still held it", status)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if status := <-refused; status != http.StatusForbidden {
		t.Fatalf("the refused part answered %d, want 403", status)
	}
}

// A DIRECTORY BUCKET HANDS OUT SESSIONS: S3 Express's CreateSession — `GET
// /{bucket}?session`, which the SDK sends before it signs any request to a
// bucket named `…--x-s3` — is answered with credentials that expire
// [s3fake.Server.SessionLife] from now, and in the past for a negative life,
// and counted as a CreateSession; where it was routed as a bucket-level
// request it fell through to 501 and no directory bucket could be stood up.
func TestADirectoryBucketHandsOutSessions(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		life time.Duration
		// want is how long from now the session should expire.
		want time.Duration
	}{
		{name: "S3 Express's own five minutes", want: 5 * time.Minute},
		{name: "a life of its own", life: time.Hour, want: time.Hour},
		{name: "already expired", life: -time.Minute, want: -time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			const bucket = "files--use1-az4--x-s3"
			srv := s3fake.Start(t, bucket)
			srv.SessionLife = c.life
			if got := srv.Bucket(); got != bucket {
				t.Fatalf("the fake serves %q, want %q", got, bucket)
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/"+bucket+"?session", nil)
			if err != nil {
				t.Fatal(err)
			}
			asked := time.Now()
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CreateSession answered %d, want 200", resp.StatusCode)
			}
			var session struct {
				XMLName     xml.Name `xml:"CreateSessionResult"`
				Credentials struct {
					AccessKeyID     string `xml:"AccessKeyId"`
					SecretAccessKey string `xml:"SecretAccessKey"`
					SessionToken    string `xml:"SessionToken"`
					Expiration      string `xml:"Expiration"`
				} `xml:"Credentials"`
			}
			if err := xml.NewDecoder(resp.Body).Decode(&session); err != nil {
				t.Fatalf("CreateSession's answer does not decode: %v", err)
			}
			creds := session.Credentials
			if creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.SessionToken == "" {
				t.Fatalf("CreateSession answered credentials %+v, want every field set", creds)
			}
			expires, err := time.Parse("2006-01-02T15:04:05Z", creds.Expiration)
			if err != nil {
				t.Fatalf("the session's expiration %q does not parse: %v", creds.Expiration, err)
			}
			// TO THE SECOND, as the answer spells it.
			if off := expires.Sub(asked.Add(c.want)); off < -2*time.Second || off > 2*time.Second {
				t.Fatalf("the session expires at %v, want about %v from when it was asked for",
					expires, c.want)
			}
			if n := srv.RequestsOf("CreateSession"); n != 1 {
				t.Fatalf("one CreateSession counted as %d", n)
			}
		})
	}
}

// FAIL FOR REFUSES EVERYTHING FOR ITS SPAN: what [s3fake.Server.FailFor] arms
// is a bucket that fails for a while measured in time, so every request inside
// the span — whatever it is — is answered the S3 error named, without spending
// a [s3fake.Server.FailNext] count, and a request after the span is answered
// as ever.
func TestFailForRefusesEverythingForItsSpan(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.Put("k", []byte("an object"))
	do := func(method string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+"/files/k", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	srv.FailNext(1, http.StatusServiceUnavailable, "SlowDown")
	// A SPAN NO REQUEST HERE OUTLASTS, so every one sent is inside it.
	srv.FailFor(time.Hour, http.StatusInternalServerError, "InternalError")
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodGet} {
		if status := do(method); status != http.StatusInternalServerError {
			t.Fatalf("a %s inside the span answered %d, want 500", method, status)
		}
	}
	// A SPAN THAT HAS ENDED, waited past, so the next request is after it.
	const span = 50 * time.Millisecond
	srv.FailFor(span, http.StatusInternalServerError, "InternalError")
	time.Sleep(span)
	// THE COUNT FAILNEXT ARMED IS STILL THERE, untouched by the span.
	if status := do(http.MethodGet); status != http.StatusServiceUnavailable {
		t.Fatalf("the first request after the span answered %d, want FailNext's 503", status)
	}
	if status := do(http.MethodGet); status != http.StatusOK {
		t.Fatalf("a request after the span answered %d, want 200", status)
	}
}

// A DIRECTORY BUCKET'S SESSIONS ARE REFUSED AS ASKED: every CreateSession
// [s3fake.Server.RefuseSessions] refuses is answered the S3 error named while
// its objects answer as ever, and a status of zero hands sessions out again.
func TestRefusedSessionsAreAnsweredTheErrorNamed(t *testing.T) {
	t.Parallel()
	const bucket = "files--use1-az4--x-s3"
	srv := s3fake.Start(t, bucket)
	srv.Put("k", []byte("an object"))
	do := func(path string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	srv.RefuseSessions(http.StatusInternalServerError, "InternalError")
	if status := do("/" + bucket + "?session"); status != http.StatusInternalServerError {
		t.Fatalf("a refused CreateSession answered %d, want 500", status)
	}
	if status := do("/" + bucket + "/k"); status != http.StatusOK {
		t.Fatalf("an object of a bucket whose sessions are refused answered %d, want 200", status)
	}
	srv.RefuseSessions(0, "")
	if status := do("/" + bucket + "?session"); status != http.StatusOK {
		t.Fatalf("a CreateSession once sessions are handed out again answered %d, want 200", status)
	}
	if n := srv.RequestsOf("CreateSession"); n != 2 {
		t.Fatalf("two CreateSessions counted as %d", n)
	}
}
