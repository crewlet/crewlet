package executor

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// mediaPayload is n bytes of base64 text, a payload a harness read whole: the
// encoding of bytes seeded by seed, so two seeds differ and one repeats.
func mediaPayload(seed byte, n int) string {
	raw := make([]byte, n/4*3)
	rand.NewChaCha8([32]byte{seed}).Read(raw)
	return base64.StdEncoding.EncodeToString(raw)
}

// mediaTake is archive, of layout, taken by a take keeping its media apart
// within allowance: the archive whole, and the body it frames.
func mediaTake(t *testing.T, layout archiveLayout, archive []byte, s scrubber, allowance int64) (whole, body []byte) {
	t.Helper()
	var full, framed bytes.Buffer
	if err := scrubArchive(layout, bytes.NewReader(archive), reopenOf(archive), &full, s,
		testBounds.splitting(&framed, allowance)); err != nil {
		t.Fatalf("the take keeping its media apart: %v", err)
	}
	return full.Bytes(), framed.Bytes()
}

// inlineTake is archive taken as a take keeping no media apart writes it.
func inlineTake(t *testing.T, layout archiveLayout, archive []byte, s scrubber) []byte {
	t.Helper()
	var full bytes.Buffer
	if err := scrubArchive(layout, bytes.NewReader(archive), reopenOf(archive), &full, s, testBounds); err != nil {
		t.Fatalf("the take keeping its media inline: %v", err)
	}
	return full.Bytes()
}

// keptApart is a framed body as a spool keeps it and a later launch opens it:
// split (sandbox.SplitTakenBody), each first member sealed and uploaded as an
// object of its own and the text object beside them, then opened through the
// session store, which splices the members back, its read budgeted by the
// split's count of the archive restored, which must be what the open read. It
// answers what the open read, the text object as it was sealed, and each
// member uploaded by its digest.
func keptApart(t *testing.T, body []byte) (opened, text []byte, members map[string][]byte) {
	t.Helper()
	at := func() time.Time { return time.Now().UTC() }
	objects, err := objstore.NewStoreAt(memobj.NewAt(at), at)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewStreamSealer(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": bytes.Repeat([]byte("1"), 32)}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := executordomain.NewSessionStore(objects, sealer)
	if err != nil {
		t.Fatal(err)
	}
	handle, turn := "ana", statelog.NewOpID(time.Now(), "")
	members = map[string][]byte{}
	var media []executorkeep.MediaObject
	split := sandbox.SplitTakenBody(bytes.NewReader(body), func(member io.Reader) (string, error) {
		var raw, sealed bytes.Buffer
		spool, sealErr := store.SealMedia(handle, io.TeeReader(member, &raw), &sealed)
		if sealErr != nil {
			return "", sealErr
		}
		put, putErr := store.UploadMedia(t.Context(), spool, &sealed)
		if putErr != nil {
			return "", putErr
		}
		if _, twice := members[put.Digest]; twice {
			t.Errorf("media %s was uploaded twice", put.Digest)
		}
		media, members[put.Digest] = append(media, put), raw.Bytes()
		return spool.Digest, nil
	})
	var kept, sealed bytes.Buffer
	spool, err := store.Seal(handle, turn, io.TeeReader(split, &kept), &sealed)
	if err != nil {
		t.Fatalf("sealing the split body's text: %v", err)
	}
	put, err := store.Upload(t.Context(), spool, &sealed)
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.Open(t.Context(), executordomain.Session{Handle: handle, Turn: turn, Object: put.Object,
		Media: media, ArchiveBytes: split.Restored()})
	if err != nil {
		t.Fatalf("opening the kept session: %v", err)
	}
	defer func() { _ = read.Close() }()
	if opened, err = io.ReadAll(read); err != nil {
		t.Fatalf("reading the kept session: %v", err)
	}
	if len(media) > 0 && split.Restored() != int64(len(opened)) {
		t.Errorf("the split counts the archive its frames restore to as %d bytes, and it opens as %d",
			split.Restored(), len(opened))
	}
	return opened, kept.Bytes(), members
}

// gzipMembers is each gzip member of stream, its bytes as written.
func gzipMembers(t *testing.T, stream []byte) [][]byte {
	t.Helper()
	var members [][]byte
	r := bytes.NewReader(stream)
	for r.Len() > 0 {
		start := len(stream) - r.Len()
		zr, err := gzip.NewReader(r)
		if err != nil {
			t.Fatalf("member %d is no gzip: %v", len(members), err)
		}
		zr.Multistream(false)
		if _, err = io.Copy(io.Discard, zr); err != nil {
			t.Fatalf("member %d does not read: %v", len(members), err)
		}
		members = append(members, stream[start:len(stream)-r.Len()])
	}
	return members
}

// mediaPlain is stream's members read as one.
func mediaPlain(t *testing.T, stream []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(plain)
}

// frameKinds is the kind of each frame of a text object of frames, and the
// digest each media frame names.
func frameKinds(t *testing.T, text []byte) (kinds string, named [][]byte) {
	t.Helper()
	for r := bytes.NewReader(text); r.Len() > 0; {
		kind, _ := r.ReadByte()
		switch kind {
		case executorkeep.FrameText:
			n, err := binary.ReadUvarint(r)
			if err != nil || int64(n) > int64(r.Len()) {
				t.Fatalf("a text frame of %d bytes does not read: %v", n, err)
			}
			_, _ = r.Seek(int64(n), io.SeekCurrent)
			if !strings.HasSuffix(kinds, "T") {
				kinds += "T"
			}
		case executorkeep.FrameMedia:
			digest := make([]byte, executorkeep.MediaDigestBytes)
			if _, err := io.ReadFull(r, digest); err != nil {
				t.Fatal(err)
			}
			kinds, named = kinds+"R", append(named, digest)
		default:
			t.Fatalf("the text object holds a frame of kind %q", kind)
		}
	}
	return kinds, named
}

// textFrames is how many text frames frames holds: a take's body past its
// lead, or a text object.
func textFrames(t *testing.T, frames []byte) int {
	t.Helper()
	n := 0
	for r := bytes.NewReader(frames); r.Len() > 0; {
		switch kind, _ := r.ReadByte(); kind {
		case executorkeep.FrameText, sandbox.FrameMember:
			size, err := binary.ReadUvarint(r)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = r.Seek(int64(size), io.SeekCurrent)
			if kind == executorkeep.FrameText {
				n++
			}
		case executorkeep.FrameMedia, sandbox.FrameMemberEnd:
			_, _ = r.Seek(executorkeep.MediaDigestBytes, io.SeekCurrent)
		default:
			t.Fatalf("a frame of kind %q", kind)
		}
	}
	return n
}

// claudeRead is a Claude Code line holding a file it read whole: the payload in
// the message the model is sent and, where twice is set, again in the tool's
// own result, as Claude Code writes a PDF read.
func claudeRead(payload string, twice bool) string {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":[{"type":` +
		`"document","source":{"type":"base64","media_type":"application/pdf","data":"` + payload + `"}}]}]}`
	if twice {
		line += `,"toolUseResult":{"type":"pdf","file":{"base64":"` + payload + `"}}`
	}
	return line + "}"
}

// claudeArchive is a Claude Code session of lines, as a box packs it.
func claudeArchive(t *testing.T, lines ...string) []byte {
	t.Helper()
	return tarOf(t, map[string]string{"projects/p/s.jsonl": `{"type":"user","sessionId":"s"}` + "\n" +
		strings.Join(lines, "\n") + "\n"})
}

// A TAKE CUTS EACH LARGE PAYLOAD INTO A GZIP MEMBER OF ITS OWN, and nothing
// else changes: the archive whole is the text's member, the payload's and the
// text's after it, the payload's member holds the payload's text alone, and
// the body framed from it, split, kept and opened as a launch opens it, reads
// back byte for byte as the archive whole, the digest its box keeps
// included, its text's frames merged a read at a time around the payload's.
// Each layout: a transcript's tar, and an export, its payload a data URI's
// base64.
//
// Mutation: open the payload's member a byte late, splice its frame in the
// wrong place, or merge text frames short, and the members or the open differ.
func TestATakeCutsEachLargePayloadIntoAMemberOfItsOwn(t *testing.T) {
	t.Parallel()
	payload := mediaPayload(1, mediaMinBytes+8)
	export := `{"info":{"id":"ses_1"},"messages":[{"parts":[{"type":"file","mime":"image/png","url":` +
		`"data:image/png;base64,` + payload + `"}]}]}`
	for name, tc := range map[string]struct {
		layout  archiveLayout
		archive []byte
	}{
		"a transcript's tar": {archiveTar, claudeArchive(t, claudeRead(payload, false))},
		"an export":          {archiveDocument, gzipOf(t, export)},
	} {
		whole, body := mediaTake(t, tc.layout, tc.archive, newScrubber(nil), executorkeep.SessionMaxBytes)
		if body[0] != sandbox.BodySpliced {
			t.Fatalf("%s: the body is led by %q, want it spliced", name, body[0])
		}
		members := gzipMembers(t, whole)
		if len(members) != 3 || mediaPlain(t, members[1]) != payload {
			t.Fatalf("%s: the archive is %d members, want the text's, the payload's alone and the text's after",
				name, len(members))
		}
		opened, text, kept := keptApart(t, body)
		if !bytes.Equal(opened, whole) {
			t.Errorf("%s: the session opened is %d bytes, not the %d of the archive the take digested", name,
				len(opened), len(whole))
		}
		var sum cksum
		_, _ = sum.Write(opened)
		var want cksum
		_, _ = want.Write(whole)
		if sum.String() != want.String() {
			t.Errorf("%s: the restore's cksum is %s, the take's %s", name, sum.String(), want.String())
		}
		digest := sha256.Sum256(members[1])
		if kinds, named := frameKinds(t, text); kinds != "TRT" || !bytes.Equal(named[0], digest[:]) ||
			!bytes.Equal(kept[hex.EncodeToString(digest[:])], members[1]) {
			t.Errorf("%s: the text object's frames are %q, want the text, the payload's member by its digest, "+
				"and the text", name, kinds)
		}
		if strings.Contains(string(text), payload[:64]) {
			t.Errorf("%s: the text object still holds the payload", name)
		}
		if merged, framed := textFrames(t, text), textFrames(t, body[1:]); merged >= framed {
			t.Errorf("%s: the text object holds %d text frames of the body's %d, want them merged", name, merged,
				framed)
		}
	}
}

// THE ARCHIVE A TAKE KEEPS WHOLE IS THE ONE A TAKE KEEPING NO MEDIA APART
// KEEPS, read as gzip: the payloads cut into members of their own change the
// members and not a byte of what they hold, in each layout. And a body with no
// payload to cut is the archive whole, byte for byte, behind its lead.
//
// Mutation: keep the payload's member out of the archive whole, or write a
// frame's head into it, and the plaintexts differ.
func TestTheFullStreamIsTheArchiveATakeWithoutMediaKeeps(t *testing.T) {
	t.Parallel()
	payload := mediaPayload(2, mediaMinBytes)
	export := `{"messages":[{"parts":[{"url":"data:image/jpeg;base64,` + payload + `"},{"text":"x"}]}]}`
	for name, tc := range map[string]struct {
		layout  archiveLayout
		archive []byte
	}{
		"a transcript's tar": {archiveTar, claudeArchive(t, claudeRead(payload, true), `{"type":"x"}`)},
		"an export":          {archiveDocument, gzipOf(t, export)},
	} {
		whole, _ := mediaTake(t, tc.layout, tc.archive, newScrubber(nil), executorkeep.SessionMaxBytes)
		if got, want := mediaPlain(t, whole), mediaPlain(t, inlineTake(t, tc.layout, tc.archive,
			newScrubber(nil))); got != want {
			t.Errorf("%s: the archive kept with its media apart reads %d bytes, and kept whole %d", name,
				len(got), len(want))
		}
	}
	text := claudeArchive(t, `{"type":"assistant","message":{"content":"short"}}`)
	whole, body := mediaTake(t, archiveTar, text, newScrubber(nil), executorkeep.SessionMaxBytes)
	if body[0] != sandbox.BodyWhole || !bytes.Equal(body[1:], whole) || !bytes.Equal(whole,
		inlineTake(t, archiveTar, text, newScrubber(nil))) {
		t.Error("a session with no payload is not its archive whole behind the body's lead")
	}
}

// A PAYLOAD WRITTEN TWICE IS STORED ONCE: Claude Code writes a PDF it read in
// the message and again in the tool's result, and the take frames the second
// as a repeat of the first, so the spool stores one member, which the open
// splices in both places.
//
// Mutation: frame the repeat as a first, or leave its member's digest
// unreset, and the take stores two or refuses the session.
func TestAPayloadWrittenTwiceIsStoredOnce(t *testing.T) {
	t.Parallel()
	payload := mediaPayload(3, mediaMinBytes)
	whole, body := mediaTake(t, archiveTar, claudeArchive(t, claudeRead(payload, true)), newScrubber(nil),
		executorkeep.SessionMaxBytes)
	members := gzipMembers(t, whole)
	if len(members) != 5 || !bytes.Equal(members[1], members[3]) {
		t.Fatalf("the archive is %d members, want the payload's twice, alike, between the text's", len(members))
	}
	opened, text, kept := keptApart(t, body)
	kinds, named := frameKinds(t, text)
	if len(kept) != 1 || kinds != "TRTRT" || !bytes.Equal(named[0], named[1]) {
		t.Errorf("the spool stored %d members and the text object frames %q, want one member named twice", len(kept),
			kinds)
	}
	if !bytes.Equal(opened, whole) {
		t.Error("the session opened is not the archive the take digested")
	}
}

// A PAYLOAD UNDER THE THRESHOLD STAYS INLINE: one byte of kept text past it
// (a whole quantum, as base64 comes) and none under it is cut, and one a
// quantum shorter is not, so a short image costs no object of its own.
//
// Mutation: compare with > for >=, or count the data URI's head, and an edge
// flips.
func TestAPayloadUnderTheThresholdStaysInline(t *testing.T) {
	t.Parallel()
	for n, apart := range map[int]bool{mediaMinBytes: true, mediaMinBytes - 4: false} {
		export := `{"parts":[{"url":"data:image/png;base64,` + mediaPayload(4, n) + `"}]}`
		_, body := mediaTake(t, archiveDocument, gzipOf(t, export), newScrubber(nil), executorkeep.SessionMaxBytes)
		if got := body[0] == sandbox.BodySpliced; got != apart {
			t.Errorf("a payload of %d bytes is kept apart: %v, want %v", n, got, apart)
		}
	}
}

// A RUN THE SCRUB REDACTED STAYS INLINE, redacted: a value the engine put in
// the box that a payload's text holds is taken out, and the marker in its
// place ends the run, so no member holds it and the archive is kept as today.
//
// Mutation: find payloads in the text before its scrub, and the value is kept
// apart, unscrubbed.
func TestARedactedRunStaysInline(t *testing.T) {
	t.Parallel()
	const planted = "PlantedTokenValue0123456789abcdef"
	payload := mediaPayload(5, mediaMinBytes)
	payload = payload[:len(payload)/2] + planted + payload[len(payload)/2:]
	whole, body := mediaTake(t, archiveTar, claudeArchive(t, claudeRead(payload, false)),
		newScrubber([]string{planted}), executorkeep.SessionMaxBytes)
	if body[0] != sandbox.BodyWhole {
		t.Errorf("a payload the scrub redacted is kept apart")
	}
	if strings.Contains(mediaPlain(t, whole), planted) {
		t.Error("the kept archive holds the planted value")
	}
}

// A VALUE PLANTED BESIDE A PAYLOAD IS STILL TAKEN OUT: the scrub runs before
// the cut, once a take, so a line keeping its payload apart keeps no value the
// engine put in the box in its text, in its payload's member or in the
// session opened.
//
// Mutation: cut the payload before the line's scrub, and the value is kept.
func TestAValuePlantedBesideAPayloadIsStillTakenOut(t *testing.T) {
	t.Parallel()
	const planted = "planted-beside-0123456789abcdef"
	payload := mediaPayload(6, mediaMinBytes)
	line := strings.TrimSuffix(claudeRead(payload, false), "}") + `,"note":"the token is ` + planted + `"}`
	whole, body := mediaTake(t, archiveTar, claudeArchive(t, line), newScrubber([]string{planted}),
		executorkeep.SessionMaxBytes)
	if body[0] != sandbox.BodySpliced {
		t.Fatal("the payload beside the planted value was not kept apart")
	}
	opened, text, kept := keptApart(t, body)
	if strings.Contains(mediaPlain(t, opened), planted) || strings.Contains(mediaPlain(t, whole), planted) {
		t.Error("the session opened holds the planted value")
	}
	for _, member := range kept {
		if strings.Contains(mediaPlain(t, member), planted) {
			t.Error("the payload's member holds the planted value")
		}
	}
	if bytes.Contains(text, []byte(planted)) {
		t.Error("the text object holds the planted value")
	}
}

// MEDIA PAST THE ALLOWANCE OR THE CAP STAYS INLINE: a take keeps apart the
// first occurrences of at most executorkeep.MediaMaxCount texts holding at
// most its allowance in all, and every repeat of one it keeps; past either, a
// payload stays in the text, as the archive always held it.
//
// Mutation: count a repeat against the cap or the allowance, or let either
// pass by one, and a case goes red.
func TestMediaPastTheAllowanceOrCapStaysInline(t *testing.T) {
	t.Parallel()
	sum := func(i int) (s [sha256.Size]byte) {
		s[0], s[1] = byte(i), byte(i>>8)
		return s
	}
	capped := &mediaPlan{on: true, allowance: math.MaxInt64}
	for i := range executorkeep.MediaMaxCount + 1 {
		capped.add(int64(i)<<21, mediaMinBytes, sum(i))
	}
	capped.add(1<<40, mediaMinBytes, sum(0))
	if len(capped.texts) != executorkeep.MediaMaxCount || len(capped.found) != executorkeep.MediaMaxCount+1 {
		t.Errorf("past the cap the plan keeps %d texts and %d payloads, want %d and a repeat beside them",
			len(capped.texts), len(capped.found), executorkeep.MediaMaxCount)
	}
	allowed := &mediaPlan{on: true, allowance: 2 * mediaMinBytes}
	for i := range 3 {
		allowed.add(int64(i)<<21, mediaMinBytes, sum(i))
	}
	allowed.add(1<<40, mediaMinBytes, sum(1))
	if allowed.taken != 2*mediaMinBytes || len(allowed.found) != 3 {
		t.Errorf("past the allowance the plan holds %d bytes of %d payloads, want two firsts and a repeat",
			allowed.taken, len(allowed.found))
	}

}

// A LINE THAT LOSES A DEFINITION CUTS NO MEDIA: its cut is applied as the
// second reading copies it, so its payloads stay inline and the cut never
// meets a member's edge; and what it would have first held is first held by
// the next line holding it.
//
// Mutation: keep the line's payloads, and the second reading's cut or the
// later line's member goes wrong.
func TestALineThatLosesADefinitionCutsNoMedia(t *testing.T) {
	t.Parallel()
	payload := mediaPayload(9, mediaMinBytes)
	record := `{"type":"attachment","uuid":"u-1","image":"` + payload + `","attachment":{"type":` +
		`"deferred_tools_record","entries":[{"name":"mcp__cloudhost__backup_list","description":"x"},` +
		`{"name":"mcp__monitor__search","description":"y"}]}}`
	s := newScrubber([]string{"cloudhost"})
	whole, body := mediaTake(t, archiveTar, claudeArchive(t, record), s, executorkeep.SessionMaxBytes)
	if body[0] != sandbox.BodyWhole || strings.Contains(mediaPlain(t, whole), "backup_list") {
		t.Fatalf("a line losing a definition keeps its payload apart (%q) or its definition", body[0])
	}
	whole, body = mediaTake(t, archiveTar, claudeArchive(t, record, claudeRead(payload, false)), s,
		executorkeep.SessionMaxBytes)
	if members := gzipMembers(t, whole); len(members) != 3 || mediaPlain(t, members[1]) != payload {
		t.Fatalf("the next line's payload is %d members, want the one it first holds apart", len(members))
	}
	if opened, _, _ := keptApart(t, body); !bytes.Equal(opened, whole) {
		t.Error("the session opened is not the archive the take digested")
	}
}

// A REPEAT COMPRESSES TO ITS FIRST'S BYTES: a payload's member is a function
// of the payload alone, wherever it stands and however its writes are cut, so
// a repeat's member is held to its first's digest and the spool stores one.
//
// Mutation: carry the text member's compressor state into the payload's (no
// Reset), and the members differ with what stood before them.
func TestARepeatCompressesToItsFirstsBytes(t *testing.T) {
	t.Parallel()
	payload := mediaPayload(10, mediaMinBytes)
	member := func(before string, chunk int) []byte {
		plan := &mediaPlan{on: true, allowance: math.MaxInt64}
		plan.add(int64(len(before)), int64(len(payload)), sha256.Sum256([]byte(payload)))
		var full, body bytes.Buffer
		split, err := newMemberSplit(&full, &body, plan)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = split.Write([]byte(before)); err != nil {
			t.Fatal(err)
		}
		if err = split.media(0); err != nil {
			t.Fatal(err)
		}
		for at := 0; at < len(payload); at += chunk {
			if _, err = split.Write([]byte(payload[at:min(at+chunk, len(payload))])); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = split.Write([]byte("after")); err != nil {
			t.Fatal(err)
		}
		if err = split.Close(); err != nil {
			t.Fatal(err)
		}
		return gzipMembers(t, full.Bytes())[1]
	}
	want := member("", len(payload))
	for _, tc := range []struct {
		before string
		chunk  int
	}{{strings.Repeat(payload[:4096], 9), 7}, {"{", 32 << 10}, {payload[:100], 64<<10 + 1}} {
		if got := member(tc.before, tc.chunk); !bytes.Equal(got, want) {
			t.Errorf("after %d bytes in writes of %d, the payload's member differs from its first's",
				len(tc.before), tc.chunk)
		}
	}
}

// A LINE KEPT AS TEXT CUTS NO MEDIA: one read as JSON that turns out none (a
// value with more after it) is read again as text, from where it began, and
// what its JSON listed goes with that reading, so its payload stays in the
// text as it was written.
//
// Mutation: keep a unit's payloads past the reading that goes on from it, and
// the line kept as text is cut.
func TestALineKeptAsTextCutsNoMedia(t *testing.T) {
	t.Parallel()
	payload := mediaPayload(11, mediaMinBytes)
	whole, body := mediaTake(t, archiveTar, claudeArchive(t, claudeRead(payload, false)+" and more"),
		newScrubber(nil), executorkeep.SessionMaxBytes)
	if body[0] != sandbox.BodyWhole || !strings.Contains(mediaPlain(t, whole), payload+`"}}]}]}} and more`) {
		t.Errorf("a line kept as text keeps its payload apart (%q), or not as written", body[0])
	}
}

// THE MEDIA KEPT APART FIT THE KEEP'S PACE: the store paces each object by the
// mebibyte begun, so thirty-two payloads of 1.4 MiB kept apart would cost a
// keep at the floor pace a third more than today's budget, and the take keeps
// fewer apart, the rest inline, so its session and media fit that budget.
//
// Mutation: count the allowance in raw bytes alone, or forget the text
// object's own mebibyte, and the keep's pace passes its budget.
func TestTheMediaKeptApartFitTheKeepsPace(t *testing.T) {
	t.Parallel()
	const payload = 1_468_006
	member := int64(payload)*3/4 + 4<<10
	plan := &mediaPlan{on: true, allowance: executorkeep.SessionMaxBytes}
	for i := range executorkeep.MediaMaxCount {
		plan.add(int64(i)<<22, payload, [sha256.Size]byte{byte(i), 1})
	}
	kept := int64(executorkeep.MediaMaxCount)*member + 1<<20
	plan.paced(kept)
	apart := int64(len(plan.texts))
	if apart == 0 || apart >= executorkeep.MediaMaxCount || int64(len(plan.found)) != apart {
		t.Fatalf("the take keeps %d of %d payloads apart (%d listed), want fewer, each once", apart,
			executorkeep.MediaMaxCount, len(plan.found))
	}
	pace := objstore.PaceFor(kept - apart*member)
	for range apart {
		pace += objstore.PaceFor(member)
	}
	if budget := objstore.PaceFor(executorkeep.SealedMaxBytes); pace > budget {
		t.Errorf("a keep of %d payloads apart is paced %v, past today's %v", apart, pace, budget)
	}
}
