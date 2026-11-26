package executordomain

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// payload is a media member named by seed, held in an object of size bytes,
// as a capture names it fresh.
func payload(seed string, size int64) CaptureMedia {
	sum := sha256.Sum256([]byte(seed))
	return CaptureMedia{Digest: hex.EncodeToString(sum[:]), Object: keyAt(monday).String(),
		ObjectHash: string(objstore.HashOf([]byte("sealed/" + seed))), ObjectSize: size, ContentKey: "k1"}
}

// carry is m as a later capture carries it.
func carry(m CaptureMedia) CaptureMedia {
	m.Carried = true
	return m
}

// naming is c naming media.
func naming(c Capture, media ...CaptureMedia) Capture {
	c.Media = media
	return c
}

// newMedia is m as a writer's input names it.
func newMedia(t *testing.T, m CaptureMedia) NewMedia {
	t.Helper()
	o, err := parsedObject(m.Object, m.ObjectHash, m.ObjectSize)
	if err != nil {
		t.Fatal(err)
	}
	return NewMedia{MediaObject: executorkeep.MediaObject{Digest: m.Digest, Object: o, ContentKey: m.ContentKey},
		Carried: m.Carried}
}

// media is the media turn's row names, in digest order.
func (e *estate) media(handle, turn string) []executorkeep.MediaObject {
	e.t.Helper()
	var out []executorkeep.MediaObject
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		var err error
		out, err = mediaOf(e.t.Context(), tx, handle, turn)
		return err
	}); err != nil {
		e.t.Fatalf("read %s's media of turn %s: %v", handle, turn, err)
	}
	return out
}

// orphanedMedia is every media row whose turn's row names no object: what no
// apply may leave, since the collector keeps an object while any row names it.
func (e *estate) orphanedMedia() []string {
	e.t.Helper()
	type named struct{ handle, turn, digest string }
	var all []named
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(e.t.Context(), `SELECT agent_handle, turn_id, digest FROM executor_session_media`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n named
			if err := rows.Scan(&n.handle, &n.turn, &n.digest); err != nil {
				_ = rows.Close()
				return err
			}
			all = append(all, n)
		}
		return errors.Join(rows.Err(), rows.Close())
	}); err != nil {
		e.t.Fatalf("read the media rows: %v", err)
	}
	var out []string
	for _, n := range all {
		if s, found := e.session(n.handle, n.turn); !found || !s.HasObject() {
			out = append(out, fmt.Sprintf("%s's turn %s names media %s and no object", n.handle, n.turn, n.digest))
		}
	}
	return out
}

// digestsOf is the digests of media, in their order.
func digestsOf(media []executorkeep.MediaObject) []string {
	out := make([]string, 0, len(media))
	for _, m := range media {
		out = append(out, m.Digest)
	}
	return out
}

// A CAPTURE NAMES EACH MEDIA ONCE, and its record names exactly the rows its
// apply writes. The writer refuses a set no row could hold — a digest or an
// object twice, the session's own object, more than MediaMaxCount, a digest in
// any other spelling, or the whole past KeptSealedMaxBytes — and the
// declaration's reader answers every media row the shipped applier wrote,
// owned by seat, turn and digest, carried ones included.
//
// Mutation: drop the digest-twice check from checkMedia, answer only the
// fresh media from sessionMediaRecordReferences, or spell the owner turn-first,
// and a check goes red.
func TestACaptureNamesEachMediaOnce(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	turn := newTurn()
	a, b := payload("a", 3<<20), payload("b", 2<<20)
	base := newCapture("ana", "chat:room", turn, 1, 1, 1<<20, monday)
	pastCount := make([]CaptureMedia, executorkeep.MediaMaxCount+1)
	for i := range pastCount {
		pastCount[i] = payload(fmt.Sprint(i), 1)
	}
	for name, media := range map[string][]CaptureMedia{
		"a digest twice":                 {a, func() CaptureMedia { m := payload("c", 1); m.Digest = a.Digest; return m }()},
		"an object twice":                {a, func() CaptureMedia { m := payload("c", 1); m.Object = a.Object; return m }()},
		"the session's own object":       {func() CaptureMedia { m := payload("c", 1); m.Object = base.Object.Key.String(); return m }()},
		"a digest spelled in upper case": {func() CaptureMedia { m := payload("c", 1); m.Digest = strings.ToUpper(m.Digest); return m }()},
		"a whole past the kept bound":    {payload("c", executorkeep.KeptSealedMaxBytes)},
		"past the count":                 pastCount,
	} {
		n := base
		for _, m := range media {
			n.Media = append(n.Media, newMedia(t, m))
		}
		if err := n.validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("a capture naming %s: %v, want ErrInvalid", name, err)
		}
	}
	atCap := base
	for i := range executorkeep.MediaMaxCount {
		atCap.Media = append(atCap.Media, newMedia(t, payload(fmt.Sprint(i), 1)))
	}
	if err := atCap.validate(); err != nil {
		t.Errorf("a capture naming MediaMaxCount media was refused: %v", err)
	}

	first := naming(capturing("ana", "chat:room", turn, 1, 1, 100, budget), a, b)
	e.capture(first)
	later := naming(capturing("ana", "chat:room", turn, 1, 2, 100, budget), carry(a))
	encoded := e.encode(ConversationSubject("ana", "chat:room", turn), OpCapture, seatScope("ana"), later)
	if env, err := DecodeEnvelope(encoded); err != nil || env.V != MediaVersion {
		t.Fatalf("a capture naming media is stamped %d (%v), want %d", env.V, err, MediaVersion)
	}
	if env, err := DecodeEnvelope(e.encode(ConversationSubject("ana", "chat:room", turn), OpCapture,
		seatScope("ana"), capturing("ana", "chat:room", turn, 1, 3, 100, budget))); err != nil || env.V >= MediaVersion {
		t.Errorf("a capture naming no media is stamped %d (%v), want below %d: an older build reads it",
			env.V, err, MediaVersion)
	}
	e.applyEncoded(OpCapture, encoded)
	refs, err := SessionMediaReferences.Records(encoded)
	if err != nil {
		t.Fatal(err)
	}
	rows := e.media("ana", turn)
	if len(refs) != len(rows) || len(rows) != 1 {
		t.Fatalf("the record names %d media rows and its apply left %d (%v), want the one it carries",
			len(refs), len(rows), digestsOf(rows))
	}
	if want := objstore.JoinOwner("ana", turn, a.Digest); refs[0].NamedBy != want || !refs[0].Names ||
		refs[0].Object != rows[0].Object {
		t.Errorf("the record names %+v, and its apply wrote %s naming %+v", refs[0], want, rows[0].Object)
	}
}

// A CARRY NAMES ONLY AN OBJECT ITS TURN OR ITS BASE NAMES, LIVE (ADR-0027's
// amendment "a session's media, carried"): a capture may name, without
// uploading it, a media object the turn's own row names, or the base the
// turn's begin holds names, as the very object. Anything else — another
// conversation's, the base's once the turn's first capture lifted the hold, or
// the same digest on another object — is refused at the decide (ErrCarryGone)
// and writes nothing at the apply, so no key is named that no live row named.
//
// Mutation: carry from any row of the seat by digest, skip the base, or skip
// the object's equality in mediaNamed, and a case goes red.
func TestACarryNamesOnlyAnObjectItsTurnOrBaseNamesLive(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	base, holder, stranger := newTurn(), newTurn(), newTurn()
	pdf, image := payload("pdf", 40<<20), payload("image", 20<<20)
	e.capture(naming(capturing("ana", "chat:room", base, 1, 1, 100, budget), pdf, image))
	e.end(ending("ana", "chat:room", base, "", completed))
	e.begin(beginning(e.head("ana", "chat:room"), "ana", "chat:room", holder, ""))

	decided := func(c Capture) error {
		n := newCapture(c.Handle, c.ConversationKey, c.Turn, c.LaunchSeq, c.CaptureSeq, c.ObjectSize, monday)
		for _, m := range c.Media {
			n.Media = append(n.Media, newMedia(t, m))
		}
		return e.decide(n, monday)
	}
	elsewhere := naming(capturing("ana", "chat:other", stranger, 1, 1, 100, budget), carry(pdf))
	if err := decided(elsewhere); !errors.Is(err, ErrCarryGone) {
		t.Errorf("another conversation's carry of the base's media: %v, want ErrCarryGone", err)
	}
	if _, n := e.capture(elsewhere); n != 0 {
		t.Errorf("another conversation's carry wrote %d rows", n)
	}
	moved := carry(pdf)
	moved.Object = keyAt(monday).String()
	if err := decided(naming(capturing("ana", "chat:room", holder, 1, 1, 100, budget), moved)); !errors.Is(err,
		ErrCarryGone) {
		t.Errorf("a carry of the base's digest on another object: %v, want ErrCarryGone", err)
	}

	first := naming(capturing("ana", "chat:room", holder, 1, 1, 100, budget), carry(pdf))
	if err := decided(first); err != nil {
		t.Fatalf("the holder's first capture carrying its base's media was refused: %v", err)
	}
	if _, n := e.capture(first); n == 0 {
		t.Fatal("the holder's first capture carrying its base's media wrote nothing")
	}
	if got := digestsOf(e.media("ana", holder)); !slices.Equal(got, []string{pdf.Digest}) {
		t.Fatalf("the holder's row names media %v, want the one it carried", got)
	}
	second := naming(capturing("ana", "chat:room", holder, 1, 2, 100, budget), carry(pdf))
	if err := decided(second); err != nil {
		t.Errorf("a later capture carrying its own row's media was refused: %v", err)
	}
	lifted := naming(capturing("ana", "chat:room", holder, 1, 2, 100, budget), carry(pdf), carry(image))
	if err := decided(lifted); !errors.Is(err, ErrCarryGone) {
		t.Errorf("a carry from the base after the first capture lifted its hold: %v, want ErrCarryGone", err)
	}
	if _, n := e.capture(lifted); n != 0 {
		t.Errorf("a carry from a base no longer held wrote %d rows", n)
	}
	if s, _ := e.session("ana", holder); s.CaptureSeq != 1 {
		t.Errorf("the refused carry's capture landed: the row holds capture %d", s.CaptureSeq)
	}
}

// A CARRY OF MEDIA NO ROW HOLDS ANY MORE IS REFUSED: a retirement takes a
// spent session off every ended row of its conversation, held ones included,
// and its media with it, so a holder's capture carrying what it read off that
// base finds nothing and names nothing, which the collector may already have
// deleted. Its keep uploads the media fresh instead.
//
// Mutation: answer a carry from the base's row whatever it names, or keep the
// media rows of a retired row, and the capture names a key no live row named.
func TestACarryOfMediaNoRowHoldsIsRefused(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	base, spender, holder := newTurn(), newTurn(), newTurn()
	pdf := payload("pdf", 40<<20)
	e.capture(naming(capturingSession("ana", "chat:room", base, "s-1", 1), pdf))
	e.end(ending("ana", "chat:room", base, "", completed))
	read := e.head("ana", "chat:room")
	e.begin(beginning(read, "ana", "chat:room", spender, ""))
	e.begin(beginning(read, "ana", "chat:room", holder, ""))
	e.end(ending("ana", "chat:room", spender, "", exhausted("s-1")))
	if got := e.media("ana", base); len(got) != 0 {
		t.Fatalf("the retired row still names media %v", digestsOf(got))
	}
	c := naming(capturing("ana", "chat:room", holder, 1, 1, 100, budget), carry(pdf))
	n := newCapture("ana", "chat:room", holder, 1, 1, 100, monday)
	n.Media = []NewMedia{newMedia(t, carry(pdf))}
	if err := e.decide(n, monday); !errors.Is(err, ErrCarryGone) {
		t.Errorf("a carry from a retired base: %v, want ErrCarryGone", err)
	}
	if _, written := e.capture(c); written != 0 {
		t.Errorf("a carry from a retired base wrote %d rows", written)
	}
}

// NO MEDIA OUTLIVES ITS SESSION: every record that takes a turn's object off
// its row, or deletes the row, takes the row's media with it, in the same
// apply, so the collector deletes a media object exactly when no live session
// names it. One case per such record; after each, no media row's turn lacks an
// object, and the turn the record reached names none.
//
// Mutation: drop dropMedia from any one path (the budget's release, a stale
// end, a retirement, a dedupe, an absorption), turnMediaStatement from
// removeTurn, seatMediaDeleteStatement from removeSeat, or the replacement in
// nameMedia, and its case goes red.
func TestNoMediaOutlivesItsSession(t *testing.T) {
	t.Parallel()
	pdf, image := payload("pdf", 30), payload("image", 20)
	for _, tc := range []struct {
		name string
		// run applies the records, and answers the turn whose media must go
		// and the digests it must still name.
		run func(e *estate) (string, []string)
	}{
		{"a later capture replaces the set", func(e *estate) (string, []string) {
			turn := newTurn()
			e.capture(naming(capturing("ana", "chat:room", turn, 1, 1, 10, budget), pdf, image))
			e.capture(naming(capturing("ana", "chat:room", turn, 1, 2, 10, budget), carry(pdf)))
			return turn, []string{pdf.Digest}
		}},
		{"the seat's budget releases an ended session", func(e *estate) (string, []string) {
			old := newTurn()
			e.capture(naming(capturing("ana", "chat:a", old, 1, 1, 10, 100), pdf, image))
			e.end(ending("ana", "chat:a", old, "", completed))
			e.capture(capturing("ana", "chat:b", newTurn(), 1, 1, 60, 100))
			return old, nil
		}},
		{"an end releases its stale session", func(e *estate) (string, []string) {
			turn := newTurn()
			e.capture(naming(capturing("ana", "chat:room", turn, 1, 1, 10, budget), pdf))
			e.end(ending("ana", "chat:room", turn, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
				Unkept: &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: "refused"}}))
			return turn, nil
		}},
		{"an end retires a spent session", func(e *estate) (string, []string) {
			spent, spender := newTurn(), newTurn()
			e.capture(naming(capturingSession("ana", "chat:room", spent, "s-1", 1), pdf))
			e.end(ending("ana", "chat:room", spent, "", completed))
			e.begin(beginning(e.head("ana", "chat:room"), "ana", "chat:room", spender, ""))
			e.end(ending("ana", "chat:room", spender, "", exhausted("s-1")))
			return spent, nil
		}},
		{"an end dedupes an older attempt", func(e *estate) (string, []string) {
			older, retry := newTurn(), newTurn()
			e.capture(naming(capturing("ana", "task:ENG-7", older, 1, 1, 10, budget), pdf))
			e.end(ending("ana", "task:ENG-7", older, "work-1", completed))
			e.capture(capturing("ana", "task:ENG-7", retry, 1, 1, 10, budget))
			e.end(ending("ana", "task:ENG-7", retry, "work-1", completed))
			return older, nil
		}},
		{"an end absorbs what its read handed out", func(e *estate) (string, []string) {
			shown, reader := newTurn(), newTurn()
			e.capture(naming(capturing("ana", "chat:room", shown, 1, 1, 10, budget), pdf))
			e.end(ending("ana", "chat:room", shown, "", completed))
			e.capture(launching(e.head("ana", "chat:room"), capturing("ana", "chat:room", reader, 1, 1, 10, budget)))
			e.end(ending("ana", "chat:room", reader, "", completed))
			return shown, nil
		}},
		{"the retention deletes a turn", func(e *estate) (string, []string) {
			turn := newTurn()
			e.capture(naming(capturing("ana", "chat:room", turn, 1, 1, 10, budget), pdf))
			ended, _ := e.end(ending("ana", "chat:room", turn, "", completed))
			e.apply(ConversationSubject("ana", "chat:room", turn), OpDelete, ScopeSet{Subject: true},
				Delete{V: 1, Handle: "ana", Turn: turn, ConversationKey: "chat:room", Cutoff: stamped(ended.Seq)})
			return turn, nil
		}},
		{"the seat is removed", func(e *estate) (string, []string) {
			turn := newTurn()
			e.capture(naming(capturing("ana", "chat:room", turn, 1, 1, 10, budget), pdf))
			e.removeSeat("ana")
			return turn, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEstate(t)
			// ANOTHER SEAT'S MEDIA, which no record here may reach.
			bystander := newTurn()
			e.capture(naming(capturing("ben", "chat:room", bystander, 1, 1, 10, budget), pdf))
			turn, want := tc.run(e)
			if got := digestsOf(e.media("ana", turn)); !slices.Equal(got, want) {
				t.Errorf("the turn names media %v, want %v", got, want)
			}
			if got := e.orphanedMedia(); len(got) != 0 {
				t.Errorf("media outlived its session:\n%s", strings.Join(got, "\n"))
			}
			if got := e.media("ben", bystander); len(got) != 1 {
				t.Errorf("another seat's media went too: %v", digestsOf(got))
			}
		})
	}
}

// THE SEAT'S BUDGET COUNTS SHARED MEDIA ONCE: a turn carrying its base's media
// names the very object its base's row names, which the store holds once, so a
// seat holds the base and its continuation beside each other where counting
// the object twice would release the base for room the seat already has.
//
// Mutation: count each row's media in seatMedia rather than each object, and
// the base is released.
func TestTheSeatBudgetCountsSharedMediaOnce(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	const tight = 100
	base, holder := newTurn(), newTurn()
	pdf := payload("pdf", 60)
	e.capture(naming(capturing("ana", "chat:room", base, 1, 1, 10, tight), pdf))
	e.end(ending("ana", "chat:room", base, "", completed))
	e.begin(beginning(e.head("ana", "chat:room"), "ana", "chat:room", holder, ""))
	if _, n := e.capture(naming(capturing("ana", "chat:room", holder, 1, 1, 20, tight), carry(pdf))); n == 0 {
		t.Fatal("the continuation carrying its base's media wrote nothing")
	}
	if s, _ := e.session("ana", base); !s.HasObject() || len(e.media("ana", base)) != 1 {
		t.Errorf("the base was released for room the seat had: 10 + 20 + one 60-byte object fit %d", tight)
	}
	n := newCapture("ana", "chat:other", newTurn(), 1, 1, 11, monday)
	n.SeatBudget = tight
	if err := e.decide(n, monday); err != nil {
		t.Errorf("a capture the seat holds once the base is released was refused: %v", err)
	}
}

// A RELEASE FREES ONLY THE MEDIA NO OTHER ROW NAMES: releasing an ended turn
// frees its object and each media object no remaining row, nor the capture
// making room, still names, so the budget walks on to the next ending while a
// shared object stays named. Two continuations of one base, each carrying its
// media and each ended, share the object here. Every node walks alike: the
// media are counted per object, whatever order a map hands them in.
//
// Mutation: free every media object of a released row, and the walk stops
// after the first continuation, leaving the seat past its budget.
func TestAReleaseFreesOnlyMediaNoOtherRowNames(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	const tight = 100
	base, first, second, next := newTurn(), newTurn(), newTurn(), newTurn()
	pdf := payload("pdf", 50)
	e.capture(naming(capturing("ana", "chat:room", base, 1, 1, 10, tight), pdf))
	e.end(ending("ana", "chat:room", base, "", completed))
	read := e.head("ana", "chat:room")
	e.begin(beginning(read, "ana", "chat:room", first, ""))
	e.begin(beginning(read, "ana", "chat:room", second, ""))
	// THE FIRST ENDS WHILE THE SECOND STILL HOLDS THE BASE, which stays; the
	// second's end absorbs it.
	e.capture(naming(capturing("ana", "chat:room", first, 1, 1, 10, tight), carry(pdf)))
	e.end(ending("ana", "chat:room", first, "", completed))
	e.capture(naming(capturing("ana", "chat:room", second, 1, 1, 10, tight), carry(pdf)))
	e.end(ending("ana", "chat:room", second, "", completed))
	if _, found := e.session("ana", base); found {
		t.Fatal("the premise: the second continuation's end left the base it read")
	}

	var release []string
	if err := e.db.Read(t.Context(), func(tx *sql.Tx) error {
		var (
			fits bool
			err  error
		)
		release, fits, err = seatPlan(t.Context(), tx, "ana", next, 45, nil, tight)
		if err == nil && !fits {
			err = errors.New("the capture does not fit")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(release, []string{first, second}) {
		t.Errorf("the budget releases %v, want both continuations: the object they share is freed only "+
			"with its last row", release)
	}
	e.capture(capturing("ana", "chat:x", next, 1, 1, 45, tight))
	for _, turn := range []string{first, second} {
		if s, _ := e.session("ana", turn); s.HasObject() || len(e.media("ana", turn)) != 0 {
			t.Errorf("turn %s keeps %+v and media %v after its release", turn, s.Object,
				digestsOf(e.media("ana", turn)))
		}
	}
}

// EVERY READ CARRIES ITS MEDIA: a row naming media is read with them by every
// read of it — a turn's row, a seat's page, a conversation's head and its
// siblings, a search's hits — and copied by the views a relaunch and a person's
// read open it through, so no reader opens a session's frames without what
// they splice in. A row naming none reads none.
//
// Mutation: skip withMedia in any one read, or leave Media out of
// Session.Kept, and its check goes red.
func TestEveryReadCarriesItsMedia(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	older, head, plain := newTurn(), newTurn(), newTurn()
	pdf, image, earlier := payload("pdf", 40<<20), payload("image", 20<<20), payload("earlier", 20<<20)
	e.capture(naming(capturing("ana", "chat:room", older, 1, 1, 100, budget), earlier))
	e.end(ending("ana", "chat:room", older, "", completed))
	e.capture(naming(capturing("ana", "chat:room", head, 1, 1, 100, budget), pdf, image))
	e.end(ending("ana", "chat:room", head, "", completed))
	e.capture(capturing("ana", "chat:plain", plain, 1, 1, 100, budget))
	want := map[string][]string{older: {earlier.Digest}, head: digestsOf(e.media("ana", head)), plain: nil}
	if len(want[head]) != 2 {
		t.Fatalf("the premise: the head names media %v", want[head])
	}
	check := func(read string, s Session) {
		t.Helper()
		if got := digestsOf(s.Media); !slices.Equal(got, want[s.Turn]) {
			t.Errorf("%s reads turn %s with media %v, want %v", read, s.Turn, got, want[s.Turn])
		}
		if got := digestsOf(s.Kept().Media); !slices.Equal(got, want[s.Turn]) {
			t.Errorf("%s's view of turn %s carries media %v, want %v", read, s.Turn, got, want[s.Turn])
		}
	}
	r := e.reader()
	for turn := range want {
		one, err := r.Session(t.Context(), "ana", turn, stale)
		if err != nil {
			t.Fatal(err)
		}
		check("a turn's row", one.Session)
	}
	page, err := r.Sessions(t.Context(), "ana", "", MaxPageSize, stale)
	if err != nil || len(page.Sessions) != len(want) {
		t.Fatalf("the seat's page is %d rows (%v), want %d", len(page.Sessions), err, len(want))
	}
	for _, s := range page.Sessions {
		check("the seat's page", s)
	}
	h := e.head("ana", "chat:room")
	if h.Session == nil || h.Session.Turn != head || len(h.Siblings) != 1 {
		t.Fatalf("the premise: the head is %+v beside %d siblings", h.Session, len(h.Siblings))
	}
	check("the conversation's head", *h.Session)
	check("the conversation's sibling", h.Siblings[0])
	if kept := h.Kept(); !slices.Equal(digestsOf(kept.Session.Media), want[head]) ||
		!slices.Equal(digestsOf(kept.Siblings[0].Media), want[older]) {
		t.Errorf("the head's frozen view carries media %v and %v", digestsOf(kept.Session.Media),
			digestsOf(kept.Siblings[0].Media))
	}
	hits := e.search("ana", "build", "", MaxPageSize).Hits
	if len(hits) != 2 {
		t.Fatalf("the search found %d rows, want the two ended turns asked to fix the build", len(hits))
	}
	for _, hit := range hits {
		check("a search's hit", hit.Session)
	}
}

// A PAGE OF A SEAT'S SESSIONS IS HELD TO ITS BYTES WITH ITS MEDIA: what the
// answer carries of a row is its media too, so a page of rows each naming the
// most media a capture may stops where they reach the bound, and goes on from
// its last kept row, rather than build an answer its reply refuses.
//
// Mutation: hold the page to its rows' bytes before their media are read, and
// the page carries every row past the bound.
func TestASeatsPageIsHeldToItsBytesWithItsMedia(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	media := make([]CaptureMedia, executorkeep.MediaMaxCount)
	for i := range media {
		media[i] = payload(fmt.Sprint(i), 1)
	}
	for range 4 {
		e.capture(naming(capturing("ana", "chat:"+newTurn(), newTurn(), 1, 1, 100, budget), media...))
	}
	one, err := e.reader().Session(t.Context(), "ana", e.anyTurn("ana"), stale)
	if err != nil {
		t.Fatal(err)
	}
	bare := one.Session
	bare.Media = nil
	full, bareRow := encodedBytes(t, one.Session), encodedBytes(t, bare)
	bound := 2*full + full/2
	if 3*bareRow > bound {
		t.Fatalf("the premise: three rows without media (%d bytes) fit no fewer than %d bytes", 3*bareRow, bound)
	}
	ctx := statelog.WithAnswerBytes(t.Context(), bound)
	page, err := e.reader().Sessions(ctx, "ana", "", MaxPageSize, stale)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 2 || page.Next == "" {
		t.Errorf("a page held to %d bytes of %d-byte rows carries %d rows (next %q), want two and a cursor",
			bound, full, len(page.Sessions), page.Next)
	}
}

// anyTurn is one turn handle's seat holds a row of.
func (e *estate) anyTurn(handle string) string {
	e.t.Helper()
	page, err := e.reader().Sessions(e.t.Context(), handle, "", 1, stale)
	if err != nil || len(page.Sessions) == 0 {
		e.t.Fatalf("read a row of %s: %v", handle, err)
	}
	return page.Sessions[0].Turn
}

// encodedBytes is v's length as the wire encodes it, which a read's bound counts.
func encodedBytes(t *testing.T, v any) int {
	t.Helper()
	held := statelog.NewRowBytes(1 << 30)
	held.TakeEncoded(v)
	return held.Held()
}

// THE SEALED CENSUS COUNTS MEDIA TOO: a media object is sealed under a keyring
// key as its session is, so a rotation waits it out by the key its row names,
// and a released row's media hold no key back.
//
// Mutation: count only the sessions' rows, and a rotation drops a key the
// media still need.
func TestTheSealedCensusCountsMediaToo(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	const tight = 100
	turn := newTurn()
	pdf := payload("pdf", 40)
	pdf.ContentKey = "k2"
	e.capture(naming(capturing("ana", "chat:a", turn, 1, 1, 50, tight), pdf))
	census := func() map[string]int {
		t.Helper()
		got, err := e.reader().SealedKeys(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return got.Values
	}
	if got := census(); got["k1"] != 1 || got["k2"] != 1 {
		t.Errorf("the census is %v, want the session under k1 and its media under k2", got)
	}
	e.end(ending("ana", "chat:a", turn, "", completed))
	e.capture(capturing("ana", "chat:b", newTurn(), 1, 1, 90, tight))
	if got := census(); got["k1"] != 1 || got["k2"] != 0 {
		t.Errorf("the census after the release is %v, want the new session alone", got)
	}
}
