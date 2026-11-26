package sandbox

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/secrets"
)

// mediaPart marks a part of [mediaBody] as a payload kept apart.
const mediaPart = "media:"

// mediaBody is a session as a take keeping its media apart hands it over: each
// part a gzip member, one marked [mediaPart] a payload's own, framed as its
// first (in two pieces, as a take writes one a write at a time) or as a
// repeat; the archive whole, and its framed body.
func mediaBody(t *testing.T, parts ...string) (whole, body []byte) {
	t.Helper()
	var full, framed bytes.Buffer
	framed.WriteByte(BodySpliced)
	firsts := map[[sha256.Size]byte]bool{}
	for _, part := range parts {
		payload, isMedia := strings.CutPrefix(part, mediaPart)
		var member bytes.Buffer
		zw := gzip.NewWriter(&member)
		if _, err := zw.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		full.Write(member.Bytes())
		sum := sha256.Sum256(member.Bytes())
		switch {
		case !isMedia:
			framed.WriteByte(executorkeep.FrameText)
			framed.Write(binary.AppendUvarint(nil, uint64(member.Len())))
			framed.Write(member.Bytes())
		case firsts[sum]:
			framed.WriteByte(executorkeep.FrameMedia)
			framed.Write(sum[:])
		default:
			firsts[sum] = true
			half := member.Len() / 2
			for _, piece := range [][]byte{member.Bytes()[:half], member.Bytes()[half:]} {
				framed.WriteByte(FrameMember)
				framed.Write(binary.AppendUvarint(nil, uint64(len(piece))))
				framed.Write(piece)
			}
			framed.WriteByte(FrameMemberEnd)
			framed.Write(sum[:])
		}
	}
	return full.Bytes(), framed.Bytes()
}

// spliced is the archive a text object of frames and its members, by digest,
// splice back to, as the session store's open does.
func spliced(t *testing.T, text []byte, members map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	for r := bytes.NewReader(text); r.Len() > 0; {
		kind, _ := r.ReadByte()
		switch kind {
		case executorkeep.FrameText:
			n, err := binary.ReadUvarint(r)
			if err != nil || int64(n) > int64(r.Len()) {
				t.Fatalf("a text frame of %d bytes does not read: %v", n, err)
			}
			_, _ = io.CopyN(&out, r, int64(n))
		case executorkeep.FrameMedia:
			digest := make([]byte, executorkeep.MediaDigestBytes)
			_, _ = io.ReadFull(r, digest)
			member, held := members[hex.EncodeToString(digest)]
			if !held {
				t.Fatalf("the text object frames media %x, which nothing uploaded", digest)
			}
			out.Write(member)
		default:
			t.Fatalf("the text object holds a frame of kind %q", kind)
		}
	}
	return out.Bytes()
}

// mediaRig is a keeping rig whose policy keeps media apart, its runner
// handing a take that keeps them apart body, framed, of the archive whole.
//
//fixedwait:bubble
func mediaRig(t *testing.T, whole, body []byte) (*coordRig, *sessionWrites, *sessionKeep) {
	t.Helper()
	rig, writes, keep := keepingRig(t)
	keep.policy.MediaApart = true
	rig.runner.mu.Lock()
	rig.runner.Session.Body, rig.runner.Session.Framed = whole, body
	rig.runner.Session.Archive.Size = int64(len(whole))
	rig.runner.mu.Unlock()
	return rig, writes, keep
}

// keptFinal is a collection's take of turnID's job, ended, kept.
func keptFinal(t *testing.T, rig *coordRig, turnID string) *SessionKept {
	t.Helper()
	spooled, taken := finalTake(t, rig, rig.launchKept(t, turnID), rig.runner)
	if spooled == nil || taken == nil || taken.NotKept != "" {
		t.Fatalf("the take came to %+v, want its session spooled", taken)
	}
	if spooled.bytes() != taken.Bytes {
		t.Errorf("the take says %d bytes were taken, and its spools hold %d", taken.Bytes, spooled.bytes())
	}
	return rig.coordinator.keepCapture(t.Context(), spooled)
}

// uploadedMembers is each upload past the first, the session's, by the digest
// of what it holds.
func uploadedMembers(uploads [][]byte) map[string][]byte {
	members := map[string][]byte{}
	for _, up := range uploads[1:] {
		sum := sha256.Sum256(up)
		members[hex.EncodeToString(sum[:])] = up
	}
	return members
}

// A SESSION WITH LARGE READS IS KEPT, AND RESTORED, BYTE IDENTICAL: the spool
// stores its text object, the frames naming each payload where it stood, and
// each distinct payload's member once, apart; the capture names each, fresh,
// and the archive they restore to, a repeat counted at each place; and the
// text spliced with the members is the archive the take digested. A policy
// keeping no media apart (a reader below the version) asks the take for none,
// and keeps the archive whole, as today, stating no archive size.
//
// Mutation: write a member into the text object, upload a repeat again, name a
// member the capture did not upload, count a repeat or a text frame as nothing,
// or ask the take for media apart against the policy, and a check goes red.
func TestASessionWithLargeReadsIsKeptAndRestoredByteIdentical(t *testing.T) {
	t.Parallel()
	pdf, image := strings.Repeat("JVBERi0xLjQK", 6<<10), strings.Repeat("iVBORw0KGgo", 2<<10)
	whole, body := mediaBody(t, `{"type":"user"}`, mediaPart+pdf, `"},"toolUseResult":{"base64":"`,
		mediaPart+pdf, `"}}`+"\n", mediaPart+image, "\n")
	t.Run("kept apart", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, keep := mediaRig(t, whole, body)
			if kept := keptFinal(t, rig, "t1"); kept == nil || kept.NotKept != "" {
				t.Fatalf("the keep came to %+v, want the session kept", kept)
			}
			if takes := rig.runner.Takes(); len(takes) != 1 || !takes[0].MediaApart {
				t.Fatalf("the take was asked %+v, want its media kept apart", takes)
			}
			uploads := keep.uploads()
			members := uploadedMembers(uploads)
			if len(uploads) != 3 || len(members) != 2 {
				t.Fatalf("uploaded %d objects, want the session and each distinct payload once", len(uploads))
			}
			if got := spliced(t, uploads[0], members); !bytes.Equal(got, whole) {
				t.Errorf("the session restored is %d bytes, not the %d of the archive the take digested",
					len(got), len(whole))
			}
			captured := writes.captured()
			media := captured[len(captured)-1].Stamp.Media
			if len(media) != 2 {
				t.Fatalf("the capture names %d media, want the two", len(media))
			}
			if got := captured[len(captured)-1].Stamp.ArchiveBytes; got != int64(len(whole)) {
				t.Errorf("the capture says its media restore to %d bytes, want the %d of the archive whole", got,
					len(whole))
			}
			for _, m := range media {
				if _, held := members[m.Digest]; !held || m.Carried {
					t.Errorf("the capture names media %+v, want each uploaded, fresh", m)
				}
			}
		})
	})
	t.Run("a policy keeping none apart", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, keep := mediaRig(t, whole, body)
			keep.policy.MediaApart = false
			if kept := keptFinal(t, rig, "t1"); kept == nil || kept.NotKept != "" {
				t.Fatalf("the keep came to %+v, want the session kept", kept)
			}
			uploads := keep.uploads()
			captured := writes.captured()
			if takes := rig.runner.Takes(); len(takes) != 1 || takes[0].MediaApart {
				t.Errorf("the take was asked %+v, want its media kept inline", takes)
			}
			stamp := captured[len(captured)-1].Stamp
			if len(uploads) != 1 || !bytes.Equal(uploads[0], whole) || len(stamp.Media) != 0 || stamp.ArchiveBytes != 0 {
				t.Errorf("uploaded %d objects naming %d media restoring to %d bytes, want the archive whole, "+
					"none and none", len(uploads), len(stamp.Media), stamp.ArchiveBytes)
			}
		})
	})
}

// A CAPTURE'S RESTORED SIZE IS HELD TO ITS BOUND: the stamp states the archive
// its media restore to, which its record carries and a restore's read is
// budgeted by, so a size below zero or past executorkeep.ArchiveMaxBytes is
// refused before the row carries it, as the log refuses it.
//
// Mutation: leave the size unbounded, or bound it short of its bound.
func TestACapturesRestoredSizeIsHeldToItsBound(t *testing.T) {
	t.Parallel()
	for size, refused := range map[int64]bool{-1: true, 0: false, executorkeep.ArchiveMaxBytes: false,
		executorkeep.ArchiveMaxBytes + 1: true} {
		err := SessionCapture{ArchiveBytes: size}.Bounded()
		if got := errors.Is(err, ErrCaptureUnbounded); got != refused {
			t.Errorf("a stamp restoring to %d bytes is refused %t (%v), want %t", size, got, err, refused)
		}
	}
}

// A TAKE'S MEDIA SHARE THE ROOM ITS SESSION HOLDS: the sealed spools of a
// take are held together to its archive counted ([countedFor]), the text
// object and every member, so a text and a member that each fit it alone and
// together do not are refused before a byte past it lands, the capture
// refused and no spool left, though its scrub's drained spool left the take
// room for both.
//
// Mutation: hold each spool to the count apart, and the two land past it.
func TestATakesMediaShareTheRoomItsSessionHolds(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		raw := make([]byte, 80<<10)
		rand.NewChaCha8([32]byte{1}).Read(raw)
		whole, body := mediaBody(t, string(raw[:40<<10]), mediaPart+string(raw[40<<10:]), "end")
		rig, _, keep := mediaRig(t, whole, body)
		rig.runner.mu.Lock()
		rig.runner.Session.Archive.Size, rig.runner.Session.Drains = 14, true
		rig.runner.mu.Unlock()
		spooled, taken := finalTake(t, rig, rig.launchKept(t, "t1"), rig.runner)
		if spooled != nil || taken == nil || taken.NotKept == "" {
			t.Fatalf("a member past the take's room came to %+v, want the capture refused", taken)
		}
		if left := rig.spools(t); len(left) != 0 {
			t.Errorf("the refused take left %d spools", len(left))
		}
		if len(keep.uploads()) != 0 {
			t.Error("the refused take uploaded")
		}
	})
}

// secondCapture is a later capture of turnID's job, taken as the first was
// and spooled at the next place, its turn's row and base read through rows.
func secondCapture(t *testing.T, rig *coordRig, turnID string, rows map[string]executorkeep.TurnRow) *spooledCapture {
	t.Helper()
	rig.coordinator.restore = &keptRestore{rows: rows}
	run := rig.get(turnID)
	taken, err := rig.runner.TakeSession(t.Context(), rig.provider.Box(run.SandboxID),
		SessionTake{MediaApart: true, MaxBytes: executorkeep.SessionMaxBytes})
	if err != nil {
		t.Fatal(err)
	}
	stamp := *run.Kept
	stamp.CaptureSeq, stamp.State, stamp.Media, stamp.Object = stamp.CaptureSeq+1, CaptureBegun, nil, ""
	hold := rig.coordinator.trySpoolRoom(takeRoomBytes)
	spooled, err := rig.coordinator.spool(t.Context(), run, stamp, taken, nil, hold)
	if err != nil {
		t.Fatalf("spooling the second capture: %v", err)
	}
	return spooled
}

// mediaRow is a row of the seat's turn naming media, BaseTurn its base.
func mediaRow(turn, base string, media []CaptureMedia) executorkeep.TurnRow {
	row := executorkeep.TurnRow{Handle: "swe", Turn: turn, BaseTurn: base}
	for _, m := range media {
		key, _ := objstore.ParseKey(m.Object)
		row.Media = append(row.Media, executorkeep.MediaObject{Digest: m.Digest, ContentKey: m.ContentKey,
			Object: objstore.Object{Key: key, Hash: objstore.Hash(m.ObjectHash), Size: m.ObjectSize}})
	}
	return row
}

// A KEEP CARRIES THE MEDIA ITS TURN HOLDS, and uploads the rest: a payload
// the turn's own row names live, or the base that row holds, is named carried,
// its very object, its spool dropped, and never uploaded again; one neither
// names is uploaded fresh; and a row that cannot be read, or is read only past
// the keep's room for it, uploads every one fresh, which is always correct,
// the read costing the keep that room and no more.
//
// Mutation: carry from another turn's row, upload a carried member, keep a
// carried spool, or wait on the row past its room, and a case goes red.
func TestAKeepCarriesMediaItsTurnHolds(t *testing.T) {
	t.Parallel()
	pdf, image := strings.Repeat("JVBERi0xLjQK", 6<<10), strings.Repeat("iVBORw0KGgo", 2<<10)
	whole, body := mediaBody(t, "one", mediaPart+pdf, "two", mediaPart+image, "three")
	for name, tc := range map[string]struct {
		rows    func(first []CaptureMedia) map[string]executorkeep.TurnRow
		late    bool
		carried int
	}{
		"the turn's own row names both": {carried: 2, rows: func(first []CaptureMedia) map[string]executorkeep.TurnRow {
			return map[string]executorkeep.TurnRow{"t1": mediaRow("t1", "", first)}
		}},
		"its held base names one": {carried: 1, rows: func(first []CaptureMedia) map[string]executorkeep.TurnRow {
			return map[string]executorkeep.TurnRow{"t1": mediaRow("t1", "t0", nil), "t0": mediaRow("t0", "", first[:1])}
		}},
		"a base its row no longer holds": {rows: func(first []CaptureMedia) map[string]executorkeep.TurnRow {
			own := mediaRow("t1", "t0", nil)
			own.LaunchSeq = 1
			return map[string]executorkeep.TurnRow{"t1": own, "t0": mediaRow("t0", "", first[:1])}
		}},
		"another turn names them": {rows: func(first []CaptureMedia) map[string]executorkeep.TurnRow {
			return map[string]executorkeep.TurnRow{"t1": mediaRow("t1", "", nil), "t9": mediaRow("t9", "", first)}
		}},
		"the row cannot be read": {rows: func([]CaptureMedia) map[string]executorkeep.TurnRow { return nil }},
		"the row is read only past its room": {late: true, rows: func(first []CaptureMedia) map[string]executorkeep.TurnRow {
			return map[string]executorkeep.TurnRow{"t1": mediaRow("t1", "", first)}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, keep := mediaRig(t, whole, body)
				if kept := keptFinal(t, rig, "t1"); kept == nil || kept.NotKept != "" {
					t.Fatalf("the first keep came to %+v", kept)
				}
				first := rig.get("t1").Kept.Media
				spooled := secondCapture(t, rig, "t1", tc.rows(first))
				if tc.late {
					rig.coordinator.restore = lateRows{rig.coordinator.restore}
				}
				began := time.Now()
				if kept := rig.coordinator.keepCapture(t.Context(), spooled); kept == nil || kept.NotKept != "" {
					t.Fatalf("the second keep came to %+v, want it kept", kept)
				}
				if took := time.Since(began); tc.late && took != keepReadRoom {
					t.Errorf("the keep took %v beside a row read past its room, want the room, %v", took,
						keepReadRoom)
				}
				captured := writes.captured()
				media := captured[len(captured)-1].Stamp.Media
				carried := 0
				for i, m := range media {
					if m.Carried {
						carried++
						if m != (CaptureMedia{Digest: first[i].Digest, Object: first[i].Object,
							ObjectHash: first[i].ObjectHash, ObjectSize: first[i].ObjectSize,
							ContentKey: first[i].ContentKey, Carried: true}) {
							t.Errorf("the capture carries %+v, want the very object %+v", m, first[i])
						}
					}
				}
				if uploads := keep.uploads(); carried != tc.carried || len(uploads) != 3+1+2-carried {
					t.Errorf("the second capture carried %d and the keeps uploaded %d objects, want %d carried "+
						"and the rest uploaded", carried, len(uploads), tc.carried)
				}
				if left := rig.spools(t); len(left) != 0 {
					t.Errorf("the keeps left %d spools", len(left))
				}
			})
		})
	}
}

// A CAPTURE NAMING MEDIA THAT NEVER REACHED THE LOG IS ASKED AGAIN WHOLE: its
// run's row carries its media list beside its object, so the turn's end asks
// the very record its keep sent, naming every member its frames splice, and
// the session the log then names reads back whole. Asked without the list, the
// log would name a text object of frames whose members nothing names, which
// opens to no archive at all.
//
// Mutation: rebuild the record asked again without the stamp's media, and the
// session it names is unreadable; without its archive size, and it names none.
func TestACaptureNamingMediaIsAskedAgainWhole(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		pdf, image := strings.Repeat("JVBERi0xLjQK", 6<<10), strings.Repeat("iVBORw0KGgo", 2<<10)
		whole, body := mediaBody(t, "one", mediaPart+pdf, "two", mediaPart+image, "three")
		rig, writes, keep := mediaRig(t, whole, body)
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		collectHeld(t, rig, "t1")
		if run := rig.get("t1"); run.Capture == nil || run.Capture.State != CaptureUnresolved {
			t.Fatalf("the row holds %+v, want the capture unresolved", run.Capture)
		}

		writes.answer(nil)
		ctx := t.Context()
		if owed, err := rig.coordinator.OweEnd(ctx, "t1", executorkeep.Ending{
			Outcome: executorkeep.OutcomeCompleted}); err != nil || !owed {
			t.Fatalf("OweEnd = %v, %v", owed, err)
		}
		if _, err := rig.coordinator.publishEnd(ctx, rig.get("t1")); err != nil {
			t.Fatalf("publishEnd: %v", err)
		}
		all := writes.captured()
		asked := all[len(all)-1]
		if run := rig.get("t1"); run.Kept == nil || run.Kept.State != CaptureLanded {
			t.Fatalf("the row keeps %+v, want the capture the end's ask landed", run.Kept)
		}
		uploads := keep.uploads()
		members := uploadedMembers(uploads)
		named := map[string][]byte{}
		for _, m := range asked.Stamp.Media {
			named[m.Digest] = members[m.Digest]
		}
		if len(named) != len(members) {
			t.Fatalf("the record asked again names %d media of the %d its session's frames splice", len(named),
				len(members))
		}
		if asked.Stamp.ArchiveBytes != int64(len(whole)) {
			t.Errorf("the record asked again restores to %d bytes, want the %d of the archive taken",
				asked.Stamp.ArchiveBytes, len(whole))
		}
		if got := spliced(t, uploads[0], named); !bytes.Equal(got, whole) {
			t.Error("the session the record asked again names does not read back as the archive taken")
		}
	})
}

// lateRows answers no read of a turn's row before its context ends, as a store
// slow past the keep's room does.
type lateRows struct{ SessionRestore }

func (lateRows) Session(ctx context.Context, _, _ string) (executorkeep.TurnRow, error) {
	<-ctx.Done()
	return executorkeep.TurnRow{}, ctx.Err()
}

// carryRefusing is the executor log refusing, once, a capture that carries
// media its row no longer names, as its decide finds it gone: another capture
// or a save of the turn moved the row since the keep read it.
type carryRefusing struct {
	*sessionWrites
	refused *bool
}

func (w carryRefusing) Capture(ctx context.Context, c CaptureRecord) (LogAnswer, error) {
	for _, m := range c.Stamp.Media {
		if m.Carried && !*w.refused {
			*w.refused = true
			return LogRefused, fmt.Errorf("%w: media %s of turn %s", ErrCarryGone, m.Digest, c.Turn)
		}
	}
	return w.sessionWrites.Capture(ctx, c)
}

// A CARRY THE LOG FINDS GONE IS RECORDED AGAIN, FRESH: a save and a capture
// of one turn race, the loser's carry names an object the winner's record took
// off the row, and the log refuses its record whole; its keep, which kept the
// carried member's spool, uploads it and records again, and the capture lands
// naming its own fresh object.
//
// Mutation: refuse the capture at the log's refusal, or drop a carried spool
// before the record lands, and the session is lost or cannot be uploaded.
func TestACarryTheLogFindsGoneIsRecordedAgainFresh(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		whole, body := mediaBody(t, "one", mediaPart+strings.Repeat("JVBERi0xLjQK", 6<<10), "two")
		rig, writes, keep := mediaRig(t, whole, body)
		if kept := keptFinal(t, rig, "t1"); kept == nil || kept.NotKept != "" {
			t.Fatalf("the first keep came to %+v", kept)
		}
		first := rig.get("t1").Kept.Media
		spooled := secondCapture(t, rig, "t1", map[string]executorkeep.TurnRow{"t1": mediaRow("t1", "", first)})
		refused := false
		rig.coordinator.sessions = carryRefusing{sessionWrites: writes, refused: &refused}
		if kept := rig.coordinator.keepCapture(t.Context(), spooled); kept == nil || kept.NotKept != "" || !refused {
			t.Fatalf("the carry the log found gone came to %+v (refused once: %v), want it recorded fresh", kept,
				refused)
		}
		captured := writes.captured()
		media := captured[len(captured)-1].Stamp.Media
		if len(media) != 1 || media[0].Carried || media[0].Object == first[0].Object {
			t.Errorf("the capture landed naming %+v, want its member uploaded fresh", media)
		}
		if uploads := keep.uploads(); len(uploads) != 4 {
			t.Errorf("the keeps uploaded %d objects, want the first's two and the second's text and member", len(uploads))
		}
	})
}

// A MEDIA RECORD IS REFUSED ONCE A READER FALLS BELOW, AND ONLY THEN: a node
// that joined after the take and reads no capture naming media would retain it,
// or ask it again short, and a take is not made again; so who reads it is read
// again before the record, in a room of its own ([MediaReadersRoom]), and a
// definite answer below refuses the capture as keep_failed, naming the node,
// with nothing sent. An unknown answer (the readers not read in their room,
// the policy unread) is no evidence of such a node, and a policy keeping
// nothing now asks none: the capture lands on its take's answer rather than
// lose media already uploaded.
//
// Mutation: read the policy at the take alone or within the record's budget,
// refuse an unknown answer of any kind, or stamp the refusal as the log's.
func TestAMediaRecordIsRefusedOnceAReaderFallsBelow(t *testing.T) {
	t.Parallel()
	const below = "node seats-1 says capture stamp version 0, below the 1 a capture naming media is asked again at"
	for name, tc := range map[string]struct {
		unread string
		err    error
		off    bool
		lands  bool
	}{
		"a reader below the version":      {},
		"readers unread in their room":    {unread: "the position register could not be read", lands: true},
		"the policy unread":               {err: errors.New("the activation pointer could not be read"), lands: true},
		"a policy keeping no session now": {off: true, lands: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				whole, body := mediaBody(t, "one", mediaPart+strings.Repeat("JVBERi0xLjQK", 6<<10), "two")
				rig, writes, keep := mediaRig(t, whole, body)
				spooled, taken := finalTake(t, rig, rig.launchKept(t, "t1"), rig.runner)
				if spooled == nil || len(spooled.media) != 1 {
					t.Fatalf("the take came to %+v, want its member spooled apart", taken)
				}
				var room time.Duration
				keep.mu.Lock()
				keep.policy.MediaApart, keep.policy.MediaUnread, keep.policy.MediaBelow = false, tc.unread, ""
				if tc.unread == "" && tc.err == nil && !tc.off {
					keep.policy.MediaBelow = below
				}
				keep.policyErr, keep.policy.Keep = tc.err, !tc.off
				keep.policyHook = func(ctx context.Context) {
					if by, ok := ctx.Deadline(); ok {
						room = time.Until(by)
					}
				}
				keep.mu.Unlock()
				kept := rig.coordinator.keepCapture(t.Context(), spooled)
				captured, stamp := writes.captured(), rig.get("t1").Capture
				switch {
				case room != MediaReadersRoom:
					t.Errorf("who reads the capture's media was read again in %s, want a room of its own, %s", room,
						MediaReadersRoom)
				case tc.lands && (kept == nil || kept.NotKept != "" || len(captured) != 1):
					t.Errorf("the keep came to %+v after %d records, want the capture landed on its take's "+
						"answer", kept, len(captured))
				case !tc.lands && (kept == nil || !strings.Contains(kept.NotKept, below) || len(captured) != 0 ||
					stamp == nil || stamp.Loss != executorkeep.LossKeepFailed):
					t.Errorf("the keep came to %+v after %d records, stamped %+v, want the media record refused "+
						"unsent as keep_failed, naming the node below", kept, len(captured), stamp)
				}
			})
		})
	}
}

// A KEEP STAMPS ITS MEDIA COUNT ON ITS BEGUN STAMP FIRST: before its upload,
// the capture's begun stamp says how many media objects its spool cut apart,
// which a relaunch prices its wait by ([captureRestFor]), and its outcome
// says the same.
//
// Mutation: stamp no count, stamp it only with the outcome, or let the row
// refuse a begun take's count as it grows.
func TestAKeepStampsItsMediaCountOnItsBegunStampFirst(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		whole, body := mediaBody(t, "one", mediaPart+strings.Repeat("JVBERi0xLjQK", 6<<10), "two")
		rig, _, keep := mediaRig(t, whole, body)
		keep.gate = make(chan struct{})
		spooled, _ := finalTake(t, rig, rig.launchKept(t, "t1"), rig.runner)
		if spooled == nil || len(spooled.media) != 1 {
			t.Fatalf("the take spooled %+v, want its member apart", spooled)
		}
		rig.coordinator.keepLater(t.Context(), spooled)
		synctest.Wait()
		if got := rig.get("t1").Capture; got == nil || got.State != CaptureBegun || got.MediaCount != 1 {
			t.Errorf("before its upload the row holds %+v, want its begun stamp counting its one media object", got)
		}
		close(keep.gate)
		rig.awaitKeeps()
		if got := rig.get("t1").Capture; got == nil || got.State != CaptureLanded || got.MediaCount != 1 {
			t.Errorf("the keep left %+v, want it landed counting its one media object", got)
		}
	})
}

// A STAMP A NEWER BUILD EXTENDED IS SETTLED OFF THE LOG: a capture stamped with
// keys this build does not know (a media list, to a build before it) is one
// whose record it cannot rebuild, so it is never asked again short, nor refused
// unasked while its record may have landed. The turn's row on the executor log
// decides: LANDED where it names the capture's place; REFUSED, naming the keys,
// where it does not and the object is past the window the log records one
// within; otherwise left unresolved for the build that wrote it.
//
// Mutation: refuse the stamp unasked, ask it again whatever it carries, or
// settle it refused within the window, and a case goes red.
func TestAStampANewerBuildExtendedIsSettledOffTheLog(t *testing.T) {
	t.Parallel()
	stale := objstore.RecordWithin + 2*objstore.AssumedClockSkew + time.Minute
	for name, tc := range map[string]struct {
		minted time.Duration
		landed bool
		want   CaptureState
	}{
		"its record landed":                    {landed: true, want: CaptureLanded},
		"named nowhere, past the log's window": {minted: -stale, want: CaptureRefused},
		"named nowhere, within the window":     {want: CaptureUnresolved},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, restore, kept := keptTurn(t)
				writes := rig.coordinator.sessions.(*sessionWrites)
				sent := len(writes.captured())
				stamp := kept
				stamp.CaptureSeq, stamp.State, stamp.At = kept.CaptureSeq+1, CaptureUnresolved, rig.now
				stamp.Object = objstore.KeyAt(rig.now.Add(tc.minted)).String()
				stamp.Extra = map[string]json.RawMessage{"media_next": json.RawMessage(`[]`)}
				if took, err := rig.pending.StampCapture(t.Context(), "t1", stamp); err != nil || !took {
					t.Fatalf("stamping the unanswered capture = %v, %v", took, err)
				}
				if tc.landed {
					onTheLog(restore, landedRow(t, stamp))
				}
				settled := rig.coordinator.settleCapture(t.Context(), rig.get("t1"), stamp)
				newest := rig.get("t1").Capture
				switch {
				case settled.State != tc.want || newest == nil || newest.State != tc.want:
					t.Errorf("the stamp settled %s and the row holds %+v, want %s", settled.State, newest, tc.want)
				case tc.want == CaptureLanded && (newest.ContentKey != "k2" || newest.ContextTokens != 42_000):
					t.Errorf("the landed stamp holds %+v, want it written from the turn's row", newest)
				case tc.want == CaptureRefused && (!strings.Contains(newest.Reason, "media_next") ||
					newest.Loss != executorkeep.LossLogRefused):
					t.Errorf("the refused stamp says %q (%s), want the key it does not know named", newest.Reason,
						newest.Loss)
				}
				if captured := writes.captured(); len(captured) != sent {
					t.Errorf("sent %d records, want none", len(captured)-sent)
				}
			})
		})
	}
}

// A CAPTURE SETTLED OFF THE LOG KEEPS ITS MEDIA: a keep whose taker stopped
// after its record landed is settled from the turn's row, and stamped kept
// with every media object that row names and the archive they restore to, so
// its seat's budget and its saves count them and the stamp is its record's.
//
// Mutation: settle the object alone, or the media without their archive size,
// and the stamp keeps neither.
func TestACaptureSettledOffTheLogKeepsItsMedia(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		store := newTakerStore()
		rig, restore, kept := keptTurnBy(t, taking(thisProcess, store))
		rig.relaunchWithin(2 * time.Second)
		lease := claimTaker(t, store, takingProcess)
		begun := takenBy(t, rig, kept, takingProcess, lease.Epoch)
		row := landedRow(t, begun)
		row.Media = []executorkeep.MediaObject{{Digest: strings.Repeat("b", 64), ContentKey: "k1",
			Object: objstore.Object{Key: objstore.KeyAt(rig.now), Hash: objstore.Hash(strings.Repeat("c", 64)),
				Size: 9}}}
		row.ArchiveBytes = 5 << 20
		onTheLog(restore, row)
		if _, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, "")); why != "" {
			t.Fatalf("the relaunch decided %q, want the capture the log took", why)
		}
		got := rig.get("t1").Kept
		if got == nil || len(got.Media) != 1 || got.Media[0] != captureMediaOf(row.Media[0], false) ||
			got.mediaBytes() != 9 || got.ArchiveBytes != row.ArchiveBytes {
			t.Errorf("the row keeps %+v, want the capture settled with the media its row names", got)
		}
	})
}

// storeRestore is the executor log's rows and the session store's open, as
// the engine's restore wires them, over an in-memory object store.
type storeRestore struct {
	store *executordomain.SessionStore
	rows  map[string]executorkeep.TurnRow
}

func (s storeRestore) Session(_ context.Context, _, turn string) (executorkeep.TurnRow, error) {
	row, held := s.rows[turn]
	if !held {
		return executorkeep.TurnRow{}, ErrTurnRowNotFound
	}
	return row, nil
}

func (s storeRestore) Open(ctx context.Context, row executorkeep.TurnRow) (io.ReadCloser, error) {
	return s.store.Open(ctx, executordomain.Session{Handle: row.Handle, Turn: row.Turn, Object: row.Object,
		Media: row.Media, ArchiveBytes: row.ArchiveBytes})
}

// A MISSING MEDIA OBJECT RESTORES NOTHING: a session whose text the store
// still holds and one of whose media it does not is no session, so the launch
// starts fresh, its loss restore_failed and its sentence naming the media,
// never a session with a hole where the payload stood.
//
// Mutation: splice a placeholder for a missing member, and the launch carries
// a corrupt session.
func TestAMissingMediaObjectRestoresNothing(t *testing.T) {
	t.Parallel()
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
	member := []byte("a member never uploaded")
	sum := sha256.Sum256(member)
	text := append([]byte{executorkeep.FrameText, 1, 'x', executorkeep.FrameMedia}, sum[:]...)
	var sealed bytes.Buffer
	turn := uuid.NewString()
	spool, err := store.Seal("swe", turn, bytes.NewReader(text), &sealed)
	if err != nil {
		t.Fatal(err)
	}
	put, err := store.Upload(t.Context(), spool, &sealed)
	if err != nil {
		t.Fatal(err)
	}
	digest := hex.EncodeToString(sum[:])
	kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, Final: true, Through: new(0),
		Object: put.Object.Key.String(), ObjectHash: string(put.Object.Hash), ObjectSize: put.Object.Size,
		ContentKey: put.ContentKey, Home: "/boxes/old/home", Media: []CaptureMedia{{Digest: digest,
			Object: objects.Mint().String(), ObjectHash: strings.Repeat("c", 64), ObjectSize: 64, ContentKey: "k1"}}}
	row := keptRow(t, kept)
	row.Turn, row.Media = turn, mediaRow(turn, "", kept.Media).Media
	rig := newCoordRig(t)
	rig.coordinator.spoolDir = t.TempDir()
	rig.runner.Carries = true
	rig.coordinator.restore = storeRestore{store: store, rows: map[string]executorkeep.TurnRow{turn: row}}
	opened := PendingRun{TurnID: turn, AgentHandle: "swe", LaunchSeq: 2, Capture: &kept, Kept: &kept}
	req := continuingKept(kept)
	req.Turn.TurnID = turn
	got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
		rig.runner, false, false)
	t.Cleanup(got.release)
	if got.kind != BriefFresh || got.carried != nil {
		t.Fatalf("a session missing its media hands brief %s carrying %+v, want the fresh brief and nothing",
			got.kind, got.carried)
	}
	if lost := got.notContinued; lost == nil || lost.Loss != executorkeep.LossRestoreFailed ||
		!strings.Contains(lost.Detail, digest[:16]) {
		t.Errorf("the launch says %+v, want restore_failed naming the media", lost)
	}
}

// mediaSaved is a saving rig whose policy keeps media apart, its runner
// handing body, framed, of the archive whole, and whose turn t1 landed one save
// charged at its text and its member.
//
//fixedwait:bubble
func mediaSaved(t *testing.T, whole, body []byte) (*coordRig, *memory.Fleet, *sessionKeep) {
	t.Helper()
	ledger := memory.NewFleet()
	rig, _, keep := savingRig(t, ledger, newTakerStore())
	keep.policy.MediaApart = true
	rig.runner.mu.Lock()
	rig.runner.Session.Body, rig.runner.Session.Framed = whole, body
	rig.runner.Session.Archive.Size = int64(len(whole))
	rig.runner.mu.Unlock()
	rig.launchKept(t, "t1")
	if out := saveOf(rig, "t1", mediaLongPast); out.class != saveLanded {
		t.Fatalf("the first save came to %+v, want it landed", out)
	}
	uploads := keep.uploads()
	if got := ledgerOf(t, ledger, "swe"); len(got.Entries) != 1 ||
		got.Entries[0].Bytes != int64(len(uploads[0])+len(uploads[1])) {
		t.Fatalf("the ledger holds %+v, want the save at its text and its member, %d", got.Entries,
			len(uploads[0])+len(uploads[1]))
	}
	return rig, ledger, keep
}

// mediaLongPast paces a save long past its last, so its charge alone decides.
func mediaLongPast(j *saveJob) { j.last = j.last.Add(-3 * saveLedgerHold) }

// A SAVE IS CHARGED WHAT IT UPLOADS: before its take, its archive less the
// media its turn's newest kept capture holds, which its keep will most likely
// carry; once its keep knows its carries, the entry is amended to its sealed
// text and its fresh media. A save carrying its media fits a seat the archive
// whole would not, and one whose upload passes the seat's budget at the amend
// is refused there, its charge left as it was and nothing uploaded.
//
// Mutation: charge the archive whole, skip the amend, or amend past the budget,
// and a case goes red.
func TestASaveIsChargedWhatItUploads(t *testing.T) {
	t.Parallel()
	raw := make([]byte, 640<<10)
	rand.NewChaCha8([32]byte{2}).Read(raw)
	whole, body := mediaBody(t, "text", mediaPart+string(raw), "end")
	counted := countedFor(int64(len(whole)), textBound.sealed)
	t.Run("a save carrying its media", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, ledger, keep := mediaSaved(t, whole, body)
			first := ledgerHeld(ledgerOf(t, ledger, "swe").Entries)
			keep.policy.SeatBudget = first + counted - rig.get("t1").Kept.mediaBytes()
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{
				"t1": mediaRow("t1", "", rig.get("t1").Kept.Media)}}
			if out := saveOf(rig, "t1", mediaLongPast); out.class != saveLanded {
				t.Fatalf("a save carrying its media came to %+v, want it landed within %d", out,
					keep.policy.SeatBudget)
			}
			uploads := keep.uploads()
			if got := ledgerOf(t, ledger, "swe"); len(uploads) != 3 || len(got.Entries) != 2 ||
				got.Entries[1].Bytes != int64(len(uploads[2])) {
				t.Errorf("the second save uploaded %d and the ledger holds %+v, want its text alone, charged "+
					"at it", len(uploads)-2, got.Entries)
			}
		})
	})
	t.Run("a save whose upload passes the budget at its amend", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, ledger, keep := mediaSaved(t, whole, body)
			first := ledgerHeld(ledgerOf(t, ledger, "swe").Entries)
			keep.policy.SeatBudget = first + counted - rig.get("t1").Kept.mediaBytes()
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{
				"t1": mediaRow("t1", "", nil)}}
			out := saveOf(rig, "t1", mediaLongPast)
			if out.class != saveFailed || !strings.Contains(out.why, "max_bytes_per_seat") {
				t.Fatalf("a save uploading its media past the budget came to %+v, want it refused at its amend",
					out)
			}
			if got := ledgerOf(t, ledger, "swe"); len(keep.uploads()) != 2 || len(got.Entries) != 2 ||
				got.Entries[1].Bytes != max(counted-rig.get("t1").Kept.mediaBytes(), saveMarginFloor) {
				t.Errorf("the refused save uploaded %d and the ledger holds %+v, want nothing uploaded and its "+
					"estimate left", len(keep.uploads())-2, got.Entries)
			}
		})
	})
}
