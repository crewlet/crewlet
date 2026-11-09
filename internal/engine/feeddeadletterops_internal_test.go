package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/code"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ---- the operator's listing and discard ------------------------------- //

// subjectBox is a dead-letter store keyed by subject, as the broker's is: ONE
// sequence across every subject, a listing a case can break per subject, marks
// that count the feeds' subjects, and a drop that refuses a sequence held on
// another subject and answers one already gone as done — the queue contract's
// answers.
type subjectBox struct {
	mu    sync.Mutex
	seq   uint64
	held  map[string][]queue.Kept
	fails map[string]error
	// frames is how many frames anything read through the box: no
	// operator's listing or discard reads one.
	frames int
}

func newSubjectBox() *subjectBox {
	return &subjectBox{held: map[string][]queue.Kept{}, fails: map[string]error{}}
}

func (b *subjectBox) KeepDeadLetter(context.Context, string, []byte, []byte) error {
	return errors.New("no case here keeps through the box: it dates its entries itself")
}

func (b *subjectBox) DeadLettersOn(_ context.Context, subject string) ([]queue.Kept, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.fails[subject]; err != nil {
		return nil, err
	}
	return slices.Clone(b.held[subject]), nil
}

func (b *subjectBox) DeadLetterFrame(_ context.Context, subject string, seq uint64) ([]byte, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frames++
	if slices.ContainsFunc(b.held[subject], func(k queue.Kept) bool { return k.Seq == seq }) {
		return []byte("sealed frame"), true, nil
	}
	return nil, false, nil
}

func (b *subjectBox) DeadLetterMarks(_ context.Context, under string) (queue.Marks, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	marks := queue.Marks{Counts: map[string]uint64{}, Last: b.seq}
	for subject, kept := range b.held {
		if strings.HasPrefix(subject, under+".") && len(kept) > 0 {
			marks.Counts[subject] = uint64(len(kept))
		}
	}
	return marks, nil
}

// framesRead is how many frames anything read through the box.
func (b *subjectBox) framesRead() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.frames
}

func (b *subjectBox) DropDeadLetter(_ context.Context, subject string, seq uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for other, kept := range b.held {
		i := slices.IndexFunc(kept, func(k queue.Kept) bool { return k.Seq == seq })
		switch {
		case i < 0:
		case other != subject:
			return fmt.Errorf("box: %d is held on %s, not %s", seq, other, subject)
		default:
			b.held[subject] = slices.Delete(kept, i, i+1)
		}
	}
	return nil
}

// keep holds an entry kept under header on subject as the broker would at
// kept, letting it go at expires, and answers its sequence.
func (b *subjectBox) keep(subject string, header []byte, kept, expires time.Time) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	b.held[subject] = append(b.held[subject], queue.Kept{Seq: b.seq, StoredAt: kept,
		Expires: expires, Header: header})
	return b.seq
}

// fail makes subject's listing answer err; nil lists it again.
func (b *subjectBox) fail(subject string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails[subject] = err
}

// holds reports whether subject holds seq.
func (b *subjectBox) holds(subject string, seq uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.ContainsFunc(b.held[subject], func(k queue.Kept) bool { return k.Seq == seq })
}

// The feeds this build declares, from each domain's own constants: what the
// listing must name, read independently of the register it is derived from.
var (
	workFeed = changefeed.Source{Name: worklet.Source, Group: worklet.FeedGroup}
	pageFeed = changefeed.Source{Name: doclet.Source, Group: doclet.FeedGroup}
	chatFeed = changefeed.Source{Name: chatlet.Source, Group: chatlet.FeedGroup}
	codeFeed = changefeed.Source{Name: code.Source, Group: code.FeedGroup}
)

// letterBytes is d as the header its entry is kept under.
func letterBytes(t *testing.T, d changefeed.DeadLetter) []byte {
	t.Helper()
	header, err := changefeed.EncodeDeadLetter(d)
	if err != nil {
		t.Fatal(err)
	}
	return header
}

// letterOn is a dead letter the src feed kept of the record at seq on its log,
// handed back for an hour under a key no node held.
func letterOn(src changefeed.Source, seq uint64, at time.Time) changefeed.DeadLetter {
	return changefeed.DeadLetter{Source: src.Name, Group: src.Group,
		Stream: "CREWLET_TRACKER_LOG", Seq: seq, Gen: 1, Key: "crewlet.tracker.task.T-1",
		OpID: fmt.Sprintf("op-%d", seq), Reason: changefeed.DeferUnknownKey,
		Detail:   "the frame is sealed under a key this node does not hold (it holds [k1])",
		StoredAt: at.Add(-time.Hour), Delivered: 312, DeadLetteredAt: at}
}

// discardedCount is how many discards rec counted on src.
func discardedCount(rec *metrics.Recorder, src changefeed.Source) uint64 {
	var n uint64
	for _, s := range rec.Read() {
		if s.Name == metrics.TrackerFeedDeadLetterDiscarded && s.Attrs["source"] == src.Name {
			n += s.Total
		}
	}
	return n
}

// operatorOf is an engine's operator surface over store, accounting in claims,
// counting in rec, dated at now.
func operatorOf(t *testing.T, store deadLetterStore, claims expiryClaims,
	rec *metrics.Recorder, now time.Time) (*FeedDeadLetters, *deadLetterLedger) {

	t.Helper()
	ledger := newDeadLetterLedger(store, claims)
	ledger.now = func() time.Time { return now }
	ops := (&Engine{deadLetterLedger: ledger, metrics: rec}).FeedDeadLetters()
	if ops == nil {
		t.Fatal("an engine keeping dead letters answered no operator surface")
	}
	ops.now = func() time.Time { return now }
	return ops, ledger
}

// THE LISTING NAMES EVERY FEED THE REGISTER DECLARES, EVERY ENTRY, AND EVERY
// FEED IT COULD NOT READ — ON AN ENGINE RUNNING NO LOG.
//
// Which feeds exist is the domains' declaration, so an engine with no core and
// no log running lists all four, in the register's order. An entry this build
// reads is listed by every field its feed kept of the record; one a newer build
// kept by its id, its instants and why this build cannot read it; a feed
// holding nothing as an empty list; and a feed whose listing failed by its
// error, never left out and never read as empty. A subscription's poison under
// another `dlq.` subject is no feed's, and is not listed.
//
// Mutation: list only the feeds a running log declares, leave a failed feed
// out, or drop an entry this build cannot decode, and a case goes red.
func TestTheListingNamesEveryDeclaredFeedEveryEntryAndEveryFeedItCouldNotRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	month := 30 * 24 * time.Hour
	box := newSubjectBox()
	work, page := changefeed.DeadLetterSubject(workFeed), changefeed.DeadLetterSubject(pageFeed)
	read := box.keep(work, letterBytes(t, letterOn(workFeed, 1234, now.Add(-2*time.Hour))),
		now.Add(-2*time.Hour), now.Add(month-2*time.Hour))
	newer := box.keep(work, []byte(`{"v":2,"source":"work"}`),
		now.Add(-time.Hour), now.Add(month-time.Hour))
	box.keep(topics.DeadLetter("crewlet.agent.ana.inbox", "agent-ana"), nil,
		now.Add(-time.Hour), now.Add(month-time.Hour))
	box.fail(page, fmt.Errorf("jetstream: list the dead letters: %w", queue.ErrUnavailable))
	ops, _ := operatorOf(t, box, nil, nil, now)

	got := ops.List(t.Context())
	if !got.At.Equal(now) {
		t.Errorf("the listing is dated %v, want the node's now %v", got.At, now)
	}
	var feeds []changefeed.Source
	for _, f := range got.Feeds {
		feeds = append(feeds, changefeed.Source{Name: f.Source, Group: f.Group})
		if f.Subject != changefeed.DeadLetterSubject(changefeed.Source{Name: f.Source, Group: f.Group}) {
			t.Errorf("the %s feed names the subject %q, which is not where it keeps its entries",
				f.Source, f.Subject)
		}
	}
	if want := []changefeed.Source{workFeed, pageFeed, chatFeed, codeFeed}; !slices.Equal(feeds, want) {
		t.Fatalf("the listing names the feeds %v, want every feed the domains declare, %v", feeds, want)
	}
	var declared []string
	for _, domain := range registeredDomains() {
		if group := domain.FeedGroup(); group != "" {
			declared = append(declared, group)
		}
	}
	if !slices.Equal(declared, []string{workFeed.Group, pageFeed.Group, chatFeed.Group, codeFeed.Group}) {
		t.Fatalf("the register declares the wake feeds %v, so this case names the wrong ones", declared)
	}

	entries := got.Feeds[0].Entries
	if len(entries) != 2 || got.Feeds[0].ListError != "" {
		t.Fatalf("the work feed lists %+v (%q), want its two entries", entries, got.Feeds[0].ListError)
	}
	stored := now.Add(-3 * time.Hour)
	want := FeedDeadLetter{ID: read, Source: "work", Stream: "CREWLET_TRACKER_LOG", Position: 1234,
		Generation: 1, Key: "crewlet.tracker.task.T-1", OpID: "op-1234", Reason: "unknown_key",
		Detail:   "the frame is sealed under a key this node does not hold (it holds [k1])",
		StoredAt: stored, Delivered: 312, DeadLetteredAt: now.Add(-2 * time.Hour),
		KeptAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(month - 2*time.Hour)}
	if entries[0] != want {
		t.Errorf("the entry this build reads is listed as\n%+v\nwant\n%+v", entries[0], want)
	}
	if e := entries[1]; e.ID != newer || e.Source != "work" || e.DecodeError == "" ||
		e.Stream != "" || e.Reason != "" || !e.KeptAt.Equal(now.Add(-time.Hour)) ||
		!e.ExpiresAt.Equal(now.Add(month-time.Hour)) {
		t.Errorf("the entry a newer build kept is listed as %+v, want its id, instants and "+
			"why this build cannot read it, and nothing it could not read", e)
	}
	if f := got.Feeds[1]; f.ListError == "" || f.Entries != nil {
		t.Errorf("the page feed, whose listing failed, reads %+v: want its error and no entries", f)
	}
	for _, f := range got.Feeds[2:] {
		if f.Entries == nil || len(f.Entries) != 0 || f.ListError != "" {
			t.Errorf("the %s feed, holding nothing, reads %+v: want an empty list", f.Source, f)
		}
	}
	// FROM THE HEADERS ALONE: an operator decides by what the feed recorded
	// of the record, and its frame may be as large as its log admits.
	if n := box.framesRead(); n != 0 {
		t.Errorf("the listing read %d frames, want none", n)
	}
}

// A DISCARD DROPS THE ENTRY IT NAMES AND NOTHING ELSE, ANSWERS IT, AND LOGS AND
// COUNTS IT AS WHO PRESSED IT — over the queue's own memory twin.
//
// An id on a subscription's dead-letter subject is no feed's, and an id nothing
// holds is nobody's: both answer that no feed holds it, and remove nothing. The
// work feed's entry is dropped, answered whole, counted once on its source and
// logged naming the person and the credential; the chat feed's entry beside
// it, and the subscription's poison, stay. A second discard of the same entry
// finds nothing, and counts and logs nothing.
//
// NOT PARALLEL: it swaps the process-wide logger, which this package otherwise
// leaves at its boot default, and a parallel case's lines would be counted.
//
// Mutation: search every dead-letter subject rather than the feeds', count or
// log a discard that found nothing, or log no presser, and a case goes red.
func TestADiscardDropsTheEntryItNamesAnswersItAndLogsAndCountsIt(t *testing.T) {
	logs := &stopLineBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	q := queuememory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	now := time.Now().UTC()
	work, chat := changefeed.DeadLetterSubject(workFeed), changefeed.DeadLetterSubject(chatFeed)
	poisonOn := topics.DeadLetter("crewlet.agent.ana.inbox", "agent-ana")
	for subject, entry := range map[string]struct{ header, body []byte }{
		work:     {letterBytes(t, letterOn(workFeed, 1234, now)), []byte("sealed frame")},
		chat:     {letterBytes(t, letterOn(chatFeed, 77, now)), []byte("sealed frame")},
		poisonOn: {nil, []byte("a poison message")},
	} {
		if err := q.KeepDeadLetter(t.Context(), subject, entry.header, entry.body); err != nil {
			t.Fatalf("keep on %s: %v", subject, err)
		}
	}
	seqOn := func(subject string) uint64 {
		t.Helper()
		kept, err := q.DeadLettersOn(t.Context(), subject)
		if err != nil {
			t.Fatalf("list %s: %v", subject, err)
		}
		if len(kept) != 1 {
			return 0
		}
		return kept[0].Seq
	}
	workSeq, chatSeq, poisonSeq := seqOn(work), seqOn(chat), seqOn(poisonOn)
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	ops, _ := operatorOf(t, q, coordmem.NewFleet(), rec, now)
	by := iam.Actor{Name: "ana", Kind: iam.ActorHuman, OperatorID: "session:lineage-1"}
	discarded := func() []map[string]any {
		var out []map[string]any
		for _, record := range logs.records(t) {
			if record["msg"] == "changefeed_dead_letter_discarded" {
				out = append(out, record)
			}
		}
		return out
	}

	for name, id := range map[string]uint64{"a subscription's poison": poisonSeq, "nothing": 99999} {
		if _, err := ops.Discard(t.Context(), FeedDeadLetterDiscard{ID: id, By: by}); !errors.Is(err, ErrNoDeadLetter) {
			t.Errorf("discarding %s (%d) answered %v, want that no feed holds it", name, id, err)
		}
	}
	if seqOn(poisonOn) != poisonSeq {
		t.Fatal("discarding a subscription's poison removed it: no feed's discard may reach it")
	}

	got, err := ops.Discard(t.Context(), FeedDeadLetterDiscard{ID: workSeq, By: by})
	if err != nil {
		t.Fatalf("discard the work feed's entry: %v", err)
	}
	if got.ID != workSeq || got.Source != "work" || got.Stream != "CREWLET_TRACKER_LOG" ||
		got.Position != 1234 || got.OpID != "op-1234" || got.Reason != "unknown_key" {
		t.Errorf("the discard answered %+v, want the entry it removed", got)
	}
	if seqOn(work) != 0 || seqOn(chat) != chatSeq || seqOn(poisonOn) != poisonSeq {
		t.Errorf("after the discard the work feed holds %d, the chat feed %d and the poison "+
			"subject %d: want the one entry gone and the others kept", seqOn(work), seqOn(chat),
			seqOn(poisonOn))
	}
	if n := discardedCount(rec, workFeed); n != 1 {
		t.Errorf("the discard was counted %d time(s) on the work feed, want once", n)
	}
	lines := discarded()
	if len(lines) != 1 {
		t.Fatalf("the discard left %d changefeed_dead_letter_discarded lines, want one: %v",
			len(lines), lines)
	}
	line := lines[0]
	if line["level"] != "WARN" || line["by"] != "ana" || line["by_kind"] != "human" ||
		line["operator"] != "session:lineage-1" || line["source"] != "work" ||
		line["id"] != float64(workSeq) || line["op_id"] != "op-1234" {
		t.Errorf("the discard's line reads %v, want a WARN naming who pressed it and the entry", line)
	}

	if _, err := ops.Discard(t.Context(), FeedDeadLetterDiscard{ID: workSeq, By: by}); !errors.Is(err, ErrNoDeadLetter) {
		t.Errorf("discarding the same entry again answered %v, want that no feed holds it", err)
	}
	if n, lines := discardedCount(rec, workFeed), discarded(); n != 1 || len(lines) != 1 {
		t.Errorf("a discard that found nothing was counted (%d) or logged (%d lines)", n, len(lines))
	}
}

// A DISCARD THAT COULD NOT LIST EVERY FEED IS NEVER A "NO SUCH ENTRY".
//
// The entry may be the unread feed's, so an id no listed feed holds answers
// that the feed could not be listed — a condition waiting may clear, where the
// broker did not answer — and removes nothing. An id a listed feed does hold is
// discarded all the same: an entry's id is unique across every subject, so the
// feed that did not answer cannot hold it too.
//
// Mutation: answer the unread case as no such entry, or refuse every discard
// while any feed is unread, and a case goes red.
func TestADiscardThatCouldNotListEveryFeedIsNeverANoSuchEntry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	box := newSubjectBox()
	work, page := changefeed.DeadLetterSubject(workFeed), changefeed.DeadLetterSubject(pageFeed)
	held := box.keep(work, letterBytes(t, letterOn(workFeed, 7, now)), now, now.Add(time.Hour))
	box.fail(page, fmt.Errorf("jetstream: list the dead letters: %w", queue.ErrUnavailable))
	ops, _ := operatorOf(t, box, coordmem.NewFleet(), nil, now)

	_, err := ops.Discard(t.Context(), FeedDeadLetterDiscard{ID: held + 1})
	var unread *DeadLettersUnread
	if !errors.As(err, &unread) || errors.Is(err, ErrNoDeadLetter) ||
		!errors.Is(err, queue.ErrUnavailable) || len(unread.Feeds) != 1 ||
		unread.Feeds[0].Source != "page" {
		t.Fatalf("a discard of an id no listed feed holds, beside an unread feed, answered %v: "+
			"want the page feed named as unread, a condition waiting may clear", err)
	}
	if !box.holds(work, held) {
		t.Fatal("a discard that found nothing removed an entry")
	}
	if _, err := ops.Discard(t.Context(), FeedDeadLetterDiscard{ID: held}); err != nil {
		t.Errorf("discarding an entry a listed feed holds answered %v beside an unread feed, "+
			"want it discarded", err)
	}
	if box.holds(work, held) {
		t.Error("the discarded entry is still held")
	}
	// FOUND BY ITS HEADER, never its frame: a discard reads none.
	if n := box.framesRead(); n != 0 {
		t.Errorf("the discards read %d frames, want none", n)
	}
}

// A DISCARD IS ACCOUNTED FOR, SO NO NODE COUNTS IT AS A WAKE THE RETENTION
// LOST.
//
// A discarded entry vanishes as an expired one does, so a node's alarm
// reading that saw it within a beat of its expiry, and then no more, would
// count it as expired. The discard drops it through the ledger, which takes
// the claim the expiry count takes, so neither the node that discarded it nor
// a peer that only saw it vanish counts it expired, and it is counted once, as
// discarded, while an entry the retention took beside it is still counted.
//
// Mutation: drop through the queue rather than the ledger, and the peer counts
// the discarded entry as expired.
func TestADiscardIsAccountedSoNoNodeCountsItAsAWakeTheRetentionLost(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	stored := now.Add(-30 * 24 * time.Hour)
	box := newSubjectBox()
	work := changefeed.DeadLetterSubject(workFeed)
	expiring := box.keep(work, letterBytes(t, letterOn(workFeed, 1, stored)), stored,
		now.Add(5*time.Second))
	discardedSeq := box.keep(work, letterBytes(t, letterOn(workFeed, 2, stored)), stored,
		now.Add(5*time.Second))

	claims := coordmem.NewFleet()
	expired := func(rec *metrics.Recorder) uint64 {
		for _, s := range rec.Read() {
			if s.Name == metrics.TrackerFeedDeadLetterExpired && s.Attrs["source"] == workFeed.Name {
				return s.Total
			}
		}
		return 0
	}
	recHere, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	recPeer, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	ops, here := operatorOf(t, box, claims, recHere, now)
	watchHere := newDeadLetterWatch(here, claims, recHere)
	watchPeer := newDeadLetterWatch(newDeadLetterLedger(box, claims), claims, recPeer)
	feeds := map[string]changefeed.Source{"tracker": workFeed}
	watchHere.read(t.Context(), feeds, now)
	watchPeer.read(t.Context(), feeds, now)

	// AN OPERATOR DISCARDS ONE, AND THE RETENTION TAKES THE OTHER.
	if _, err := ops.Discard(t.Context(), FeedDeadLetterDiscard{ID: discardedSeq}); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if err := box.DropDeadLetter(t.Context(), work, expiring); err != nil {
		t.Fatal(err)
	}
	beat := now.Add(statelog.AlarmInterval)
	watchHere.read(t.Context(), feeds, beat)
	watchPeer.read(t.Context(), feeds, beat)

	if got := expired(recHere) + expired(recPeer); got != 1 {
		t.Errorf("the fleet counted %d lost wake(s) as expired, want the one the retention "+
			"took, once (this node %d, its peer %d)", got, expired(recHere), expired(recPeer))
	}
	if n := discardedCount(recHere, workFeed) + discardedCount(recPeer, workFeed); n != 1 {
		t.Errorf("the fleet counted %d discard(s), want the one", n)
	}
}

// A DISCARD SOMEBODY ELSE ACCOUNTED FOR FIRST SAYS IT DISCARDED NOTHING, AND
// COUNTS NOTHING.
//
// Between a discard's listing and its drop a peer's replay can drop the same
// entry — its wake published — or the retention let it go and an alarm
// reading count it; the queue answers the discard's drop of an entry already
// gone as done. The claim on the entry's going is what tells: here it is
// already held, as that replay would hold it, so the discard answers that no
// feed holds the entry and counts no loss — a published wake is none.
//
// Mutation: count and answer a discard whose claim somebody else holds, and
// the replayed wake reads as one an operator gave up.
func TestADiscardSomebodyElseAccountedForFirstSaysItDiscardedNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	box := newSubjectBox()
	work := changefeed.DeadLetterSubject(workFeed)
	seq := box.keep(work, letterBytes(t, letterOn(workFeed, 9, now)), now, now.Add(time.Hour))
	claims := coordmem.NewFleet()
	if won, err := claims.Claim(t.Context(), goneKey(letterAt{work, seq}), expiredClaimTTL, now); err != nil || !won {
		t.Fatalf("the replay's claim on the entry's going: %v, %v", won, err)
	}
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	ops, _ := operatorOf(t, box, claims, rec, now)
	if _, err := ops.Discard(t.Context(), FeedDeadLetterDiscard{ID: seq}); !errors.Is(err, ErrNoDeadLetter) {
		t.Errorf("a discard whose entry a replay accounted for answered %v, want that no feed "+
			"holds it", err)
	}
	if n := discardedCount(rec, workFeed); n != 0 {
		t.Errorf("a discard that removed nothing was counted %d time(s)", n)
	}
}
