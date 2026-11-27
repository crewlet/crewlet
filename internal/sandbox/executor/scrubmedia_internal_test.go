package executor

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
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
	"github.com/crewlet/crewlet/internal/redact"
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
		testBounds.splitting(&framed, allowance, textPace)); err != nil {
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

// A PAYLOAD HOLDING A VALUE THE ENGINE PUT IN THE BOX IS KEPT APART,
// REDACTED: its text is classed as it came, so the marker the by-value pass
// writes in the value's place leaves it a payload, and the value is out of its
// member, the archive and the session opened.
//
// Mutation: class a payload by its text as kept, and it stays inline; keep a
// payload from the by-value pass, and its member holds the value.
func TestAPayloadHoldingAValueIsKeptApartRedacted(t *testing.T) {
	t.Parallel()
	const planted = "PlantedTokenValue0123456789abcde"
	payload := mediaPayload(5, mediaMinBytes)
	payload = payload[:len(payload)/2] + planted + payload[len(payload)/2:]
	whole, body := mediaTake(t, archiveTar, claudeArchive(t, claudeRead(payload, false)),
		newScrubber([]string{planted}), executorkeep.SessionMaxBytes)
	if body[0] != sandbox.BodySpliced {
		t.Fatal("a payload holding a value the scrub took out is not kept apart")
	}
	opened, text, kept := keptApart(t, body)
	if strings.Contains(mediaPlain(t, whole), planted) || strings.Contains(mediaPlain(t, opened), planted) ||
		strings.Contains(textObjectPlain(t, text), planted) {
		t.Error("the kept archive holds the planted value")
	}
	for _, member := range kept {
		if m := mediaPlain(t, member); strings.Contains(m, planted) || !strings.Contains(m, scrubMarker) {
			t.Error("the payload's member holds the planted value, or no marker in its place")
		}
	}
}

// shapedPayload is n bytes of base64 seeded by seed, url-safe where url, that
// spells shape at its middle, run on to a multiple of four so the whole stays
// well-formed: a token's shape, as encoded bytes spell one by chance.
func shapedPayload(seed byte, n int, url bool, shape string) string {
	raw := make([]byte, n/4*3)
	rand.NewChaCha8([32]byte{seed}).Read(raw)
	enc := base64.StdEncoding.EncodeToString(raw)
	if url {
		enc = base64.RawURLEncoding.EncodeToString(raw)
	}
	for len(shape)%4 != 0 {
		shape += "A"
	}
	return enc[:len(enc)/2] + shape + enc[len(enc)/2:]
}

// textObjectPlain is a kept session's text object read as text: its text
// frames' gzip members, the media frames between them left out.
func textObjectPlain(t *testing.T, text []byte) string {
	t.Helper()
	var members bytes.Buffer
	for r := bytes.NewReader(text); r.Len() > 0; {
		switch kind, _ := r.ReadByte(); kind {
		case executorkeep.FrameText:
			n, err := binary.ReadUvarint(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.CopyN(&members, r, int64(n)); err != nil {
				t.Fatal(err)
			}
		case executorkeep.FrameMedia:
			_, _ = r.Seek(executorkeep.MediaDigestBytes, io.SeekCurrent)
		default:
			t.Fatalf("the text object holds a frame of kind %q", kind)
		}
	}
	return mediaPlain(t, members.Bytes())
}

// awsKeyID is an AWS key id of prefix's kind ("AKIA", "ASIA"), well-formed
// for its rule and built here, so no scanner reads the source as holding one.
func awsKeyID(prefix string) string { return prefix + strings.Repeat("EXAMPLE0", 2) }

// payloadShapes is a payload spelling each shape a rule reads that base64 can
// spell (internal/redact's TestTheRulesBase64CanSpellAreKnown names them).
var payloadShapes = map[string]struct {
	url   bool
	shape string
}{
	"a Google refresh token's": {shape: "1//0" + strings.Repeat("Ab3", 20)},
	"an AWS access key id's":   {shape: awsKeyID("AKIA")},
	"an AWS session key id's":  {shape: awsKeyID("ASIA")},
	"an API key's":             {url: true, shape: "sk-" + strings.Repeat("Ab3_", 6)},
	"an Anthropic key's":       {url: true, shape: "sk-ant-" + strings.Repeat("Ab3_", 6)},
	"an OpenAI project key's":  {url: true, shape: "sk-proj-" + strings.Repeat("Ab3_", 6)},
	"a Slack token's":          {url: true, shape: "xoxb-" + strings.Repeat("Ab3-", 6)},
	"a GitHub token's":         {url: true, shape: "ghp_" + strings.Repeat("Ab3", 13)},
	"a GitHub fine-grained's":  {url: true, shape: "github_pat_" + strings.Repeat("Ab3_", 13)},
	"a GitLab token's":         {url: true, shape: "glpat-" + strings.Repeat("Ab3_", 6)},
	"a GitLab runner token's":  {url: true, shape: "glrt-" + strings.Repeat("Ab3_", 6)},
	"a Google client secret's": {url: true, shape: "GOCSPX-" + strings.Repeat("Ab3_", 6)},
	"a DigitalOcean token's":   {url: true, shape: "dop_v1_" + strings.Repeat("ab3", 8)},
}

// A PAYLOAD IS KEPT FROM THE SHAPE RULES, WHOLE: one spelling by chance what a
// rule reads, for every rule base64 can spell, is kept apart and opens byte
// for byte as it was read, its member the payload's text, in a transcript and
// as an export's data URI; no marker is written into it. The case first holds
// that the rule does take the shape out of the payload read alone.
//
// Mutation: shape a payload's text, as every text's, and every case goes red.
func TestAPayloadIsKeptFromTheShapeRulesWhole(t *testing.T) {
	t.Parallel()
	seed := byte(30)
	for name, tc := range payloadShapes {
		seed++
		payload := shapedPayload(seed, mediaMinBytes+8, tc.url, tc.shape)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if redact.Secrets(payload) == payload {
				t.Fatalf("the premise: the shape rules leave %q alone in a payload read alone", tc.shape)
			}
			export := `{"parts":[{"url":"data:image/png;base64,` + payload + `"}]}`
			for layout, archive := range map[archiveLayout][]byte{
				archiveTar:      claudeArchive(t, claudeRead(payload, false)),
				archiveDocument: gzipOf(t, export),
			} {
				whole, body := mediaTake(t, layout, archive, newScrubber(nil), executorkeep.SessionMaxBytes)
				if body[0] != sandbox.BodySpliced {
					t.Fatalf("layout %d: the payload is not kept apart", layout)
				}
				if members := gzipMembers(t, whole); len(members) != 3 || mediaPlain(t, members[1]) != payload {
					t.Errorf("layout %d: the payload's member is not the payload as it was read", layout)
				}
				opened, _, _ := keptApart(t, body)
				if plain := mediaPlain(t, opened); !strings.Contains(plain, payload) ||
					strings.Contains(plain, redact.Marker) {
					t.Errorf("layout %d: the session opened does not hold the payload as it was read", layout)
				}
			}
		})
	}
}

// A TEXT READ AS A PAYLOAD THAT TURNS OUT NONE IS SHAPED AS EVERY TEXT IS: a
// mebibyte and more of base64 and then words is kept byte for byte as a take
// keeping no media apart keeps it, and inline, whether the shape rules changed
// its first mebibyte (the first reading read again, every payload shaped) or
// left it as it came (shaped on from where it was written), a key in its
// words taken out either way.
//
// Mutation: write its first mebibyte unshaped where the rules change it, or
// its words unshaped, and a case goes red.
func TestATextReadAsAPayloadThatTurnsOutNoneIsShaped(t *testing.T) {
	t.Parallel()
	words := " and then the key sk-ant-api03-" + "REALKEY0123456789abcdef" + "ABCDEF here"
	clean := mediaPayload(41, mediaMinBytes+8)
	if redact.Secrets(clean) != clean {
		t.Fatal("the premise: the shape rules leave the clean payload alone")
	}
	long := "ghp_" + strings.Repeat("Ab3", 128<<10/3)
	for name, text := range map[string]string{
		"its first mebibyte changed by the shape rules": shapedPayload(42, mediaMinBytes+8, false,
			payloadShapes["a Google refresh token's"].shape) + words,
		"its first mebibyte as it came":                      clean + words,
		"a token longer than a piece, then one byte outside": shapedPayload(43, mediaMinBytes+8, true, long) + ".",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			archive := claudeArchive(t, claudeRead(text, false))
			whole, body := mediaTake(t, archiveTar, archive, newScrubber(nil), executorkeep.SessionMaxBytes)
			if body[0] != sandbox.BodyWhole {
				t.Error("a text that turned out no payload is kept apart")
			}
			plain, want := mediaPlain(t, whole), mediaPlain(t, inlineTake(t, archiveTar, archive, newScrubber(nil)))
			if plain != want {
				t.Errorf("the text kept is not what a take keeping no media apart keeps (%d bytes, want %d)",
					len(plain), len(want))
			}
			if strings.Contains(plain, "REALKEY") || strings.Contains(plain, long[:64]) {
				t.Error("a token in the text is kept")
			}
		})
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
	if strings.Contains(textObjectPlain(t, text), planted) {
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

// textPace is the pace units a take at the text bound uploads in: one object's
// at the sealed bound (internal/sandbox's textBound).
var textPace = paceUnits(executorkeep.SealedMaxBytes)

// THE MEDIA KEPT APART FIT THE KEEP'S PACE: the store paces each object by the
// mebibyte begun, so at the text bound thirty-two payloads of 1.4 MiB kept
// apart would cost a keep at the floor pace a third more than its budget, and
// the take keeps fewer apart, the rest inline; and at the kept bound, paced at
// executorkeep.KeptPaceMiB, two whole-file reads (each payload twice, 80.3 MiB
// kept) beside 13.7 MiB of text both stay apart, the session and its media
// within that budget.
//
// Mutation: count the allowance in raw bytes alone, forget the text object's
// own mebibyte, or pace the kept bound at one object's pace, and a case goes
// red.
func TestTheMediaKeptApartFitTheKeepsPace(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		allowance, pace, payload int64
		texts, copies            int
		text                     int64
		wantAll                  bool
	}{
		"thirty-two payloads at the text bound": {executorkeep.SessionMaxBytes, textPace, 1_468_006,
			executorkeep.MediaMaxCount, 1, 1 << 20, false},
		"two whole-file reads at the kept bound": {executorkeep.SessionKeptMaxBytes, executorkeep.KeptPaceMiB,
			28_001_000, 2, 2, 13_700 << 10, true},
	} {
		member := tc.payload*3/4 + 4<<10
		plan := &mediaPlan{on: true, allowance: tc.allowance, pace: tc.pace}
		for i := range tc.texts {
			for c := range tc.copies {
				plan.add(int64(i*tc.copies+c)<<26, tc.payload, [sha256.Size]byte{byte(i), 1})
			}
		}
		kept := int64(tc.texts*tc.copies)*member + tc.text
		plan.paced(kept)
		apart := int64(len(plan.texts))
		switch {
		case tc.wantAll && apart != int64(tc.texts):
			t.Errorf("%s: the take keeps %d of %d payloads apart, want every one", name, apart, tc.texts)
		case !tc.wantAll && (apart == 0 || apart >= int64(tc.texts)):
			t.Errorf("%s: the take keeps %d of %d payloads apart, want fewer", name, apart, tc.texts)
		}
		if int64(len(plan.found)) != apart*int64(tc.copies) {
			t.Errorf("%s: %d payloads listed, want each of the %d apart %d times", name, len(plan.found),
				apart, tc.copies)
		}
		pace := objstore.PaceFor(kept - apart*int64(tc.copies)*member)
		for range apart {
			pace += objstore.PaceFor(member)
		}
		if budget := time.Duration(tc.pace) * objstore.MiBPace; pace > budget {
			t.Errorf("%s: a keep of %d payloads apart is paced %v, past its %v", name, apart, pace, budget)
		}
	}
}

// TEXT PAST ITS BOUND IS REFUSED BEFORE THE SECOND READING: a take keeping its
// media apart whose first reading leaves more text inline than its seal keeps
// (here a session of text alone) is refused as too large once that reading is
// done, nothing written to its body and no spool left; within the bound it is
// kept.
//
// Mutation: drop the check, or count the payloads kept apart as text, and a
// case goes red.
func TestTextPastItsBoundIsRefusedBeforeTheSecondReading(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range 32 {
		lines = append(lines, `{"type":"assistant","text":"`+mediaPayload(byte(i), 64<<10)+`"}`)
	}
	archive := claudeArchive(t, lines...)
	for name, tc := range map[string]struct {
		textMax int64
		kept    bool
	}{
		"text past its bound": {textMax: 1 << 20},
		"text within it":      {textMax: 8 << 20, kept: true},
	} {
		dir := t.TempDir()
		var full, framed bytes.Buffer
		bounds := testBounds.spooling(spoolPlace{dir: dir}).splitting(&framed, executorkeep.SessionKeptMaxBytes,
			executorkeep.KeptPaceMiB)
		bounds.split.textMax = tc.textMax
		err := scrubArchive(archiveTar, bytes.NewReader(archive), reopenOf(archive), &full, newScrubber(nil), bounds)
		switch {
		case tc.kept && err != nil:
			t.Errorf("%s: refused: %v", name, err)
		case !tc.kept && (!errors.Is(err, executorkeep.ErrSessionTooLarge) || framed.Len() != 0 || full.Len() != 0):
			t.Errorf("%s: came to %v having written %d bytes, want it refused as too large before writing", name,
				err, framed.Len()+full.Len())
		}
		if left := spoolsLeft(t, dir); len(left) != 0 {
			t.Errorf("%s: the scrub left %v", name, left)
		}
	}
	plan := &mediaPlan{on: true, textMax: 1 << 20, found: []mediaAt{{n: 4 << 20}}}
	if err := plan.inlinePast(5<<20 - 64<<10); err != nil {
		t.Errorf("a spool of payloads kept apart is refused as text past its bound: %v", err)
	}
}

// ONLY WELL-FORMED BASE64 IS A PAYLOAD: a run of one alphabet, `=` only as
// its last one or two bytes, whole: so a key a keyed rule reads (`KEY=value`),
// a run mixing standard and base64url, a pad within it, a run one byte past a
// multiple of four, and a data URI with a parameter or a head past what a
// rule can spell are no payloads, scrubbed as every text is, the secret in
// them taken out; and runs of whole base64, padded or not, are payloads.
//
// Mutation: accept a mixed run, a pad within one, a ragged length, a
// parameter or a long head, and a case goes red.
func TestOnlyWellFormedBase64IsAPayload(t *testing.T) {
	t.Parallel()
	if mediaHeadPartMax != 19 {
		t.Fatalf("a media head's part holds %d bytes: move redact's mediaHeadPartMax, which its "+
			"TestNoRuleMatchesWithinAMediaHeadsPart reads, with it", mediaHeadPartMax)
	}
	std, url := mediaPayload(51, mediaMinBytes+8), shapedPayload(52, mediaMinBytes+8, true, "")
	half := func(run, in string) string { return run[:len(run)/2] + in + run[len(run)/2:] }
	secret := strings.Repeat("wJa1rXUt", 5)
	for name, c := range map[string]struct {
		text    string
		payload bool
	}{
		"an AWS secret key's assignment":      {half(std, "AWS_SECRET_ACCESS_KEY="+secret), false},
		"an AWS session token's assignment":   {half(std, "AWS_SESSION_TOKEN="+secret), false},
		"a password's assignment":             {half(std, "password="+secret), false},
		"a security token's assignment":       {half(std, "SecurityToken="+secret), false},
		"standard and base64url in one run":   {std + "-_" + url, false},
		"a pad within the run":                {half(std, "=="), false},
		"one byte past a multiple of four":    {std + "A", false},
		"three pads":                          {std[:len(std)-4] + "A===", false},
		"a data URI with a parameter":         {"data:image/png;charset=utf-8;base64," + std, false},
		"a data URI whose type spells a key":  {"data:xoxb-" + strings.Repeat("a1", 12) + "/png;base64," + std, false},
		"a data URI whose subtype spells one": {"data:image/" + awsKeyID("AKIA") + ";base64," + std, false},
		"standard, unpadded":                  {std, true},
		"standard, padded":                    {std[:len(std)-4] + "AA==", true},
		"base64url, unpadded and ragged":      {url + "AB", true},
		"a data URI":                          {"data:image/png;base64," + std, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			archive := claudeArchive(t, claudeRead(c.text, false))
			whole, body := mediaTake(t, archiveTar, archive, newScrubber(nil), executorkeep.SessionMaxBytes)
			if got := body[0] == sandbox.BodySpliced; got != c.payload {
				t.Fatalf("the string is kept apart: %v, want %v", got, c.payload)
			}
			if !c.payload {
				plain := mediaPlain(t, whole)
				if plain != mediaPlain(t, inlineTake(t, archiveTar, archive, newScrubber(nil))) {
					t.Error("the string is not scrubbed as a take keeping no media apart scrubs it")
				}
				if strings.Contains(plain, secret) {
					t.Error("the secret in it is kept")
				}
			}
		})
	}
}

// EVERY RUN THE CLASSIFIER TAKES IS WELL-FORMED BASE64, the premise redact's
// TestTheRulesBase64CanSpellAreKnown reads runs by: over random strings of
// both alphabets, pads and other bytes, whatever [mediaRun] calls a payload
// decodes under one of base64's four encodings, `=` only at its end.
//
// Mutation: let a pad stand within a run, or mix the alphabets, and a string
// it takes does not decode.
func TestEveryRunThePayloadClassTakesIsWellFormed(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(9, 9))
	const bytesOf = "ABCDabcd0189+/-_=.\\"
	run := []byte(mediaPayload(53, mediaMinBytes))
	taken := 0
	for range 4000 {
		tail := make([]byte, r.IntN(6))
		for i := range tail {
			tail[i] = bytesOf[r.IntN(len(bytesOf))]
		}
		text := append(append([]byte(nil), run[:mediaMinBytes-r.IntN(8)]...), tail...)
		var m mediaRun
		m.begin()
		m.feed(text)
		if !m.payload() {
			continue
		}
		taken++
		ok := false
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding,
			base64.RawURLEncoding} {
			if _, err := enc.DecodeString(string(text)); err == nil {
				ok = true
			}
		}
		if !ok || bytes.Contains(bytes.TrimRight(text, "="), []byte("=")) {
			t.Fatalf("the class takes %q, no well-formed base64", text[len(text)-12:])
		}
	}
	if taken == 0 {
		t.Fatal("the premise: the class takes none of the strings")
	}
}

var _ = holdsScrubTerm("payloads", "TestAPayloadsHoldIsWithinItsFigure")

// A PAYLOAD'S HOLD IS WITHIN ITS FIGURE: a string read for a payload is held
// in room the rewrite reuses, grown only as far as its text needs and never
// past [payloadHeldBytes], however its pieces fall. The "payloads" term counts
// it, and a string of words costs no mebibyte.
//
// Mutation: hand the room back unkept, or allocate the figure up front, and a
// case goes red.
func TestAPayloadsHoldIsWithinItsFigure(t *testing.T) {
	t.Parallel()
	figure := termFigure(t, "payloads", 0, 0)
	text := []byte("data:image/png;base64," + mediaPayload(54, 2*mediaMinBytes))
	write := func(held *[]byte, text []byte, piece int) {
		t.Helper()
		var out rewriteOut
		var str jsonStringWriter
		var span payloadSpan
		if err := str.begin(&out, 0); err != nil {
			t.Fatal(err)
		}
		str.finding(&span)
		room := *held
		p := newScrubber(nil).finding(&mediaPlan{}).payloadText(&str, &span, held)
		for at := 0; at < len(text); at += piece {
			if _, err := p.Write(text[at:min(at+piece, len(text))]); err != nil {
				t.Fatal(err)
			}
			if int64(cap(p.held)) > figure {
				t.Fatalf("a string read %d bytes at a time is held in %d bytes, past the figure %d", piece,
					cap(p.held), figure)
			}
			if len(p.held) > 0 && len(p.held) <= cap(room) && &p.held[0] != &room[:1][0] {
				t.Fatalf("a string read %d bytes at a time is held in room of its own, not the rewrite's", piece)
			}
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		if int64(cap(*held)) > figure {
			t.Fatalf("the rewrite keeps %d bytes of room, past the figure %d", cap(*held), figure)
		}
	}
	var held []byte
	words := []byte("a summary of words, no payload past its first bytes")
	write(&held, words, len(words))
	if cap(held) > len(words) {
		t.Errorf("a string of %d bytes of words leaves the rewrite %d bytes of room", len(words), cap(held))
	}
	for _, piece := range []int{1, 7, 4 << 10, textWholeBytes} {
		write(&held, text, piece)
	}
	if cap(held) < mediaMinBytes {
		t.Errorf("the rewrite keeps %d bytes of room after a payload, want what it held reused", cap(held))
	}
}

// A CREDENTIAL'S VALUE IS ITS MARKER, PAYLOAD OR NOT: with a take keeping its
// media apart, a value of whole base64 past the mebibyte under a credential's
// or a password's key is written as the credential's marker, nothing kept
// apart and nothing of it left, as it is with media inline.
//
// Mutation: read a string for a payload before its key is judged a
// credential's, and the value is kept apart.
func TestACredentialsValueIsItsMarkerWithMediaApart(t *testing.T) {
	t.Parallel()
	value := mediaPayload(55, mediaMinBytes+8)
	for _, key := range []string{"password", "db_password", "api_key", "client_secret", "token", "privateKey"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			archive := claudeArchive(t, `{"type":"user","`+key+`":"`+value+`"}`)
			whole, body := mediaTake(t, archiveTar, archive, newScrubber(nil), executorkeep.SessionMaxBytes)
			plain := mediaPlain(t, whole)
			if body[0] != sandbox.BodyWhole || !strings.Contains(plain, `"`+key+`":"`+credentialMarker+`"`) ||
				strings.Contains(plain, value[:64]) {
				t.Errorf("the value under %q is not its marker alone, nothing kept apart", key)
			}
		})
	}
}

// THE MEDIA BOUNDARY IS WHERE THE DOCS SAY: inside a session's payload, well-
// formed base64 of a mebibyte or more, no content-shape rule applies, so a
// GitHub token glued inside a base64url run is kept with it; a configured
// value inside one is still taken out, the payload still kept apart; and a
// `password=` inside a run leaves it no payload, so it is scrubbed as every
// text is, the password's value taken out (ADR-0073's amendment, code-
// sandbox.md's "What is a payload").
//
// Mutation: shape a payload's content, keep a payload from the values, or
// take a run with a pad within it, and a case goes red.
func TestTheMediaBoundaryIsWhereTheDocsSay(t *testing.T) {
	t.Parallel()
	token := "ghp_" + strings.Repeat("Ab3", 12) + "A"
	const planted, secret = "PlantedUrlValue0123456789abcdefg", "Hunter2Hunter2Hunter2"
	for name, c := range map[string]struct {
		text, kept, gone string
		apart            bool
	}{
		"a token in its alphabet": {shapedPayload(57, mediaMinBytes+8, true, token), token, "", true},
		"a configured value":      {shapedPayload(58, mediaMinBytes+8, true, planted), scrubMarker, planted, true},
		"a password's assignment, base64url": {shapedPayload(59, mediaMinBytes+8, true, "password="+secret), "",
			"", false},
		"a password's assignment, standard": {shapedPayload(60, mediaMinBytes+8, false, "password="+secret), "",
			secret, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			archive := claudeArchive(t, claudeRead(c.text, false))
			s := newScrubber([]string{planted})
			whole, body := mediaTake(t, archiveTar, archive, s, executorkeep.SessionMaxBytes)
			plain := mediaPlain(t, whole)
			if got := body[0] == sandbox.BodySpliced; got != c.apart {
				t.Errorf("the string is kept apart: %v, want %v", got, c.apart)
			}
			if !c.apart && plain != mediaPlain(t, inlineTake(t, archiveTar, archive, s)) {
				t.Error("the string is not scrubbed as a take keeping no media apart scrubs it")
			}
			if c.kept != "" && !strings.Contains(plain, c.kept) || c.gone != "" && strings.Contains(plain, c.gone) {
				t.Errorf("the session keeps %q and not %q: want the reverse", c.gone, c.kept)
			}
		})
	}
}
