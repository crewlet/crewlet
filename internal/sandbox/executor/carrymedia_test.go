package executor_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// inMembers is archive, gzip, written again as n gzip members, as a session
// kept with its payloads apart is spliced back: one stream of the same
// plaintext, cut at n-1 places.
func inMembers(t *testing.T, archive []byte, n int) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for i := range n {
		zw := gzip.NewWriter(&out)
		if _, err = zw.Write(plain[i*len(plain)/n : (i+1)*len(plain)/n]); err != nil {
			t.Fatal(err)
		}
		if err = zw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out.Bytes()
}

// A SESSION SPLICED BACK FROM ITS MEMBERS IS RESTORED AS ONE, AND ITS BOX KNOWS
// IT: a session kept with its large payloads apart comes back as several gzip
// members, which the run's script restores and resumes as it does an archive of
// one; the box keeps the digest of those very bytes, so the same session carried
// back into it is its own copy, never written over the work it did since.
//
// Mutation: have the script read one member alone, or digest the archive
// otherwise than the take does, and the restore or the second carry goes red.
func TestASessionSplicedFromItsMembersIsRestoredAsOne(t *testing.T) {
	t.Parallel()
	first, a := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, first, a, sandbox.RunRequest{Brief: "begin"})
	described, body, err := first.OpenCapture(t.Context(), a)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v", body, err)
	}
	archive, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}
	spliced := inMembers(t, archive, 3)
	described.Size = int64(len(spliced))

	second, b := sessionBox(t, "2.1.310")
	_, res := runToEnd(t, second, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: bytes.NewReader(spliced)}})
	if !res.Continued || res.SessionID != ran.SessionID {
		t.Fatalf("the spliced session was not continued (continued %v, session %q):\n%s", res.Continued,
			res.SessionID, boxFile(t, b, paths(b).Err()))
	}
	own := boxFile(t, b, transcriptOf(b, ran.SessionID)) + `{"type":"user","line":"the box's own"}` + "\n"
	if err = b.WriteFile(t.Context(), transcriptOf(b, ran.SessionID), []byte(own)); err != nil {
		t.Fatal(err)
	}
	_, again := runToEnd(t, second, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: bytes.NewReader(spliced)}})
	if !again.Continued {
		t.Fatalf("the run did not continue the session its box held:\n%s", boxFile(t, b, paths(b).Err()))
	}
	if got := boxFile(t, b, transcriptOf(b, ran.SessionID)); !strings.HasPrefix(got, own) {
		t.Errorf("the box's own copy was written over by the spliced session it already held:\n%s", got)
	}
}
