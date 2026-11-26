package sandbox

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
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
// each distinct payload's member once, apart; the capture names each, fresh;
// and the text spliced with the members is the archive the take digested, byte
// for byte. A policy that keeps no media apart (a reader below the version)
// asks the take for none, and keeps the archive whole, as today.
//
// Mutation: write a member into the text object, upload a repeat again, name a
// member the capture did not upload, or ask the take for media apart against
// the policy, and a check goes red.
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
			if len(uploads) != 1 || !bytes.Equal(uploads[0], whole) || len(captured[len(captured)-1].Stamp.Media) != 0 {
				t.Errorf("uploaded %d objects naming %d media, want the archive whole and none", len(uploads),
					len(captured[len(captured)-1].Stamp.Media))
			}
		})
	})
}

// A TAKE'S MEDIA SHARE THE ROOM ITS SESSION HOLDS: today's sealed half of a
// take's room holds the text object and every member together, so a text and
// a member that each fit it alone and together do not are refused before a
// byte past it lands, the capture refused and no spool left.
//
// Mutation: hold each spool to the room apart, and the two land past it.
func TestATakesMediaShareTheRoomItsSessionHolds(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		raw := make([]byte, 80<<10)
		rand.NewChaCha8([32]byte{1}).Read(raw)
		whole, body := mediaBody(t, string(raw[:40<<10]), mediaPart+string(raw[40<<10:]), "end")
		rig, _, keep := mediaRig(t, whole, body)
		rig.runner.mu.Lock()
		rig.runner.Session.Archive.Size = 14
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
// session it names is unreadable.
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

// carryRefusing is the executor log refusing a capture that carries media its
// row no longer names, as its apply finds it gone (executordomain's
// ErrCarryGone).
type carryRefusing struct{ *sessionWrites }

func (w carryRefusing) Capture(ctx context.Context, c CaptureRecord) (LogAnswer, error) {
	for _, m := range c.Stamp.Media {
		if m.Carried {
			return LogRefused, fmt.Errorf("media %s of turn %s is no longer named by its row", m.Digest, c.Turn)
		}
	}
	return w.sessionWrites.Capture(ctx, c)
}

// A REFUSED CARRY IS STAMPED LOG REFUSED: a row that moved between the keep's
// read and the record leaves the carry naming media no row names, which the
// log refuses for good, and the stamp says so as every refusal of the log is.
//
// Mutation: read the refusal as unsettled, and the capture stands unresolved.
func TestARefusedCarryIsStampedLogRefused(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		whole, body := mediaBody(t, "one", mediaPart+strings.Repeat("JVBERi0xLjQK", 6<<10), "two")
		rig, writes, _ := mediaRig(t, whole, body)
		if kept := keptFinal(t, rig, "t1"); kept == nil || kept.NotKept != "" {
			t.Fatalf("the first keep came to %+v", kept)
		}
		spooled := secondCapture(t, rig, "t1",
			map[string]executorkeep.TurnRow{"t1": mediaRow("t1", "", rig.get("t1").Kept.Media)})
		rig.coordinator.sessions = carryRefusing{writes}
		kept := rig.coordinator.keepCapture(t.Context(), spooled)
		if kept == nil || !strings.Contains(kept.NotKept, "no longer named") || kept.Loss != executorkeep.LossLogRefused {
			t.Errorf("the refused carry came to %+v, want it refused as the log refused it", kept)
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
		Media: row.Media})
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
	longPast := func(j *saveJob) { j.last = j.last.Add(-3 * saveLedgerHold) }
	saving := func(t *testing.T) (*coordRig, *memory.Fleet, *sessionKeep) {
		t.Helper()
		ledger := memory.NewFleet()
		rig, _, keep := savingRig(t, ledger, newTakerStore())
		keep.policy.MediaApart = true
		rig.runner.mu.Lock()
		rig.runner.Session.Body, rig.runner.Session.Framed = whole, body
		rig.runner.Session.Archive.Size = int64(len(whole))
		rig.runner.mu.Unlock()
		rig.launchKept(t, "t1")
		if out := saveOf(rig, "t1", longPast); out.class != saveLanded {
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
	counted := countedFor(int64(len(whole)))
	t.Run("a save carrying its media", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, ledger, keep := saving(t)
			first := ledgerHeld(ledgerOf(t, ledger, "swe").Entries)
			keep.policy.SeatBudget = first + counted - rig.get("t1").Kept.mediaBytes()
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{
				"t1": mediaRow("t1", "", rig.get("t1").Kept.Media)}}
			if out := saveOf(rig, "t1", longPast); out.class != saveLanded {
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
			rig, ledger, keep := saving(t)
			first := ledgerHeld(ledgerOf(t, ledger, "swe").Entries)
			keep.policy.SeatBudget = first + counted - rig.get("t1").Kept.mediaBytes()
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{
				"t1": mediaRow("t1", "", nil)}}
			out := saveOf(rig, "t1", longPast)
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
