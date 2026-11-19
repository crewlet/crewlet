package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// memoTake scrubs archive, of layout, as a take does under use (nil: no
// memo), and is what it kept, read back ([unpacked]), and how many times it
// reopened the archive: once for a take that read its first reading once.
func memoTake(t *testing.T, layout archiveLayout, archive []byte, s scrubber, use *memoUse) (string, int) {
	t.Helper()
	kept, opened, err := scrubTake(layout, archive, s, use)
	if err != nil {
		t.Fatalf("the take: %v", err)
	}
	return unpacked(t, layout, kept), opened
}

// scrubTake is [memoTake]'s scrub: what it kept, how many times it reopened
// the archive, and its error.
func scrubTake(layout archiveLayout, archive []byte, s scrubber, use *memoUse) ([]byte, int, error) {
	opened := 0
	reopen := func() (io.ReadCloser, error) {
		opened++
		return io.NopCloser(bytes.NewReader(archive)), nil
	}
	var out bytes.Buffer
	err := scrubArchive(layout, bytes.NewReader(archive), reopen, &out, s, testBounds.remembering(use))
	return out.Bytes(), opened, err
}

// unpacked is a kept archive as text: an export's document, or each member of
// a tar as its name and its body.
func unpacked(t *testing.T, layout archiveLayout, kept []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(kept))
	if err != nil {
		t.Fatal(err)
	}
	if layout == archiveDocument {
		doc, readErr := io.ReadAll(zr)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(doc)
	}
	var b strings.Builder
	tr := tar.NewReader(zr)
	for {
		h, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			return b.String()
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		body, readErr := io.ReadAll(tr)
		if readErr != nil {
			t.Fatal(readErr)
		}
		fmt.Fprintf(&b, "%s\n%s\n", h.Name, body)
	}
}

// randomBase64 is n bytes of base64 of random bytes, as a file's encoding
// reads, seeded by seed.
func randomBase64(seed uint64, n int) string {
	r := rand.New(rand.NewPCG(seed, 1170))
	raw := make([]byte, n*3/4+3)
	for i := range raw {
		raw[i] = byte(r.Uint32())
	}
	return base64.StdEncoding.EncodeToString(raw)[:n]
}

// pdfLine is a Claude Code line answering a whole-PDF Read: its base64 twice,
// the result's and the tool's own, under the field names the harness writes.
func pdfLine(seed uint64, n int) string {
	return benchPDFLine("u0", "u1", "toolu_01", "2026-11-19T10:00:00.000Z", "JVBERi0xLjcK"+randomBase64(seed, n))
}

// userLine is a Claude Code user line whose content is text.
func userLine(text string) string {
	return `{"type":"user","message":{"role":"user","content":"` + text + `"}}`
}

// A TEXT A TAKE FOUND UNCHANGED IS NOT REDACTED AGAIN: a session holding a
// whole-PDF read, taken twice by one process, keeps the same bytes as a take
// with no memo, and the second take's first reading has the memo answer for
// all three of the read's texts (the line as text, and each copy of its
// base64; the first take, for the second copy), while its second reading
// writes both copies a chunk at a time as planned, without the memo.
//
// Mutation: never look the memo up, or have the second reading look it up
// (a first reading's stage), and the hits or the checked texts go red.
func TestATextATakeFoundUnchangedIsNotRedactedAgain(t *testing.T) {
	t.Parallel()
	archive := tarOf(t, map[string]string{"projects/p/s.jsonl": pdfLine(1, 400<<10) + "\n"})
	s := newScrubber([]string{longValue})
	want, _ := memoTake(t, archiveTar, archive, s, nil)
	memo := newScrubMemo()
	for take, hits := range []int{1, 3} {
		use := &memoUse{memo: memo, scope: "agent-a"}
		got, opened := memoTake(t, archiveTar, archive, s, use)
		switch {
		case got != want:
			t.Errorf("take %d kept other bytes than a take with no memo", take+1)
		case opened != 1:
			t.Errorf("take %d read its first reading %d times, want once", take+1, opened)
		case use.n.hits != hits || use.n.checked != 2 || use.n.diverged != 0:
			t.Errorf("take %d: %+v; want %d hits, both copies checked and none diverged", take+1, use.n, hits)
		}
	}
}

// memoVariant is the session of seed's variant v, a tar and an export: each
// large text shares its first chunk with every other variant's, and differs
// past it, variant 2 holding a key there; beside a whole-PDF read and a line
// losing a loaded tool definition whose description is large.
func memoVariant(t *testing.T, seed uint64, v int) (tarArchive, export []byte) {
	t.Helper()
	head := randomBase64(seed, 300<<10)
	tail := randomBase64(seed+uint64(v)+1, 20<<10)
	if v == 2 {
		tail += " " + shapedKey
	}
	tool := `{"type":"attachment","uuid":"u-2","attachment":{"type":"deferred_tools_record","entries":[` +
		`{"name":"mcp__cloudhost__x","description":"` + head + `"},` +
		`{"name":"mcp__monitor__y","description":"` + head + tail + `"}]}}`
	tarArchive = tarOf(t, map[string]string{
		"projects/p/s.jsonl":                pdfLine(seed, 300<<10) + "\n" + userLine(head+tail) + "\n" + tool + "\n",
		"tasks/s/1.json":                    `{"text":"` + head + tail + `"}`,
		"projects/p/s/tool-results/out.txt": head + tail,
	})
	export = gzipOf(t, `{"info":{"id":"ses_1"},"messages":[{"parts":[{"type":"file","url":"data:application/pdf;base64,`+
		head+tail+`"},{"type":"text","text":"`+head+`"}]}]}`)
	return tarArchive, export
}

// THE MEMO CHANGES NO BYTE OF A KEPT SESSION, whatever it holds: sessions
// whose large texts share their first chunks and differ past them, taken in a
// random order under one memo, each keep exactly what a take with no memo
// keeps, and none keeps the key one of them holds past a shared chunk.
//
// Mutation: plan a text the redaction changed, or have the second reading drop
// a text's last short chunk, and a take keeps a key or other bytes.
func TestTheMemoChangesNoByteOfAKeptSession(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{"cloudhost"})
	for seed := uint64(1); seed <= 2; seed++ {
		r := rand.New(rand.NewPCG(seed, 0))
		var tars, exports [3][]byte
		for v := range tars {
			tars[v], exports[v] = memoVariant(t, seed, v)
		}
		memo := newScrubMemo()
		for take := range 5 {
			v := r.IntN(3)
			for layout, archive := range map[archiveLayout][]byte{archiveTar: tars[v], archiveDocument: exports[v]} {
				want, _ := memoTake(t, layout, archive, s, nil)
				got, _ := memoTake(t, layout, archive, s, &memoUse{memo: memo, scope: "agent-a"})
				switch {
				case got != want:
					t.Errorf("seed %d, take %d of variant %d (layout %d) kept other bytes than with no memo",
						seed, take, v, layout)
				case strings.Contains(got, shapedKey) || strings.Contains(want, shapedKey):
					t.Errorf("seed %d, take %d of variant %d (layout %d) kept the key", seed, take, v, layout)
				}
			}
		}
	}
}

// A TEXT THE MEMO KNEW BY ITS START IS REDACTED WHERE IT CHANGED PAST IT: a
// take whose large line the memo answers for by its first chunk, but which no
// longer names a value an earlier take took out (the key then a marker, now
// itself), or whose key was written over its text past that chunk at the
// same length, keeps no key: its first reading is read once more without
// that chunk's answer, and redacts the line.
//
// Mutation: key the memo on the text before the by-value pass, check a text's
// length alone, or read again with the diverged answer still looked up, and
// a case keeps the key or reads three times.
func TestATextTheMemoKnewByItsStartIsRedactedWhereItChanged(t *testing.T) {
	t.Parallel()
	head := randomBase64(7, 300<<10)
	filler := strings.Repeat("x", len(shapedKey))
	for name, tc := range map[string]struct {
		first, second     string
		firstBy, secondBy scrubber
	}{
		"a value no longer named": {first: head + " " + shapedKey, second: head + " " + shapedKey,
			firstBy: newScrubber([]string{shapedKey}), secondBy: newScrubber(nil)},
		"a key written over it": {first: head + " " + filler, second: head + " " + shapedKey,
			firstBy: newScrubber(nil), secondBy: newScrubber(nil)},
	} {
		memo := newScrubMemo()
		first := tarOf(t, map[string]string{"projects/p/s.jsonl": userLine(tc.first) + "\n"})
		memoTake(t, archiveTar, first, tc.firstBy, &memoUse{memo: memo, scope: "agent-a"})
		second := tarOf(t, map[string]string{"projects/p/s.jsonl": userLine(tc.second) + "\n"})
		use := &memoUse{memo: memo, scope: "agent-a"}
		got, opened := memoTake(t, archiveTar, second, tc.secondBy, use)
		switch {
		case strings.Contains(got, shapedKey):
			t.Errorf("%s: the take kept the key", name)
		case use.n.diverged == 0 || opened != 2:
			t.Errorf("%s: %+v, reopened %d times; want the answer diverged and one reading again", name, use.n,
				opened)
		}
	}
}

// A `.json` FILE WHOSE DOCUMENT'S TEXT THE MEMO KNEW BY ITS START IS READ
// AGAIN, NEVER KEPT AS TEXT: kept as text, the value under a credential's key,
// a password no shape places, would be kept with it. Its text is shifted
// between the takes, so only the document's string meets the memo's answer.
//
// Mutation: drop measureFile's check of a diverged answer, and the file is
// kept as text, the password with it.
func TestAJSONFileWhoseAnswerDivergedIsReadAgainNeverKeptAsText(t *testing.T) {
	t.Parallel()
	const password = "correct-horse-battery"
	head := randomBase64(8, 300<<10)
	file := func(pad, tail string) []byte {
		return tarOf(t, map[string]string{
			"tasks/s/1.json": `{"auth":"` + password + `","pad":"` + pad + `","blob":"` + head + tail + `"}`,
		})
	}
	if !strings.Contains(redact.Secrets(`{"auth":"`+password+`"}`), password) {
		t.Fatal("the redaction places the password: the case would keep nothing either way")
	}
	s := newScrubber(nil)
	memo := newScrubMemo()
	memoTake(t, archiveTar, file("a", "one"), s, &memoUse{memo: memo, scope: "agent-a"})
	use := &memoUse{memo: memo, scope: "agent-a"}
	got, opened := memoTake(t, archiveTar, file("ab", "two"), s, use)
	switch {
	case strings.Contains(got, password) || !strings.Contains(got, credentialMarker):
		t.Errorf("the file was kept as text, its password with it: %.120q", got)
	case use.n.diverged != 1 || opened != 2:
		t.Errorf("%+v, reopened %d times; want one answer diverged and one reading again", use.n, opened)
	}
}

// A TEXT THAT DIVERGED COSTS NO OTHER TEXT ITS ANSWER: a session whose tool
// output grows past its first chunk between takes, beside a whole-PDF read,
// reads its first reading again with that output's chunk unanswered, and the
// read's three texts still answered.
//
// Mutation: read again with no lookups at the first divergence, and the
// read's texts are answered by nothing.
func TestATextThatDivergedCostsNoOtherTextItsAnswer(t *testing.T) {
	t.Parallel()
	head := randomBase64(9, 300<<10)
	session := func(output string) []byte {
		return tarOf(t, map[string]string{
			"a/tool-results/out.txt": output,
			"projects/p/s.jsonl":     pdfLine(9, 300<<10) + "\n",
		})
	}
	s := newScrubber(nil)
	memo := newScrubMemo()
	memoTake(t, archiveTar, session(head), s, &memoUse{memo: memo, scope: "agent-a"})
	use := &memoUse{memo: memo, scope: "agent-a"}
	if _, opened := memoTake(t, archiveTar, session(head+" and more"), s, use); opened != 2 ||
		use.n.diverged != 1 || use.n.hits != 3 {
		t.Errorf("%+v, reopened %d times; want the output diverged, one reading again, the read's 3 answered",
			use.n, opened)
	}
}

// A TEXT THE SECOND READING FINDS CHANGED FAILS THE TAKE, AND NOTHING OF THE
// CHUNK THAT DIFFERS IS WRITTEN: a planned text is written a chunk at a time,
// each once it matches its digest, so a box that rewrote a text between the
// readings has the take refused as a changed member, or export, and only the
// chunks before the change were handed on.
//
// Mutation: check the digests only at the text's close, and the changed chunk
// is written.
func TestATextTheSecondReadingFindsChangedFailsTheTake(t *testing.T) {
	t.Parallel()
	text := []byte(randomBase64(10, 3*scrubPieceBaseBytes+100))
	in := newPieceDigests(true)
	_, _ = in.Write(text)
	in.finish()
	changed := bytes.Clone(text)
	changed[2*scrubPieceBaseBytes+5] ^= 1
	var out bytes.Buffer
	var buf []byte
	piece := &checkedPiece{digests: in.list, out: &textOut{w: &out}, buf: &buf}
	_, err := piece.Write(changed)
	if err == nil {
		err = piece.Close()
	}
	if !errors.Is(err, errPieceChanged) || !bytes.Equal(out.Bytes(), text[:2*scrubPieceBaseBytes]) {
		t.Errorf("a text changed in its third chunk answered %v and wrote %d bytes; want it refused after the "+
			"first two chunks, %d bytes", err, out.Len(), 2*scrubPieceBaseBytes)
	}

	before, after := string(text[:len(text)-100]), string(changed[:len(changed)-100])
	s := newScrubber(nil)
	readings := [][2][]byte{
		{tarOf(t, map[string]string{"projects/p/s.jsonl": userLine(before) + "\n"}),
			tarOf(t, map[string]string{"projects/p/s.jsonl": userLine(after) + "\n"})},
		{tarOf(t, map[string]string{"out.txt": before}), tarOf(t, map[string]string{"out.txt": after})},
	}
	for _, r := range readings {
		err := scrubTar(bytes.NewReader(r[0]), reopenOf(r[1]), io.Discard, s, testBounds)
		if err == nil || !strings.Contains(err.Error(), "changed its member") {
			t.Errorf("a member rewritten between the readings: %v, want it refused as changed", err)
		}
	}
	err = scrubDocument(bytes.NewReader(gzipOf(t, `{"t":"`+before+`"}`)), reopenOf(gzipOf(t, `{"t":"`+after+`"}`)),
		io.Discard, s, testBounds)
	if err == nil || !strings.Contains(err.Error(), "export changed") {
		t.Errorf("an export rewritten between the readings: %v, want it refused as changed", err)
	}
}

// A TAKE LEARNS NOTHING OF ANOTHER SCOPE'S TEXTS: a session one agent's take
// left in the memo is answered for that agent's next take, and for another
// agent's not at all, so a box timing its takes learns nothing of another
// seat's texts.
//
// Mutation: leave the scope out of the memo's key, and the other agent's take
// is answered.
func TestATakeLearnsNothingOfAnotherScopesTexts(t *testing.T) {
	t.Parallel()
	archive := tarOf(t, map[string]string{"projects/p/s.jsonl": pdfLine(11, 300<<10) + "\n"})
	s := newScrubber(nil)
	memo := newScrubMemo()
	memoTake(t, archiveTar, archive, s, &memoUse{memo: memo, scope: "agent-a"})
	for scope, hits := range map[string]int{"agent-a": 3, "agent-b": 1} {
		use := &memoUse{memo: memo, scope: scope}
		memoTake(t, archiveTar, archive, s, use)
		if use.n.hits != hits {
			t.Errorf("a take scoped %s had %d answers, want %d (the read's second copy its own)", scope,
				use.n.hits, hits)
		}
	}
}

// THE MEMO KEEPS TO ITS BOUNDS, LEAST RECENTLY USED OUT FIRST: past
// scrubMemoEntries texts it holds that many; one scope holds at most
// scrubMemoScopeEntries and one first chunk scrubMemoWays; and a text used
// again outlives those kept after it.
//
// Mutation: drop a bound, or evict the newest, and a check goes red.
func TestTheMemoKeepsToItsBoundsLeastRecentlyUsedOutFirst(t *testing.T) {
	t.Parallel()
	memo := newScrubMemo()
	text := func(i int) pieceWhole { return pieceWhole{length: int64(i), digest: sha256.Sum256(fmt.Append(nil, i))} }
	key := func(scope string, i int) memoKey { return memoKeyOf(scope, sha256.Sum256(fmt.Append(nil, "k", i))) }
	memo.keep("s0", key("s0", 0), text(0))
	for i := 1; i < scrubMemoEntries+500; i++ {
		scope := fmt.Sprint("s", i%(scrubMemoEntries/scrubMemoScopeEntries+8))
		memo.keep(scope, key(scope, i), text(i))
		if i%100 == 0 {
			memo.keep("s0", key("s0", 0), text(0))
		}
	}
	if n, evicted := memo.size(); n != scrubMemoEntries || evicted == 0 {
		t.Errorf("the memo holds %d texts after evicting %d, want %d", n, evicted, scrubMemoEntries)
	}
	if got := memo.candidates(key("s0", 0)); len(got) != 1 || got[0] != text(0) {
		t.Errorf("the text used again holds %v, want it kept", got)
	}
	if got := memo.candidates(key("s1", 1)); len(got) != 0 {
		t.Errorf("the oldest text not used again holds %v, want it evicted", got)
	}
	for i := range 2 * scrubMemoScopeEntries {
		memo.keep("flood", key("flood", i), text(i))
	}
	if n := memo.scopes["flood"].entries.Len(); n != scrubMemoScopeEntries {
		t.Errorf("one scope holds %d texts after %d, want %d", n, 2*scrubMemoScopeEntries, scrubMemoScopeEntries)
	}
	shared := key("one", 0)
	for i := range scrubMemoWays + 3 {
		memo.keep("one", shared, text(1_000_000+i))
	}
	if got := memo.candidates(shared); len(got) != scrubMemoWays || got[0] != text(1_000_003) {
		t.Errorf("one first chunk holds %v, want the newest %d", got, scrubMemoWays)
	}
}

// TAKES SHARE THE MEMO SAFELY: eight takes at once under one memo each keep
// what a take with no memo keeps, run under the race detector.
//
// Mutation: drop the memo's lock, and the race detector fails the case.
func TestTakesShareTheMemoSafely(t *testing.T) {
	t.Parallel()
	archive := tarOf(t, map[string]string{"projects/p/s.jsonl": pdfLine(12, 300<<10) + "\n"})
	s := newScrubber(nil)
	want, _ := memoTake(t, archiveTar, archive, s, nil)
	memo := newScrubMemo()
	var wg sync.WaitGroup
	kept := make([][]byte, 8)
	errs := make([]error, 8)
	for i := range kept {
		wg.Go(func() {
			kept[i], _, errs[i] = scrubTake(archiveTar, archive, s, &memoUse{memo: memo, scope: fmt.Sprint("agent-", i%2)})
		})
	}
	wg.Wait()
	for i := range kept {
		if errs[i] != nil || unpacked(t, archiveTar, kept[i]) != want {
			t.Errorf("take %d (%v) kept other bytes than a take with no memo", i, errs[i])
		}
	}
}
